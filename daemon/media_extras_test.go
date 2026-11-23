package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSubtitleLangNormalizationAndYtDlpEmbedArgs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "en"},
		{"  EN , es ; fr  ", "en,es,fr"},
		{"en.*,si,en.*", "en.*,si"},
		{"all", "all"},
		{"$$$invalid$$$", "invalid"},
		{"...", "en"},
	}
	for _, tc := range cases {
		if got := normalizeSubLangs(tc.in); got != tc.want {
			t.Errorf("normalizeSubLangs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if args := buildYtDlpEmbedArgs(true, "en,es", true, false, false); len(args) != 0 {
		t.Errorf("expected no embed args when hasFFmpeg=false, got %v", args)
	}

	videoArgs := buildYtDlpEmbedArgs(true, "en, es", true, false, true)
	joinedVideo := strings.Join(videoArgs, " ")
	for _, flag := range []string{"--write-subs", "--write-auto-subs", "--sub-langs en,es", "--embed-subs", "--embed-chapters"} {
		if !strings.Contains(joinedVideo, flag) {
			t.Errorf("expected video embed args %q to contain %q", joinedVideo, flag)
		}
	}

	// Audio-only downloads should still allow chapters while omitting video subtitle flags
	audioArgs := buildYtDlpEmbedArgs(true, "en", true, true, true)
	if len(audioArgs) != 1 || audioArgs[0] != "--embed-chapters" {
		t.Errorf("expected audio embed args [--embed-chapters], got %v", audioArgs)
	}

	if !isYtDlpSubtitleDownloadError("ERROR: Unable to download video subtitles for 'en': HTTP Error 429: Too Many Requests") {
		t.Error("expected HTTP 429 subtitle error to be detected")
	}
	if isYtDlpSubtitleDownloadError("ERROR: Video unavailable") {
		t.Error("did not expect non-subtitle error to match isYtDlpSubtitleDownloadError")
	}
}

func TestSubtitleTrackSelectionAndFFMetadataChapters(t *testing.T) {
	manual := map[string][]ytSubtitleFormat{
		"es":        {{Ext: "json3", URL: "https://sub.example/es.json3"}, {Ext: "vtt", URL: "https://sub.example/es.vtt"}},
		"live_chat": {{Ext: "json", URL: "https://sub.example/chat.json"}},
	}
	auto := map[string][]ytSubtitleFormat{
		"en":      {{Ext: "vtt", URL: "https://sub.example/en-translated.vtt"}},
		"en-orig": {{Ext: "srt", URL: "https://sub.example/en-orig.srt"}, {Ext: "vtt", URL: "https://sub.example/en-orig.vtt"}},
		"es":      {{Ext: "vtt", URL: "https://sub.example/es-auto.vtt"}},
	}

	tracks := selectSubtitleTracks(manual, auto, "en,es", 3)
	if len(tracks) != 2 {
		t.Fatalf("expected 2 selected subtitle tracks, got %d (%+v)", len(tracks), tracks)
	}
	// First pattern "en" has no manual track, so it should pick auto "en-orig" in vtt format
	if tracks[0].Lang != "en-orig" || !tracks[0].Auto || tracks[0].Ext != "vtt" {
		t.Errorf("unexpected first track: %+v", tracks[0])
	}
	// Second pattern "es" has a manual track, which must win over auto "es"
	if tracks[1].Lang != "es" || tracks[1].Auto || tracks[1].Ext != "vtt" {
		t.Errorf("unexpected second track: %+v", tracks[1])
	}

	chapters := []ytChapter{
		{StartTime: 0, EndTime: 65.5, Title: "Intro = Welcome; #1"},
		{StartTime: 65.5, EndTime: 0, Title: "Part 2"},
	}
	ffmeta := buildFFMetadataChapters(chapters, 180.0)
	for _, want := range []string{
		";FFMETADATA1",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=0",
		"END=65500",
		`title=Intro \= Welcome\; \#1`,
		"START=65500",
		"END=180000",
		"title=Part 2",
	} {
		if !strings.Contains(ffmeta, want) {
			t.Errorf("FFMETADATA missing %q in:\n%s", want, ffmeta)
		}
	}

	// Verify prepareHybridSidecars + buildHybridMergeArgs with a local subtitle HTTP server
	subSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nHello world\n"))
	}))
	defer subSrv.Close()

	tmpDir := t.TempDir()
	finalPath := filepath.Join(tmpDir, "video.mp4")
	info := &ytDlpInfoJSON{
		ID:       "vid123",
		Title:    "Demo",
		Duration: 120,
		Subtitles: map[string][]ytSubtitleFormat{
			"en": {{Ext: "vtt", URL: subSrv.URL + "/en.vtt"}},
		},
		Chapters: []ytChapter{
			{StartTime: 0, EndTime: 60, Title: "First Half"},
			{StartTime: 60, EndTime: 120, Title: "Second Half"},
		},
	}

	subPaths, subLangs, chapPath := prepareHybridSidecars(context.Background(), info, finalPath, true, "en", true)
	if len(subPaths) != 1 || len(subLangs) != 1 || subLangs[0] != "en" {
		t.Fatalf("expected 1 prepared subtitle sidecar, got paths=%v langs=%v", subPaths, subLangs)
	}
	if chapPath == "" {
		t.Fatal("expected prepared chapters sidecar file")
	}
	if !isPartialOrTempPath(subPaths[0]) || !isPartialOrTempPath(chapPath) {
		t.Errorf("expected sidecar paths to be recognized by isPartialOrTempPath: %q, %q", subPaths[0], chapPath)
	}

	st := &hybridMergeState{
		FinalPath:     finalPath,
		VideoPath:     filepath.Join(tmpDir, "v.mp4"),
		AudioPath:     filepath.Join(tmpDir, "a.m4a"),
		SubtitlePaths: subPaths,
		SubtitleLangs: subLangs,
		ChaptersPath:  chapPath,
	}
	ffArgs := strings.Join(buildHybridMergeArgs(st, filepath.Join(tmpDir, "out.mp4"), true), " ")
	for _, want := range []string{"-c:s mov_text", "language=eng", "-f ffmetadata", "-map_chapters 3"} {
		if !strings.Contains(ffArgs, want) {
			t.Errorf("expected buildHybridMergeArgs output %q to contain %q", ffArgs, want)
		}
	}
	fallbackArgs := strings.Join(buildHybridMergeArgs(st, filepath.Join(tmpDir, "out.mp4"), false), " ")
	if strings.Contains(fallbackArgs, "mov_text") || strings.Contains(fallbackArgs, "ffmetadata") {
		t.Errorf("expected fallback buildHybridMergeArgs without sidecars, got %q", fallbackArgs)
	}
}

