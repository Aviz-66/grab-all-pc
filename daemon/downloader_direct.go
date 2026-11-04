package main

import (
	"context"
	"encoding/json"
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
	"sync"
	"sync/atomic"
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

	directHTTPTransport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
)

type turboStateMeta struct {
	TotalBytes int64  `json:"totalBytes"`
	BlockSize  int64  `json:"blockSize"`
	DoneBlocks []bool `json:"doneBlocks"`
}

func (m *turboStateMeta) completedBytes() int64 {
	if m == nil || m.BlockSize <= 0 {
		return 0
	}
	var sum int64
	for i, done := range m.DoneBlocks {
		if !done {
			continue
		}
		start := int64(i) * m.BlockSize
		ln := m.BlockSize
		if start+ln > m.TotalBytes {
			ln = m.TotalBytes - start
		}
		if ln > 0 {
			sum += ln
		}
	}
	return sum
}

func (m *turboStateMeta) contiguousPrefixBytes() int64 {
	if m == nil || m.BlockSize <= 0 {
		return 0
	}
	var sum int64
	for i, done := range m.DoneBlocks {
		if !done {
			break
		}
		start := int64(i) * m.BlockSize
		ln := m.BlockSize
		if start+ln > m.TotalBytes {
			ln = m.TotalBytes - start
		}
		if ln > 0 {
			sum += ln
		}
	}
	return sum
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
	if m.TotalBytes <= 0 || m.BlockSize <= 0 || len(m.DoneBlocks) == 0 {
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

	rangeHdr := ""
	if existingBytes > 0 {
		rangeHdr = fmt.Sprintf("bytes=%d-", existingBytes)
	}

	req, err := buildDirectRequest(ctx, task, userAgent, rangeHdr)
	if err != nil {
		return err
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

		req2, err := buildDirectRequest(ctx, task, userAgent, "")
		if err != nil {
			return err
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

	return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, existingBytes, totalBytes, appendMode, speedSuffix)
}

func streamResponseSequentially(ctx context.Context, qm *QueueManager, task *DownloadTask, body io.Reader, downloadDir, fileName, partPath string, existingBytes, totalBytes int64, appendMode bool, speedSuffix string) error {
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
	_ = os.Remove(partPath + ".turbo")
	finalPath := getUniqueFilename(filepath.Join(downloadDir, fileName))
	if err := os.Rename(partPath, finalPath); err != nil {
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
	probeRange := fmt.Sprintf("bytes=0-%d", turboBlockSize-1)
	if hasPriorState {
		probeRange = "bytes=0-0"
	}

	req, err := buildDirectRequest(ctx, task, userAgent, probeRange)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = resp.Body.Close()
		reqPlain, rErr := buildDirectRequest(ctx, task, userAgent, "")
		if rErr != nil {
			return rErr
		}
		resp, err = client.Do(reqPlain)
		if err != nil {
			return err
		}
		hasPriorState = false
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		status := resp.Status
		code := resp.StatusCode
		_ = resp.Body.Close()
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

	// If server does not support Range (200 OK), stream the response directly in 1 pass
	if resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		_ = os.Remove(turboMetaPath)
		return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, 0, resp.ContentLength, false, "")
	}

	totalBytes := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if totalBytes <= 0 {
		_ = resp.Body.Close()
		// Server returned 206 without total size; fall back to standard sequential mode
		return runRegularDirectDownload(ctx, client, qm, task, downloadDir, fileName, false, userAgent, "")
	}

	// Small files (< 2 MB) on fresh probe already fit inside our initial 0-(2MB-1) response!
	if !hasPriorState && totalBytes <= turboBlockSize {
		defer resp.Body.Close()
		return streamResponseSequentially(ctx, qm, task, resp.Body, downloadDir, fileName, partPath, 0, totalBytes, false, "")
	}

	var firstBlockBody io.ReadCloser
	if hasPriorState {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	} else {
		firstBlockBody = resp.Body
	}
	defer func() {
		if firstBlockBody != nil {
			_ = firstBlockBody.Close()
		}
	}()

	numBlocks := int((totalBytes + turboBlockSize - 1) / turboBlockSize)
	var meta *turboStateMeta
	openFlags := os.O_CREATE | os.O_RDWR

	if loadedMeta != nil && loadedMeta.TotalBytes == totalBytes && loadedMeta.BlockSize == turboBlockSize && len(loadedMeta.DoneBlocks) == numBlocks {
		meta = loadedMeta
	} else if existingBytes > 0 && existingBytes < totalBytes && loadedMeta == nil {
		// Transitioning a paused/active Regular download -> Turbo: preserve completed 2MB prefix blocks!
		meta = &turboStateMeta{
			TotalBytes: totalBytes,
			BlockSize:  turboBlockSize,
			DoneBlocks: make([]bool, numBlocks),
		}
		fullBlocks := int(existingBytes / turboBlockSize)
		for i := 0; i < fullBlocks && i < numBlocks; i++ {
			meta.DoneBlocks[i] = true
		}
	} else {
		openFlags |= os.O_TRUNC
		meta = &turboStateMeta{
			TotalBytes: totalBytes,
			BlockSize:  turboBlockSize,
			DoneBlocks: make([]bool, numBlocks),
		}
	}

	file, err := os.OpenFile(partPath, openFlags, 0644)
	if err != nil {
		return fmt.Errorf("failed to open partial file: %w", err)
	}
	defer file.Close()

	initialDone := meta.completedBytes()
	task.mu.Lock()
	task.FileName = fileName
	task.Title = fileName
	task.SavePath = partPath
	task.DownloadedBytes = initialDone
	task.TotalBytes = totalBytes
	task.mu.Unlock()
	qm.BroadcastUpdate()

	pending := make([]int, 0, numBlocks)
	for i := 0; i < numBlocks; i++ {
		if meta.DoneBlocks[i] {
			continue
		}
		if i == 0 && firstBlockBody != nil {
			continue // Consumed directly from initial Range response by worker 0
		}
		pending = append(pending, i)
	}

	totalTodo := len(pending)
	if firstBlockBody != nil {
		totalTodo++
	}
	if totalTodo == 0 {
		_ = file.Truncate(totalBytes)
		_ = file.Close()
		return finalizeDirectDownload(task, downloadDir, fileName, partPath, totalBytes, totalBytes)
	}

	workerCount := turboWorkers
	if totalTodo < workerCount {
		workerCount = totalTodo
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	var (
		metaMu    sync.Mutex
		nextIdx   atomic.Int64
		liveBytes atomic.Int64
		errOnce   sync.Once
		firstErr  error
		wg        sync.WaitGroup
	)
	liveBytes.Store(initialDone)

	recordErr := func(e error) {
		if e == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = e
			cancelWorkers()
		})
	}

	// Progress & sidecar persistence ticker
	stopProgress := make(chan struct{})
	var progWG sync.WaitGroup
	progWG.Add(1)
	go func() {
		defer progWG.Done()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		lastTick := time.Now()
		lastBytes := initialDone
		lastMetaSave := time.Now()

		for {
			select {
			case <-stopProgress:
				return
			case now := <-ticker.C:
				cur := liveBytes.Load()
				if cur > totalBytes {
					cur = totalBytes
				}
				elapsed := now.Sub(lastTick).Seconds()
				if elapsed <= 0.001 {
					elapsed = 0.001
				}
				delta := cur - lastBytes
				if delta < 0 {
					delta = 0
				}
				rate := float64(delta) / elapsed
				suffix := fmt.Sprintf(" (%dx)", workerCount)
				speedStr := formatBytes(int64(rate)) + "/s" + suffix
				pct := float64(cur) / float64(totalBytes) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
				etaStr := ""
				if rate > 0 && totalBytes > cur {
					etaStr = formatDuration(int64(float64(totalBytes-cur) / rate))
				}

				task.mu.Lock()
				stillActive := task.Status == StatusDownloading
				if stillActive {
					task.DownloadedBytes = cur
					task.TotalBytes = totalBytes
					task.Progress = pct
					if delta > 0 || task.Speed == "" {
						task.Speed = speedStr
						task.ETA = etaStr
					}
				}
				task.mu.Unlock()
				if stillActive {
					qm.BroadcastUpdate()
				}

				lastBytes = cur
				lastTick = now

				if now.Sub(lastMetaSave) >= time.Second {
					metaMu.Lock()
					saveTurboMeta(turboMetaPath, meta)
					metaMu.Unlock()
					lastMetaSave = now
				}
			}
		}
	}()

	writeStreamToBlock := func(r io.Reader, blockIdx int) error {
		start := int64(blockIdx) * turboBlockSize
		expLen := int64(turboBlockSize)
		if start+expLen > totalBytes {
			expLen = totalBytes - start
		}
		buf := make([]byte, 32*1024)
		var written int64
		for written < expLen {
			if workerCtx.Err() != nil {
				liveBytes.Add(-written)
				return workerCtx.Err()
			}
			toRead := len(buf)
			rem := int(expLen - written)
			if rem < toRead {
				toRead = rem
			}
			n, rErr := r.Read(buf[:toRead])
			if n > 0 {
				if _, wErr := file.WriteAt(buf[:n], start+written); wErr != nil {
					liveBytes.Add(-written)
					return fmt.Errorf("disk write error: %w", wErr)
				}
				written += int64(n)
				liveBytes.Add(int64(n))
			}
			if rErr != nil {
				if rErr == io.EOF && written == expLen {
					break
				}
				liveBytes.Add(-written)
				if rErr == io.EOF {
					return fmt.Errorf("unexpected EOF in block %d (%d/%d)", blockIdx, written, expLen)
				}
				return rErr
			}
		}
		metaMu.Lock()
		meta.DoneBlocks[blockIdx] = true
		metaMu.Unlock()
		return nil
	}

	fetchBlockWithRetry := func(blockIdx int) error {
		start := int64(blockIdx) * turboBlockSize
		end := start + turboBlockSize - 1
		if end >= totalBytes {
			end = totalBytes - 1
		}
		rangeStr := fmt.Sprintf("bytes=%d-%d", start, end)

		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if workerCtx.Err() != nil {
				return workerCtx.Err()
			}
			if attempt > 0 {
				select {
				case <-workerCtx.Done():
					return workerCtx.Err()
				case <-time.After(time.Duration(attempt*300) * time.Millisecond):
				}
			}
			bReq, rErr := buildDirectRequest(workerCtx, task, userAgent, rangeStr)
			if rErr != nil {
				return rErr
			}
			bResp, dErr := client.Do(bReq)
			if dErr != nil {
				lastErr = dErr
				continue
			}
			if bResp.StatusCode != http.StatusPartialContent {
				lastErr = fmt.Errorf("block %d returned HTTP %d", blockIdx, bResp.StatusCode)
				_ = bResp.Body.Close()
				continue
			}
			wErr := writeStreamToBlock(bResp.Body, blockIdx)
			_ = bResp.Body.Close()
			if wErr == nil {
				return nil
			}
			lastErr = wErr
		}
		return lastErr
	}

	for w := 0; w < workerCount; w++ {
		var fb io.ReadCloser
		if w == 0 && firstBlockBody != nil {
			fb = firstBlockBody
			firstBlockBody = nil
		}
		wg.Add(1)
		go func(initialReader io.ReadCloser) {
			defer wg.Done()
			if initialReader != nil {
				err0 := writeStreamToBlock(initialReader, 0)
				_ = initialReader.Close()
				if err0 != nil && workerCtx.Err() == nil {
					if retry0 := fetchBlockWithRetry(0); retry0 != nil {
						recordErr(retry0)
						return
					}
				} else if err0 != nil {
					recordErr(err0)
					return
				}
			}

			for {
				if workerCtx.Err() != nil {
					return
				}
				pos := int(nextIdx.Add(1) - 1)
				if pos >= len(pending) {
					return
				}
				if errB := fetchBlockWithRetry(pending[pos]); errB != nil {
					recordErr(errB)
					return
				}
			}
		}(fb)
	}

	wg.Wait()
	close(stopProgress)
	progWG.Wait()

	if ctx.Err() != nil {
		metaMu.Lock()
		saveTurboMeta(turboMetaPath, meta)
		metaMu.Unlock()
		return ctx.Err()
	}
	if firstErr != nil {
		metaMu.Lock()
		saveTurboMeta(turboMetaPath, meta)
		metaMu.Unlock()
		return firstErr
	}

	_ = file.Truncate(totalBytes)
	_ = file.Close()
	return finalizeDirectDownload(task, downloadDir, fileName, partPath, totalBytes, totalBytes)
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
