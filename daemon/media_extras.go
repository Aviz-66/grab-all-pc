package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Milestone #5: Pre-download quality/size preview, cached metadata reuse,
// and subtitle + chapter embedding (yt-dlp path and hybrid Go-engine path).
// =============================================================================

// --- 5A. Subtitle / chapter request normalization ---

const defaultSubLangs = "en"

// normalizeSubLangs sanitizes a user supplied yt-dlp style language list
// ("en", "en,si", "en.*", "all") into a safe comma separated value.
func normalizeSubLangs(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return defaultSubLangs
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ',', r == '-', r == '.', r == '*', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == ';':
			b.WriteRune(',')
		}
	}
	parts := strings.Split(b.String(), ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.Trim(p, ".-_")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= 6 {
			break
		}
	}
	if len(out) == 0 {
		return defaultSubLangs
	}
	res := strings.Join(out, ",")
	if len(res) > 64 {
		res = res[:64]
		res = strings.TrimRight(res, ",")
	}
	return res
}

// buildYtDlpEmbedArgs returns the yt-dlp flags that embed subtitles and chapters.
// Embedding requires FFmpeg; without it nothing is added so downloads still succeed.
func buildYtDlpEmbedArgs(embedSubs bool, subLangs string, embedChapters bool, isAudio, hasFFmpeg bool) []string {
	if !hasFFmpeg {
		return nil
	}
	var args []string
	if embedSubs && !isAudio {
		args = append(args,
			"--write-subs",
			"--write-auto-subs",
			"--sub-langs", normalizeSubLangs(subLangs),
			"--embed-subs",
		)
	}
	if embedChapters {
		args = append(args, "--embed-chapters")
	}
	return args
}

// isYtDlpSubtitleDownloadError detects yt-dlp aborting because a subtitle track failed
// (commonly HTTP 429 on YouTube timedtext), so we can retry without subtitles.
func isYtDlpSubtitleDownloadError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "unable to download video subtitles") ||
		strings.Contains(lower, "unable to download subtitles") ||
		(strings.Contains(lower, "subtitle") && strings.Contains(lower, "429"))
}

// --- 5B. Subtitle & chapter metadata from yt-dlp JSON ---

