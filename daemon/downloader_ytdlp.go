package main

import (
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
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ansiRegex    = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	percentRegex = regexp.MustCompile(`([0-9.]+)%`)
)

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
	URL   string
	Title string
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
			return "https://www.youtube.com/playlist?list=" + url.QueryEscape(listID)
		}
	}
	return rawURL
}

// ExpandYtDlpPlaylist uses yt-dlp --flat-playlist --dump-single-json to fetch playlist entries quickly.
func ExpandYtDlpPlaylist(ctx context.Context, cfg *Config, req AddRequest) (string, []PlaylistEntry, error) {
	binPath, err := resolveYtDlpBinary(cfg)
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

	args := []string{
		"--flat-playlist",
		"--dump-single-json",
		"--no-warnings",
		"--playlist-end", "200",
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
			ID         string `json:"id"`
			URL        string `json:"url"`
			WebpageURL string `json:"webpage_url"`
			Title      string `json:"title"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", nil, fmt.Errorf("invalid playlist metadata: %w", err)
	}

	if len(parsed.Entries) == 0 {
		return parsed.Title, nil, nil
	}

	entries := make([]PlaylistEntry, 0, len(parsed.Entries))
	for _, e := range parsed.Entries {
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
		entries = append(entries, PlaylistEntry{
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
	binPath, err := resolveYtDlpBinary(qm.config)
	if err != nil {
		return err
	}

	downloadDir := qm.config.GetDownloadDir()
	if task.SubDir != "" {
		downloadDir = filepath.Join(downloadDir, sanitizeFileName(task.SubDir))
	}
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
		// Retry without cookies if YouTube served SABR-only formats or flagged rotated session cookies
		if strings.Contains(errMsg, "Requested format is not available") ||
			strings.Contains(errMsg, "Only images are available") ||
			strings.Contains(errMsg, "no longer valid") ||
			strings.Contains(errMsg, "not a bot") {
			if retryErr := executeYtDlpAttempt(ctx, qm, task, binPath, downloadDir, ""); retryErr == nil {
				return nil
			}
		}
	}

	return err
}

func executeYtDlpAttempt(ctx context.Context, qm *QueueManager, task *DownloadTask, binPath, downloadDir, cookieFile string) error {
	args := []string{
		"--newline",
		"--progress",
		"--progress-delta", "0.15",
		"--no-warnings",
		"--no-playlist",
		"--progress-template", "download:DOWNLOAD_INFO:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s|%(progress.downloaded_bytes)s|%(progress.total_bytes,progress.total_bytes_estimate)s|%(progress.fragment_index)s|%(progress.fragment_count)s|%(progress.filename)s",
		"--progress-template", "postprocess:POSTPROCESS_INFO:%(progress.status)s|%(progress.postprocessor)s",
		"--paths", fmt.Sprintf("home:%s", downloadDir),
		"--concurrent-fragments", "2",
		"--retries", "3",
		"--fragment-retries", "3",
	}

	// Enable all supported JS runtimes for YouTube EJS signature/n-challenge solving
	args = append(args, "--js-runtimes", "deno", "--js-runtimes", "node", "--js-runtimes", "bun")
	if qjsPath := resolveQuickJSBinary(); qjsPath != "" {
		args = append(args, "--js-runtimes", "quickjs:"+qjsPath)
	} else {
		args = append(args, "--js-runtimes", "quickjs")
	}

	// Format & Quality selection
	ffmpegBin := resolveFFmpegBinary(qm.config)
	hasFFmpeg := ffmpegBin != ""

	quality := strings.ToLower(strings.TrimSpace(task.Quality))
	if quality == "" {
		quality = "best"
	}

	if strings.HasPrefix(quality, "audio") {
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
				args = append(args, "--extract-audio", "--audio-format", "mp3", "--audio-quality", "0")
			}
		default: // "audio-best" or "audio"
			args = append(args, "-f", "ba[ext=m4a]/ba/b")
			if hasFFmpeg {
				args = append(args, "--extract-audio", "--audio-format", "mp3", "--audio-quality", "0")
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
			args = append(args, "-S", "res:2160,ext:mp4:m4a")
		case "1080":
			args = append(args, "-S", "res:1080,ext:mp4:m4a")
		case "720":
			args = append(args, "-S", "res:720,ext:mp4:m4a")
		case "480":
			args = append(args, "-S", "res:480,ext:mp4:m4a")
		case "360":
			args = append(args, "-S", "res:360,ext:mp4:m4a")
		default: // "best"
			args = append(args, "-S", "ext:mp4:m4a")
		}
	}

	// Output template
	if task.FileName != "" {
		args = append(args, "-o", task.FileName)
	} else {
		args = append(args, "-o", "%(title)s [%(id)s].%(ext)s")
	}

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

	args = append(args, task.URL)

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

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch yt-dlp: %w", err)
	}

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

				var filename string
				if len(parts) >= 8 {
					filename = strings.TrimSpace(parts[7])
				}

				task.mu.Lock()
				prevProgress := task.Progress
				if dlBytes > 0 {
					task.DownloadedBytes = dlBytes
				}
				if totBytes > 0 {
					task.TotalBytes = totBytes
				}

				var pct float64 = -1
				if match := percentRegex.FindStringSubmatch(percentStr); len(match) > 1 {
					if p, err := strconv.ParseFloat(match[1], 64); err == nil {
						pct = p
					}
				}
				if pct < 0 && totBytes > 0 && dlBytes > 0 {
					pct = float64(dlBytes) / float64(totBytes) * 100.0
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
					task.Speed = speedStr
				}
				if etaStr != "NA" && etaStr != "Unknown" && etaStr != "" {
					task.ETA = etaStr
				}
				if filename != "" && filename != "NA" {
					task.FileName = filepath.Base(filename)
					task.SavePath = filename
					if task.Title == "" || task.Title == task.URL {
						task.Title = task.FileName
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
				if task.Progress < 99.0 {
					task.Progress = 99.0
				}
				task.Speed = "Merging / Converting..."
				task.ETA = ""
				task.mu.Unlock()
				qm.BroadcastUpdate()
			}
		} else if strings.Contains(line, "[download] Destination:") {
			dest := strings.TrimSpace(strings.TrimPrefix(line, "[download] Destination:"))
			task.mu.Lock()
			task.SavePath = dest
			task.FileName = filepath.Base(dest)
			if task.Title == "" || task.Title == task.URL {
				task.Title = task.FileName
			}
			task.mu.Unlock()
			qm.BroadcastUpdate()
		} else if strings.Contains(line, "[ExtractAudio] Destination:") {
			dest := strings.TrimSpace(strings.TrimPrefix(line, "[ExtractAudio] Destination:"))
			task.mu.Lock()
			task.SavePath = dest
			task.FileName = filepath.Base(dest)
			task.Speed = "Extracting Audio..."
			task.mu.Unlock()
			qm.BroadcastUpdate()
		} else if strings.Contains(line, "[Merger] Merging formats into") {
			parts := strings.Split(line, `"`)
			if len(parts) >= 2 {
				mergedFile := parts[1]
				task.mu.Lock()
				task.SavePath = mergedFile
				task.FileName = filepath.Base(mergedFile)
				task.Speed = "Merging Video + Audio..."
				task.mu.Unlock()
				qm.BroadcastUpdate()
			}
		}
	}

	wg.Wait()
	cmdErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if cmdErr != nil {
		stderrMu.Lock()
		detail := strings.Join(stderrLines, "; ")
		stderrMu.Unlock()
		if detail != "" {
			return fmt.Errorf("yt-dlp failed: %s (%w)", detail, cmdErr)
		}
		return fmt.Errorf("yt-dlp failed: %w", cmdErr)
	}

	return nil
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

// DownloadYtDlpStandalone fetches the official standalone yt-dlp binary AND QuickJS runtime into destDir.
func DownloadYtDlpStandalone(destDir string) (string, error) {
	binName := "yt-dlp"
	downloadURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"

	if runtime.GOOS == "windows" {
		binName = "yt-dlp.exe"
		downloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	} else if runtime.GOOS == "darwin" {
		downloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos"
	}

	targetPath := filepath.Join(destDir, binName)
	if err := downloadExecutableToDir(downloadURL, targetPath, 3*time.Minute); err != nil {
		return "", err
	}

	// Best-effort install of lightweight QuickJS (2.5MB) so YouTube JS decryption works out-of-the-box
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
