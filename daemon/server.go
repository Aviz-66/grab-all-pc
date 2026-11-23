package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var embeddedWebFS embed.FS

var openFileHandler = openFileDefault

type SSEBroker struct {
	clients map[chan []byte]bool
	mu      sync.Mutex
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan []byte]bool),
	}
}

func (b *SSEBroker) AddClient(ch chan []byte) {
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
}

func (b *SSEBroker) RemoveClient(ch chan []byte) {
	b.mu.Lock()
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *SSEBroker) BroadcastJSON(eventType string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(payload))
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- []byte(msg):
		default:
		}
	}
}

type Server struct {
	config             *Config
	queue              *QueueManager
	broker             *SSEBroker
	updater            *Updater
	httpSrv            *http.Server
	onShowWindow       func()
	onRestartForUpdate func()
}

func NewServer(cfg *Config, qm *QueueManager, broker *SSEBroker) *Server {
	return &Server{
		config:  cfg,
		queue:   qm,
		broker:  broker,
		updater: NewUpdater(),
	}
}

func (s *Server) SetShowWindowHandler(fn func()) {
	s.onShowWindow = fn
}

func (s *Server) SetRestartForUpdateHandler(fn func()) {
	s.onRestartForUpdate = fn
}

func (s *Server) setupRoutes() http.Handler {
	mux := http.NewServeMux()

	// Embedded static web UI
	webSubFS, err := fs.Sub(embeddedWebFS, "web")
	if err == nil {
		mux.Handle("/", http.FileServer(http.FS(webSubFS)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("GrabAll Download Manager UI"))
		})
	}

	// API endpoints
	mux.HandleFunc("/api/ping", s.handlePing)
	mux.HandleFunc("/api/pair", s.handlePair)
	mux.HandleFunc("/api/sync-cookies", s.handleSyncCookies)
	mux.HandleFunc("/api/show-window", s.handleShowWindow)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/add", s.handleAdd)
	mux.HandleFunc("/api/playlist/preview", s.handlePlaylistPreview)
	mux.HandleFunc("/api/media/preview", s.handleMediaPreview)
	mux.HandleFunc("/api/preview", s.handleMediaPreview)
	mux.HandleFunc("/api/pause", s.handlePause)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/speed-mode", s.handleSpeedMode)
	mux.HandleFunc("/api/speed-limit", s.handleSpeedLimit)
	mux.HandleFunc("/api/when-done", s.handleWhenDone)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/delete", s.handleDelete)
	mux.HandleFunc("/api/clear-completed", s.handleClearCompleted)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/install-ytdlp", s.handleInstallYtDlp)
	mux.HandleFunc("/api/install-ffmpeg", s.handleInstallFFmpeg)
	mux.HandleFunc("/api/install-all", s.handleInstallAll)
	mux.HandleFunc("/api/open-folder", s.handleOpenFolder)
	mux.HandleFunc("/api/open-file", s.handleOpenFile)
	mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)

	return s.corsAndAuthMiddleware(mux)
}

func isBrowserExtensionOrigin(origin string) bool {
	lower := strings.ToLower(strings.TrimSpace(origin))
	return strings.HasPrefix(lower, "chrome-extension://") ||
		strings.HasPrefix(lower, "moz-extension://") ||
		strings.HasPrefix(lower, "safari-web-extension://")
}

// isTrustedExtensionRequest verifies requests coming from our MV3 browser extension.
// Note: Chromium MV3 service workers with <all_urls> host_permissions omit Origin on GET
// requests and send Sec-Fetch-Site: none, whereas regular websites ALWAYS send Origin
// (http/https/null) + Sec-Fetch-Site: cross-site and cannot send X-GrabAll-Ext (blocked by CORS preflight).
func isTrustedExtensionRequest(r *http.Request) bool {
	origin := strings.ToLower(strings.TrimSpace(r.Header.Get("Origin")))
	if isBrowserExtensionOrigin(origin) {
		return true
	}
	// Block any web page origin immediately
	if strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://") || origin == "null" {
		return false
	}
	secSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if secSite == "cross-site" || secSite == "same-site" {
		return false
	}
	if strings.TrimSpace(r.Header.Get("X-GrabAll-Ext")) != "" {
		return true
	}
	return false
}

