package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultYtDlpLatestPageURL = "https://github.com/yt-dlp/yt-dlp/releases/latest"
	defaultYtDlpLatestAPIURL  = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"
	ytDlpCheckCacheTTL        = 10 * time.Minute
	ytDlpVersionMarkerName    = "ytdlp.version"
)

var errYtDlpActiveDownloads = errors.New("active downloads are in progress; pause or finish them before updating yt-dlp")

// YtDlpUpdateInfo represents the live version & update state of the yt-dlp engine.
type YtDlpUpdateInfo struct {
	Installed       bool   `json:"installed"`
	CurrentVersion  string `json:"currentVersion,omitempty"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Updating        bool   `json:"updating"`
	Stage           string `json:"stage,omitempty"` // "", "checking", "update_available", "up_to_date", "not_installed", "downloading", "extracting", "prewarming", "updated", "error"
	StatusText      string `json:"statusText,omitempty"`
	Progress        int    `json:"progress"`
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
	TotalBytes      int64  `json:"totalBytes,omitempty"`
	BinaryPath      string `json:"binaryPath,omitempty"`
	LastCheckedAt   string `json:"lastCheckedAt,omitempty"`
	Error           string `json:"error,omitempty"`
}

// YtDlpUpdater tracks yt-dlp version status, checks GitHub for updates without burning
// API rate limits, and performs live-progress updates safely.
type YtDlpUpdater struct {
	latestPageURL string
	latestAPIURL  string
	httpClient    *http.Client
	broker        *SSEBroker

	// Overridable in tests
	installFn func(destDir string, onProgress func(stage string, downloaded, total int64)) (string, error)
	versionFn func(binPath string) string
	prewarmFn func(cfg *Config)

	mu            sync.Mutex
	state         YtDlpUpdateInfo
	checkedAt     time.Time
	onStatusHooks []func(*YtDlpUpdateInfo)
}

func NewYtDlpUpdater(broker *SSEBroker) *YtDlpUpdater {
	return &YtDlpUpdater{
		latestPageURL: defaultYtDlpLatestPageURL,
		latestAPIURL:  defaultYtDlpLatestAPIURL,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		broker:        broker,
		installFn:     DownloadYtDlpStandaloneWithProgress,
		versionFn:     readInstalledYtDlpVersion,
		prewarmFn:     PrewarmYouTubeSolverCache,
	}
}

func (u *YtDlpUpdater) AddStatusListener(fn func(*YtDlpUpdateInfo)) {
	if fn == nil {
		return
	}
	u.mu.Lock()
	u.onStatusHooks = append(u.onStatusHooks, fn)
	u.mu.Unlock()
}

func (u *YtDlpUpdater) broadcastLocked() {
	snap := u.state
	hooks := append([]func(*YtDlpUpdateInfo){}, u.onStatusHooks...)
	broker := u.broker
	go func() {
		if broker != nil {
			broker.BroadcastJSON("ytdlp_status", &snap)
		}
		for _, fn := range hooks {
			fn(&snap)
		}
	}()
}

// resolveWritableDataSubdir returns a writable subdirectory (e.g. "cache", "plugins", "bin"),
// preferring executableDir()/sub and falling back to %APPDATA%\GrabAll\sub on Windows
// (or user config dir / temp dir) when the install folder is read-only (e.g. Program Files).
func resolveWritableDataSubdir(sub string) string {
	exeDir := executableDir()
	cand := filepath.Join(exeDir, sub)
	if isDirWritable(cand) {
		return cand
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			fb := filepath.Join(appData, "GrabAll", sub)
			if isDirWritable(fb) {
				return fb
			}
		}
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		fb := filepath.Join(cfgDir, "GrabAll", sub)
		if isDirWritable(fb) {
			return fb
		}
	}
	fb := filepath.Join(os.TempDir(), "graball_"+sub)
	_ = os.MkdirAll(fb, 0755)
	return fb
}

func clearYtDlpSolverCache() {
	cacheDir := resolveWritableDataSubdir("cache")
	for _, sub := range []string{"challenge-solver", "youtube-nsig", "youtube-sigfuncs"} {
		_ = os.RemoveAll(filepath.Join(cacheDir, sub))
	}
}

func writeYtDlpVersionMarker(binPath, version string) {
	version = strings.TrimSpace(version)
	if binPath == "" || version == "" {
		return
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		return
	}
	markerPath := filepath.Join(filepath.Dir(binPath), ytDlpVersionMarkerName)
	content := fmt.Sprintf("%s|%d\n", version, fi.ModTime().Unix())
	_ = os.WriteFile(markerPath, []byte(content), 0644)
}

func readYtDlpVersionMarker(binPath string) string {
	if binPath == "" {
		return ""
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		return ""
	}
	markerPath := filepath.Join(filepath.Dir(binPath), ytDlpVersionMarkerName)
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), "|", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return ""
	}
	if ts, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil && ts == fi.ModTime().Unix() {
		return strings.TrimSpace(parts[0])
	}
	return ""
}

// readInstalledYtDlpVersion returns the installed yt-dlp version string (e.g. "2026.11.16"),
// using a tiny mtime-validated marker file to avoid spawning Python on repeated checks.
func readInstalledYtDlpVersion(binPath string) string {
	if binPath == "" {
		return ""
	}
	if cached := readYtDlpVersionMarker(binPath); cached != "" {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "--version")
	configureSubprocess(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	ver := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if ver != "" {
		writeYtDlpVersionMarker(binPath, ver)
	}
	return ver
}

func normalizeYtDlpVersion(v string) string {
	s := strings.TrimSpace(v)
	s = strings.TrimLeft(s, "vV")
	if idx := strings.IndexAny(s, " \t\r\n"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func parseTagFromLocationHeader(loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return ""
	}
	if q := strings.IndexByte(loc, '?'); q >= 0 {
		loc = loc[:q]
	}
	loc = strings.TrimRight(loc, "/")
	const marker = "/releases/tag/"
	if idx := strings.LastIndex(loc, marker); idx >= 0 {
		return normalizeYtDlpVersion(loc[idx+len(marker):])
	}
	return ""
}

func (u *YtDlpUpdater) fetchLatestVersion(ctx context.Context) (string, error) {
	var primaryErr error

	// Primary: HEAD request to /releases/latest (zero body, zero GitHub REST API rate limit!)
	if strings.TrimSpace(u.latestPageURL) != "" {
		noRedirectClient := &http.Client{
			Timeout:   u.httpClient.Timeout,
			Transport: u.httpClient.Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.latestPageURL, nil)
		if err == nil {
			req.Header.Set("User-Agent", "GrabAll-PC/"+AppVersion)
			resp, rErr := noRedirectClient.Do(req)
			if rErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				if tag := parseTagFromLocationHeader(resp.Header.Get("Location")); tag != "" {
					return tag, nil
				}
				if resp.StatusCode >= 400 {
					primaryErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, u.latestPageURL)
				} else {
					primaryErr = fmt.Errorf("no redirect tag in response from %s", u.latestPageURL)
				}
			} else {
				primaryErr = rErr
			}
		} else {
			primaryErr = err
		}
	}

	// Fallback: GitHub Releases API
	if strings.TrimSpace(u.latestAPIURL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.latestAPIURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "GrabAll-PC/"+AppVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := u.httpClient.Do(req)
		if err != nil {
			if primaryErr != nil {
				return "", fmt.Errorf("yt-dlp update check failed (%v; api: %v)", primaryErr, err)
			}
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := fmt.Errorf("HTTP %d from %s", resp.StatusCode, u.latestAPIURL)
			if primaryErr != nil {
				return "", fmt.Errorf("yt-dlp update check failed (%v; api: %v)", primaryErr, apiErr)
			}
			return "", apiErr
		}
		var payload struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
			return "", fmt.Errorf("invalid yt-dlp release metadata: %w", err)
		}
		if tag := normalizeYtDlpVersion(payload.TagName); tag != "" {
			return tag, nil
		}
	}

	if primaryErr != nil {
		return "", primaryErr
	}
	return "", fmt.Errorf("could not determine latest yt-dlp version")
}

// Status returns a zero-network snapshot of yt-dlp's installed state and last known update info.
func (u *YtDlpUpdater) Status(cfg *Config) *YtDlpUpdateInfo {
	binPath, err := resolveYtDlpBinary(cfg)
	installed := err == nil && binPath != ""

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.state.Updating {
		cp := u.state
		return &cp
	}

	u.state.Installed = installed
	u.state.BinaryPath = binPath
	if installed {
		if u.state.CurrentVersion == "" {
			if marker := readYtDlpVersionMarker(binPath); marker != "" {
				u.state.CurrentVersion = marker
			}
		}
	} else {
		u.state.CurrentVersion = ""
		u.state.UpdateAvailable = false
		if u.state.Stage == "" {
			u.state.Stage = "not_installed"
			u.state.StatusText = "Not Installed"
		}
	}

	if installed && u.state.CurrentVersion != "" && u.state.LatestVersion != "" {
		u.state.UpdateAvailable = compareSemver(u.state.LatestVersion, u.state.CurrentVersion) > 0
	}

	if u.state.StatusText == "" {
		if !installed {
			u.state.StatusText = "Not Installed"
		} else if u.state.UpdateAvailable {
			u.state.StatusText = fmt.Sprintf("Update found: %s -> %s", u.state.CurrentVersion, u.state.LatestVersion)
		} else if u.state.CurrentVersion != "" {
			u.state.StatusText = fmt.Sprintf("v%s", u.state.CurrentVersion)
		} else {
			u.state.StatusText = "Installed & Ready"
		}
	}

	cp := u.state
	return &cp
}

// CheckForUpdate compares the installed yt-dlp version against the latest GitHub release.
func (u *YtDlpUpdater) CheckForUpdate(ctx context.Context, cfg *Config, force bool) (*YtDlpUpdateInfo, error) {
	u.mu.Lock()
	if u.state.Updating {
		cp := u.state
		u.mu.Unlock()
		return &cp, nil
	}
	if !force && !u.checkedAt.IsZero() && time.Since(u.checkedAt) < ytDlpCheckCacheTTL && u.state.LatestVersion != "" {
		cp := u.state
		u.mu.Unlock()
		return &cp, nil
	}
	u.state.Stage = "checking"
	u.state.StatusText = "Checking for yt-dlp updates..."
	u.state.Error = ""
	u.broadcastLocked()
	u.mu.Unlock()

	binPath, binErr := resolveYtDlpBinary(cfg)
	installed := binErr == nil && binPath != ""
	curVer := ""
	if installed && u.versionFn != nil {
		curVer = normalizeYtDlpVersion(u.versionFn(binPath))
	}

	latestVer, err := u.fetchLatestVersion(ctx)
	if err != nil {
		u.mu.Lock()
		u.state.Installed = installed
		u.state.BinaryPath = binPath
		if curVer != "" {
			u.state.CurrentVersion = curVer
		}
		u.state.Stage = "error"
		u.state.Error = err.Error()
		u.state.StatusText = "Update check failed"
		u.broadcastLocked()
		cp := u.state
		u.mu.Unlock()
		return &cp, err
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	updateAvail := !installed || curVer == "" || compareSemver(latestVer, curVer) > 0

	u.mu.Lock()
	u.checkedAt = time.Now()
	u.state.Installed = installed
	u.state.BinaryPath = binPath
	u.state.CurrentVersion = curVer
	u.state.LatestVersion = latestVer
	u.state.UpdateAvailable = updateAvail
	u.state.Updating = false
	u.state.Progress = 0
	u.state.DownloadedBytes = 0
	u.state.TotalBytes = 0
	u.state.LastCheckedAt = nowStr
	u.state.Error = ""
	switch {
	case !installed:
		u.state.Stage = "not_installed"
		u.state.StatusText = fmt.Sprintf("Not installed (latest: %s)", latestVer)
	case updateAvail && curVer != "":
		u.state.Stage = "update_available"
		u.state.StatusText = fmt.Sprintf("Update found: %s -> %s", curVer, latestVer)
	case updateAvail:
		u.state.Stage = "update_available"
		u.state.StatusText = fmt.Sprintf("Update available: %s", latestVer)
	default:
		u.state.Stage = "up_to_date"
		u.state.StatusText = fmt.Sprintf("Up to date (%s)", curVer)
	}
	u.broadcastLocked()
	cp := u.state
	u.mu.Unlock()

	return &cp, nil
}

// Update downloads and installs the latest yt-dlp + QuickJS package with live stage/progress updates.
func (u *YtDlpUpdater) Update(ctx context.Context, cfg *Config, qm *QueueManager, force bool, onProgress func(*YtDlpUpdateInfo)) (*YtDlpUpdateInfo, error) {
	if qm != nil && qm.countDownloading() > 0 && !force {
		return nil, errYtDlpActiveDownloads
	}

	u.mu.Lock()
	if u.state.Updating {
		u.mu.Unlock()
		return nil, fmt.Errorf("yt-dlp update is already in progress")
	}
	u.state.Updating = true
	u.state.Stage = "downloading"
	u.state.Progress = 0
	u.state.DownloadedBytes = 0
	u.state.TotalBytes = 0
	u.state.Error = ""
	targetVer := u.state.LatestVersion
	if targetVer != "" {
		u.state.StatusText = fmt.Sprintf("Downloading yt-dlp %s...", targetVer)
	} else {
		u.state.StatusText = "Downloading latest yt-dlp..."
	}
	u.broadcastLocked()
	firstSnap := u.state
	u.mu.Unlock()

	if onProgress != nil {
		onProgress(&firstSnap)
	}

	// Best-effort quick version lookup if LatestVersion wasn't fetched yet
	if targetVer == "" {
		checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if v, err := u.fetchLatestVersion(checkCtx); err == nil && v != "" {
			targetVer = v
		}
		cancel()
	}

	binDir := resolveWritableDataSubdir("bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return u.finishUpdateError(fmt.Errorf("failed to create bin directory: %w", err), onProgress)
	}

	lastEmit := time.Now()
	installFn := u.installFn
	if installFn == nil {
		installFn = DownloadYtDlpStandaloneWithProgress
	}

	installedPath, err := installFn(binDir, func(stage string, downloaded, total int64) {
		now := time.Now()
		if stage == "downloading" && now.Sub(lastEmit) < 150*time.Millisecond && (total <= 0 || downloaded < total) {
			return
		}
		lastEmit = now

		pct := 0
		if stage == "downloading" && total > 0 {
			pct = int(float64(downloaded) / float64(total) * 85.0)
			if pct > 85 {
				pct = 85
			}
		} else if stage == "extracting" {
			pct = 90
		} else if stage == "quickjs" {
			pct = 95
		}

		label := "Updating yt-dlp..."
		verSuffix := ""
		if targetVer != "" {
			verSuffix = " " + targetVer
		}
		switch stage {
		case "downloading":
			if total > 0 {
				dlPct := int(float64(downloaded) / float64(total) * 100.0)
				if dlPct > 100 {
					dlPct = 100
				}
				label = fmt.Sprintf("Downloading yt-dlp%s... %d%% (%s / %s)", verSuffix, dlPct, formatBytes(downloaded), formatBytes(total))
			} else if downloaded > 0 {
				label = fmt.Sprintf("Downloading yt-dlp%s... %s", verSuffix, formatBytes(downloaded))
			} else {
				label = fmt.Sprintf("Downloading yt-dlp%s...", verSuffix)
			}
		case "extracting":
			label = fmt.Sprintf("Extracting yt-dlp%s...", verSuffix)
		case "quickjs":
			label = "Setting up QuickJS solver..."
		}

		u.mu.Lock()
		u.state.Stage = stage
		u.state.Progress = pct
		u.state.DownloadedBytes = downloaded
		u.state.TotalBytes = total
		u.state.StatusText = label
		u.broadcastLocked()
		snap := u.state
		u.mu.Unlock()
		if onProgress != nil {
			onProgress(&snap)
		}
	})
	if err != nil {
		return u.finishUpdateError(err, onProgress)
	}

	if cfg != nil {
		cfg.mu.Lock()
		cfg.YtDlpPath = installedPath
		cfg.mu.Unlock()
		_ = cfg.Save()
	}

	// Clear stale challenge-solver and preview caches so the updated binary starts fresh
	clearYtDlpSolverCache()
	clearAllPreviewCache()

	newVer := ""
	if u.versionFn != nil {
		newVer = normalizeYtDlpVersion(u.versionFn(installedPath))
	}
	if newVer == "" {
		newVer = targetVer
	}
	if newVer != "" {
		writeYtDlpVersionMarker(installedPath, newVer)
	}
	latestVer := targetVer
	if latestVer == "" || (newVer != "" && compareSemver(newVer, latestVer) > 0) {
		latestVer = newVer
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	u.mu.Lock()
	u.checkedAt = time.Now()
	u.state.Installed = true
	u.state.BinaryPath = installedPath
	u.state.CurrentVersion = newVer
	u.state.LatestVersion = latestVer
	u.state.UpdateAvailable = false
	u.state.Updating = false
	u.state.Stage = "updated"
	u.state.Progress = 100
	u.state.LastCheckedAt = nowStr
	u.state.Error = ""
	if newVer != "" {
		u.state.StatusText = fmt.Sprintf("Updated to %s", newVer)
	} else {
		u.state.StatusText = "Updated & Ready"
	}
	u.broadcastLocked()
	finalSnap := u.state
	u.mu.Unlock()

	if onProgress != nil {
		onProgress(&finalSnap)
	}
	if u.prewarmFn != nil && cfg != nil {
		go u.prewarmFn(cfg)
	}

	return &finalSnap, nil
}

func (u *YtDlpUpdater) finishUpdateError(err error, onProgress func(*YtDlpUpdateInfo)) (*YtDlpUpdateInfo, error) {
	u.mu.Lock()
	u.state.Updating = false
	u.state.Stage = "error"
	u.state.Progress = 0
	u.state.Error = err.Error()
	u.state.StatusText = "Update failed: " + err.Error()
	u.broadcastLocked()
	snap := u.state
	u.mu.Unlock()
	if onProgress != nil {
		onProgress(&snap)
	}
	return &snap, err
}
