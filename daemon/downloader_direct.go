package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	maxWinSafePathUnits = 238
	turboMinSizeBytes   = 2 * 1024 * 1024 // 2 MB threshold for multi-connection split
	turboBlockSize      = 2 * 1024 * 1024 // 2 MB sequential rolling blocks (contiguous-friendly + low RAM)
	turboWorkers        = 6               // 6 parallel HTTP streams (~192 KB total buffer RAM)
)

var (
	invalidFileCharsRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	multiSpaceRe       = regexp.MustCompile(`\s+`)

	// Use dedicated HTTP/1.1 connections for parallel byte-range workers so all 6 Turbo
	// streams get independent TCP sockets instead of multiplexing onto a single HTTP/2
	// connection (which causes head-of-line stalls and per-socket throttling on CDNs/VPNs).
	directHTTPTransport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   24,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
	}
)

type turboStateMeta struct {
	TotalBytes      int64       `json:"totalBytes"`
	BlockSize       int64       `json:"blockSize,omitempty"`
	DoneBlocks      []bool      `json:"doneBlocks,omitempty"`
	CompletedRanges []byteRange `json:"completedRanges,omitempty"`
}

func (m *turboStateMeta) normalizeCompletedRanges() []byteRange {
	if m == nil || m.TotalBytes <= 0 {
		return nil
	}
	all := make([]byteRange, 0, len(m.CompletedRanges)+len(m.DoneBlocks))
	all = append(all, m.CompletedRanges...)
	if m.BlockSize > 0 {
		for i, done := range m.DoneBlocks {
			if !done {
				continue
			}
			start := int64(i) * m.BlockSize
			end := start + m.BlockSize - 1
			if end >= m.TotalBytes {
				end = m.TotalBytes - 1
			}
			if start <= end {
				all = append(all, byteRange{Start: start, End: end})
			}
		}
	}
	return mergeByteRanges(all, m.TotalBytes)
}

func (m *turboStateMeta) completedBytes() int64 {
	return countRangeBytes(m.normalizeCompletedRanges())
}

func (m *turboStateMeta) contiguousPrefixBytes() int64 {
	ranges := m.normalizeCompletedRanges()
	if len(ranges) > 0 && ranges[0].Start == 0 {
		return ranges[0].End + 1
	}
	return 0
}

func loadTurboMeta(metaPath string) (*turboStateMeta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var m turboStateMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.TotalBytes <= 0 || (len(m.CompletedRanges) == 0 && (m.BlockSize <= 0 || len(m.DoneBlocks) == 0)) {
		return nil, fmt.Errorf("invalid turbo metadata")
	}
	return &m, nil
}

func saveTurboMeta(metaPath string, m *turboStateMeta) {
	if m == nil {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		_ = os.Rename(tmp, metaPath)
	}
}

func buildDirectRequest(ctx context.Context, task *DownloadTask, userAgent, rangeHeader string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", task.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	if task.Referer != "" {
		req.Header.Set("Referer", task.Referer)
	}
	if len(task.Cookies) > 0 {
		req.Header.Set("Cookie", FormatCookieHeader(task.Cookies))
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return req, nil
}

func runDirectDownload(ctx context.Context, qm *QueueManager, task *DownloadTask) error {
	downloadDir := resolveTaskDownloadDir(qm.config.GetDownloadDir(), task.SubDir)
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	client := &http.Client{
		Transport: directHTTPTransport,
		Timeout:   0, // No overall timeout for streaming downloads
	}

	userAgent := task.UserAgent
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	}

	task.mu.Lock()
	rawFileName := task.FileName
	rawTitle := task.Title
	task.mu.Unlock()

	fileName := rawFileName
	if fileName == "" {
		fileName = sanitizeFileName(rawTitle)
	}
	if fileName == "" || fileName == "." || fileName == "/" {
		fileName = defaultTitleFromURL(task.URL)
	}
	for strings.HasSuffix(strings.ToLower(fileName), ".part") {
		fileName = strings.TrimSuffix(fileName, fileName[len(fileName)-5:])
	}
	fileName = fitDirectFileName(downloadDir, sanitizeFileName(fileName))

	speedMode := qm.ResolveTaskSpeedMode(task)
	if speedMode == SpeedModeRegular {
		return runRegularDirectDownload(ctx, client, qm, task, downloadDir, fileName, rawFileName == "", userAgent, " (Live)")
	}
	return runTurboDirectDownload(ctx, client, qm, task, downloadDir, fileName, rawFileName == "", userAgent)
}

