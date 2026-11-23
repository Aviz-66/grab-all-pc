package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestYouTubeCookieClassification(t *testing.T) {
	signInErrors := []string{
		"ERROR: [youtube] abc: Private video. Sign in if you've been granted access",
		"ERROR: [youtube] abc: Sign in to confirm your age",
		"ERROR: [youtube] abc: This video is age-restricted",
		"ERROR: [youtube] abc: Join this channel to get access to members-only content",
		"ERROR: [youtube] abc: This video is available to this channel's members",
		"ERROR: [youtube] abc: This video requires payment to watch",
	}
	for _, msg := range signInErrors {
		if !isYouTubeSignInRequiredError(msg) {
			t.Errorf("expected isYouTubeSignInRequiredError=true for %q", msg)
		}
		if isYouTubeSignedInRefusalError(msg) {
			t.Errorf("expected isYouTubeSignedInRefusalError=false (must preserve cookies!) for %q", msg)
		}
	}

	refusalErrors := []string{
		"ERROR: [youtube] xyz: Sign in to confirm you're not a bot",
		"ERROR: unable to download video data: HTTP Error 403: Forbidden",
		"WARNING: PO Token missing for this client",
		"ERROR: Requested format is not available",
		"ERROR: Only images are available for download",
		"ERROR: SABR stream missing URL",
	}
	for _, msg := range refusalErrors {
		if isYouTubeSignInRequiredError(msg) {
			t.Errorf("expected isYouTubeSignInRequiredError=false for %q", msg)
		}
		if !isYouTubeSignedInRefusalError(msg) {
			t.Errorf("expected isYouTubeSignedInRefusalError=true for %q", msg)
		}
	}

	for _, av := range []string{"private", "subscriber_only", "premium_only", "needs_auth", " PRIVATE "} {
		if !isYouTubePrivateAvailability(av) {
			t.Errorf("expected isYouTubePrivateAvailability(%q)=true", av)
		}
	}
	for _, av := range []string{"public", "unlisted", ""} {
		if isYouTubePrivateAvailability(av) {
			t.Errorf("expected isYouTubePrivateAvailability(%q)=false", av)
		}
	}

	if !isFatalYtDlpProbeError("ERROR: Video unavailable") || !isFatalYtDlpProbeError("ERROR: Unsupported URL") {
		t.Errorf("expected definitive probe errors to return isFatalYtDlpProbeError=true")
	}
	if isFatalYtDlpProbeError("WARNING: Read timed out") {
		t.Errorf("expected transient timeout NOT to be fatal probe error")
	}
}

