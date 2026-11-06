package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	StatusQueued      = "queued"
	StatusDownloading = "downloading"
	StatusPaused      = "paused"
	StatusCompleted   = "completed"
	StatusError       = "error"
	StatusCanceled    = "canceled"

	EngineDirect = "direct"
	EngineYtDlp  = "ytdlp"
	EngineAuto   = "auto"
)

type DownloadTask struct {
	ID              string     `json:"id"`
	URL             string     `json:"url"`
	Title           string     `json:"title"`
	FileName        string     `json:"fileName"`
	Engine          string     `json:"engine"` // "direct" or "ytdlp"
	Status          string     `json:"status"`
	Progress        float64    `json:"progress"`
	DownloadedBytes int64      `json:"downloadedBytes"`
	TotalBytes      int64      `json:"totalBytes"`
	Speed           string     `json:"speed"`
	ETA             string     `json:"eta"`
	ErrorMessage    string     `json:"errorMessage,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`

	Quality   string   `json:"quality,omitempty"`   // "best", "2160", "1080", "720", "480", "360", "audio-best", "audio-mp3"
	SpeedMode string   `json:"speedMode,omitempty"` // "turbo" or "regular" (empty inherits config)
	SubDir    string   `json:"subDir,omitempty"`    // Optional subfolder inside DownloadDir (e.g. playlist title)
	Cookies   []Cookie `json:"cookies,omitempty"`
	UserAgent string   `json:"userAgent,omitempty"`
	Referer   string   `json:"referer,omitempty"`
	SavePath  string   `json:"savePath,omitempty"`

	// Runtime controls
	cancelFunc context.CancelFunc `json:"-"`
	runID      uint64             `json:"-"`
	mu         sync.Mutex         `json:"-"`
	runMu      sync.Mutex         `json:"-"`
}

type AddRequest struct {
	URL           string          `json:"url"`
	Title         string          `json:"title,omitempty"`
	FileName      string          `json:"fileName,omitempty"`
	Engine        string          `json:"engine,omitempty"`        // "direct", "ytdlp", or "auto"
	Quality       string          `json:"quality,omitempty"`       // "best", "2160", "1080", "720", "480", "360", "audio-best", "audio-mp3"
	SpeedMode     string          `json:"speedMode,omitempty"`     // "turbo" or "regular"
	Playlist      bool            `json:"playlist,omitempty"`      // Expand playlist into queue items
	PlaylistStart int             `json:"playlistStart,omitempty"` // 1-based start index (default 1)
	PlaylistEnd   int             `json:"playlistEnd,omitempty"`   // 1-based end index (0 = all/count)
	PlaylistCount int             `json:"playlistCount,omitempty"` // Max number of videos to queue from PlaylistStart
	PlaylistItems []PlaylistEntry `json:"playlistItems,omitempty"` // Pre-selected entries from checklist UI
	SubDir        string          `json:"subDir,omitempty"`
	Cookies       []Cookie        `json:"cookies,omitempty"`
	UserAgent     string          `json:"userAgent,omitempty"`
	Referer       string          `json:"referer,omitempty"`
}

type QueueManager struct {
	config          *Config
	tasks           []*DownloadTask
	tasksMu         sync.RWMutex
	saveMu          sync.Mutex
	dataFile        string
	notifyChan      chan struct{}
	stopChan        chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
	broker          *SSEBroker
	hooksMu         sync.RWMutex
	onUpdate        func()
	onTaskAdded     func(task *DownloadTask)
	onPlaylistAdded func(title string, count int)
	ytCookiesMu     sync.RWMutex
	ytCookies       []Cookie
}

func (qm *QueueManager) UpdateYouTubeCookies(cookies []Cookie) {
	if len(cookies) == 0 {
		return
	}
	cp := make([]Cookie, len(cookies))
	copy(cp, cookies)
	qm.ytCookiesMu.Lock()
	qm.ytCookies = cp
	qm.ytCookiesMu.Unlock()
}

func (qm *QueueManager) GetYouTubeCookies() []Cookie {
	qm.ytCookiesMu.RLock()
	defer qm.ytCookiesMu.RUnlock()
	if len(qm.ytCookies) == 0 {
		return nil
	}
	cp := make([]Cookie, len(qm.ytCookies))
	copy(cp, qm.ytCookies)
	return cp
}

