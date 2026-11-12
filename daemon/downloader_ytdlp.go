package main

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ansiRegex     = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	percentRegex  = regexp.MustCompile(`([0-9.]+)%`)
	ytInstallMu       sync.Mutex
	ytInstallDone     chan struct{}
	ffmpegInstallMu   sync.Mutex
	ffmpegInstallDone chan struct{}
	prewarmMu         sync.Mutex
)

const graballFastEJSPluginPy = `import re
from yt_dlp.extractor.common import InfoExtractor

try:
    from yt_dlp.extractor.youtube.jsc._builtin.ejs import EJSBaseJCP
    from yt_dlp.extractor.youtube.jsc.provider import JsChallengeRequest, JsChallengeType, NChallengeInput
    from yt_dlp.extractor.youtube.jsc._director import initialize_jsc_director

    # Enable preprocessed YouTube player caching (cuts QuickJS solve time by ~11x,
    # avoiding re-downloading and re-parsing 85,000 lines of base.js on every video!)
    EJSBaseJCP._ENABLE_PREPROCESSED_PLAYER_CACHE = True
except Exception:
    EJSBaseJCP = None


class GrabAllPrewarmIE(InfoExtractor):
    IE_NAME = 'graball:prewarm'
    _VALID_URL = r'^graball:prewarm$'

    def _real_extract(self, url):
        dummy = {'id': 'prewarm', 'title': 'prewarm', 'url': 'https://www.youtube.com/', 'ext': 'mp4'}
        if EJSBaseJCP is None:
            return dummy

        webpage = self._download_webpage(
            'https://www.youtube.com/watch?v=jNQXAC9IVRw&bpctr=9999999999&has_verified=1',
            'prewarm',
            note='Checking YouTube JS player cache',
            fatal=False,
        ) or ''
        m = re.search(r'"jsUrl"\s*:\s*"(/s/player/[a-zA-Z0-9_-]+/[^"]+?base\.js)"', webpage)
        if not m:
            return dummy

        player_url = 'https://www.youtube.com' + m.group(1)
        if self._downloader.cache.load('challenge-solver', f'player:{player_url}'):
            return dummy

        yt_ie = self._downloader.get_info_extractor('Youtube')
        director = initialize_jsc_director(yt_ie)
        req = JsChallengeRequest(JsChallengeType.N, NChallengeInput(player_url, ['Zd5f3T8xQ1w9KpL2']))
        director.bulk_solve([req])
        return dummy
`

// ensureYtDlpSpeedPlugin writes GrabAll's lightweight yt-dlp plugin that caches YouTube's
// preprocessed base.js AST across runs, cutting extraction CPU time by ~11x on low-RAM PCs.
func ensureYtDlpSpeedPlugin() string {
	pluginsRoot := filepath.Join(executableDir(), "plugins")
	pluginFile := filepath.Join(pluginsRoot, "graball_speed", "yt_dlp_plugins", "extractor", "graball_fast_ejs.py")
	if existing, err := os.ReadFile(pluginFile); err == nil && string(existing) == graballFastEJSPluginPy {
		return pluginsRoot
	}
	if err := os.MkdirAll(filepath.Dir(pluginFile), 0755); err != nil {
		return ""
	}
	if err := os.WriteFile(pluginFile, []byte(graballFastEJSPluginPy), 0644); err != nil {
		return ""
	}
	return pluginsRoot
}

// hasCachedPlayerSolver checks whether cache/challenge-solver already holds at least one preprocessed player.
func hasCachedPlayerSolver(cacheDir string) bool {
	entries, err := os.ReadDir(filepath.Join(cacheDir, "challenge-solver"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "player") {
			return true
		}
	}
	return false
}

// pruneOldPlayerCaches keeps at most the 3 newest cached YouTube player ASTs (~7 MB max).
func pruneOldPlayerCaches(cacheDir string) {
	solverDir := filepath.Join(cacheDir, "challenge-solver")
	entries, err := os.ReadDir(solverDir)
	if err != nil {
		return
	}
	type fileAge struct {
		path string
		mod  time.Time
	}
	var players []fileAge
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "player") {
			continue
		}
		full := filepath.Join(solverDir, e.Name())
		if info, statErr := e.Info(); statErr == nil {
			players = append(players, fileAge{path: full, mod: info.ModTime()})
		}
	}
	if len(players) <= 3 {
		return
	}
	sort.Slice(players, func(i, j int) bool {
		return players[i].mod.After(players[j].mod)
	})
	for _, old := range players[3:] {
		_ = os.Remove(old.path)
	}
}

