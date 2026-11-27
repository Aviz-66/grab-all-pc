package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
)

const (
	SpeedModeTurbo   = "turbo"
	SpeedModeRegular = "regular"

	VideoCodecH264 = "h264"
	VideoCodecVP9  = "vp9"
	VideoCodecAV1  = "av1"
	VideoCodecAuto = "auto"
)

func normalizeSpeedMode(mode string) string {
	if mode == SpeedModeRegular {
		return SpeedModeRegular
	}
	return SpeedModeTurbo
}

func normalizeVideoCodec(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "vp9", "vp09":
		return VideoCodecVP9
	case "av1", "av01":
		return VideoCodecAV1
	case "auto", "best", "any":
		return VideoCodecAuto
	case "h264", "h-264", "h.264", "avc", "avc1", "264", "":
		return VideoCodecH264
	default:
		return VideoCodecH264
	}
}

type Config struct {
	Port              int    `json:"port"`
	AuthToken         string `json:"authToken"`
	DownloadDir       string `json:"downloadDir"`
	MaxConcurrent     int    `json:"maxConcurrent"`
	YtDlpPath         string `json:"ytDlpPath"`
	FFmpegPath        string `json:"ffmpegPath"`
	DefaultEngine     string `json:"defaultEngine"`       // "auto", "direct", "ytdlp"
	SpeedMode           string `json:"speedMode,omitempty"`      // "turbo" (default 6x) or "regular" (1x sequential watch-live)
	VideoCodec          string `json:"videoCodec,omitempty"`     // "h264" (default), "vp9", "av1", "auto"
	SpeedLimitKBps      int64  `json:"speedLimitKBps,omitempty"` // 0 = unlimited
	Theme               string `json:"theme,omitempty"`          // "dark" (default), "auto", "light"
	ShowProgressPopup   *bool  `json:"showProgressPopup,omitempty"`
	FirstRunPromptShown bool   `json:"firstRunPromptShown,omitempty"`
	AutoCheckUpdates    *bool  `json:"autoCheckUpdates,omitempty"`

	mu         sync.RWMutex `json:"-"`
	configPath string       `json:"-"`
}

func defaultDownloadDir() string {
	usr, err := user.Current()
	if err == nil && usr.HomeDir != "" {
		downloads := filepath.Join(usr.HomeDir, "Downloads")
		if _, err := os.Stat(downloads); err == nil {
			return filepath.Join(downloads, "grabAll")
		}
		return filepath.Join(usr.HomeDir, "Downloads", "grabAll")
	}
	cwd, err := os.Getwd()
	if err == nil {
		return filepath.Join(cwd, "downloads", "grabAll")
	}
	return filepath.Join("downloads", "grabAll")
}

func generateToken() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "graball-secure-token-default"
	}
	return hex.EncodeToString(bytes)
}

// LoadConfig loads configuration from the specified path or creates a default one.
func LoadConfig(configPath string) (*Config, error) {
	cfg := &Config{
		Port:          17890,
		AuthToken:     generateToken(),
		DownloadDir:   defaultDownloadDir(),
		MaxConcurrent: 2,
		YtDlpPath:     "yt-dlp",
		FFmpegPath:    "ffmpeg",
		DefaultEngine: "auto",
		SpeedMode:     SpeedModeTurbo,
		VideoCodec:    VideoCodecH264,
		Theme:         ThemeDark,
		configPath:    configPath,
	}

	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
		cfg.configPath = configPath
		cfg.VideoCodec = normalizeVideoCodec(cfg.VideoCodec)
		cfg.Theme = normalizeTheme(cfg.Theme)
		if cfg.SpeedLimitKBps < 0 {
			cfg.SpeedLimitKBps = 0
		}
		if cfg.Port <= 0 {
			cfg.Port = 17890
		}
		if cfg.MaxConcurrent <= 0 {
			cfg.MaxConcurrent = 2
		}
		if cfg.AuthToken == "" {
			cfg.AuthToken = generateToken()
			_ = cfg.Save()
		}
		if cfg.DownloadDir == "" {
			cfg.DownloadDir = defaultDownloadDir()
			_ = cfg.Save()
		} else if strings.EqualFold(filepath.Base(filepath.Clean(cfg.DownloadDir)), "downloads") {
			cfg.DownloadDir = filepath.Join(cfg.DownloadDir, "grabAll")
			_ = cfg.Save()
		}
		_ = os.MkdirAll(cfg.DownloadDir, 0755)
		return cfg, nil
	}

	// Create directory if it doesn't exist
	if dir := filepath.Dir(configPath); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}

	// Ensure download directory exists
	_ = os.MkdirAll(cfg.DownloadDir, 0755)

	// Save defaults
	if err := cfg.Save(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Save writes the current config to disk.
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := c.configPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, c.configPath)
}

