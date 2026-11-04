package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"sync"
)

type Config struct {
	Port          int    `json:"port"`
	AuthToken     string `json:"authToken"`
	DownloadDir   string `json:"downloadDir"`
	MaxConcurrent int    `json:"maxConcurrent"`
	YtDlpPath     string `json:"ytDlpPath"`
	FFmpegPath    string `json:"ffmpegPath"`
	DefaultEngine string `json:"defaultEngine"` // "auto", "direct", "ytdlp"

	mu         sync.RWMutex `json:"-"`
	configPath string       `json:"-"`
}

func defaultDownloadDir() string {
	usr, err := user.Current()
	if err == nil && usr.HomeDir != "" {
		downloads := filepath.Join(usr.HomeDir, "Downloads")
		if _, err := os.Stat(downloads); err == nil {
			return downloads
		}
	}
	cwd, err := os.Getwd()
	if err == nil {
		return filepath.Join(cwd, "downloads")
	}
	return "downloads"
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
		configPath:    configPath,
	}

	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
		cfg.configPath = configPath
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
		}
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