// PrewarmYouTubeSolverCache silently pre-builds YouTube's preprocessed base.js cache in the background
// so even the very first YouTube download skips the 30-60s cold AST parse on 1GB RAM CPUs.
func PrewarmYouTubeSolverCache(cfg *Config) {
	if !prewarmMu.TryLock() {
		return
	}
	defer prewarmMu.Unlock()

	binPath, err := resolveYtDlpBinary(cfg)
	if err != nil || isManagedSlowWindowsYtDlp(binPath) {
		return
	}

	cacheDir := filepath.Join(executableDir(), "cache")
	_ = os.MkdirAll(cacheDir, 0755)

	// Skip spawning yt-dlp if a player cache file was already verified/created within the last 6 hours
	if entries, rdErr := os.ReadDir(filepath.Join(cacheDir, "challenge-solver")); rdErr == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "player") {
				if info, iErr := e.Info(); iErr == nil && time.Since(info.ModTime()) < 6*time.Hour {
					return
				}
			}
		}
	}

	pluginsDir := ensureYtDlpSpeedPlugin()
	if pluginsDir == "" {
		return
	}

	if resolveQuickJSBinary() == "" {
		_, _ = DownloadQuickJSStandalone(filepath.Join(executableDir(), "bin"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	args := []string{
		"--force-ipv4",
		"--socket-timeout", "15",
		"--cache-dir", cacheDir,
		"--plugin-dirs", pluginsDir,
		"--simulate",
		"--skip-download",
		"--no-warnings",
		"--js-runtimes", "deno",
		"--js-runtimes", "node",
		"--js-runtimes", "bun",
	}
	if qjsPath := resolveQuickJSBinary(); qjsPath != "" {
		args = append(args, "--js-runtimes", "quickjs:"+qjsPath)
	} else {
		args = append(args, "--js-runtimes", "quickjs")
	}
	args = append(args, "graball:prewarm")

	cmd := exec.CommandContext(ctx, binPath, args...)
	configureSubprocess(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")
	_ = cmd.Run()
	pruneOldPlayerCaches(cacheDir)
}

func waitForActiveYtDlpInstall(ctx context.Context, onWait func()) error {
	ytInstallMu.Lock()
	ch := ytInstallDone
	ytInstallMu.Unlock()
	if ch == nil {
		return nil
	}
	if onWait != nil {
		onWait()
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ensureYtDlpBinary resolves yt-dlp, or automatically installs yt-dlp + QuickJS
// seamlessly on first use without failing the user's task.
func ensureYtDlpBinary(ctx context.Context, cfg *Config, onWait func(msg string)) (string, error) {
	if err := waitForActiveYtDlpInstall(ctx, func() {
		if onWait != nil {
			onWait("⚡ Waiting for yt-dlp engine setup to finish...")
		}
	}); err != nil {
		return "", err
	}

	binPath, err := resolveYtDlpBinary(cfg)
	if err == nil {
		if isManagedSlowWindowsYtDlp(binPath) {
			if onWait != nil {
				onWait("⚡ Upgrading yt-dlp engine to Fast-Start mode (1-time)...")
			}
			if upgraded, upErr := DownloadYtDlpStandalone(filepath.Dir(binPath)); upErr == nil {
				binPath = upgraded
				cfg.mu.Lock()
				cfg.YtDlpPath = upgraded
				cfg.mu.Unlock()
				_ = cfg.Save()
			}
		}
		return binPath, nil
	}

	// Auto-install yt-dlp on first use!
	if onWait != nil {
		onWait("⚡ Auto-installing yt-dlp & QuickJS engine (1-time setup)...")
	}

	binDir := filepath.Join(executableDir(), "bin")
	_ = os.MkdirAll(binDir, 0755)

	installedPath, installErr := DownloadYtDlpStandalone(binDir)
	if installErr != nil {
		return "", fmt.Errorf("failed to auto-install yt-dlp: %w. Open Tools menu to install manually", installErr)
	}

	cfg.mu.Lock()
	cfg.YtDlpPath = installedPath
	cfg.mu.Unlock()
	_ = cfg.Save()

	return installedPath, nil
}

// ensureFFmpegBinary resolves ffmpeg, or automatically installs static FFmpeg
// seamlessly when high-definition merging (1080p/4K) or MP3 transcoding is needed.
func ensureFFmpegBinary(ctx context.Context, cfg *Config, onWait func(msg string)) (string, error) {
	if ffmpegBin := resolveFFmpegBinary(cfg); ffmpegBin != "" {
		return ffmpegBin, nil
	}

	if onWait != nil {
		onWait("⚡ Auto-installing FFmpeg (1-time setup for 1080p/4K/MP3)...")
	}

	binDir := filepath.Join(executableDir(), "bin")
	_ = os.MkdirAll(binDir, 0755)

	installedPath, installErr := DownloadFFmpegStandalone(binDir)
	if installErr != nil {
		return "", fmt.Errorf("failed to auto-install FFmpeg: %w", installErr)
	}

	cfg.mu.Lock()
	cfg.FFmpegPath = installedPath
	cfg.mu.Unlock()
	_ = cfg.Save()

	return installedPath, nil
}

func cleanOutput(s string) string {
	return strings.TrimSpace(ansiRegex.ReplaceAllString(s, ""))
}

func isYouTubeURL(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	return strings.Contains(lower, "youtube.com/") ||
		strings.Contains(lower, "youtu.be/") ||
		strings.Contains(lower, "youtube-nocookie.com/")
}

// hasYouTubeAuthCookies checks whether a cookie slice actually contains signed-in
// Google/YouTube session cookies (as opposed to logged-out VISITOR_INFO1_LIVE tracking cookies).
func hasYouTubeAuthCookies(cookies []Cookie) bool {
	hasLoginInfo := false
	hasSID := false
	for _, c := range cookies {
		switch c.Name {
		case "LOGIN_INFO":
			if c.Value != "" {
				hasLoginInfo = true
			}
		case "SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID", "SID", "__Secure-1PSID", "__Secure-3PSID":
			if c.Value != "" {
				hasSID = true
			}
		}
	}
	return hasLoginInfo || hasSID
}

// executableDir returns the directory containing the running daemon binary, falling back to CWD.
func executableDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return filepath.Dir(resolved)
		}
		return filepath.Dir(exe)
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

func candidateBinDirs() []string {
	dirs := []string{}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(cwd, "bin"))
	}
	exeDir := executableDir()
	exeBin := filepath.Join(exeDir, "bin")
	if len(dirs) == 0 || dirs[0] != exeBin {
		dirs = append(dirs, exeBin)
	}
	return dirs
}

// resolveYtDlpBinary finds the yt-dlp executable path.
func resolveYtDlpBinary(cfg *Config) (string, error) {
	if cfg.YtDlpPath != "" && cfg.YtDlpPath != "yt-dlp" {
		if _, err := os.Stat(cfg.YtDlpPath); err == nil {
			return cfg.YtDlpPath, nil
		}
		if path, err := exec.LookPath(cfg.YtDlpPath); err == nil {
			return path, nil
		}
	}

	binName := "yt-dlp"
	if runtime.GOOS == "windows" {
		binName = "yt-dlp.exe"
	}

	for _, dir := range candidateBinDirs() {
		localPath := filepath.Join(dir, binName)
		if _, err := os.Stat(localPath); err == nil {
			return localPath, nil
		}
	}

	if path, err := exec.LookPath(binName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("yt-dlp executable not found. Open Settings and click 'Install / Update yt-dlp'")
}

// resolveFFmpegBinary finds ffmpeg if available.
func resolveFFmpegBinary(cfg *Config) string {
	if cfg.FFmpegPath != "" && cfg.FFmpegPath != "ffmpeg" {
		if _, err := os.Stat(cfg.FFmpegPath); err == nil {
			return cfg.FFmpegPath
		}
		if path, err := exec.LookPath(cfg.FFmpegPath); err == nil {
			return path
		}
	}

	binName := "ffmpeg"
	if runtime.GOOS == "windows" {
		binName = "ffmpeg.exe"
	}

	for _, dir := range candidateBinDirs() {
		localPath := filepath.Join(dir, binName)
		if _, err := os.Stat(localPath); err == nil {
			return localPath
		}
	}

	if path, err := exec.LookPath(binName); err == nil {
		return path
	}

	return ""
}

// resolveQuickJSBinary finds standalone QuickJS (qjs) for YouTube JS challenge decryption.
func resolveQuickJSBinary() string {
	binName := "qjs"
	if runtime.GOOS == "windows" {
		binName = "qjs.exe"
	}

	for _, dir := range candidateBinDirs() {
		localPath := filepath.Join(dir, binName)
		if _, err := os.Stat(localPath); err == nil {
			return localPath
		}
	}

	if path, err := exec.LookPath(binName); err == nil {
		return path
	}

	return ""
}

func parseOptionalInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return 0
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return int64(f)
	}
	return 0
}

type PlaylistEntry struct {
	Index int    `json:"index,omitempty"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func computePlaylistBounds(rawURL string, start, end, count int) (int, int) {
	if start < 1 {
		start = 1
	}
	if count > 0 {
		computedEnd := start + count - 1
		if end <= 0 || computedEnd < end {
			end = computedEnd
		}
	}
	if end <= 0 {
		// Cap infinite YouTube Radio/Mix (RD...) playlists at 25 by default unless specified,
		// and regular playlists at 300 to stay fast on 1GB RAM PCs.
		if u, err := url.Parse(rawURL); err == nil && strings.HasPrefix(u.Query().Get("list"), "RD") {
			end = start + 24
		} else {
			end = start + 299
		}
	}
	if end < start {
		end = start
	}
	return start, end
}

func normalizePlaylistURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := strings.ToLower(u.Host)
	if strings.Contains(host, "youtube.com") || strings.Contains(host, "youtu.be") {
		listID := u.Query().Get("list")
		if listID != "" {
			// YouTube Mix / Radio playlists (RD...) require the watch?v=...&list=RD... URL
			if strings.HasPrefix(listID, "RD") {
				vid := u.Query().Get("v")
				if vid == "" && strings.Contains(host, "youtu.be") {
					vid = strings.Trim(u.Path, "/")
				}
				if vid != "" {
					return "https://www.youtube.com/watch?v=" + url.QueryEscape(vid) + "&list=" + url.QueryEscape(listID)
				}
			}
			return "https://www.youtube.com/playlist?list=" + url.QueryEscape(listID)
		}
	}
	return rawURL
}

// ExpandYtDlpPlaylist uses yt-dlp --flat-playlist --dump-single-json to fetch playlist entries quickly.
func ExpandYtDlpPlaylist(ctx context.Context, cfg *Config, req AddRequest) (string, []PlaylistEntry, error) {
	binPath, err := ensureYtDlpBinary(ctx, cfg, nil)
	if err != nil {
		return "", nil, err
	}

	var cookieFile string
	if len(req.Cookies) > 0 {
		cookieFile = filepath.Join(os.TempDir(), fmt.Sprintf("ytdlp_pl_cookies_%d.txt", time.Now().UnixNano()))
		if err := WriteNetscapeCookiesFile(req.Cookies, cookieFile); err == nil {
			defer func() { _ = os.Remove(cookieFile) }()
		} else {
			cookieFile = ""
		}
	}

	targetURL := normalizePlaylistURL(req.URL)
	title, entries, err := runFlatPlaylist(ctx, binPath, targetURL, cookieFile, req)
	if err != nil && ctx.Err() == nil && cookieFile != "" && isYouTubeURL(targetURL) {
		// Retry without cookies if YouTube rejected cookie session
		title, entries, err = runFlatPlaylist(ctx, binPath, targetURL, "", req)
	}
	return title, entries, err
}

func runFlatPlaylist(ctx context.Context, binPath, targetURL, cookieFile string, req AddRequest) (string, []PlaylistEntry, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	startIdx, endIdx := computePlaylistBounds(req.URL, req.PlaylistStart, req.PlaylistEnd, req.PlaylistCount)

	cacheDir := filepath.Join(executableDir(), "cache")
	args := []string{
		"--force-ipv4",
		"--socket-timeout", "15",
		"--cache-dir", cacheDir,
		"--yes-playlist",
		"--flat-playlist",
		"--dump-single-json",
		"--no-warnings",
		"--playlist-start", strconv.Itoa(startIdx),
		"--playlist-end", strconv.Itoa(endIdx),
		"--js-runtimes", "deno",
		"--js-runtimes", "node",
		"--js-runtimes", "bun",
	}
	if qjsPath := resolveQuickJSBinary(); qjsPath != "" {
		args = append(args, "--js-runtimes", "quickjs:"+qjsPath)
	} else {
		args = append(args, "--js-runtimes", "quickjs")
	}

	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}
	if !isYouTubeURL(targetURL) {
		if req.UserAgent != "" {
			args = append(args, "--user-agent", req.UserAgent)
		}
		if req.Referer != "" {
			args = append(args, "--referer", req.Referer)
		}
	}
	args = append(args, targetURL)

	cmd := exec.CommandContext(scanCtx, binPath, args...)
	configureSubprocess(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")

	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("failed to inspect playlist: %w", err)
	}

	var parsed struct {
		Type    string `json:"_type"`
		Title   string `json:"title"`
		Entries []struct {
			ID            string `json:"id"`
			URL           string `json:"url"`
			WebpageURL    string `json:"webpage_url"`
			Title         string `json:"title"`
			PlaylistIndex int    `json:"playlist_index"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", nil, fmt.Errorf("invalid playlist metadata: %w", err)
	}

	if len(parsed.Entries) == 0 {
		return parsed.Title, nil, nil
	}

	entries := make([]PlaylistEntry, 0, len(parsed.Entries))
	for i, e := range parsed.Entries {
		// Skip deleted/private placeholder entries
		if e.Title == "[Deleted video]" || e.Title == "[Private video]" {
			continue
		}
		entryURL := strings.TrimSpace(e.WebpageURL)
		if entryURL == "" {
			entryURL = strings.TrimSpace(e.URL)
		}
		if !strings.HasPrefix(entryURL, "http://") && !strings.HasPrefix(entryURL, "https://") {
			if e.ID != "" && isYouTubeURL(targetURL) {
				entryURL = "https://www.youtube.com/watch?v=" + e.ID
			} else if entryURL != "" && isYouTubeURL(targetURL) {
				entryURL = "https://www.youtube.com/watch?v=" + entryURL
			}
		}
		if entryURL == "" {
			continue
		}
		idx := e.PlaylistIndex
		if idx <= 0 {
			idx = startIdx + i
		}
		entries = append(entries, PlaylistEntry{
			Index: idx,
			URL:   entryURL,
			Title: strings.TrimSpace(e.Title),
		})
	}

	plTitle := strings.TrimSpace(parsed.Title)
	if plTitle == "" {
		plTitle = req.Title
	}
	if plTitle == "" {
		plTitle = "Playlist"
	}

	return plTitle, entries, nil
}

