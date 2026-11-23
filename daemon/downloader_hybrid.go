package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- 2A. Smarter YouTube Cookie & Probe Error Classifiers ---

// isYouTubeSignInRequiredError returns true when the error specifically indicates a
// private, age-gated, members-only, or paid video where cookies MUST NOT be discarded.
func isYouTubeSignInRequiredError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	markers := []string{
		"private video",
		"confirm your age",
		"age-restricted",
		"members-only",
		"channel's members",
		"join this channel",
		"premium members",
		"requires payment",
		"available to this channel",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isYouTubeSignedInRefusalError returns true when YouTube appears to have rejected a
// signed-in cookie session on a public video and retrying without cookies can succeed.
func isYouTubeSignedInRefusalError(errMsg string) bool {
	if isYouTubeSignInRequiredError(errMsg) {
		return false
	}
	lower := strings.ToLower(errMsg)
	markers := []string{
		"not a bot",
		"http error 403",
		"po token",
		"requested format is not available",
		"only images are available",
		"sabr",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isYouTubePrivateAvailability reports whether yt-dlp metadata marks the video as
// restricted to authenticated/authorized accounts.
func isYouTubePrivateAvailability(availability string) bool {
	switch strings.ToLower(strings.TrimSpace(availability)) {
	case "private", "subscriber_only", "premium_only", "needs_auth":
		return true
	}
	return false
}

// isFatalYtDlpProbeError returns true for definitive metadata errors where falling back
// to another download attempt cannot recover.
func isFatalYtDlpProbeError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	markers := []string{
		"video unavailable",
		"private video",
		"confirm your age",
		"age-restricted",
		"members-only",
		"requires payment",
		"copyright",
		"this live event will begin",
		"premieres in",
		"not a bot",
		"unsupported url",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// --- 2B. Flexible yt-dlp -J JSON Schema & Eligibility Evaluator ---

// flexInt64 safely parses null, JSON integers, JSON floats, or numeric strings.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.TrimSpace(s[1 : len(s)-1])
		if s == "" || strings.EqualFold(s, "null") || strings.EqualFold(s, "na") {
			*f = 0
			return nil
		}
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt64(v)
		return nil
	}
	if fl, err := strconv.ParseFloat(s, 64); err == nil {
		if fl < 0 {
			*f = 0
		} else {
			*f = flexInt64(int64(fl))
		}
		return nil
	}
	return fmt.Errorf("invalid flexInt64: %s", string(data))
}

func isDRMValue(v interface{}) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		lower := strings.ToLower(strings.TrimSpace(val))
		return lower != "" && lower != "false" && lower != "none"
	}
	return true
}

type ytDlpFormatJSON struct {
	FormatID       string            `json:"format_id"`
	URL            string            `json:"url"`
	Ext            string            `json:"ext"`
	Protocol       string            `json:"protocol"`
	VCodec         string            `json:"vcodec"`
	ACodec         string            `json:"acodec"`
	Filesize       flexInt64         `json:"filesize"`
	FilesizeApprox flexInt64         `json:"filesize_approx"`
	HTTPHeaders    map[string]string `json:"http_headers"`
	Cookies        string            `json:"cookies"`
	HasDRM         interface{}       `json:"has_drm"`
}

type ytDlpInfoJSON struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Filename           string            `json:"filename"`
	UnderscoreFilename string            `json:"_filename"`
	Ext                string            `json:"ext"`
	IsLive             *bool             `json:"is_live"`
	WasLive            *bool             `json:"was_live"`
	LiveStatus         string            `json:"live_status"`
	Availability       string            `json:"availability"`
	HasDRM             interface{}       `json:"_has_drm"`
	URL                string            `json:"url"`
	FormatID           string            `json:"format_id"`
	Protocol           string            `json:"protocol"`
	VCodec             string            `json:"vcodec"`
	ACodec             string            `json:"acodec"`
	Filesize           flexInt64         `json:"filesize"`
	FilesizeApprox     flexInt64         `json:"filesize_approx"`
	HTTPHeaders        map[string]string `json:"http_headers"`
	Cookies            string            `json:"cookies"`
	RequestedFormats   []ytDlpFormatJSON `json:"requested_formats"`
}

func parseYtDlpInfoJSON(raw []byte) (*ytDlpInfoJSON, error) {
	start := strings.IndexByte(string(raw), '{')
	end := strings.LastIndexByte(string(raw), '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("yt-dlp did not emit valid JSON metadata")
	}
	var info ytDlpInfoJSON
	if err := json.Unmarshal(raw[start:end+1], &info); err != nil {
		return nil, fmt.Errorf("failed to parse yt-dlp metadata: %w", err)
	}
	return &info, nil
}

type hybridStreamSpec struct {
	Role         string // "video", "audio", "single"
	FormatID     string
	URL          string
	Ext          string
	DestPath     string
	ExpectedSize int64
	ApproxSize   int64
	Headers      map[string]string
}

type hybridMediaPlan struct {
	ID             string
	Title          string
	FinalPath      string
	Availability   string
	Streams        []hybridStreamSpec
	CombinedExact  int64
	CombinedApprox int64
}