func TestEvaluateHybridPlanEligibilityAndFallback(t *testing.T) {
	tmpDir := t.TempDir()
	boolFalse := false
	boolTrue := true

	// 1. Completed livestream VOD with audio listed before video & float filesize JSON
	vodJSON := []byte(`{
		"id": "dQw4w9WgXcQ",
		"title": "7-Hour Stream VOD",
		"was_live": true,
		"is_live": false,
		"live_status": "was_live",
		"availability": "public",
		"http_headers": {
			"User-Agent": "CustomUA/1.0",
			"Accept-Encoding": "gzip, deflate",
			"Range": "bytes=0-100"
		},
		"requested_formats": [
			{
				"format_id": "140",
				"url": "https://rr1.googlevideo.com/audio",
				"ext": "m4a",
				"protocol": "https",
				"vcodec": "none",
				"acodec": "mp4a.40.2",
				"filesize": 300000000.0
			},
			{
				"format_id": "137",
				"url": "https://rr1.googlevideo.com/video",
				"ext": "mp4",
				"protocol": "HTTPS",
				"vcodec": "avc1.640028",
				"acodec": "none",
				"filesize": 10800000000.0,
				"cookies": "VISITOR=1"
			}
		]
	}`)

	info, err := parseYtDlpInfoJSON(vodJSON)
	if err != nil {
		t.Fatalf("parseYtDlpInfoJSON failed: %v", err)
	}
	plan, reason := evaluateHybridPlan(info, tmpDir, "", true)
	if plan == nil {
		t.Fatalf("expected VOD plan to be eligible, rejected because: %s", reason)
	}
	if len(plan.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(plan.Streams))
	}
	if plan.Streams[0].Role != "video" || plan.Streams[0].FormatID != "137" {
		t.Errorf("expected stream 0 to be video format 137, got %+v", plan.Streams[0])
	}
	if plan.Streams[1].Role != "audio" || plan.Streams[1].FormatID != "140" {
		t.Errorf("expected stream 1 to be audio format 140, got %+v", plan.Streams[1])
	}
	if !strings.HasSuffix(plan.Streams[0].DestPath, ".f137.mp4") || !strings.HasSuffix(plan.Streams[1].DestPath, ".f140.m4a") {
		t.Errorf("unexpected stream dest paths: %q, %q", plan.Streams[0].DestPath, plan.Streams[1].DestPath)
	}
	if plan.CombinedExact != 11100000000 {
		t.Errorf("expected CombinedExact=11100000000, got %d", plan.CombinedExact)
	}
	if _, hasAE := plan.Streams[0].Headers["Accept-Encoding"]; hasAE {
		t.Errorf("Accept-Encoding must not be copied into stream headers")
	}
	if _, hasRange := plan.Streams[0].Headers["Range"]; hasRange {
		t.Errorf("Range must not be copied into stream headers")
	}
	if plan.Streams[0].Headers["Cookie"] != "VISITOR=1" {
		t.Errorf("expected Cookie header copied from format, got %q", plan.Streams[0].Headers["Cookie"])
	}

	// Same 2-stream info without FFmpeg must reject
	if pNoFF, _ := evaluateHybridPlan(info, tmpDir, "", false); pNoFF != nil {
		t.Errorf("expected 2-stream plan without FFmpeg to fall back")
	}

	// 2. Single progressive stream without FFmpeg must be eligible
	singleInfo := &ytDlpInfoJSON{
		ID:       "prog123",
		Title:    "Progressive Clip",
		IsLive:   &boolFalse,
		URL:      "https://rr2.googlevideo.com/prog.mp4",
		FormatID: "18",
		Ext:      "mp4",
		Protocol: "https",
		Filesize: 15000000,
	}
	singlePlan, sReason := evaluateHybridPlan(singleInfo, tmpDir, "", false)
	if singlePlan == nil {
		t.Fatalf("expected single stream without FFmpeg to be eligible, rejected: %s", sReason)
	}
	if len(singlePlan.Streams) != 1 || singlePlan.Streams[0].Role != "single" || singlePlan.Streams[0].DestPath != singlePlan.FinalPath {
		t.Errorf("unexpected single stream spec: %+v", singlePlan)
	}

	// 3. Disallowed cases: live, upcoming, DRM, m3u8, http_dash_segments, rtmp
	rejectCases := []struct {
		name string
		mod  func() *ytDlpInfoJSON
	}{
		{
			name: "is_live_bool",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{IsLive: &boolTrue, URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}
			},
		},
		{
			name: "live_status_is_live",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{LiveStatus: "is_live", URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}
			},
		},
		{
			name: "live_status_upcoming",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{LiveStatus: "is_upcoming", URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}
			},
		},
		{
			name: "top_drm_true",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{HasDRM: true, URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}
			},
		},
		{
			name: "top_drm_maybe",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{HasDRM: "maybe", URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}
			},
		},
		{
			name: "format_drm",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{RequestedFormats: []ytDlpFormatJSON{{HasDRM: true, URL: "https://example.com/a.mp4", Ext: "mp4", Protocol: "https"}}}
			},
		},
		{
			name: "protocol_m3u8",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{URL: "https://example.com/p.m3u8", Ext: "mp4", Protocol: "m3u8_native"}
			},
		},
		{
			name: "protocol_dash_segments",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{URL: "https://example.com/manifest.mpd", Ext: "mp4", Protocol: "http_dash_segments"}
			},
		},
		{
			name: "protocol_rtmp",
			mod: func() *ytDlpInfoJSON {
				return &ytDlpInfoJSON{URL: "rtmp://example.com/live/s", Ext: "flv", Protocol: "rtmp"}
			},
		},
	}

	for _, tc := range rejectCases {
		if p, _ := evaluateHybridPlan(tc.mod(), tmpDir, "", true); p != nil {
			t.Errorf("case %s should have fallen back to yt-dlp, but returned plan %+v", tc.name, p)
		}
	}
}

