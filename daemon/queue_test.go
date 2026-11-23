package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

	// 3. Verify player cache detection & automatic 3-file rotation
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	solverDir := filepath.Join(cacheRoot, "challenge-solver")
	if err := os.MkdirAll(solverDir, 0755); err != nil {
		t.Fatal(err)
	}
	if hasCachedPlayerSolver(cacheRoot) {
		t.Fatalf("Expected false before player files exist")
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		fp := filepath.Join(solverDir, "player_test_"+string(rune('A'+i))+".json")
		_ = os.WriteFile(fp, []byte("{}"), 0644)
		mod := now.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(fp, mod, mod)
	}
	if !hasCachedPlayerSolver(cacheRoot) {
		t.Fatalf("Expected true after player files exist")
	}
	pruneOldPlayerCaches(cacheRoot)
	rem, _ := os.ReadDir(solverDir)
	if len(rem) != 3 {
		t.Fatalf("Expected pruneOldPlayerCaches to keep 3 newest files, got %d", len(rem))
	}
}

func TestWindowsMaxPathAndUnicodeSanitization(t *testing.T) {
	// 1. Multi-byte Sinhala UTF-8 must never split mid-rune or leave trailing spaces/dots
	sinhalaPlaylist := "Mix - Wickszen - Natawannam | Mewa Balan Iddi (මේවා බලන් ඉද්දි) Official Music Video (Dir.By Pasindu Fdo)..."
	sub := sanitizeSubDirName(sinhalaPlaylist)
	if !utf8.ValidString(sub) {
		t.Fatalf("sanitizeSubDirName produced invalid UTF-8: %q", sub)
	}
	if utf16Len(sub) > 55 {
		t.Fatalf("Expected sanitizeSubDirName <= 55 UTF-16 units, got %d (%q)", utf16Len(sub), sub)
	}
	if strings.HasSuffix(sub, " ") || strings.HasSuffix(sub, ".") {
		t.Fatalf("Subdir must not end with space or dot: %q", sub)
	}

	// 2. Exact Windows MAX_PATH overflow scenario from user report (existing long Mix folder + long multi-artist track)
	legacyWinDir := `C:\Users\Administrator\Downloads\Mix - Wickszen - Natawannam _ Mewa Balan Iddi (මේවා බලන් ඉද්දි) Official Music Video (Dir.By Pasindu Fdo)`
	longTrackTitle := "Mata Aye Ona (මට ආයේ ඕනා) - Raveen Tharuka Yasas Medagedara Shanudrie, Zenith, Kasuni, Piyumali"

	// Case A: Template form ("04 - %(title)s [%(id)s].%(ext)s")
	fittedTmpl := fitYtDlpOutputTemplate(legacyWinDir, "04 - %(title)s [%(id)s].%(ext)s")
	if !strings.Contains(fittedTmpl, "%(title).65s") {
		t.Fatalf("Expected dynamic title precision slice in template, got %q", fittedTmpl)
	}
	simulatedExpandedA := legacyWinDir + `\` + "04 - " + truncateUTF16Safe(longTrackTitle, 65) + " [2euX0MvU-Wo].f251.webm.part"
	if utf16Len(simulatedExpandedA) >= 260 {
		t.Fatalf("Expanded path A exceeded Windows MAX_PATH (260): %d (%q)", utf16Len(simulatedExpandedA), simulatedExpandedA)
	}

	// Case B: Persisted literal .webm filename from an earlier failed attempt before v2.1.2
	persistedFailedName := "04 - " + longTrackTitle + " [2euX0MvU-Wo].webm"
	fittedLiteral := fitYtDlpOutputTemplate(legacyWinDir, persistedFailedName)
	if !utf8.ValidString(fittedLiteral) || !strings.HasSuffix(fittedLiteral, " [2euX0MvU-Wo].%(ext)s") {
		t.Fatalf("Expected fitted literal template to preserve [id].%%(ext)s and valid UTF-8, got %q", fittedLiteral)
	}
	simulatedExpandedB := legacyWinDir + `\` + strings.TrimSuffix(fittedLiteral, ".%(ext)s") + ".f251.webm.part"
	if utf16Len(simulatedExpandedB) >= 260 {
		t.Fatalf("Expanded path B exceeded Windows MAX_PATH (260): %d (%q)", utf16Len(simulatedExpandedB), simulatedExpandedB)
	}

	// 3. Existing legacy subfolder on disk is reused when retrying in-progress playlists
	baseTmp := t.TempDir()
	legacyName := "Mix - Existing Long Playlist Folder For Retry Continuity"
	legacyFull := filepath.Join(baseTmp, legacyName)
	if err := os.MkdirAll(legacyFull, 0755); err != nil {
		t.Fatal(err)
	}
	if got := resolveTaskDownloadDir(baseTmp, legacyName); got != legacyFull {
		t.Fatalf("Expected resolveTaskDownloadDir to reuse existing legacy folder %q, got %q", legacyFull, got)
	}

	// 4. Windows reserved device names are safely prefixed
	if got := sanitizeFileName("CON.txt"); strings.EqualFold(strings.TrimSuffix(got, ".txt"), "CON") {
		t.Fatalf("Expected Windows reserved name CON.txt to be escaped, got %q", got)
	}
}

func TestTurboParallelDownloadAndLiveModeSwitch(t *testing.T) {
	// Generate deterministic 5.2 MB payload (> 2x 2MB blocks so Turbo spawns multiple workers)
	const payloadSize = 5*1024*1024 + 200*1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte((i*31 + 17) & 0xFF)
	}

	// Thread-safe Range server with optional throttled stream to test live mode switching
	rangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow.mp4" {
			http.ServeContent(w, r, "slow.mp4", time.Unix(1700000000, 0), &slowReadSeeker{data: payload, firstDelay: 120 * time.Millisecond})
			return
		}
		http.ServeContent(w, r, "movie.mp4", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer rangeSrv.Close()

	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "tok",
		DownloadDir:   tmpDir,
		MaxConcurrent: 2,
		SpeedMode:     SpeedModeTurbo,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	// 1. Full Turbo multi-connection download integrity check
	t1, err := qm.AddTask(AddRequest{
		URL:       rangeSrv.URL + "/movie.mp4",
		FileName:  "turbo_movie.mp4",
		Engine:    EngineDirect,
		SpeedMode: SpeedModeTurbo,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur := qm.GetByID(t1.ID)
		cur.mu.Lock()
		st := cur.Status
		errMsg := cur.ErrorMessage
		cur.mu.Unlock()
		if st == StatusCompleted {
			break
		}
		if st == StatusError {
			t.Fatalf("Turbo download failed: %s", errMsg)
		}
		time.Sleep(25 * time.Millisecond)
	}

	gotBytes, err := os.ReadFile(filepath.Join(tmpDir, "turbo_movie.mp4"))
	if err != nil {
		t.Fatalf("Expected completed turbo_movie.mp4: %v", err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("Turbo downloaded file byte mismatch (len=%d vs %d)", len(gotBytes), len(payload))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "turbo_movie.mp4.part.turbo")); err == nil {
		t.Fatalf("Expected .part.turbo sidecar to be cleaned up on completion")
	}

	// 2. Start in Turbo, switch live to Regular (Watch While Downloading) mid-flight, verify exact bytes!
	t2, err := qm.AddTask(AddRequest{
		URL:       rangeSrv.URL + "/slow.mp4",
		FileName:  "switched_movie.mp4",
		Engine:    EngineDirect,
		SpeedMode: SpeedModeTurbo,
	})
	if err != nil {
		t.Fatalf("AddTask slow failed: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if !qm.SetTaskSpeedMode(t2.ID, SpeedModeRegular) {
		t.Fatalf("Expected SetTaskSpeedMode to succeed on active download")
	}

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cur := qm.GetByID(t2.ID)
		cur.mu.Lock()
		st := cur.Status
		errMsg := cur.ErrorMessage
		cur.mu.Unlock()
		if st == StatusCompleted {
			break
		}
		if st == StatusError {
			t.Fatalf("Switched download failed: %s", errMsg)
		}
		time.Sleep(30 * time.Millisecond)
	}

	gotSwitched, err := os.ReadFile(filepath.Join(tmpDir, "switched_movie.mp4"))
	if err != nil {
		t.Fatalf("Expected completed switched_movie.mp4: %v", err)
	}
	if !bytes.Equal(gotSwitched, payload) {
		t.Fatalf("Switched mode file byte mismatch (len=%d vs %d)", len(gotSwitched), len(payload))
	}
}

type slowReadSeeker struct {
	data       []byte
	pos        int
	firstDelay time.Duration
	delayed    bool
}

func (s *slowReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	if !s.delayed && s.firstDelay > 0 {
		s.delayed = true
		time.Sleep(s.firstDelay)
	}
	n := copy(p, s.data[s.pos:])
	s.pos += n
	return n, nil
}

func (s *slowReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = int(offset)
	case io.SeekCurrent:
		s.pos += int(offset)
	case io.SeekEnd:
		s.pos = len(s.data) + int(offset)
	}
	if s.pos < 0 {
		s.pos = 0
	}
	return int64(s.pos), nil
}

func TestYtSizeTracker(t *testing.T) {
	tracker := &ytSizeTracker{}

	// Test 1: Even if reportedTotal wildly fluctuates (e.g. 5GB -> 2GB -> 800MB fragment estimate),
	// exactStreamSize (YouTube's real content-length) locks the total size stable!
	dl, tot := tracker.update("137", 10*1024*1024, 500*1024*1024, 5*1024*1024*1024)
	if dl != 10*1024*1024 || tot != 500*1024*1024 {
		t.Fatalf("Expected dl=10MB, tot=500MB; got dl=%d, tot=%d", dl, tot)
	}

	dl, tot = tracker.update("137", 80*1024*1024, 500*1024*1024, 2*1024*1024*1024)
	if dl != 80*1024*1024 || tot != 500*1024*1024 {
		t.Fatalf("Total size should stay locked at 500MB, got dl=%d, tot=%d", dl, tot)
	}

	// Test 2: Transition from video stream ("137") to audio stream ("140")
	// Completed video stream (500MB) must roll over and sum seamlessly with audio stream (20MB)
	dl, tot = tracker.update("140", 5*1024*1024, 20*1024*1024, 20*1024*1024)
	if dl != 505*1024*1024 || tot != 520*1024*1024 {
		t.Fatalf("Expected combined dl=505MB, tot=520MB; got dl=%d, tot=%d", dl, tot)
	}

	// Test 3: Sidecar size file with combined size
	tmpFile := filepath.Join(t.TempDir(), "size.txt")
	_ = os.WriteFile(tmpFile, []byte("550000000\n"), 0644)
	tracker2 := &ytSizeTracker{}
	tracker2.loadCombinedFromFile(tmpFile)
	if !tracker2.combinedKnown() || tracker2.combined != 550000000 {
		t.Fatalf("Failed to load combined size from file: %d", tracker2.combined)
	}
}

func TestCancelAndCleanupArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	task, err := qm.AddTask(AddRequest{
		URL:      "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		Title:    "Test Video [aqz-KE-bpKQ]",
		FileName: "Test Video [aqz-KE-bpKQ].mp4",
		Engine:   EngineYtDlp,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	// Simulate stray partial files created during Turbo DASH or interrupted attempts
	partFile1 := filepath.Join(tmpDir, "Test Video [aqz-KE-bpKQ].f137.mp4.part")
	partFile2 := filepath.Join(tmpDir, "Test Video [aqz-KE-bpKQ].f251.webm.part")
	fragFile := filepath.Join(tmpDir, "Test Video [aqz-KE-bpKQ].f137.mp4.part-Frag0.part")
	ytdlFile := filepath.Join(tmpDir, "Test Video [aqz-KE-bpKQ].mp4.ytdl")
	turboMeta := filepath.Join(tmpDir, "Test Video [aqz-KE-bpKQ].mp4.part.turbo")

	for _, p := range []string{partFile1, partFile2, fragFile, ytdlFile, turboMeta} {
		if err := os.WriteFile(p, []byte("partial content"), 0644); err != nil {
			t.Fatalf("Failed to create test artifact %s: %v", p, err)
		}
	}

	qm.CancelTask(task.ID)
	// Give asynchronous cleanup a moment
	time.Sleep(350 * time.Millisecond)

	for _, p := range []string{partFile1, partFile2, fragFile, ytdlFile, turboMeta} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Expected artifact %s to be cleaned up on cancel, but it still exists", filepath.Base(p))
		}
	}
}

func TestDeleteTaskCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	// 1. Incomplete task deleted with deleteFile = false: partial files MUST be purged!
	t1, err := qm.AddTask(AddRequest{
		URL:      "https://example.com/test.bin",
		Title:    "Test Unfinished",
		FileName: "test_unfinished.bin",
		Engine:   EngineDirect,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	part1 := filepath.Join(tmpDir, "test_unfinished.bin.part")
	turbo1 := filepath.Join(tmpDir, "test_unfinished.bin.part.turbo")
	_ = os.WriteFile(part1, []byte("incomplete data"), 0644)
	_ = os.WriteFile(turbo1, []byte("{}"), 0644)

	qm.DeleteTask(t1.ID, false)
	time.Sleep(350 * time.Millisecond)

	if _, err := os.Stat(part1); !os.IsNotExist(err) {
		t.Errorf("Expected partial file %s to be purged when incomplete task deleted", part1)
	}
	if _, err := os.Stat(turbo1); !os.IsNotExist(err) {
		t.Errorf("Expected turbo file %s to be purged when incomplete task deleted", turbo1)
	}

	// 2. Completed task: deleteFile = false retains completed file; deleteFile = true deletes it.
	t2, err := qm.AddTask(AddRequest{
		URL:      "https://example.com/done.mp4",
		Title:    "Finished Video",
		FileName: "done.mp4",
		Engine:   EngineDirect,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	final2 := filepath.Join(tmpDir, "done.mp4")
	_ = os.WriteFile(final2, []byte("final video content"), 0644)
	t2.mu.Lock()
	t2.Status = StatusCompleted
	t2.SavePath = final2
	t2.mu.Unlock()

	qm.DeleteTask(t2.ID, false)
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(final2); os.IsNotExist(err) {
		t.Errorf("Completed file %s should NOT be removed when deleteFile is false", final2)
	}

	// Now add again and delete with deleteFile = true
	t3, _ := qm.AddTask(AddRequest{
		URL:      "https://example.com/done.mp4",
		Title:    "Finished Video 3",
		FileName: "done.mp4",
		Engine:   EngineDirect,
	})
	t3.mu.Lock()
	t3.Status = StatusCompleted
	t3.SavePath = final2
	t3.mu.Unlock()

	qm.DeleteTask(t3.ID, true)
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(final2); !os.IsNotExist(err) {
		t.Errorf("Completed file %s SHOULD be removed when deleteFile is true", final2)
	}
}

func TestCompletionPurgesTurboArtifactsAndResolvesRealFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir: tmpDir,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	task, _ := qm.AddTask(AddRequest{
		URL:      "https://www.youtube.com/watch?v=2euX0MvU-Wo",
		Title:    "Alan Walker - Faded [2euX0MvU-Wo]",
		FileName: "Alan Walker - Faded [2euX0MvU-Wo].mp4.part",
		Engine:   EngineYtDlp,
	})

	// Simulate final merged video file exists on disk
	finalVideo := filepath.Join(tmpDir, "Alan Walker - Faded [2euX0MvU-Wo].mp4")
	_ = os.WriteFile(finalVideo, []byte("final merged 1080p mp4 bytes"), 0644)

	// Simulate stray leftover DASH fragment and intermediate streams from earlier Turbo attempt
	strayPart := filepath.Join(tmpDir, "Alan Walker - Faded [2euX0MvU-Wo].f616.webm.part")
	strayFrag := filepath.Join(tmpDir, "Alan Walker - Faded [2euX0MvU-Wo].f616.webm.part-Frag0.part")
	strayYtdl := filepath.Join(tmpDir, "Alan Walker - Faded [2euX0MvU-Wo].mp4.ytdl")
	_ = os.WriteFile(strayPart, []byte("stray chunk"), 0644)
	_ = os.WriteFile(strayFrag, []byte("stray frag"), 0644)
	_ = os.WriteFile(strayYtdl, []byte("ytdl temp"), 0644)

	// Test resolveExistingMediaFile properly resolves finalVideo even though cand ended in .part
	resolved := resolveExistingMediaFile(tmpDir, task.SavePath, task.FileName, task.Title)
	if resolved != finalVideo {
		t.Fatalf("resolveExistingMediaFile failed: expected %s, got %s", finalVideo, resolved)
	}

	// Purge artifacts as done on completion
	qm.cleanTaskArtifacts(task, false)

	// Final video must be preserved
	if _, err := os.Stat(finalVideo); os.IsNotExist(err) {
		t.Fatalf("Final completed video was accidentally deleted!")
	}

	// All stray artifacts must be purged
	for _, stray := range []string{strayPart, strayFrag, strayYtdl} {
		if _, err := os.Stat(stray); !os.IsNotExist(err) {
			t.Errorf("Expected stray turbo artifact %s to be purged, but it remains", filepath.Base(stray))
		}
	}
}

func TestResolveTaskRevealPathsWithAndWithoutPart(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir: tmpDir,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	task, _ := qm.AddTask(AddRequest{
		URL:      "https://example.com/clip.mp4",
		Title:    "Music Clip",
		FileName: "clip.mp4",
		Engine:   EngineDirect,
	})

	finalFile := filepath.Join(tmpDir, "clip.mp4")
	_ = os.WriteFile(finalFile, []byte("clip data"), 0644)

	// Even if task.SavePath points to clip.mp4.part (as during active downloads),
	// resolveTaskRevealPaths must accurately return the existing clip.mp4!
	task.mu.Lock()
	task.SavePath = filepath.Join(tmpDir, "clip.mp4.part")
	task.mu.Unlock()

	fp, dir := resolveTaskRevealPaths(cfg, qm, task.ID)
	if fp != finalFile {
		t.Errorf("Expected revealed file path %s, got %s", finalFile, fp)
	}
	if dir != tmpDir {
		t.Errorf("Expected revealed dir %s, got %s", tmpDir, dir)
	}
}

func TestYtDlpResumeTemplateConsistency(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir: tmpDir,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	// 1. Standard grab without custom file name
	task1, err := qm.AddTask(AddRequest{
		URL:    "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Engine: EngineYtDlp,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	tmplRun1 := fitYtDlpOutputTemplate(tmpDir, task1.CustomFileName)

	// Simulate first download run setting task.FileName to resolved video title
	task1.mu.Lock()
	task1.FileName = "Rick Astley - Never Gonna Give You Up (Official Music Video) [dQw4w9WgXcQ].mp4"
	task1.mu.Unlock()

	// On Resume, the template generated must be 100% IDENTICAL so yt-dlp finds the existing .part file!
	tmplResume := fitYtDlpOutputTemplate(tmpDir, task1.CustomFileName)
	if tmplResume != tmplRun1 {
		t.Fatalf("Resume template mismatch! Run 1: %q, Resume: %q", tmplRun1, tmplResume)
	}

	if !strings.Contains(tmplResume, "%(title)") || !strings.Contains(tmplResume, "%(id)s") {
		t.Errorf("Expected template to contain %%(title) and %%(id)s tokens, got: %s", tmplResume)
	}

	// 2. Custom filename grab
	task2, err := qm.AddTask(AddRequest{
		URL:      "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		FileName: "custom_video.mp4",
		Engine:   EngineYtDlp,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	tmplCustom1 := fitYtDlpOutputTemplate(tmpDir, task2.CustomFileName)
	task2.mu.Lock()
	task2.FileName = "custom_video.mp4"
	task2.mu.Unlock()
	tmplCustomResume := fitYtDlpOutputTemplate(tmpDir, task2.CustomFileName)
	if tmplCustomResume != tmplCustom1 {
		t.Fatalf("Custom resume template mismatch! Run 1: %q, Resume: %q", tmplCustom1, tmplCustomResume)
	}
}

func TestYtSizeTrackerResumeWithCompletedStream(t *testing.T) {
	tracker := &ytSizeTracker{}

	// Simulate Stream 1 (video, 100MB) was already downloaded in Run 1 before Pause.
	videoFile := "/downloads/sample.f137.mp4"
	tracker.addCompletedStream(videoFile, 100*1024*1024)

	// Now in Run 2 (Resume), yt-dlp starts downloading Stream 2 (audio, 20MB).
	// Currently 5MB of audio has been downloaded so far.
	audioFile := "/downloads/sample.f140.m4a"
	dl, total := tracker.update(audioFile, 5*1024*1024, 20*1024*1024, 20*1024*1024)

	expectedDl := int64(105 * 1024 * 1024) // 100MB video + 5MB audio
	expectedTotal := int64(120 * 1024 * 1024)

	if dl != expectedDl {
		t.Errorf("Expected downloaded bytes %d, got %d", expectedDl, dl)
	}
	if total != expectedTotal {
		t.Errorf("Expected total bytes %d, got %d", expectedTotal, total)
	}

	pct := float64(dl) / float64(total) * 100.0
	if pct < 85.0 || pct > 90.0 {
		t.Errorf("Expected progress around 87.5%%, got %.2f%%", pct)
	}
}

func TestVideoCodecNormalizationAndSortOrder(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", VideoCodecH264},
		{"h264", VideoCodecH264},
		{"H-264", VideoCodecH264},
		{"avc1", VideoCodecH264},
		{"264", VideoCodecH264},
		{"vp9", VideoCodecVP9},
		{"VP09", VideoCodecVP9},
		{"av1", VideoCodecAV1},
		{"av01", VideoCodecAV1},
		{"auto", VideoCodecAuto},
	}
	for _, tc := range cases {
		if got := normalizeVideoCodec(tc.input); got != tc.want {
			t.Errorf("normalizeVideoCodec(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	if got := buildYtDlpSortOrder("1080", VideoCodecH264, ",proto:http_dash_segments"); got != "res:1080,vcodec:h264,ext:mp4:m4a,proto:http_dash_segments" {
		t.Errorf("unexpected 1080 h264 sort order: %q", got)
	}
	if got := buildYtDlpSortOrder("2160", VideoCodecVP9, ""); got != "res:2160,vcodec:vp9,ext:mp4:m4a" {
		t.Errorf("unexpected 4k vp9 sort order: %q", got)
	}
	if got := buildYtDlpSortOrder("best", VideoCodecAV1, ""); got != "vcodec:av01,ext:mp4:m4a" {
		t.Errorf("unexpected best av1 sort order: %q", got)
	}
	if got := buildYtDlpSortOrder("720", VideoCodecAuto, ""); got != "res:720,ext:mp4:m4a" {
		t.Errorf("unexpected 720 auto sort order: %q", got)
	}

	tmpDir := t.TempDir()
	cfg := &Config{
		DownloadDir: tmpDir,
		VideoCodec:  VideoCodecH264,
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	// Default task inherits H.264
	t1, err := qm.AddTask(AddRequest{
		URL:    "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Engine: EngineYtDlp,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	if snapshotTask(t1, false).VideoCodec != VideoCodecH264 {
		t.Errorf("Expected default task VideoCodec %q, got %q", VideoCodecH264, t1.VideoCodec)
	}

	// Explicit override uses requested codec
	t2, err := qm.AddTask(AddRequest{
		URL:        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Engine:     EngineYtDlp,
		VideoCodec: "av1",
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	if snapshotTask(t2, false).VideoCodec != VideoCodecAV1 {
		t.Errorf("Expected explicit task VideoCodec %q, got %q", VideoCodecAV1, t2.VideoCodec)
	}
}

func TestYtDlpResilienceFlags(t *testing.T) {
	args := buildYtDlpBaseArgs("/tmp/cache", "/tmp/dl", "6")

	hasFlag := func(flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	hasPair := func(flag, val string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == val {
				return true
			}
		}
		return false
	}

	if !hasFlag("--abort-on-unavailable-fragments") {
		t.Errorf("expected --abort-on-unavailable-fragments in args: %v", args)
	}
	if !hasPair("--retries", "10") {
		t.Errorf("expected --retries 10 in args: %v", args)
	}
	if !hasPair("--fragment-retries", "10") {
		t.Errorf("expected --fragment-retries 10 in args: %v", args)
	}
	if !hasPair("--socket-timeout", "30") {
		t.Errorf("expected --socket-timeout 30 in args: %v", args)
	}
	if !hasPair("--file-access-retries", "5") {
		t.Errorf("expected --file-access-retries 5 in args: %v", args)
	}
	if !hasPair("--retry-sleep", "http:exp=1:16") {
		t.Errorf("expected --retry-sleep http:exp=1:16 in args: %v", args)
	}
	if !hasPair("--retry-sleep", "fragment:exp=1:16") {
		t.Errorf("expected --retry-sleep fragment:exp=1:16 in args: %v", args)
	}
}

func TestYtDlpRetryableNetworkErrorClassification(t *testing.T) {
	retryable := []string{
		"HTTPSConnectionPool(host='rr1---sn.googlevideo.com', port=443): Read timed out",
		"yt-dlp failed: [download] Got error: HTTPConnectionPool: Connection reset by peer",
		"ERROR: unable to download video data: HTTP Error 403: Forbidden",
		"ERROR: fragment 200 not found, unable to continue",
	}
	for _, msg := range retryable {
		if !isRetryableYtDlpNetworkError(msg) {
			t.Errorf("expected error to be retryable: %q", msg)
		}
	}

	nonRetryable := []string{
		"ERROR: [youtube] xyz: Sign in to confirm you're not a bot",
		"ERROR: Postprocessing: No space left on device",
		"merged video is truncated (2.00 GB saved vs 11.10 GB expected)",
		"ERROR: Requested format is not available",
	}
	for _, msg := range nonRetryable {
		if isRetryableYtDlpNetworkError(msg) {
			t.Errorf("expected error NOT to be retryable: %q", msg)
		}
	}
}

func TestReconcileDashyYtdlAndModeSwitchSafety(t *testing.T) {
	tmpDir := t.TempDir()
	task := &DownloadTask{
		ID:       "dl_test_dashy",
		URL:      "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Title:    "Long Livestream [dQw4w9WgXcQ]",
		FileName: "Long Livestream [dQw4w9WgXcQ].mp4",
	}

	partPath := filepath.Join(tmpDir, "Long Livestream [dQw4w9WgXcQ].f137.mp4.part")
	f, err := os.Create(partPath)
	if err != nil {
		t.Fatalf("failed to create .part file: %v", err)
	}
	_ = f.Close()
	if err := os.Truncate(partPath, 20*ytDashyChunkSize); err != nil {
		t.Fatalf("failed to truncate .part file: %v", err)
	}

	// 1. Sequential .part exists with NO .ytdl -> Turbo must fall back to non-dashy HttpFD (allowDashy=false)
	if allow := prepareYtDlpResumeState(tmpDir, task, true); allow {
		t.Fatalf("expected allowDashy=false when .part exists without .ytdl checkpoint")
	}

	// 2. Write drifted dashy .ytdl (index=23 instead of 20) -> Turbo allows dashy AND aligns index back to 20
	ytdlPath := filepath.Join(tmpDir, "Long Livestream [dQw4w9WgXcQ].f137.mp4.ytdl")
	initialJSON := `{"downloader":{"current_fragment":{"index":23},"fragment_count":100,"extra_state":{"k":"v"}}}`
	if err := os.WriteFile(ytdlPath, []byte(initialJSON), 0644); err != nil {
		t.Fatalf("failed to write .ytdl file: %v", err)
	}

	if allow := prepareYtDlpResumeState(tmpDir, task, true); !allow {
		t.Fatalf("expected allowDashy=true when .ytdl checkpoint exists")
	}

	updatedRaw, err := os.ReadFile(ytdlPath)
	if err != nil {
		t.Fatalf("failed to read reconciled .ytdl: %v", err)
	}
	var parsed struct {
		Downloader struct {
			CurrentFragment struct {
				Index int64 `json:"index"`
			} `json:"current_fragment"`
			FragmentCount int64             `json:"fragment_count"`
			ExtraState    map[string]string `json:"extra_state"`
		} `json:"downloader"`
	}
	if err := json.Unmarshal(updatedRaw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal reconciled .ytdl: %v", err)
	}
	if parsed.Downloader.CurrentFragment.Index != 20 {
		t.Errorf("expected reconciled fragment index 20, got %d", parsed.Downloader.CurrentFragment.Index)
	}
	if parsed.Downloader.FragmentCount != 100 || parsed.Downloader.ExtraState["k"] != "v" {
		t.Errorf("expected fragment_count=100 and extra_state preserved, got %+v", parsed)
	}

	// 3. Regular mode switch -> stale .ytdl is removed cleanly
	prepareYtDlpResumeState(tmpDir, task, false)
	if _, err := os.Stat(ytdlPath); !os.IsNotExist(err) {
		t.Errorf("expected stale .ytdl to be removed when switching to Regular mode")
	}
}

func TestFormatYtDlpStderrAndPostMergeIntegrity(t *testing.T) {
	lines := []string{
		"WARNING: [download] HTTPSConnectionPool(host='rr1', port=443): Read timed out",
		"[download] Got error: HTTPSConnectionPool: retrying",
		"ERROR: Postprocessing: No space left on device",
	}
	formatted := formatYtDlpStderrError(lines)
	if !strings.HasPrefix(formatted, "ERROR: Postprocessing: No space left on device") {
		t.Errorf("expected newest ERROR: line first, got: %q", formatted)
	}

	tmpDir := t.TempDir()
	sizeFile := filepath.Join(tmpDir, "size.txt")
	if err := os.WriteFile(sizeFile, []byte("NA|11100000000|10800000000|300000000\n"), 0644); err != nil {
		t.Fatalf("write size file failed: %v", err)
	}
	tracker := &ytSizeTracker{}
	tracker.loadCombinedFromFile(sizeFile)
	if tracker.combinedExact != 11100000000 || tracker.combined != 11100000000 {
		t.Errorf("expected combinedExact=11100000000, combined=11100000000; got %d, %d", tracker.combinedExact, tracker.combined)
	}

	// 2.0 GB saved vs 11.1 GB exact video must fail integrity guard
	if err := checkMergedOutputIntegrity(false, 11100000000, 2000000000, 0); err == nil {
		t.Errorf("expected truncated 2 GB vs 11.1 GB video to fail integrity check")
	}
	// 10.9 GB saved vs 11.1 GB exact video (muxing container overhead diff) must pass
	if err := checkMergedOutputIntegrity(false, 11100000000, 10900000000, 0); err != nil {
		t.Errorf("expected 10.9 GB vs 11.1 GB video to pass, got error: %v", err)
	}
	// Any skipped fragments must fail
	if err := checkMergedOutputIntegrity(false, 11100000000, 11090000000, 2); err == nil {
		t.Errorf("expected skippedFragments=2 to fail integrity check")
	}
	// Audio-only transcode (e.g. MP3/Opus much smaller than source m4a/webm) must pass
	if err := checkMergedOutputIntegrity(true, 300000000, 80000000, 0); err != nil {
		t.Errorf("expected audio-only task to bypass video size ratio check, got: %v", err)
	}
}

func TestAutoStartParsingAndStartupApproved(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"C:\Program Files\GrabAll\graball.exe" --tray`, `C:\Program Files\GrabAll\graball.exe`},
		{`"C:\Users\User Name\Desktop\graball.exe"`, `C:\Users\User Name\Desktop\graball.exe`},
		{`C:\GrabAll\graball.exe --tray`, `C:\GrabAll\graball.exe`},
		{`C:\GrabAll\graball.exe`, `C:\GrabAll\graball.exe`},
		{`   `, ``},
	}
	for _, tc := range cases {
		if got := parseAutoStartExePath(tc.raw); got != tc.want {
			t.Errorf("parseAutoStartExePath(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	if !isStartupApprovedBytesEnabled(nil) {
		t.Errorf("empty StartupApproved data should default to enabled")
	}
	if !isStartupApprovedBytesEnabled([]byte{0x02, 0x00, 0x00, 0x00}) {
		t.Errorf("0x02 StartupApproved byte should be enabled")
	}
	if !isStartupApprovedBytesEnabled([]byte{0x06, 0x00, 0x00, 0x00}) {
		t.Errorf("0x06 StartupApproved byte should be enabled")
	}
	for _, b := range []byte{0x03, 0x01, 0x07} {
		if isStartupApprovedBytesEnabled([]byte{b, 0x00, 0x00, 0x00}) {
			t.Errorf("0x%02x StartupApproved byte should be disabled", b)
		}
	}
}