func runYtDlpDownload(ctx context.Context, qm *QueueManager, task *DownloadTask) error {
	binPath, err := ensureYtDlpBinary(ctx, qm.config, func(msg string) {
		task.mu.Lock()
		if task.Status == StatusDownloading {
			task.Speed = msg
		}
		task.mu.Unlock()
		qm.BroadcastUpdate()
	})
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	downloadDir := resolveTaskDownloadDir(qm.config.GetDownloadDir(), task.SubDir)
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	// Ensure QuickJS is present for YouTube JS challenge decryption even if user has a global yt-dlp
	if isYouTubeURL(task.URL) && resolveQuickJSBinary() == "" {
		_, _ = DownloadQuickJSStandalone(filepath.Join(executableDir(), "bin"))
	}

	// Write Netscape temp cookie file if cookies were provided.
	// Note: For YouTube, only pass cookies if they contain real login cookies (passing logged-out
	// VISITOR_INFO1_LIVE cookies forces yt-dlp off non-cookie clients and triggers bot checks).
	useCookies := len(task.Cookies) > 0 && (!isYouTubeURL(task.URL) || hasYouTubeAuthCookies(task.Cookies))
	var cookieFile string
	if useCookies {
		cookieFile = filepath.Join(os.TempDir(), fmt.Sprintf("ytdlp_cookies_%s.txt", task.ID))
		if err := WriteNetscapeCookiesFile(task.Cookies, cookieFile); err == nil {
			defer func() {
				_ = os.Remove(cookieFile)
			}()
		} else {
			cookieFile = ""
		}
	}

	err = executeYtDlpAttempt(ctx, qm, task, binPath, downloadDir, cookieFile)
	if err != nil && ctx.Err() == nil && cookieFile != "" && isYouTubeURL(task.URL) {
		errMsg := err.Error()
		// Retry without cookies ONLY if YouTube served SABR-only / empty formats to web cookies
		// (do NOT retry without cookies on "not a bot", which would just double startup wait time)
		if strings.Contains(errMsg, "Requested format is not available") ||
			strings.Contains(errMsg, "Only images are available") {
			if retryErr := executeYtDlpAttempt(ctx, qm, task, binPath, downloadDir, ""); retryErr == nil {
				return nil
			}
		}
	}

	return err
}