func serveRangePayload(w http.ResponseWriter, r *http.Request, payload []byte) {
	total := int64(len(payload))
	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
		return
	}
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	end := total - 1
	if len(parts) == 2 && parts[1] != "" {
		if parsedEnd, err2 := strconv.ParseInt(parts[1], 10, 64); err2 == nil {
			end = parsedEnd
		}
	}
	if err1 != nil || start < 0 || start >= total || end < start {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= total {
		end = total - 1
	}
	slice := payload[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(slice)
}

func TestFetchStreamWithGoEngineResumeAndRefreshOn403(t *testing.T) {
	const payloadSize = 5 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte((i*31 + i/257) & 0xFF)
	}
	wantSum := sha256.Sum256(payload)

	var v1Reqs atomic.Int64
	var v2Reqs atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1":
			n := v1Reqs.Add(1)
			if n > 2 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// Tiny delay so remaining workers hit /v1 concurrently
			time.Sleep(15 * time.Millisecond)
			serveRangePayload(w, r, payload)
		case "/v2":
			v2Reqs.Add(1)
			serveRangePayload(w, r, payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// 1. Parallel Turbo mode with 403 mid-download & single-flight RefreshURL
	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "video.f137.mp4")

	var refreshCalls atomic.Int32
	written, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:          srv.URL + "/v1",
		DestPath:     destPath,
		ExpectedSize: payloadSize,
		MaxWorkers:   4,
		BlockSize:    512 * 1024, // 10 blocks so 4 parallel workers experience 403 together
		RefreshURL: func(ctx context.Context) (string, map[string]string, error) {
			refreshCalls.Add(1)
			time.Sleep(30 * time.Millisecond)
			return srv.URL + "/v2", nil, nil
		},
	})
	if err != nil {
		t.Fatalf("Turbo fetch with 403 refresh failed: %v", err)
	}
	if written != payloadSize {
		t.Fatalf("expected written=%d, got %d", payloadSize, written)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("expected single-flight RefreshURL to run exactly once, got %d", refreshCalls.Load())
	}

	gotBytes, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed reading downloaded file: %v", err)
	}
	if sha256.Sum256(gotBytes) != wantSum {
		t.Fatalf("SHA-256 mismatch after 403 URL refresh")
	}
	if _, err := os.Stat(destPath + ".part"); !os.IsNotExist(err) {
		t.Errorf("expected .part to be removed on completion")
	}
	if _, err := os.Stat(destPath + ".part.turbo"); !os.IsNotExist(err) {
		t.Errorf("expected .part.turbo to be removed on completion")
	}

	// 2. Calling fetchStreamWithGoEngine again on completed file must make 0 HTTP requests
	v2Before := v2Reqs.Load()
	skippedLen, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:          srv.URL + "/v2",
		DestPath:     destPath,
		ExpectedSize: payloadSize,
		MaxWorkers:   4,
	})
	if err != nil || skippedLen != payloadSize {
		t.Fatalf("expected instant skip to return (%d, nil), got (%d, %v)", payloadSize, skippedLen, err)
	}
	if v2Reqs.Load() != v2Before {
		t.Errorf("expected 0 HTTP requests when file already exists cleanly, got %d extra", v2Reqs.Load()-v2Before)
	}

	// 3. Regular (sequential) mode resuming from an interrupted Turbo .part + .part.turbo state
	// (Block 0 + Block 2 done, Block 1 missing -> must trim to 512 KiB contiguous prefix)
	regDir := t.TempDir()
	regDest := filepath.Join(regDir, "video.f137.mp4")
	regPart := regDest + ".part"
	regTurbo := regPart + ".turbo"
	const blk = 512 * 1024
	partialBuf := make([]byte, 3*blk)
	copy(partialBuf[:blk], payload[:blk])
	copy(partialBuf[2*blk:3*blk], payload[2*blk:3*blk])
	if err := os.WriteFile(regPart, partialBuf, 0644); err != nil {
		t.Fatal(err)
	}
	saveTurboMeta(regTurbo, &turboStateMeta{
		TotalBytes: payloadSize,
		BlockSize:  blk,
		DoneBlocks: []bool{true, false, true, false, false, false, false, false, false, false},
	})

	regLen, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:          srv.URL + "/v2",
		DestPath:     regDest,
		ExpectedSize: payloadSize,
		MaxWorkers:   1,
		BlockSize:    blk,
	})
	if err != nil || regLen != payloadSize {
		t.Fatalf("Turbo->Regular sequential resume failed: (%d, %v)", regLen, err)
	}
	regGot, err := os.ReadFile(regDest)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(regGot) != wantSum {
		t.Fatalf("SHA-256 mismatch after Turbo->Regular sequential resume")
	}
}