func (qm *QueueManager) SetUIHooks(onUpdate func(), onTaskAdded func(task *DownloadTask), onPlaylistAdded func(title string, count int)) {
	qm.hooksMu.Lock()
	qm.onUpdate = onUpdate
	qm.onTaskAdded = onTaskAdded
	qm.onPlaylistAdded = onPlaylistAdded
	qm.hooksMu.Unlock()
}

func NewQueueManager(cfg *Config, dataFile string, broker *SSEBroker) *QueueManager {
	qm := &QueueManager{
		config:     cfg,
		tasks:      make([]*DownloadTask, 0),
		dataFile:   dataFile,
		notifyChan: make(chan struct{}, 10),
		stopChan:   make(chan struct{}),
		broker:     broker,
	}

	qm.load()
	qm.wg.Add(1)
	go qm.processLoop()
	return qm
}

func (qm *QueueManager) load() {
	data, err := os.ReadFile(qm.dataFile)
	if err != nil {
		return
	}

	var savedTasks []*DownloadTask
	if err := json.Unmarshal(data, &savedTasks); err != nil {
		return
	}

	qm.tasksMu.Lock()
	defer qm.tasksMu.Unlock()

	for _, task := range savedTasks {
		// If task was downloading when daemon closed, mark as paused
		if task.Status == StatusDownloading {
			task.Status = StatusPaused
			task.Speed = ""
			task.ETA = ""
		}
		qm.tasks = append(qm.tasks, task)
	}
}

func snapshotTask(t *DownloadTask, includeCookies bool) *DownloadTask {
	t.mu.Lock()
	defer t.mu.Unlock()

	cp := &DownloadTask{
		ID:              t.ID,
		URL:             t.URL,
		Title:           t.Title,
		FileName:        t.FileName,
		Engine:          t.Engine,
		Status:          t.Status,
		Progress:        t.Progress,
		DownloadedBytes: t.DownloadedBytes,
		TotalBytes:      t.TotalBytes,
		Speed:           t.Speed,
		ETA:             t.ETA,
		ErrorMessage:    t.ErrorMessage,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
		CompletedAt:     t.CompletedAt,
		Quality:         t.Quality,
		SpeedMode:       t.SpeedMode,
		SubDir:          t.SubDir,
		UserAgent:       t.UserAgent,
		Referer:         t.Referer,
		SavePath:        t.SavePath,
	}
	if includeCookies {
		cp.Cookies = t.Cookies
	}
	return cp
}

func (qm *QueueManager) save() {
	qm.saveMu.Lock()
	defer qm.saveMu.Unlock()

	qm.tasksMu.RLock()
	snapshots := make([]*DownloadTask, len(qm.tasks))
	for i, t := range qm.tasks {
		snapshots[i] = snapshotTask(t, true)
	}
	qm.tasksMu.RUnlock()

	data, err := json.MarshalIndent(snapshots, "", "  ")
	if err != nil {
		return
	}

	dir := filepath.Dir(qm.dataFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	tmp := qm.dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		_ = os.Remove(qm.dataFile) // Windows rename compatibility
		_ = os.Rename(tmp, qm.dataFile)
	}
}

func (qm *QueueManager) triggerLoop() {
	select {
	case qm.notifyChan <- struct{}{}:
	default:
	}
}