func executeYtDlpAttempt(ctx context.Context, qm *QueueManager, task *DownloadTask, binPath, downloadDir, cookieFile string) error {
	cacheDir := filepath.Join(executableDir(), "cache")
	_ = os.MkdirAll(cacheDir, 0755)
	defer pruneOldPlayerCaches(cacheDir)

	speedMode := qm.ResolveTaskSpeedMode(task)
	isTurbo := speedMode == SpeedModeTurbo
	concFrags := "1"
	if isTurbo {
		concFrags = "6"
	}

	args := []string{
		"--force-ipv4",
		"--socket-timeout", "15",
		"--cache-dir", cacheDir,
		"--newline",
		"--progress",
		"--progress-delta", "0.15",
		"--no-warnings",
		"--no-playlist",
		// Fields: pct|speed|eta|downloaded|total(or estimate)|fragIdx|fragCnt|exactStreamSize|formatID|filename
		// exactStreamSize comes from YouTube's real contentLength (info.filesize), so the shown size never
		// "guesses" from fragment averages in Turbo (dashy) mode. Filename is kept LAST (may contain '|').
		"--progress-template", "download:DOWNLOAD_INFO:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s|%(progress.downloaded_bytes)s|%(progress.total_bytes,progress.total_bytes_estimate)s|%(progress.fragment_index)s|%(progress.fragment_count)s|%(info.filesize,info.filesize_approx)s|%(info.format_id)s|%(progress.filename)s",
		"--progress-template", "postprocess:POSTPROCESS_INFO:%(progress.status)s|%(progress.postprocessor)s",
		"--paths", fmt.Sprintf("home:%s", downloadDir),
		"--concurrent-fragments", concFrags,
		"--buffer-size", "64K",
		"--retries", "3",
		"--fragment-retries", "3",
	}

	ytDashySort := ""
	if isTurbo && isYouTubeURL(task.URL) {
		args = append(args, "--extractor-args", "youtube:formats=dashy,duplicate")
		ytDashySort = ",proto:http_dash_segments"
	}

	if pluginsDir := ensureYtDlpSpeedPlugin(); pluginsDir != "" {
		args = append(args, "--plugin-dirs", pluginsDir)
	}

	// Enable all supported JS runtimes for YouTube EJS signature/n-challenge solving
	args = append(args, "--js-runtimes", "deno", "--js-runtimes", "node", "--js-runtimes", "bun")
	if qjsPath := resolveQuickJSBinary(); qjsPath != "" {
		args = append(args, "--js-runtimes", "quickjs:"+qjsPath)
	} else {
		args = append(args, "--js-runtimes", "quickjs")
	}

	// Format & Quality selection
	quality := strings.ToLower(strings.TrimSpace(task.Quality))
	if quality == "" {
		quality = "best"
	}

	needsFFmpeg := quality == "2160" || quality == "4k" || quality == "1080" || quality == "audio-mp3"
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

	if strings.HasPrefix(quality, "audio") {
		if ytDashySort != "" {
			args = append(args, "-S", "proto:http_dash_segments")
		}
		switch quality {
		case "audio-m4a":
			args = append(args, "-f", "ba[ext=m4a]/ba/b")
			if hasFFmpeg {
				args = append(args, "--extract-audio", "--audio-format", "m4a")
			}
		case "audio-opus":
			args = append(args, "-f", "ba[acodec*=opus]/ba[ext=webm]/ba/b")
			if hasFFmpeg {
				args = append(args, "--extract-audio", "--audio-format", "opus")
			}
		case "audio-mp3":
			args = append(args, "-f", "ba/b")
			if hasFFmpeg {
				args = append(args, "--extract-audio", "--audio-format", "mp3", "--audio-quality", "0", "--postprocessor-args", "ExtractAudio:-threads 2")
			}
		default: // "audio-best" or "audio"
			args = append(args, "-f", "ba[ext=m4a]/ba/b")
			if hasFFmpeg {
				args = append(args, "--extract-audio", "--audio-format", "mp3", "--audio-quality", "0", "--postprocessor-args", "ExtractAudio:-threads 2")
			}
		}
	} else {
		// Video mode: use -f with -S (--format-sort) so vertical Shorts & missing exact
		// heights never trigger "Requested format is not available"
		if hasFFmpeg {
			args = append(args, "-f", "bv*+ba/b", "--merge-output-format", "mp4")
		} else {
			args = append(args, "-f", "b/bv*+ba/bv*")
		}

		switch quality {
		case "2160", "4k":
			args = append(args, "-S", "res:2160,ext:mp4:m4a"+ytDashySort)
		case "1080":
			args = append(args, "-S", "res:1080,ext:mp4:m4a"+ytDashySort)
		case "720":
			args = append(args, "-S", "res:720,ext:mp4:m4a"+ytDashySort)
		case "480":
			args = append(args, "-S", "res:480,ext:mp4:m4a"+ytDashySort)
		case "360":
			args = append(args, "-S", "res:360,ext:mp4:m4a"+ytDashySort)
		default: // "best"
			args = append(args, "-S", "ext:mp4:m4a"+ytDashySort)
		}
	}

	task.mu.Lock()
	origFileName := task.FileName
	task.mu.Unlock()

	// Force Windows-safe characters and dynamically fit %(title)s below Windows MAX_PATH (260)
	args = append(args, "--windows-filenames", "-o", fitYtDlpOutputTemplate(downloadDir, origFileName))

	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}

	// Do NOT override User-Agent / Referer on YouTube: yt-dlp manages exact client UAs
	// internally for Android/iOS/TV innertube APIs, and desktop UA breaks them.
	if !isYouTubeURL(task.URL) {
		if task.UserAgent != "" {
			args = append(args, "--user-agent", task.UserAgent)
		}
		if task.Referer != "" {
			args = append(args, "--referer", task.Referer)
		}
	}

	if hasFFmpeg {
		args = append(args, "--ffmpeg-location", ffmpegBin)
	}

	// Write the exact combined (video + audio) size to a tiny side file right before the download
	// starts. --print-to-file (unlike --print) does NOT force quiet/simulate mode.
	sizeFile := filepath.Join(os.TempDir(), fmt.Sprintf("graball_size_%s.txt", task.ID))
	_ = os.Remove(sizeFile)
	defer os.Remove(sizeFile)
	args = append(args, "--print-to-file", "before_dl:%(filesize,filesize_approx)s", sizeFile)

	args = append(args, task.URL)

	sizes := &ytSizeTracker{}

	cmd := exec.CommandContext(ctx, binPath, args...)
	configureSubprocess(cmd)
	// Critical on Windows: force Python inside yt-dlp.exe to flush stdout on every newline
	// instead of 4KB-8KB block-buffering when piped!
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	task.mu.Lock()
	if task.Status == StatusDownloading && (task.Speed == "" || strings.Contains(task.Speed, "Upgrading")) {
		task.Speed = "Starting yt-dlp..."
	}
	task.mu.Unlock()
	qm.BroadcastUpdate()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch yt-dlp: %w", err)
	}

	// Ensure pipes always unblock promptly on Windows if Pause/Cancel kills a subprocess tree
	stopPipeClose := context.AfterFunc(ctx, func() {
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
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if ctx.Err() != nil {
				break
			}
			text := cleanOutput(scanner.Text())
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

	scanner := bufio.NewScanner(stdout)
	var lastUpdate time.Time

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := cleanOutput(scanner.Text())

		if strings.HasPrefix(line, "DOWNLOAD_INFO:") {
			payload := strings.TrimPrefix(line, "DOWNLOAD_INFO:")
			parts := strings.Split(payload, "|")
			if len(parts) >= 7 {
				percentStr := strings.TrimSpace(parts[0])
				speedStr := strings.TrimSpace(parts[1])
				etaStr := strings.TrimSpace(parts[2])
				dlBytes := parseOptionalInt64(parts[3])
				totBytes := parseOptionalInt64(parts[4])
				fragIdx := parseOptionalInt64(parts[5])
				fragCnt := parseOptionalInt64(parts[6])

				var (
					filename   string
					streamSize int64
					formatID   string
				)
				if len(parts) >= 10 {
					streamSize = parseOptionalInt64(parts[7])
					formatID = strings.TrimSpace(parts[8])
					filename = strings.TrimSpace(strings.Join(parts[9:], "|"))
				} else if len(parts) >= 8 {
					filename = strings.TrimSpace(parts[7])
				}

				if !sizes.combinedKnown() {
					sizes.loadCombinedFromFile(sizeFile)
				}
				streamKey := formatID
				if streamKey == "" || streamKey == "NA" {
					streamKey = filename
				}
				aggDownloaded, aggTotal := sizes.update(streamKey, dlBytes, streamSize, totBytes)

				task.mu.Lock()
				if task.Status != StatusDownloading {
					task.mu.Unlock()
					break
				}
				prevProgress := task.Progress
				if aggDownloaded > 0 {
					task.DownloadedBytes = aggDownloaded
				}
				if aggTotal > 0 {
					task.TotalBytes = aggTotal
				}

				var pct float64 = -1
				if aggTotal > 0 && aggDownloaded > 0 {
					// Exact overall progress across video + audio streams
					pct = float64(aggDownloaded) / float64(aggTotal) * 100.0
				}
				if pct < 0 {
					if match := percentRegex.FindStringSubmatch(percentStr); len(match) > 1 {
						if p, err := strconv.ParseFloat(match[1], 64); err == nil {
							pct = p
						}
					}
				}
				if pct < 0 && fragCnt > 0 && fragIdx > 0 {
					pct = float64(fragIdx) / float64(fragCnt) * 100.0
				}
				if pct >= 0 {
					if pct > 100.0 {
						pct = 100.0
					}
					task.Progress = pct
				}

				if speedStr != "NA" && speedStr != "Unknown B/s" && speedStr != "" {
					if !isTurbo {
						task.Speed = speedStr + " (Live)"
					} else if fragCnt > 1 {
						task.Speed = speedStr + " (6x)"
					} else {
						task.Speed = speedStr
					}
				}
				if etaStr != "NA" && etaStr != "Unknown" && etaStr != "" {
					task.ETA = etaStr
				}
				if filename != "" && filename != "NA" {
					cleanFile := filename
					for strings.HasSuffix(strings.ToLower(cleanFile), ".part") {
						cleanFile = cleanFile[:len(cleanFile)-5]
					}
					ext := filepath.Ext(cleanFile)
					if isKnownYtMediaExt(ext) {
						stem := strings.TrimSuffix(cleanFile, ext)
						stem = ytFormatCodeExtRe.ReplaceAllString(stem, "")
						cleanFile = stem + ext
					}
					task.FileName = filepath.Base(cleanFile)
					task.SavePath = cleanFile
					cleanTitle := strings.TrimSuffix(task.FileName, filepath.Ext(task.FileName))
					if task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://") {
						task.Title = cleanTitle
					}
				}
				newProgress := task.Progress
				task.mu.Unlock()

				now := time.Now()
				// Broadcast every 150ms, OR immediately when leaving 0%
				if now.Sub(lastUpdate) >= 150*time.Millisecond || (prevProgress == 0 && newProgress > 0) {
					qm.BroadcastUpdate()
					lastUpdate = now
				}
			}
		} else if strings.HasPrefix(line, "POSTPROCESS_INFO:") {
			payload := strings.TrimPrefix(line, "POSTPROCESS_INFO:")
			parts := strings.Split(payload, "|")
			if len(parts) >= 1 && strings.TrimSpace(parts[0]) == "started" {
				task.mu.Lock()
				stillActive := task.Status == StatusDownloading
				if stillActive {
					if task.Progress < 99.0 {
						task.Progress = 99.0
					}
					task.Speed = "Merging / Converting..."
					task.ETA = ""
				}
				task.mu.Unlock()
				if stillActive {
					qm.BroadcastUpdate()
				}
			}
		} else if strings.Contains(line, "[download] Destination:") {
			dest := strings.TrimSpace(strings.TrimPrefix(line, "[download] Destination:"))
			task.mu.Lock()
			stillActive := task.Status == StatusDownloading
			if stillActive {
				task.SavePath = dest
				task.FileName = filepath.Base(dest)
				if task.Title == "" || task.Title == task.URL {
					task.Title = task.FileName
				}
			}
			task.mu.Unlock()
			if stillActive {
				qm.BroadcastUpdate()
			}
		} else if strings.Contains(line, "[ExtractAudio] Destination:") {
			dest := strings.TrimSpace(strings.TrimPrefix(line, "[ExtractAudio] Destination:"))
			task.mu.Lock()
			stillActive := task.Status == StatusDownloading
			if stillActive {
				task.SavePath = dest
				task.FileName = filepath.Base(dest)
				task.Speed = "Extracting Audio..."
			}
			task.mu.Unlock()
			if stillActive {
				qm.BroadcastUpdate()
			}
		} else if strings.Contains(line, "[Merger] Merging formats into") {
			parts := strings.Split(line, `"`)
			if len(parts) >= 2 {
				mergedFile := parts[1]
				task.mu.Lock()
				stillActive := task.Status == StatusDownloading
				if stillActive {
					task.SavePath = mergedFile
					task.FileName = filepath.Base(mergedFile)
					task.Speed = "Merging Video + Audio..."
				}
				task.mu.Unlock()
				if stillActive {
					qm.BroadcastUpdate()
				}
			}
		} else if strings.HasPrefix(line, "[") && strings.Contains(line, "] ") {
			// Show granular live extraction phase before bytes start flowing
			phase := "Extracting video stream..."
			switch {
			case strings.Contains(line, "Downloading webpage"):
				phase = "Fetching video page..."
			case strings.Contains(line, "Downloading player"):
				phase = "Caching YT player (1-time)..."
			case strings.Contains(line, "Solving JS challenges"):
				if hasCachedPlayerSolver(cacheDir) {
					phase = "Decrypting stream URL..."
				} else {
					phase = "Caching YT JS solver (1-time)..."
				}
			case strings.Contains(line, "player API JSON"):
				phase = "Fetching stream formats..."
			}
			task.mu.Lock()
			stillActive := task.Status == StatusDownloading
			if stillActive && task.Progress == 0 {
				task.Speed = phase
			}
			task.mu.Unlock()
			if stillActive {
				qm.BroadcastUpdate()
			}
		}
	}

	wg.Wait()
	cmdErr := cmd.Wait()
	if ctx.Err() != nil {
		task.mu.Lock()
		task.FileName = origFileName
		task.mu.Unlock()
		return ctx.Err()
	}

	if cmdErr != nil {
		task.mu.Lock()
		task.FileName = origFileName
		task.mu.Unlock()
		stderrMu.Lock()
		detail := strings.Join(stderrLines, "; ")
		stderrMu.Unlock()
		if detail != "" {
			return fmt.Errorf("yt-dlp failed: %s (%w)", detail, cmdErr)
		}
		return fmt.Errorf("yt-dlp failed: %w", cmdErr)
	}

	// Lock in the true final size from disk (after merge / audio conversion)
	task.mu.Lock()
	finalPath := task.SavePath
	task.mu.Unlock()
	cand := finalPath
	if cand != "" && !filepath.IsAbs(cand) {
		cand = filepath.Join(downloadDir, cand)
	}

	if resolved := resolveExistingMediaFile(downloadDir, cand, task.FileName, task.Title); resolved != "" {
		if fi, statErr := os.Stat(resolved); statErr == nil && !fi.IsDir() && fi.Size() > 0 {
			task.mu.Lock()
			task.SavePath = resolved
			task.FileName = filepath.Base(resolved)
			task.TotalBytes = fi.Size()
			task.DownloadedBytes = fi.Size()
			cleanTitle := strings.TrimSuffix(task.FileName, filepath.Ext(task.FileName))
			if task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://") {
				task.Title = cleanTitle
			}
			task.mu.Unlock()
			qm.BroadcastUpdate()
		}
	}

	// Purge any lingering temporary/partial files or DASH fragments left by yt-dlp
	qm.cleanTaskArtifacts(task, false)

	return nil
}

