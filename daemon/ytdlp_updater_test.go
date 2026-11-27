package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestYtDlpUpdaterCheckRedirectAndFallback(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "yt-dlp")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho 2026.10.01\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.YtDlpPath = fakeBin

	// 1. Primary HEAD redirect check (zero API quota usage)
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			w.Header().Set("Location", "https://github.com/yt-dlp/yt-dlp/releases/tag/2026.11.20")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer redirectSrv.Close()

	u := NewYtDlpUpdater(nil)
	u.latestPageURL = redirectSrv.URL + "/releases/latest"
	u.latestAPIURL = ""
	u.versionFn = func(binPath string) string { return "2026.10.01" }

	info, err := u.CheckForUpdate(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("CheckForUpdate failed: %v", err)
	}
	if !info.Installed || info.CurrentVersion != "2026.10.01" || info.LatestVersion != "2026.11.20" {
		t.Fatalf("unexpected version info: %+v", info)
	}
	if !info.UpdateAvailable || info.Stage != "update_available" {
		t.Fatalf("expected UpdateAvailable=true stage=update_available, got %+v", info)
	}

	// 2. Fallback to GitHub Releases API when redirect endpoint fails
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "2026.11.25"}`))
	}))
	defer apiSrv.Close()

	u2 := NewYtDlpUpdater(nil)
	u2.latestPageURL = redirectSrv.URL + "/broken"
	u2.latestAPIURL = apiSrv.URL
	u2.versionFn = func(binPath string) string { return "2026.11.25" }

	info2, err := u2.CheckForUpdate(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("API fallback CheckForUpdate failed: %v", err)
	}
	if info2.UpdateAvailable || info2.Stage != "up_to_date" {
		t.Fatalf("expected up_to_date when versions match, got %+v", info2)
	}
}

func TestYtDlpUpdaterProgressAndActiveDownloadGuard(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "config.json"))
	cfg.DownloadDir = tmpDir
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), nil)
	defer qm.Stop()

	// Add a simulated active download to test 409/active-download guard
	qm.tasksMu.Lock()
	qm.tasks = append(qm.tasks, &DownloadTask{ID: "active1", Status: StatusDownloading})
	qm.tasksMu.Unlock()

	u := NewYtDlpUpdater(nil)
	u.latestPageURL = ""
	u.latestAPIURL = ""
	u.state.LatestVersion = "2026.11.27"

	if _, err := u.Update(context.Background(), cfg, qm, false, nil); !errors.Is(err, errYtDlpActiveDownloads) {
		t.Fatalf("expected errYtDlpActiveDownloads when active downloads exist, got %v", err)
	}

	// Clear active download and run full progress update
	qm.tasksMu.Lock()
	qm.tasks[0].Status = StatusPaused
	qm.tasksMu.Unlock()

	var stages []string
	u.installFn = func(destDir string, onProgress func(stage string, downloaded, total int64)) (string, error) {
		if onProgress != nil {
			onProgress("downloading", 5*1024*1024, 10*1024*1024)
			onProgress("extracting", 0, 0)
			onProgress("quickjs", 0, 0)
		}
		outBin := filepath.Join(tmpDir, "yt-dlp-new")
		if err := os.WriteFile(outBin, []byte("bin"), 0755); err != nil {
			return "", err
		}
		return outBin, nil
	}
	u.versionFn = func(binPath string) string { return "2026.11.27" }
	u.prewarmFn = nil

	info, err := u.Update(context.Background(), cfg, qm, false, func(st *YtDlpUpdateInfo) {
		stages = append(stages, st.Stage)
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if !info.Installed || info.CurrentVersion != "2026.11.27" || info.UpdateAvailable || info.Updating || info.Stage != "updated" {
		t.Fatalf("unexpected post-update info: %+v", info)
	}
	if marker := readYtDlpVersionMarker(info.BinaryPath); marker != "2026.11.27" {
		t.Errorf("expected version marker 2026.11.27, got %q", marker)
	}
	joined := strings.Join(stages, ",")
	if !strings.Contains(joined, "downloading") || !strings.Contains(joined, "extracting") || !strings.Contains(joined, "updated") {
		t.Errorf("expected live downloading/extracting/updated stages, got: %v", stages)
	}
}

func TestYtDlpUpdateAPIEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "config.json"))
	cfg.DownloadDir = tmpDir
	broker := NewSSEBroker()
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), broker)
	defer qm.Stop()

	srv := NewServer(cfg, qm, broker)
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/yt-dlp/yt-dlp/releases/tag/2026.11.27")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirectSrv.Close()

	srv.ytdlpUpdater.latestPageURL = redirectSrv.URL
	srv.ytdlpUpdater.latestAPIURL = ""
	srv.ytdlpUpdater.versionFn = func(binPath string) string { return "2026.10.10" }
	srv.ytdlpUpdater.prewarmFn = nil
	srv.ytdlpUpdater.installFn = func(destDir string, onProgress func(stage string, downloaded, total int64)) (string, error) {
		p := filepath.Join(tmpDir, "yt-dlp-installed")
		_ = os.WriteFile(p, []byte("ok"), 0755)
		return p, nil
	}

	handler := srv.setupRoutes()

	// 1. GET /api/ytdlp/check?force=1
	req := httptest.NewRequest(http.MethodGet, "/api/ytdlp/check?force=1", nil)
	req.Header.Set("X-Daemon-Token", cfg.GetToken())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ytdlp/check status=%d body=%s", rec.Code, rec.Body.String())
	}
	var checkResp YtDlpUpdateInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &checkResp); err != nil {
		t.Fatal(err)
	}
	if checkResp.LatestVersion != "2026.11.27" || !checkResp.UpdateAvailable {
		t.Fatalf("unexpected check response: %+v", checkResp)
	}

	// 2. POST /api/ytdlp/update
	req2 := httptest.NewRequest(http.MethodPost, "/api/ytdlp/update", strings.NewReader(`{"force":false}`))
	req2.Header.Set("X-Daemon-Token", cfg.GetToken())
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST /api/ytdlp/update status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var updResp struct {
		Success bool            `json:"success"`
		Version string          `json:"version"`
		Info    YtDlpUpdateInfo `json:"info"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &updResp); err != nil {
		t.Fatal(err)
	}
	if !updResp.Success || !updResp.Info.Installed || updResp.Info.Stage != "updated" {
		t.Fatalf("unexpected update response: %+v", updResp)
	}
}