func (qm *QueueManager) BroadcastUpdate() {
	if qm.broker != nil {
		qm.broker.BroadcastJSON("queue_update", qm.GetAll())
	}
	qm.hooksMu.RLock()
	fn := qm.onUpdate
	qm.hooksMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (qm *QueueManager) GetAll() []*DownloadTask {
	qm.tasksMu.RLock()
	defer qm.tasksMu.RUnlock()

	res := make([]*DownloadTask, len(qm.tasks))
	for i, t := range qm.tasks {
		res[i] = snapshotTask(t, false)
	}
	return res
}

func (qm *QueueManager) GetByID(id string) *DownloadTask {
	qm.tasksMu.RLock()
	defer qm.tasksMu.RUnlock()

	for _, t := range qm.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// AddTask creates a new download task.
func (qm *QueueManager) AddTask(req AddRequest) (*DownloadTask, error) {
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		return nil, fmt.Errorf("URL cannot be empty")
	}

	engine := strings.ToLower(strings.TrimSpace(req.Engine))
	if engine == "" || engine == EngineAuto {
		engine = detectEngine(req.URL)
	}

	taskID := fmt.Sprintf("dl_%d_%s", time.Now().Unix(), generateToken()[:6])

	title := req.Title
	if title == "" {
		title = req.FileName
	}
	if title == "" {
		title = defaultTitleFromURL(req.URL)
	}

	if isYouTubeURL(req.URL) {
		if len(req.Cookies) > 0 && hasYouTubeAuthCookies(req.Cookies) {
			qm.UpdateYouTubeCookies(req.Cookies)
		} else if len(req.Cookies) == 0 {
			req.Cookies = qm.GetYouTubeCookies()
		}
	}

	var reqSpeedMode string
	if strings.TrimSpace(req.SpeedMode) != "" {
		reqSpeedMode = normalizeSpeedMode(strings.ToLower(strings.TrimSpace(req.SpeedMode)))
	}

	task := &DownloadTask{
		ID:        taskID,
		URL:       req.URL,
		Title:     title,
		FileName:  req.FileName,
		Engine:    engine,
		Quality:   req.Quality,
		SpeedMode: reqSpeedMode,
		SubDir:    req.SubDir,
		Status:    StatusQueued,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Cookies:   req.Cookies,
		UserAgent: req.UserAgent,
		Referer:   req.Referer,
	}

	qm.tasksMu.Lock()
	qm.tasks = append([]*DownloadTask{snapshotTask(task, true)}, qm.tasks...) // Add to top
	// Return pointer to actual stored task
	stored := qm.tasks[0]
	qm.tasksMu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()

	snap := snapshotTask(stored, false)
	qm.hooksMu.RLock()
	addedFn := qm.onTaskAdded
	qm.hooksMu.RUnlock()
	if addedFn != nil {
		addedFn(snap)
	}

	return snap, nil
}

func isPurePlaylistURL(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	return (strings.Contains(lower, "youtube.com/playlist?") || strings.Contains(lower, "music.youtube.com/playlist?")) &&
		strings.Contains(lower, "list=")
}

// AddPlaylistTasks expands a playlist via yt-dlp --flat-playlist and queues all entries in order.
func (qm *QueueManager) AddPlaylistTasks(ctx context.Context, req AddRequest) ([]*DownloadTask, string, error) {
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		return nil, "", fmt.Errorf("URL cannot be empty")
	}

	if isYouTubeURL(req.URL) {
		if len(req.Cookies) > 0 && hasYouTubeAuthCookies(req.Cookies) {
			qm.UpdateYouTubeCookies(req.Cookies)
		} else if len(req.Cookies) == 0 {
			req.Cookies = qm.GetYouTubeCookies()
		}
	}

	var (
		playlistTitle string
		entries       []PlaylistEntry
		err           error
	)

	// If user already selected exact items from the Playlist Checklist UI, queue them directly (0ms!)
	if len(req.PlaylistItems) > 0 {
		playlistTitle = strings.TrimSpace(req.Title)
		if playlistTitle == "" {
			playlistTitle = "Playlist"
		}
		for i, item := range req.PlaylistItems {
			u := strings.TrimSpace(item.URL)
			if u == "" {
				continue
			}
			idx := item.Index
			if idx <= 0 {
				idx = i + 1
			}
			entries = append(entries, PlaylistEntry{
				Index: idx,
				URL:   u,
				Title: strings.TrimSpace(item.Title),
			})
		}
	} else {
		playlistTitle, entries, err = ExpandYtDlpPlaylist(ctx, qm.config, req)
		if err != nil {
			return nil, "", err
		}
	}

	if len(entries) == 0 {
		// Fallback to single video task if not a multi-entry playlist
		singleReq := req
		singleReq.Playlist = false
		single, err := qm.AddTask(singleReq)
		if err != nil {
			return nil, "", err
		}
		return []*DownloadTask{single}, "", nil
	}

	subDir := sanitizeSubDirName(playlistTitle)

	maxIdx := len(entries)
	for _, e := range entries {
		if e.Index > maxIdx {
			maxIdx = e.Index
		}
	}

	var reqSpeedMode string
	if strings.TrimSpace(req.SpeedMode) != "" {
		reqSpeedMode = normalizeSpeedMode(strings.ToLower(strings.TrimSpace(req.SpeedMode)))
	}

	now := time.Now()
	batch := make([]*DownloadTask, 0, len(entries))
	publicSnapshots := make([]*DownloadTask, 0, len(entries))

	for i, entry := range entries {
		num := entry.Index
		if num <= 0 {
			num = i + 1
		}
		idxPrefix := fmt.Sprintf("%02d - ", num)
		if maxIdx >= 100 {
			idxPrefix = fmt.Sprintf("%03d - ", num)
		}
		entryTitle := entry.Title
		if entryTitle == "" {
			entryTitle = entry.URL
		}
		displayTitle := fmt.Sprintf("%s%s", idxPrefix, entryTitle)
		outputTemplate := fmt.Sprintf("%s%%(title)s [%%(id)s].%%(ext)s", idxPrefix)

		task := &DownloadTask{
			ID:        fmt.Sprintf("dl_%d_%s", now.Unix(), generateToken()[:6]),
			URL:       entry.URL,
			Title:     displayTitle,
			FileName:  outputTemplate,
			Engine:    EngineYtDlp,
			Quality:   req.Quality,
			SpeedMode: reqSpeedMode,
			SubDir:    subDir,
			Status:    StatusQueued,
			CreatedAt: now,
			UpdatedAt: now,
			Cookies:   req.Cookies,
			UserAgent: req.UserAgent,
			Referer:   req.Referer,
		}
		batch = append(batch, task)
		publicSnapshots = append(publicSnapshots, snapshotTask(task, false))
	}

	qm.tasksMu.Lock()
	qm.tasks = append(batch, qm.tasks...) // Entry #1 at top, processed first!
	qm.tasksMu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()

	qm.hooksMu.RLock()
	plFn := qm.onPlaylistAdded
	qm.hooksMu.RUnlock()
	if plFn != nil {
		plFn(playlistTitle, len(publicSnapshots))
	}

	return publicSnapshots, playlistTitle, nil
}

