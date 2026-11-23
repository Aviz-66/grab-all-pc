//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
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

	srv.SetRestartForUpdateHandler(func() {
		fmt.Println("\n[*] Restarting GrabAll into newly installed version...")
		qm.Stop()
		qm.save()
		srv.Shutdown()
		if exePath, err := currentExecutablePath(); err == nil && exePath != "" {
			_ = startDetachedExecutable(exePath, buildRestartArgs(os.Args[1:], os.Getpid()))
		}
		os.Exit(0)
	})

	if cfg.GetAutoCheckUpdates() && srv.updater != nil {
		go func() {
			time.Sleep(4 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if info, err := srv.updater.CheckForUpdate(ctx, false); err == nil && info.UpdateAvailable {
				fmt.Printf("  [!] Update available: GrabAll v%s (current v%s) — update via Web UI or /api/update/apply\n\n", info.LatestVersion, info.CurrentVersion)
			}
		}()
	}

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
