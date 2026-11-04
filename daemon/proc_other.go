//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func configureSubprocess(cmd *exec.Cmd) {
	// No-op on non-Windows platforms
}

func openFolderOrSelectFile(filePath, fallbackDir string) {
	target := fallbackDir
	if filePath != "" {
		if _, err := os.Stat(filePath); err == nil {
			if runtime.GOOS == "darwin" {
				_ = exec.Command("open", "-R", filePath).Start()
				return
			}
			target = filepath.Dir(filePath)
		}
	}
	if target == "" {
		target = defaultDownloadDir()
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", target).Start()
	} else {
		_ = exec.Command("xdg-open", target).Start()
	}
}