func detectEngine(rawURL string) string {
	lower := strings.ToLower(rawURL)

	// Direct file extensions
	directExts := []string{
		".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz",
		".iso", ".dmg", ".exe", ".msi", ".apk", ".deb", ".rpm",
		".pdf", ".epub", ".mobi", ".docx", ".xlsx", ".pptx",
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg",
		".mp3", ".flac", ".wav", ".aac", ".ogg",
	}

	for _, ext := range directExts {
		if strings.HasSuffix(lower, ext) || strings.Contains(lower, ext+"?") {
			return EngineDirect
		}
	}

	// Video streaming patterns typically best handled by yt-dlp
	videoDomains := []string{
		"youtube.com", "youtu.be", "vimeo.com", "twitch.tv",
		"tiktok.com", "bilibili.com", "twitter.com", "x.com",
		"instagram.com", "facebook.com", "dailymotion.com",
		"reddit.com", "rumble.com", "odysee.com",
	}

	for _, d := range videoDomains {
		if strings.Contains(lower, d) {
			return EngineYtDlp
		}
	}

	// Stream playlists
	if strings.Contains(lower, ".m3u8") || strings.Contains(lower, ".mpd") {
		return EngineYtDlp
	}

	// Default fallback to direct if it looks like a file, otherwise yt-dlp
	if strings.Contains(lower, ".mp4") || strings.Contains(lower, ".webm") || strings.Contains(lower, ".mkv") {
		// Could be either direct or stream; if it has query parameters like tokens, direct often works
		return EngineDirect
	}

	return EngineYtDlp
}

func defaultTitleFromURL(rawURL string) string {
	// Strip query string
	urlPart := rawURL
	if idx := strings.Index(urlPart, "?"); idx != -1 {
		urlPart = urlPart[:idx]
	}
	base := filepath.Base(urlPart)
	if base == "" || base == "/" || base == "." {
		return rawURL
	}
	return base
}

// PauseTask pauses an active or queued task.
func (qm *QueueManager) PauseTask(id string) bool {
	task := qm.GetByID(id)
	if task == nil {
		return false
	}

	var shouldSave bool
	var shouldTrigger bool

	task.mu.Lock()
	if task.Status == StatusDownloading {
		task.runID++
		task.Status = StatusPaused
		task.Speed = ""
		task.ETA = ""
		task.UpdatedAt = time.Now()
		if task.cancelFunc != nil {
			task.cancelFunc()
			task.cancelFunc = nil
		}
		shouldSave = true
		shouldTrigger = true
	} else if task.Status == StatusQueued {
		task.runID++
		task.Status = StatusPaused
		task.UpdatedAt = time.Now()
		shouldSave = true
	}
	task.mu.Unlock()

	if shouldSave {
		qm.save()
		qm.BroadcastUpdate()
		if shouldTrigger {
			qm.triggerLoop()
		}
		return true
	}

	return false
}

