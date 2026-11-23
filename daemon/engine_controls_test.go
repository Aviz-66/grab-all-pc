package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBandwidthLimiterDynamicThrottleAndLiveUpdate(t *testing.T) {
	// 1. 256 KiB/s throttle for 192 KiB in 32 KiB chunks must pace cleanly.
	lim := newBandwidthLimiter(256 * 1024)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 6; i++ {
		if err := lim.Wait(ctx, 32*1024); err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 180*time.Millisecond {
		t.Errorf("expected throttled transfers to take >= 180ms, took %v", elapsed)
	}

	// 2. Raising rate to Unlimited (0) must unblock a deeply throttled waiter immediately.
	slowLim := newBandwidthLimiter(1024) // 1 KiB/s -> 64 KiB would take 64s
	unblocked := make(chan error, 1)
	go func() {
		unblocked <- slowLim.Wait(ctx, 64*1024)
	}()

	time.Sleep(25 * time.Millisecond)
	wakeStart := time.Now()
	slowLim.SetRate(0)

	select {
	case err := <-unblocked:
		if err != nil {
			t.Fatalf("expected nil on unblocked Wait, got %v", err)
		}
		if since := time.Since(wakeStart); since > 100*time.Millisecond {
			t.Errorf("expected SetRate(0) to wake waiter immediately (<100ms), took %v", since)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("Wait did not unblock after SetRate(0)")
	}

	// 3. Global + per-task API & tighter-cap resolution.
	tmpDir := t.TempDir()
	cfg, _ := LoadConfig(filepath.Join(tmpDir, "cfg.json"))
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "q.json"), nil)
	defer qm.Stop()
	srv := NewServer(cfg, qm, NewSSEBroker())
	handler := srv.setupRoutes()

	task, err := qm.AddTask(AddRequest{
		URL:    "https://example.com/archive.zip",
		Engine: EngineDirect,
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	qm.PauseTask(task.ID)

	// Set global cap to 2048 KB/s via POST /api/config
	storedTask := qm.GetByID(task.ID)
	cfgReq := httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"speedLimitKBps":2048}`))
	cfgReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	cfgRec := httptest.NewRecorder()
	handler.ServeHTTP(cfgRec, cfgReq)
	if cfgRec.Code != 200 {
		t.Fatalf("POST /api/config failed: %d (%s)", cfgRec.Code, cfgRec.Body.String())
	}
	if got := qm.EffectiveSpeedLimitKBps(storedTask); got != 2048 {
		t.Errorf("expected EffectiveSpeedLimitKBps=2048 from global cap, got %d", got)
	}

	// Set tighter per-task cap to 512 KB/s via POST /api/speed-limit
	limBody, _ := json.Marshal(map[string]interface{}{"id": task.ID, "speedLimitKBps": 512})
	limReq := httptest.NewRequest("POST", "/api/speed-limit", bytes.NewReader(limBody))
	limReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	limRec := httptest.NewRecorder()
	handler.ServeHTTP(limRec, limReq)
	if limRec.Code != 200 {
		t.Fatalf("POST /api/speed-limit failed: %d (%s)", limRec.Code, limRec.Body.String())
	}
	if got := qm.EffectiveSpeedLimitKBps(storedTask); got != 512 {
		t.Errorf("expected EffectiveSpeedLimitKBps=512 (tighter task cap), got %d", got)
	}
	if eff := effectiveLimitBytesPerSec(qm.TaskLimiters(storedTask)...); eff != 512*1024 {
		t.Errorf("expected effectiveLimitBytesPerSec=%d, got %d", 512*1024, eff)
	}

	// Raise task cap above global -> global cap wins
	qm.SetTaskSpeedLimitKBps(task.ID, 4096)
	if got := qm.EffectiveSpeedLimitKBps(storedTask); got != 2048 {
		t.Errorf("expected tighter global cap 2048 over task 4096, got %d", got)
	}
}

func TestAdaptiveHostConnectionPoolThrottleAndRecovery(t *testing.T) {
	// 1. Unit test halving, cooldown, floor=1, and 6-success recovery.
	pool := newHostConnectionPool(8)
	host := "cdn.example.com"
	if got := pool.EffectiveLimit(host); got != 8 {
		t.Fatalf("expected initial limit 8, got %d", got)
	}

	pool.ReportThrottle(host, 120*time.Millisecond)
	if got := pool.EffectiveLimit(host); got != 4 {
		t.Errorf("expected halved limit 4, got %d", got)
	}

	// Acquire must wait for cooldown
	acqStart := time.Now()
	rel, err := pool.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	rel()
	if waited := time.Since(acqStart); waited < 80*time.Millisecond {
		t.Errorf("expected Acquire to honor cooldown (~120ms), returned in %v", waited)
	}

	pool.ReportThrottle(host, 10*time.Millisecond) // 4 -> 2
	pool.ReportThrottle(host, 10*time.Millisecond) // 2 -> 1
	pool.ReportThrottle(host, 10*time.Millisecond) // floor stays 1
	if got := pool.EffectiveLimit(host); got != 1 {
		t.Errorf("expected floor limit 1, got %d", got)
	}

	// 6 consecutive clean successes recover limit 1 -> 2
	for i := 0; i < hostRecoveryStreak; i++ {
		pool.ReportSuccess(host)
	}
	if got := pool.EffectiveLimit(host); got != 2 {
		t.Errorf("expected recovered limit 2 after %d successes, got %d", hostRecoveryStreak, got)
	}

	if d := parseRetryAfter("3"); d != 3*time.Second {
		t.Errorf("parseRetryAfter(\"3\") = %v, want 3s", d)
	}
	if extractHostKey("https://RR1.Example.COM:443/video") != "rr1.example.com" {
		t.Errorf("unexpected extractHostKey result")
	}

	// 2. End-to-end 429 throttle adaptation inside fetchStreamWithGoEngine.
	const payloadSize = 2 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte((i*17 + 5) & 0xFF)
	}
	wantSum := sha256.Sum256(payload)

	var inFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		if cur > 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(15 * time.Millisecond)
		serveRangePayload(w, r, payload)
	}))
	defer srv.Close()

	dlPool := newHostConnectionPool(8)
	destPath := filepath.Join(t.TempDir(), "throttled.mp4")
	written, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:            srv.URL + "/throttled.mp4",
		DestPath:       destPath,
		ExpectedSize:   payloadSize,
		MaxWorkers:     6,
		BlockSize:      512 * 1024,
		MaxRequestSpan: 512 * 1024,
		DisablePreempt: true,
		HostPool:       dlPool,
	})
	if err != nil {
		t.Fatalf("fetchStreamWithGoEngine with 429 throttling failed: %v", err)
	}
	if written != payloadSize {
		t.Fatalf("expected written=%d, got %d", payloadSize, written)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != wantSum {
		t.Fatalf("SHA-256 mismatch after 429 recovery")
	}
	srvHost := extractHostKey(srv.URL)
	if lim := dlPool.EffectiveLimit(srvHost); lim > 4 {
		t.Errorf("expected adaptive pool limit for %s to shrink <= 4 after 429s, got %d", srvHost, lim)
	}
}

func TestDynamicWorkStealingSegmentation(t *testing.T) {
	// 1. Legacy DoneBlocks + CompletedRanges sidecar compatibility.
	meta := &turboStateMeta{
		TotalBytes: 1000,
		BlockSize:  400,
		DoneBlocks: []bool{true, false, false},
		CompletedRanges: []byteRange{
			{Start: 400, End: 499},
			{Start: 800, End: 999},
		},
	}
	if meta.contiguousPrefixBytes() != 500 {
		t.Errorf("expected contiguousPrefixBytes=500, got %d", meta.contiguousPrefixBytes())
	}
	if meta.completedBytes() != 700 {
		t.Errorf("expected completedBytes=700, got %d", meta.completedBytes())
	}

	// 2. Live work stealing: first half crawls while second half finishes fast -> fast workers steal slow tail!
	const payloadSize = 4 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte((i*41 + 9) & 0xFF)
	}
	wantSum := sha256.Sum256(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		spec := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := int64(payloadSize - 1)
		if len(parts) == 2 && parts[1] != "" {
			if pEnd, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				end = pEnd
			}
		}
		if end >= payloadSize {
			end = payloadSize - 1
		}
		slice := payload[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, payloadSize))
		w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
		w.WriteHeader(http.StatusPartialContent)

		if start < payloadSize/2 {
			// Drip slow first half in 32 KiB chunks so fast second-half workers catch up & steal
			for off := 0; off < len(slice); {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(25 * time.Millisecond):
				}
				n := 32 * 1024
				if off+n > len(slice) {
					n = len(slice) - off
				}
				_, _ = w.Write(slice[off : off+n])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				off += n
			}
			return
		}
		_, _ = w.Write(slice)
	}))
	defer srv.Close()

	var stealsCount, preemptsCount atomic.Int64
	destPath := filepath.Join(t.TempDir(), "stolen.bin")
	written, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:            srv.URL + "/stolen.bin",
		DestPath:       destPath,
		ExpectedSize:   payloadSize,
		MaxWorkers:     4,
		BlockSize:      1024 * 1024,
		MaxRequestSpan: 4 * 1024 * 1024,
		MinStealSize:   256 * 1024,
		DisablePreempt: true,
		OnStats: func(steals, preempts int64) {
			stealsCount.Store(steals)
			preemptsCount.Store(preempts)
		},
	})
	if err != nil {
		t.Fatalf("work-stealing download failed: %v", err)
	}
	if written != payloadSize {
		t.Fatalf("expected written=%d, got %d", payloadSize, written)
	}
	if stealsCount.Load() <= 0 {
		t.Errorf("expected steals > 0, got %d", stealsCount.Load())
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != wantSum {
		t.Fatalf("SHA-256 mismatch after work-stealing download")
	}
	if _, err := os.Stat(destPath + ".part.turbo"); !os.IsNotExist(err) {
		t.Errorf("expected .part.turbo sidecar to be removed on completion")
	}
}

func TestSlowConnectionPreemption(t *testing.T) {
	const payloadSize = 3 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte((i*53 + 13) & 0xFF)
	}
	wantSum := sha256.Sum256(payload)

	var midCrawlOnce atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		spec := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := int64(payloadSize - 1)
		if len(parts) == 2 && parts[1] != "" {
			if pEnd, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				end = pEnd
			}
		}
		if end >= payloadSize {
			end = payloadSize - 1
		}
		slice := payload[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, payloadSize))
		w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
		w.WriteHeader(http.StatusPartialContent)

		// First request targeting the mid-file [1MB, 2MB) segment crawls slowly until preempted;
		// the replacement connection opened after preemption streams at full speed!
		if start >= 1024*1024 && start < 2*1024*1024 && !midCrawlOnce.Swap(true) {
			for off := 0; off < len(slice); {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				n := 32 * 1024
				if off+n > len(slice) {
					n = len(slice) - off
				}
				_, _ = w.Write(slice[off : off+n])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				off += n
			}
			return
		}

		_, _ = w.Write(slice)
	}))
	defer srv.Close()

	var preemptsCount atomic.Int64
	destPath := filepath.Join(t.TempDir(), "preempted.bin")
	dlStart := time.Now()
	written, err := fetchStreamWithGoEngine(context.Background(), streamFetchOptions{
		URL:                  srv.URL + "/preempted.bin",
		DestPath:             destPath,
		ExpectedSize:         payloadSize,
		MaxWorkers:           3,
		BlockSize:            1024 * 1024,
		MaxRequestSpan:       4 * 1024 * 1024,
		MinStealSize:         2 * 1024 * 1024, // Force preemption rather than steal
		PreemptGrace:         150 * time.Millisecond,
		PreemptCheckInterval: 80 * time.Millisecond,
		OnStats: func(steals, preempts int64) {
			preemptsCount.Store(preempts)
		},
	})
	dlElapsed := time.Since(dlStart)

	if err != nil {
		t.Fatalf("slow-connection preemption download failed: %v", err)
	}
	if written != payloadSize {
		t.Fatalf("expected written=%d, got %d", payloadSize, written)
	}
	if preemptsCount.Load() <= 0 {
		t.Errorf("expected preemptions > 0, got %d", preemptsCount.Load())
	}
	if dlElapsed >= 3*time.Second {
		t.Errorf("expected preempted download to finish quickly (<3s), took %v", dlElapsed)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != wantSum {
		t.Fatalf("SHA-256 mismatch after slow-connection preemption")
	}
}