func sanitizeFormatID(fid, fallback string) string {
	cleaned := invalidFileCharsRe.ReplaceAllString(strings.TrimSpace(fid), "_")
	cleaned = strings.ReplaceAll(cleaned, " ", "_")
	if len(cleaned) > 24 {
		cleaned = cleaned[:24]
	}
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func mergeStreamHeaders(base, override map[string]string, cookies string) map[string]string {
	merged := make(map[string]string)
	setCI := func(k, v string) {
		kTrim := strings.TrimSpace(k)
		if kTrim == "" {
			return
		}
		if strings.EqualFold(kTrim, "Accept-Encoding") || strings.EqualFold(kTrim, "Range") {
			return
		}
		for existing := range merged {
			if strings.EqualFold(existing, kTrim) {
				delete(merged, existing)
				break
			}
		}
		merged[kTrim] = v
	}
	for k, v := range base {
		setCI(k, v)
	}
	for k, v := range override {
		setCI(k, v)
	}
	if strings.TrimSpace(cookies) != "" {
		hasCookie := false
		for k := range merged {
			if strings.EqualFold(k, "Cookie") {
				hasCookie = true
				break
			}
		}
		if !hasCookie {
			merged["Cookie"] = cookies
		}
	}
	return merged
}

func isDirectHTTPStream(rawURL, protocol string) bool {
	lowerURL := strings.ToLower(strings.TrimSpace(rawURL))
	if !strings.HasPrefix(lowerURL, "http://") && !strings.HasPrefix(lowerURL, "https://") {
		return false
	}
	proto := strings.ToLower(strings.TrimSpace(protocol))
	return proto == "http" || proto == "https"
}

// evaluateHybridPlan checks whether yt-dlp metadata represents a non-live, non-DRM VOD
// whose 1 or 2 direct HTTP streams can be downloaded natively by GrabAll's Go engine.
func evaluateHybridPlan(info *ytDlpInfoJSON, downloadDir, customFile string, hasFFmpeg bool) (*hybridMediaPlan, string) {
	if info == nil {
		return nil, "missing metadata"
	}
	liveStatus := strings.ToLower(strings.TrimSpace(info.LiveStatus))
	if (info.IsLive != nil && *info.IsLive) || liveStatus == "is_live" || liveStatus == "is_upcoming" {
		return nil, "live or upcoming stream"
	}
	if isDRMValue(info.HasDRM) {
		return nil, "DRM protected"
	}

	var rawStreams []ytDlpFormatJSON
	if len(info.RequestedFormats) > 0 {
		rawStreams = make([]ytDlpFormatJSON, len(info.RequestedFormats))
		copy(rawStreams, info.RequestedFormats)
	} else if strings.TrimSpace(info.URL) != "" {
		rawStreams = []ytDlpFormatJSON{{
			FormatID:       info.FormatID,
			URL:            info.URL,
			Ext:            info.Ext,
			Protocol:       info.Protocol,
			VCodec:         info.VCodec,
			ACodec:         info.ACodec,
			Filesize:       info.Filesize,
			FilesizeApprox: info.FilesizeApprox,
			HTTPHeaders:    info.HTTPHeaders,
			Cookies:        info.Cookies,
		}}
	}

	if len(rawStreams) == 0 {
		return nil, "no downloadable stream URLs"
	}
	if len(rawStreams) > 2 {
		return nil, "more than two streams"
	}
	if len(rawStreams) == 2 && !hasFFmpeg {
		return nil, "FFmpeg unavailable for dual-stream merge"
	}

	for i := range rawStreams {
		if isDRMValue(rawStreams[i].HasDRM) {
			return nil, "DRM protected stream"
		}
		if !isDirectHTTPStream(rawStreams[i].URL, rawStreams[i].Protocol) {
			return nil, fmt.Sprintf("unsupported protocol %q", rawStreams[i].Protocol)
		}
		rawStreams[i].Ext = strings.TrimPrefix(strings.TrimSpace(rawStreams[i].Ext), ".")
		if rawStreams[i].Ext == "" {
			return nil, "missing stream extension"
		}
	}

	// Resolve final merged/single output path cleanly inside downloadDir.
	finalPath := strings.TrimSpace(info.UnderscoreFilename)
	if finalPath == "" {
		finalPath = strings.TrimSpace(info.Filename)
	}
	if finalPath == "" {
		baseName := strings.TrimSpace(customFile)
		if baseName == "" {
			titlePart := strings.TrimSpace(info.Title)
			if titlePart == "" {
				titlePart = "video"
			}
			if info.ID != "" {
				baseName = fmt.Sprintf("%s [%s]", titlePart, info.ID)
			} else {
				baseName = titlePart
			}
		}
		targetExt := ".mp4"
		if len(rawStreams) == 1 && rawStreams[0].Ext != "" {
			targetExt = "." + rawStreams[0].Ext
		}
		if !strings.HasSuffix(strings.ToLower(baseName), strings.ToLower(targetExt)) {
			if curExt := filepath.Ext(baseName); isKnownYtMediaExt(curExt) {
				baseName = strings.TrimSuffix(baseName, curExt)
			}
			baseName += targetExt
		}
		finalPath = filepath.Join(downloadDir, fitDirectFileName(downloadDir, sanitizeFileName(baseName)))
	} else {
		if !filepath.IsAbs(finalPath) && downloadDir != "" {
			finalPath = filepath.Join(downloadDir, finalPath)
		}
	}

	for strings.HasSuffix(strings.ToLower(finalPath), ".part") {
		finalPath = finalPath[:len(finalPath)-5]
	}

	if len(rawStreams) == 2 && hasFFmpeg {
		if curExt := filepath.Ext(finalPath); !strings.EqualFold(curExt, ".mp4") {
			finalPath = strings.TrimSuffix(finalPath, curExt) + ".mp4"
		}
	}

	plan := &hybridMediaPlan{
		ID:           strings.TrimSpace(info.ID),
		Title:        strings.TrimSpace(info.Title),
		FinalPath:    finalPath,
		Availability: strings.TrimSpace(info.Availability),
		Streams:      make([]hybridStreamSpec, 0, len(rawStreams)),
	}

	if len(rawStreams) == 2 {
		// Put video stream first and audio stream second.
		s0, s1 := rawStreams[0], rawStreams[1]
		isS0VideoOnly := s0.VCodec != "" && s0.VCodec != "none" && (s0.ACodec == "" || s0.ACodec == "none")
		isS1VideoOnly := s1.VCodec != "" && s1.VCodec != "none" && (s1.ACodec == "" || s1.ACodec == "none")
		s0HasVideo := s0.VCodec != "" && s0.VCodec != "none"
		s1HasVideo := s1.VCodec != "" && s1.VCodec != "none"
		if (!s0HasVideo && s1HasVideo) || (!isS0VideoOnly && isS1VideoOnly) {
			s0, s1 = s1, s0
		}

		finalStem := strings.TrimSuffix(finalPath, filepath.Ext(finalPath))
		roles := []string{"video", "audio"}
		fallbacks := []string{"v", "a"}
		ordered := []ytDlpFormatJSON{s0, s1}

		for idx, st := range ordered {
			fid := sanitizeFormatID(st.FormatID, fallbacks[idx])
			cookieStr := st.Cookies
			if cookieStr == "" {
				cookieStr = info.Cookies
			}
			exact := int64(st.Filesize)
			approx := exact
			if approx <= 0 {
				approx = int64(st.FilesizeApprox)
			}
			plan.Streams = append(plan.Streams, hybridStreamSpec{
				Role:         roles[idx],
				FormatID:     fid,
				URL:          st.URL,
				Ext:          st.Ext,
				DestPath:     fmt.Sprintf("%s.f%s.%s", finalStem, fid, st.Ext),
				ExpectedSize: exact,
				ApproxSize:   approx,
				Headers:      mergeStreamHeaders(info.HTTPHeaders, st.HTTPHeaders, cookieStr),
			})
		}
	} else {
		st := rawStreams[0]
		fid := sanitizeFormatID(st.FormatID, "0")
		cookieStr := st.Cookies
		if cookieStr == "" {
			cookieStr = info.Cookies
		}
		exact := int64(st.Filesize)
		approx := exact
		if approx <= 0 {
			approx = int64(st.FilesizeApprox)
		}
		plan.Streams = append(plan.Streams, hybridStreamSpec{
			Role:         "single",
			FormatID:     fid,
			URL:          st.URL,
			Ext:          st.Ext,
			DestPath:     finalPath,
			ExpectedSize: exact,
			ApproxSize:   approx,
			Headers:      mergeStreamHeaders(info.HTTPHeaders, st.HTTPHeaders, cookieStr),
		})
	}

	allExact := true
	for _, s := range plan.Streams {
		if s.ExpectedSize <= 0 {
			allExact = false
		} else {
			plan.CombinedExact += s.ExpectedSize
		}
		if s.ApproxSize > 0 {
			plan.CombinedApprox += s.ApproxSize
		}
	}
	if !allExact {
		plan.CombinedExact = 0
	}

	return plan, ""
}

// --- 2C. Hybrid Metadata Resolver ---

func resolveHybridMediaPlan(ctx context.Context, binPath, cacheDir, downloadDir, taskURL, customFile, quality, videoCodec, cookieFile string, hasFFmpeg bool) (*ytDlpInfoJSON, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	args := []string{
		"--force-ipv4",
		"--socket-timeout", "30",
		"--cache-dir", cacheDir,
		"--no-warnings",
		"--no-playlist",
		"--no-progress",
		"--dump-single-json",
		"--paths", fmt.Sprintf("home:%s", downloadDir),
		"--windows-filenames",
		"-o", fitYtDlpOutputTemplate(downloadDir, customFile),
	}

	if pluginsDir := ensureYtDlpSpeedPlugin(); pluginsDir != "" {
		args = append(args, "--plugin-dirs", pluginsDir)
	}

	args = append(args, "--js-runtimes", "deno", "--js-runtimes", "node", "--js-runtimes", "bun")
	if qjsPath := resolveQuickJSBinary(); qjsPath != "" {
		args = append(args, "--js-runtimes", "quickjs:"+qjsPath)
	} else {
		args = append(args, "--js-runtimes", "quickjs")
	}

	if hasFFmpeg {
		args = append(args, "-f", "bv*+ba/b", "--merge-output-format", "mp4")
	} else {
		args = append(args, "-f", "b/bv*+ba/bv*")
	}

	if sortOrder := buildYtDlpSortOrder(quality, videoCodec, ""); sortOrder != "" {
		args = append(args, "-S", sortOrder)
	}

	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}

	args = append(args, taskURL)

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
		return nil, fmt.Errorf("failed to launch yt-dlp probe: %w", err)
	}

	stopPipeClose := context.AfterFunc(probeCtx, func() {
		time.Sleep(250 * time.Millisecond)
		_ = stdout.Close()
		_ = stderr.Close()
	})
	defer stopPipeClose()

	var (
		stderrMu    sync.Mutex
		stderrLines []string
		wg          sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if probeCtx.Err() != nil {
				break
			}
			text := cleanOutput(sc.Text())
			if text != "" {
				stderrMu.Lock()
				stderrLines = append(stderrLines, text)
				if len(stderrLines) > 20 {
					stderrLines = stderrLines[len(stderrLines)-20:]
				}
				stderrMu.Unlock()
			}
		}
	}()

	rawOut, readErr := io.ReadAll(io.LimitReader(stdout, 32*1024*1024))
	wg.Wait()
	cmdErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if cmdErr != nil {
		stderrMu.Lock()
		detail := formatYtDlpStderrError(stderrLines)
		stderrMu.Unlock()
		if detail != "" {
			return nil, fmt.Errorf("%s (%w)", detail, cmdErr)
		}
		return nil, cmdErr
	}
	if readErr != nil {
		return nil, readErr
	}

	return parseYtDlpInfoJSON(rawOut)
}

// --- 2D. Reusable Multi-Connection Go HTTP Stream Fetcher ---

type httpStatusError struct {
	StatusCode int
	Message    string
}

func (e *httpStatusError) Error() string {
	return e.Message
}

func isHTTPStatusError(err error) bool {
	var hs *httpStatusError
	return errors.As(err, &hs)
}

type streamFetchOptions struct {
	Client          *http.Client
	URL             string
	Headers         map[string]string
	DestPath        string
	ExpectedSize    int64
	MaxWorkers      int
	BlockSize       int64
	StallTimeout    time.Duration
	MaxURLRefreshes int
	OnProgress      func(downloaded, total int64, rateBytesPerSec float64, workers int)
	OnStatus        func(msg string)
	RefreshURL      func(ctx context.Context) (newURL string, newHeaders map[string]string, err error)
}

func cloneHeaderMap(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	cp := make(map[string]string, len(h))
	for k, v := range h {
		cp[k] = v
	}
	return cp
}