type ytSubtitleFormat struct {
	Ext  string `json:"ext"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

type ytChapter struct {
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Title     string  `json:"title"`
}

type subtitleTrack struct {
	Lang string
	URL  string
	Ext  string
	Auto bool
}

func subtitleExtPriority(ext string) int {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case "vtt":
		return 0
	case "srt":
		return 1
	case "ass", "ssa":
		return 2
	}
	return -1
}

func pickSubtitleFormat(formats []ytSubtitleFormat) (ytSubtitleFormat, bool) {
	best := -1
	var chosen ytSubtitleFormat
	for _, f := range formats {
		if strings.TrimSpace(f.URL) == "" {
			continue
		}
		p := subtitleExtPriority(f.Ext)
		if p < 0 {
			continue
		}
		if best < 0 || p < best {
			best = p
			chosen = f
		}
	}
	return chosen, best >= 0
}

func subLangMatches(key, pattern string) bool {
	key = strings.ToLower(key)
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || key == "live_chat" {
		return false
	}
	if pattern == "all" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") || strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(strings.TrimSuffix(pattern, "*"), ".")
		return strings.HasPrefix(key, prefix)
	}
	return key == pattern || strings.HasPrefix(key, pattern+"-")
}

// selectSubtitleTracks chooses up to maxTracks subtitle tracks honoring the language
// list order. Human-made subtitles win over automatic captions for the same language.
func selectSubtitleTracks(manual, auto map[string][]ytSubtitleFormat, langs string, maxTracks int) []subtitleTrack {
	if maxTracks <= 0 {
		maxTracks = 3
	}
	patterns := strings.Split(normalizeSubLangs(langs), ",")
	sortedKeys := func(m map[string][]ytSubtitleFormat) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	manualKeys := sortedKeys(manual)
	autoKeys := sortedKeys(auto)

	var out []subtitleTrack
	usedBase := make(map[string]bool)
	add := func(key string, formats []ytSubtitleFormat, isAuto bool) bool {
		base := strings.SplitN(strings.ToLower(key), "-", 2)[0]
		if usedBase[base] {
			return false
		}
		f, ok := pickSubtitleFormat(formats)
		if !ok {
			return false
		}
		usedBase[base] = true
		out = append(out, subtitleTrack{Lang: key, URL: f.URL, Ext: strings.ToLower(f.Ext), Auto: isAuto})
		return true
	}

	for _, p := range patterns {
		if len(out) >= maxTracks {
			break
		}
		matched := false
		for _, k := range manualKeys {
			if len(out) >= maxTracks {
				break
			}
			if subLangMatches(k, p) && add(k, manual[k], false) {
				matched = true
				if p != "all" {
					break
				}
			}
		}
		if matched || p == "all" {
			continue
		}
		// Prefer the original-language auto caption ("en-orig") over machine translations.
		for _, pass := range []bool{true, false} {
			done := false
			for _, k := range autoKeys {
				if len(out) >= maxTracks {
					break
				}
				isOrig := strings.HasSuffix(strings.ToLower(k), "-orig")
				if isOrig != pass {
					continue
				}
				if subLangMatches(k, p) && add(k, auto[k], true) {
					done = true
					break
				}
			}
			if done {
				break
			}
		}
	}
	return out
}

var iso639To3 = map[string]string{
	"en": "eng", "es": "spa", "fr": "fra", "de": "deu", "it": "ita", "pt": "por",
	"ru": "rus", "ja": "jpn", "ko": "kor", "zh": "zho", "ar": "ara", "hi": "hin",
	"si": "sin", "ta": "tam", "nl": "nld", "sv": "swe", "pl": "pol", "tr": "tur",
	"vi": "vie", "id": "ind", "th": "tha", "uk": "ukr", "cs": "ces", "el": "ell",
	"he": "heb", "fa": "fas", "bn": "ben", "ur": "urd", "ms": "msa", "ro": "ron",
	"hu": "hun", "da": "dan", "fi": "fin", "no": "nor", "nb": "nob",
}

func subtitleLangTo3(lang string) string {
	base := strings.SplitN(strings.ToLower(strings.TrimSpace(lang)), "-", 2)[0]
	if len(base) == 3 {
		return base
	}
	if v, ok := iso639To3[base]; ok {
		return v
	}
	return "und"
}

func escapeFFMetadata(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		"=", `\=`,
		";", `\;`,
		"#", `\#`,
		"\r", "",
		"\n", `\`+"\n",
	)
	return r.Replace(s)
}

// buildFFMetadataChapters renders an FFmpeg FFMETADATA1 document containing chapters.
// Returns "" when there are no usable chapters.
func buildFFMetadataChapters(chapters []ytChapter, durationSec float64) string {
	valid := make([]ytChapter, 0, len(chapters))
	for _, c := range chapters {
		if c.StartTime < 0 || math.IsNaN(c.StartTime) {
			continue
		}
		valid = append(valid, c)
	}
	if len(valid) == 0 {
		return ""
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].StartTime < valid[j].StartTime })

	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	written := 0
	for i, c := range valid {
		start := int64(math.Round(c.StartTime * 1000))
		endSec := c.EndTime
		if endSec <= c.StartTime {
			if i+1 < len(valid) {
				endSec = valid[i+1].StartTime
			} else if durationSec > c.StartTime {
				endSec = durationSec
			} else {
				endSec = c.StartTime + 1
			}
		}
		end := int64(math.Round(endSec * 1000))
		if end <= start {
			continue
		}
		title := strings.TrimSpace(c.Title)
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		b.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		fmt.Fprintf(&b, "START=%d\nEND=%d\n", start, end)
		b.WriteString("title=" + escapeFFMetadata(title) + "\n")
		written++
	}
	if written == 0 {
		return ""
	}
	return b.String()
}

func subtitleCodecForContainer(finalPath string) string {
	switch strings.ToLower(filepath.Ext(finalPath)) {
	case ".mkv":
		return "srt"
	case ".webm":
		return "webvtt"
	default:
		return "mov_text"
	}
}

// buildHybridMergeArgs builds the FFmpeg argument list that muxes video + audio
// and optionally subtitle tracks and a chapters FFMETADATA file.
func buildHybridMergeArgs(st *hybridMergeState, outPath string, includeSidecars bool) []string {
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", st.VideoPath,
		"-i", st.AudioPath,
	}
	var subs []int
	var subLangs []string
	nextInput := 2
	metaIdx := -1
	if includeSidecars {
		for i, p := range st.SubtitlePaths {
			args = append(args, "-i", p)
			subs = append(subs, nextInput)
			lang := ""
			if i < len(st.SubtitleLangs) {
				lang = st.SubtitleLangs[i]
			}
			subLangs = append(subLangs, lang)
			nextInput++
		}
		if st.ChaptersPath != "" {
			args = append(args, "-f", "ffmetadata", "-i", st.ChaptersPath)
			metaIdx = nextInput
			nextInput++
		}
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	for _, idx := range subs {
		args = append(args, "-map", fmt.Sprintf("%d:0", idx))
	}
	args = append(args, "-c", "copy")
	if len(subs) > 0 {
		args = append(args, "-c:s", subtitleCodecForContainer(st.FinalPath))
		for i, lang := range subLangs {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+subtitleLangTo3(lang))
		}
	}
	if metaIdx >= 0 {
		args = append(args, "-map_metadata", fmt.Sprintf("%d", metaIdx), "-map_chapters", fmt.Sprintf("%d", metaIdx))
	}
	args = append(args, "-strict", "-2", outPath)
	return args
}

func hybridSidecarPaths(st *hybridMergeState) []string {
	if st == nil {
		return nil
	}
	out := append([]string(nil), st.SubtitlePaths...)
	if st.ChaptersPath != "" {
		out = append(out, st.ChaptersPath)
	}
	return out
}

// pruneMissingSidecars drops sidecar files that no longer exist (e.g. after manual cleanup).
func pruneMissingSidecars(st *hybridMergeState) {
	if st == nil {
		return
	}
	var paths, langs []string
	for i, p := range st.SubtitlePaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 {
			paths = append(paths, p)
			if i < len(st.SubtitleLangs) {
				langs = append(langs, st.SubtitleLangs[i])
			} else {
				langs = append(langs, "")
			}
		}
	}
	st.SubtitlePaths, st.SubtitleLangs = paths, langs
	if st.ChaptersPath != "" {
		if fi, err := os.Stat(st.ChaptersPath); err != nil || fi.IsDir() || fi.Size() == 0 {
			st.ChaptersPath = ""
		}
	}
}

var sidecarHTTPClient = &http.Client{Timeout: 25 * time.Second}

func fetchSubtitleFile(ctx context.Context, track subtitleTrack, headers map[string]string, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, track.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Range") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := sidecarHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subtitle HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 12*1024*1024))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("empty subtitle body")
	}
	return os.WriteFile(destPath, data, 0644)
}

// prepareHybridSidecars downloads selected subtitle tracks and writes a chapters file
// next to the final output. Failures are non-fatal: the video is still merged.
func prepareHybridSidecars(ctx context.Context, info *ytDlpInfoJSON, finalPath string, embedSubs bool, subLangs string, embedChapters bool) (subPaths, subLangsOut []string, chaptersPath string) {
	if info == nil {
		return nil, nil, ""
	}
	stem := strings.TrimSuffix(finalPath, filepath.Ext(finalPath))
	if embedSubs {
		tracks := selectSubtitleTracks(info.Subtitles, info.AutomaticCaptions, subLangs, 3)
		for _, tr := range tracks {
			safeLang := sanitizeFormatID(tr.Lang, "sub")
			dest := fmt.Sprintf("%s.graball.%s.%s", stem, safeLang, tr.Ext)
			if err := fetchSubtitleFile(ctx, tr, info.HTTPHeaders, dest); err != nil {
				_ = os.Remove(dest)
				continue
			}
			subPaths = append(subPaths, dest)
			subLangsOut = append(subLangsOut, tr.Lang)
		}
	}
	if embedChapters {
		if doc := buildFFMetadataChapters(info.Chapters, float64(info.Duration)); doc != "" {
			dest := stem + ".graball.chapters.txt"
			if err := os.WriteFile(dest, []byte(doc), 0644); err == nil {
				chaptersPath = dest
			}
		}
	}
	return subPaths, subLangsOut, chaptersPath
}

// --- 5C. Pre-download Media Preview ---

type ytPreviewFormat struct {
	FormatID       string    `json:"format_id"`
	Ext            string    `json:"ext"`
	VCodec         string    `json:"vcodec"`
	ACodec         string    `json:"acodec"`
	Width          flexInt64 `json:"width"`
	Height         flexInt64 `json:"height"`
	TBR            float64   `json:"tbr"`
	Filesize       flexInt64 `json:"filesize"`
	FilesizeApprox flexInt64 `json:"filesize_approx"`
	Protocol       string    `json:"protocol"`
}

type ytPreviewJSON struct {
	ID                string                        `json:"id"`
	Title             string                        `json:"title"`
	Uploader          string                        `json:"uploader"`
	Channel           string                        `json:"channel"`
	Thumbnail         string                        `json:"thumbnail"`
	Duration          float64                       `json:"duration"`
	IsLive            *bool                         `json:"is_live"`
	LiveStatus        string                        `json:"live_status"`
	WebpageURL        string                        `json:"webpage_url"`
	Formats           []ytPreviewFormat             `json:"formats"`
	Subtitles         map[string][]ytSubtitleFormat `json:"subtitles"`
	AutomaticCaptions map[string][]ytSubtitleFormat `json:"automatic_captions"`
	Chapters          []ytChapter                   `json:"chapters"`
	Type              string                        `json:"_type"`
}

type PreviewOption struct {
	Quality    string `json:"quality"`
	Label      string `json:"label"`
	Resolution string `json:"resolution,omitempty"`
	VideoCodec string `json:"videoCodec,omitempty"`
	AudioCodec string `json:"audioCodec,omitempty"`
	SizeBytes  int64  `json:"sizeBytes"`
	Approx     bool   `json:"approx"`
}

type PreviewChapter struct {
	Title string  `json:"title"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type MediaPreview struct {
	URL               string           `json:"url"`
	ID                string           `json:"id,omitempty"`
	Title             string           `json:"title"`
	Uploader          string           `json:"uploader,omitempty"`
	Thumbnail         string           `json:"thumbnail,omitempty"`
	Duration          float64          `json:"duration"`
	DurationText      string           `json:"durationText,omitempty"`
	IsLive            bool             `json:"isLive"`
	Options           []PreviewOption  `json:"options"`
	SubtitleLangs     []string         `json:"subtitleLangs"`
	AutoSubtitleLangs []string         `json:"autoSubtitleLangs"`
	Chapters          []PreviewChapter `json:"chapters"`
	Cached            bool             `json:"cached"`
}

func isNoneCodec(c string) bool {
	c = strings.ToLower(strings.TrimSpace(c))
	return c == "" || c == "none"
}

func shortCodecName(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case strings.HasPrefix(c, "avc"), strings.HasPrefix(c, "h264"):
		return "H.264"
	case strings.HasPrefix(c, "vp09"), strings.HasPrefix(c, "vp9"):
		return "VP9"
	case strings.HasPrefix(c, "av01"), strings.HasPrefix(c, "av1"):
		return "AV1"
	case strings.HasPrefix(c, "mp4a"):
		return "AAC"
	case strings.HasPrefix(c, "opus"):
		return "Opus"
	case c == "" || c == "none":
		return ""
	}
	if i := strings.IndexByte(c, '.'); i > 0 {
		return strings.ToUpper(c[:i])
	}
	return strings.ToUpper(c)
}

