package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const Banner = `
  ======================================================
     ⚡ GrabAll Download Manager v` + AppVersion + ` (Ultra-Lean Edition)
  ======================================================
`

func isDirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false
	}
	probe := filepath.Join(dir, fmt.Sprintf(".graball_write_check_%d", os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}

func resolveAppFilePath(flagVal, defaultName string) string {
	if flagVal != defaultName {
		return flagVal
	}
	// On Windows (especially when launched via Explorer or Startup registry where CWD is System32),
	// prefer storing config.json / queue.json beside graball.exe, falling back to %APPDATA%\GrabAll.
	if runtime.GOOS == "windows" {
		exeDir := executableDir()
		exeCandidate := filepath.Join(exeDir, defaultName)
		if _, err := os.Stat(exeCandidate); err == nil || isDirWritable(exeDir) {
			return exeCandidate
		}
		if appData := os.Getenv("APPDATA"); appData != "" {
			appDir := filepath.Join(appData, "GrabAll")
			_ = os.MkdirAll(appDir, 0755)
			return filepath.Join(appDir, defaultName)
		}
	}
	return flagVal
}

func main() {
	var (
		portFlag        = flag.Int("port", 0, "Server port (default: 17890 or config)")
		configPathFlag  = flag.String("config", "config.json", "Path to config file")
		dataPathFlag    = flag.String("data", "queue.json", "Path to queue data file")
		downloadDirFlag = flag.String("downloads", "", "Directory to save downloaded files")
		trayFlag        = flag.Bool("tray", false, "Start minimized in the Windows system tray")
		headlessFlag    = flag.Bool("headless", false, "Run background daemon only without native GUI")
		waitPIDFlag     = flag.Int("wait-pid", 0, "Wait for previous GrabAll PID to exit before starting")
	)
	flag.Parse()

	if *waitPIDFlag > 0 {
		waitForParentPIDExit(*waitPIDFlag, 6*time.Second)
	}
	if runtime.GOOS == "windows" {
		if exeDir := executableDir(); exeDir != "" {
			_ = os.Chdir(exeDir)
		}
	}
	go cleanupOldUpdatedBinary()

	if runtime.GOOS != "windows" || *headlessFlag {
		fmt.Print(Banner)
	}

	cfgPath := resolveAppFilePath(*configPathFlag, "config.json")
	dataPath := resolveAppFilePath(*dataPathFlag, "queue.json")

	// Load configuration
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("[-] Failed to load config: %v", err)
	}

	if *portFlag > 0 {
		cfg.Port = *portFlag
	}
	if *downloadDirFlag != "" {
		cfg.DownloadDir = *downloadDirFlag
	}

	// Single-instance check on Windows: bring existing GrabAll window to front if already running
	if !*headlessFlag && activateExistingInstance(cfg) {
		os.Exit(0)
	}

	// Ensure download directory exists
	if err := os.MkdirAll(cfg.GetDownloadDir(), 0755); err != nil {
		log.Printf("[!] Warning: Could not create download dir %s: %v", cfg.GetDownloadDir(), err)
	}

	// SSE Broker
	broker := NewSSEBroker()

	// Queue Manager
	qm := NewQueueManager(cfg, dataPath, broker)

	// Server
	srv := NewServer(cfg, qm, broker)

	runPlatformApp(cfg, qm, srv, *trayFlag, *headlessFlag)
}
