package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const Banner = `
  ======================================================
     ⚡ GrabAll Download Manager (Ultra-Lean Edition)
  ======================================================
`

func main() {
	var (
		portFlag        = flag.Int("port", 0, "Server port (default: 17890 or config)")
		configPathFlag  = flag.String("config", "config.json", "Path to config file")
		dataPathFlag    = flag.String("data", "queue.json", "Path to queue data file")
		downloadDirFlag = flag.String("downloads", "", "Directory to save downloaded files")
	)
	flag.Parse()

	fmt.Print(Banner)

	// Load configuration
	cfg, err := LoadConfig(*configPathFlag)
	if err != nil {
		log.Fatalf("[-] Failed to load config: %v", err)
	}

	if *portFlag > 0 {
		cfg.Port = *portFlag
	}
	if *downloadDirFlag != "" {
		cfg.DownloadDir = *downloadDirFlag
	}

	// Ensure download directory exists
	if err := os.MkdirAll(cfg.GetDownloadDir(), 0755); err != nil {
		log.Printf("[!] Warning: Could not create download dir %s: %v", cfg.GetDownloadDir(), err)
	}

	// Check yt-dlp
	ytPath, ytErr := resolveYtDlpBinary(cfg)
	if ytErr != nil {
		fmt.Printf("  [!] yt-dlp status: Not found in PATH.\n")
		fmt.Printf("      You can install it automatically from the Web UI or run scripts/setup_ytdlp.sh\n")
	} else {
		fmt.Printf("  [+] yt-dlp binary: %s\n", ytPath)
	}

	ffmpegPath := resolveFFmpegBinary(cfg)
	if ffmpegPath != "" {
		fmt.Printf("  [+] ffmpeg binary: %s\n", ffmpegPath)
	} else {
		fmt.Printf("  [*] ffmpeg: Not found (optional, only needed when merging split audio/video)\n")
	}

	fmt.Printf("\n  🚀 Web Dashboard: http://127.0.0.1:%d\n", cfg.Port)
	fmt.Printf("  🔑 Auth Token:   %s\n", cfg.GetToken())
	fmt.Printf("  📁 Save Folder:  %s\n", filepath.Clean(cfg.GetDownloadDir()))
	fmt.Printf("  ⚙️ Max Workers:  %d concurrent\n\n", cfg.GetMaxConcurrent())
	fmt.Println("  Ready and listening for connections from Brave Extension and Web UI...")
	fmt.Println("  Press Ctrl+C to stop.")

	// SSE Broker
	broker := NewSSEBroker()

	// Queue Manager
	qm := NewQueueManager(cfg, *dataPathFlag, broker)

	// Server
	srv := NewServer(cfg, qm, broker)

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n[*] Shutting down GrabAll gracefully...")
		qm.Stop()
		qm.save()
		os.Exit(0)
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("[-] Server failed: %v", err)
	}
}