func formatPreviewDuration(sec float64) string {
	if sec <= 0 {
		return ""
	}
	total := int64(math.Round(sec))
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// estimateFormatSize returns the format size in bytes and whether it's an estimate.
func estimateFormatSize(f ytPreviewFormat, duration float64) (int64, bool) {
	if f.Filesize > 0 {
		return int64(f.Filesize), false
	}
	if f.FilesizeApprox > 0 {
		return int64(f.FilesizeApprox), true
	}
	if f.TBR > 0 && duration > 0 {
		return int64(f.TBR * 1000 / 8 * duration), true
	}
	return 0, true
}

func formatDim(f ytPreviewFormat) int {
	w, h := int(f.Width), int(f.Height)
	if w > 0 && h > 0 {
		if w < h {
			return w
		}
		return h
	}
	return h
}

func codecMatchesPreference(vcodec, pref string) bool {
	v := strings.ToLower(vcodec)
	switch normalizeVideoCodec(pref) {
	case VideoCodecH264:
		return strings.HasPrefix(v, "avc") || strings.HasPrefix(v, "h264")
	case VideoCodecVP9:
		return strings.HasPrefix(v, "vp09") || strings.HasPrefix(v, "vp9")
	case VideoCodecAV1:
		return strings.HasPrefix(v, "av01")
	}
	return true
}

var previewBuckets = []struct {
	Quality string
	Dim     int
	Label   string
}{
	{"2160", 2160, "4K (2160p)"},
	{"1080", 1080, "1080p"},
	{"720", 720, "720p"},
	{"480", 480, "480p"},
	{"360", 360, "360p"},
}

// buildMediaPreview converts raw yt-dlp single-video JSON into a compact preview with
// estimated download sizes per quality preset (mirroring GrabAll's quality menu).
func buildMediaPreview(raw []byte, sourceURL, preferredCodec string) (*MediaPreview, error) {
	start := strings.IndexByte(string(raw), '{')
	end := strings.LastIndexByte(string(raw), '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("yt-dlp did not return media metadata")
	}
	var info ytPreviewJSON
	if err := json.Unmarshal(raw[start:end+1], &info); err != nil {
		return nil, fmt.Errorf("failed to parse media metadata: %w", err)
	}
	if strings.EqualFold(info.Type, "playlist") {
		return nil, fmt.Errorf("this URL is a playlist — enable playlist mode to queue its videos")
	}

	p := &MediaPreview{
		URL:          sourceURL,
		ID:           info.ID,
		Title:        strings.TrimSpace(info.Title),
		Uploader:     strings.TrimSpace(info.Uploader),
		Thumbnail:    strings.TrimSpace(info.Thumbnail),
		Duration:     info.Duration,
		DurationText: formatPreviewDuration(info.Duration),
		IsLive:       (info.IsLive != nil && *info.IsLive) || strings.EqualFold(info.LiveStatus, "is_live"),
	}
	if p.Uploader == "" {
		p.Uploader = strings.TrimSpace(info.Channel)
	}
	if !strings.HasPrefix(p.Thumbnail, "https://") && !strings.HasPrefix(p.Thumbnail, "http://") {
		p.Thumbnail = ""
	}

	var videos, audios []ytPreviewFormat
	for _, f := range info.Formats {
		proto := strings.ToLower(f.Protocol)
		if strings.Contains(proto, "mhtml") || strings.EqualFold(f.Ext, "mhtml") {
			continue
		}
		hasV := !isNoneCodec(f.VCodec) && formatDim(f) > 0
		hasA := !isNoneCodec(f.ACodec)
		if hasV {
			videos = append(videos, f)
		} else if hasA {
			audios = append(audios, f)
		}
	}

	// Best audio-only stream = highest bitrate (fallback largest size).
	var bestAudio *ytPreviewFormat
	for i := range audios {
		a := &audios[i]
		if bestAudio == nil || a.TBR > bestAudio.TBR ||
			(a.TBR == bestAudio.TBR && int64(a.Filesize) > int64(bestAudio.Filesize)) {
			bestAudio = a
		}
	}
	audioSize, audioApprox := int64(0), true
	if bestAudio != nil {
		audioSize, audioApprox = estimateFormatSize(*bestAudio, info.Duration)
	}

	pickVideo := func(maxDim int) *ytPreviewFormat {
		bestDim := 0
		for _, v := range videos {
			d := formatDim(v)
			if (maxDim <= 0 || d <= maxDim) && d > bestDim {
				bestDim = d
			}
		}
		if bestDim == 0 {
			return nil
		}
		var chosen *ytPreviewFormat
		chosenPref := false
		for i := range videos {
			v := &videos[i]
			if formatDim(*v) != bestDim {
				continue
			}
			pref := codecMatchesPreference(v.VCodec, preferredCodec)
			if chosen == nil || (pref && !chosenPref) ||
				(pref == chosenPref && v.TBR > chosen.TBR) {
				chosen = v
				chosenPref = pref
			}
		}
		return chosen
	}

	makeOption := func(quality, label string, v *ytPreviewFormat) PreviewOption {
		size, approx := estimateFormatSize(*v, info.Duration)
		opt := PreviewOption{
			Quality:    quality,
			Label:      label,
			VideoCodec: shortCodecName(v.VCodec),
		}
		if v.Width > 0 && v.Height > 0 {
			opt.Resolution = fmt.Sprintf("%dx%d", int(v.Width), int(v.Height))
		}
		if isNoneCodec(v.ACodec) && bestAudio != nil {
			size += audioSize
			approx = approx || audioApprox
			opt.AudioCodec = shortCodecName(bestAudio.ACodec)
		} else {
			opt.AudioCodec = shortCodecName(v.ACodec)
		}
		opt.SizeBytes = size
		opt.Approx = approx || size <= 0
		return opt
	}

	if best := pickVideo(0); best != nil {
		p.Options = append(p.Options, makeOption("best", fmt.Sprintf("Best (%dp)", formatDim(*best)), best))
		seenDims := map[int]bool{}
		for _, bucket := range previewBuckets {
			v := pickVideo(bucket.Dim)
			if v == nil {
				continue
			}
			d := formatDim(*v)
			// Only list presets that yield a distinct resolution near the bucket.
			if seenDims[d] || d < bucket.Dim*2/3 {
				continue
			}
			seenDims[d] = true
			label := bucket.Label
			if d != bucket.Dim {
				label = fmt.Sprintf("%s (%dp)", bucket.Label, d)
			}
			p.Options = append(p.Options, makeOption(bucket.Quality, label, v))
		}
	}

	if bestAudio != nil {
		p.Options = append(p.Options, PreviewOption{
			Quality:    "audio-best",
			Label:      "Audio only",
			AudioCodec: shortCodecName(bestAudio.ACodec),
			SizeBytes:  audioSize,
			Approx:     audioApprox || audioSize <= 0,
		})
	}

	for k := range info.Subtitles {
		if k != "live_chat" {
			p.SubtitleLangs = append(p.SubtitleLangs, k)
		}
	}
	sort.Strings(p.SubtitleLangs)
	for k := range info.AutomaticCaptions {
		if strings.HasSuffix(k, "-orig") {
			p.AutoSubtitleLangs = append(p.AutoSubtitleLangs, strings.TrimSuffix(k, "-orig"))
		}
	}
	if len(p.AutoSubtitleLangs) == 0 && len(info.AutomaticCaptions) > 0 && len(info.AutomaticCaptions) <= 12 {
		for k := range info.AutomaticCaptions {
			p.AutoSubtitleLangs = append(p.AutoSubtitleLangs, k)
		}
	}
	sort.Strings(p.AutoSubtitleLangs)
	if p.SubtitleLangs == nil {
		p.SubtitleLangs = []string{}
	}
	if p.AutoSubtitleLangs == nil {
		p.AutoSubtitleLangs = []string{}
	}

	p.Chapters = []PreviewChapter{}
	for _, c := range info.Chapters {
		p.Chapters = append(p.Chapters, PreviewChapter{Title: strings.TrimSpace(c.Title), Start: c.StartTime, End: c.EndTime})
		if len(p.Chapters) >= 200 {
			break
		}
	}
	if p.Options == nil {
		p.Options = []PreviewOption{}
	}
	return p, nil
}

// --- 5D. Preview metadata cache (reused by the hybrid downloader) ---

const (
	previewCacheTTL     = 20 * time.Minute
	previewCacheMaxSize = 24
)

type previewCacheEntry struct {
	raw []byte
	at  time.Time
}

var (
	previewCacheMu sync.Mutex
	previewCache   = make(map[string]previewCacheEntry)
)

func previewCacheKey(rawURL string) string {
	return strings.TrimSpace(rawURL)
}

func storePreviewCache(rawURL string, raw []byte) {
	key := previewCacheKey(rawURL)
	if key == "" || len(raw) == 0 {
		return
	}
	previewCacheMu.Lock()
	defer previewCacheMu.Unlock()
	now := time.Now()
	for k, e := range previewCache {
		if now.Sub(e.at) > previewCacheTTL {
			delete(previewCache, k)
		}
	}
	for len(previewCache) >= previewCacheMaxSize {
		oldestKey := ""
		var oldest time.Time
		for k, e := range previewCache {
			if oldestKey == "" || e.at.Before(oldest) {
				oldestKey, oldest = k, e.at
			}
		}
		delete(previewCache, oldestKey)
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	previewCache[key] = previewCacheEntry{raw: cp, at: now}
}

func lookupPreviewCache(rawURL string) []byte {
	key := previewCacheKey(rawURL)
	previewCacheMu.Lock()
	defer previewCacheMu.Unlock()
	e, ok := previewCache[key]
	if !ok {
		return nil
	}
	if time.Since(e.at) > previewCacheTTL {
		delete(previewCache, key)
		return nil
	}
	return e.raw
}

func dropPreviewCache(rawURL string) {
	previewCacheMu.Lock()
	delete(previewCache, previewCacheKey(rawURL))
	previewCacheMu.Unlock()
}

func clearAllPreviewCache() {
	previewCacheMu.Lock()
	previewCache = make(map[string]previewCacheEntry)
	previewCacheMu.Unlock()
}

// writeCachedInfoJSON writes cached preview metadata to a unique temp file usable with
// yt-dlp --load-info-json. Returns "" when nothing is cached.
func writeCachedInfoJSON(rawURL, taskID string) string {
	raw := lookupPreviewCache(rawURL)
	if len(raw) == 0 {
		return ""
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("graball_info_%s_%d.json", sanitizeFormatID(taskID, "task"), time.Now().UnixNano()))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return ""
	}
	return path
}

// --- 5E. Preview probe execution ---

type previewProbeRequest struct {
	URL        string
	CookieFile string
	VideoCodec string
}

// previewProbeFunc runs yt-dlp and returns raw single-video JSON. Overridable in tests.
var previewProbeFunc = runYtDlpPreviewProbe

func runYtDlpPreviewProbe(ctx context.Context, cfg *Config, req previewProbeRequest) ([]byte, error) {
	binPath, err := ensureYtDlpBinary(ctx, cfg, func(string) {})
	if err != nil {
		return nil, err
	}
	if isYouTubeURL(req.URL) && resolveQuickJSBinary() == "" {
		_, _ = DownloadQuickJSStandalone(resolveWritableDataSubdir("bin"))
	}
	cacheDir := resolveWritableDataSubdir("cache")
	_ = os.MkdirAll(cacheDir, 0755)
	downloadDir := cfg.GetDownloadDir()
	hasFFmpeg := resolveFFmpegBinary(cfg) != ""

	probeCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()

	args := buildYtDlpProbeArgs(cacheDir, downloadDir, "", "best", req.VideoCodec, req.CookieFile, hasFFmpeg)
	args = append(args, req.URL)

	cmd := exec.CommandContext(probeCtx, binPath, args...)
	configureSubprocess(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to launch yt-dlp: %w", err)
	}

	var stderrLines []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if t := cleanOutput(sc.Text()); t != "" {
				stderrLines = append(stderrLines, t)
				if len(stderrLines) > 20 {
					stderrLines = stderrLines[len(stderrLines)-20:]
				}
			}
		}
	}()
	rawOut, readErr := io.ReadAll(io.LimitReader(stdout, 32*1024*1024))
	wg.Wait()
	cmdErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if probeCtx.Err() != nil {
		return nil, fmt.Errorf("timed out reading media info")
	}
	if cmdErr != nil {
		if detail := formatYtDlpStderrError(stderrLines); detail != "" {
			return nil, fmt.Errorf("%s", detail)
		}
		return nil, cmdErr
	}
	if readErr != nil {
		return nil, readErr
	}
	return rawOut, nil
}

