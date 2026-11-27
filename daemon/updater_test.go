package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildTestZipBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create(%s): %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip Write(%s): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

func buildTestTarGzBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range entries {
		data := []byte(content)
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.2.8", "2.2.9", -1},
		{"pc-v2.2.9", "v2.2.9", 0},
		{"pc-v2.3.0", "2.2.9", 1},
		{"2.10.0", "2.9.9", 1},
		{"v2.2.9-beta", "2.2.9", 0},
		{"3.0", "2.9.9", 1},
	}
	for _, tc := range cases {
		if got := compareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckForUpdateFromLatestJSONIgnoresAndroid(t *testing.T) {
	manifestJSON := `{
  "android": {
    "version": "9.9.9",
    "tag": "android-v9.9.9",
    "url": "https://github.com/Avishka-55/graball-website/releases/download/android-v9.9.9/GrabAll-Android-v9.9.9.apk"
  },
  "pc": {
    "version": "2.3.0",
    "tag": "pc-v2.3.0",
    "published_at": "2026-11-23T06:00:00Z",
    "downloads": {
      "windows": {
        "file": "graball-windows-amd64.zip",
        "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "size_bytes": 3300000,
        "url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.3.0/graball-windows-amd64.zip"
      },
      "linux_amd64": {
        "file": "graball-linux-amd64.tar.gz",
        "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "size_bytes": 3000000,
        "url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.3.0/graball-linux-amd64.tar.gz"
      },
      "macos_arm64": {
        "file": "graball-darwin-arm64.tar.gz",
        "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        "size_bytes": 2900000,
        "url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.3.0/graball-darwin-arm64.tar.gz"
      }
    }
  }
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifestJSON))
	}))
	defer srv.Close()

	// Windows client check
	uWin := NewUpdater()
	uWin.manifestURL = srv.URL
	uWin.releasesURL = ""
	uWin.latestRedirectURL = ""
	uWin.currentVersion = "2.2.9"
	uWin.goos = "windows"
	uWin.goarch = "amd64"

	infoWin, err := uWin.CheckForUpdate(context.Background(), true)
	if err != nil {
		t.Fatalf("CheckForUpdate(windows) failed: %v", err)
	}
	if infoWin.LatestVersion != "2.3.0" || !infoWin.UpdateAvailable {
		t.Fatalf("expected pc version 2.3.0 available, got %+v", infoWin)
	}
	if infoWin.FileName != "graball-windows-amd64.zip" || infoWin.SHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("unexpected windows asset metadata: %+v", infoWin)
	}

	// Linux client check (should also ignore Android 9.9.9)
	uLin := NewUpdater()
	uLin.manifestURL = srv.URL
	uLin.releasesURL = ""
	uLin.latestRedirectURL = ""
	uLin.currentVersion = "2.3.0"
	uLin.goos = "linux"
	uLin.goarch = "amd64"

	infoLin, err := uLin.CheckForUpdate(context.Background(), true)
	if err != nil {
		t.Fatalf("CheckForUpdate(linux) failed: %v", err)
	}
	if infoLin.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=false when already at 2.3.0 (Android 9.9.9 must be ignored), got %+v", infoLin)
	}
	if infoLin.FileName != "graball-linux-amd64.tar.gz" {
		t.Errorf("expected graball-linux-amd64.tar.gz, got %q", infoLin.FileName)
	}
}

func TestCheckForUpdateFallbackToGitHubReleasesAPI(t *testing.T) {
	releasesJSON := `[
  {
    "tag_name": "android-v9.9.9",
    "draft": false,
    "prerelease": false,
    "assets": [
      {
        "name": "GrabAll-Android-v9.9.9.apk",
        "size": 40000000,
        "browser_download_url": "https://github.com/Avishka-55/graball-website/releases/download/android-v9.9.9/GrabAll-Android-v9.9.9.apk"
      }
    ]
  },
  {
    "tag_name": "pc-v9.0.0",
    "draft": true,
    "prerelease": false,
    "assets": [
      {
        "name": "graball-windows-amd64.zip",
        "size": 3000000,
        "browser_download_url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v9.0.0/graball-windows-amd64.zip"
      }
    ]
  },
  {
    "tag_name": "pc-v8.0.0",
    "draft": false,
    "prerelease": true,
    "assets": [
      {
        "name": "graball-windows-amd64.zip",
        "size": 3000000,
        "browser_download_url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v8.0.0/graball-windows-amd64.zip"
      }
    ]
  },
  {
    "tag_name": "pc-v2.2.8",
    "draft": false,
    "prerelease": false,
    "html_url": "https://github.com/Avishka-55/graball-website/releases/tag/pc-v2.2.8",
    "assets": [
      {
        "name": "graball-windows-amd64.zip",
        "size": 3242779,
        "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
        "browser_download_url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.2.8/graball-windows-amd64.zip"
      }
    ]
  },
  {
    "tag_name": "pc-v2.3.1",
    "draft": false,
    "prerelease": false,
    "html_url": "https://github.com/Avishka-55/graball-website/releases/tag/pc-v2.3.1",
    "assets": [
      {
        "name": "graball-windows-amd64.zip",
        "size": 3333333,
        "digest": "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
        "browser_download_url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.3.1/graball-windows-amd64.zip"
      }
    ]
  }
]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.json" {
			http.Error(w, "upstream error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releasesJSON))
	}))
	defer srv.Close()

	u := NewUpdater()
	u.manifestURL = srv.URL + "/latest.json"
	u.releasesURL = srv.URL + "/releases"
	u.latestRedirectURL = ""
	u.currentVersion = "2.2.9"
	u.goos = "windows"
	u.goarch = "amd64"

	info, err := u.CheckForUpdate(context.Background(), true)
	if err != nil {
		t.Fatalf("fallback CheckForUpdate failed: %v", err)
	}
	if info.LatestVersion != "2.3.1" || info.Tag != "pc-v2.3.1" {
		t.Fatalf("expected highest stable pc-v2.3.1 release, got %+v", info)
	}
	if !info.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=true")
	}
	if info.SHA256 != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("expected stripped sha256 digest, got %q", info.SHA256)
	}
	if info.ReleaseURL != "https://github.com/Avishka-55/graball-website/releases/tag/pc-v2.3.1" {
		t.Errorf("unexpected releaseUrl: %q", info.ReleaseURL)
	}
}