func buildStreamHTTPRequest(ctx context.Context, rawURL string, headers map[string]string, rangeHdr string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid stream request: %w", err)
	}
	hasUA := false
	for k, v := range headers {
		if strings.EqualFold(k, "Accept-Encoding") || strings.EqualFold(k, "Range") {
			continue
		}
		if strings.EqualFold(k, "User-Agent") {
			hasUA = true
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if !hasUA {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	return req, nil
}

func finalizePartToDest(partPath, turboMetaPath, destPath string) error {
	if turboMetaPath != "" {
		_ = removeFileWithRetry(turboMetaPath, 3, 50*time.Millisecond)
	}
	_ = removeFileWithRetry(destPath, 5, 100*time.Millisecond)
	return renameFileWithRetry(partPath, destPath, 5, 100*time.Millisecond)
}

type streamEndpointState struct {
	mu           sync.RWMutex
	url          string
	headers      map[string]string
	gen          uint64
	refreshMu    sync.Mutex
	refreshCount int
	maxRefreshes int
	onStatus     func(msg string)
	refreshFn    func(ctx context.Context) (string, map[string]string, error)
}

func (s *streamEndpointState) current() (string, map[string]string, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.url, cloneHeaderMap(s.headers), s.gen
}

func (s *streamEndpointState) tryRefresh(ctx context.Context, seenGen uint64, statusCode int, reason string, afterSuccess func()) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.RLock()
	nowGen := s.gen
	s.mu.RUnlock()
	if nowGen != seenGen {
		return nil // Another worker already refreshed this endpoint.
	}

	if s.refreshFn == nil {
		if statusCode > 0 {
			return &httpStatusError{StatusCode: statusCode, Message: reason}
		}
		return fmt.Errorf("%s", reason)
	}
	if s.refreshCount >= s.maxRefreshes {
		msg := fmt.Sprintf("%s (URL refresh limit reached)", reason)
		if statusCode > 0 {
			return &httpStatusError{StatusCode: statusCode, Message: msg}
		}
		return fmt.Errorf("%s", msg)
	}

	s.refreshCount++
	if s.onStatus != nil {
		s.onStatus("Stream URL expired — refreshing signed link...")
	}

	newURL, newHeaders, err := s.refreshFn(ctx)
	if err != nil {
		if statusCode > 0 {
			return &httpStatusError{StatusCode: statusCode, Message: fmt.Sprintf("%s: %v", reason, err)}
		}
		return err
	}
	if strings.TrimSpace(newURL) == "" {
		if statusCode > 0 {
			return &httpStatusError{StatusCode: statusCode, Message: reason}
		}
		return fmt.Errorf("%s: refreshed URL is empty", reason)
	}

	s.mu.Lock()
	s.url = newURL
	if newHeaders != nil {
		s.headers = cloneHeaderMap(newHeaders)
	}
	s.gen++
	s.mu.Unlock()

	if afterSuccess != nil {
		afterSuccess()
	}
	return nil
}

// stallWatchdog cancels a per-request context when no bytes arrive for timeout.
type stallWatchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	cancel  context.CancelFunc
	timeout time.Duration
	stopped bool
}

func newStallWatchdog(parent context.Context, timeout time.Duration) (context.Context, *stallWatchdog) {
	rCtx, cancel := context.WithCancel(parent)
	w := &stallWatchdog{
		cancel:  cancel,
		timeout: timeout,
	}
	w.timer = time.AfterFunc(timeout, cancel)
	return rCtx, w
}

func (w *stallWatchdog) Kick() {
	w.mu.Lock()
	if !w.stopped && w.timer != nil {
		w.timer.Reset(w.timeout)
	}
	w.mu.Unlock()
}

func (w *stallWatchdog) Stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		if w.timer != nil {
			w.timer.Stop()
		}
	}
	w.mu.Unlock()
	w.cancel()
}

func pickStreamBlockSize(opts streamFetchOptions, prevMeta *turboStateMeta) int64 {
	if prevMeta != nil && prevMeta.BlockSize > 0 && (opts.ExpectedSize <= 0 || prevMeta.TotalBytes == opts.ExpectedSize) {
		return prevMeta.BlockSize
	}
	if opts.BlockSize > 0 {
		return opts.BlockSize
	}
	if opts.ExpectedSize >= 256<<20 {
		return 4 << 20
	}
	return turboBlockSize
}

func fetchStreamWithGoEngine(ctx context.Context, opts streamFetchOptions) (int64, error) {
	if strings.TrimSpace(opts.DestPath) == "" {
		return 0, fmt.Errorf("missing stream destination path")
	}
	if opts.Client == nil {
		opts.Client = &http.Client{
			Transport: directHTTPTransport,
			Timeout:   0,
		}
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 30 * time.Second
	}
	if opts.MaxURLRefreshes <= 0 {
		opts.MaxURLRefreshes = 8
	}

	partPath := opts.DestPath + ".part"
	turboMetaPath := partPath + ".turbo"

	// Fast path: destination stream file is already completely present on disk.
	if fi, err := os.Stat(opts.DestPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		_, partStatErr := os.Stat(partPath)
		if (opts.ExpectedSize > 0 && fi.Size() == opts.ExpectedSize) || (opts.ExpectedSize <= 0 && os.IsNotExist(partStatErr)) {
			if opts.OnProgress != nil {
				opts.OnProgress(fi.Size(), fi.Size(), 0, 0)
			}
			return fi.Size(), nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(opts.DestPath), 0755); err != nil {
		return 0, fmt.Errorf("failed to create download directory: %w", err)
	}

	endpoint := &streamEndpointState{
		url:          opts.URL,
		headers:      cloneHeaderMap(opts.Headers),
		maxRefreshes: opts.MaxURLRefreshes,
		onStatus:     opts.OnStatus,
		refreshFn:    opts.RefreshURL,
	}

	if opts.MaxWorkers <= 1 {
		return fetchStreamSequential(ctx, opts, endpoint, partPath, turboMetaPath)
	}
	return fetchStreamTurbo(ctx, opts, endpoint, partPath, turboMetaPath)
}

// copyStreamToOffset streams body into file at startOffset while updating liveBytes and resetting watchdog.
func copyStreamToOffset(ctx context.Context, file *os.File, body io.Reader, startOffset, maxLen int64, liveBytes *atomic.Int64, watchdog *stallWatchdog) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for maxLen <= 0 || written < maxLen {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		toRead := len(buf)
		if maxLen > 0 {
			rem := maxLen - written
			if rem < int64(toRead) {
				toRead = int(rem)
			}
		}
		n, rErr := body.Read(buf[:toRead])
		if n > 0 {
			watchdog.Kick()
			if _, wErr := file.WriteAt(buf[:n], startOffset+written); wErr != nil {
				return written, fmt.Errorf("disk write error: %w", wErr)
			}
			written += int64(n)
			liveBytes.Add(int64(n))
		}
		if rErr != nil {
			if rErr == io.EOF {
				if maxLen > 0 && written < maxLen {
					return written, fmt.Errorf("unexpected EOF (%d/%d bytes)", written, maxLen)
				}
				return written, nil
			}
			if ctx.Err() != nil {
				return written, ctx.Err()
			}
			return written, rErr
		}
	}
	return written, nil
}

func startStreamProgressTicker(totalPtr *atomic.Int64, liveBytes *atomic.Int64, initialDone int64, workerCount int, onProgress func(downloaded, total int64, rate float64, workers int), extraEverySecond func()) (stop func()) {
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		lastTick := time.Now()
		lastBytes := initialDone
		lastExtra := time.Now()

		for {
			select {
			case <-stopCh:
				return
			case now := <-ticker.C:
				cur := liveBytes.Load()
				total := totalPtr.Load()
				if total > 0 && cur > total {
					cur = total
				}
				elapsed := now.Sub(lastTick).Seconds()
				if elapsed <= 0.001 {
					elapsed = 0.001
				}
				delta := cur - lastBytes
				if delta < 0 {
					delta = 0
				}
				rate := float64(delta) / elapsed
				if onProgress != nil {
					onProgress(cur, total, rate, workerCount)
				}
				lastBytes = cur
				lastTick = now

				if extraEverySecond != nil && now.Sub(lastExtra) >= time.Second {
					extraEverySecond()
					lastExtra = now
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopCh)
		})
		wg.Wait()
	}
}