func runRegularDirectDownload(ctx context.Context, client *http.Client, qm *QueueManager, task *DownloadTask, downloadDir, fileName string, allowHeaderRename bool, userAgent, speedSuffix string) error {
	partPath := filepath.Join(downloadDir, fileName+".part")
	turboMetaPath := partPath + ".turbo"

	// If switching from Turbo -> Regular mid-download, trim .part cleanly to the contiguous
	// downloaded prefix from byte 0 so single-stream resume + live video playback work immediately!
	if meta, mErr := loadTurboMeta(turboMetaPath); mErr == nil {
		contig := meta.contiguousPrefixBytes()
		if fi, sErr := os.Stat(partPath); sErr == nil && fi.Size() > contig {
			_ = os.Truncate(partPath, contig)
		}
		_ = os.Remove(turboMetaPath)
	}

	var existingBytes int64 = 0
	if fi, err := os.Stat(partPath); err == nil {
		existingBytes = fi.Size()
	}

	hostKey := extractHostKey(task.URL)
	var (
		resp    *http.Response
		release func()
	)
	for throttleTry := 0; ; throttleTry++ {
		rel, acqErr := qm.hostPool.Acquire(ctx, hostKey)
		if acqErr != nil {
			return acqErr
		}
		rangeHdr := ""
		if existingBytes > 0 {
			rangeHdr = fmt.Sprintf("bytes=%d-", existingBytes)
		}

		req, err := buildDirectRequest(ctx, task, userAgent, rangeHdr)
		if err != nil {
			rel()
			return err
		}

		r, err := client.Do(req)
		if err != nil {
			rel()
			return err
		}

		// If 416 Range Not Satisfiable, try restarting from 0
		if r.StatusCode == http.StatusRequestedRangeNotSatisfiable && existingBytes > 0 {
			_ = r.Body.Close()
			existingBytes = 0
			_ = os.Remove(partPath)

			req2, err := buildDirectRequest(ctx, task, userAgent, "")
			if err != nil {
				rel()
				return err
			}
			r, err = client.Do(req2)
			if err != nil {
				rel()
				return err
			}
		}

		if (r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusServiceUnavailable) && throttleTry < 6 {
			retryAfter := parseRetryAfter(r.Header.Get("Retry-After"))
			_ = r.Body.Close()
			qm.hostPool.ReportThrottle(hostKey, retryAfter)
			rel()
			continue
		}

		resp = r
		release = rel
		break
	}
	defer release()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned HTTP %d %s", resp.StatusCode, resp.Status)
	}
	qm.hostPool.ReportSuccess(hostKey)

	if allowHeaderRename {
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

	var totalBytes int64 = -1
	appendMode := false
	if resp.StatusCode == http.StatusPartialContent {
		appendMode = true
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			totalBytes = parseContentRangeTotal(cr)
		} else if resp.ContentLength > 0 {
			totalBytes = existingBytes + resp.ContentLength
		}
	} else {
		existingBytes = 0
		totalBytes = resp.ContentLength
	}

	return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, existingBytes, totalBytes, appendMode, speedSuffix, qm.TaskLimiters(task))
}

func streamResponseSequentially(ctx context.Context, qm *QueueManager, task *DownloadTask, body io.Reader, downloadDir, fileName, partPath string, existingBytes, totalBytes int64, appendMode bool, speedSuffix string, limiters []*bandwidthLimiter) error {
	flags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(partPath, flags, 0644)
	if err != nil {
		return fmt.Errorf("failed to open target file: %w", err)
	}
	defer file.Close()

	task.mu.Lock()
	task.FileName = fileName
	if task.Title == "" || task.Title == task.URL {
		task.Title = fileName
	} else {
		task.Title = fileName
	}
	task.SavePath = partPath
	task.DownloadedBytes = existingBytes
	task.TotalBytes = totalBytes
	task.mu.Unlock()

	buffer := make([]byte, 64*1024) // 64KB buffer
	lastUpdate := time.Now()
	firstChunk := true
	var bytesSinceUpdate int64 = 0
	downloaded := existingBytes

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		n, readErr := body.Read(buffer)
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
				speedStr := formatBytes(int64(speedBytesPerSec)) + "/s" + speedSuffix

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

			if len(limiters) > 0 {
				if lErr := waitRateLimiters(ctx, n, limiters...); lErr != nil {
					return lErr
				}
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
	return finalizeDirectDownload(task, downloadDir, fileName, partPath, downloaded, totalBytes)
}

func finalizeDirectDownload(task *DownloadTask, downloadDir, fileName, partPath string, downloaded, totalBytes int64) error {
	_ = removeFileWithRetry(partPath+".turbo", 5, 50*time.Millisecond)
	_ = removeFileWithRetry(partPath+".turbo.tmp", 5, 50*time.Millisecond)
	finalPath := getUniqueFilename(filepath.Join(downloadDir, fileName))
	if err := renameFileWithRetry(partPath, finalPath, 10, 100*time.Millisecond); err != nil {
		return fmt.Errorf("failed to finalize file: %w", err)
	}

	task.mu.Lock()
	task.SavePath = finalPath
	task.FileName = filepath.Base(finalPath)
	task.DownloadedBytes = downloaded
	if totalBytes <= 0 {
		task.TotalBytes = downloaded
	} else {
		task.TotalBytes = totalBytes
	}
	task.Progress = 100.0
	task.Speed = ""
	task.ETA = ""
	task.mu.Unlock()
	return nil
}

type releaseOnCloseReadCloser struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (r *releaseOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		if r.onClose != nil {
			r.onClose()
		}
	})
	return err
}

