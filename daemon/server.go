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
	config       *Config
	queue        *QueueManager
	broker       *SSEBroker
	httpSrv      *http.Server
	onShowWindow func()
}

func NewServer(cfg *Config, qm *QueueManager, broker *SSEBroker) *Server {
	return &Server{
		config: cfg,
		queue:  qm,
		broker: broker,
	}
}

func (s *Server) SetShowWindowHandler(fn func()) {
	s.onShowWindow = fn
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
	mux.HandleFunc("/api/show-window", s.handleShowWindow)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/add", s.handleAdd)
	mux.HandleFunc("/api/pause", s.handlePause)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/delete", s.handleDelete)
	mux.HandleFunc("/api/clear-completed", s.handleClearCompleted)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/install-ytdlp", s.handleInstallYtDlp)
	mux.HandleFunc("/api/install-ffmpeg", s.handleInstallFFmpeg)
	mux.HandleFunc("/api/open-folder", s.handleOpenFolder)

	return s.corsAndAuthMiddleware(mux)
}

func isBrowserExtensionOrigin(origin string) bool {
	lower := strings.ToLower(strings.TrimSpace(origin))
	return strings.HasPrefix(lower, "chrome-extension://") ||
		strings.HasPrefix(lower, "moz-extension://") ||
		strings.HasPrefix(lower, "safari-web-extension://")
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

	// 4. Same-origin requests from the embedded web dashboard (Chrome/Brave always
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
		"version":         "2.0.0",
		"nativeGui":       s.onShowWindow != nil,
		"downloadDir":     s.config.GetDownloadDir(),
		"maxConcurrent":   s.config.GetMaxConcurrent(),
		"ytDlpAvailable":  hasYtDlp == nil,
		"ffmpegAvailable": hasFFmpeg,
	})
}

// handlePair allows browser extensions (Origin: chrome-extension://... or moz-extension://...)
// to pair automatically with zero manual token copy-pasting, while blocking regular web pages.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !isBrowserExtensionOrigin(origin) {
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

func resolveTaskRevealPaths(cfg *Config, qm *QueueManager, taskID string) (filePath, dirPath string) {
	dirPath = cfg.GetDownloadDir()
	if taskID == "" {
		return "", dirPath
	}
	if task := qm.GetByID(taskID); task != nil {
		task.mu.Lock()
		sp := task.SavePath
		sub := task.SubDir
		task.mu.Unlock()
		if sp != "" {
			if _, err := os.Stat(sp); err == nil {
				return sp, filepath.Dir(sp)
			}
		}
		if sub != "" {
			cand := filepath.Join(dirPath, sanitizeFileName(sub))
			if _, err := os.Stat(cand); err == nil {
				dirPath = cand
			}
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

		s.config.mu.RLock()
		resp := map[string]interface{}{
			"port":            s.config.Port,
			"authToken":       s.config.AuthToken,
			"downloadDir":     s.config.DownloadDir,
			"maxConcurrent":   s.config.MaxConcurrent,
			"ytDlpPath":       s.config.YtDlpPath,
			"ffmpegPath":      s.config.FFmpegPath,
			"defaultEngine":   s.config.DefaultEngine,
			"ytDlpAvailable":  hasYtDlp == nil,
			"ffmpegAvailable": hasFFmpeg,
		}
		s.config.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			DownloadDir   string `json:"downloadDir"`
			MaxConcurrent int    `json:"maxConcurrent"`
			DefaultEngine string `json:"defaultEngine"`
			YtDlpPath     string `json:"ytDlpPath"`
			FFmpegPath    string `json:"ffmpegPath"`
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
		if req.YtDlpPath != "" {
			s.config.YtDlpPath = req.YtDlpPath
		}
		if req.FFmpegPath != "" {
			s.config.FFmpegPath = req.FFmpegPath
		}
		s.config.mu.Unlock()

		_ = s.config.Save()

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