func TestMergeProgressAndFailurePreservesStreams(t *testing.T) {
	// Cross-platform temp artifact recognition assertions
	tempCases := []struct {
		name string
		want bool
	}{
		{"My Video [abc123def45].temp.mp4", true},
		{"My Video [abc123def45].merge.json", true},
		{"My Video [abc123def45].f251-drc-en-US.webm", true},
		{"My Video [abc123def45].f137.mp4", true},
		{"My Video [abc123def45].mp4", false},
	}
	for _, tc := range tempCases {
		if got := isPartialOrTempPath(tc.name); got != tc.want {
			t.Errorf("isPartialOrTempPath(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}

	if runtime.GOOS == "windows" {
		return
	}

	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "cfg.json"))
	cfg.DownloadDir = tmpDir
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "q.json"), nil)
	defer qm.Stop()

	dlDir := filepath.Join(tmpDir, "dl")
	if err := os.MkdirAll(dlDir, 0755); err != nil {
		t.Fatal(err)
	}

	videoPath := filepath.Join(dlDir, "Big Video [dQw4w9WgXcQ].f137.mp4")
	audioPath := filepath.Join(dlDir, "Big Video [dQw4w9WgXcQ].f140.m4a")
	finalPath := filepath.Join(dlDir, "Big Video [dQw4w9WgXcQ].mp4")

	videoData := bytes.Repeat([]byte{'V'}, 2*1024*1024)
	audioData := bytes.Repeat([]byte{'A'}, 512*1024)
	if err := os.WriteFile(videoPath, videoData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, audioData, 0644); err != nil {
		t.Fatal(err)
	}

	mState := &hybridMergeState{
		FinalPath:  finalPath,
		VideoPath:  videoPath,
		VideoSize:  int64(len(videoData)),
		AudioPath:  audioPath,
		AudioSize:  int64(len(audioData)),
		Quality:    "1080",
		VideoCodec: "h264",
		Title:      "Big Video",
	}
	statePath := mergeStatePathForFinal(finalPath)
	if err := saveMergeState(statePath, mState); err != nil {
		t.Fatalf("saveMergeState failed: %v", err)
	}

	task := &DownloadTask{
		ID:       "dl_merge_test",
		URL:      "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Title:    "Big Video",
		SavePath: finalPath,
		FileName: filepath.Base(finalPath),
		Status:   StatusDownloading,
		Progress: 99.0,
	}

	// 1. Failing FFmpeg (simulating disk full): must preserve .f137.mp4, .f140.m4a, and .merge.json
	failFFmpeg := filepath.Join(tmpDir, "fail_ffmpeg.sh")
	failScript := fmt.Sprintf("#!/bin/sh\nout=\"\"\nfor arg in \"$@\"; do out=\"$arg\"; done\ncat %q > \"$out\"\necho \"av_interleaved_write_frame(): No space left on device\" >&2\nexit 1\n", audioPath)
	if err := os.WriteFile(failFFmpeg, []byte(failScript), 0755); err != nil {
		t.Fatal(err)
	}

	mergeErr := mergeHybridStreamsWithProgress(context.Background(), qm, task, failFFmpeg, mState, statePath)
	if mergeErr == nil {
		t.Fatalf("expected merge error from failing mock ffmpeg")
	}
	if !strings.Contains(strings.ToLower(mergeErr.Error()), "free disk space") || !strings.Contains(strings.ToLower(mergeErr.Error()), "preserved") {
		t.Errorf("expected clear disk-space + preserved message, got: %v", mergeErr)
	}

	tempMergedPath := filepath.Join(dlDir, "Big Video [dQw4w9WgXcQ].temp.mp4")
	if _, err := os.Stat(tempMergedPath); !os.IsNotExist(err) {
		t.Errorf("expected failed .temp.mp4 to be cleaned up")
	}
	for _, preserved := range []string{videoPath, audioPath, statePath} {
		if _, err := os.Stat(preserved); err != nil {
			t.Errorf("expected source stream/checkpoint %s to remain preserved on disk: %v", preserved, err)
		}
	}

	reusable, foundStatePath := findReusableMergeState(dlDir, task, "1080", "h264")
	if reusable == nil || foundStatePath != statePath {
		t.Fatalf("expected findReusableMergeState to locate preserved checkpoint, got (%+v, %q)", reusable, foundStatePath)
	}

	// 2. Succeeding FFmpeg: must report live >0% merge progress and clean up source streams afterwards
	var (
		speedsMu       sync.Mutex
		observedSpeeds []string
	)
	qm.SetUIHooks(func() {
		task.mu.Lock()
		sp := task.Speed
		task.mu.Unlock()
		if sp != "" {
			speedsMu.Lock()
			observedSpeeds = append(observedSpeeds, sp)
			speedsMu.Unlock()
		}
	}, nil, nil)

	okFFmpeg := filepath.Join(tmpDir, "ok_ffmpeg.sh")
	okScript := fmt.Sprintf("#!/bin/sh\nout=\"\"\nfor arg in \"$@\"; do out=\"$arg\"; done\ncat %q > \"$out\"\nsleep 0.45\ncat %q >> \"$out\"\nexit 0\n", videoPath, audioPath)
	if err := os.WriteFile(okFFmpeg, []byte(okScript), 0755); err != nil {
		t.Fatal(err)
	}

	if err := mergeHybridStreamsWithProgress(context.Background(), qm, task, okFFmpeg, reusable, foundStatePath); err != nil {
		t.Fatalf("succeeding mergeHybridStreamsWithProgress failed: %v", err)
	}

	fi, err := os.Stat(finalPath)
	if err != nil || fi.Size() != int64(len(videoData)+len(audioData)) {
		t.Fatalf("expected final merged file size %d, got err=%v size=%v", len(videoData)+len(audioData), err, fi)
	}
	for _, removed := range []string{videoPath, audioPath, statePath, tempMergedPath} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Errorf("expected temporary/source file %s to be cleaned up after successful merge", removed)
		}
	}

	speedsMu.Lock()
	defer speedsMu.Unlock()
	sawLivePct := false
	for _, sp := range observedSpeeds {
		if strings.HasPrefix(sp, "Merging Video + Audio...") && !strings.Contains(sp, " 0%") && strings.Contains(sp, "%") {
			sawLivePct = true
			break
		}
	}
	if !sawLivePct {
		t.Errorf("expected live non-zero merge %% status in task.Speed, observed: %v", observedSpeeds)
	}
}
