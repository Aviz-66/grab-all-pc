package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxConnsPerHost = 8
	minConnsPerHost        = 1
	maxConnsPerHost        = 12
	hostRecoveryStreak     = 6

	defaultMinSegmentSize = 256 * 1024 // 256 KiB minimum segment floor
	defaultMinStealSize   = 512 * 1024 // 512 KiB minimum work-steal split unit
	defaultPreemptGrace   = 6 * time.Second
	defaultPreemptCheck   = 1 * time.Second
	maxSegmentPreemptions = 4
)

// --- 1A. Dynamic Token-Bucket Speed Limiter ---

// bandwidthLimiter enforces a configurable bytes-per-second rate cap (0 = unlimited)
// across one or more worker goroutines and wakes sleepers immediately on live rate increases.
type bandwidthLimiter struct {
	rateBytesPerSec atomic.Int64
	mu              sync.Mutex
	tokens          float64
	lastRefill      time.Time
	notifyCh        chan struct{}
}

func newBandwidthLimiter(bytesPerSec int64) *bandwidthLimiter {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	l := &bandwidthLimiter{
		lastRefill: time.Now(),
		notifyCh:   make(chan struct{}),
	}
	l.rateBytesPerSec.Store(bytesPerSec)
	return l
}

// Rate returns the current rate limit in bytes/sec (0 = unlimited).
func (l *bandwidthLimiter) Rate() int64 {
	if l == nil {
		return 0
	}
	return l.rateBytesPerSec.Load()
}

// SetRate updates the rate limit dynamically and wakes any sleeping workers.
func (l *bandwidthLimiter) SetRate(bytesPerSec int64) {
	if l == nil {
		return
	}
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	l.rateBytesPerSec.Store(bytesPerSec)

	l.mu.Lock()
	if l.tokens < 0 {
		l.tokens = 0
	}
	l.lastRefill = time.Now()
	if l.notifyCh != nil {
		close(l.notifyCh)
	}
	l.notifyCh = make(chan struct{})
	l.mu.Unlock()
}

// Wait blocks until n bytes can be consumed under the configured rate limit or ctx is canceled.
func (l *bandwidthLimiter) Wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 || l.rateBytesPerSec.Load() <= 0 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	l.mu.Lock()
	rate := float64(l.rateBytesPerSec.Load())
	if rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if l.lastRefill.IsZero() {
		l.lastRefill = now
	}
	elapsed := now.Sub(l.lastRefill).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * rate
		l.lastRefill = now
	}
	maxBurst := math.Max(math.Min(rate*0.25, 2*1024*1024), 32*1024)
	if l.tokens > maxBurst {
		l.tokens = maxBurst
	}
	l.tokens -= float64(n)
	if l.tokens >= 0 {
		l.mu.Unlock()
		return nil
	}

	waitDur := time.Duration((-l.tokens / rate) * float64(time.Second))
	ch := l.notifyCh
	l.mu.Unlock()

	if waitDur <= 0 {
		return nil
	}
	timer := time.NewTimer(waitDur)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ch:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			newRate := l.rateBytesPerSec.Load()
			if newRate <= 0 {
				return nil
			}
			// Recompute wait based on the updated rate so raising a tight limit takes effect immediately.
			l.mu.Lock()
			ch = l.notifyCh
			l.mu.Unlock()
			newWait := time.Duration((float64(n) / float64(newRate)) * float64(time.Second))
			if newWait < waitDur {
				waitDur = newWait
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				if waitDur <= 0 {
					return nil
				}
				timer.Reset(waitDur)
			}
		}
	}
}

func waitRateLimiters(ctx context.Context, n int, limiters ...*bandwidthLimiter) error {
	for _, lim := range limiters {
		if lim == nil {
			continue
		}
		if err := lim.Wait(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func effectiveLimitBytesPerSec(limiters ...*bandwidthLimiter) int64 {
	var minPos int64
	for _, lim := range limiters {
		if lim == nil {
			continue
		}
		r := lim.Rate()
		if r > 0 && (minPos == 0 || r < minPos) {
			minPos = r
		}
	}
	return minPos
}

// --- 1B. Adaptive Per-Host Connection Pool ---

type hostLimitState struct {
	active        int
	limit         int
	successStreak int
	cooldownUntil time.Time
}

// hostConnectionPool coordinates per-host concurrency, backs off automatically on HTTP 429/503,
// and gradually recovers connection capacity after sustained healthy transfers.
type hostConnectionPool struct {
	mu           sync.Mutex
	defaultLimit int
	hosts        map[string]*hostLimitState
	changed      chan struct{}
}

var defaultHostPool = newHostConnectionPool(defaultMaxConnsPerHost)

func newHostConnectionPool(defaultLimit int) *hostConnectionPool {
	if defaultLimit < minConnsPerHost {
		defaultLimit = defaultMaxConnsPerHost
	}
	if defaultLimit > maxConnsPerHost {
		defaultLimit = maxConnsPerHost
	}
	return &hostConnectionPool{
		defaultLimit: defaultLimit,
		hosts:        make(map[string]*hostLimitState),
		changed:      make(chan struct{}),
	}
}

func extractHostKey(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "default"
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return "default"
	}
	return host
}

func parseRetryAfter(hdr string) time.Duration {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(hdr, 10, 64); err == nil {
		d := time.Duration(secs) * time.Second
		if d <= 0 {
			return 250 * time.Millisecond
		}
		if d > 30*time.Second {
			return 30 * time.Second
		}
		return d
	}
	if t, err := http.ParseTime(hdr); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 250 * time.Millisecond
		}
		if d > 30*time.Second {
			return 30 * time.Second
		}
		return d
	}
	return 0
}

