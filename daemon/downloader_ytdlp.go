package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
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

func runYtDlpDownload(ctx context.Context, qm *QueueManager, task *DownloadTask) error {
	binPath, err := resolveYtDlpBinary(qm.config)
	if err != nil {
		return err
	}

	downloadDir := qm.config.GetDownloadDir()
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	// Write Netscape temp cookie file if cookies were provided
	var cookieFile string
	if len(task.Cookies) > 0 {
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
		// YouTube sometimes serves SABR-only / empty formats to logged-in web cookies;
		// retrying without cookies lets yt-dlp fall back to TV / mobile clients cleanly.
		if strings.Contains(errMsg, "Requested format is not available") ||
			strings.Contains(errMsg, "Only images are available") {
			return executeYtDlpAttempt(ctx, qm, task, binPath, downloadDir, "")
		}
	}

	return err
}

func executeYtDlpAttempt(ctx context.Context, qm *QueueManager, task *DownloadTask, binPath, downloadDir, cookieFile string) error {
	args := []string{
		"--newline",
		"--no-warnings",
		"--no-playlist",
		"--progress-template", "DOWNLOAD_INFO:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s|%(progress._total_bytes_str,progress._total_bytes_estimate_str)s|%(progress.filename)s",
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
	lastUpdate := time.Now()

	for scanner.Scan() {
		line := cleanOutput(scanner.Text())

		if strings.HasPrefix(line, "DOWNLOAD_INFO:") {
			payload := strings.TrimPrefix(line, "DOWNLOAD_INFO:")
			parts := strings.Split(payload, "|")
			if len(parts) >= 4 {
				percentStr := strings.TrimSpace(parts[0])
				speedStr := strings.TrimSpace(parts[1])
				etaStr := strings.TrimSpace(parts[2])

				var filename string
				if len(parts) >= 5 {
					filename = strings.TrimSpace(parts[4])
				}

				task.mu.Lock()
				if match := percentRegex.FindStringSubmatch(percentStr); len(match) > 1 {
					if p, err := strconv.ParseFloat(match[1], 64); err == nil {
						task.Progress = p
					}
				}
				if speedStr != "NA" && speedStr != "" {
					task.Speed = speedStr
				}
				if etaStr != "NA" && etaStr != "" {
					task.ETA = etaStr
				}
				if filename != "" && filename != "NA" {
					task.FileName = filepath.Base(filename)
					task.SavePath = filename
					if task.Title == "" || task.Title == task.URL {
						task.Title = task.FileName
					}
				}
				task.mu.Unlock()

				now := time.Now()
				if now.Sub(lastUpdate) >= 400*time.Millisecond {
					qm.BroadcastUpdate()
					lastUpdate = now
				}
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
			task.mu.Unlock()
			qm.BroadcastUpdate()
		} else if strings.Contains(line, "[Merger] Merging formats into") {
			parts := strings.Split(line, `"`)
			if len(parts) >= 2 {
				mergedFile := parts[1]
				task.mu.Lock()
				task.SavePath = mergedFile
				task.FileName = filepath.Base(mergedFile)
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
