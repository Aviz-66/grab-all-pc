//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
	winSynchronize        = 0x00100000
)

// killProcessTree terminates a process AND all of its child processes (PyInstaller inner
// yt-dlp.exe, qjs.exe, and ffmpeg.exe) immediately when Pause or Cancel is clicked.
func killProcessTree(p *os.Process) error {
	if p == nil || p.Pid <= 0 {
		return nil
	}
	tk := exec.Command("taskkill.exe", "/F", "/T", "/PID", strconv.Itoa(p.Pid))
	tk.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	_ = tk.Run()
	return p.Kill()
}

// configureSubprocess prevents child console processes (yt-dlp.exe, ffmpeg.exe)
// from flashing a black CMD window AND ensures Pause/Cancel kills the full process tree.
func configureSubprocess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	cmd.Cancel = func() error {
		return killProcessTree(cmd.Process)
	}
	cmd.WaitDelay = 500 * time.Millisecond
}

func openFolderOrSelectFile(filePath, fallbackDir string) {
	var cmd *exec.Cmd
	if filePath != "" {
		if _, err := os.Stat(filePath); err == nil {
			cmd = exec.Command("explorer.exe", "/select,"+filepath.Clean(filePath))
		}
	}
	if cmd == nil {
		if fallbackDir == "" {
			fallbackDir = defaultDownloadDir()
		}
		_ = os.MkdirAll(fallbackDir, 0755)
		cmd = exec.Command("explorer.exe", filepath.Clean(fallbackDir))
	}
	_ = cmd.Start()
}

func openFileDefault(filePath string) error {
	if filePath == "" {
		return fmt.Errorf("file path is empty")
	}
	if _, err := os.Stat(filePath); err != nil {
		return err
	}
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(filepath.Clean(filePath))
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(filePath))
	r, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0,
		uintptr(unsafe.Pointer(dir)),
		swShowNormal,
	)
	if r <= 32 {
		return fmt.Errorf("ShellExecute failed (code %d)", r)
	}
	return nil
}

func openURLDefault(rawURL string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(rawURL)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, swShowNormal)
}

const (
	hkeyCurrentUser        = uintptr(0x80000001)
	keyRead                = 0x20019
	keySetValue            = 0x0002
	keyReadWrite           = keyRead | keySetValue
	regSZ                  = 1
	runRegistryPath        = `Software\Microsoft\Windows\CurrentVersion\Run`
	startupApprovedRunPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	runRegistryName        = "GrabAll"
)

var (
	modAdvapi32        = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW  = modAdvapi32.NewProc("RegOpenKeyExW")
	procRegCreateKeyExW = modAdvapi32.NewProc("RegCreateKeyExW")
	procRegQueryValueW = modAdvapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW = modAdvapi32.NewProc("RegSetValueExW")
	procRegDeleteValW  = modAdvapi32.NewProc("RegDeleteValueW")
	procRegCloseKey    = modAdvapi32.NewProc("RegCloseKey")
)

func readAutoStartRegistryValue() string {
	subKey, _ := syscall.UTF16PtrFromString(runRegistryPath)
	valName, _ := syscall.UTF16PtrFromString(runRegistryName)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyRead,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return ""
	}
	defer procRegCloseKey.Call(hKey)

	var dataLen uint32
	ret, _, _ = procRegQueryValueW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&dataLen)),
	)
	if ret != 0 || dataLen < 2 || dataLen > 4096 {
		return ""
	}

	buf := make([]uint16, (dataLen/2)+1)
	bufBytes := uint32(len(buf) * 2)
	ret, _, _ = procRegQueryValueW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufBytes)),
	)
	if ret != 0 {
		return ""
	}
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

func isStartupApprovedEnabled() bool {
	subKey, _ := syscall.UTF16PtrFromString(startupApprovedRunPath)
	valName, _ := syscall.UTF16PtrFromString(runRegistryName)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyRead,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return true
	}
	defer procRegCloseKey.Call(hKey)

	var buf [64]byte
	dataLen := uint32(len(buf))
	ret, _, _ = procRegQueryValueW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&dataLen)),
	)
	if ret != 0 || dataLen == 0 {
		return true
	}
	return isStartupApprovedBytesEnabled(buf[:dataLen])
}

func clearStartupApprovedEntry() {
	subKey, _ := syscall.UTF16PtrFromString(startupApprovedRunPath)
	valName, _ := syscall.UTF16PtrFromString(runRegistryName)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keySetValue,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return
	}
	defer procRegCloseKey.Call(hKey)
	procRegDeleteValW.Call(hKey, uintptr(unsafe.Pointer(valName)))
}

func isAutoStartEnabled() bool {
	raw := readAutoStartRegistryValue()
	if raw == "" {
		return false
	}
	exePath := parseAutoStartExePath(raw)
	if exePath == "" {
		return false
	}
	if _, err := os.Stat(exePath); err != nil {
		return false
	}
	return isStartupApprovedEnabled()
}