func (p *hostConnectionPool) stateLocked(host string) *hostLimitState {
	if host == "" {
		host = "default"
	}
	st, ok := p.hosts[host]
	if !ok {
		st = &hostLimitState{limit: p.defaultLimit}
		p.hosts[host] = st
	}
	return st
}

func (p *hostConnectionPool) broadcastLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Acquire waits for cooldown expiry and an available per-host slot, returning a release callback.
func (p *hostConnectionPool) Acquire(ctx context.Context, host string) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	if host == "" {
		host = "default"
	}

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		p.mu.Lock()
		st := p.stateLocked(host)
		now := time.Now()

		if !st.cooldownUntil.IsZero() && now.Before(st.cooldownUntil) {
			waitDur := st.cooldownUntil.Sub(now)
			ch := p.changed
			p.mu.Unlock()

			timer := time.NewTimer(waitDur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-ch:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}

		if st.active < st.limit {
			st.active++
			var once sync.Once
			release := func() {
				once.Do(func() {
					p.mu.Lock()
					if s, ok := p.hosts[host]; ok && s.active > 0 {
						s.active--
					}
					p.broadcastLocked()
					p.mu.Unlock()
				})
			}
			p.mu.Unlock()
			return release, nil
		}

		ch := p.changed
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// ReportThrottle halves the host concurrency limit and applies a shared host cooldown.
func (p *hostConnectionPool) ReportThrottle(host string, retryAfter time.Duration) {
	if p == nil {
		return
	}
	if retryAfter <= 0 {
		retryAfter = 2 * time.Second
	}
	p.mu.Lock()
	st := p.stateLocked(host)
	newLim := st.limit / 2
	if newLim < minConnsPerHost {
		newLim = minConnsPerHost
	}
	st.limit = newLim
	st.successStreak = 0
	until := time.Now().Add(retryAfter)
	if until.After(st.cooldownUntil) {
		st.cooldownUntil = until
	}
	p.broadcastLocked()
	p.mu.Unlock()
}

// ReportSuccess records a clean response and slowly restores concurrency up to defaultLimit.
func (p *hostConnectionPool) ReportSuccess(host string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	st := p.stateLocked(host)
	if st.limit < p.defaultLimit {
		st.successStreak++
		if st.successStreak >= hostRecoveryStreak {
			st.limit++
			st.successStreak = 0
			p.broadcastLocked()
		}
	}
	p.mu.Unlock()
}

// EffectiveLimit returns the current concurrency cap for host.
func (p *hostConnectionPool) EffectiveLimit(host string) int {
	if p == nil {
		return defaultMaxConnsPerHost
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.hosts[host]; ok {
		return st.limit
	}
	return p.defaultLimit
}

// --- 1C. Byte-Range Interval Helpers & Work-Stealing + Slow-Preemption Scheduler ---

type byteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func mergeByteRanges(ranges []byteRange, totalBytes int64) []byteRange {
	if totalBytes <= 0 || len(ranges) == 0 {
		return nil
	}
	valid := make([]byteRange, 0, len(ranges))
	for _, r := range ranges {
		s := r.Start
		e := r.End
		if s < 0 {
			s = 0
		}
		if e >= totalBytes {
			e = totalBytes - 1
		}
		if s <= e {
			valid = append(valid, byteRange{Start: s, End: e})
		}
	}
	if len(valid) == 0 {
		return nil
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].Start != valid[j].Start {
			return valid[i].Start < valid[j].Start
		}
		return valid[i].End < valid[j].End
	})

	merged := make([]byteRange, 0, len(valid))
	cur := valid[0]
	for _, next := range valid[1:] {
		if next.Start <= cur.End+1 {
			if next.End > cur.End {
				cur.End = next.End
			}
		} else {
			merged = append(merged, cur)
			cur = next
		}
	}
	merged = append(merged, cur)
	return merged
}