func fetchStreamSequential(ctx context.Context, opts streamFetchOptions, endpoint *streamEndpointState, partPath, turboMetaPath string) (int64, error) {
	// If switching from Turbo -> Regular mid-download, trim .part cleanly to the contiguous prefix.
	if meta, mErr := loadTurboMeta(turboMetaPath); mErr == nil {
		contig := meta.contiguousPrefixBytes()
		if fi, sErr := os.Stat(partPath); sErr == nil && fi.Size() > contig {
			_ = os.Truncate(partPath, contig)
		}
		_ = removeFileWithRetry(turboMetaPath, 3, 50*time.Millisecond)
	}

	var offset int64
	if fi, err := os.Stat(partPath); err == nil && !fi.IsDir() {
		offset = fi.Size()
	}
	if opts.ExpectedSize > 0 && offset == opts.ExpectedSize {
		if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
			return 0, err
		}
		return offset, nil
	}
	if opts.ExpectedSize > 0 && offset > opts.ExpectedSize {
		_ = os.Truncate(partPath, opts.ExpectedSize)
		if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
			return 0, err
		}
		return opts.ExpectedSize, nil
	}

	blockSize := pickStreamBlockSize(opts, nil)
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return 0, fmt.Errorf("failed to open partial file: %w", err)
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()

	var totalPtr, liveBytes atomic.Int64
	totalPtr.Store(opts.ExpectedSize)
	liveBytes.Store(offset)

	stopTicker := startStreamProgressTicker(&totalPtr, &liveBytes, offset, 1, opts.OnProgress, nil)
	defer stopTicker()

	const maxNetAttempts = 6
	for {
		total := totalPtr.Load()
		if total > 0 && offset >= total {
			break
		}

		end := offset + blockSize - 1
		if total > 0 && end >= total {
			end = total - 1
		}
		rangeStr := fmt.Sprintf("bytes=%d-%d", offset, end)

		chunkDone := false
		hitFullEOF := false
		netAttempts := 0

		for !chunkDone {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}

			curURL, curHeaders, gen := endpoint.current()
			reqCtx, watchdog := newStallWatchdog(ctx, opts.StallTimeout)
			req, rErr := buildStreamHTTPRequest(reqCtx, curURL, curHeaders, rangeStr)
			if rErr != nil {
				watchdog.Stop()
				return 0, rErr
			}

			resp, dErr := opts.Client.Do(req)
			if dErr != nil {
				watchdog.Stop()
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				netAttempts++
				if netAttempts >= 3 && opts.RefreshURL != nil {
					_ = endpoint.tryRefresh(ctx, gen, 0, dErr.Error(), nil)
				}
				if netAttempts >= maxNetAttempts {
					return 0, dErr
				}
				backoff := time.Duration(500*(1<<(netAttempts-1))) * time.Millisecond
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				select {
				case <-ctx.Done():
					return 0, ctx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
				_ = resp.Body.Close()
				watchdog.Stop()
				if refErr := endpoint.tryRefresh(ctx, gen, resp.StatusCode, fmt.Sprintf("server returned HTTP %d", resp.StatusCode), nil); refErr != nil {
					return 0, refErr
				}
				continue
			}

			if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
				_ = resp.Body.Close()
				watchdog.Stop()
				if opts.ExpectedSize > 0 && offset >= opts.ExpectedSize {
					offset = opts.ExpectedSize
					hitFullEOF = true
					chunkDone = true
					break
				}
				// Range no longer valid; restart from 0
				_ = file.Truncate(0)
				offset = 0
				liveBytes.Store(0)
				chunkDone = true
				break
			}

			if resp.StatusCode == http.StatusOK {
				// Server ignored Range: stream full response sequentially from offset 0.
				if offset > 0 {
					_ = file.Truncate(0)
					offset = 0
					liveBytes.Store(0)
				}
				expFull := resp.ContentLength
				if expFull > 0 {
					totalPtr.Store(expFull)
				} else if opts.ExpectedSize > 0 {
					expFull = opts.ExpectedSize
					totalPtr.Store(expFull)
				}
				written, cErr := copyStreamToOffset(reqCtx, file, resp.Body, 0, expFull, &liveBytes, watchdog)
				_ = resp.Body.Close()
				watchdog.Stop()
				offset = written
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				if cErr == nil {
					if totalPtr.Load() <= 0 {
						totalPtr.Store(offset)
					}
					hitFullEOF = true
					chunkDone = true
					break
				}
				netAttempts++
				if netAttempts >= maxNetAttempts {
					return 0, cErr
				}
				_ = file.Truncate(0)
				offset = 0
				liveBytes.Store(0)
				continue
			}

			if resp.StatusCode != http.StatusPartialContent {
				code := resp.StatusCode
				_ = resp.Body.Close()
				watchdog.Stop()
				return 0, &httpStatusError{
					StatusCode: code,
					Message:    fmt.Sprintf("server returned HTTP %d", code),
				}
			}

			if crTotal := parseContentRangeTotal(resp.Header.Get("Content-Range")); crTotal > 0 {
				totalPtr.Store(crTotal)
			}
			curTotal := totalPtr.Load()
			expLen := resp.ContentLength
			if expLen <= 0 {
				expLen = end - offset + 1
				if curTotal > 0 && offset+expLen > curTotal {
					expLen = curTotal - offset
				}
			}

			requestedLen := end - offset + 1
			written, cErr := copyStreamToOffset(reqCtx, file, resp.Body, offset, expLen, &liveBytes, watchdog)
			_ = resp.Body.Close()
			watchdog.Stop()
			offset += written

			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if cErr != nil {
				netAttempts++
				if netAttempts >= 3 && opts.RefreshURL != nil {
					_ = endpoint.tryRefresh(ctx, gen, 0, cErr.Error(), nil)
				}
				if netAttempts >= maxNetAttempts {
					return 0, cErr
				}
				rangeStr = fmt.Sprintf("bytes=%d-%d", offset, end)
				continue
			}

			if curTotal <= 0 && resp.ContentLength >= 0 && resp.ContentLength < requestedLen {
				totalPtr.Store(offset)
				hitFullEOF = true
			}
			chunkDone = true
		}

		if hitFullEOF {
			break
		}
	}

	stopTicker()
	finalSize := offset
	if curTotal := totalPtr.Load(); curTotal > 0 {
		finalSize = curTotal
	}
	if err := file.Truncate(finalSize); err != nil {
		return 0, err
	}
	_ = file.Close()
	fileClosed = true

	if opts.OnProgress != nil {
		opts.OnProgress(finalSize, finalSize, 0, 1)
	}
	if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
		return 0, err
	}
	return finalSize, nil
}