// PreviewMedia fetches (or reuses cached) yt-dlp metadata for a URL and returns
// a quality/size preview. The raw metadata is cached for the downloader to reuse.
func (qm *QueueManager) PreviewMedia(ctx context.Context, req AddRequest) (*MediaPreview, error) {
	target := strings.TrimSpace(req.URL)
	if target == "" {
		return nil, fmt.Errorf("URL cannot be empty")
	}
	if !strings.HasPrefix(strings.ToLower(target), "http://") && !strings.HasPrefix(strings.ToLower(target), "https://") {
		return nil, fmt.Errorf("only http(s) URLs can be previewed")
	}

	codec := req.VideoCodec
	if strings.TrimSpace(codec) == "" && qm.config != nil {
		qm.config.mu.RLock()
		codec = qm.config.VideoCodec
		qm.config.mu.RUnlock()
	}
	codec = normalizeVideoCodec(codec)

	if raw := lookupPreviewCache(target); raw != nil {
		if p, err := buildMediaPreview(raw, target, codec); err == nil {
			p.Cached = true
			return p, nil
		}
	}

	cookies := req.Cookies
	if isYouTubeURL(target) {
		if hasYouTubeAuthCookies(cookies) {
			qm.UpdateYouTubeCookies(cookies)
		} else if cached := qm.GetYouTubeCookies(); hasYouTubeAuthCookies(cached) {
			cookies = cached
		}
	}
	useCookies := len(cookies) > 0 && (!isYouTubeURL(target) || hasYouTubeAuthCookies(cookies))
	cookieFile := ""
	if useCookies {
		cookieFile = filepath.Join(os.TempDir(), fmt.Sprintf("graball_preview_cookies_%d.txt", time.Now().UnixNano()))
		if err := WriteNetscapeCookiesFile(cookies, cookieFile); err == nil {
			defer os.Remove(cookieFile)
		} else {
			cookieFile = ""
		}
	}

	raw, err := previewProbeFunc(ctx, qm.config, previewProbeRequest{URL: target, CookieFile: cookieFile, VideoCodec: codec})
	if err != nil && cookieFile != "" && ctx.Err() == nil && isYouTubeSignedInRefusalError(err.Error()) {
		raw, err = previewProbeFunc(ctx, qm.config, previewProbeRequest{URL: target, VideoCodec: codec})
	}
	if err != nil {
		return nil, err
	}
	p, err := buildMediaPreview(raw, target, codec)
	if err != nil {
		return nil, err
	}
	storePreviewCache(target, raw)
	return p, nil
}