func TestYouTubeCookiePersistenceAndAuthValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "config.json"))
	cfg.DownloadDir = tmpDir
	dataFile := filepath.Join(tmpDir, "queue.json")
	futureExp := float64(time.Now().Add(48 * time.Hour).Unix())
	pastExp := float64(time.Now().Add(-2 * time.Hour).Unix())

	// 1. google.com-only SID without youtube.com auth cookie must NOT qualify
	googleOnly := []Cookie{
		{Name: "SID", Value: "g_only", Domain: ".google.com", Path: "/", ExpirationDate: futureExp},
		{Name: "VISITOR_INFO1_LIVE", Value: "anon", Domain: ".youtube.com", Path: "/", ExpirationDate: futureExp},
	}
	if hasYouTubeAuthCookies(googleOnly) {
		t.Errorf("expected google.com-only SID to return hasYouTubeAuthCookies=false")
	}

	// 2. Expired youtube.com LOGIN_INFO must NOT qualify
	expiredYT := []Cookie{
		{Name: "LOGIN_INFO", Value: "old", Domain: ".youtube.com", Path: "/", ExpirationDate: pastExp},
	}
	if hasYouTubeAuthCookies(expiredYT) {
		t.Errorf("expected expired LOGIN_INFO to return hasYouTubeAuthCookies=false")
	}

	// 3. Valid youtube.com cookies persist across QueueManager restarts and resist logged-out overwrites
	validYT := []Cookie{
		{Name: "LOGIN_INFO", Value: "live_token", Domain: ".youtube.com", Path: "/", Secure: true, ExpirationDate: futureExp},
		{Name: "SAPISID", Value: "sapi_val", Domain: ".youtube.com", Path: "/", Secure: true, ExpirationDate: futureExp},
	}

	qm1 := NewQueueManager(cfg, dataFile, nil)
	if !qm1.UpdateYouTubeCookies(validYT) {
		t.Fatalf("expected UpdateYouTubeCookies to accept valid YouTube cookies")
	}
	if qm1.UpdateYouTubeCookies(googleOnly) {
		t.Errorf("expected logged-out sync to be rejected and NOT overwrite signed-in cookies")
	}
	signedIn, count, updatedAt := qm1.YouTubeCookieStatus()
	if !signedIn || count != 2 || updatedAt.IsZero() {
		t.Fatalf("unexpected YouTubeCookieStatus: signedIn=%v count=%d updatedAt=%v", signedIn, count, updatedAt)
	}
	qm1.Stop()

	// Reload fresh QueueManager from same data dir -> must restore persisted cookies!
	qm2 := NewQueueManager(cfg, dataFile, nil)
	defer qm2.Stop()
	restoredSignedIn, restoredCount, _ := qm2.YouTubeCookieStatus()
	if !restoredSignedIn || restoredCount != 2 {
		t.Fatalf("expected persisted YouTube cookies to reload after restart, got signedIn=%v count=%d", restoredSignedIn, restoredCount)
	}

	// Adding a YouTube task with only logged-out cookies must fall back to persisted signed-in cookies!
	task, err := qm2.AddTask(AddRequest{
		URL:     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Engine:  EngineYtDlp,
		Cookies: googleOnly,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	stored := qm2.GetByID(task.ID)
	if stored == nil || !hasYouTubeAuthCookies(stored.Cookies) {
		t.Fatalf("expected AddTask to fall back to persisted signed-in cookies when request had only non-auth cookies")
	}
}

func TestWriteCachedInfoJSONUniquePaths(t *testing.T) {
	url := "https://www.youtube.com/watch?v=uniqueCacheTest"
	storePreviewCache(url, []byte(`{"id":"uniqueCacheTest","title":"Test"}`))
	defer dropPreviewCache(url)

	p1 := writeCachedInfoJSON(url, "hybrid")
	time.Sleep(2 * time.Millisecond)
	p2 := writeCachedInfoJSON(url, "hybrid")
	defer os.Remove(p1)
	defer os.Remove(p2)

	if p1 == "" || p2 == "" || p1 == p2 {
		t.Fatalf("expected unique temp info paths for concurrent hybrid probes, got %q and %q", p1, p2)
	}
}
