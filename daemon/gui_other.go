//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func activateExistingInstance(cfg *Config) bool {
	return false
}

func runPlatformApp(cfg *Config, qm *QueueManager, srv *Server, startInTray bool, headless bool) {
	ytPath, ytErr := resolveYtDlpBinary(cfg)
	if ytErr != nil {
		fmt.Printf("  [!] yt-dlp status: Not found in PATH.\n")
		fmt.Printf("      You can install it automatically from the Web UI or run scripts/setup_ytdlp.sh\n")
	} else {
		fmt.Printf("  [+] yt-dlp binary: %s\n", ytPath)
		go PrewarmYouTubeSolverCache(cfg)
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
	fmt.Println("  Ready and listening for connections from Browser Extension and Web UI...")
	fmt.Println("  Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n[*] Shutting down GrabAll gracefully...")
		qm.Stop()
		qm.save()
		srv.Shutdown()
		os.Exit(0)
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("[-] Server failed: %v", err)
	}
}
