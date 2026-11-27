package main

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// --- 1A. Theme Constants & Resolver ---

const (
	ThemeAuto  = "auto"
	ThemeLight = "light"
	ThemeDark  = "dark"
)

func normalizeTheme(theme string) string {
	switch strings.ToLower(strings.TrimSpace(theme)) {
	case ThemeLight:
		return ThemeLight
	case ThemeAuto:
		return ThemeAuto
	default:
		return ThemeDark
	}
}

func resolveDarkMode(themePref string, systemDark bool) bool {
	switch normalizeTheme(themePref) {
	case ThemeDark:
		return true
	case ThemeLight:
		return false
	default:
		return systemDark
	}
}

// --- 1B. Categories & Multi-Term Search Filtering ---

const (
	CategoryAll       = "all"
	CategoryActive    = "active"
	CategoryVideo     = "video"
	CategoryMusic     = "music"
	CategoryArchives  = "archives"
	CategoryPrograms  = "programs"
	CategoryDocuments = "documents"
	CategoryImages    = "images"
	CategoryCompleted = "completed"
	CategoryFailed    = "failed"
)

type uiCategoryDef struct {
	Key   string
	Label string
}

var uiCategories = []uiCategoryDef{
	{CategoryAll, "All Downloads"},
	{CategoryActive, "Active / Queued"},
	{CategoryVideo, "Videos"},
	{CategoryMusic, "Music / Audio"},
	{CategoryArchives, "Archives"},
	{CategoryPrograms, "Programs"},
	{CategoryDocuments, "Documents"},
	{CategoryImages, "Images"},
	{CategoryCompleted, "Completed"},
	{CategoryFailed, "Failed / Canceled"},
}

func extractTaskExt(t *DownloadTask) string {
	if t == nil {
		return ""
	}
	checkCandidate := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		for {
			lower := strings.ToLower(s)
			switch {
			case strings.HasSuffix(lower, ".part"):
				s = s[:len(s)-5]
			case strings.HasSuffix(lower, ".turbo"):
				s = s[:len(s)-6]
			case strings.HasSuffix(lower, ".ytdl"):
				s = s[:len(s)-5]
			default:
				goto doneStrip
			}
		}
	doneStrip:
		ext := strings.ToLower(filepath.Ext(s))
		if len(ext) >= 2 && len(ext) <= 12 && !strings.ContainsAny(ext, " /\\?") {
			return ext
		}
		return ""
	}

	if ext := checkCandidate(t.SavePath); ext != "" {
		return ext
	}
	if ext := checkCandidate(t.FileName); ext != "" {
		return ext
	}
	if ext := checkCandidate(t.Title); ext != "" {
		return ext
	}
	if u, err := url.Parse(strings.TrimSpace(t.URL)); err == nil && u.Path != "" {
		if ext := checkCandidate(path.Base(u.Path)); ext != "" {
			return ext
		}
	}
	return ""
}

// classifyTaskFileCategory assigns a media/file type category to a task based on quality,
// filename extension, or engine metadata.
func classifyTaskFileCategory(t *DownloadTask) string {
	if t == nil {
		return "other"
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t.Quality)), "audio") {
		return CategoryMusic
	}

	switch extractTaskExt(t) {
	case ".mp4", ".mkv", ".webm", ".mov", ".avi", ".flv", ".ts", ".m4v", ".3gp", ".wmv":
		return CategoryVideo
	case ".mp3", ".m4a", ".opus", ".flac", ".wav", ".aac", ".ogg", ".wma":
		return CategoryMusic
	case ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz", ".iso", ".dmg", ".tgz":
		return CategoryArchives
	case ".exe", ".msi", ".apk", ".deb", ".rpm", ".appimage", ".pkg", ".bat", ".ps1":
		return CategoryPrograms
	case ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".txt", ".epub", ".mobi", ".csv", ".md", ".rtf":
		return CategoryDocuments
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif":
		return CategoryImages
	}

	if strings.EqualFold(strings.TrimSpace(t.Engine), EngineYtDlp) {
		return CategoryVideo
	}
	return "other"
}