func (s *Server) corsAndAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Daemon-Token, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Check auth for /api/ routes (except /api/ping and /api/pair which enforces extension-origin check)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/ping" && r.URL.Path != "/api/pair" {
			if !s.isAuthorized(r) {
				http.Error(w, `{"error": "Unauthorized: invalid or missing daemon token"}`, http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAuthorized(r *http.Request) bool {
	expectedToken := s.config.GetToken()

	// 1. Header X-Daemon-Token
	if token := r.Header.Get("X-Daemon-Token"); token != "" && token == expectedToken {
		return true
	}

	// 2. Authorization Bearer
	if auth := r.Header.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") && parts[1] == expectedToken {
			return true
		}
	}

	// 3. Query parameter ?token=...
	if token := r.URL.Query().Get("token"); token != "" && token == expectedToken {
		return true
	}

	// 4. Verified browser extension request (Origin: chrome-extension://... or CORS-protected X-GrabAll-Ext)
	if isTrustedExtensionRequest(r) {
		return true
	}

	// 5. Same-origin requests from the embedded web dashboard (Chrome/Brave always
	// sets Sec-Fetch-Site: same-origin even when Brave Shields strips Referer)
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}

	// 5. Local origin / referer
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	hostPort := fmt.Sprintf("127.0.0.1:%d", s.config.Port)
	localPort := fmt.Sprintf("localhost:%d", s.config.Port)

	if origin != "" {
		if strings.Contains(origin, hostPort) || strings.Contains(origin, localPort) {
			return true
		}
	} else if referer != "" {
		if strings.Contains(referer, hostPort) || strings.Contains(referer, localPort) {
			return true
		}
	} else {
		if expectedToken == "" {
			return true
		}
	}

	return false
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, hasYtDlp := resolveYtDlpBinary(s.config)
	hasFFmpeg := resolveFFmpegBinary(s.config) != ""

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         AppVersion,
		"nativeGui":       s.onShowWindow != nil,
		"downloadDir":     s.config.GetDownloadDir(),
		"maxConcurrent":   s.config.GetMaxConcurrent(),
		"speedMode":       s.config.GetSpeedMode(),
		"speedLimitKBps":  s.config.GetSpeedLimitKBps(),
		"theme":           s.config.GetTheme(),
		"whenDoneAction":  s.queue.GetWhenDoneAction(),
		"ytDlpAvailable":  hasYtDlp == nil,
		"ffmpegAvailable": hasFFmpeg,
	})
}

func (s *Server) handleSyncCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil && len(req.Cookies) > 0 {
		s.queue.UpdateYouTubeCookies(req.Cookies)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// handlePair allows browser extensions to pair automatically with zero manual token copy-pasting,
// while blocking regular web pages.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if !isTrustedExtensionRequest(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Forbidden: pairing restricted to browser extensions"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"token":   s.config.GetToken(),
	})
}

func (s *Server) handleShowWindow(w http.ResponseWriter, r *http.Request) {
	if s.onShowWindow != nil {
		s.onShowWindow()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"nativeGui": s.onShowWindow != nil,
	})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	clientChan := make(chan []byte, 32)
	s.broker.AddClient(clientChan)
	defer s.broker.RemoveClient(clientChan)

	// Send initial state immediately
	initialData, _ := json.Marshal(s.queue.GetAll())
	_, _ = fmt.Fprintf(w, "event: queue_init\ndata: %s\n\n", string(initialData))
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-clientChan:
			if !ok {
				return
			}
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-ticker.C:
			// Keepalive ping
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.queue.GetAll())
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request JSON: " + err.Error()})
		return
	}

	if req.Playlist || isPurePlaylistURL(req.URL) {
		tasks, plTitle, err := s.queue.AddPlaylistTasks(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":       true,
			"playlist":      true,
			"playlistTitle": plTitle,
			"count":         len(tasks),
			"tasks":         tasks,
		})
		return
	}

	task, err := s.queue.AddTask(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"task":    task,
	})
}

func (s *Server) handlePlaylistPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request JSON: " + err.Error()})
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "URL cannot be empty"})
		return
	}

	if isYouTubeURL(req.URL) {
		if len(req.Cookies) > 0 && hasYouTubeAuthCookies(req.Cookies) {
			s.queue.UpdateYouTubeCookies(req.Cookies)
		} else if len(req.Cookies) == 0 {
			req.Cookies = s.queue.GetYouTubeCookies()
		}
	}

	plTitle, entries, err := ExpandYtDlpPlaylist(r.Context(), s.config, req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"playlistTitle": plTitle,
		"count":         len(entries),
		"entries":       entries,
	})
}

func (s *Server) handleMediaPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request JSON: " + err.Error()})
		return
	}

	preview, err := s.queue.PreviewMedia(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"preview": preview,
	})
}