// ytSizeTracker converts yt-dlp's per-stream progress into one stable overall size.
// In Turbo (dashy fragment) mode yt-dlp only reports a fluctuating total_bytes_estimate,
// so we prefer YouTube's exact per-format filesize and the exact combined video+audio size.
type ytSizeTracker struct {
	combined       int64 // exact video+audio total from --print-to-file (0 = unknown)
	combinedTries  int
	completedBytes int64 // bytes of streams already finished (e.g. video before audio)
	curKey         string
	curSize        int64
	curDownloaded  int64
	lastTotal      int64
}

func (s *ytSizeTracker) combinedKnown() bool {
	return s.combined > 0 || s.combinedTries >= 40
}

func (s *ytSizeTracker) loadCombinedFromFile(path string) {
	s.combinedTries++
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v := parseOptionalInt64(strings.TrimSpace(line)); v > 0 {
			s.combined = v
			return
		}
	}
}

// update records a progress sample and returns (overallDownloaded, overallTotal).
func (s *ytSizeTracker) update(key string, downloaded, exactStreamSize, reportedTotal int64) (int64, int64) {
	if key != "" && s.curKey != "" && key != s.curKey {
		// Moved on to the next stream (e.g. video finished, audio started)
		done := s.curSize
		if s.curDownloaded > done {
			done = s.curDownloaded
		}
		s.completedBytes += done
		s.curSize = 0
		s.curDownloaded = 0
	}
	if key != "" {
		s.curKey = key
	}
	if downloaded > 0 {
		s.curDownloaded = downloaded
	}
	if exactStreamSize > 0 {
		s.curSize = exactStreamSize
	} else if reportedTotal > 0 {
		// No exact size known for this stream; fall back to yt-dlp's own figure
		s.curSize = reportedTotal
	}

	overallDownloaded := s.completedBytes + s.curDownloaded
	overallTotal := int64(0)
	if s.curSize > 0 {
		overallTotal = s.completedBytes + s.curSize
	}
	if s.combined > 0 && s.combined >= overallTotal {
		overallTotal = s.combined
	}
	if overallTotal > 0 && overallTotal < overallDownloaded {
		overallTotal = overallDownloaded
	}
	if overallTotal > 0 {
		s.lastTotal = overallTotal
	}
	return overallDownloaded, s.lastTotal
}