func invertByteRanges(completed []byteRange, totalBytes int64) []byteRange {
	if totalBytes <= 0 {
		return nil
	}
	merged := mergeByteRanges(completed, totalBytes)
	var missing []byteRange
	var cursor int64
	for _, r := range merged {
		if r.Start > cursor {
			missing = append(missing, byteRange{Start: cursor, End: r.Start - 1})
		}
		if r.End+1 > cursor {
			cursor = r.End + 1
		}
	}
	if cursor < totalBytes {
		missing = append(missing, byteRange{Start: cursor, End: totalBytes - 1})
	}
	return missing
}

func countRangeBytes(ranges []byteRange) int64 {
	var sum int64
	for _, r := range ranges {
		if r.End >= r.Start {
			sum += r.End - r.Start + 1
		}
	}
	return sum
}

type activeSegment struct {
	id            int
	start         int64
	current       int64
	end           int64
	assigned      bool
	done          bool
	owner         int
	startedAt     time.Time
	lastSampleAt  time.Time
	lastSamplePos int64
	emaBps        float64
	reqCancel     context.CancelFunc
	preempted     atomic.Bool
	preemptCount  int
}

type rangeScheduler struct {
	mu             sync.Mutex
	totalBytes     int64
	completed      []byteRange
	segments       []*activeSegment
	completedRates []float64
	nextSegID      int
	steals         atomic.Int64
	preemptions    atomic.Int64
}

func align4KiB(v int64) int64 {
	const page = 4096
	return ((v + page - 1) / page) * page
}

func newRangeScheduler(totalBytes int64, completed []byteRange, targetWorkers int, minSegmentSize int64) *rangeScheduler {
	return newRangeSchedulerWithReserved(totalBytes, completed, nil, targetWorkers, minSegmentSize)
}

func newRangeSchedulerWithReserved(totalBytes int64, completed []byteRange, reserved []byteRange, targetWorkers int, minSegmentSize int64) *rangeScheduler {
	if targetWorkers < 1 {
		targetWorkers = 1
	}
	if minSegmentSize <= 0 {
		minSegmentSize = defaultMinSegmentSize
	}

	mergedDone := mergeByteRanges(completed, totalBytes)
	occupied := make([]byteRange, 0, len(mergedDone)+len(reserved))
	occupied = append(occupied, mergedDone...)
	occupied = append(occupied, reserved...)
	missing := invertByteRanges(occupied, totalBytes)

	s := &rangeScheduler{
		totalBytes: totalBytes,
		completed:  mergedDone,
	}

	totalMissing := countRangeBytes(missing)
	if totalMissing <= 0 {
		return s
	}

	targetSegSize := totalMissing / int64(targetWorkers)
	if targetSegSize < minSegmentSize {
		targetSegSize = minSegmentSize
	}

	for _, gap := range missing {
		gapLen := gap.End - gap.Start + 1
		nParts := int(gapLen / targetSegSize)
		if nParts < 1 {
			nParts = 1
		}
		if nParts > targetWorkers {
			nParts = targetWorkers
		}

		// Keep segments above minSegmentSize.
		for nParts > 1 && gapLen/int64(nParts) < minSegmentSize {
			nParts--
		}

		pos := gap.Start
		for p := 0; p < nParts && pos <= gap.End; p++ {
			if p == nParts-1 {
				s.segments = append(s.segments, &activeSegment{
					id:      s.nextSegID,
					start:   pos,
					current: pos,
					end:     gap.End,
				})
				s.nextSegID++
				break
			}

			step := gapLen / int64(nParts)
			candEnd := align4KiB(pos+step) - 1
			if candEnd < pos+minSegmentSize-1 {
				candEnd = pos + minSegmentSize - 1
			}
			remAfter := gap.End - candEnd
			if remAfter < minSegmentSize/2 {
				candEnd = gap.End
			}
			if candEnd > gap.End {
				candEnd = gap.End
			}

			s.segments = append(s.segments, &activeSegment{
				id:      s.nextSegID,
				start:   pos,
				current: pos,
				end:     candEnd,
			})
			s.nextSegID++
			pos = candEnd + 1
		}
	}

	return s
}