func taskMatchesCategory(t *DownloadTask, category string) bool {
	if t == nil {
		return false
	}
	cat := strings.ToLower(strings.TrimSpace(category))
	switch cat {
	case "", CategoryAll:
		return true
	case CategoryActive:
		return t.Status == StatusDownloading || t.Status == StatusQueued || t.Status == StatusPaused
	case CategoryCompleted:
		return t.Status == StatusCompleted
	case CategoryFailed:
		return t.Status == StatusError || t.Status == StatusCanceled
	default:
		return classifyTaskFileCategory(t) == cat
	}
}

func taskMatchesSearch(t *DownloadTask, query string) bool {
	if t == nil {
		return false
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return true
	}

	haystack := strings.ToLower(strings.Join([]string{
		t.Title,
		t.FileName,
		t.URL,
		t.SubDir,
		t.Quality,
		t.Status,
		t.ErrorMessage,
	}, "\n"))

	for _, tok := range tokens {
		if !strings.Contains(haystack, tok) {
			return false
		}
	}
	return true
}

func filterTasksForUI(tasks []*DownloadTask, category, query string) []*DownloadTask {
	out := make([]*DownloadTask, 0, len(tasks))
	for _, t := range tasks {
		if taskMatchesCategory(t, category) && taskMatchesSearch(t, query) {
			out = append(out, t)
		}
	}
	return out
}

func countTasksByCategory(tasks []*DownloadTask) map[string]int {
	counts := make(map[string]int, len(uiCategories))
	for _, cat := range uiCategories {
		counts[cat.Key] = 0
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		counts[CategoryAll]++
		if t.Status == StatusDownloading || t.Status == StatusQueued || t.Status == StatusPaused {
			counts[CategoryActive]++
		} else if t.Status == StatusCompleted {
			counts[CategoryCompleted]++
		} else if t.Status == StatusError || t.Status == StatusCanceled {
			counts[CategoryFailed]++
		}
		fileCat := classifyTaskFileCategory(t)
		if fileCat != "other" {
			counts[fileCat]++
		}
	}
	return counts
}

// --- 1C. One-Shot "When Done" Post-Download Action ---

const (
	WhenDoneNone       = "none"
	WhenDoneOpenFile   = "open_file"
	WhenDoneOpenFolder = "open_folder"
	WhenDoneSleep      = "sleep"
	WhenDoneShutdown   = "shutdown"
)

type whenDonePreset struct {
	Value string
	Label string
}

var whenDonePresets = []whenDonePreset{
	{WhenDoneNone, "Do Nothing"},
	{WhenDoneOpenFile, "Open Completed File"},
	{WhenDoneOpenFolder, "Open Downloads Folder"},
	{WhenDoneSleep, "Put PC to Sleep"},
	{WhenDoneShutdown, "Shut Down PC (30s countdown)"},
}

func normalizeWhenDoneAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case WhenDoneOpenFile, "open", "openfile":
		return WhenDoneOpenFile
	case WhenDoneOpenFolder, "folder", "openfolder":
		return WhenDoneOpenFolder
	case WhenDoneSleep, "suspend":
		return WhenDoneSleep
	case WhenDoneShutdown, "poweroff":
		return WhenDoneShutdown
	default:
		return WhenDoneNone
	}
}

func whenDoneLabel(action string) string {
	norm := normalizeWhenDoneAction(action)
	for _, p := range whenDonePresets {
		if p.Value == norm {
			return p.Label
		}
	}
	return "Do Nothing"
}

// --- 1D. Per-Connection Segment Progress Telemetry ---

// SegmentProgress represents live progress of an individual byte range / connection worker.
type SegmentProgress struct {
	Start   int64   `json:"start"`
	Current int64   `json:"current"`
	End     int64   `json:"end"`
	Active  bool    `json:"active"`
	RateBps float64 `json:"rateBps,omitempty"`
}