func fetchStreamTurbo(ctx context.Context, opts streamFetchOptions, endpoint *streamEndpointState, partPath, turboMetaPath string) (int64, error) {
	prevMeta, prevMetaErr := loadTurboMeta(turboMetaPath)
	blockSize := pickStreamBlockSize(opts, prevMeta)

	// Probe block 0 to confirm Range support, authoritative totalBytes, and reuse first body.
	var (
		probeResp     *http.Response
		probeWatchdog *stallWatchdog
		probeCtx      context.Context
	)

	const maxProbeAttempts = 4
	netAttempts := 0
	for {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		curURL, curHeaders, gen := endpoint.current()
		pCtx, watchdog := newStallWatchdog(ctx, opts.StallTimeout)
		req, rErr := buildStreamHTTPRequest(pCtx, curURL, curHeaders, fmt.Sprintf("bytes=0-%d", blockSize-1))
		if rErr != nil {
			watchdog.Stop()
			return 0, rErr
		}

		resp, dErr := opts.Client.Do(req)
		if dErr != nil {
			watchdog.Stop()
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			netAttempts++
			if netAttempts >= 2 && opts.RefreshURL != nil {
				_ = endpoint.tryRefresh(ctx, gen, 0, dErr.Error(), nil)
			}
			if netAttempts >= maxProbeAttempts {
				return 0, dErr
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(netAttempts*300) * time.Millisecond):
			}
			continue
		}

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
			_ = resp.Body.Close()
			watchdog.Stop()
			if refErr := endpoint.tryRefresh(ctx, gen, resp.StatusCode, fmt.Sprintf("server returned HTTP %d", resp.StatusCode), nil); refErr != nil {
				return 0, refErr
			}
			continue
		}

		probeResp = resp
		probeWatchdog = watchdog
		probeCtx = pCtx
		break
	}

	cleanupProbe := func() {
		if probeResp != nil && probeResp.Body != nil {
			_ = probeResp.Body.Close()
			probeResp.Body = nil
		}
		if probeWatchdog != nil {
			probeWatchdog.Stop()
			probeWatchdog = nil
		}
	}

	if probeResp.StatusCode == http.StatusOK {
		// Server ignored Range; stream sequentially from probeResp.Body.
		file, err := os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
		if err != nil {
			cleanupProbe()
			return 0, fmt.Errorf("failed to open target file: %w", err)
		}
		expTotal := probeResp.ContentLength
		if expTotal <= 0 && opts.ExpectedSize > 0 {
			expTotal = opts.ExpectedSize
		}
		var totalPtr, liveBytes atomic.Int64
		totalPtr.Store(expTotal)
		stopTicker := startStreamProgressTicker(&totalPtr, &liveBytes, 0, 1, opts.OnProgress, nil)
		written, cErr := copyStreamToOffset(probeCtx, file, probeResp.Body, 0, probeResp.ContentLength, &liveBytes, probeWatchdog)
		stopTicker()
		cleanupProbe()
		_ = file.Close()
		if cErr != nil {
			return 0, cErr
		}
		if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
			return 0, err
		}
		return written, nil
	}

	if probeResp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		cleanupProbe()
		return fetchStreamSequential(ctx, opts, endpoint, partPath, turboMetaPath)
	}

	if probeResp.StatusCode != http.StatusPartialContent {
		code := probeResp.StatusCode
		cleanupProbe()
		return 0, &httpStatusError{
			StatusCode: code,
			Message:    fmt.Sprintf("server returned HTTP %d", code),
		}
	}

	totalBytes := parseContentRangeTotal(probeResp.Header.Get("Content-Range"))
	if totalBytes <= 0 {
		totalBytes = opts.ExpectedSize
	}
	if totalBytes <= 0 {
		if probeResp.ContentLength > 0 && probeResp.ContentLength < blockSize {
			totalBytes = probeResp.ContentLength
		} else {
			// Cannot determine total content length for parallel block table; fall back safely to sequential.
			cleanupProbe()
			return fetchStreamSequential(ctx, opts, endpoint, partPath, turboMetaPath)
		}
	}

	numBlocks := int((totalBytes + blockSize - 1) / blockSize)
	openFlags := os.O_CREATE | os.O_RDWR
	var meta *turboStateMeta

	partInfo, partStatErr := os.Stat(partPath)
	partExists := partStatErr == nil && !partInfo.IsDir()

	if prevMetaErr == nil && prevMeta != nil && prevMeta.TotalBytes == totalBytes && prevMeta.BlockSize == blockSize && len(prevMeta.DoneBlocks) == numBlocks && partExists {
		meta = prevMeta
	} else if partExists && partInfo.Size() > 0 && partInfo.Size() <= totalBytes {
		// Switched Regular -> Turbo or previous run had no .turbo metadata: adopt full contiguous blocks.
		meta = &turboStateMeta{
			TotalBytes: totalBytes,
			BlockSize:  blockSize,
			DoneBlocks: make([]bool, numBlocks),
		}
		fullBlocks := int(partInfo.Size() / blockSize)
		for i := 0; i < fullBlocks && i < numBlocks; i++ {
			meta.DoneBlocks[i] = true
		}
	} else {
		openFlags |= os.O_TRUNC
		meta = &turboStateMeta{
			TotalBytes: totalBytes,
			BlockSize:  blockSize,
			DoneBlocks: make([]bool, numBlocks),
		}
	}

	file, err := os.OpenFile(partPath, openFlags, 0644)
	if err != nil {
		cleanupProbe()
		return 0, fmt.Errorf("failed to open partial file: %w", err)
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()

	useProbeForBlock0 := !meta.DoneBlocks[0] && numBlocks > 0
	if !useProbeForBlock0 {
		cleanupProbe()
	}

	pending := make([]int, 0, numBlocks)
	for i := 0; i < numBlocks; i++ {
		if meta.DoneBlocks[i] {
			continue
		}
		if i == 0 && useProbeForBlock0 {
			continue
		}
		pending = append(pending, i)
	}

	totalTodo := len(pending)
	if useProbeForBlock0 {
		totalTodo++
	}
	if totalTodo == 0 {
		cleanupProbe()
		_ = file.Truncate(totalBytes)
		_ = file.Close()
		fileClosed = true
		if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
			return 0, err
		}
		return totalBytes, nil
	}

	workerCount := opts.MaxWorkers
	if totalTodo < workerCount {
		workerCount = totalTodo
	}
	if workerCount < 1 {
		workerCount = 1
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	initialDone := meta.completedBytes()
	var (
		metaMu    sync.Mutex
		nextIdx   atomic.Int64
		totalPtr  atomic.Int64
		liveBytes atomic.Int64
		errOnce   sync.Once
		firstErr  error
		wg        sync.WaitGroup
	)
	totalPtr.Store(totalBytes)
	liveBytes.Store(initialDone)

	saveMetaSafe := func() {
		metaMu.Lock()
		saveTurboMeta(turboMetaPath, meta)
		metaMu.Unlock()
	}

	recordErr := func(e error) {
		if e == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = e
			cancelWorkers()
		})
	}

	stopTicker := startStreamProgressTicker(&totalPtr, &liveBytes, initialDone, workerCount, opts.OnProgress, saveMetaSafe)
	defer stopTicker()

	writeStreamToBlock := func(rCtx context.Context, r io.Reader, blockIdx int, watchdog *stallWatchdog) error {
		start := int64(blockIdx) * blockSize
		expLen := blockSize
		if start+expLen > totalBytes {
			expLen = totalBytes - start
		}
		buf := make([]byte, 32*1024)
		var written int64
		for written < expLen {
			if workerCtx.Err() != nil {
				liveBytes.Add(-written)
				return workerCtx.Err()
			}
			if rCtx.Err() != nil {
				liveBytes.Add(-written)
				if workerCtx.Err() != nil {
					return workerCtx.Err()
				}
				return fmt.Errorf("stream stalled in block %d (%d/%d bytes)", blockIdx, written, expLen)
			}
			toRead := len(buf)
			if rem := int(expLen - written); rem < toRead {
				toRead = rem
			}
			n, rErr := r.Read(buf[:toRead])
			if n > 0 {
				if watchdog != nil {
					watchdog.Kick()
				}
				if _, wErr := file.WriteAt(buf[:n], start+written); wErr != nil {
					liveBytes.Add(-written)
					return fmt.Errorf("disk write error: %w", wErr)
				}
				written += int64(n)
				liveBytes.Add(int64(n))
			}
			if rErr != nil {
				if rErr == io.EOF && written == expLen {
					break
				}
				liveBytes.Add(-written)
				if rErr == io.EOF {
					return fmt.Errorf("unexpected EOF in block %d (%d/%d)", blockIdx, written, expLen)
				}
				return rErr
			}
		}
		metaMu.Lock()
		meta.DoneBlocks[blockIdx] = true
		metaMu.Unlock()
		return nil
	}

	fetchBlockWithRetry := func(blockIdx int) error {
		start := int64(blockIdx) * blockSize
		end := start + blockSize - 1
		if end >= totalBytes {
			end = totalBytes - 1
		}
		rangeStr := fmt.Sprintf("bytes=%d-%d", start, end)

		var lastErr error
		netAttempt := 0
		const maxNetAttempts = 6

		for {
			if workerCtx.Err() != nil {
				return workerCtx.Err()
			}

			curURL, curHeaders, gen := endpoint.current()
			reqCtx, watchdog := newStallWatchdog(workerCtx, opts.StallTimeout)
			bReq, rErr := buildStreamHTTPRequest(reqCtx, curURL, curHeaders, rangeStr)
			if rErr != nil {
				watchdog.Stop()
				return rErr
			}

			bResp, dErr := opts.Client.Do(bReq)
			if dErr != nil {
				watchdog.Stop()
				if workerCtx.Err() != nil {
					return workerCtx.Err()
				}
				lastErr = dErr
				netAttempt++
				if netAttempt >= 3 && opts.RefreshURL != nil {
					_ = endpoint.tryRefresh(workerCtx, gen, 0, dErr.Error(), saveMetaSafe)
				}
				if netAttempt >= maxNetAttempts {
					return lastErr
				}
				backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				select {
				case <-workerCtx.Done():
					return workerCtx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			if bResp.StatusCode == http.StatusForbidden || bResp.StatusCode == http.StatusGone {
				code := bResp.StatusCode
				_ = bResp.Body.Close()
				watchdog.Stop()
				if refErr := endpoint.tryRefresh(workerCtx, gen, code, fmt.Sprintf("block %d returned HTTP %d", blockIdx, code), saveMetaSafe); refErr != nil {
					return refErr
				}
				continue
			}

			if bResp.StatusCode != http.StatusPartialContent {
				code := bResp.StatusCode
				_ = bResp.Body.Close()
				watchdog.Stop()
				lastErr = &httpStatusError{
					StatusCode: code,
					Message:    fmt.Sprintf("block %d returned HTTP %d", blockIdx, code),
				}
				netAttempt++
				if netAttempt >= maxNetAttempts {
					return lastErr
				}
				backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				select {
				case <-workerCtx.Done():
					return workerCtx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			wErr := writeStreamToBlock(reqCtx, bResp.Body, blockIdx, watchdog)
			_ = bResp.Body.Close()
			watchdog.Stop()
			if wErr == nil {
				return nil
			}
			if workerCtx.Err() != nil {
				return workerCtx.Err()
			}
			lastErr = wErr
			netAttempt++
			if netAttempt >= 3 && opts.RefreshURL != nil {
				_ = endpoint.tryRefresh(workerCtx, gen, 0, wErr.Error(), saveMetaSafe)
			}
			if netAttempt >= maxNetAttempts {
				return lastErr
			}
			backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-workerCtx.Done():
				return workerCtx.Err()
			case <-time.After(backoff):
			}
		}
	}

	// Ensure probe body is consumed or cleaned up by worker 0.
	var initialBody io.ReadCloser
	var initialWatchdog *stallWatchdog
	var initialReqCtx context.Context
	if useProbeForBlock0 {
		initialBody = probeResp.Body
		initialWatchdog = probeWatchdog
		initialReqCtx = probeCtx
	}

	for w := 0; w < workerCount; w++ {
		var wBody io.ReadCloser
		var wDog *stallWatchdog
		var wCtx context.Context
		if w == 0 && initialBody != nil {
			wBody = initialBody
			wDog = initialWatchdog
			wCtx = initialReqCtx
			initialBody = nil
		}
		wg.Add(1)
		go func(initCtx context.Context, initBody io.ReadCloser, initDog *stallWatchdog) {
			defer wg.Done()
			if initBody != nil {
				err0 := writeStreamToBlock(initCtx, initBody, 0, initDog)
				_ = initBody.Close()
				if initDog != nil {
					initDog.Stop()
				}
				if err0 != nil {
					if workerCtx.Err() != nil {
						recordErr(workerCtx.Err())
						return
					}
					if retry0 := fetchBlockWithRetry(0); retry0 != nil {
						recordErr(retry0)
						return
					}
				}
			}

			for {
				if workerCtx.Err() != nil {
					return
				}
				pos := int(nextIdx.Add(1) - 1)
				if pos >= len(pending) {
					return
				}
				if errB := fetchBlockWithRetry(pending[pos]); errB != nil {
					recordErr(errB)
					return
				}
			}
		}(wCtx, wBody, wDog)
	}

	wg.Wait()
	stopTicker()

	if ctx.Err() != nil {
		saveMetaSafe()
		return 0, ctx.Err()
	}
	if firstErr != nil {
		saveMetaSafe()
		return 0, firstErr
	}

	for i, done := range meta.DoneBlocks {
		if !done {
			saveMetaSafe()
			return 0, fmt.Errorf("incomplete stream download: block %d missing", i)
		}
	}

	if err := file.Truncate(totalBytes); err != nil {
		return 0, err
	}
	_ = file.Close()
	fileClosed = true

	fi, statErr := os.Stat(partPath)
	if statErr != nil {
		return 0, fmt.Errorf("failed to verify downloaded stream file: %w", statErr)
	}
	if fi.Size() != totalBytes {
		return 0, fmt.Errorf("downloaded stream size mismatch (%d vs %d expected)", fi.Size(), totalBytes)
	}

	if opts.OnProgress != nil {
		opts.OnProgress(totalBytes, totalBytes, 0, workerCount)
	}
	if err := finalizePartToDest(partPath, turboMetaPath, opts.DestPath); err != nil {
		return 0, err
	}
	return totalBytes, nil
}

// --- 2E. Merge-Ready Checkpoint, Live Merge % Watcher & Stream-Preserving FFmpeg Merge ---

type hybridMergeState struct {
	FinalPath  string `json:"finalPath"`
	VideoPath  string `json:"videoPath"`
	VideoSize  int64  `json:"videoSize"`
	AudioPath  string `json:"audioPath"`
	AudioSize  int64  `json:"audioSize"`
	Quality    string `json:"quality"`
	VideoCodec string `json:"videoCodec"`
	Title      string `json:"title"`
}

func mergeStatePathForFinal(finalPath string) string {
	stem := strings.TrimSuffix(finalPath, filepath.Ext(finalPath))
	return stem + ".merge.json"
}

func saveMergeState(path string, st *hybridMergeState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	_ = removeFileWithRetry(path, 3, 50*time.Millisecond)
	if err := renameFileWithRetry(tmp, path, 4, 50*time.Millisecond); err != nil {
		_ = removeFileWithRetry(tmp, 2, 20*time.Millisecond)
		return err
	}
	return nil
}

func loadMergeStateIfValid(path, quality, videoCodec string) *hybridMergeState {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st hybridMergeState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	if st.FinalPath == "" || st.VideoPath == "" || st.AudioPath == "" || st.VideoSize <= 0 || st.AudioSize <= 0 {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(st.Quality), strings.TrimSpace(quality)) ||
		!strings.EqualFold(normalizeVideoCodec(st.VideoCodec), normalizeVideoCodec(videoCodec)) {
		return nil
	}
	vfi, vErr := os.Stat(st.VideoPath)
	afi, aErr := os.Stat(st.AudioPath)
	if vErr != nil || vfi.IsDir() || vfi.Size() != st.VideoSize {
		return nil
	}
	if aErr != nil || afi.IsDir() || afi.Size() != st.AudioSize {
		return nil
	}
	return &st
}

func findReusableMergeState(downloadDir string, task *DownloadTask, quality, videoCodec string) (*hybridMergeState, string) {
	if task == nil || downloadDir == "" {
		return nil, ""
	}
	task.mu.Lock()
	sp := task.SavePath
	fn := task.FileName
	title := task.Title
	taskURL := task.URL
	task.mu.Unlock()

	if sp != "" {
		cand := mergeStatePathForFinal(sp)
		if !filepath.IsAbs(cand) {
			cand = filepath.Join(downloadDir, cand)
		}
		if st := loadMergeStateIfValid(cand, quality, videoCodec); st != nil {
			return st, cand
		}
	}

	stems := extractCandidateStems(sp, fn, title)
	if vid := extractYouTubeVideoID(taskURL); vid != "" {
		bracketID := "[" + vid + "]"
		stems = append(stems, bracketID)
	}
	if len(stems) == 0 {
		return nil, ""
	}

	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		return nil, ""
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".merge.json") {
			continue
		}
		lower := strings.ToLower(entry.Name())
		for _, stem := range stems {
			if stem != "" && strings.Contains(lower, strings.ToLower(stem)) {
				cand := filepath.Join(downloadDir, entry.Name())
				if st := loadMergeStateIfValid(cand, quality, videoCodec); st != nil {
					return st, cand
				}
				break
			}
		}
	}
	return nil, ""
}