func TestCheckForUpdateDirectMainRepoAndRedirectFallback(t *testing.T) {
	mainReleasesJSON := `[
  {
    "tag_name": "v2.5.3",
    "draft": false,
    "prerelease": false,
    "html_url": "https://github.com/Aviz-66/grab-all-pc/releases/tag/v2.5.3",
    "assets": [
      {
        "name": "graball-windows-amd64.zip",
        "size": 3500000,
        "digest": "sha256:feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface",
        "browser_download_url": "https://github.com/Aviz-66/grab-all-pc/releases/download/v2.5.3/graball-windows-amd64.zip"
      }
    ]
  }
]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mainReleasesJSON))
		case "/rate-limited":
			http.Error(w, "API rate limit exceeded", http.StatusForbidden)
		case "/releases/latest":
			w.Header().Set("Location", "https://github.com/Aviz-66/grab-all-pc/releases/tag/v2.5.4")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 1. Direct Aviz-66/grab-all-pc v* release via API
	u := NewUpdater()
	u.releasesURL = srv.URL + "/releases"
	u.latestRedirectURL = srv.URL + "/releases/latest"
	u.manifestURL = ""
	u.currentVersion = "2.5.2"
	u.goos = "windows"
	u.goarch = "amd64"

	info, err := u.CheckForUpdate(context.Background(), true)
	if err != nil {
		t.Fatalf("direct main repo CheckForUpdate failed: %v", err)
	}
	if info.LatestVersion != "2.5.3" || info.Tag != "v2.5.3" || !info.UpdateAvailable {
		t.Fatalf("unexpected direct release info: %+v", info)
	}
	if info.SHA256 != "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface" {
		t.Errorf("unexpected SHA256: %q", info.SHA256)
	}

	// 2. When API is 403 rate-limited, zero-limit HEAD redirect fallback must succeed!
	u2 := NewUpdater()
	u2.releasesURL = srv.URL + "/rate-limited"
	u2.latestRedirectURL = srv.URL + "/releases/latest"
	u2.manifestURL = ""
	u2.currentVersion = "2.5.2"
	u2.goos = "windows"
	u2.goarch = "amd64"

	info2, err := u2.CheckForUpdate(context.Background(), true)
	if err != nil {
		t.Fatalf("HEAD redirect fallback CheckForUpdate failed: %v", err)
	}
	if info2.LatestVersion != "2.5.4" || info2.Tag != "v2.5.4" || !info2.UpdateAvailable {
		t.Fatalf("unexpected redirect fallback info: %+v", info2)
	}
	wantDL := "https://github.com/Aviz-66/grab-all-pc/releases/download/v2.5.4/graball-windows-amd64.zip"
	if info2.DownloadURL != wantDL {
		t.Errorf("DownloadURL = %q, want %q", info2.DownloadURL, wantDL)
	}
}

func TestApplyUpdateZipTarGzSHA256AndZipSlip(t *testing.T) {
	zipBytes := buildTestZipBytes(t, map[string]string{
		"graball.exe":             "NEW_WINDOWS_BINARY_v2.3.0",
		"extension/manifest.json": `{"version":"2.3.0"}`,
	})
	tgzBytes := buildTestTarGzBytes(t, map[string]string{
		"./graball":                 "NEW_LINUX_BINARY_v2.3.0",
		"./extension/manifest.json": `{"version":"2.3.0"}`,
	})
	slipZipBytes := buildTestZipBytes(t, map[string]string{
		"../evil.txt": "pwned",
		"graball.exe": "NEW_WINDOWS_BINARY",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/win.zip":
			_, _ = w.Write(zipBytes)
		case "/lin.tar.gz":
			_, _ = w.Write(tgzBytes)
		case "/slip.zip":
			_, _ = w.Write(slipZipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 1. Windows .zip update + bundled extension update + SHA-256 verification
	winDir := t.TempDir()
	winExe := filepath.Join(winDir, "graball.exe")
	winExtDir := filepath.Join(winDir, "extension")
	_ = os.MkdirAll(winExtDir, 0755)
	_ = os.WriteFile(winExe, []byte("OLD_WINDOWS_BINARY"), 0755)
	_ = os.WriteFile(filepath.Join(winExtDir, "manifest.json"), []byte(`{"version":"2.2.9"}`), 0644)

	uWin := NewUpdater()
	uWin.allowInsecureDownload = true
	uWin.goos = "windows"
	uWin.goarch = "amd64"

	// Bad SHA-256 must fail AND leave original binary intact
	errBadHash := uWin.applyUpdateToPath(context.Background(), &UpdateInfo{
		LatestVersion: "2.3.0",
		FileName:      "graball-windows-amd64.zip",
		DownloadURL:   srv.URL + "/win.zip",
		SHA256:        strings.Repeat("0", 64),
	}, winExe, nil)
	if errBadHash == nil || !strings.Contains(errBadHash.Error(), "SHA-256") {
		t.Fatalf("expected SHA-256 error, got: %v", errBadHash)
	}
	if cur, _ := os.ReadFile(winExe); string(cur) != "OLD_WINDOWS_BINARY" {
		t.Fatalf("original binary was modified on SHA-256 mismatch: %q", string(cur))
	}

	// Valid SHA-256 must replace executable & update extension
	if err := uWin.applyUpdateToPath(context.Background(), &UpdateInfo{
		LatestVersion: "2.3.0",
		FileName:      "graball-windows-amd64.zip",
		DownloadURL:   srv.URL + "/win.zip",
		SHA256:        sha256HexBytes(zipBytes),
	}, winExe, nil); err != nil {
		t.Fatalf("valid windows update failed: %v", err)
	}
	if cur, _ := os.ReadFile(winExe); string(cur) != "NEW_WINDOWS_BINARY_v2.3.0" {
		t.Errorf("expected NEW_WINDOWS_BINARY_v2.3.0, got %q", string(cur))
	}
	if extData, _ := os.ReadFile(filepath.Join(winExtDir, "manifest.json")); string(extData) != `{"version":"2.3.0"}` {
		t.Errorf("expected extension manifest updated, got %q", string(extData))
	}

	// cleanupOldBinaryAt removes .old and .new artifacts
	_ = os.WriteFile(winExe+".old", []byte("old"), 0644)
	_ = os.WriteFile(winExe+".new", []byte("new"), 0644)
	cleanupOldBinaryAt(winExe)
	if _, err := os.Stat(winExe + ".old"); !os.IsNotExist(err) {
		t.Errorf("expected %s.old to be removed", winExe)
	}
	if _, err := os.Stat(winExe + ".new"); !os.IsNotExist(err) {
		t.Errorf("expected %s.new to be removed", winExe)
	}

	// 2. Linux .tar.gz update
	linDir := t.TempDir()
	linExe := filepath.Join(linDir, "graball")
	linExtDir := filepath.Join(linDir, "extension")
	_ = os.MkdirAll(linExtDir, 0755)
	_ = os.WriteFile(linExe, []byte("OLD_LINUX_BINARY"), 0755)
	_ = os.WriteFile(filepath.Join(linExtDir, "manifest.json"), []byte(`{"version":"2.2.9"}`), 0644)

	uLin := NewUpdater()
	uLin.allowInsecureDownload = true
	uLin.goos = "linux"
	uLin.goarch = "amd64"

	if err := uLin.applyUpdateToPath(context.Background(), &UpdateInfo{
		LatestVersion: "2.3.0",
		FileName:      "graball-linux-amd64.tar.gz",
		DownloadURL:   srv.URL + "/lin.tar.gz",
		SHA256:        sha256HexBytes(tgzBytes),
	}, linExe, nil); err != nil {
		t.Fatalf("valid linux update failed: %v", err)
	}
	if cur, _ := os.ReadFile(linExe); string(cur) != "NEW_LINUX_BINARY_v2.3.0" {
		t.Errorf("expected NEW_LINUX_BINARY_v2.3.0, got %q", string(cur))
	}
	if extData, _ := os.ReadFile(filepath.Join(linExtDir, "manifest.json")); string(extData) != `{"version":"2.3.0"}` {
		t.Errorf("expected linux extension updated, got %q", string(extData))
	}

	// 3. Zip-Slip path traversal must be rejected
	errSlip := uWin.applyUpdateToPath(context.Background(), &UpdateInfo{
		LatestVersion: "2.3.0",
		FileName:      "graball-windows-amd64.zip",
		DownloadURL:   srv.URL + "/slip.zip",
	}, winExe, nil)
	if errSlip == nil || !strings.Contains(errSlip.Error(), "unsafe archive entry") {
		t.Errorf("expected zip-slip rejection, got: %v", errSlip)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(winDir), "evil.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("zip-slip wrote evil.txt outside target dir!")
	}
}

func TestUpdateAPIEndpointsAndActiveDownloadGuard(t *testing.T) {
	// Verify buildRestartArgs strips old --wait-pid variants cleanly
	gotArgs := buildRestartArgs([]string{"--tray", "--wait-pid", "99", "--port=17890", "-wait-pid=12"}, 4321)
	wantArgs := []string{"--tray", "--port=17890", "--wait-pid", "4321"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("buildRestartArgs = %v, want %v", gotArgs, wantArgs)
	}

	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "testtoken-update",
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
		DefaultEngine: "auto",
	}

	// Blocking download server so task remains StatusDownloading
	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10485760")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer dlServer.Close()

	key := platformDownloadKey(runtime.GOOS, runtime.GOARCH)
	if key == "" {
		key = "linux_amd64"
	}
	manifestBody := fmt.Sprintf(`{
  "android": {"version": "9.9.9", "url": "https://github.com/Avishka-55/graball-website/releases/download/android-v9.9.9/app.apk"},
  "pc": {
    "version": "2.3.0",
    "tag": "pc-v2.3.0",
    "downloads": {
      "%s": {
        "file": "graball-linux-amd64.tar.gz",
        "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "size_bytes": 1234,
        "url": "https://github.com/Avishka-55/graball-website/releases/download/pc-v2.3.0/graball-linux-amd64.tar.gz"
      }
    }
  }
}`, key)

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifestBody))
	}))
	defer metaSrv.Close()

	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	srv := NewServer(cfg, qm, NewSSEBroker())
	srv.updater.manifestURL = metaSrv.URL
	srv.updater.releasesURL = ""
	srv.updater.latestRedirectURL = ""
	srv.updater.currentVersion = "2.2.9"
	handler := srv.setupRoutes()

	// 1. Add active download and wait briefly until it is StatusDownloading
	if _, err := qm.AddTask(AddRequest{URL: dlServer.URL + "/large.iso", Engine: EngineDirect}); err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && qm.countDownloading() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if qm.countDownloading() == 0 {
		t.Fatalf("expected active downloading task")
	}

	// 2. GET /api/update/check
	reqCheck := httptest.NewRequest(http.MethodGet, "/api/update/check?force=1", nil)
	reqCheck.Header.Set("X-Daemon-Token", "testtoken-update")
	recCheck := httptest.NewRecorder()
	handler.ServeHTTP(recCheck, reqCheck)
	if recCheck.Code != http.StatusOK {
		t.Fatalf("GET /api/update/check status = %d, body = %s", recCheck.Code, recCheck.Body.String())
	}
	var checkResp UpdateInfo
	if err := json.Unmarshal(recCheck.Body.Bytes(), &checkResp); err != nil {
		t.Fatalf("decode checkResp failed: %v", err)
	}
	if checkResp.LatestVersion != "2.3.0" || !checkResp.UpdateAvailable {
		t.Errorf("unexpected checkResp: %+v", checkResp)
	}

	// 3. POST /api/update/apply must return 409 Conflict while download is active
	reqApply := httptest.NewRequest(http.MethodPost, "/api/update/apply", strings.NewReader(`{"force":false}`))
	reqApply.Header.Set("X-Daemon-Token", "testtoken-update")
	recApply := httptest.NewRecorder()
	handler.ServeHTTP(recApply, reqApply)
	if recApply.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict during active download, got %d (%s)", recApply.Code, recApply.Body.String())
	}
}