func downloadExecutableToDir(downloadURL, targetPath string, timeout time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}

	tempPath := targetPath + ".download"
	client := &http.Client{Timeout: timeout}

	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed from %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(tempPath)
		return err
	}
	_ = out.Close()

	_ = os.Chmod(tempPath, 0755)
	_ = os.Remove(targetPath) // Needed on Windows before rename if target exists

	if err := os.Rename(tempPath, targetPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func downloadAndExtractZip(downloadURL, destDir string, timeout time.Duration) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	tmpZip := filepath.Join(os.TempDir(), fmt.Sprintf("graball_pkg_%d.zip", time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmpZip) }()

	if err := downloadExecutableToDir(downloadURL, tmpZip, timeout); err != nil {
		return err
	}

	zr, err := zip.OpenReader(tmpZip)
	if err != nil {
		return fmt.Errorf("failed to open zip archive: %w", err)
	}
	defer zr.Close()

	// Clean previous _internal directory so old Python DLLs don't conflict across updates
	_ = os.RemoveAll(filepath.Join(destDir, "_internal"))

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for _, f := range zr.File {
		relName := filepath.FromSlash(f.Name)
		outPath := filepath.Join(destDir, relName)
		cleanOut := filepath.Clean(outPath)
		if cleanOut != filepath.Clean(destDir) && !strings.HasPrefix(cleanOut, cleanDest) {
			continue // Zip-slip protection
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(cleanOut, 0755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(cleanOut), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		tmpOut := cleanOut + ".tmp"
		wf, err := os.OpenFile(tmpOut, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, copyErr := io.Copy(wf, rc)
		_ = wf.Close()
		_ = rc.Close()
		if copyErr != nil {
			_ = os.Remove(tmpOut)
			return copyErr
		}
		_ = os.Remove(cleanOut)
		if err := os.Rename(tmpOut, cleanOut); err != nil {
			_ = os.Remove(tmpOut)
			return err
		}
	}
	return nil
}

// isFastUnpackedYtDlp checks on Windows whether yt-dlp is using the instant-start onedir
// layout (yt-dlp_win.zip with _internal/) rather than the slow PyInstaller single-file exe.
func isFastUnpackedYtDlp(binPath string) bool {
	if runtime.GOOS != "windows" || binPath == "" {
		return true
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(binPath), "_internal"))
	return err == nil && info.IsDir()
}

// isManagedSlowWindowsYtDlp returns true if binPath is GrabAll's own bin/yt-dlp.exe
// and is still the old slow single-file PyInstaller binary (without _internal/).
func isManagedSlowWindowsYtDlp(binPath string) bool {
	if runtime.GOOS != "windows" || binPath == "" || isFastUnpackedYtDlp(binPath) {
		return false
	}
	binDir := filepath.Clean(filepath.Dir(binPath))
	for _, cand := range candidateBinDirs() {
		if strings.EqualFold(binDir, filepath.Clean(cand)) {
			return true
		}
	}
	return false
}

// DownloadYtDlpStandalone fetches the official yt-dlp package AND QuickJS runtime into destDir.
// On Windows, it installs yt-dlp_win.zip (pre-extracted onedir build) so yt-dlp starts in ~1s
// instead of taking 60-90s unpacking 146 DLLs to %TEMP% and triggering Windows Defender on every video!
func DownloadYtDlpStandalone(destDir string) (string, error) {
	ytInstallMu.Lock()
	for ytInstallDone != nil {
		ch := ytInstallDone
		ytInstallMu.Unlock()
		<-ch
		ytInstallMu.Lock()
	}
	done := make(chan struct{})
	ytInstallDone = done
	ytInstallMu.Unlock()
	defer func() {
		ytInstallMu.Lock()
		ytInstallDone = nil
		close(done)
		ytInstallMu.Unlock()
	}()

	if runtime.GOOS == "windows" {
		asset := "yt-dlp_win.zip"
		if runtime.GOARCH == "386" {
			asset = "yt-dlp_win_x86.zip"
		} else if runtime.GOARCH == "arm64" {
			asset = "yt-dlp_win_arm64.zip"
		}
		zipURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/" + asset
		if err := downloadAndExtractZip(zipURL, destDir, 4*time.Minute); err != nil {
			return "", err
		}
		targetPath := filepath.Join(destDir, "yt-dlp.exe")
		_, _ = DownloadQuickJSStandalone(destDir)
		return targetPath, nil
	}

	binName := "yt-dlp"
	downloadURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"
	if runtime.GOOS == "darwin" {
		downloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos"
	}

	targetPath := filepath.Join(destDir, binName)
	if err := downloadExecutableToDir(downloadURL, targetPath, 3*time.Minute); err != nil {
		return "", err
	}

	_, _ = DownloadQuickJSStandalone(destDir)
	return targetPath, nil
}

// DownloadQuickJSStandalone fetches the tiny standalone QuickJS engine for yt-dlp EJS solving.
func DownloadQuickJSStandalone(destDir string) (string, error) {
	binName := "qjs"
	var asset string

	switch runtime.GOOS {
	case "windows":
		binName = "qjs.exe"
		if runtime.GOARCH == "386" {
			asset = "qjs-windows-x86.exe"
		} else {
			asset = "qjs-windows-x86_64.exe"
		}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			asset = "qjs-darwin-arm64"
		} else {
			asset = "qjs-darwin-x86_64"
		}
	default: // linux
		if runtime.GOARCH == "arm64" {
			asset = "qjs-linux-aarch64"
		} else {
			asset = "qjs-linux-x86_64"
		}
	}

	downloadURL := "https://github.com/quickjs-ng/quickjs/releases/latest/download/" + asset
	targetPath := filepath.Join(destDir, binName)
	if err := downloadExecutableToDir(downloadURL, targetPath, 2*time.Minute); err != nil {
		return "", err
	}
	return targetPath, nil
}

// DownloadFFmpegStandalone fetches a static single-file ffmpeg binary into destDir.
func DownloadFFmpegStandalone(destDir string) (string, error) {
	ffmpegInstallMu.Lock()
	for ffmpegInstallDone != nil {
		ch := ffmpegInstallDone
		ffmpegInstallMu.Unlock()
		<-ch
		ffmpegInstallMu.Lock()
	}
	done := make(chan struct{})
	ffmpegInstallDone = done
	ffmpegInstallMu.Unlock()
	defer func() {
		ffmpegInstallMu.Lock()
		ffmpegInstallDone = nil
		close(done)
		ffmpegInstallMu.Unlock()
	}()

	binName := "ffmpeg"
	var asset string

	switch runtime.GOOS {
	case "windows":
		binName = "ffmpeg.exe"
		asset = "ffmpeg-win32-x64"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			asset = "ffmpeg-darwin-arm64"
		} else {
			asset = "ffmpeg-darwin-x64"
		}
	default: // linux
		if runtime.GOARCH == "arm64" {
			asset = "ffmpeg-linux-arm64"
		} else {
			asset = "ffmpeg-linux-x64"
		}
	}

	downloadURL := "https://github.com/eugeneware/ffmpeg-static/releases/latest/download/" + asset
	targetPath := filepath.Join(destDir, binName)
	if err := downloadExecutableToDir(downloadURL, targetPath, 5*time.Minute); err != nil {
		return "", err
	}
	return targetPath, nil
}
