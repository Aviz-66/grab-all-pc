package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEngineDetection(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"https://example.com/archive.zip", EngineDirect},
		{"https://example.com/installer.exe", EngineDirect},
		{"https://example.com/manual.pdf?download=true", EngineDirect},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", EngineYtDlp},
		{"https://vimeo.com/12345678", EngineYtDlp},
		{"https://stream.provider.com/live/playlist.m3u8", EngineYtDlp},
		{"https://cdn.example.com/dash/manifest.mpd", EngineYtDlp},
		{"https://tiktok.com/@user/video/123", EngineYtDlp},
	}

	for _, tt := range tests {
		result := detectEngine(tt.url)
		if result != tt.expected {
			t.Errorf("detectEngine(%q) = %s, expected %s", tt.url, result, tt.expected)
		}
	}
}

func TestQueueManagerBasicOps(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "testtoken",
		DownloadDir:   tmpDir,
		MaxConcurrent: 2,
		DefaultEngine: "auto",
	}

	// Local blocking test server so direct download stays in-flight deterministically without external DNS/HTTP
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	dataFile := filepath.Join(tmpDir, "queue.json")
	broker := NewSSEBroker()
	qm := NewQueueManager(cfg, dataFile, broker)
	defer qm.Stop()

	// Add direct task
	task, err := qm.AddTask(AddRequest{
		URL:      ts.URL + "/test.zip",
		FileName: "test.zip",
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	if task.Status != StatusQueued && task.Status != StatusDownloading {
		t.Errorf("Unexpected status: %s", task.Status)
	}

	// Verify get
	got := qm.GetByID(task.ID)
	if got == nil || snapshotTask(got, false).URL != task.URL {
		t.Fatalf("GetByID failed: got %v", got)
	}

	// Pause
	qm.PauseTask(task.ID)
	gotSnap := snapshotTask(qm.GetByID(task.ID), false)
	if gotSnap.Status != StatusPaused {
		t.Errorf("Expected status paused, got %s", gotSnap.Status)
	}

	// Resume
	qm.ResumeTask(task.ID)
	gotSnap = snapshotTask(qm.GetByID(task.ID), false)
	if gotSnap.Status != StatusQueued && gotSnap.Status != StatusDownloading {
		t.Errorf("Expected status queued/downloading after resume, got %s", gotSnap.Status)
	}

	// Cancel
	qm.CancelTask(task.ID)
	gotSnap = snapshotTask(qm.GetByID(task.ID), false)
	if gotSnap.Status != StatusCanceled {
		t.Errorf("Expected status canceled, got %s", gotSnap.Status)
	}

	// Clear completed
	cleared := qm.ClearCompleted()
	if cleared != 1 {
		t.Errorf("Expected 1 task cleared, got %d", cleared)
	}

	all := qm.GetAll()
	if len(all) != 0 {
		t.Errorf("Expected 0 tasks remaining, got %d", len(all))
	}
}

func TestYouTubeAndQuality(t *testing.T) {
	if !isYouTubeURL("https://www.youtube.com/watch?v=jTJxwQlb_jY") {
		t.Errorf("Expected youtube watch URL to be detected")
	}
	if !isYouTubeURL("https://youtu.be/jTJxwQlb_jY") {
		t.Errorf("Expected youtu.be URL to be detected")
	}
	if isYouTubeURL("https://vimeo.com/12345") {
		t.Errorf("Expected vimeo URL not to be detected as YouTube")
	}

	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "testtoken",
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
		DefaultEngine: "auto",
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	task, err := qm.AddTask(AddRequest{
		URL:     "https://www.youtube.com/watch?v=jTJxwQlb_jY",
		Engine:  EngineYtDlp,
		Quality: "audio-mp3",
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	if task.Quality != "audio-mp3" {
		t.Errorf("Expected quality audio-mp3, got %q", task.Quality)
	}
	qm.CancelTask(task.ID)
}

func TestPlaylistHelpersAndProgressParsing(t *testing.T) {
	if !isPurePlaylistURL("https://www.youtube.com/playlist?list=PL12345") {
		t.Errorf("Expected pure playlist URL to return true")
	}
	if isPurePlaylistURL("https://www.youtube.com/watch?v=abc&list=PL12345") {
		t.Errorf("Expected watch URL with list= not to be treated as pure playlist automatically")
	}

	gotURL := normalizePlaylistURL("https://www.youtube.com/watch?v=abc123&list=PL_XYZ")
	if gotURL != "https://www.youtube.com/playlist?list=PL_XYZ" {
		t.Errorf("normalizePlaylistURL = %q, expected playlist URL", gotURL)
	}

	// YouTube Mix / Radio (RD...) must keep watch?v=...&list=RD...
	mixURL := normalizePlaylistURL("https://www.youtube.com/watch?v=abc123&list=RDabc123&index=3")
	if mixURL != "https://www.youtube.com/watch?v=abc123&list=RDabc123" {
		t.Errorf("normalizePlaylistURL for Mix = %q, expected watch URL with RD list", mixURL)
	}

	// Verify computePlaylistBounds for Start # + Count and RD Mix cap
	if st, end := computePlaylistBounds("https://www.youtube.com/playlist?list=PL1", 4, 0, 10); st != 4 || end != 13 {
		t.Errorf("computePlaylistBounds(start=4,count=10) = %d..%d, expected 4..13", st, end)
	}
	if st, end := computePlaylistBounds("https://www.youtube.com/watch?v=x&list=RDx", 1, 0, 0); st != 1 || end != 25 {
		t.Errorf("computePlaylistBounds(RD default) = %d..%d, expected 1..25", st, end)
	}

	// Verify 0ms checklist PlaylistItems queuing preserves custom indices and quality
	tmpDir := t.TempDir()
	cfg := &Config{Port: 17890, DownloadDir: tmpDir, MaxConcurrent: 0}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	tasks, plTitle, err := qm.AddPlaylistTasks(context.Background(), AddRequest{
		URL:      "https://www.youtube.com/playlist?list=PL_COURSE",
		Title:    "Go Course",
		Quality:  "audio-mp3",
		Playlist: true,
		PlaylistItems: []PlaylistEntry{
			{Index: 3, URL: "https://www.youtube.com/watch?v=vid3", Title: "Lesson 3"},
			{Index: 5, URL: "https://www.youtube.com/watch?v=vid5", Title: "Lesson 5"},
		},
	})
	if err != nil || len(tasks) != 2 || plTitle != "Go Course" {
		t.Fatalf("AddPlaylistTasks with PlaylistItems failed: err=%v len=%d title=%q", err, len(tasks), plTitle)
	}
	if !strings.Contains(tasks[0].Title, "03 - Lesson 3") || tasks[0].Quality != "audio-mp3" {
		t.Errorf("Unexpected task[0]: title=%q quality=%q", tasks[0].Title, tasks[0].Quality)
	}

	if v := parseOptionalInt64("123456.8"); v != 123456 {
		t.Errorf("parseOptionalInt64 float = %d, expected 123456", v)
	}
	if v := parseOptionalInt64("NA"); v != 0 {
		t.Errorf("parseOptionalInt64 NA = %d, expected 0", v)
	}
}

func TestSameOriginAuthAndCookieSanitization(t *testing.T) {
	tmpDir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer ts.Close()

	cfg := &Config{
		Port:          17890,
		AuthToken:     "supersecret",
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	srv := NewServer(cfg, qm, NewSSEBroker())

	req := httptest.NewRequest("GET", "/api/events", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if !srv.isAuthorized(req) {
		t.Errorf("Expected Sec-Fetch-Site: same-origin to be authorized for SSE")
	}

	crossReq := httptest.NewRequest("POST", "/api/add", nil)
	crossReq.Header.Set("Sec-Fetch-Site", "cross-site")
	crossReq.Header.Set("Origin", "https://evil.example.com")
	if srv.isAuthorized(crossReq) {
		t.Errorf("Expected cross-site request without token to be rejected")
	}

	task, err := qm.AddTask(AddRequest{
		URL:     ts.URL + "/file.zip",
		Cookies: []Cookie{{Name: "SID", Value: "secretval", Domain: ".example.com"}},
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	if len(task.Cookies) != 0 {
		t.Errorf("Expected returned public snapshot to strip cookies")
	}
	qm.CancelTask(task.ID)
	for _, snap := range qm.GetAll() {
		if len(snap.Cookies) != 0 {
			t.Errorf("Expected GetAll() snapshot to strip cookies")
		}
	}
}

func TestExtensionAutoPairingAndShowWindow(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "cfg.json"))
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "q.json"), nil)
	defer qm.Stop()
	srv := NewServer(cfg, qm, NewSSEBroker())

	windowShown := false
	srv.SetShowWindowHandler(func() {
		windowShown = true
	})

	handler := srv.setupRoutes()

	// 1. Web origin must be blocked from /api/pair (403)
	evilReq := httptest.NewRequest("GET", "/api/pair", nil)
	evilReq.Header.Set("Origin", "https://malicious-site.example")
	evilRec := httptest.NewRecorder()
	handler.ServeHTTP(evilRec, evilReq)
	if evilRec.Code != 403 {
		t.Fatalf("Expected 403 Forbidden for web origin on /api/pair, got %d", evilRec.Code)
	}

	// 2. Empty origin must be blocked from /api/pair (403)
	emptyReq := httptest.NewRequest("GET", "/api/pair", nil)
	emptyRec := httptest.NewRecorder()
	handler.ServeHTTP(emptyRec, emptyReq)
	if emptyRec.Code != 403 {
		t.Fatalf("Expected 403 Forbidden for empty origin on /api/pair, got %d", emptyRec.Code)
	}

	// 3. Browser extension origin must succeed on /api/pair (200)
	extReq := httptest.NewRequest("GET", "/api/pair", nil)
	extReq.Header.Set("Origin", "chrome-extension://abcdefghijklmnopabcdefghijklmnop")
	extRec := httptest.NewRecorder()
	handler.ServeHTTP(extRec, extReq)
	if extRec.Code != 200 || !strings.Contains(extRec.Body.String(), cfg.GetToken()) {
		t.Fatalf("Expected 200 OK with token for chrome-extension origin, got %d (%s)", extRec.Code, extRec.Body.String())
	}

	// 4. MV3 service worker with <all_urls> (no Origin, Sec-Fetch-Site: none, X-GrabAll-Ext) must succeed
	mv3Req := httptest.NewRequest("POST", "/api/pair", nil)
	mv3Req.Header.Set("Sec-Fetch-Site", "none")
	mv3Req.Header.Set("X-GrabAll-Ext", "abcdefghijklmnopabcdefghijklmnop")
	mv3Rec := httptest.NewRecorder()
	handler.ServeHTTP(mv3Rec, mv3Req)
	if mv3Rec.Code != 200 {
		t.Fatalf("Expected 200 OK for MV3 service worker X-GrabAll-Ext, got %d", mv3Rec.Code)
	}

	// 5. Web origin trying to spoof X-GrabAll-Ext must still be blocked (403)
	spoofReq := httptest.NewRequest("POST", "/api/pair", nil)
	spoofReq.Header.Set("Origin", "https://evil.example")
	spoofReq.Header.Set("Sec-Fetch-Site", "cross-site")
	spoofReq.Header.Set("X-GrabAll-Ext", "spoofed")
	spoofRec := httptest.NewRecorder()
	handler.ServeHTTP(spoofRec, spoofReq)
	if spoofRec.Code != 403 {
		t.Fatalf("Expected 403 Forbidden for cross-site web request with X-GrabAll-Ext, got %d", spoofRec.Code)
	}

	// 4. /api/show-window with token triggers native window callback
	showReq := httptest.NewRequest("POST", "/api/show-window", nil)
	showReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	showRec := httptest.NewRecorder()
	handler.ServeHTTP(showRec, showReq)
	if showRec.Code != 200 || !windowShown {
		t.Fatalf("Expected /api/show-window to invoke callback and return 200, got %d (shown=%v)", showRec.Code, windowShown)
	}
}

func TestZipExtractionAndSubprocessTreeKill(t *testing.T) {
	// 1. Verify downloadAndExtractZip extracts yt-dlp.exe + _internal/ and blocks zip-slip paths
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f1, _ := zw.Create("yt-dlp.exe")
	_, _ = f1.Write([]byte("mock-exe"))
	f2, _ := zw.Create("_internal/python312.dll")
	_, _ = f2.Write([]byte("mock-dll"))
	fEvil, _ := zw.Create("../escaped.txt")
	_, _ = fEvil.Write([]byte("evil"))
	_ = zw.Close()

	zipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}))
	defer zipSrv.Close()

	destDir := filepath.Join(t.TempDir(), "bin")
	if err := downloadAndExtractZip(zipSrv.URL+"/yt-dlp_win.zip", destDir, 10*time.Second); err != nil {
		t.Fatalf("downloadAndExtractZip failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "yt-dlp.exe")); err != nil {
		t.Fatalf("Expected extracted yt-dlp.exe, got error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "_internal", "python312.dll")); err != nil {
		t.Fatalf("Expected extracted _internal/python312.dll, got error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(destDir), "escaped.txt")); err == nil {
		t.Fatalf("Zip-slip file ../escaped.txt should NOT have been extracted")
	}

	// 2. Verify configureSubprocess kills parent + spawned child quickly on context cancel
	if runtime.GOOS != "windows" {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & wait")
		configureSubprocess(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("Failed to start test subprocess: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		startCancel := time.Now()
		cancel()
		_ = cmd.Wait()
		if elapsed := time.Since(startCancel); elapsed > 2*time.Second {
			t.Fatalf("Subprocess tree took too long to terminate on cancel: %v", elapsed)
		}
	}
}