func startMergeProgressWatcher(ctx context.Context, qm *QueueManager, task *DownloadTask, tempPath string, expectedBytes int64) (stop func()) {
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		var (
			lastSize  int64
			lastTime  = time.Now()
			smoothBps float64
		)

		for {
			select {
			case <-stopCh:
				return
			case now := <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				fi, err := os.Stat(tempPath)
				if err != nil || fi.IsDir() || fi.Size() <= 0 {
					continue
				}
				curSize := fi.Size()
				elapsed := now.Sub(lastTime).Seconds()
				if elapsed > 0.05 && curSize > lastSize {
					instRate := float64(curSize-lastSize) / elapsed
					if smoothBps == 0 {
						smoothBps = instRate
					} else {
						smoothBps = smoothBps*0.5 + instRate*0.5
					}
					lastSize = curSize
					lastTime = now
				} else if now.Sub(lastTime) > 2*time.Second && curSize == lastSize {
					smoothBps = 0
				}

				if expectedBytes > 0 {
					mergePct := float64(curSize) / float64(expectedBytes) * 100.0
					if mergePct < 0 {
						mergePct = 0
					}
					if mergePct > 100.0 {
						mergePct = 100.0
					}
					overallPct := 99.0 + (mergePct/100.0)*0.9

					speedText := fmt.Sprintf("Merging Video + Audio... %.0f%%", mergePct)
					etaText := ""
					if smoothBps > 0 {
						speedText = fmt.Sprintf("Merging Video + Audio... %.0f%% (%s/s)", mergePct, formatBytes(int64(smoothBps)))
						if expectedBytes > curSize {
							etaText = formatDuration(int64(float64(expectedBytes-curSize) / smoothBps))
						}
					}

					task.mu.Lock()
					stillActive := task.Status == StatusDownloading
					if stillActive {
						if overallPct > task.Progress {
							task.Progress = overallPct
						}
						task.Speed = speedText
						task.ETA = etaText
					}
					task.mu.Unlock()
					if stillActive {
						qm.BroadcastUpdate()
					}
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopCh)
		})
		wg.Wait()
	}
}

