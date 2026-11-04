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

	Quality   string   `json:"quality,omitempty"` // "best", "2160", "1080", "720", "480", "360", "audio-best", "audio-mp3"
	SubDir    string   `json:"subDir,omitempty"`  // Optional subfolder inside DownloadDir (e.g. playlist title)
	Cookies   []Cookie `json:"cookies,omitempty"`
	UserAgent string   `json:"userAgent,omitempty"`
	Referer   string   `json:"referer,omitempty"`
	SavePath  string   `json:"savePath,omitempty"`

	// Runtime controls
	cancelFunc context.CancelFunc `json:"-"`
	mu         sync.Mutex         `json:"-"`
}

type AddRequest struct {
	URL       string   `json:"url"`
	Title     string   `json:"title,omitempty"`
	FileName  string   `json:"fileName,omitempty"`
	Engine    string   `json:"engine,omitempty"`   // "direct", "ytdlp", or "auto"
	Quality   string   `json:"quality,omitempty"`  // "best", "2160", "1080", "720", "480", "360", "audio-best", "audio-mp3"
	Playlist  bool     `json:"playlist,omitempty"` // Expand full playlist into queue items
	SubDir    string   `json:"subDir,omitempty"`
	Cookies   []Cookie `json:"cookies,omitempty"`
	UserAgent string   `json:"userAgent,omitempty"`
	Referer   string   `json:"referer,omitempty"`
}

type QueueManager struct {
	config     *Config
	tasks      []*DownloadTask
	tasksMu    sync.RWMutex
	dataFile   string
	notifyChan chan struct{}
	stopChan   chan struct{}
	broker     *SSEBroker
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

	task := &DownloadTask{
		ID:        taskID,
		URL:       req.URL,
		Title:     title,
		FileName:  req.FileName,
		Engine:    engine,
		Quality:   req.Quality,
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

	return snapshotTask(stored, false), nil
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

	playlistTitle, entries, err := ExpandYtDlpPlaylist(ctx, qm.config, req)
	if err != nil {
		return nil, "", err
	}

	if len(entries) == 0 {
		// Fallback to single video task if not a multi-entry playlist
		single, err := qm.AddTask(req)
		if err != nil {
			return nil, "", err
		}
		return []*DownloadTask{single}, "", nil
	}

	subDir := sanitizeFileName(playlistTitle)
	if subDir == "" || subDir == "download.bin" {
		subDir = "Playlist"
	}

	now := time.Now()
	batch := make([]*DownloadTask, 0, len(entries))
	publicSnapshots := make([]*DownloadTask, 0, len(entries))

	for i, entry := range entries {
		idxPrefix := fmt.Sprintf("%02d - ", i+1)
		if len(entries) >= 100 {
			idxPrefix = fmt.Sprintf("%03d - ", i+1)
		}
		entryTitle := entry.Title
		if entryTitle == "" {
			entryTitle = entry.URL
		}
		displayTitle := fmt.Sprintf("[%s] %s%s", playlistTitle, idxPrefix, entryTitle)
		outputTemplate := fmt.Sprintf("%s%%(title)s [%%(id)s].%%(ext)s", idxPrefix)

		task := &DownloadTask{
			ID:        fmt.Sprintf("dl_%d_%s", now.Unix(), generateToken()[:6]),
			URL:       entry.URL,
			Title:     displayTitle,
			FileName:  outputTemplate,
			Engine:    EngineYtDlp,
			Quality:   req.Quality,
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
	task.mu.Lock()
	if task.Status == StatusPaused || task.Status == StatusError || task.Status == StatusCanceled {
		task.Status = StatusQueued
		task.ErrorMessage = ""
		task.Speed = ""
		task.ETA = ""
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
		task.Status = StatusCanceled
		task.Speed = ""
		task.ETA = ""
		task.UpdatedAt = time.Now()
		if task.cancelFunc != nil {
			task.cancelFunc()
			task.cancelFunc = nil
		}
	} else if task.Status == StatusQueued || task.Status == StatusPaused {
		task.Status = StatusCanceled
		task.UpdatedAt = time.Now()
	}
	task.mu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()
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
		task.Status = StatusDownloading
		task.UpdatedAt = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		task.cancelFunc = cancel
		task.mu.Unlock()

		active++
		qm.save()
		qm.BroadcastUpdate()

		go qm.runDownload(ctx, task)
	}
}

func (qm *QueueManager) runDownload(ctx context.Context, task *DownloadTask) {
	var err error
	if task.Engine == EngineYtDlp {
		err = runYtDlpDownload(ctx, qm, task)
	} else {
		err = runDirectDownload(ctx, qm, task)
	}

	task.mu.Lock()
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
	}
	task.mu.Unlock()

	qm.save()
	qm.BroadcastUpdate()
	qm.triggerLoop()
}

func (qm *QueueManager) Stop() {
	close(qm.stopChan)
}