func resolveTaskRevealPaths(cfg *Config, qm *QueueManager, taskID string) (filePath, dirPath string) {
	dirPath = cfg.GetDownloadDir()
	if taskID == "" {
		return "", dirPath
	}
	if task := qm.GetByID(taskID); task != nil {
		task.mu.Lock()
		sp := task.SavePath
		fn := task.FileName
		title := task.Title
		sub := task.SubDir
		task.mu.Unlock()

		taskDir := resolveTaskDownloadDir(dirPath, sub)
		if cand := resolveExistingMediaFile(taskDir, sp, fn, title); cand != "" {
			return cand, filepath.Dir(cand)
		}
		if sp != "" {
			if !filepath.IsAbs(sp) {
				sp = filepath.Join(taskDir, sp)
			}
			if _, err := os.Stat(sp); err == nil {
				return sp, filepath.Dir(sp)
			}
			if _, err := os.Stat(sp + ".part"); err == nil {
				return sp + ".part", filepath.Dir(sp)
			}
			if strings.HasSuffix(strings.ToLower(sp), ".part") {
				trimmed := sp[:len(sp)-5]
				if _, err := os.Stat(trimmed); err == nil {
					return trimmed, filepath.Dir(trimmed)
				}
			}
		}
		if fn != "" {
			candFn := filepath.Join(taskDir, fn)
			if _, err := os.Stat(candFn); err == nil {
				return candFn, taskDir
			}
			if strings.HasSuffix(strings.ToLower(candFn), ".part") {
				trimmed := candFn[:len(candFn)-5]
				if _, err := os.Stat(trimmed); err == nil {
					return trimmed, taskDir
				}
			}
		}
		if _, err := os.Stat(taskDir); err == nil {
			dirPath = taskDir
		}
	}
	return "", dirPath
}

func (s *Server) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	filePath, target := resolveTaskRevealPaths(s.config, s.queue, req.ID)
	openFolderOrSelectFile(filePath, target)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (s *Server) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	w.Header().Set("Content-Type", "application/json")
	if req.ID == "" || s.queue.GetByID(req.ID) == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Task not found",
		})
		return
	}

	filePath, _ := resolveTaskRevealPaths(s.config, s.queue, req.ID)
	if filePath == "" {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Downloaded file not found on disk",
		})
		return
	}

	if err := openFileHandler(filePath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    filePath,
	})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ok := s.queue.PauseTask(req.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": ok})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ok := s.queue.ResumeTask(req.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": ok})
}

func (s *Server) handleSpeedMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID        string `json:"id"`
		SpeedMode string `json:"speedMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ok := s.queue.SetTaskSpeedMode(req.ID, req.SpeedMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   ok,
		"speedMode": normalizeSpeedMode(strings.ToLower(strings.TrimSpace(req.SpeedMode))),
	})
}

func (s *Server) handleSpeedLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID             string `json:"id"`
		SpeedLimitKBps int64  `json:"speedLimitKBps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.SpeedLimitKBps < 0 {
		req.SpeedLimitKBps = 0
	}
	ok := s.queue.SetTaskSpeedLimitKBps(req.ID, req.SpeedLimitKBps)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        ok,
		"speedLimitKBps": req.SpeedLimitKBps,
	})
}