// ResumeTask resumes a paused or errored task.
func (qm *QueueManager) ResumeTask(id string) bool {
	task := qm.GetByID(id)
	if task == nil {
		return false
	}

	resumed := false
	freshYTCookies := qm.GetYouTubeCookies()
	task.mu.Lock()
	if task.Status == StatusPaused || task.Status == StatusError || task.Status == StatusCanceled {
		task.runID++
		task.Status = StatusQueued
		task.ErrorMessage = ""
		task.Speed = ""
		task.ETA = ""
		if isYouTubeURL(task.URL) && len(freshYTCookies) > 0 {
			task.Cookies = freshYTCookies
		}
		task.UpdatedAt = time.Now()
		resumed = true
	}
	task.mu.Unlock()

	if resumed {
		qm.save()
		qm.BroadcastUpdate()
		qm.triggerLoop()
		return true
	}

	return false
}

// CancelTask cancels an active or queued task.
func (qm *QueueManager) CancelTask(id string) bool {
	task := qm.GetByID(id)
	if task == nil {
		return false
	}

	task.mu.Lock()
	if task.Status == StatusDownloading {
		task.runID++
		task.Status = StatusCanceled
		task.Speed = ""
		task.ETA = ""
		task.UpdatedAt = time.Now()
		if task.cancelFunc != nil {
			task.cancelFunc()
			task.cancelFunc = nil
		}
	} else if task.Status == StatusQueued || task.Status == StatusPaused {
		task.runID++
		task.Status = StatusCanceled
		task.UpdatedAt = time.Now()
	}
	task.mu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()
	return true
}

// ResolveTaskSpeedMode returns the effective speed mode ("turbo" or "regular") for a task.
func (qm *QueueManager) ResolveTaskSpeedMode(task *DownloadTask) string {
	task.mu.Lock()
	m := strings.ToLower(strings.TrimSpace(task.SpeedMode))
	task.mu.Unlock()
	if m == SpeedModeRegular || m == SpeedModeTurbo {
		return m
	}
	return qm.config.GetSpeedMode()
}

// SetTaskSpeedMode switches a task between "turbo" (6x multi-connection) and "regular"
// (1x sequential watch-while-downloading), seamlessly restarting active downloads in-place.
func (qm *QueueManager) SetTaskSpeedMode(id, mode string) bool {
	task := qm.GetByID(id)
	if task == nil {
		return false
	}

	target := normalizeSpeedMode(strings.ToLower(strings.TrimSpace(mode)))
	var restartActive bool

	task.mu.Lock()
	if task.Status == StatusCompleted {
		task.mu.Unlock()
		return false
	}
	task.SpeedMode = target
	task.UpdatedAt = time.Now()
	if task.Status == StatusDownloading {
		task.runID++
		task.Status = StatusQueued
		task.Speed = "Switching mode..."
		task.ETA = ""
		if task.cancelFunc != nil {
			task.cancelFunc()
			task.cancelFunc = nil
		}
		restartActive = true
	}
	task.mu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	if restartActive {
		qm.triggerLoop()
	}
	return true
}

// DeleteTask removes a task from the list and cleans up partial files if requested.
func (qm *QueueManager) DeleteTask(id string, deleteFile bool) bool {
	task := qm.GetByID(id)
	if task == nil {
		return false
	}

	task.mu.Lock()
	if task.cancelFunc != nil {
		task.cancelFunc()
		task.cancelFunc = nil
	}
	savePath := task.SavePath
	task.mu.Unlock()

	if deleteFile && savePath != "" {
		_ = os.Remove(savePath)
		_ = os.Remove(savePath + ".part")
		_ = os.Remove(savePath + ".part.turbo")
	}

	qm.tasksMu.Lock()
	newTasks := make([]*DownloadTask, 0, len(qm.tasks))
	found := false
	for _, t := range qm.tasks {
		if t.ID == id {
			found = true
			continue
		}
		newTasks = append(newTasks, t)
	}
	qm.tasks = newTasks
	qm.tasksMu.Unlock()

	if found {
		qm.save()
		qm.BroadcastUpdate()
		qm.triggerLoop()
	}

	return found
}