func setAutoStartEnabled(enable bool) error {
	subKey, _ := syscall.UTF16PtrFromString(runRegistryPath)
	valName, _ := syscall.UTF16PtrFromString(runRegistryName)
	var hKey uintptr
	ret, _, _ := procRegCreateKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		0,
		0,
		keyReadWrite,
		0,
		uintptr(unsafe.Pointer(&hKey)),
		0,
	)
	if ret != 0 {
		return fmt.Errorf("failed to open Run registry key (code %d)", ret)
	}
	defer procRegCloseKey.Call(hKey)

	if !enable {
		procRegDeleteValW.Call(hKey, uintptr(unsafe.Pointer(valName)))
		clearStartupApprovedEntry()
		return nil
	}

	exePath, err := currentExecutablePath()
	if err != nil || exePath == "" {
		exePath, err = os.Executable()
		if err != nil {
			return err
		}
	}
	cmdStr := fmt.Sprintf(`"%s" --tray`, filepath.Clean(exePath))
	utf16Val, err := syscall.UTF16FromString(cmdStr)
	if err != nil {
		return err
	}
	byteLen := uint32(len(utf16Val) * 2)
	ret, _, _ = procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&utf16Val[0])),
		uintptr(byteLen),
	)
	if ret != 0 {
		return fmt.Errorf("failed to write Run registry value (code %d)", ret)
	}
	clearStartupApprovedEntry()
	return nil
}

// syncAutoStartPathIfEnabled heals stale Run\GrabAll paths if graball.exe was moved
// or replaced in a new folder while Windows auto-start was previously enabled.
func syncAutoStartPathIfEnabled() {
	raw := readAutoStartRegistryValue()
	if raw == "" || !isStartupApprovedEnabled() {
		return
	}
	regExe := parseAutoStartExePath(raw)
	curExe, err := currentExecutablePath()
	if err != nil || curExe == "" {
		return
	}
	if !strings.EqualFold(filepath.Clean(regExe), filepath.Clean(curExe)) {
		_ = setAutoStartEnabled(true)
	}
}

// syncInstalledAppVersionRegistry updates HKCU\...\Uninstall\GrabAll DisplayVersion
// if GrabAll was installed via GrabAll-Setup.exe so Windows Installed Apps stays current
// after in-app auto-updates.
func syncInstalledAppVersionRegistry(ver string) {
	ver = normalizeVersionString(ver)
	if ver == "" {
		return
	}
	subKey, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Uninstall\GrabAll`)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keySetValue,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return // Portable install (no Uninstall entry)
	}
	defer procRegCloseKey.Call(hKey)

	valName, _ := syscall.UTF16PtrFromString("DisplayVersion")
	utf16Val, err := syscall.UTF16FromString(ver)
	if err != nil {
		return
	}
	procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&utf16Val[0])),
		uintptr(uint32(len(utf16Val)*2)),
	)
}

func waitForParentPIDExit(pid int, maxWait time.Duration) {
	if pid <= 0 {
		return
	}
	h, err := syscall.OpenProcess(winSynchronize, false, uint32(pid))
	if err == nil && h != 0 {
		defer syscall.CloseHandle(h)
		ms := uint32(maxWait.Milliseconds())
		_, _ = syscall.WaitForSingleObject(h, ms)
		return
	}
	time.Sleep(350 * time.Millisecond)
}

func startDetachedExecutable(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	cmd.Dir = filepath.Dir(exePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup,
	}
	return cmd.Start()
}

// isSystemDarkMode reads Windows's AppsUseLightTheme DWORD from Personalize registry settings.
func isSystemDarkMode() bool {
	subKey, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`)
	valName, _ := syscall.UTF16PtrFromString("AppsUseLightTheme")
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyRead,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return false
	}
	defer procRegCloseKey.Call(hKey)

	var buf [4]byte
	dataLen := uint32(len(buf))
	ret, _, _ = procRegQueryValueW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&dataLen)),
	)
	if ret != 0 || dataLen < 4 {
		return false
	}
	val := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	return val == 0
}

func triggerSystemSleep() error {
	modPowrprof := syscall.NewLazyDLL("powrprof.dll")
	procSetSuspendState := modPowrprof.NewProc("SetSuspendState")
	if err := procSetSuspendState.Find(); err != nil {
		return err
	}
	ret, _, callErr := procSetSuspendState.Call(0, 0, 0)
	if ret == 0 {
		return fmt.Errorf("failed to suspend PC: %v", callErr)
	}
	return nil
}

func triggerSystemShutdown(delaySeconds int) error {
	if delaySeconds <= 0 {
		delaySeconds = 30
	} else if delaySeconds < 10 {
		delaySeconds = 10
	}
	cmd := exec.Command(
		"shutdown.exe",
		"/s",
		"/t", strconv.Itoa(delaySeconds),
		"/c", "GrabAll Download Manager finished all downloads. Run 'shutdown /a' to cancel.",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd.Start()
}