func (s *Server) handleWhenDone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"whenDoneAction": s.queue.GetWhenDoneAction(),
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		applied := s.queue.SetWhenDoneAction(req.Action)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":        true,
			"whenDoneAction": applied,
		})
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ok := s.queue.CancelTask(req.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": ok})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID         string `json:"id"`
		DeleteFile bool   `json:"deleteFile"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ok := s.queue.DeleteTask(req.ID, req.DeleteFile)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": ok})
}

func (s *Server) handleClearCompleted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	count := s.queue.ClearCompleted()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"cleared": count})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, hasYtDlp := resolveYtDlpBinary(s.config)
		hasFFmpeg := resolveFFmpegBinary(s.config) != ""
		autoUpdates := s.config.GetAutoCheckUpdates()

		s.config.mu.RLock()
		resp := map[string]interface{}{
			"version":          AppVersion,
			"port":             s.config.Port,
			"authToken":        s.config.AuthToken,
			"downloadDir":      s.config.DownloadDir,
			"maxConcurrent":    s.config.MaxConcurrent,
			"ytDlpPath":        s.config.YtDlpPath,
			"ffmpegPath":       s.config.FFmpegPath,
			"defaultEngine":    s.config.DefaultEngine,
			"speedMode":        normalizeSpeedMode(s.config.SpeedMode),
			"videoCodec":       normalizeVideoCodec(s.config.VideoCodec),
			"speedLimitKBps":   s.config.SpeedLimitKBps,
			"theme":            normalizeTheme(s.config.Theme),
			"whenDoneAction":   s.queue.GetWhenDoneAction(),
			"ytDlpAvailable":   hasYtDlp == nil,
			"ffmpegAvailable":  hasFFmpeg,
			"autoCheckUpdates": autoUpdates,
		}
		s.config.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			DownloadDir      string `json:"downloadDir"`
			MaxConcurrent    int    `json:"maxConcurrent"`
			DefaultEngine    string `json:"defaultEngine"`
			SpeedMode        string `json:"speedMode"`
			VideoCodec       string `json:"videoCodec"`
			SpeedLimitKBps   *int64 `json:"speedLimitKBps"`
			Theme            string `json:"theme"`
			YtDlpPath        string `json:"ytDlpPath"`
			FFmpegPath       string `json:"ffmpegPath"`
			AutoCheckUpdates *bool  `json:"autoCheckUpdates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		s.config.mu.Lock()
		if req.DownloadDir != "" {
			s.config.DownloadDir = req.DownloadDir
		}
		if req.MaxConcurrent > 0 {
			s.config.MaxConcurrent = req.MaxConcurrent
		}
		if req.DefaultEngine != "" {
			s.config.DefaultEngine = req.DefaultEngine
		}
		if req.SpeedMode != "" {
			s.config.SpeedMode = normalizeSpeedMode(strings.ToLower(strings.TrimSpace(req.SpeedMode)))
		}
		if req.VideoCodec != "" {
			s.config.VideoCodec = normalizeVideoCodec(req.VideoCodec)
		}
		if req.Theme != "" {
			s.config.Theme = normalizeTheme(req.Theme)
		}
		if req.YtDlpPath != "" {
			s.config.YtDlpPath = req.YtDlpPath
		}
		if req.FFmpegPath != "" {
			s.config.FFmpegPath = req.FFmpegPath
		}
		if req.AutoCheckUpdates != nil {
			val := *req.AutoCheckUpdates
			s.config.AutoCheckUpdates = &val
		}
		s.config.mu.Unlock()

		_ = s.config.Save()

		if req.SpeedLimitKBps != nil && s.queue != nil {
			s.queue.SetGlobalSpeedLimitKBps(*req.SpeedLimitKBps)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleInstallYtDlp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	binDir := filepath.Join(executableDir(), "bin")
	installedPath, err := DownloadYtDlpStandalone(binDir)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	s.config.mu.Lock()
	s.config.YtDlpPath = installedPath
	s.config.mu.Unlock()
	_ = s.config.Save()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    installedPath,
	})
}

func (s *Server) handleInstallAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	binDir := filepath.Join(executableDir(), "bin")
	_ = os.MkdirAll(binDir, 0755)

	ytPath, ytErr := DownloadYtDlpStandalone(binDir)
	if ytErr == nil {
		s.config.mu.Lock()
		s.config.YtDlpPath = ytPath
		s.config.mu.Unlock()
	}

	ffPath, ffErr := DownloadFFmpegStandalone(binDir)
	if ffErr == nil {
		s.config.mu.Lock()
		s.config.FFmpegPath = ffPath
		s.config.mu.Unlock()
	}

	_ = s.config.Save()

	w.Header().Set("Content-Type", "application/json")
	if ytErr != nil && ffErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("yt-dlp: %v; ffmpeg: %v", ytErr, ffErr),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"ytDlpPath":  ytPath,
		"ffmpegPath": ffPath,
	})
}

func (s *Server) handleInstallFFmpeg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	binDir := filepath.Join(executableDir(), "bin")
	installedPath, err := DownloadFFmpegStandalone(binDir)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	s.config.mu.Lock()
	s.config.FFmpegPath = installedPath
	s.config.mu.Unlock()
	_ = s.config.Save()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    installedPath,
	})
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.config.Port)
	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: s.setupRoutes(),
	}
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Shutdown() {
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(ctx)
	}
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.updater == nil {
		s.updater = NewUpdater()
	}

	force := r.URL.Query().Get("force") == "1" || strings.EqualFold(r.URL.Query().Get("force"), "true")
	info, err := s.updater.CheckForUpdate(r.Context(), force)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.updater == nil {
		s.updater = NewUpdater()
	}

	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	w.Header().Set("Content-Type", "application/json")
	if s.queue != nil && s.queue.countDownloading() > 0 && !req.Force {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Active downloads are in progress. Pause or finish them before updating.",
		})
		return
	}

	info, err := s.updater.CheckForUpdate(r.Context(), true)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if !info.UpdateAvailable {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"upToDate": true,
			"version":  info.CurrentVersion,
		})
		return
	}

	if err := s.updater.ApplyUpdate(r.Context(), info, nil); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"version":    info.LatestVersion,
		"restarting": s.onRestartForUpdate != nil,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if s.onRestartForUpdate != nil {
		restartFn := s.onRestartForUpdate
		go func() {
			time.Sleep(500 * time.Millisecond)
			restartFn()
		}()
	}
}