// ClearCompleted removes all completed, errored, or canceled tasks.
func (qm *QueueManager) ClearCompleted() int {
	qm.tasksMu.Lock()
	newTasks := make([]*DownloadTask, 0, len(qm.tasks))
	count := 0
	for _, t := range qm.tasks {
		t.mu.Lock()
		st := t.Status
		t.mu.Unlock()
		if st == StatusCompleted || st == StatusCanceled || st == StatusError {
			count++
			continue
		}
		newTasks = append(newTasks, t)
	}
	qm.tasks = newTasks
	qm.tasksMu.Unlock()

	if count > 0 {
		qm.save()
		qm.BroadcastUpdate()
	}
	return count
}

func (qm *QueueManager) countDownloading() int {
	qm.tasksMu.RLock()
	defer qm.tasksMu.RUnlock()

	count := 0
	for _, t := range qm.tasks {
		t.mu.Lock()
		if t.Status == StatusDownloading {
			count++
		}
		t.mu.Unlock()
	}
	return count
}

func (qm *QueueManager) getNextQueued() *DownloadTask {
	qm.tasksMu.RLock()
	defer qm.tasksMu.RUnlock()

	for _, t := range qm.tasks {
		t.mu.Lock()
		queued := t.Status == StatusQueued
		t.mu.Unlock()
		if queued {
			return t
		}
	}
	return nil
}

func (qm *QueueManager) processLoop() {
	defer qm.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-qm.stopChan:
			return
		case <-qm.notifyChan:
			qm.scheduleTasks()
		case <-ticker.C:
			qm.scheduleTasks()
		}
	}
}

func (qm *QueueManager) scheduleTasks() {
	maxConcurrent := qm.config.GetMaxConcurrent()
	active := qm.countDownloading()

	for active < maxConcurrent {
		task := qm.getNextQueued()
		if task == nil {
			break
		}

		task.mu.Lock()
		if task.Status != StatusQueued {
			task.mu.Unlock()
			continue
		}
		task.runID++
		myRunID := task.runID
		task.Status = StatusDownloading
		task.UpdatedAt = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		task.cancelFunc = cancel
		task.mu.Unlock()

		active++
		qm.save()
		qm.BroadcastUpdate()

		qm.wg.Add(1)
		go qm.runDownload(ctx, task, myRunID)
	}
}

func (qm *QueueManager) runDownload(ctx context.Context, task *DownloadTask, myRunID uint64) {
	defer qm.wg.Done()
	task.runMu.Lock()
	defer task.runMu.Unlock()

	task.mu.Lock()
	if task.runID != myRunID {
		task.mu.Unlock()
		return
	}
	engine := task.Engine
	task.mu.Unlock()

	var err error
	if engine == EngineYtDlp {
		err = runYtDlpDownload(ctx, qm, task)
	} else {
		err = runDirectDownload(ctx, qm, task)
	}

	task.mu.Lock()
	if task.runID != myRunID {
		// Superseded by Pause, Resume, or Cancel; do not overwrite newer state
		task.mu.Unlock()
		return
	}
	task.cancelFunc = nil
	now := time.Now()
	task.UpdatedAt = now
	task.Speed = ""
	task.ETA = ""

	if ctx.Err() != nil {
		// Context was canceled
		if task.Status != StatusPaused && task.Status != StatusCanceled {
			task.Status = StatusPaused
		}
	} else if err != nil {
		task.Status = StatusError
		task.ErrorMessage = err.Error()
	} else {
		task.Status = StatusCompleted
		task.Progress = 100.0
		task.CompletedAt = &now
		if task.SavePath != "" {
			cand := task.SavePath
			if !filepath.IsAbs(cand) {
				cand = filepath.Join(qm.config.GetDownloadDir(), cand)
			}
			if fi, sErr := os.Stat(cand); sErr == nil && !fi.IsDir() && fi.Size() > 0 {
				task.SavePath = cand
				task.DownloadedBytes = fi.Size()
				task.TotalBytes = fi.Size()
			}
		}
		if task.FileName != "" {
			cleanName := strings.TrimSuffix(task.FileName, filepath.Ext(task.FileName))
			if task.Title == "" || task.Title == task.URL || strings.Contains(task.Title, "://") {
				task.Title = cleanName
			}
		}
	}
	task.mu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()
}

func (qm *QueueManager) Stop() {
	qm.stopOnce.Do(func() {
		close(qm.stopChan)
		qm.tasksMu.RLock()
		for _, t := range qm.tasks {
			t.mu.Lock()
			if t.cancelFunc != nil {
				t.cancelFunc()
				t.cancelFunc = nil
			}
			t.mu.Unlock()
		}
		qm.tasksMu.RUnlock()
	})
	qm.wg.Wait()
}
