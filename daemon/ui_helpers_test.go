package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestThemeNormalizationAndDarkModeResolution(t *testing.T) {
	normCases := []struct {
		in   string
		want string
	}{
		{"", ThemeAuto},
		{"AUTO", ThemeAuto},
		{"Dark", ThemeDark},
		{"  dark ", ThemeDark},
		{"LIGHT", ThemeLight},
		{" light ", ThemeLight},
		{"unknown-value", ThemeAuto},
	}
	for _, tc := range normCases {
		if got := normalizeTheme(tc.in); got != tc.want {
			t.Errorf("normalizeTheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	resolveCases := []struct {
		pref       string
		systemDark bool
		want       bool
	}{
		{ThemeAuto, false, false},
		{ThemeAuto, true, true},
		{ThemeLight, false, false},
		{ThemeLight, true, false},
		{ThemeDark, false, true},
		{ThemeDark, true, true},
	}
	for _, tc := range resolveCases {
		if got := resolveDarkMode(tc.pref, tc.systemDark); got != tc.want {
			t.Errorf("resolveDarkMode(%q, %v) = %v, want %v", tc.pref, tc.systemDark, got, tc.want)
		}
	}

	// Config persistence and load normalization
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got := cfg.GetTheme(); got != ThemeAuto {
		t.Fatalf("expected default theme %q, got %q", ThemeAuto, got)
	}

	cfg.SetTheme("DARK")
	if got := cfg.GetTheme(); got != ThemeDark {
		t.Fatalf("expected GetTheme()=%q after SetTheme, got %q", ThemeDark, got)
	}

	reloaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig reload failed: %v", err)
	}
	if got := reloaded.GetTheme(); got != ThemeDark {
		t.Errorf("expected persisted theme %q on reload, got %q", ThemeDark, got)
	}
}

func TestCategoryClassificationAndSearchFiltering(t *testing.T) {
	videoTask := &DownloadTask{
		ID:       "v1",
		Title:    "Go Systems Talk 4K",
		FileName: "systems_talk.mkv.part",
		URL:      "https://video.example/watch?v=123",
		Engine:   EngineYtDlp,
		Quality:  "2160",
		Status:   StatusDownloading,
	}
	musicTask := &DownloadTask{
		ID:       "m1",
		Title:    "LoFi Mix",
		FileName: "lofi_mix.webm",
		URL:      "https://audio.example/track.webm",
		Engine:   EngineYtDlp,
		Quality:  "audio-mp3",
		Status:   StatusCompleted,
	}
	flacTask := &DownloadTask{
		ID:       "m2",
		Title:    "Concert Recording",
		FileName: "concert.flac",
		Engine:   EngineDirect,
		Status:   StatusQueued,
	}
	archiveTask := &DownloadTask{
		ID:       "a1",
		Title:    "Ubuntu ISO Archive",
		FileName: "archive.tar.gz.turbo",
		Engine:   EngineDirect,
		Status:   StatusPaused,
	}
	programTask := &DownloadTask{
		ID:       "p1",
		Title:    "GrabAll Installer",
		FileName: "GrabAllSetup.msi",
		Engine:   EngineDirect,
		Status:   StatusCompleted,
	}
	docTask := &DownloadTask{
		ID:     "d1",
		Title:  "Architecture Spec",
		URL:    "https://docs.example/files/spec.pdf?dl=1",
		Engine: EngineDirect,
		Status: StatusError,
	}
	imgTask := &DownloadTask{
		ID:       "i1",
		Title:    "Wallpaper",
		SavePath: "/tmp/wallpaper.png",
		Engine:   EngineDirect,
		Status:   StatusCanceled,
	}
	otherTask := &DownloadTask{
		ID:       "o1",
		Title:    "Opaque Blob",
		FileName: "dataset.bin",
		Engine:   EngineDirect,
		Status:   StatusCompleted,
	}

	classCases := []struct {
		name string
		task *DownloadTask
		want string
	}{
		{"video", videoTask, CategoryVideo},
		{"audio-quality-override", musicTask, CategoryMusic},
		{"flac", flacTask, CategoryMusic},
		{"archive-turbo-suffix", archiveTask, CategoryArchives},
		{"program", programTask, CategoryPrograms},
		{"document-url", docTask, CategoryDocuments},
		{"image-savepath", imgTask, CategoryImages},
		{"other", otherTask, "other"},
	}
	for _, tc := range classCases {
		if got := classifyTaskFileCategory(tc.task); got != tc.want {
			t.Errorf("%s: classifyTaskFileCategory() = %q, want %q", tc.name, got, tc.want)
		}
	}

	allTasks := []*DownloadTask{videoTask, musicTask, flacTask, archiveTask, programTask, docTask, imgTask, otherTask}

	if got := filterTasksForUI(allTasks, CategoryAll, ""); len(got) != 8 {
		t.Errorf("CategoryAll expected 8 tasks, got %d", len(got))
	}
	if got := filterTasksForUI(allTasks, CategoryActive, ""); len(got) != 3 {
		t.Errorf("CategoryActive expected 3 tasks (downloading/queued/paused), got %d", len(got))
	}
	if got := filterTasksForUI(allTasks, CategoryCompleted, ""); len(got) != 3 {
		t.Errorf("CategoryCompleted expected 3 tasks, got %d", len(got))
	}
	if got := filterTasksForUI(allTasks, CategoryFailed, ""); len(got) != 2 {
		t.Errorf("CategoryFailed expected 2 tasks (error/canceled), got %d", len(got))
	}
	if got := filterTasksForUI(allTasks, CategoryVideo, ""); len(got) != 1 || got[0].ID != "v1" {
		t.Errorf("CategoryVideo mismatch: %+v", got)
	}
	if got := filterTasksForUI(allTasks, CategoryMusic, ""); len(got) != 2 {
		t.Errorf("CategoryMusic expected 2 tasks, got %d", len(got))
	}

	// Multi-term search spanning title, URL, and quality case-insensitively
	searchHit := filterTasksForUI(allTasks, CategoryAll, "2160 video.example systems")
	if len(searchHit) != 1 || searchHit[0].ID != "v1" {
		t.Errorf("multi-term search expected [v1], got %+v", searchHit)
	}
	if miss := filterTasksForUI(allTasks, CategoryMusic, "2160"); len(miss) != 0 {
		t.Errorf("category + search mismatch expected 0 results, got %d", len(miss))
	}

	counts := countTasksByCategory(allTasks)
	if counts[CategoryAll] != 8 || counts[CategoryActive] != 3 || counts[CategoryCompleted] != 3 ||
		counts[CategoryFailed] != 2 || counts[CategoryMusic] != 2 || counts[CategoryVideo] != 1 {
		t.Errorf("unexpected countTasksByCategory map: %+v", counts)
	}
}

func TestWhenDoneOneShotLifecycleAndAPI(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	qm := NewQueueManager(cfg, filepath.Join(tmpDir, "queue.json"), nil)
	defer qm.Stop()

	type triggerEvent struct {
		action string
		taskID string
	}
	var mu sync.Mutex
	var events []triggerEvent
	doneCh := make(chan triggerEvent, 4)

	qm.SetWhenDoneHandler(func(action string, lastTask *DownloadTask) {
		ev := triggerEvent{action: action}
		if lastTask != nil {
			ev.taskID = lastTask.ID
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		doneCh <- ev
	})

	task1 := &DownloadTask{ID: "t1", URL: "https://example.com/1.bin", Status: StatusCompleted}
	task2 := &DownloadTask{ID: "t2", URL: "https://example.com/2.bin", Status: StatusDownloading}

	qm.tasksMu.Lock()
	qm.tasks = []*DownloadTask{task1, task2}
	qm.tasksMu.Unlock()

	if applied := qm.SetWhenDoneAction(WhenDoneOpenFolder); applied != WhenDoneOpenFolder {
		t.Fatalf("expected SetWhenDoneAction=%q, got %q", WhenDoneOpenFolder, applied)
	}

	// Completing task1 while task2 is still downloading must NOT trigger yet.
	qm.maybeTriggerWhenDone(task1)
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if len(events) != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 triggers while task2 still downloading, got %+v", events)
	}
	mu.Unlock()
	if got := qm.GetWhenDoneAction(); got != WhenDoneOpenFolder {
		t.Fatalf("expected whenDoneAction to stay %q while task2 active, got %q", WhenDoneOpenFolder, got)
	}

	// Mark task2 completed and trigger completion check -> fires once and resets to WhenDoneNone.
	qm.tasksMu.Lock()
	task2.Status = StatusCompleted
	qm.tasksMu.Unlock()
	qm.maybeTriggerWhenDone(task2)

	select {
	case ev := <-doneCh:
		if ev.action != WhenDoneOpenFolder || ev.taskID != "t2" {
			t.Errorf("unexpected trigger event: %+v", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for when-done handler")
	}

	if got := qm.GetWhenDoneAction(); got != WhenDoneNone {
		t.Errorf("expected one-shot reset to %q after firing, got %q", WhenDoneNone, got)
	}

	// HTTP API checks (/api/when-done + /api/config)
	srv := NewServer(cfg, qm, NewSSEBroker())
	handler := srv.setupRoutes()

	getWDReq := httptest.NewRequest("GET", "/api/when-done", nil)
	getWDReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	getWDRec := httptest.NewRecorder()
	handler.ServeHTTP(getWDRec, getWDReq)
	if getWDRec.Code != 200 {
		t.Fatalf("GET /api/when-done status=%d body=%s", getWDRec.Code, getWDRec.Body.String())
	}
	var wdResp map[string]string
	if err := json.Unmarshal(getWDRec.Body.Bytes(), &wdResp); err != nil {
		t.Fatalf("decode GET /api/when-done: %v", err)
	}
	if wdResp["whenDoneAction"] != WhenDoneNone {
		t.Errorf("GET /api/when-done expected %q, got %q", WhenDoneNone, wdResp["whenDoneAction"])
	}

	postWDBody, _ := json.Marshal(map[string]string{"action": "sleep"})
	postWDReq := httptest.NewRequest("POST", "/api/when-done", bytes.NewReader(postWDBody))
	postWDReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	postWDRec := httptest.NewRecorder()
	handler.ServeHTTP(postWDRec, postWDReq)
	if postWDRec.Code != 200 {
		t.Fatalf("POST /api/when-done status=%d body=%s", postWDRec.Code, postWDRec.Body.String())
	}
	var postResp struct {
		Success        bool   `json:"success"`
		WhenDoneAction string `json:"whenDoneAction"`
	}
	if err := json.Unmarshal(postWDRec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("decode POST /api/when-done: %v", err)
	}
	if !postResp.Success || postResp.WhenDoneAction != WhenDoneSleep {
		t.Errorf("unexpected POST /api/when-done response: %+v", postResp)
	}

	cfgPostReq := httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"theme":"dark"}`))
	cfgPostReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	cfgPostRec := httptest.NewRecorder()
	handler.ServeHTTP(cfgPostRec, cfgPostReq)
	if cfgPostRec.Code != 200 {
		t.Fatalf("POST /api/config status=%d body=%s", cfgPostRec.Code, cfgPostRec.Body.String())
	}

	cfgGetReq := httptest.NewRequest("GET", "/api/config", nil)
	cfgGetReq.Header.Set("X-Daemon-Token", cfg.GetToken())
	cfgGetRec := httptest.NewRecorder()
	handler.ServeHTTP(cfgGetRec, cfgGetReq)
	if cfgGetRec.Code != 200 {
		t.Fatalf("GET /api/config status=%d body=%s", cfgGetRec.Code, cfgGetRec.Body.String())
	}
	var cfgResp map[string]interface{}
	if err := json.Unmarshal(cfgGetRec.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode GET /api/config: %v", err)
	}
	if cfgResp["theme"] != ThemeDark {
		t.Errorf("GET /api/config theme expected %q, got %v", ThemeDark, cfgResp["theme"])
	}
	if cfgResp["whenDoneAction"] != WhenDoneSleep {
		t.Errorf("GET /api/config whenDoneAction expected %q, got %v", WhenDoneSleep, cfgResp["whenDoneAction"])
	}
}

func TestRangeSchedulerSnapshotSegments(t *testing.T) {
	const totalSize = int64(10 << 20)
	const minSeg = int64(1 << 20)

	s := newRangeScheduler(totalSize, nil, 4, minSeg)
	seg0 := s.NextOrSteal(0, minSeg)
	if seg0 == nil {
		t.Fatal("expected worker 0 to claim initial segment")
	}
	seg1 := s.NextOrSteal(1, minSeg)
	if seg1 == nil {
		t.Fatal("expected worker 1 to claim second segment")
	}

	s.Advance(seg0, 512<<10)
	s.Advance(seg1, 256<<10)

	snaps := s.SnapshotSegments()
	if len(snaps) != 4 {
		t.Fatalf("expected 4 segment snapshots, got %d: %+v", len(snaps), snaps)
	}

	var activeCount, pendingCount int
	for i, seg := range snaps {
		if i > 0 && seg.Start < snaps[i-1].Start {
			t.Errorf("segments not sorted by Start at index %d: prev=%d cur=%d", i, snaps[i-1].Start, seg.Start)
		}
		if seg.Active {
			activeCount++
			if seg.Start == seg0.start && seg.Current != seg0.start+(512<<10) {
				t.Errorf("seg0 Current=%d, want %d", seg.Current, seg0.start+(512<<10))
			}
			if seg.Start == seg1.start && seg.Current != seg1.start+(256<<10) {
				t.Errorf("seg1 Current=%d, want %d", seg.Current, seg1.start+(256<<10))
			}
		} else {
			pendingCount++
			if seg.Current != seg.Start {
				t.Errorf("pending segment should have Current == Start, got Current=%d Start=%d", seg.Current, seg.Start)
			}
		}
	}
	if activeCount != 2 || pendingCount != 2 {
		t.Errorf("expected 2 active and 2 pending segments, got active=%d pending=%d", activeCount, pendingCount)
	}

	// Releasing partial seg1 must preserve downloaded bytes in completed and unassign remaining gap cleanly.
	origSeg1Start := seg1.start
	s.Release(seg1)
	afterRelease := s.SnapshotSegments()
	var foundCompletedPrefix, foundReleasedTail bool
	for _, seg := range afterRelease {
		if seg.Start == origSeg1Start && seg.End == origSeg1Start+(256<<10)-1 && !seg.Active && seg.Current == origSeg1Start+(256<<10) {
			foundCompletedPrefix = true
		}
		if seg.Start == origSeg1Start+(256<<10) && !seg.Active && seg.Current == seg.Start {
			foundReleasedTail = true
		}
	}
	if !foundCompletedPrefix || !foundReleasedTail {
		t.Errorf("expected completed prefix and unassigned tail after Release, got %+v", afterRelease)
	}
}