func runTurboDirectDownload(ctx context.Context, client *http.Client, qm *QueueManager, task *DownloadTask, downloadDir, fileName string, allowHeaderRename bool, userAgent string) error {
	partPath := filepath.Join(downloadDir, fileName+".part")
	turboMetaPath := partPath + ".turbo"

	loadedMeta, metaErr := loadTurboMeta(turboMetaPath)
	var existingBytes int64
	if fi, err := os.Stat(partPath); err == nil {
		existingBytes = fi.Size()
	} else {
		loadedMeta = nil
	}

	hasPriorState := (metaErr == nil && loadedMeta != nil) || existingBytes > 0
	hostKey := extractHostKey(task.URL)

	var (
		resp          *http.Response
		release       func()
		probeCtx      context.Context
		probeWatchdog *stallWatchdog
	)
	for throttleTry := 0; ; throttleTry++ {
		rel, acqErr := qm.hostPool.Acquire(ctx, hostKey)
		if acqErr != nil {
			return acqErr
		}
		probeRange := fmt.Sprintf("bytes=0-%d", turboBlockSize-1)
		if hasPriorState {
			probeRange = "bytes=0-0"
		}

		pCtx, pDog := newStallWatchdog(ctx, 30*time.Second)
		req, err := buildDirectRequest(pCtx, task, userAgent, probeRange)
		if err != nil {
			pDog.Stop()
			rel()
			return err
		}
		r, err := client.Do(req)
		if err != nil {
			pDog.Stop()
			rel()
			return err
		}

		if r.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			_ = r.Body.Close()
			pDog.Kick()
			reqPlain, rErr := buildDirectRequest(pCtx, task, userAgent, "")
			if rErr != nil {
				pDog.Stop()
				rel()
				return rErr
			}
			r, err = client.Do(reqPlain)
			if err != nil {
				pDog.Stop()
				rel()
				return err
			}
			hasPriorState = false
		}

		if (r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusServiceUnavailable) && throttleTry < 6 {
			retryAfter := parseRetryAfter(r.Header.Get("Retry-After"))
			_ = r.Body.Close()
			pDog.Stop()
			qm.hostPool.ReportThrottle(hostKey, retryAfter)
			rel()
			continue
		}

		resp = r
		release = rel
		probeCtx = pCtx
		probeWatchdog = pDog
		break
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		status := resp.Status
		code := resp.StatusCode
		_ = resp.Body.Close()
		probeWatchdog.Stop()
		release()
		return fmt.Errorf("server returned HTTP %d %s", code, status)
	}

	if allowHeaderRename {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			if parsedName := parseFilenameFromHeader(cd); parsedName != "" {
				fileName = fitDirectFileName(downloadDir, sanitizeFileName(parsedName))
				newPartPath := filepath.Join(downloadDir, fileName+".part")
				if newPartPath != partPath {
					_ = os.Rename(partPath, newPartPath)
					_ = os.Rename(turboMetaPath, newPartPath+".turbo")
					partPath = newPartPath
					turboMetaPath = newPartPath + ".turbo"
				}
			}
		}
	}

	limiters := qm.TaskLimiters(task)

	// If server does not support Range (200 OK), stream the response directly in 1 pass.
	if resp.StatusCode == http.StatusOK {
		qm.hostPool.ReportSuccess(hostKey)
		probeWatchdog.Disarm()
		defer probeWatchdog.Stop()
		defer release()
		defer resp.Body.Close()
		_ = os.Remove(turboMetaPath)
		return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, 0, resp.ContentLength, false, "", limiters)
	}

	totalBytes := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if totalBytes <= 0 {
		_ = resp.Body.Close()
		probeWatchdog.Stop()
		release()
		// Server returned 206 without total size; fall back to standard sequential mode.
		return runRegularDirectDownload(ctx, client, qm, task, downloadDir, fileName, false, userAgent, "")
	}

	// Small files (<= 2 MB) on fresh probe already fit inside our initial 0-(2MB-1) response.
	if !hasPriorState && totalBytes <= turboBlockSize {
		qm.hostPool.ReportSuccess(hostKey)
		probeWatchdog.Disarm()
		defer probeWatchdog.Stop()
		defer release()
		defer resp.Body.Close()
		return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, 0, totalBytes, false, "", limiters)
	}

	var firstBlockBody io.ReadCloser
	if hasPriorState {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		probeWatchdog.Stop()
		probeWatchdog = nil
		probeCtx = nil
		release()
	} else {
		firstBlockBody = &releaseOnCloseReadCloser{ReadCloser: resp.Body, onClose: release}
	}
	defer func() {
		if firstBlockBody != nil {
			_ = firstBlockBody.Close()
		}
		if probeWatchdog != nil {
			probeWatchdog.Stop()
		}
	}()

	var initialRanges []byteRange
	openFlags := os.O_CREATE | os.O_RDWR

	if loadedMeta != nil && loadedMeta.TotalBytes == totalBytes {
		initialRanges = loadedMeta.normalizeCompletedRanges()
	} else if existingBytes > 0 && existingBytes <= totalBytes && loadedMeta == nil {
		// Transitioning a paused/active Regular download -> Turbo: preserve contiguous prefix bytes!
		initialRanges = []byteRange{{Start: 0, End: existingBytes - 1}}
	} else {
		openFlags |= os.O_TRUNC
	}

	file, err := os.OpenFile(partPath, openFlags, 0644)
	if err != nil {
		return fmt.Errorf("failed to open partial file: %w", err)
	}
	sparseOK := enableSparseFile(file)
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()

	initialDone := countRangeBytes(initialRanges)
	task.mu.Lock()
	task.FileName = fileName
	task.Title = fileName
	task.SavePath = partPath
	task.DownloadedBytes = initialDone
	task.TotalBytes = totalBytes
	task.mu.Unlock()
	qm.BroadcastUpdate()

	headers := map[string]string{
		"User-Agent": userAgent,
	}
	if task.Referer != "" {
		headers["Referer"] = task.Referer
	}
	if len(task.Cookies) > 0 {
		headers["Cookie"] = FormatCookieHeader(task.Cookies)
	}

	endpoint := &streamEndpointState{
		url:          task.URL,
		headers:      headers,
		maxRefreshes: 4,
	}

	probeEnd := int64(turboBlockSize) - 1
	if probeEnd >= totalBytes {
		probeEnd = totalBytes - 1
	}

	wsErr := runWorkStealingDownload(ctx, workStealingOptions{
		Client:         client,
		Endpoint:       endpoint,
		File:           file,
		TotalBytes:     totalBytes,
		InitialRanges:  initialRanges,
		TurboMetaPath:  turboMetaPath,
		MaxWorkers:     turboWorkers,
		MaxRequestSpan: 0,
		NonSparseFile:  !sparseOK,
		HostPool:       qm.hostPool,
		Limiters:       limiters,
		ProbeBody:      firstBlockBody,
		ProbeCtx:       probeCtx,
		ProbeWatchdog:  probeWatchdog,
		ProbeStart:     0,
		ProbeEnd:       probeEnd,
		OnProgress: func(cur, tot int64, rate float64, workers int) {
			pct := float64(cur) / float64(tot) * 100.0
			if pct > 100.0 {
				pct = 100.0
			}
			speedStr := fmt.Sprintf("%s/s (%dx)", formatBytes(int64(rate)), workers)
			etaStr := ""
			if rate > 0 && tot > cur {
				etaStr = formatDuration(int64(float64(tot-cur) / rate))
			}

			task.mu.Lock()
			stillActive := task.Status == StatusDownloading
			if stillActive {
				task.DownloadedBytes = cur
				task.TotalBytes = tot
				task.Progress = pct
				if rate > 0 || task.Speed == "" || strings.Contains(task.Speed, "/s") {
					task.Speed = speedStr
					task.ETA = etaStr
				}
			}
			task.mu.Unlock()
			if stillActive {
				qm.BroadcastUpdate()
			}
		},
		OnSegments: func(segs []SegmentProgress) {
			task.mu.Lock()
			if task.Status == StatusDownloading {
				task.Segments = segs
			}
			task.mu.Unlock()
		},
	})
	firstBlockBody = nil
	probeWatchdog = nil

	if wsErr != nil {
		return wsErr
	}

	_ = file.Truncate(totalBytes)
	_ = file.Close()
	fileClosed = true
	return finalizeDirectDownload(task, downloadDir, fileName, partPath, totalBytes, totalBytes)
}

var (
	ytTitleTokenRe      = regexp.MustCompile(`%\(title\)(?:\.\d+)?s`)
	ytFormatCodeExtRe   = regexp.MustCompile(`\.f[0-9A-Za-z_-]{1,24}$`)
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
	case ".webm", ".mp4", ".m4a", ".mp3", ".opus", ".mkv", ".ogg", ".wav", ".aac", ".flv", ".ts", ".mov", ".3gp":
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
	for strings.HasSuffix(strings.ToLower(stem), ".part") {
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
