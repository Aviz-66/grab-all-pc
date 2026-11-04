package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func runDirectDownload(ctx context.Context, qm *QueueManager, task *DownloadTask) error {
	downloadDir := qm.config.GetDownloadDir()
	if task.SubDir != "" {
		downloadDir = filepath.Join(downloadDir, sanitizeFileName(task.SubDir))
	}
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	client := &http.Client{
		Timeout: 0, // No timeout for streaming downloads
	}

	userAgent := task.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	}

	// Determine candidate filename
	fileName := task.FileName
	if fileName == "" {
		fileName = sanitizeFileName(task.Title)
	}
	if fileName == "" || fileName == "." || fileName == "/" {
		fileName = defaultTitleFromURL(task.URL)
	}
	fileName = sanitizeFileName(fileName)

	var existingBytes int64 = 0
	partPath := filepath.Join(downloadDir, fileName+".part")
	if fi, err := os.Stat(partPath); err == nil {
		existingBytes = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", task.URL, nil)
	if err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	if task.Referer != "" {
		req.Header.Set("Referer", task.Referer)
	}
	if len(task.Cookies) > 0 {
		req.Header.Set("Cookie", FormatCookieHeader(task.Cookies))
	}

	if existingBytes > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingBytes))
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// If 416 Range Not Satisfiable, try restarting from 0
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && existingBytes > 0 {
		existingBytes = 0
		_ = os.Remove(partPath)

		req2, err := http.NewRequestWithContext(ctx, "GET", task.URL, nil)
		if err != nil {
			return err
		}
		req2.Header.Set("User-Agent", userAgent)
		if task.Referer != "" {
			req2.Header.Set("Referer", task.Referer)
		}
		if len(task.Cookies) > 0 {
			req2.Header.Set("Cookie", FormatCookieHeader(task.Cookies))
		}
		resp, err = client.Do(req2)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned HTTP %d %s", resp.StatusCode, resp.Status)
	}

	// Try extracting filename from Content-Disposition if not explicitly given
	if task.FileName == "" {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			if parsedName := parseFilenameFromHeader(cd); parsedName != "" {
				fileName = sanitizeFileName(parsedName)
				newPartPath := filepath.Join(downloadDir, fileName+".part")
				if newPartPath != partPath {
					_ = os.Rename(partPath, newPartPath)
					partPath = newPartPath
				}
			}
		}
	}

	task.mu.Lock()
	task.FileName = fileName
	task.Title = fileName
	task.mu.Unlock()

	var file *os.File
	var totalBytes int64 = -1

	if resp.StatusCode == http.StatusPartialContent {
		// Appending to existing file
		file, err = os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open partial file: %w", err)
		}
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			// bytes 100-999/1000
			totalBytes = parseContentRangeTotal(cr)
		} else if resp.ContentLength > 0 {
			totalBytes = existingBytes + resp.ContentLength
		}
	} else {
		// Fresh download (or server does not support ranges)
		existingBytes = 0
		file, err = os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to create target file: %w", err)
		}
		totalBytes = resp.ContentLength
	}
	defer file.Close()

	task.mu.Lock()
	task.DownloadedBytes = existingBytes
	task.TotalBytes = totalBytes
	task.mu.Unlock()

	// Stream transfer with rate calculation
	buffer := make([]byte, 64*1024) // 64KB buffer
	lastUpdate := time.Now()
	firstChunk := true
	var bytesSinceUpdate int64 = 0
	downloaded := existingBytes

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := file.Write(buffer[:n]); writeErr != nil {
				return fmt.Errorf("write error: %w", writeErr)
			}

			downloaded += int64(n)
			bytesSinceUpdate += int64(n)

			now := time.Now()
			elapsed := now.Sub(lastUpdate)
			if firstChunk || elapsed >= 150*time.Millisecond {
				firstChunk = false
				secs := elapsed.Seconds()
				if secs <= 0.001 {
					secs = 0.001
				}
				speedBytesPerSec := float64(bytesSinceUpdate) / secs
				speedStr := formatBytes(int64(speedBytesPerSec)) + "/s"

				var progress float64 = 0
				etaStr := ""
				if totalBytes > 0 {
					progress = float64(downloaded) / float64(totalBytes) * 100.0
					if progress > 100.0 {
						progress = 100.0
					}
					if speedBytesPerSec > 0 {
						remBytes := totalBytes - downloaded
						if remBytes > 0 {
							etaSec := int64(float64(remBytes) / speedBytesPerSec)
							etaStr = formatDuration(etaSec)
						}
					}
				}

				task.mu.Lock()
				task.DownloadedBytes = downloaded
				task.TotalBytes = totalBytes
				task.Progress = progress
				task.Speed = speedStr
				task.ETA = etaStr
				task.mu.Unlock()

				qm.BroadcastUpdate()

				bytesSinceUpdate = 0
				lastUpdate = now
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("stream read error: %w", readErr)
		}
	}

	_ = file.Close()

	// Finalize file: move from .part to final file
	finalPath := filepath.Join(downloadDir, fileName)
	finalPath = getUniqueFilename(finalPath)

	if err := os.Rename(partPath, finalPath); err != nil {
		return fmt.Errorf("failed to finalize file: %w", err)
	}

	task.mu.Lock()
	task.SavePath = finalPath
	task.FileName = filepath.Base(finalPath)
	task.DownloadedBytes = downloaded
	if totalBytes <= 0 {
		task.TotalBytes = downloaded
	}
	task.Progress = 100.0
	task.Speed = ""
	task.ETA = ""
	task.mu.Unlock()

	return nil
}

func sanitizeFileName(name string) string {
	// Remove invalid chars
	reg := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	cleaned := reg.ReplaceAllString(name, "_")
	cleaned = strings.TrimSpace(cleaned)
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		return "download.bin"
	}
	if len(cleaned) > 200 {
		cleaned = cleaned[:200]
	}
	return cleaned
}

func parseFilenameFromHeader(header string) string {
	_, params, err := mime.ParseMediaType(header)
	if err == nil {
		if filename, ok := params["filename*"]; ok {
			// RFC 5987: UTF-8''encoded_name
			parts := strings.SplitN(filename, "''", 2)
			if len(parts) == 2 {
				if unescaped, err := url.PathUnescape(parts[1]); err == nil {
					return unescaped
				}
			}
		}
		if filename, ok := params["filename"]; ok && filename != "" {
			return filename
		}
	}
	return ""
}

func parseContentRangeTotal(cr string) int64 {
	// Example: "bytes 0-1023/2048" or "bytes */2048"
	parts := strings.Split(cr, "/")
	if len(parts) == 2 {
		if total, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
			return total
		}
	}
	return -1
}

func getUniqueFilename(targetPath string) string {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return targetPath
	}

	dir := filepath.Dir(targetPath)
	ext := filepath.Ext(targetPath)
	base := strings.TrimSuffix(filepath.Base(targetPath), ext)

	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return targetPath
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatDuration(seconds int64) string {
	if seconds < 0 {
		return ""
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