func (s *rangeScheduler) AssignReserved(workerID int, start, end int64) *activeSegment {
	if end >= s.totalBytes {
		end = s.totalBytes - 1
	}
	if start > end || start < 0 || s.totalBytes <= 0 {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	seg := &activeSegment{
		id:            s.nextSegID,
		start:         start,
		current:       start,
		end:           end,
		assigned:      true,
		owner:         workerID,
		startedAt:     now,
		lastSampleAt:  now,
		lastSamplePos: start,
	}
	s.nextSegID++
	s.segments = append(s.segments, seg)
	return seg
}

func (s *rangeScheduler) NextOrSteal(workerID int, minStealSize int64) *activeSegment {
	if minStealSize < 64*1024 {
		minStealSize = 64 * 1024
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Pick lowest-start unassigned segment.
	var bestUnassigned *activeSegment
	for _, seg := range s.segments {
		if !seg.done && !seg.assigned && seg.current <= seg.end {
			if bestUnassigned == nil || seg.start < bestUnassigned.start {
				bestUnassigned = seg
			}
		}
	}
	if bestUnassigned != nil {
		bestUnassigned.assigned = true
		bestUnassigned.owner = workerID
		bestUnassigned.startedAt = now
		bestUnassigned.lastSampleAt = now
		bestUnassigned.lastSamplePos = bestUnassigned.current
		bestUnassigned.emaBps = 0
		bestUnassigned.preempted.Store(false)
		return bestUnassigned
	}

	// 2. Steal trailing half from the active segment with largest remaining span.
	var victim *activeSegment
	var maxRem int64
	for _, seg := range s.segments {
		if seg.done || !seg.assigned || seg.current > seg.end {
			continue
		}
		rem := seg.end - seg.current + 1
		if rem >= 2*minStealSize && rem > maxRem {
			victim = seg
			maxRem = rem
		}
	}
	if victim == nil {
		return nil
	}

	mid := align4KiB(victim.current + maxRem/2)
	// Ensure safe headroom past victim.current and >= minStealSize on both halves.
	if mid < victim.current+64*1024 {
		mid = align4KiB(victim.current + minStealSize)
	}
	if mid > victim.end-minStealSize+1 || mid <= victim.current {
		return nil
	}

	oldEnd := victim.end
	victim.end = mid - 1

	stolen := &activeSegment{
		id:            s.nextSegID,
		start:         mid,
		current:       mid,
		end:           oldEnd,
		assigned:      true,
		owner:         workerID,
		startedAt:     now,
		lastSampleAt:  now,
		lastSamplePos: mid,
	}
	s.nextSegID++
	s.segments = append(s.segments, stolen)
	s.steals.Add(1)
	return stolen
}

func (s *rangeScheduler) SegmentBounds(seg *activeSegment) (cur, end int64) {
	s.mu.Lock()
	cur = seg.current
	end = seg.end
	s.mu.Unlock()
	return cur, end
}

func (s *rangeScheduler) ClampWritable(seg *activeSegment, n int64) int64 {
	if n <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seg.current > seg.end {
		return 0
	}
	rem := seg.end - seg.current + 1
	if n > rem {
		return rem
	}
	return n
}

func (s *rangeScheduler) Advance(seg *activeSegment, n int64) (segmentDone bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	seg.current += n
	if seg.lastSampleAt.IsZero() {
		seg.lastSampleAt = now
		seg.lastSamplePos = seg.current
	} else {
		dt := now.Sub(seg.lastSampleAt).Seconds()
		sampleBytes := seg.current - seg.lastSamplePos
		if (seg.emaBps == 0 && dt >= 0.05 && sampleBytes > 0) || dt >= 0.25 {
			inst := float64(sampleBytes) / dt
			if seg.emaBps == 0 {
				seg.emaBps = inst
			} else {
				seg.emaBps = seg.emaBps*0.6 + inst*0.4
			}
			seg.lastSampleAt = now
			seg.lastSamplePos = seg.current
		}
	}

	if seg.current > seg.end {
		seg.done = true
		seg.assigned = false
		seg.reqCancel = nil
		s.completed = mergeByteRanges(append(s.completed, byteRange{Start: seg.start, End: seg.end}), s.totalBytes)

		totalDone := seg.end - seg.start + 1
		if totalDone >= 128*1024 && !seg.startedAt.IsZero() {
			el := now.Sub(seg.startedAt).Seconds()
			if el < 0.001 {
				el = 0.001
			}
			rate := float64(totalDone) / el
			s.completedRates = append(s.completedRates, rate)
			if len(s.completedRates) > 16 {
				s.completedRates = s.completedRates[len(s.completedRates)-16:]
			}
		}
		return true
	}
	return false
}

func (s *rangeScheduler) SetRequestCancel(seg *activeSegment, cancel context.CancelFunc) {
	s.mu.Lock()
	seg.reqCancel = cancel
	s.mu.Unlock()
}

func (s *rangeScheduler) Release(seg *activeSegment) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seg.reqCancel = nil
	seg.assigned = false
	seg.owner = -1

	if seg.current > seg.start {
		doneEnd := seg.current - 1
		if doneEnd > seg.end {
			doneEnd = seg.end
		}
		s.completed = mergeByteRanges(append(s.completed, byteRange{Start: seg.start, End: doneEnd}), s.totalBytes)
		seg.start = seg.current
	}
	if seg.current > seg.end {
		seg.done = true
	}
}

func (s *rangeScheduler) CheckPreemptSlow(gracePeriod time.Duration, minRemBytes int64, speedLimitBps int64) int {
	if minRemBytes <= 0 {
		minRemBytes = 256 * 1024
	}
	now := time.Now()

	s.mu.Lock()
	active := make([]*activeSegment, 0, len(s.segments))
	for _, seg := range s.segments {
		if !seg.done && seg.assigned && seg.current <= seg.end {
			active = append(active, seg)
		}
	}
	peerCount := len(active)
	if peerCount == 0 {
		s.mu.Unlock()
		return 0
	}
	effectivePeers := peerCount + len(s.completedRates)
	if effectivePeers < 2 {
		s.mu.Unlock()
		return 0
	}
	if speedLimitBps > 0 && speedLimitBps < int64(effectivePeers)*128*1024 {
		s.mu.Unlock()
		return 0
	}

	rates := make([]float64, 0, len(active)+len(s.completedRates))
	rates = append(rates, s.completedRates...)
	for _, seg := range active {
		if seg.emaBps > 0 {
			rates = append(rates, seg.emaBps)
		}
	}
	if len(rates) == 0 {
		s.mu.Unlock()
		return 0
	}
	sort.Float64s(rates)
	medianBps := rates[len(rates)/2]

	var (
		victimSeg    *activeSegment
		victimCancel context.CancelFunc
	)
	for _, seg := range active {
		age := now.Sub(seg.startedAt)
		rem := seg.end - seg.current + 1
		if age < gracePeriod || rem < minRemBytes || seg.preemptCount >= maxSegmentPreemptions || seg.reqCancel == nil {
			continue
		}
		isSlow := (medianBps >= 64*1024 && seg.emaBps < medianBps*0.20) || (medianBps > 0 && seg.emaBps == 0)
		if isSlow {
			if victimSeg == nil || seg.emaBps < victimSeg.emaBps {
				victimSeg = seg
			}
		}
	}
	if victimSeg != nil {
		victimSeg.preempted.Store(true)
		victimSeg.preemptCount++
		victimCancel = victimSeg.reqCancel
		s.preemptions.Add(1)
	}
	s.mu.Unlock()

	if victimCancel != nil {
		victimCancel()
		return 1
	}
	return 0
}

func (s *rangeScheduler) SnapshotMeta() *turboStateMeta {
	s.mu.Lock()
	defer s.mu.Unlock()

	all := make([]byteRange, 0, len(s.completed)+len(s.segments))
	all = append(all, s.completed...)
	for _, seg := range s.segments {
		if seg.current > seg.start {
			doneEnd := seg.current - 1
			if doneEnd > seg.end {
				doneEnd = seg.end
			}
			all = append(all, byteRange{Start: seg.start, End: doneEnd})
		}
	}
	return &turboStateMeta{
		TotalBytes:      s.totalBytes,
		CompletedRanges: mergeByteRanges(all, s.totalBytes),
	}
}

func (s *rangeScheduler) CompletedBytes() int64 {
	return countRangeBytes(s.SnapshotMeta().CompletedRanges)
}

func (s *rangeScheduler) IsComplete() bool {
	return len(invertByteRanges(s.SnapshotMeta().CompletedRanges, s.totalBytes)) == 0
}

func (s *rangeScheduler) Stats() (steals, preemptions int64) {
	return s.steals.Load(), s.preemptions.Load()
}

// SnapshotSegments returns a sorted, capped view of completed and active byte segments
// for per-connection UI telemetry.
func (s *rangeScheduler) SnapshotSegments() []SegmentProgress {
	s.mu.Lock()
	defer s.mu.Unlock()

	active := make([]SegmentProgress, 0, len(s.segments))
	pending := make([]SegmentProgress, 0, len(s.segments))
	for _, seg := range s.segments {
		if seg.done || seg.start > seg.end {
			continue
		}
		item := SegmentProgress{
			Start:   seg.start,
			Current: seg.current,
			End:     seg.end,
			Active:  seg.assigned,
			RateBps: seg.emaBps,
		}
		if seg.assigned {
			active = append(active, item)
		} else {
			pending = append(pending, item)
		}
	}

	done := make([]SegmentProgress, 0, len(s.completed))
	for _, r := range s.completed {
		done = append(done, SegmentProgress{
			Start:   r.Start,
			Current: r.End + 1,
			End:     r.End,
			Active:  false,
		})
	}

	const maxSegments = 24
	out := make([]SegmentProgress, 0, maxSegments)
	for _, item := range active {
		if len(out) >= maxSegments {
			break
		}
		out = append(out, item)
	}
	for _, item := range done {
		if len(out) >= maxSegments {
			break
		}
		out = append(out, item)
	}
	for _, item := range pending {
		if len(out) >= maxSegments {
			break
		}
		out = append(out, item)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Start < out[j].Start
	})
	return out
}