func TestMediaPreviewParsingCacheReuseAndAPI(t *testing.T) {
	sampleJSON := `{
		"id": "abc987",
		"title": "Sample 4K Demo Video",
		"uploader": "GrabAll Channel",
		"duration": 200,
		"formats": [
			{"format_id": "140", "ext": "m4a", "vcodec": "none", "acodec": "mp4a.40.2", "tbr": 128, "filesize": 3200000, "protocol": "https"},
			{"format_id": "136", "ext": "mp4", "vcodec": "avc1.64001f", "acodec": "none", "width": 1280, "height": 720, "tbr": 1500, "filesize": 37500000, "protocol": "https"},
			{"format_id": "137", "ext": "mp4", "vcodec": "avc1.640028", "acodec": "none", "width": 1920, "height": 1080, "tbr": 3200, "filesize": 80000000, "protocol": "https"},
			{"format_id": "313", "ext": "webm", "vcodec": "vp9", "acodec": "none", "width": 3840, "height": 2160, "tbr": 12000, "filesize_approx": 300000000, "protocol": "https"}
		],
		"requested_formats": [
			{"format_id": "137", "url": "https://cdn.example/v1080.mp4", "ext": "mp4", "protocol": "https", "vcodec": "avc1.640028", "acodec": "none", "filesize": 80000000},
			{"format_id": "140", "url": "https://cdn.example/a140.m4a", "ext": "m4a", "protocol": "https", "vcodec": "none", "acodec": "mp4a.40.2", "filesize": 3200000}
		],
		"subtitles": {
			"en": [{"ext": "vtt", "url": "https://cdn.example/en.vtt"}]
		},
		"automatic_captions": {
			"es-orig": [{"ext": "vtt", "url": "https://cdn.example/es.vtt"}]
		},
		"chapters": [
			{"start_time": 0, "end_time": 100, "title": "Part 1"},
			{"start_time": 100, "end_time": 200, "title": "Part 2"}
		]
	}`

	origProbe := previewProbeFunc
	var probeCalls atomic.Int32
	previewProbeFunc = func(ctx context.Context, cfg *Config, req previewProbeRequest) ([]byte, error) {
		probeCalls.Add(1)
		return []byte(sampleJSON), nil
	}
	defer func() { previewProbeFunc = origProbe }()

	tmpDir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), nil)
	defer qm.Stop()
	srv := NewServer(cfg, qm, NewSSEBroker())
	handler := srv.setupRoutes()

	targetURL := "https://www.youtube.com/watch?v=abc987"
	dropPreviewCache(targetURL)
	defer dropPreviewCache(targetURL)

	// 1. Call POST /api/media/preview
	reqBody := `{"url":"` + targetURL + `","videoCodec":"h264"}`
	req := httptest.NewRequest("POST", "/api/media/preview", strings.NewReader(reqBody))
	req.Header.Set("Origin", "chrome-extension://testext123")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/media/preview status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success bool         `json:"success"`
		Preview MediaPreview `json:"preview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if !resp.Success || resp.Preview.Title != "Sample 4K Demo Video" || resp.Preview.DurationText != "3:20" {
		t.Fatalf("unexpected preview metadata: %+v", resp.Preview)
	}
	if len(resp.Preview.Chapters) != 2 || len(resp.Preview.SubtitleLangs) != 1 || resp.Preview.SubtitleLangs[0] != "en" {
		t.Errorf("unexpected chapters/subtitles in preview: %+v", resp.Preview)
	}

	// Verify 1080p option includes combined video (80 MB) + audio (3.2 MB) exact size
	var found1080, foundAudio bool
	for _, opt := range resp.Preview.Options {
		if opt.Quality == "1080" {
			found1080 = true
			if opt.SizeBytes != 83200000 || opt.Approx {
				t.Errorf("expected exact 1080p size 83200000, got %+v", opt)
			}
		}
		if opt.Quality == "audio-best" {
			foundAudio = true
			if opt.SizeBytes != 3200000 {
				t.Errorf("expected audio size 3200000, got %+v", opt)
			}
		}
	}
	if !found1080 || !foundAudio {
		t.Errorf("missing expected quality options in %+v", resp.Preview.Options)
	}

	// 2. Second preview request for the same URL must hit cache without calling previewProbeFunc again.
	req2 := httptest.NewRequest("POST", "/api/preview", strings.NewReader(reqBody))
	req2.Header.Set("Origin", "chrome-extension://testext123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || probeCalls.Load() != 1 {
		t.Errorf("expected cached preview to avoid second probe call (calls=%d, code=%d)", probeCalls.Load(), rec2.Code)
	}

	// 3. writeCachedInfoJSON + resolveHybridMediaPlan ("best") reuses cached JSON in 0ms without spawning yt-dlp
	infoPath := writeCachedInfoJSON(targetURL, "unit")
	if infoPath == "" {
		t.Fatal("expected writeCachedInfoJSON to write temp info file")
	}
	_ = os.Remove(infoPath)

	info, err := resolveHybridMediaPlan(context.Background(), "/nonexistent/yt-dlp", tmpDir, tmpDir, targetURL, "", "best", "h264", "", true)
	if err != nil || info == nil || info.ID != "abc987" || len(info.RequestedFormats) != 2 {
		t.Fatalf("expected resolveHybridMediaPlan to reuse cached preview JSON without invoking binary, got info=%+v err=%v", info, err)
	}

	// 4. AddTask preserves EmbedSubs, normalized SubLangs, and EmbedChapters
	added, err := qm.AddTask(AddRequest{
		URL:           "https://example.com/archive.mp4",
		Engine:        EngineDirect,
		EmbedSubs:     true,
		SubLangs:      " EN , es ",
		EmbedChapters: true,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	qm.PauseTask(added.ID)
	if !added.EmbedSubs || added.SubLangs != "en,es" || !added.EmbedChapters {
		t.Errorf("expected EmbedSubs=true, SubLangs=en,es, EmbedChapters=true on task snapshot, got %+v", added)
	}
}
