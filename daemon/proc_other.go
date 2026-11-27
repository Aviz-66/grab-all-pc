//go:build !windows

package main

import (
	"fmt"
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

func waitForParentPIDExit(pid int, maxWait time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil && err != syscall.EPERM {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startDetachedExecutable(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	cmd.Dir = filepath.Dir(exePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func isSystemDarkMode() bool {
	return false
}

func openFileDefault(filePath string) error {
	if filePath == "" {
		return fmt.Errorf("file path is empty")
	}
	if _, err := os.Stat(filePath); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("open", filePath).Start()
	}
	return exec.Command("xdg-open", filePath).Start()
}

func triggerSystemSleep() error {
	return fmt.Errorf("not supported on %s", runtime.GOOS)
}

func triggerSystemShutdown(delaySeconds int) error {
	return fmt.Errorf("not supported on %s", runtime.GOOS)
}

func syncInstalledAppVersionRegistry(ver string) {}

// enableSparseFile is a no-op returning true on Unix/macOS where pwrite creates sparse holes natively.
func enableSparseFile(f *os.File) bool {
	return f != nil
}
