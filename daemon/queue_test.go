package main

import (
	"path/filepath"
	"testing"
)

func TestEngineDetection(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"https://example.com/archive.zip", EngineDirect},
		{"https://example.com/installer.exe", EngineDirect},
		{"https://example.com/manual.pdf?download=true", EngineDirect},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", EngineYtDlp},
		{"https://vimeo.com/12345678", EngineYtDlp},
		{"https://stream.provider.com/live/playlist.m3u8", EngineYtDlp},
		{"https://cdn.example.com/dash/manifest.mpd", EngineYtDlp},
		{"https://tiktok.com/@user/video/123", EngineYtDlp},
	}

	for _, tt := range tests {
		result := detectEngine(tt.url)
		if result != tt.expected {
			t.Errorf("detectEngine(%q) = %s, expected %s", tt.url, result, tt.expected)
		}
	}
}

func TestQueueManagerBasicOps(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "testtoken",
		DownloadDir:   tmpDir,
		MaxConcurrent: 2,
		DefaultEngine: "auto",
	}

	dataFile := filepath.Join(tmpDir, "queue.json")
	broker := NewSSEBroker()
	qm := NewQueueManager(cfg, dataFile, broker)
	defer qm.Stop()

	// Add direct task
	task, err := qm.AddTask(AddRequest{
		URL:      "https://example.com/test.zip",
		FileName: "test.zip",
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}

	if task.Status != StatusQueued && task.Status != StatusDownloading {
		t.Errorf("Unexpected status: %s", task.Status)
	}

	// Verify get
	got := qm.GetByID(task.ID)
	if got == nil || got.URL != task.URL {
		t.Fatalf("GetByID failed: got %v", got)
	}

	// Pause
	qm.PauseTask(task.ID)
	got = qm.GetByID(task.ID)
	if got.Status != StatusPaused {
		t.Errorf("Expected status paused, got %s", got.Status)
	}

	// Resume
	qm.ResumeTask(task.ID)
	got = qm.GetByID(task.ID)
	if got.Status != StatusQueued && got.Status != StatusDownloading {
		t.Errorf("Expected status queued/downloading after resume, got %s", got.Status)
	}

	// Cancel
	qm.CancelTask(task.ID)
	got = qm.GetByID(task.ID)
	if got.Status != StatusCanceled {
		t.Errorf("Expected status canceled, got %s", got.Status)
	}

	// Clear completed
	cleared := qm.ClearCompleted()
	if cleared != 1 {
		t.Errorf("Expected 1 task cleared, got %d", cleared)
	}

	all := qm.GetAll()
	if len(all) != 0 {
		t.Errorf("Expected 0 tasks remaining, got %d", len(all))
	}
}

func TestYouTubeAndQuality(t *testing.T) {
	if !isYouTubeURL("https://www.youtube.com/watch?v=jTJxwQlb_jY") {
		t.Errorf("Expected youtube watch URL to be detected")
	}
	if !isYouTubeURL("https://youtu.be/jTJxwQlb_jY") {
		t.Errorf("Expected youtu.be URL to be detected")
	}
	if isYouTubeURL("https://vimeo.com/12345") {
		t.Errorf("Expected vimeo URL not to be detected as YouTube")
	}

	tmpDir := t.TempDir()
	cfg := &Config{
		Port:          17890,
		AuthToken:     "testtoken",
		DownloadDir:   tmpDir,
		MaxConcurrent: 1,
		DefaultEngine: "auto",
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), NewSSEBroker())
	defer qm.Stop()

	task, err := qm.AddTask(AddRequest{
		URL:     "https://www.youtube.com/watch?v=jTJxwQlb_jY",
		Engine:  EngineYtDlp,
		Quality: "audio-mp3",
	})
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	if task.Quality != "audio-mp3" {
		t.Errorf("Expected quality audio-mp3, got %q", task.Quality)
	}
	qm.CancelTask(task.ID)
}
