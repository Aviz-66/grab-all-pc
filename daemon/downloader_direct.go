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
	"unicode/utf16"
)

const maxWinSafePathUnits = 238

var (
	invalidFileCharsRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	multiSpaceRe       = regexp.MustCompile(`\s+`)
)

func runDirectDownload(ctx context.Context, qm *QueueManager, task *DownloadTask) error {
	downloadDir := resolveTaskDownloadDir(qm.config.GetDownloadDir(), task.SubDir)
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
	fileName = fitDirectFileName(downloadDir, sanitizeFileName(fileName))

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
				fileName = fitDirectFileName(downloadDir, sanitizeFileName(parsedName))
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
				stillActive := task.Status == StatusDownloading
				if stillActive {
					task.DownloadedBytes = downloaded
					task.TotalBytes = totalBytes
					task.Progress = progress
					task.Speed = speedStr
					task.ETA = etaStr
				}
				task.mu.Unlock()

				if stillActive {
					qm.BroadcastUpdate()
				}

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

var (
	ytTitleTokenRe      = regexp.MustCompile(`%\(title\)(?:\.\d+)?s`)
	ytFormatCodeExtRe   = regexp.MustCompile(`\.f[0-9A-Za-z_-]{1,12}$`)
	trailingBracketIDRe = regexp.MustCompile(` \[([A-Za-z0-9_-]{6,16})\]$`)
)

// utf16Len counts UTF-16 code units, matching Win32 MAX_PATH (260 WCHARs) 1:1.
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// truncateUTF16Safe slices a UTF-8 string cleanly at rune boundaries without exceeding
// maxUnits UTF-16 code units (never splitting multi-byte UTF-8 or surrogate pairs).
func truncateUTF16Safe(s string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	if utf16Len(s) <= maxUnits {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if used+w > maxUnits {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}

func isWindowsReservedName(stem string) bool {
	switch strings.ToUpper(strings.TrimSpace(stem)) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

func sanitizeCleanComponent(name string, maxUnits int, fallback string) string {
	cleaned := invalidFileCharsRe.ReplaceAllString(name, "_")
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.Trim(cleaned, " .")
	cleaned = truncateUTF16Safe(cleaned, maxUnits)
	// Re-trim trailing spaces and dots after truncation (Win32 forbids trailing space/dot)
	cleaned = strings.TrimRight(cleaned, " .")
	if cleaned == "" {
		return fallback
	}
	baseStem := strings.SplitN(cleaned, ".", 2)[0]
	if isWindowsReservedName(baseStem) {
		cleaned = truncateUTF16Safe("_"+cleaned, maxUnits)
		cleaned = strings.TrimRight(cleaned, " .")
	}
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func sanitizeFileName(name string) string {
	trimmed := strings.TrimSpace(name)
	ext := filepath.Ext(trimmed)
	if len(ext) > 1 && utf16Len(ext) <= 16 && !invalidFileCharsRe.MatchString(ext) && !strings.Contains(ext, " ") {
		ext = strings.TrimRight(ext, " .")
		if len(ext) > 1 {
			stem := strings.TrimSuffix(trimmed, filepath.Ext(trimmed))
			maxStem := 120 - utf16Len(ext)
			if maxStem < 12 {
				maxStem = 12
			}
			cleanStem := sanitizeCleanComponent(stem, maxStem, "download")
			return cleanStem + ext
		}
	}
	return sanitizeCleanComponent(trimmed, 120, "download.bin")
}

func sanitizeSubDirName(name string) string {
	return sanitizeCleanComponent(name, 55, "Playlist")
}

// resolveTaskDownloadDir resolves a playlist/category subfolder while guaranteeing headroom
// below Windows MAX_PATH (260). If a longer legacy subfolder already exists on disk (e.g. from
// an in-progress playlist started before v2.1.2) and leaves enough room for filenames, it is
// reused so retried tracks land alongside completed tracks.
func resolveTaskDownloadDir(baseDir, subDir string) string {
	sub := strings.TrimSpace(subDir)
	if sub == "" {
		return baseDir
	}

	legacyClean := strings.TrimRight(invalidFileCharsRe.ReplaceAllString(sub, "_"), " .")
	if legacyClean != "" {
		cand := filepath.Join(baseDir, legacyClean)
		if info, err := os.Stat(cand); err == nil && info.IsDir() && utf16Len(cand) <= 185 {
			return cand
		}
	}

	avail := 175 - utf16Len(baseDir) - 1
	if avail < 12 {
		return baseDir
	}
	maxSub := 55
	if avail < maxSub {
		maxSub = avail
	}
	return filepath.Join(baseDir, sanitizeCleanComponent(sub, maxSub, "Playlist"))
}

func fitDirectFileName(downloadDir, fileName string) string {
	maxFileUnits := maxWinSafePathUnits - utf16Len(downloadDir) - 1 - 5 // 5 for ".part"
	if maxFileUnits < 16 {
		maxFileUnits = 16
	}
	if utf16Len(fileName) <= maxFileUnits {
		return fileName
	}
	ext := filepath.Ext(fileName)
	if len(ext) > 1 && utf16Len(ext) <= 12 && maxFileUnits > utf16Len(ext)+6 {
		stem := strings.TrimSuffix(fileName, ext)
		return sanitizeCleanComponent(stem, maxFileUnits-utf16Len(ext), "download") + ext
	}
	return sanitizeCleanComponent(fileName, maxFileUnits, "download.bin")
}

func isKnownYtMediaExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".webm", ".mp4", ".m4a", ".mp3", ".opus", ".mkv", ".ogg", ".wav", ".aac", ".flv", ".ts", ".mov":
		return true
	}
	return false
}

// fitYtDlpOutputTemplate dynamically caps yt-dlp's output template (or a previously persisted
// literal filename) so the full expanded path including temporary suffixes like ".f251.webm.part"
// always stays safely under Windows's 260-character MAX_PATH limit.
func fitYtDlpOutputTemplate(downloadDir, tmpl string) string {
	dirUnits := utf16Len(downloadDir)
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		tmpl = "%(title)s [%(id)s].%(ext)s"
	}

	if strings.Contains(tmpl, "%(") {
		if !ytTitleTokenRe.MatchString(tmpl) {
			return tmpl
		}
		probe := ytTitleTokenRe.ReplaceAllString(tmpl, "")
		probe = strings.ReplaceAll(probe, "%(id)s", "12345678901")
		probe = strings.ReplaceAll(probe, "%(ext)s", "f251.webm.part")
		overhead := utf16Len(probe) + 1 // +1 for path separator
		maxTitle := maxWinSafePathUnits - dirUnits - overhead
		if maxTitle > 100 {
			maxTitle = 100
		}
		if maxTitle < 16 {
			maxTitle = 16
		}
		return ytTitleTokenRe.ReplaceAllString(tmpl, fmt.Sprintf("%%(title).%ds", maxTitle))
	}

	// Literal filename case (e.g. custom filename, or a task persisted after an earlier failed attempt)
	stem := tmpl
	if strings.HasSuffix(strings.ToLower(stem), ".part") {
		stem = stem[:len(stem)-5]
	}
	if ext := filepath.Ext(stem); isKnownYtMediaExt(ext) {
		stem = strings.TrimSuffix(stem, ext)
		stem = ytFormatCodeExtRe.ReplaceAllString(stem, "")
	} else if len(ext) > 1 && utf16Len(ext) <= 8 {
		stem = strings.TrimSuffix(stem, ext)
	}

	maxStemUnits := maxWinSafePathUnits - dirUnits - 1 - 15 // 15 for ".f251.webm.part"
	if maxStemUnits > 115 {
		maxStemUnits = 115
	}
	if maxStemUnits < 18 {
		maxStemUnits = 18
	}

	if match := trailingBracketIDRe.FindStringSubmatch(stem); len(match) > 0 {
		idSuffix := match[0]
		if maxStemUnits > utf16Len(idSuffix)+8 {
			prefix := strings.TrimSuffix(stem, idSuffix)
			prefix = sanitizeCleanComponent(prefix, maxStemUnits-utf16Len(idSuffix), "video")
			stem = prefix + idSuffix
		} else {
			stem = sanitizeCleanComponent(stem, maxStemUnits, "video")
		}
	} else {
		stem = sanitizeCleanComponent(stem, maxStemUnits, "video")
	}

	// Escape any literal '%' so yt-dlp's output template parser treats it literally
	stem = strings.ReplaceAll(stem, "%", "%%")
	return stem + ".%(ext)s"
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
