//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func configureSubprocess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
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