// GetToken returns the current auth token thread-safely.
func (c *Config) GetToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AuthToken
}

// GetDownloadDir returns the configured download directory.
func (c *Config) GetDownloadDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DownloadDir
}

// GetMaxConcurrent returns the max concurrent download count.
func (c *Config) GetMaxConcurrent() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxConcurrent <= 0 {
		return 1
	}
	return c.MaxConcurrent
}

// GetShowProgressPopup returns whether the mini progress window should appear on new downloads.
func (c *Config) GetShowProgressPopup() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ShowProgressPopup == nil {
		return true
	}
	return *c.ShowProgressPopup
}

// SetShowProgressPopup updates the popup preference and saves config.
func (c *Config) SetShowProgressPopup(v bool) {
	c.mu.Lock()
	val := v
	c.ShowProgressPopup = &val
	c.mu.Unlock()
	_ = c.Save()
}

// GetSpeedMode returns "turbo" (default 6x parallel) or "regular" (1x sequential watch-live).
func (c *Config) GetSpeedMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeSpeedMode(c.SpeedMode)
}

// SetSpeedMode updates the default speed mode and saves config.
func (c *Config) SetSpeedMode(mode string) {
	c.mu.Lock()
	c.SpeedMode = normalizeSpeedMode(mode)
	c.mu.Unlock()
	_ = c.Save()
}

// GetAutoCheckUpdates returns whether GrabAll checks graball-website for PC updates on startup.
func (c *Config) GetAutoCheckUpdates() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AutoCheckUpdates == nil {
		return true
	}
	return *c.AutoCheckUpdates
}

// SetAutoCheckUpdates updates the startup update-check preference and saves config.
func (c *Config) SetAutoCheckUpdates(v bool) {
	c.mu.Lock()
	val := v
	c.AutoCheckUpdates = &val
	c.mu.Unlock()
	_ = c.Save()
}

// GetSpeedLimitKBps returns the global download speed cap in KB/s (0 = unlimited).
func (c *Config) GetSpeedLimitKBps() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.SpeedLimitKBps <= 0 {
		return 0
	}
	return c.SpeedLimitKBps
}

// SetSpeedLimitKBps updates the global download speed cap in KB/s (0 = unlimited) and saves config.
func (c *Config) SetSpeedLimitKBps(kbps int64) {
	if kbps < 0 {
		kbps = 0
	}
	c.mu.Lock()
	c.SpeedLimitKBps = kbps
	c.mu.Unlock()
	_ = c.Save()
}

// GetTheme returns the normalized UI theme preference ("auto", "light", or "dark").
func (c *Config) GetTheme() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeTheme(c.Theme)
}

// SetTheme updates the UI theme preference and persists config.
func (c *Config) SetTheme(theme string) {
	c.mu.Lock()
	c.Theme = normalizeTheme(theme)
	c.mu.Unlock()
	_ = c.Save()
}

// parseAutoStartExePath extracts the executable path from a Windows Run registry command string
// (handling both quoted `"C:\Program Files\GrabAll\graball.exe" --tray` and unquoted values).
func parseAutoStartExePath(cmdStr string) string {
	s := strings.TrimSpace(cmdStr)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		rest := s[1:]
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
		return strings.TrimSpace(rest)
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// isStartupApprovedBytesEnabled interprets Windows Task Manager / Settings StartupApproved\Run binary bytes.
// Byte 0 == 0x02 or 0x06 (or missing/empty entry) means enabled; odd values like 0x03/0x01/0x07 mean disabled.
func isStartupApprovedBytesEnabled(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	return data[0] == 0x02 || data[0] == 0x06
}