func isDiskSpaceError(msg string) bool {
	lower := strings.ToLower(msg)
	markers := []string{
		"no space left",
		"not enough space",
		"disk full",
		"insufficient disk",
		"enospc",
		"0x70",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func mergeHybridStreamsWithProgress(ctx context.Context, qm *QueueManager, task *DownloadTask, ffmpegBin string, st *hybridMergeState, statePath string) error {
	ext := filepath.Ext(st.FinalPath)
	if ext == "" {
		ext = ".mp4"
	}
	tempMergedPath := strings.TrimSuffix(st.FinalPath, ext) + ".temp" + ext
	_ = removeFileWithRetry(tempMergedPath, 3, 50*time.Millisecond)

	expectedCombined := st.VideoSize + st.AudioSize

	task.mu.Lock()
	if task.Status == StatusDownloading {
		if task.Progress < 99.0 {
			task.Progress = 99.0
		}
		task.Speed = "Merging Video + Audio... 0%"
		task.ETA = ""
	}
	task.mu.Unlock()
	qm.BroadcastUpdate()

	stopWatcher := startMergeProgressWatcher(ctx, qm, task, tempMergedPath, expectedCombined)
	defer stopWatcher()

	cmd := exec.CommandContext(
		ctx,
		ffmpegBin,
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", st.VideoPath,
		"-i", st.AudioPath,
		"-c", "copy",
		"-map", "0:v:0",
		"-map", "1:a:0",
		"-strict", "-2",
		tempMergedPath,
	)
	configureSubprocess(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}

	stopPipeClose := context.AfterFunc(ctx, func() {
		time.Sleep(250 * time.Millisecond)
		_ = stderr.Close()
	})
	defer stopPipeClose()

	var (
		stderrMu    sync.Mutex
		stderrLines []string
		wg          sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if ctx.Err() != nil {
				break
			}
			line := cleanOutput(sc.Text())
			if line != "" {
				stderrMu.Lock()
				stderrLines = append(stderrLines, line)
				if len(stderrLines) > 20 {
					stderrLines = stderrLines[len(stderrLines)-20:]
				}
				stderrMu.Unlock()
			}
		}
	}()

	wg.Wait()
	cmdErr := cmd.Wait()
	stopWatcher()

	if ctx.Err() != nil {
		// Keep completed VideoPath, AudioPath, and .merge.json; remove only incomplete temp output.
		_ = removeFileWithRetry(tempMergedPath, 4, 100*time.Millisecond)
		return ctx.Err()
	}

	if cmdErr != nil {
		_ = removeFileWithRetry(tempMergedPath, 4, 100*time.Millisecond)
		stderrMu.Lock()
		detail := strings.Join(stderrLines, "; ")
		stderrMu.Unlock()
		if detail == "" {
			detail = cmdErr.Error()
		}
		if isDiskSpaceError(detail) {
			return fmt.Errorf("merge failed — not enough free disk space (%s). Your downloaded video & audio streams are safely preserved on disk; free up space and click Resume to finish merging without re-downloading", detail)
		}
		return fmt.Errorf("ffmpeg merge failed (%s). Downloaded video & audio streams are safely preserved on disk; click Resume to retry merging", detail)
	}

	fi, statErr := os.Stat(tempMergedPath)
	if statErr != nil || fi.IsDir() || fi.Size() == 0 {
		_ = removeFileWithRetry(tempMergedPath, 4, 100*time.Millisecond)
		return fmt.Errorf("merged file was empty after FFmpeg finished; source streams remain preserved on disk")
	}

	if integrityErr := checkMergedOutputIntegrity(false, expectedCombined, fi.Size(), 0); integrityErr != nil {
		_ = removeFileWithRetry(tempMergedPath, 4, 100*time.Millisecond)
		return fmt.Errorf("%w (downloaded streams preserved on disk for safe Resume)", integrityErr)
	}

	_ = removeFileWithRetry(st.FinalPath, 5, 100*time.Millisecond)
	if err := renameFileWithRetry(tempMergedPath, st.FinalPath, 5, 100*time.Millisecond); err != nil {
		return fmt.Errorf("failed to finalize merged video: %w", err)
	}

	_ = removeFileWithRetry(st.VideoPath, 4, 60*time.Millisecond)
	_ = removeFileWithRetry(st.AudioPath, 4, 60*time.Millisecond)
	_ = removeFileWithRetry(statePath, 4, 60*time.Millisecond)

	task.mu.Lock()
	task.SavePath = st.FinalPath
	task.FileName = filepath.Base(st.FinalPath)
	task.TotalBytes = fi.Size()
	task.DownloadedBytes = fi.Size()
	task.Speed = ""
	task.ETA = ""
	if task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://") {
		task.Title = strings.TrimSuffix(task.FileName, filepath.Ext(task.FileName))
	}
	task.mu.Unlock()
	qm.BroadcastUpdate()

	return nil
}

// --- 2F. Hybrid Orchestrator ---

func hasLegacyYtDlpCheckpoint(downloadDir string, task *DownloadTask) bool {
	if task == nil || downloadDir == "" {
		return false
	}
	task.mu.Lock()
	sp := task.SavePath
	fn := task.FileName
	title := task.Title
	taskURL := task.URL
	task.mu.Unlock()

	stems := extractCandidateStems(sp, fn, title)
	if vid := extractYouTubeVideoID(taskURL); vid != "" {
		stems = append(stems, "["+vid+"]")
	}
	if len(stems) == 0 {
		return false
	}

	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".ytdl") {
			continue
		}
		lower := strings.ToLower(entry.Name())
		for _, stem := range stems {
			if stem != "" && strings.Contains(lower, strings.ToLower(stem)) {
				return true
			}
		}
	}
	return false
}

func resolveStreamInitialDone(s hybridStreamSpec) int64 {
	if fi, err := os.Stat(s.DestPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return fi.Size()
	}
	partPath := s.DestPath + ".part"
	if meta, err := loadTurboMeta(partPath + ".turbo"); err == nil {
		return meta.completedBytes()
	}
	if fi, err := os.Stat(partPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return fi.Size()
	}
	return 0
}

