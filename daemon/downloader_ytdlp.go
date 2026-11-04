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
	"time"
)

var (
	ansiRegex    = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	percentRegex = regexp.MustCompile(`([0-9.]+)%`)
)

func cleanOutput(s string) string {
	return strings.TrimSpace(ansiRegex.ReplaceAllString(s, ""))
}

// resolveYtDlpBinary finds the yt-dlp executable path.
func resolveYtDlpBinary(cfg *Config) (string, error) {
	// 1. Configured custom path
	if cfg.YtDlpPath != "" && cfg.YtDlpPath != "yt-dlp" {
		if path, err := exec.LookPath(cfg.YtDlpPath); err == nil {
			return path, nil
		}
		if _, err := os.Stat(cfg.YtDlpPath); err == nil {
			return cfg.YtDlpPath, nil
		}
	}

	// 2. Check local ./bin/yt-dlp or ./bin/yt-dlp.exe
	binName := "yt-dlp"
	if runtime.GOOS == "windows" {
		binName = "yt-dlp.exe"
	}

	cwd, err := os.Getwd()
	if err == nil {
		localPath := filepath.Join(cwd, "bin", binName)
		if _, err := os.Stat(localPath); err == nil {
			return localPath, nil
		}
	}

	// 3. System PATH
	if path, err := exec.LookPath(binName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("yt-dlp executable not found. Please install it or use the Install button in settings")
}

// resolveFFmpegBinary finds ffmpeg if available.
func resolveFFmpegBinary(cfg *Config) string {
	if cfg.FFmpegPath != "" && cfg.FFmpegPath != "ffmpeg" {
		if path, err := exec.LookPath(cfg.FFmpegPath); err == nil {
			return path
		}
		if _, err := os.Stat(cfg.FFmpegPath); err == nil {
			return cfg.FFmpegPath
		}
	}

	binName := "ffmpeg"
	if runtime.GOOS == "windows" {
		binName = "ffmpeg.exe"
	}

	cwd, err := os.Getwd()
	if err == nil {
		localPath := filepath.Join(cwd, "bin", binName)
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

	// Handle cookies
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

	// Base arguments
	args := []string{
		"--newline",
		"--no-warnings",
		"--progress-template", "DOWNLOAD_INFO:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s|%(progress._total_bytes_str,progress._total_bytes_estimate_str)s|%(progress.filename)s",
		"--paths", fmt.Sprintf("home:%s", downloadDir),
		"--concurrent-fragments", "2",
		"--retries", "3",
		"--fragment-retries", "3",
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

	if task.UserAgent != "" {
		args = append(args, "--user-agent", task.UserAgent)
	}

	if task.Referer != "" {
		args = append(args, "--referer", task.Referer)
	}

	ffmpegBin := resolveFFmpegBinary(qm.config)
	if ffmpegBin != "" {
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

	var stderrLines []string
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := cleanOutput(scanner.Text())
			if text != "" {
				stderrLines = append(stderrLines, text)
				if len(stderrLines) > 20 {
					stderrLines = stderrLines[len(stderrLines)-20:]
				}
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	lastUpdate := time.Now()

	for scanner.Scan() {
		rawLine := scanner.Text()
		line := cleanOutput(rawLine)

		if strings.HasPrefix(line, "DOWNLOAD_INFO:") {
			payload := strings.TrimPrefix(line, "DOWNLOAD_INFO:")
			parts := strings.Split(payload, "|")
			if len(parts) >= 4 {
				percentStr := strings.TrimSpace(parts[0])
				speedStr := strings.TrimSpace(parts[1])
				etaStr := strings.TrimSpace(parts[2])
				totalStr := strings.TrimSpace(parts[3])

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
				_ = totalStr
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

	cmdErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if cmdErr != nil {
		detail := strings.Join(stderrLines, "; ")
		if detail != "" {
			return fmt.Errorf("yt-dlp failed: %s (%w)", detail, cmdErr)
		}
		return fmt.Errorf("yt-dlp failed: %w", cmdErr)
	}

	return nil
}

// DownloadYtDlpStandalone fetches the official standalone yt-dlp binary into destDir.
func DownloadYtDlpStandalone(destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}

	binName := "yt-dlp"
	downloadURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"

	if runtime.GOOS == "windows" {
		binName = "yt-dlp.exe"
		downloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	} else if runtime.GOOS == "darwin" {
		downloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos"
	}

	targetPath := filepath.Join(destDir, binName)
	tempPath := targetPath + ".download"

	client := &http.Client{
		Timeout: 2 * time.Minute,
	}

	resp, err := client.Get(downloadURL)
	if err != nil {
		return "", fmt.Errorf("failed to download yt-dlp from %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download yt-dlp: HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	_ = out.Close()

	if err := os.Chmod(tempPath, 0755); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	if err := os.Rename(tempPath, targetPath); err != nil {
		return "", err
	}

	return targetPath, nil
}