// --- 1D. Unified Work-Stealing Parallel Downloader ---

type workStealingOptions struct {
	Client               *http.Client
	Endpoint             *streamEndpointState
	File                 *os.File
	TotalBytes           int64
	InitialRanges        []byteRange
	TurboMetaPath        string
	MaxWorkers           int
	MaxRequestSpan       int64
	MinSegmentSize       int64
	MinStealSize         int64
	StallTimeout         time.Duration
	PreemptGrace         time.Duration
	PreemptCheckInterval time.Duration
	DisablePreempt       bool
	HostPool             *hostConnectionPool
	Limiters             []*bandwidthLimiter

	ProbeBody     io.ReadCloser
	ProbeCtx      context.Context
	ProbeWatchdog *stallWatchdog
	ProbeStart    int64
	ProbeEnd      int64

	OnProgress func(downloaded, total int64, rateBytesPerSec float64, workers int)
	OnSegments func(segs []SegmentProgress)
	OnStats    func(steals, preemptions int64)
}

func runWorkStealingDownload(ctx context.Context, opts workStealingOptions) error {
	if opts.TotalBytes <= 0 || opts.File == nil || opts.Endpoint == nil {
		return fmt.Errorf("invalid work-stealing options")
	}
	if opts.MaxWorkers < 1 {
		opts.MaxWorkers = turboWorkers
	}
	if opts.MinSegmentSize <= 0 {
		opts.MinSegmentSize = defaultMinSegmentSize
	}
	if opts.MinStealSize <= 0 {
		opts.MinStealSize = defaultMinStealSize
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 30 * time.Second
	}
	if opts.PreemptGrace <= 0 {
		opts.PreemptGrace = defaultPreemptGrace
	}
	if opts.PreemptCheckInterval <= 0 {
		opts.PreemptCheckInterval = defaultPreemptCheck
	}
	if opts.HostPool == nil {
		opts.HostPool = defaultHostPool
	}
	if opts.Client == nil {
		opts.Client = &http.Client{
			Transport: directHTTPTransport,
			Timeout:   0,
		}
	}

	var reserved []byteRange
	hasProbe := opts.ProbeBody != nil && opts.ProbeEnd >= opts.ProbeStart
	if hasProbe {
		reserved = []byteRange{{Start: opts.ProbeStart, End: opts.ProbeEnd}}
	}

	sched := newRangeSchedulerWithReserved(opts.TotalBytes, opts.InitialRanges, reserved, opts.MaxWorkers, opts.MinSegmentSize)
	if !hasProbe && sched.IsComplete() {
		return nil
	}

	workerCount := opts.MaxWorkers
	if !hasProbe {
		missingCount := len(sched.segments)
		if missingCount == 0 {
			return nil
		}
		if missingCount < workerCount && countRangeBytes(invertByteRanges(opts.InitialRanges, opts.TotalBytes)) < 2*opts.MinStealSize {
			workerCount = missingCount
		}
	}
	if workerCount < 1 {
		workerCount = 1
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	initialDone := sched.CompletedBytes()
	var (
		metaMu    sync.Mutex
		totalPtr  atomic.Int64
		liveBytes atomic.Int64
		errOnce   sync.Once
		firstErr  error
		wg        sync.WaitGroup
	)
	totalPtr.Store(opts.TotalBytes)
	liveBytes.Store(initialDone)

	saveMetaSafe := func() {
		if opts.TurboMetaPath == "" {
			return
		}
		snap := sched.SnapshotMeta()
		metaMu.Lock()
		saveTurboMeta(opts.TurboMetaPath, snap)
		metaMu.Unlock()
	}

	recordErr := func(e error) {
		if e == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = e
			cancelWorkers()
		})
	}

	progCb := opts.OnProgress
	if opts.OnSegments != nil {
		progCb = func(downloaded, total int64, rate float64, workers int) {
			opts.OnSegments(sched.SnapshotSegments())
			if opts.OnProgress != nil {
				opts.OnProgress(downloaded, total, rate, workers)
			}
		}
	}

	stopTicker := startStreamProgressTicker(&totalPtr, &liveBytes, initialDone, workerCount, progCb, saveMetaSafe)
	defer stopTicker()

	var (
		stopPreemptOnce sync.Once
		preemptStop     = make(chan struct{})
		preemptWG       sync.WaitGroup
	)
	stopPreempt := func() {
		stopPreemptOnce.Do(func() {
			close(preemptStop)
		})
		preemptWG.Wait()
	}
	if !opts.DisablePreempt && opts.MaxWorkers > 1 {
		preemptWG.Add(1)
		go func() {
			defer preemptWG.Done()
			ticker := time.NewTicker(opts.PreemptCheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-preemptStop:
					return
				case <-workerCtx.Done():
					return
				case <-ticker.C:
					limitBps := effectiveLimitBytesPerSec(opts.Limiters...)
					sched.CheckPreemptSlow(opts.PreemptGrace, 256*1024, limitBps)
				}
			}
		}()
	}
	defer stopPreempt()

	streamBodyIntoSegment := func(seg *activeSegment, reqCtx context.Context, body io.Reader, watchdog *stallWatchdog, startOff, reqEnd int64) (hitEnd bool, preempted bool, err error) {
		buf := make([]byte, 32*1024)
		streamPos := startOff
		for streamPos <= reqEnd {
			if workerCtx.Err() != nil {
				return false, false, workerCtx.Err()
			}
			if seg.preempted.Load() {
				return false, true, nil
			}
			if reqCtx.Err() != nil {
				if seg.preempted.Load() {
					return false, true, nil
				}
				if workerCtx.Err() != nil {
					return false, false, workerCtx.Err()
				}
				return false, false, fmt.Errorf("stream stalled at byte %d", streamPos)
			}

			toRead := len(buf)
			if rem := int(reqEnd - streamPos + 1); rem < toRead {
				toRead = rem
			}
			n, rErr := body.Read(buf[:toRead])
			if n > 0 {
				if watchdog != nil {
					watchdog.Kick()
				}
				allowed := sched.ClampWritable(seg, int64(n))
				if allowed > 0 {
					if _, wErr := opts.File.WriteAt(buf[:allowed], streamPos); wErr != nil {
						return false, false, fmt.Errorf("disk write error: %w", wErr)
					}
					streamPos += allowed
					liveBytes.Add(allowed)
					segDone := sched.Advance(seg, allowed)
					if len(opts.Limiters) > 0 {
						if watchdog != nil {
							watchdog.Kick()
						}
						if lErr := waitRateLimiters(reqCtx, int(allowed), opts.Limiters...); lErr != nil {
							if seg.preempted.Load() {
								return false, true, nil
							}
							return false, false, lErr
						}
						if watchdog != nil {
							watchdog.Kick()
						}
					}
					if segDone {
						return true, false, nil
					}
				}
				if allowed < int64(n) {
					// Victim reached stolen segment end cleanly.
					return true, false, nil
				}
			}

			if rErr != nil {
				if seg.preempted.Load() {
					return false, true, nil
				}
				if rErr == io.EOF && streamPos > reqEnd {
					return true, false, nil
				}
				if rErr == io.EOF {
					return false, false, fmt.Errorf("unexpected EOF at byte %d (expected %d)", streamPos, reqEnd)
				}
				return false, false, rErr
			}
		}
		return true, false, nil
	}

	downloadSegment := func(seg *activeSegment, initCtx context.Context, initBody io.ReadCloser, initDog *stallWatchdog) error {
		netAttempt := 0
		throttleAttempt := 0
		const maxNetAttempts = 6
		const maxThrottleAttempts = 8

		if initBody != nil {
			sched.SetRequestCancel(seg, func() {
				if initDog != nil {
					initDog.Stop()
				}
			})
			stopWorkerCancel := context.AfterFunc(workerCtx, func() {
				if initDog != nil {
					initDog.Stop()
				}
			})
			hitEnd, preempted, sErr := streamBodyIntoSegment(seg, initCtx, initBody, initDog, opts.ProbeStart, opts.ProbeEnd)
			stopWorkerCancel()
			sched.SetRequestCancel(seg, nil)
			_ = initBody.Close()
			if initDog != nil {
				initDog.Stop()
			}
			initBody = nil

			if preempted {
				sched.Release(seg)
				return nil
			}
			if sErr == nil && hitEnd {
				cur, end := sched.SegmentBounds(seg)
				if cur > end {
					return nil
				}
			}
			if workerCtx.Err() != nil {
				sched.Release(seg)
				return workerCtx.Err()
			}
		}

		for {
			if workerCtx.Err() != nil {
				sched.Release(seg)
				return workerCtx.Err()
			}
			if seg.preempted.Load() {
				sched.Release(seg)
				return nil
			}

			cur, end := sched.SegmentBounds(seg)
			if cur > end {
				return nil
			}

			reqEnd := end
			if opts.MaxRequestSpan > 0 && reqEnd-cur+1 > opts.MaxRequestSpan {
				reqEnd = cur + opts.MaxRequestSpan - 1
			}
			rangeStr := fmt.Sprintf("bytes=%d-%d", cur, reqEnd)

			curURL, curHeaders, gen := opts.Endpoint.current()
			hostKey := extractHostKey(curURL)
			release, acqErr := opts.HostPool.Acquire(workerCtx, hostKey)
			if acqErr != nil {
				sched.Release(seg)
				return acqErr
			}

			reqCtx, watchdog := newStallWatchdog(workerCtx, opts.StallTimeout)
			req, rErr := buildStreamHTTPRequest(reqCtx, curURL, curHeaders, rangeStr)
			if rErr != nil {
				watchdog.Stop()
				release()
				sched.Release(seg)
				return rErr
			}

			sched.SetRequestCancel(seg, watchdog.cancel)
			if seg.preempted.Load() {
				sched.SetRequestCancel(seg, nil)
				watchdog.Stop()
				release()
				sched.Release(seg)
				return nil
			}

			resp, dErr := opts.Client.Do(req)
			if dErr != nil {
				sched.SetRequestCancel(seg, nil)
				watchdog.Stop()
				release()
				if seg.preempted.Load() {
					sched.Release(seg)
					return nil
				}
				if workerCtx.Err() != nil {
					sched.Release(seg)
					return workerCtx.Err()
				}
				netAttempt++
				if netAttempt >= 3 && opts.Endpoint.refreshFn != nil {
					_ = opts.Endpoint.tryRefresh(workerCtx, gen, 0, dErr.Error(), saveMetaSafe)
				}
				if netAttempt >= maxNetAttempts {
					sched.Release(seg)
					return dErr
				}
				backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				select {
				case <-workerCtx.Done():
					sched.Release(seg)
					return workerCtx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
				retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
				code := resp.StatusCode
				_ = resp.Body.Close()
				sched.SetRequestCancel(seg, nil)
				watchdog.Stop()
				opts.HostPool.ReportThrottle(hostKey, retryAfter)
				release()
				throttleAttempt++
				if throttleAttempt > maxThrottleAttempts {
					sched.Release(seg)
					return &httpStatusError{
						StatusCode: code,
						Message:    fmt.Sprintf("host throttled with HTTP %d", code),
					}
				}
				continue
			}

			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
				code := resp.StatusCode
				_ = resp.Body.Close()
				sched.SetRequestCancel(seg, nil)
				watchdog.Stop()
				release()
				if refErr := opts.Endpoint.tryRefresh(workerCtx, gen, code, fmt.Sprintf("segment returned HTTP %d", code), saveMetaSafe); refErr != nil {
					sched.Release(seg)
					return refErr
				}
				continue
			}

			if resp.StatusCode != http.StatusPartialContent {
				code := resp.StatusCode
				_ = resp.Body.Close()
				sched.SetRequestCancel(seg, nil)
				watchdog.Stop()
				release()
				lastErr := &httpStatusError{
					StatusCode: code,
					Message:    fmt.Sprintf("segment returned HTTP %d", code),
				}
				netAttempt++
				if netAttempt >= maxNetAttempts {
					sched.Release(seg)
					return lastErr
				}
				backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				select {
				case <-workerCtx.Done():
					sched.Release(seg)
					return workerCtx.Err()
				case <-time.After(backoff):
				}
				continue
			}

			hitEnd, preempted, sErr := streamBodyIntoSegment(seg, reqCtx, resp.Body, watchdog, cur, reqEnd)
			sched.SetRequestCancel(seg, nil)
			_ = resp.Body.Close()
			watchdog.Stop()
			release()

			if preempted {
				sched.Release(seg)
				return nil
			}
			if sErr == nil && hitEnd {
				opts.HostPool.ReportSuccess(hostKey)
				netAttempt = 0
				throttleAttempt = 0
				continue
			}

			if workerCtx.Err() != nil {
				sched.Release(seg)
				return workerCtx.Err()
			}
			netAttempt++
			if netAttempt >= 3 && opts.Endpoint.refreshFn != nil {
				_ = opts.Endpoint.tryRefresh(workerCtx, gen, 0, sErr.Error(), saveMetaSafe)
			}
			if netAttempt >= maxNetAttempts {
				sched.Release(seg)
				return sErr
			}
			backoff := time.Duration(500*(1<<(netAttempt-1))) * time.Millisecond
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-workerCtx.Done():
				sched.Release(seg)
				return workerCtx.Err()
			case <-time.After(backoff):
			}
		}
	}

	probeBody := opts.ProbeBody
	probeCtx := opts.ProbeCtx
	probeDog := opts.ProbeWatchdog

	for w := 0; w < workerCount; w++ {
		var wBody io.ReadCloser
		var wCtx context.Context
		var wDog *stallWatchdog
		var wProbeSeg *activeSegment
		if w == 0 && hasProbe {
			wBody = probeBody
			wCtx = probeCtx
			wDog = probeDog
			probeBody = nil
			wProbeSeg = sched.AssignReserved(0, opts.ProbeStart, opts.ProbeEnd)
		}

		wg.Add(1)
		go func(workerID int, pSeg *activeSegment, pCtx context.Context, pBody io.ReadCloser, pDog *stallWatchdog) {
			defer wg.Done()
			if pSeg != nil {
				if err := downloadSegment(pSeg, pCtx, pBody, pDog); err != nil {
					recordErr(err)
					return
				}
			}

			for {
				if workerCtx.Err() != nil {
					return
				}
				seg := sched.NextOrSteal(workerID, opts.MinStealSize)
				if seg == nil {
					return
				}
				if err := downloadSegment(seg, nil, nil, nil); err != nil {
					recordErr(err)
					return
				}
			}
		}(w, wProbeSeg, wCtx, wBody, wDog)
	}

	wg.Wait()
	stopPreempt()
	stopTicker()

	if opts.OnStats != nil {
		steals, preempts := sched.Stats()
		opts.OnStats(steals, preempts)
	}

	if ctx.Err() != nil {
		saveMetaSafe()
		return ctx.Err()
	}
	if firstErr != nil {
		saveMetaSafe()
		return firstErr
	}
	if !sched.IsComplete() {
		saveMetaSafe()
		return fmt.Errorf("download incomplete after workers exited")
	}
	return nil
}