func runHybridYouTubeDownload(ctx context.Context, qm *QueueManager, task *DownloadTask, binPath, downloadDir string, cookieFilePtr *string) (handled bool, err error) {
	if !isYouTubeURL(task.URL) {
		return false, nil
	}

	task.mu.Lock()
	quality := strings.ToLower(strings.TrimSpace(task.Quality))
	if quality == "" {
		quality = "best"
	}
	if strings.HasPrefix(quality, "audio") {
		task.mu.Unlock()
		return false, nil
	}
	customFile := task.CustomFileName
	taskCodec := task.VideoCodec
	task.mu.Unlock()

	if strings.TrimSpace(taskCodec) == "" && qm.config != nil {
		qm.config.mu.RLock()
		taskCodec = qm.config.VideoCodec
		qm.config.mu.RUnlock()
	}
	videoCodec := normalizeVideoCodec(taskCodec)

	// 1. Instant Merge-Only Resume Check when finished source streams exist from an earlier failed merge.
	if mergeState, statePath := findReusableMergeState(downloadDir, task, quality, videoCodec); mergeState != nil {
		ffmpegBin, fErr := ensureFFmpegBinary(ctx, qm.config, func(msg string) {
			task.mu.Lock()
			if task.Status == StatusDownloading {
				task.Speed = msg
			}
			task.mu.Unlock()
			qm.BroadcastUpdate()
		})
		if fErr != nil || ffmpegBin == "" {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, fmt.Errorf("FFmpeg is required to merge preserved streams: %v", fErr)
		}

		task.mu.Lock()
		if mergeState.Title != "" {
			task.Title = mergeState.Title
		}
		task.SavePath = mergeState.FinalPath
		task.FileName = filepath.Base(mergeState.FinalPath)
		task.TotalBytes = mergeState.VideoSize + mergeState.AudioSize
		task.DownloadedBytes = mergeState.VideoSize + mergeState.AudioSize
		task.Progress = 99.0
		task.mu.Unlock()
		qm.BroadcastUpdate()

		return true, mergeHybridStreamsWithProgress(ctx, qm, task, ffmpegBin, mergeState, statePath)
	}

	// 2. Preserve compatibility with any legacy yt-dlp dashy .ytdl checkpoint.
	if hasLegacyYtDlpCheckpoint(downloadDir, task) {
		return false, nil
	}

	// 3. Ensure FFmpeg upfront for HD qualities so yt-dlp -J picks dual high-res streams.
	needsFFmpeg := quality == "best" || quality == "2160" || quality == "4k" || quality == "1080" || quality == "720"
	if needsFFmpeg && resolveFFmpegBinary(qm.config) == "" {
		_, _ = ensureFFmpegBinary(ctx, qm.config, func(msg string) {
			task.mu.Lock()
			if task.Status == StatusDownloading {
				task.Speed = msg
			}
			task.mu.Unlock()
			qm.BroadcastUpdate()
		})
	}
	ffmpegBin := resolveFFmpegBinary(qm.config)
	hasFFmpeg := ffmpegBin != ""

	cacheDir := filepath.Join(executableDir(), "cache")
	_ = os.MkdirAll(cacheDir, 0755)
	defer pruneOldPlayerCaches(cacheDir)

	task.mu.Lock()
	if task.Status == StatusDownloading {
		task.Speed = "Extracting stream info..."
		task.ETA = ""
	}
	task.mu.Unlock()
	qm.BroadcastUpdate()

	info, probeErr := resolveHybridMediaPlan(ctx, binPath, cacheDir, downloadDir, task.URL, customFile, quality, videoCodec, *cookieFilePtr, hasFFmpeg)
	if probeErr != nil && ctx.Err() == nil && *cookieFilePtr != "" && isYouTubeSignedInRefusalError(probeErr.Error()) {
		*cookieFilePtr = ""
		task.mu.Lock()
		if task.Status == StatusDownloading {
			task.Speed = "Retrying without browser cookies..."
		}
		task.mu.Unlock()
		qm.BroadcastUpdate()
		info, probeErr = resolveHybridMediaPlan(ctx, binPath, cacheDir, downloadDir, task.URL, customFile, quality, videoCodec, "", hasFFmpeg)
	}

	if probeErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if isFatalYtDlpProbeError(probeErr.Error()) {
			return true, fmt.Errorf("yt-dlp failed: %w", probeErr)
		}
		return false, nil
	}

	plan, _ := evaluateHybridPlan(info, downloadDir, customFile, hasFFmpeg)
	if plan == nil {
		return false, nil
	}

	// Calculate initial sizes & resumed progress across all streams.
	streamDone := make([]int64, len(plan.Streams))
	streamSizes := make([]int64, len(plan.Streams))
	var initialDone int64
	for i, s := range plan.Streams {
		streamDone[i] = resolveStreamInitialDone(s)
		streamSizes[i] = s.ApproxSize
		initialDone += streamDone[i]
	}

	sumTotal := func() int64 {
		if plan.CombinedExact > 0 {
			return plan.CombinedExact
		}
		var tot int64
		for i, sz := range streamSizes {
			if sz > 0 {
				tot += sz
			} else if streamDone[i] > 0 {
				tot += streamDone[i]
			}
		}
		if tot == 0 {
			return plan.CombinedApprox
		}
		return tot
	}

	task.mu.Lock()
	if plan.Title != "" && (task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://")) {
		task.Title = plan.Title
	}
	task.SavePath = plan.FinalPath
	task.FileName = filepath.Base(plan.FinalPath)
	tot0 := sumTotal()
	if tot0 > 0 {
		task.TotalBytes = tot0
	}
	if initialDone > 0 {
		task.DownloadedBytes = initialDone
		if tot0 > 0 {
			pct := float64(initialDone) / float64(tot0) * 100.0
			if len(plan.Streams) > 1 && pct > 99.0 {
				pct = 99.0
			} else if pct > 100.0 {
				pct = 100.0
			}
			task.Progress = pct
		}
	}
	task.mu.Unlock()
	qm.save()
	qm.BroadcastUpdate()

	speedMode := qm.ResolveTaskSpeedMode(task)
	maxWorkers := turboWorkers
	if speedMode == SpeedModeRegular {
		maxWorkers = 1
	}

	httpClient := &http.Client{
		Transport: directHTTPTransport,
		Timeout:   0,
	}

	var completedStreamsBytes int64
	var anyTransferredThisRun atomic.Bool
	for i, s := range plan.Streams {
		var lastBroadcastMu sync.Mutex
		var lastBroadcast time.Time
		baseDoneForStream := streamDone[i]

		streamIdx := i
		origRole := s.Role
		origFormatID := s.FormatID
		origExpectedSize := s.ExpectedSize

		writtenBytes, fetchErr := fetchStreamWithGoEngine(ctx, streamFetchOptions{
			Client:       httpClient,
			URL:          s.URL,
			Headers:      s.Headers,
			DestPath:     s.DestPath,
			ExpectedSize: s.ExpectedSize,
			MaxWorkers:   maxWorkers,
			OnStatus: func(msg string) {
				task.mu.Lock()
				if task.Status == StatusDownloading {
					task.Speed = msg
				}
				task.mu.Unlock()
				qm.BroadcastUpdate()
			},
			OnProgress: func(downloaded, streamTotal int64, rateBytesPerSec float64, workers int) {
				if downloaded > baseDoneForStream {
					anyTransferredThisRun.Store(true)
				}
				if streamTotal > 0 {
					streamSizes[streamIdx] = streamTotal
				}

				overallDone := completedStreamsBytes + downloaded
				overallTotal := sumTotal()
				if overallTotal > 0 && overallDone > overallTotal {
					overallTotal = overallDone
				}

				var pct float64
				if overallTotal > 0 {
					pct = float64(overallDone) / float64(overallTotal) * 100.0
					if len(plan.Streams) > 1 && pct > 99.0 {
						pct = 99.0
					} else if pct > 100.0 {
						pct = 100.0
					}
				}

				var speedStr, etaStr string
				if rateBytesPerSec > 0 {
					if maxWorkers <= 1 {
						speedStr = formatBytes(int64(rateBytesPerSec)) + "/s (Live)"
					} else {
						speedStr = fmt.Sprintf("%s/s (%dx)", formatBytes(int64(rateBytesPerSec)), workers)
					}
					if overallTotal > overallDone {
						etaStr = formatDuration(int64(float64(overallTotal-overallDone) / rateBytesPerSec))
					}
				}

				task.mu.Lock()
				stillActive := task.Status == StatusDownloading
				if stillActive {
					task.DownloadedBytes = overallDone
					if overallTotal > 0 {
						task.TotalBytes = overallTotal
					}
					if pct > 0 || task.Progress == 0 {
						task.Progress = pct
					}
					if speedStr != "" {
						task.Speed = speedStr
						task.ETA = etaStr
					}
				}
				task.mu.Unlock()

				now := time.Now()
				lastBroadcastMu.Lock()
				shouldBroadcast := stillActive && now.Sub(lastBroadcast) >= 150*time.Millisecond
				if shouldBroadcast {
					lastBroadcast = now
				}
				lastBroadcastMu.Unlock()
				if shouldBroadcast {
					qm.BroadcastUpdate()
				}
			},
			RefreshURL: func(rCtx context.Context) (string, map[string]string, error) {
				freshInfo, rErr := resolveHybridMediaPlan(rCtx, binPath, cacheDir, downloadDir, task.URL, customFile, quality, videoCodec, *cookieFilePtr, hasFFmpeg)
				if rErr != nil {
					return "", nil, rErr
				}
				freshPlan, reason := evaluateHybridPlan(freshInfo, downloadDir, customFile, hasFFmpeg)
				if freshPlan == nil {
					return "", nil, fmt.Errorf("refreshed plan ineligible: %s", reason)
				}
				for _, fs := range freshPlan.Streams {
					if fs.FormatID == origFormatID {
						if origExpectedSize > 0 && fs.ExpectedSize > 0 && fs.ExpectedSize != origExpectedSize {
							return "", nil, fmt.Errorf("refreshed format %s changed size (%d vs %d)", origFormatID, fs.ExpectedSize, origExpectedSize)
						}
						return fs.URL, fs.Headers, nil
					}
				}
				for _, fs := range freshPlan.Streams {
					if fs.Role == origRole && origExpectedSize > 0 && fs.ExpectedSize == origExpectedSize {
						return fs.URL, fs.Headers, nil
					}
				}
				return "", nil, fmt.Errorf("matching format %s no longer available after URL refresh", origFormatID)
			},
		})

		if fetchErr != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			// If stream 0 fails with an HTTP error on a fresh download before transferring any bytes, let yt-dlp fallback try.
			if streamIdx == 0 && baseDoneForStream == 0 && !anyTransferredThisRun.Load() && isHTTPStatusError(fetchErr) && !isDiskSpaceError(fetchErr.Error()) {
				_ = removeFileWithRetry(s.DestPath+".part", 2, 20*time.Millisecond)
				_ = removeFileWithRetry(s.DestPath+".part.turbo", 2, 20*time.Millisecond)
				return false, nil
			}
			return true, fetchErr
		}

		completedStreamsBytes += writtenBytes
		streamSizes[streamIdx] = writtenBytes
	}

	if len(plan.Streams) == 1 {
		fi, statErr := os.Stat(plan.FinalPath)
		if statErr != nil || fi.IsDir() || fi.Size() == 0 {
			return true, fmt.Errorf("final downloaded video file missing on disk")
		}
		task.mu.Lock()
		task.SavePath = plan.FinalPath
		task.FileName = filepath.Base(plan.FinalPath)
		task.TotalBytes = fi.Size()
		task.DownloadedBytes = fi.Size()
		task.Speed = ""
		task.ETA = ""
		if task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://") {
			task.Title = strings.TrimSuffix(task.FileName, filepath.Ext(task.FileName))
		}
		task.mu.Unlock()
		qm.BroadcastUpdate()
		return true, nil
	}

	vPath := plan.Streams[0].DestPath
	aPath := plan.Streams[1].DestPath
	vFi, vErr := os.Stat(vPath)
	aFi, aErr := os.Stat(aPath)
	if vErr != nil || aErr != nil || vFi.Size() <= 0 || aFi.Size() <= 0 {
		return true, fmt.Errorf("downloaded stream file missing before merge")
	}

	mState := &hybridMergeState{
		FinalPath:  plan.FinalPath,
		VideoPath:  vPath,
		VideoSize:  vFi.Size(),
		AudioPath:  aPath,
		AudioSize:  aFi.Size(),
		Quality:    quality,
		VideoCodec: videoCodec,
		Title:      plan.Title,
	}
	statePath := mergeStatePathForFinal(plan.FinalPath)
	_ = saveMergeState(statePath, mState)
	qm.save()

	if ffmpegBin == "" {
		var fErr error
		ffmpegBin, fErr = ensureFFmpegBinary(ctx, qm.config, func(msg string) {
			task.mu.Lock()
			if task.Status == StatusDownloading {
				task.Speed = msg
			}
			task.mu.Unlock()
			qm.BroadcastUpdate()
		})
		if fErr != nil || ffmpegBin == "" {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, fmt.Errorf("FFmpeg required to merge video + audio streams (streams preserved on disk): %v", fErr)
		}
	}

	return true, mergeHybridStreamsWithProgress(ctx, qm, task, ffmpegBin, mState, statePath)
}
