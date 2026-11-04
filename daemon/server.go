package main

import (
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
	config  *Config
	queue   *QueueManager
	broker  *SSEBroker
	httpSrv *http.Server
}

func NewServer(cfg *Config, qm *QueueManager, broker *SSEBroker) *Server {
	return &Server{
		config: cfg,
		queue:  qm,
		broker: broker,
	}
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

	return s.corsAndAuthMiddleware(mux)
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

		// Check auth for /api/ routes (except /api/ping which is public for health checks)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/ping" {
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

	// 4. Local origin (requests from the dashboard itself on localhost)
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
		// Native client without Origin/Referer (e.g. curl or internal client on 127.0.0.1)
		// Require token if it's external, but if token is empty allow
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
		"version":         "1.0.0",
		"downloadDir":     s.config.GetDownloadDir(),
		"maxConcurrent":   s.config.GetMaxConcurrent(),
		"ytDlpAvailable":  hasYtDlp == nil,
		"ffmpegAvailable": hasFFmpeg,
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

	clientChan := make(chan []byte, 10)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "Invalid request JSON: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	task, err := s.queue.AddTask(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"task":    task,
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

	cwd, err := os.Getwd()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	binDir := filepath.Join(cwd, "bin")
	installedPath, err := DownloadYtDlpStandalone(binDir)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	s.config.mu.Lock()
	s.config.YtDlpPath = installedPath
	s.config.mu.Unlock()
	_ = s.config.Save()

	w.Header().Set("Content-Type", "application/json")
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
