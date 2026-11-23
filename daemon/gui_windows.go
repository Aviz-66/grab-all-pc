//go:build windows

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

//go:embed graball.ico
var embeddedAppIcon []byte

const (
	// Window Styles
	wsOverlapped       = 0x00000000
	wsPopup            = 0x80000000
	wsChild            = 0x40000000
	wsMinimize         = 0x20000000
	wsVisible          = 0x10000000
	wsDisabled         = 0x08000000
	wsClipSiblings     = 0x04000000
	wsClipChildren     = 0x02000000
	wsCaption          = 0x00C00000
	wsBorder           = 0x00800000
	wsDlgFrame         = 0x00400000
	wsVScroll          = 0x00200000
	wsSysMenu          = 0x00080000
	wsThickFrame       = 0x00040000
	wsGroup            = 0x00020000
	wsTabStop          = 0x00010000
	wsMinimizeBox      = 0x00020000
	wsMaximizeBox      = 0x00010000
	wsOverlappedWindow = wsOverlapped | wsCaption | wsSysMenu | wsThickFrame | wsMinimizeBox | wsMaximizeBox

	wsExAppWindow  = 0x00040000
	wsExClientEdge = 0x00000200
	wsExTopmost    = 0x00000008
	wsExDlgModal   = 0x00000001

	// ShowWindow
	swHide       = 0
	swShowNormal = 1
	swRestore    = 9
	swShow       = 5

	// Window Messages
	wmCreate        = 0x0001
	wmDestroy       = 0x0002
	wmSize          = 0x0005
	wmSetText       = 0x000C
	wmClose         = 0x0010
	wmQuit          = 0x0012
	wmGetMinMaxInfo = 0x0024
	wmSetFont       = 0x0030
	wmNotify        = 0x004E
	wmSetIcon       = 0x0080
	wmCommand       = 0x0111
	wmSysCommand    = 0x0112
	wmTimer         = 0x0113
	wmCtlColorStatic = 0x0138
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	wmApp            = 0x8000
	wmAppTray        = wmApp + 1
	wmAppQueueUpdate = wmApp + 2
	wmAppTaskAdded   = wmApp + 3
	wmAppPlaylistAdd = wmApp + 4
	wmAppShowMain         = wmApp + 5
	wmAppStatusMsg        = wmApp + 6
	wmAppRestartForUpdate = wmApp + 7

	// Button / Edit / Static / Combo Styles
	bsPushButton    = 0x00000000
	bsDefPushButton = 0x00000001
	bsAutoCheckBox  = 0x00000003
	bmGetCheck      = 0x00F0
	bmSetCheck      = 0x00F1
	bstChecked      = 1
	bstUnchecked    = 0

	esAutoHScroll = 0x0080
	ssLeftNoWrap  = 0x000C
	ssEndEllipsis = 0x4000

	cbsDropDownList = 0x0003
	cbAddString     = 0x0143
	cbGetCurSel     = 0x0147
	cbSetCurSel     = 0x014E

	// ListView Constants
	iccListViewClasses = 0x00000001
	iccBarClasses      = 0x00000004
	iccProgressClass   = 0x00000020

	lvsReport        = 0x0001
	lvsSingleSel     = 0x0004
	lvsShowSelAlways = 0x0008

	lvsExGridLines     = 0x00000001
	lvsExFullRowSelect = 0x00000020
	lvsExDoubleBuffer  = 0x00010000

	lvmFirst                    = 0x1000
	lvmDeleteItem               = lvmFirst + 8
	lvmDeleteAllItems           = lvmFirst + 9
	lvmGetNextItem              = lvmFirst + 12
	lvmSetItemState             = lvmFirst + 43
	lvmSetExtendedListViewStyle = lvmFirst + 54
	lvmInsertItemW              = lvmFirst + 77
	lvmInsertColumnW            = lvmFirst + 97
	lvmSetItemTextW             = lvmFirst + 116

	lvcfFmt     = 0x0001
	lvcfWidth   = 0x0002
	lvcfText    = 0x0004
	lvcfSubItem = 0x0008
	lvcfmtLeft  = 0x0000
	lvcfmtRight = 0x0001

	lvifText      = 0x0001
	lvifState     = 0x0008
	lvisSelected  = 0x0002
	lvisFocused   = 0x0001
	lvniSelected  = 0x0002

	nmDblClk = ^uint32(2) // -3
	nmRClick = ^uint32(4) // -5

	// Progress Bar Constants
	pbsSmooth    = 0x01
	pbmSetPos    = 0x0402
	pbmSetRange32 = 0x0406
	pbmSetState  = 0x0410
	pbstateNormal = 0x0001
	pbstateError  = 0x0002
	pbstatePaused = 0x0003

	// System Tray Constants
	nimAdd     = 0x00000000
	nimModify  = 0x00000001
	nimDelete  = 0x00000002
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010
	niifInfo   = 0x00000001

	// Menu Flags
	mfString     = 0x00000000
	mfSeparator  = 0x00000800
	mfChecked    = 0x00000008
	mfUnchecked  = 0x00000000
	mfPopup      = 0x00000010
	mfByCommand  = 0x00000000
	mfGrayed     = 0x00000001
	mfEnabled    = 0x00000000
	tpmRightBtn  = 0x0002
	tpmReturnCmd = 0x0100

	// MessageBox Flags
	mbOK          = 0x00000000
	mbYesNo       = 0x00000004
	mbIconInfo    = 0x00000040
	mbIconWarning = 0x00000030
	mbIconError   = 0x00000010
	mbIconQuestion = 0x00000020
	idYes         = 6

	// Misc Constants
	idcArrow           = 32512
	smCxScreen         = 0
	smCyScreen         = 1
	colorBtnFace       = 15
	transparentBkMode  = 1
	logPixelsX         = 88
	cfUnicodeText      = 13
	gmemMoveable       = 0x0002
	errorAlreadyExists = 183
	bifReturnOnlyFSDirs = 0x00000001
	bifNewDialogStyle   = 0x00000040
	coinitApartment    = 0x2

	// Command & Control IDs
	cmdAddURL        = 1001
	cmdResume        = 1002
	cmdPause         = 1003
	cmdCancel        = 1004
	cmdDelete        = 1005
	cmdDeleteDisk    = 1006
	cmdOpenFolder    = 1007
	cmdOpenFile      = 1008
	cmdShowProgress  = 1009
	cmdClearDone     = 1010
	cmdOpenDownloads = 1011
	cmdInstallYtDlp  = 1012
	cmdInstallFFmpeg = 1013
	cmdChangeDir     = 1014
	cmdCopyToken     = 1015
	cmdCopyURL       = 1016
	cmdToggleStartup     = 1017
	cmdTogglePopup       = 1018
	cmdToggleTurbo       = 1019
	cmdToggleAutoUpdate  = 1020
	cmdConc1             = 1021
	cmdConc2             = 1022
	cmdConc3             = 1023
	cmdConc4             = 1024
	cmdTaskSwitchRegular = 1025
	cmdTaskSwitchTurbo   = 1026
	cmdOpenWeb           = 1030
	cmdAbout             = 1031
	cmdCheckUpdate       = 1032
	cmdShowMain          = 1040
	cmdToolsMenu         = 1041
	cmdExit              = 1099

	idcListView = 2001
	idcStatus   = 2002

	// Progress Popup Control IDs
	idcProgBtnPrimary   = 3001
	idcProgBtnSecondary = 3002
	idcProgBtnClose     = 3003

	// Add Dialog Control IDs
	idcAddEditURL   = 4001
	idcAddComboQual = 4002
	idcAddChkPlay   = 4003
	idcAddBtnStart  = 4004
	idcAddBtnCancel = 4005
	idcAddEditStart = 4006
	idcAddEditCount = 4007
	idcAddChkStream = 4008
)

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modGdi32    = syscall.NewLazyDLL("gdi32.dll")
	modComctl32 = syscall.NewLazyDLL("comctl32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modOle32    = syscall.NewLazyDLL("ole32.dll")

	procSetProcessDPIAware   = modUser32.NewProc("SetProcessDPIAware")
	procRegisterClassExW     = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW      = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW       = modUser32.NewProc("DefWindowProcW")
	procDestroyWindow        = modUser32.NewProc("DestroyWindow")
	procShowWindow           = modUser32.NewProc("ShowWindow")
	procUpdateWindow         = modUser32.NewProc("UpdateWindow")
	procSetForegroundWindow  = modUser32.NewProc("SetForegroundWindow")
	procIsWindowVisible      = modUser32.NewProc("IsWindowVisible")
	procIsIconic             = modUser32.NewProc("IsIconic")
	procMoveWindow           = modUser32.NewProc("MoveWindow")
	procGetClientRect        = modUser32.NewProc("GetClientRect")
	procGetSystemMetrics     = modUser32.NewProc("GetSystemMetrics")
	procGetMessageW          = modUser32.NewProc("GetMessageW")
	procTranslateMessage     = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW     = modUser32.NewProc("DispatchMessageW")
	procPostMessageW         = modUser32.NewProc("PostMessageW")
	procPostQuitMessage      = modUser32.NewProc("PostQuitMessage")
	procSendMessageW         = modUser32.NewProc("SendMessageW")
	procSetWindowTextW       = modUser32.NewProc("SetWindowTextW")
	procGetWindowTextW       = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
	procEnableWindow         = modUser32.NewProc("EnableWindow")
	procSetFocus             = modUser32.NewProc("SetFocus")
	procMessageBoxW          = modUser32.NewProc("MessageBoxW")
	procCreateMenu           = modUser32.NewProc("CreateMenu")
	procCreatePopupMenu      = modUser32.NewProc("CreatePopupMenu")
	procDestroyMenu          = modUser32.NewProc("DestroyMenu")
	procAppendMenuW          = modUser32.NewProc("AppendMenuW")
	procCheckMenuItem        = modUser32.NewProc("CheckMenuItem")
	procSetMenu              = modUser32.NewProc("SetMenu")
	procSetMenuDefaultItem   = modUser32.NewProc("SetMenuDefaultItem")
	procTrackPopupMenu       = modUser32.NewProc("TrackPopupMenu")
	procGetCursorPos             = modUser32.NewProc("GetCursorPos")
	procLoadCursorW              = modUser32.NewProc("LoadCursorW")
	procLoadIconW                = modUser32.NewProc("LoadIconW")
	procLoadImageW               = modUser32.NewProc("LoadImageW")
	procCreateIconIndirect       = modUser32.NewProc("CreateIconIndirect")
	procCreateIconFromResourceEx = modUser32.NewProc("CreateIconFromResourceEx")
	procGetDC                    = modUser32.NewProc("GetDC")
	procReleaseDC            = modUser32.NewProc("ReleaseDC")
	procGetSysColorBrush     = modUser32.NewProc("GetSysColorBrush")
	procOpenClipboard        = modUser32.NewProc("OpenClipboard")
	procCloseClipboard       = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard       = modUser32.NewProc("EmptyClipboard")
	procGetClipboardData     = modUser32.NewProc("GetClipboardData")
	procSetClipboardData     = modUser32.NewProc("SetClipboardData")
	procFindWindowW          = modUser32.NewProc("FindWindowW")

	procGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
	procCreateMutexW     = modKernel32.NewProc("CreateMutexW")
	procGlobalAlloc      = modKernel32.NewProc("GlobalAlloc")
	procGlobalLock       = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock     = modKernel32.NewProc("GlobalUnlock")
	procGlobalSize       = modKernel32.NewProc("GlobalSize")
	procRtlMoveMemory    = modKernel32.NewProc("RtlMoveMemory")

	procCreateFontW      = modGdi32.NewProc("CreateFontW")
	procCreateDIBSection = modGdi32.NewProc("CreateDIBSection")
	procCreateBitmap     = modGdi32.NewProc("CreateBitmap")
	procDeleteObject     = modGdi32.NewProc("DeleteObject")
	procGetDeviceCaps    = modGdi32.NewProc("GetDeviceCaps")
	procSetBkMode        = modGdi32.NewProc("SetBkMode")

	procInitCommonControlsEx = modComctl32.NewProc("InitCommonControlsEx")

	procShellNotifyIconW      = modShell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW         = modShell32.NewProc("ShellExecuteW")
	procSHBrowseForFolderW    = modShell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW  = modShell32.NewProc("SHGetPathFromIDListW")

	procCoInitializeEx = modOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree  = modOle32.NewProc("CoTaskMemFree")
)

type winPoint struct {
	X int32
	Y int32
}

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type winMinMaxInfo struct {
	PtReserved     winPoint
	PtMaxSize      winPoint
	PtMaxPosition  winPoint
	PtMinTrackSize winPoint
	PtMaxTrackSize winPoint
}

type winMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type initCommonControlsEx struct {
	DwSize uint32
	DwICC  uint32
}

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeoutVersion  uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         winGUID
	HBalloonIcon     uintptr
}

type lvColumnW struct {
	Mask       uint32
	Fmt        int32
	Cx         int32
	PszText    *uint16
	CchTextMax int32
	ISubItem   int32
	IImage     int32
	IOrder     int32
	CxMin      int32
	CxDefault  int32
	CxIdeal    int32
}

type lvItemW struct {
	Mask       uint32
	IItem      int32
	ISubItem   int32
	State      uint32
	StateMask  uint32
	PszText    *uint16
	CchTextMax int32
	IImage     int32
	LParam     uintptr
	IIndent    int32
	IGroupId   int32
	CColumns   uint32
	PuColumns  uintptr
	PiColFmt   uintptr
	IGroup     int32
}

type nmHdr struct {
	HWndFrom uintptr
	IdFrom   uintptr
	Code     uint32
}

type nmItemActivate struct {
	Hdr       nmHdr
	IItem     int32
	ISubItem  int32
	UNewState uint32
	UOldState uint32
	UChanged  uint32
	PtAction  winPoint
	LParam    uintptr
	UKeyFlags uint32
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type bitmapInfo struct {
	BmiHeader bitmapInfoHeader
	BmiColors [1]uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type browseInfoW struct {
	HwndOwner      uintptr
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
}

type toolbarButtonDef struct {
	id    uintptr
	label string
	width int
	hwnd  uintptr
}

type listRowCache struct {
	id   string
	cols [6]string
}

type progressPopup struct {
	taskID       string
	hwnd         uintptr
	lblTitle     uintptr
	lblURL       uintptr
	lblStatus    uintptr
	lblSize      uintptr
	lblSpeed     uintptr
	progBar      uintptr
	btnPrimary   uintptr
	btnSecondary uintptr
	btnClose     uintptr
	lastTitle    string
	lastStatus   string
	lastSize     string
	lastSpeed    string
	lastPos      int
	lastMode     string // "active", "paused", "completed", "error"
}

type addDialogControls struct {
	hwnd      uintptr
	editURL   uintptr
	comboQual uintptr
	chkPlay   uintptr
	chkStream uintptr
	editStart uintptr
	editCount uintptr
}

var qualityPresets = []struct {
	value string
	label string
}{
	{"best", "Best Quality (Auto)"},
	{"2160", "4K Ultra HD (2160p)"},
	{"1080", "1080p Full HD"},
	{"720", "720p HD"},
	{"480", "480p Standard"},
	{"360", "360p Data Saver"},
	{"audio-mp3", "Audio Only — MP3"},
	{"audio-m4a", "Audio Only — M4A"},
	{"audio-opus", "Audio Only — Opus"},
}

var (
	singletonMutexHandle uintptr
	appGUI               *winAppGUI
)

type winAppGUI struct {
	cfg    *Config
	qm     *QueueManager
	srv    *Server
	hInst  uintptr
	dpi    int
	quitting bool
	shownTrayHint bool

	iconBig   uintptr
	iconSmall uintptr
	fontUI    uintptr
	fontBold  uintptr

	mainHwnd   uintptr
	listHwnd   uintptr
	statusHwnd uintptr
	hMenuOpt   uintptr
	toolbar    []toolbarButtonDef
	rows       []listRowCache

	popups   map[uintptr]*progressPopup // hwnd -> popup
	taskPop  map[string]uintptr         // taskID -> hwnd
	addDlg   *addDialogControls

	updateQueued atomic.Bool
	updating     atomic.Bool

	eventsMu        sync.Mutex
	pendingTaskIDs  []string
	pendingPlaylist []string
	customStatus    string
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		empty := uint16(0)
		return &empty
	}
	return p
}

func copyUTF16Fixed(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) >= len(dst) {
		copy(dst, u[:len(dst)-1])
		dst[len(dst)-1] = 0
	} else {
		copy(dst, u)
	}
}

func getWindowString(hwnd uintptr) string {
	length, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+2)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), length+1)
	return syscall.UTF16ToString(buf)
}

func setWindowString(hwnd uintptr, text string) {
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr(text))))
}

// activateExistingInstance checks if GrabAll is already running. If so, brings its window to front and returns true.
func activateExistingInstance(cfg *Config) bool {
	mutexName := utf16Ptr(`Local\GrabAllDownloadManagerSingleton`)
	h, _, lastErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(mutexName)))
	if h != 0 && lastErr == syscall.Errno(errorAlreadyExists) {
		// Signal existing window directly if present
		clsName := utf16Ptr("GrabAllMainWndClass")
		if existingHwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(clsName)), 0); existingHwnd != 0 {
			procPostMessageW.Call(existingHwnd, wmAppShowMain, 0, 0)
			return true
		}
		// Fallback via local HTTP API
		client := &http.Client{Timeout: 800 * time.Millisecond}
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/show-window", cfg.Port), bytes.NewReader([]byte("{}")))
		if err == nil {
			req.Header.Set("X-Daemon-Token", cfg.GetToken())
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				return true
			}
		}
		return true
	}
	singletonMutexHandle = h
	return false
}

func releaseSingleInstanceLock() {
	if singletonMutexHandle != 0 {
		_ = syscall.CloseHandle(syscall.Handle(singletonMutexHandle))
		singletonMutexHandle = 0
	}
}

func runPlatformApp(cfg *Config, qm *QueueManager, srv *Server, startInTray bool, headless bool) {
	if headless {
		if err := srv.Start(); err != nil {
			log.Fatalf("[-] Server failed: %v", err)
		}
		return
	}

	runtime.LockOSThread()

	g := &winAppGUI{
		cfg:     cfg,
		qm:      qm,
		srv:     srv,
		dpi:     96,
		popups:  make(map[uintptr]*progressPopup),
		taskPop: make(map[string]uintptr),
	}
	appGUI = g

	// Wire server & queue hooks to Win32 message loop
	srv.SetShowWindowHandler(func() {
		if g.mainHwnd != 0 {
			procPostMessageW.Call(g.mainHwnd, wmAppShowMain, 0, 0)
		}
	})
	srv.SetRestartForUpdateHandler(func() {
		if g.mainHwnd != 0 {
			procPostMessageW.Call(g.mainHwnd, wmAppRestartForUpdate, 0, 0)
		}
	})

	qm.SetUIHooks(
		func() {
			if g.mainHwnd != 0 && g.updateQueued.CompareAndSwap(false, true) {
				procPostMessageW.Call(g.mainHwnd, wmAppQueueUpdate, 0, 0)
			}
		},
		func(task *DownloadTask) {
			if g.mainHwnd != 0 && task != nil {
				g.eventsMu.Lock()
				g.pendingTaskIDs = append(g.pendingTaskIDs, task.ID)
				g.eventsMu.Unlock()
				procPostMessageW.Call(g.mainHwnd, wmAppTaskAdded, 0, 0)
			}
		},
		func(title string, count int) {
			if g.mainHwnd != 0 {
				g.eventsMu.Lock()
				g.pendingPlaylist = append(g.pendingPlaylist, fmt.Sprintf("Queued %d videos from playlist \"%s\"", count, title))
				g.eventsMu.Unlock()
				procPostMessageW.Call(g.mainHwnd, wmAppPlaylistAdd, 0, 0)
			}
		},
	)

	// Start embedded HTTP server in background for browser extension & optional web UI
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			g.postStatusMessage(fmt.Sprintf("Port %d busy — check if another GrabAll is running", cfg.Port))
		}
	}()

	g.initAndRunMessageLoop(startInTray)
}

func (g *winAppGUI) scale(px int) int {
	return (px * g.dpi) / 96
}

func (g *winAppGUI) postStatusMessage(msg string) {
	g.eventsMu.Lock()
	g.customStatus = msg
	g.eventsMu.Unlock()
	if g.mainHwnd != 0 {
		procPostMessageW.Call(g.mainHwnd, wmAppStatusMsg, 0, 0)
	}
}

// createBoltIcon builds a crisp 32-bit ARGB lightning bolt icon in memory (zero external .ico files needed).
func createBoltIcon(size int) uintptr {
	var bi bitmapInfo
	bi.BmiHeader.BiSize = uint32(unsafe.Sizeof(bi.BmiHeader))
	bi.BmiHeader.BiWidth = int32(size)
	bi.BmiHeader.BiHeight = -int32(size) // top-down DIB
	bi.BmiHeader.BiPlanes = 1
	bi.BmiHeader.BiBitCount = 32
	bi.BmiHeader.BiCompression = 0 // BI_RGB

	var bitsPtr unsafe.Pointer
	hdc, _, _ := procGetDC.Call(0)
	hbmColor, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bi)),
		0,
		uintptr(unsafe.Pointer(&bitsPtr)),
		0,
		0,
	)
	procReleaseDC.Call(0, hdc)
	if hbmColor == 0 || bitsPtr == nil {
		return 0
	}

	pixels := unsafe.Slice((*uint32)(bitsPtr), size*size)
	radius := float64(size) * 0.22
	fSize := float64(size)

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := float64(x) + 0.5
			fy := float64(y) + 0.5

			// Rounded square bounds check
			inBox := true
			var dx, dy float64
			if fx < radius && fy < radius {
				dx, dy = radius-fx, radius-fy
			} else if fx > fSize-radius && fy < radius {
				dx, dy = fx-(fSize-radius), radius-fy
			} else if fx < radius && fy > fSize-radius {
				dx, dy = radius-fx, fy-(fSize-radius)
			} else if fx > fSize-radius && fy > fSize-radius {
				dx, dy = fx-(fSize-radius), fy-(fSize-radius)
			}
			if dx*dx+dy*dy > radius*radius {
				inBox = false
			}
			if !inBox {
				pixels[y*size+x] = 0x00000000
				continue
			}

			// Normalized coordinates 0..1 for lightning bolt polygon
			nx := fx / fSize
			ny := fy / fSize
			isBolt := false

			// Upper diagonal of lightning bolt
			if ny >= 0.14 && ny <= 0.55 {
				left := 0.54 - (ny-0.14)*0.72
				right := 0.68 - (ny-0.14)*0.52
				if ny >= 0.44 {
					right = 0.74
				}
				if nx >= left && nx <= right {
					isBolt = true
				}
			}
			// Lower diagonal of lightning bolt
			if ny >= 0.45 && ny <= 0.86 {
				left := 0.43 - (ny-0.45)*0.22
				if ny <= 0.55 {
					left = 0.26
				}
				right := 0.74 - (ny-0.45)*0.78
				if nx >= left && nx <= right {
					isBolt = true
				}
			}

			if isBolt {
				// Gold yellow (#FACC15 -> BGRA: 0xFF FACC15)
				pixels[y*size+x] = 0xFFFACC15
			} else {
				// Rich royal blue gradient (#2563EB -> #1D4ED8)
				shade := uint32(235 - int(ny*35))
				green := uint32(105 - int(ny*28))
				red := uint32(37 - int(ny*10))
				pixels[y*size+x] = 0xFF000000 | (red << 16) | (green << 8) | shade
			}
		}
	}

	maskStride := ((size + 15) / 16) * 2
	maskBytes := make([]byte, maskStride*size)
	hbmMask, _, _ := procCreateBitmap.Call(
		uintptr(size),
		uintptr(size),
		1,
		1,
		uintptr(unsafe.Pointer(&maskBytes[0])),
	)

	ii := iconInfo{
		FIcon:    1,
		HbmMask:  hbmMask,
		HbmColor: hbmColor,
	}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if hbmColor != 0 {
		procDeleteObject.Call(hbmColor)
	}
	if hbmMask != 0 {
		procDeleteObject.Call(hbmMask)
	}
	return hIcon
}

const (
	imageIcon      = 1
	lrDefaultColor = 0x0000
	lrShared       = 0x8000
	iconSmallVal   = 0
	iconBigVal     = 1
)

// loadAppIcon retrieves the highest fidelity native GrabAll icon:
// 1. Attempts Win32 LoadImageW from the PE executable resource section (.rsrc via rsrc_windows_amd64.syso, RT_GROUP_ICON #1)
// 2. Attempts Win32 LoadIconW
// 3. Parses the embedded graball.ico and calls Win32 CreateIconFromResourceEx
// 4. Falls back to procedural createBoltIcon
func loadAppIcon(hInst uintptr, size int) uintptr {
	if procLoadImageW.Find() == nil && hInst != 0 {
		hIcon, _, _ := procLoadImageW.Call(
			hInst,
			1, // MAKEINTRESOURCE(1)
			imageIcon,
			uintptr(size),
			uintptr(size),
			lrDefaultColor|lrShared,
		)
		if hIcon != 0 {
			return hIcon
		}
	}
	if procLoadIconW.Find() == nil && hInst != 0 {
		hIcon, _, _ := procLoadIconW.Call(hInst, 1)
		if hIcon != 0 {
			return hIcon
		}
	}
	if hIcon := loadIconFromEmbeddedData(size); hIcon != 0 {
		return hIcon
	}
	return createBoltIcon(size)
}

func loadIconFromEmbeddedData(size int) uintptr {
	if len(embeddedAppIcon) < 6 || procCreateIconFromResourceEx.Find() != nil {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(embeddedAppIcon[4:6]))
	if count <= 0 || len(embeddedAppIcon) < 6+count*16 {
		return 0
	}

	bestOffset := -1
	bestSize := 0
	bestDiff := 999999

	for i := 0; i < count; i++ {
		entryOffset := 6 + i*16
		w := int(embeddedAppIcon[entryOffset])
		if w == 0 {
			w = 256
		}
		bytesInRes := int(binary.LittleEndian.Uint32(embeddedAppIcon[entryOffset+8 : entryOffset+12]))
		imgOffset := int(binary.LittleEndian.Uint32(embeddedAppIcon[entryOffset+12 : entryOffset+16]))
		if imgOffset+bytesInRes > len(embeddedAppIcon) {
			continue
		}

		diff := w - size
		if diff < 0 {
			diff = -diff * 2
		}
		if diff < bestDiff {
			bestDiff = diff
			bestOffset = imgOffset
			bestSize = bytesInRes
		}
	}

	if bestOffset < 0 || bestSize <= 0 {
		return 0
	}

	hIcon, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&embeddedAppIcon[bestOffset])),
		uintptr(bestSize),
		1,          // fIcon (TRUE)
		0x00030000, // dwVersion
		uintptr(size),
		uintptr(size),
		lrDefaultColor,
	)
	return hIcon
}

func (g *winAppGUI) initAndRunMessageLoop(startInTray bool) {
	procSetProcessDPIAware.Call()
	procCoInitializeEx.Call(0, coinitApartment)

	hdc, _, _ := procGetDC.Call(0)
	if hdc != 0 {
		if dpi, _, _ := procGetDeviceCaps.Call(hdc, logPixelsX); dpi >= 96 {
			g.dpi = int(dpi)
		}
		procReleaseDC.Call(0, hdc)
	}

	icc := initCommonControlsEx{
		DwSize: uint32(unsafe.Sizeof(initCommonControlsEx{})),
		DwICC:  iccListViewClasses | iccProgressClass | iccBarClasses,
	}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))

	g.hInst, _, _ = procGetModuleHandleW.Call(0)
	g.iconBig = loadAppIcon(g.hInst, 32)
	g.iconSmall = loadAppIcon(g.hInst, 16)

	fontFace := utf16Ptr("Segoe UI")
	g.fontUI, _, _ = procCreateFontW.Call(
		uintptr(int32(-g.scale(13))), 0, 0, 0,
		400, 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(fontFace)),
	)
	g.fontBold, _, _ = procCreateFontW.Call(
		uintptr(int32(-g.scale(14))), 0, 0, 0,
		600, 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(fontFace)),
	)

	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	bgBrush, _, _ := procGetSysColorBrush.Call(colorBtnFace)

	// Register Main Window Class
	mainCls := utf16Ptr("GrabAllMainWndClass")
	wcMain := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(mainWndProc),
		HInstance:     g.hInst,
		HIcon:         g.iconBig,
		HCursor:       cursor,
		HbrBackground: bgBrush,
		LpszClassName: mainCls,
		HIconSm:       g.iconSmall,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcMain)))

	// Register Mini Progress Window Class
	progCls := utf16Ptr("GrabAllProgressWndClass")
	wcProg := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(progressWndProc),
		HInstance:     g.hInst,
		HIcon:         g.iconBig,
		HCursor:       cursor,
		HbrBackground: bgBrush,
		LpszClassName: progCls,
		HIconSm:       g.iconSmall,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcProg)))

	// Register Add URL Dialog Class
	addCls := utf16Ptr("GrabAllAddWndClass")
	wcAdd := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(addDlgWndProc),
		HInstance:     g.hInst,
		HIcon:         g.iconBig,
		HCursor:       cursor,
		HbrBackground: bgBrush,
		LpszClassName: addCls,
		HIconSm:       g.iconSmall,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcAdd)))

	winW := g.scale(900)
	winH := g.scale(520)
	scrW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	scrH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	posX := (int(scrW) - winW) / 2
	posY := (int(scrH) - winH) / 2
	if posX < 20 {
		posX = 20
	}
	if posY < 20 {
		posY = 20
	}

	hMenu := g.buildMainMenu()

	g.mainHwnd, _, _ = procCreateWindowExW.Call(
		wsExAppWindow,
		uintptr(unsafe.Pointer(mainCls)),
		uintptr(unsafe.Pointer(utf16Ptr("GrabAll Download Manager"))),
		wsOverlappedWindow|wsClipChildren,
		uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH),
		0,
		hMenu,
		g.hInst,
		0,
	)

	if g.iconBig != 0 {
		procSendMessageW.Call(g.mainHwnd, wmSetIcon, iconBigVal, g.iconBig)
	}
	if g.iconSmall != 0 {
		procSendMessageW.Call(g.mainHwnd, wmSetIcon, iconSmallVal, g.iconSmall)
	}

	g.addTrayIcon()
	g.syncListView()
	g.refreshStatusBar()
	go g.autoUpgradeSlowYtDlpIfNeeded()

	if !startInTray {
		procShowWindow.Call(g.mainHwnd, swShowNormal)
		procUpdateWindow.Call(g.mainHwnd)
	}
	go g.checkFirstRunEnginePrompt()
	go func() {
		time.Sleep(6 * time.Second)
		if g.cfg.GetAutoCheckUpdates() {
			g.checkForAppUpdate(false, startInTray)
		}
	}()

	var msg winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (g *winAppGUI) buildMainMenu() uintptr {
	hMain, _, _ := procCreateMenu.Call()

	// Tasks Menu
	hTasks, _, _ := procCreatePopupMenu.Call()
	procAppendMenuW.Call(hTasks, mfString, cmdAddURL, uintptr(unsafe.Pointer(utf16Ptr("Add New Download...	Ctrl+N"))))
	procAppendMenuW.Call(hTasks, mfString, cmdClearDone, uintptr(unsafe.Pointer(utf16Ptr("Clear Completed Downloads"))))
	procAppendMenuW.Call(hTasks, mfString, cmdOpenDownloads, uintptr(unsafe.Pointer(utf16Ptr("Open Downloads Folder"))))
	procAppendMenuW.Call(hTasks, mfSeparator, 0, 0)
	procAppendMenuW.Call(hTasks, mfString, cmdExit, uintptr(unsafe.Pointer(utf16Ptr("Exit GrabAll"))))

	// Tools Menu
	hTools, _, _ := procCreatePopupMenu.Call()
	procAppendMenuW.Call(hTools, mfString, cmdInstallYtDlp, uintptr(unsafe.Pointer(utf16Ptr("Install / Update yt-dlp + QuickJS (YouTube Engine)"))))
	procAppendMenuW.Call(hTools, mfString, cmdInstallFFmpeg, uintptr(unsafe.Pointer(utf16Ptr("Install / Update FFmpeg (HD Audio+Video Merger)"))))
	procAppendMenuW.Call(hTools, mfSeparator, 0, 0)
	procAppendMenuW.Call(hTools, mfString, cmdChangeDir, uintptr(unsafe.Pointer(utf16Ptr("Change Default Save Folder..."))))
	procAppendMenuW.Call(hTools, mfString, cmdCopyToken, uintptr(unsafe.Pointer(utf16Ptr("Copy Extension Auth Token"))))

	// Options Menu
	hOpt, _, _ := procCreatePopupMenu.Call()
	g.hMenuOpt = hOpt
	procAppendMenuW.Call(hOpt, mfString, cmdToggleTurbo, uintptr(unsafe.Pointer(utf16Ptr("⚡ Turbo Multi-Connection Speed (6x Faster — uncheck for Watch-Live 1x)"))))
	procAppendMenuW.Call(hOpt, mfSeparator, 0, 0)
	procAppendMenuW.Call(hOpt, mfString, cmdToggleStartup, uintptr(unsafe.Pointer(utf16Ptr("Start GrabAll Automatically with Windows"))))
	procAppendMenuW.Call(hOpt, mfString, cmdTogglePopup, uintptr(unsafe.Pointer(utf16Ptr("Show Download Progress Popup on New Downloads"))))
	procAppendMenuW.Call(hOpt, mfString, cmdToggleAutoUpdate, uintptr(unsafe.Pointer(utf16Ptr("Automatically Check for GrabAll Updates on Startup"))))
	procAppendMenuW.Call(hOpt, mfSeparator, 0, 0)
	procAppendMenuW.Call(hOpt, mfString, cmdConc1, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 1"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc2, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 2"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc3, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 3"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc4, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 4"))))

	// Help Menu
	hHelp, _, _ := procCreatePopupMenu.Call()
	procAppendMenuW.Call(hHelp, mfString, cmdOpenWeb, uintptr(unsafe.Pointer(utf16Ptr("Open Web UI in Browser"))))
	procAppendMenuW.Call(hHelp, mfString, cmdCheckUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for Updates..."))))
	procAppendMenuW.Call(hHelp, mfString, cmdAbout, uintptr(unsafe.Pointer(utf16Ptr("About GrabAll"))))

	procAppendMenuW.Call(hMain, mfPopup, hTasks, uintptr(unsafe.Pointer(utf16Ptr("&Tasks"))))
	procAppendMenuW.Call(hMain, mfPopup, hTools, uintptr(unsafe.Pointer(utf16Ptr("T&ools"))))
	procAppendMenuW.Call(hMain, mfPopup, hOpt, uintptr(unsafe.Pointer(utf16Ptr("&Options"))))
	procAppendMenuW.Call(hMain, mfPopup, hHelp, uintptr(unsafe.Pointer(utf16Ptr("&Help"))))

	g.refreshOptionMenuChecks()
	return hMain
}

func (g *winAppGUI) refreshOptionMenuChecks() {
	if g.hMenuOpt == 0 {
		return
	}
	if g.cfg.GetSpeedMode() == SpeedModeTurbo {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleTurbo, mfByCommand|mfChecked)
	} else {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleTurbo, mfByCommand|mfUnchecked)
	}

	if isAutoStartEnabled() {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleStartup, mfByCommand|mfChecked)
	} else {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleStartup, mfByCommand|mfUnchecked)
	}

	if g.cfg.GetShowProgressPopup() {
		procCheckMenuItem.Call(g.hMenuOpt, cmdTogglePopup, mfByCommand|mfChecked)
	} else {
		procCheckMenuItem.Call(g.hMenuOpt, cmdTogglePopup, mfByCommand|mfUnchecked)
	}

	if g.cfg.GetAutoCheckUpdates() {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleAutoUpdate, mfByCommand|mfChecked)
	} else {
		procCheckMenuItem.Call(g.hMenuOpt, cmdToggleAutoUpdate, mfByCommand|mfUnchecked)
	}

	conc := g.cfg.GetMaxConcurrent()
	for i, id := range []uintptr{cmdConc1, cmdConc2, cmdConc3, cmdConc4} {
		if conc == i+1 {
			procCheckMenuItem.Call(g.hMenuOpt, id, mfByCommand|mfChecked)
		} else {
			procCheckMenuItem.Call(g.hMenuOpt, id, mfByCommand|mfUnchecked)
		}
	}
}

func (g *winAppGUI) createChildControl(className, text string, style, exStyle uint32, x, y, w, h int, parent, id uintptr, bold bool) uintptr {
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(utf16Ptr(className))),
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent,
		id,
		g.hInst,
		0,
	)
	if hwnd != 0 {
		font := g.fontUI
		if bold {
			font = g.fontBold
		}
		procSendMessageW.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

func (g *winAppGUI) onCreateMainWindow(hwnd uintptr) {
	g.toolbar = []toolbarButtonDef{
		{id: cmdAddURL, label: "+ Add URL", width: 92},
		{id: cmdResume, label: "Resume", width: 78},
		{id: cmdPause, label: "Pause", width: 74},
		{id: cmdCancel, label: "Cancel", width: 74},
		{id: cmdDelete, label: "Delete", width: 74},
		{id: cmdOpenFolder, label: "Open Folder", width: 102},
		{id: cmdClearDone, label: "Clear Done", width: 92},
		{id: cmdToolsMenu, label: "Engine Setup", width: 108},
	}

	x := g.scale(10)
	y := g.scale(8)
	btnH := g.scale(30)
	gap := g.scale(6)

	for i := range g.toolbar {
		bw := g.scale(g.toolbar[i].width)
		style := uint32(wsChild | wsVisible | wsTabStop | bsPushButton)
		if i == 0 {
			style = uint32(wsChild | wsVisible | wsTabStop | bsDefPushButton)
		}
		g.toolbar[i].hwnd = g.createChildControl(
			"BUTTON", g.toolbar[i].label,
			style, 0,
			x, y, bw, btnH,
			hwnd, g.toolbar[i].id, i == 0,
		)
		x += bw + gap
	}

	// Main Download ListView (SysListView32)
	lvStyle := uint32(wsChild | wsVisible | wsTabStop | lvsReport | lvsSingleSel | lvsShowSelAlways)
	g.listHwnd = g.createChildControl(
		"SysListView32", "",
		lvStyle, wsExClientEdge,
		g.scale(10), g.scale(46), g.scale(860), g.scale(380),
		hwnd, idcListView, false,
	)

	exStyle := uintptr(lvsExFullRowSelect | lvsExGridLines | lvsExDoubleBuffer)
	procSendMessageW.Call(g.listHwnd, lvmSetExtendedListViewStyle, exStyle, exStyle)

	cols := []struct {
		title string
		width int
		right bool
	}{
		{"File / Video Title", 315, false},
		{"Size", 125, true},
		{"Status", 125, false},
		{"Transfer Rate", 115, true},
		{"Time Left", 80, true},
		{"Quality", 95, false},
	}

	for idx, c := range cols {
		fmtFlag := int32(lvcfmtLeft)
		if c.right {
			fmtFlag = int32(lvcfmtRight)
		}
		lvc := lvColumnW{
			Mask:     lvcfFmt | lvcfWidth | lvcfText | lvcfSubItem,
			Fmt:      fmtFlag,
			Cx:       int32(g.scale(c.width)),
			PszText:  utf16Ptr(c.title),
			ISubItem: int32(idx),
		}
		procSendMessageW.Call(g.listHwnd, lvmInsertColumnW, uintptr(idx), uintptr(unsafe.Pointer(&lvc)))
	}

	// Bottom Status Bar Text
	g.statusHwnd = g.createChildControl(
		"STATIC", "Ready",
		wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0,
		g.scale(12), g.scale(434), g.scale(860), g.scale(22),
		hwnd, idcStatus, false,
	)
}

func (g *winAppGUI) layoutMainWindow(hwnd uintptr) {
	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	clientW := int(rc.Right - rc.Left)
	clientH := int(rc.Bottom - rc.Top)
	if clientW <= 0 || clientH <= 0 {
		return
	}

	pad := g.scale(10)
	topY := g.scale(46)
	statusH := g.scale(22)
	listH := clientH - topY - statusH - g.scale(12)
	if listH < g.scale(80) {
		listH = g.scale(80)
	}
	listW := clientW - pad*2
	if listW < g.scale(100) {
		listW = g.scale(100)
	}

	if g.listHwnd != 0 {
		procMoveWindow.Call(g.listHwnd, uintptr(pad), uintptr(topY), uintptr(listW), uintptr(listH), 1)
	}
	if g.statusHwnd != 0 {
		procMoveWindow.Call(g.statusHwnd, uintptr(pad+2), uintptr(topY+listH+g.scale(5)), uintptr(listW-4), uintptr(statusH), 1)
	}
}

func (g *winAppGUI) addTrayIcon() {
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = g.mainHwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmAppTray
	nid.HIcon = g.iconSmall
	copyUTF16Fixed(nid.SzTip[:], "GrabAll Download Manager — Ready")
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

func (g *winAppGUI) removeTrayIcon() {
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = g.mainHwnd
	nid.UID = 1
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (g *winAppGUI) showTrayBalloon(title, info string) {
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = g.mainHwnd
	nid.UID = 1
	nid.UFlags = nifInfo
	nid.DwInfoFlags = niifInfo
	copyUTF16Fixed(nid.SzInfoTitle[:], title)
	copyUTF16Fixed(nid.SzInfo[:], info)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func (g *winAppGUI) showMainWindow() {
	if g.mainHwnd == 0 {
		return
	}
	iconic, _, _ := procIsIconic.Call(g.mainHwnd)
	if iconic != 0 {
		procShowWindow.Call(g.mainHwnd, swRestore)
	} else {
		procShowWindow.Call(g.mainHwnd, swShow)
	}
	procSetForegroundWindow.Call(g.mainHwnd)
}

func formatBytesHuman(b int64) string {
	if b <= 0 {
		return "—"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatTaskSizeCol(t *DownloadTask) string {
	if t.TotalBytes > 0 && t.DownloadedBytes > 0 && t.Status != StatusCompleted {
		return fmt.Sprintf("%s / %s", formatBytesHuman(t.DownloadedBytes), formatBytesHuman(t.TotalBytes))
	}
	if t.TotalBytes > 0 {
		return formatBytesHuman(t.TotalBytes)
	}
	if t.DownloadedBytes > 0 {
		return formatBytesHuman(t.DownloadedBytes)
	}
	return "—"
}

func formatTaskStatusCol(t *DownloadTask) string {
	switch t.Status {
	case StatusDownloading:
		return fmt.Sprintf("Downloading %.1f%%", t.Progress)
	case StatusCompleted:
		return "Completed"
	case StatusPaused:
		if t.Progress > 0 {
			return fmt.Sprintf("Paused (%.1f%%)", t.Progress)
		}
		return "Paused"
	case StatusQueued:
		return "Queued"
	case StatusCanceled:
		return "Canceled"
	case StatusError:
		return "Error"
	default:
		return string(t.Status)
	}
}

func formatTaskQualityCol(t *DownloadTask) string {
	if t.Engine == EngineDirect {
		return "Direct"
	}
	q := strings.ToLower(strings.TrimSpace(t.Quality))
	switch q {
	case "", "best":
		return "Best (yt-dlp)"
	case "2160", "4k":
		return "4K (2160p)"
	case "1080":
		return "1080p HD"
	case "720":
		return "720p HD"
	case "480":
		return "480p"
	case "360":
		return "360p"
	case "audio-mp3":
		return "MP3 Audio"
	case "audio-m4a":
		return "M4A Audio"
	case "audio-opus":
		return "Opus Audio"
	case "audio-best":
		return "Audio"
	default:
		return q
	}
}

func (g *winAppGUI) getSelectedTaskID() string {
	if g.listHwnd == 0 {
		return ""
	}
	idx, _, _ := procSendMessageW.Call(g.listHwnd, lvmGetNextItem, ^uintptr(0), lvniSelected)
	row := int(int32(idx))
	if row >= 0 && row < len(g.rows) {
		return g.rows[row].id
	}
	return ""
}

func (g *winAppGUI) selectRowIndex(row int) {
	if g.listHwnd == 0 || row < 0 || row >= len(g.rows) {
		return
	}
	item := lvItemW{
		State:     lvisSelected | lvisFocused,
		StateMask: lvisSelected | lvisFocused,
	}
	procSendMessageW.Call(g.listHwnd, lvmSetItemState, uintptr(row), uintptr(unsafe.Pointer(&item)))
}

// syncListView performs a flicker-free cell diff update on SysListView32 and updates open Progress Popups.
func (g *winAppGUI) syncListView() {
	if g.listHwnd == 0 {
		return
	}
	tasks := g.qm.GetAll()
	prevSelectedID := g.getSelectedTaskID()

	// Adjust row count
	for len(g.rows) > len(tasks) {
		last := len(g.rows) - 1
		procSendMessageW.Call(g.listHwnd, lvmDeleteItem, uintptr(last), 0)
		g.rows = g.rows[:last]
	}

	taskMap := make(map[string]*DownloadTask, len(tasks))
	newSelectedRow := -1

	for i, t := range tasks {
		taskMap[t.ID] = t
		title := t.Title
		if title == "" {
			title = t.FileName
		}
		if title == "" {
			title = t.URL
		}
		speed := t.Speed
		if speed == "" {
			speed = "—"
		}
		eta := t.ETA
		if eta == "" {
			eta = "—"
		}

		cols := [6]string{
			title,
			formatTaskSizeCol(t),
			formatTaskStatusCol(t),
			speed,
			eta,
			formatTaskQualityCol(t),
		}

		if i >= len(g.rows) {
			lvi := lvItemW{
				Mask:    lvifText,
				IItem:   int32(i),
				PszText: utf16Ptr(cols[0]),
			}
			procSendMessageW.Call(g.listHwnd, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&lvi)))
			for sub := 1; sub < 6; sub++ {
				subItem := lvItemW{
					ISubItem: int32(sub),
					PszText:  utf16Ptr(cols[sub]),
				}
				procSendMessageW.Call(g.listHwnd, lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&subItem)))
			}
			g.rows = append(g.rows, listRowCache{id: t.ID, cols: cols})
		} else {
			g.rows[i].id = t.ID
			for sub := 0; sub < 6; sub++ {
				if g.rows[i].cols[sub] != cols[sub] {
					subItem := lvItemW{
						ISubItem: int32(sub),
						PszText:  utf16Ptr(cols[sub]),
					}
					procSendMessageW.Call(g.listHwnd, lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&subItem)))
					g.rows[i].cols[sub] = cols[sub]
				}
			}
		}

		if prevSelectedID != "" && t.ID == prevSelectedID {
			newSelectedRow = i
		}
	}

	if newSelectedRow >= 0 && g.getSelectedTaskID() != prevSelectedID {
		g.selectRowIndex(newSelectedRow)
	}

	// Update any open Mini Progress Windows
	for _, pop := range g.popups {
		if t, ok := taskMap[pop.taskID]; ok {
			g.updateProgressPopup(pop, t)
		}
	}

	g.refreshStatusBar()
}

func (g *winAppGUI) refreshStatusBar() {
	if g.statusHwnd == 0 {
		return
	}

	g.eventsMu.Lock()
	custom := g.customStatus
	g.eventsMu.Unlock()
	if custom != "" {
		setWindowString(g.statusHwnd, custom)
		return
	}

	tasks := g.qm.GetAll()
	var active, queued, done int
	for _, t := range tasks {
		switch t.Status {
		case StatusDownloading:
			active++
		case StatusQueued:
			queued++
		case StatusCompleted:
			done++
		}
	}

	ytPath, ytErr := resolveYtDlpBinary(g.cfg)
	ytState := "Ready (Fast Mode)"
	if ytErr != nil {
		ytState = "Missing (click Engine Setup)"
	} else if !isFastUnpackedYtDlp(ytPath) {
		ytState = "Slow 1-file exe (click Engine Setup -> Upgrade)"
	}
	ffState := "Ready"
	if resolveFFmpegBinary(g.cfg) == "" {
		ffState = "Optional (click Engine Setup for 1080p+/MP3)"
	}

	modeLabel := "Turbo (6x)"
	if g.cfg.GetSpeedMode() == SpeedModeRegular {
		modeLabel = "Regular (Watch Live)"
	}

	status := fmt.Sprintf(
		"Active: %d  |  Queued: %d  |  Completed: %d   •   Speed: %s   •   Folder: %s   •   yt-dlp: %s  |  FFmpeg: %s",
		active, queued, done,
		modeLabel,
		filepath.Clean(g.cfg.GetDownloadDir()),
		ytState, ffState,
	)
	setWindowString(g.statusHwnd, status)
}

// openOrFocusProgressPopup creates or focuses the mini progress dialog for a download task.
func (g *winAppGUI) openOrFocusProgressPopup(taskID string, autoTriggered bool) {
	if existingHwnd, ok := g.taskPop[taskID]; ok && existingHwnd != 0 {
		procShowWindow.Call(existingHwnd, swShowNormal)
		if !autoTriggered {
			procSetForegroundWindow.Call(existingHwnd)
		}
		return
	}

	if autoTriggered && len(g.popups) >= 3 {
		// Avoid crowding screen if multiple downloads fire rapidly
		return
	}

	taskSnap := g.findSnapshot(taskID)
	if taskSnap == nil {
		return
	}

	w := g.scale(485)
	h := g.scale(248)
	scrW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	scrH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	offset := len(g.popups) * g.scale(28)
	posX := (int(scrW)-w)/2 + offset
	posY := (int(scrH)-h)/2 + offset

	style := uint32(wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox | wsClipChildren)
	hwnd, _, _ := procCreateWindowExW.Call(
		wsExAppWindow,
		uintptr(unsafe.Pointer(utf16Ptr("GrabAllProgressWndClass"))),
		uintptr(unsafe.Pointer(utf16Ptr("GrabAll — Starting Download..."))),
		uintptr(style),
		uintptr(posX), uintptr(posY), uintptr(w), uintptr(h),
		0, 0, g.hInst, 0,
	)
	if hwnd == 0 {
		return
	}

	if g.iconSmall != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmallVal, g.iconSmall)
	}

	pad := g.scale(14)
	contentW := w - pad*2 - g.scale(14)

	pop := &progressPopup{
		taskID: taskID,
		hwnd:   hwnd,
	}
	pop.lblTitle = g.createChildControl("STATIC", "", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(12), contentW, g.scale(22), hwnd, 0, true)
	pop.lblURL = g.createChildControl("STATIC", "", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(35), contentW, g.scale(18), hwnd, 0, false)
	pop.lblStatus = g.createChildControl("STATIC", "", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(60), contentW, g.scale(19), hwnd, 0, false)
	pop.lblSize = g.createChildControl("STATIC", "", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(82), contentW, g.scale(19), hwnd, 0, false)
	pop.lblSpeed = g.createChildControl("STATIC", "", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(104), contentW, g.scale(19), hwnd, 0, false)

	pop.progBar = g.createChildControl("msctls_progress32", "", wsChild|wsVisible|pbsSmooth, 0, pad, g.scale(132), contentW, g.scale(22), hwnd, 0, false)
	procSendMessageW.Call(pop.progBar, pbmSetRange32, 0, 1000)

	btnW := g.scale(106)
	btnH := g.scale(30)
	btnY := g.scale(166)
	gap := g.scale(8)
	startX := pad + contentW - (btnW*3 + gap*2)

	pop.btnPrimary = g.createChildControl("BUTTON", "Pause", wsChild|wsVisible|wsTabStop|bsDefPushButton, 0, startX, btnY, btnW, btnH, hwnd, idcProgBtnPrimary, true)
	pop.btnSecondary = g.createChildControl("BUTTON", "Cancel", wsChild|wsVisible|wsTabStop|bsPushButton, 0, startX+btnW+gap, btnY, btnW, btnH, hwnd, idcProgBtnSecondary, false)
	pop.btnClose = g.createChildControl("BUTTON", "Hide", wsChild|wsVisible|wsTabStop|bsPushButton, 0, startX+(btnW+gap)*2, btnY, btnW, btnH, hwnd, idcProgBtnClose, false)

	g.popups[hwnd] = pop
	g.taskPop[taskID] = hwnd

	g.updateProgressPopup(pop, taskSnap)
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
}

func (g *winAppGUI) updateProgressPopup(pop *progressPopup, t *DownloadTask) {
	if pop == nil || pop.hwnd == 0 || t == nil {
		return
	}

	dispTitle := t.Title
	if dispTitle == "" {
		dispTitle = t.FileName
	}
	if dispTitle == "" {
		dispTitle = t.URL
	}

	winTitle := fmt.Sprintf("[%.1f%%] %s — GrabAll", t.Progress, dispTitle)
	if t.Status == StatusCompleted {
		winTitle = fmt.Sprintf("Download Complete — %s", dispTitle)
	} else if t.Status == StatusError {
		winTitle = fmt.Sprintf("Error — %s", dispTitle)
	}
	if pop.lastTitle != winTitle {
		setWindowString(pop.hwnd, winTitle)
		setWindowString(pop.lblTitle, dispTitle)
		setWindowString(pop.lblURL, "Source: "+t.URL)
		pop.lastTitle = winTitle
	}

	var statusLine string
	var mode string
	switch t.Status {
	case StatusDownloading:
		mode = "active"
		if strings.Contains(t.Speed, "Merging") || strings.Contains(t.Speed, "Extracting") ||
			(t.Progress == 0 && t.Speed != "" && !strings.HasSuffix(t.Speed, "/s")) {
			statusLine = "Status: " + t.Speed
		} else {
			statusLine = "Status: Receiving data..."
		}
	case StatusQueued:
		mode = "active"
		statusLine = "Status: Waiting in queue..."
	case StatusPaused:
		mode = "paused"
		statusLine = "Status: Paused (resumable)"
	case StatusCompleted:
		mode = "completed"
		statusLine = "Status: Download complete! Saved to disk."
	case StatusCanceled:
		mode = "error"
		statusLine = "Status: Canceled"
	case StatusError:
		mode = "error"
		errTxt := t.ErrorMessage
		if errTxt == "" {
			errTxt = "Download failed"
		}
		statusLine = "Error: " + errTxt
	}

	if pop.lastStatus != statusLine {
		setWindowString(pop.lblStatus, statusLine)
		pop.lastStatus = statusLine
	}

	sizeLine := fmt.Sprintf("Downloaded: %s   (%.1f%%)", formatTaskSizeCol(t), t.Progress)
	if pop.lastSize != sizeLine {
		setWindowString(pop.lblSize, sizeLine)
		pop.lastSize = sizeLine
	}

	spd := t.Speed
	if spd == "" {
		spd = "—"
	}
	eta := t.ETA
	if eta == "" {
		eta = "—"
	}
	speedLine := fmt.Sprintf("Transfer rate: %s     •     Time left: %s", spd, eta)
	if t.Status == StatusCompleted && t.SavePath != "" {
		speedLine = "Saved: " + filepath.Base(t.SavePath)
	}
	if pop.lastSpeed != speedLine {
		setWindowString(pop.lblSpeed, speedLine)
		pop.lastSpeed = speedLine
	}

	pos := int(t.Progress * 10.0)
	if pos < 0 {
		pos = 0
	}
	if pos > 1000 {
		pos = 1000
	}
	if t.Status == StatusCompleted {
		pos = 1000
	}
	if pop.lastPos != pos {
		procSendMessageW.Call(pop.progBar, pbmSetPos, uintptr(pos), 0)
		pop.lastPos = pos
	}

	if pop.lastMode != mode {
		switch mode {
		case "completed":
			procSendMessageW.Call(pop.progBar, pbmSetState, pbstateNormal, 0)
			setWindowString(pop.btnPrimary, "Open File")
			setWindowString(pop.btnSecondary, "Open Folder")
			setWindowString(pop.btnClose, "Close")
			procEnableWindow.Call(pop.btnPrimary, 1)
			procEnableWindow.Call(pop.btnSecondary, 1)
		case "paused":
			procSendMessageW.Call(pop.progBar, pbmSetState, pbstatePaused, 0)
			setWindowString(pop.btnPrimary, "Resume")
			setWindowString(pop.btnSecondary, "Cancel")
			setWindowString(pop.btnClose, "Hide")
			procEnableWindow.Call(pop.btnPrimary, 1)
			procEnableWindow.Call(pop.btnSecondary, 1)
		case "error":
			procSendMessageW.Call(pop.progBar, pbmSetState, pbstateError, 0)
			setWindowString(pop.btnPrimary, "Retry")
			setWindowString(pop.btnSecondary, "Open Folder")
			setWindowString(pop.btnClose, "Close")
			procEnableWindow.Call(pop.btnPrimary, 1)
			procEnableWindow.Call(pop.btnSecondary, 1)
		default: // active
			procSendMessageW.Call(pop.progBar, pbmSetState, pbstateNormal, 0)
			setWindowString(pop.btnPrimary, "Pause")
			setWindowString(pop.btnSecondary, "Cancel")
			setWindowString(pop.btnClose, "Hide")
			procEnableWindow.Call(pop.btnPrimary, 1)
			procEnableWindow.Call(pop.btnSecondary, 1)
		}
		pop.lastMode = mode
	}
}

func (g *winAppGUI) closeProgressPopup(hwnd uintptr) {
	if pop, ok := g.popups[hwnd]; ok {
		delete(g.taskPop, pop.taskID)
		delete(g.popups, hwnd)
	}
	procDestroyWindow.Call(hwnd)
}

func (g *winAppGUI) findSnapshot(taskID string) *DownloadTask {
	if taskID == "" {
		return nil
	}
	if t := g.qm.GetByID(taskID); t != nil {
		return snapshotTask(t, false)
	}
	return nil
}

// openAddURLDialog opens the native "Add New Download" window, auto-filling clipboard URLs.
func (g *winAppGUI) openAddURLDialog() {
	if g.addDlg != nil && g.addDlg.hwnd != 0 {
		procShowWindow.Call(g.addDlg.hwnd, swShowNormal)
		procSetForegroundWindow.Call(g.addDlg.hwnd)
		return
	}

	w := g.scale(480)
	h := g.scale(312)
	scrW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	scrH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	posX := (int(scrW) - w) / 2
	posY := (int(scrH) - h) / 2

	style := uint32(wsOverlapped | wsCaption | wsSysMenu | wsClipChildren)
	hwnd, _, _ := procCreateWindowExW.Call(
		wsExAppWindow,
		uintptr(unsafe.Pointer(utf16Ptr("GrabAllAddWndClass"))),
		uintptr(unsafe.Pointer(utf16Ptr("Add New Download — GrabAll"))),
		uintptr(style),
		uintptr(posX), uintptr(posY), uintptr(w), uintptr(h),
		g.mainHwnd, 0, g.hInst, 0,
	)
	if hwnd == 0 {
		return
	}

	if g.iconSmall != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmallVal, g.iconSmall)
	}

	pad := g.scale(14)
	contentW := w - pad*2 - g.scale(14)

	g.createChildControl("STATIC", "Video / Playlist / File URL:", wsChild|wsVisible, 0, pad, g.scale(14), contentW, g.scale(18), hwnd, 0, true)
	editURL := g.createChildControl("EDIT", "", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad, g.scale(35), contentW, g.scale(26), hwnd, idcAddEditURL, false)

	g.createChildControl("STATIC", "Format / Quality:", wsChild|wsVisible, 0, pad, g.scale(72), g.scale(130), g.scale(20), hwnd, 0, false)
	combo := g.createChildControl("COMBOBOX", "", wsChild|wsVisible|wsTabStop|cbsDropDownList|wsVScroll, 0, pad+g.scale(125), g.scale(68), contentW-g.scale(125), g.scale(220), hwnd, idcAddComboQual, false)
	for _, qp := range qualityPresets {
		procSendMessageW.Call(combo, cbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(qp.label))))
	}
	procSendMessageW.Call(combo, cbSetCurSel, 0, 0)

	chkPlay := g.createChildControl("BUTTON", "Download YouTube playlist (saves into subfolder)", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad, g.scale(106), contentW, g.scale(22), hwnd, idcAddChkPlay, false)

	g.createChildControl("STATIC", "Playlist Start #:", wsChild|wsVisible, 0, pad, g.scale(138), g.scale(105), g.scale(20), hwnd, 0, false)
	editStart := g.createChildControl("EDIT", "1", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad+g.scale(108), g.scale(135), g.scale(56), g.scale(24), hwnd, idcAddEditStart, false)

	g.createChildControl("STATIC", "Max Videos (0 = All):", wsChild|wsVisible, 0, pad+g.scale(182), g.scale(138), g.scale(145), g.scale(20), hwnd, 0, false)
	editCount := g.createChildControl("EDIT", "0", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad+g.scale(330), g.scale(135), g.scale(66), g.scale(24), hwnd, idcAddEditCount, false)

	chkStream := g.createChildControl("BUTTON", "Watch while downloading (Regular 1-stream live mode)", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad, g.scale(170), contentW, g.scale(22), hwnd, idcAddChkStream, false)
	if g.cfg.GetSpeedMode() == SpeedModeRegular {
		procSendMessageW.Call(chkStream, bmSetCheck, bstChecked, 0)
	}

	btnW := g.scale(130)
	btnH := g.scale(32)
	btnY := g.scale(208)
	g.createChildControl("BUTTON", "Start Download", wsChild|wsVisible|wsTabStop|bsDefPushButton, 0, pad+contentW-btnW*2-g.scale(8), btnY, btnW, btnH, hwnd, idcAddBtnStart, true)
	g.createChildControl("BUTTON", "Cancel", wsChild|wsVisible|wsTabStop|bsPushButton, 0, pad+contentW-btnW, btnY, btnW, btnH, hwnd, idcAddBtnCancel, false)

	// Auto-paste URL from clipboard if present
	if clip := readClipboardText(hwnd); strings.HasPrefix(clip, "http://") || strings.HasPrefix(clip, "https://") {
		setWindowString(editURL, clip)
		if isPurePlaylistURL(clip) || strings.Contains(strings.ToLower(clip), "list=") {
			if isPurePlaylistURL(clip) {
				procSendMessageW.Call(chkPlay, bmSetCheck, bstChecked, 0)
			}
		}
	}

	g.addDlg = &addDialogControls{
		hwnd:      hwnd,
		editURL:   editURL,
		comboQual: combo,
		chkPlay:   chkPlay,
		chkStream: chkStream,
		editStart: editStart,
		editCount: editCount,
	}

	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	procSetFocus.Call(editURL)
}

func readClipboardText(owner uintptr) string {
	r, _, _ := procOpenClipboard.Call(owner)
	if r == 0 {
		return ""
	}
	defer procCloseClipboard.Call()

	hMem, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if hMem == 0 {
		return ""
	}
	byteSize, _, _ := procGlobalSize.Call(hMem)
	if byteSize < 2 {
		return ""
	}
	if byteSize > 8192 {
		byteSize = 8192
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(hMem)

	buf := make([]uint16, byteSize/2)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), ptr, uintptr(len(buf)*2))
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

func writeClipboardText(owner uintptr, text string) {
	u16, err := syscall.UTF16FromString(text)
	if err != nil {
		return
	}
	r, _, _ := procOpenClipboard.Call(owner)
	if r == 0 {
		return
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	sizeBytes := uintptr(len(u16) * 2)
	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, sizeBytes)
	if hMem == 0 {
		return
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return
	}
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&u16[0])), sizeBytes)
	procGlobalUnlock.Call(hMem)
	procSetClipboardData.Call(cfUnicodeText, hMem)
}

func (g *winAppGUI) browseDownloadFolder() {
	displayBuf := make([]uint16, 260)
	bi := browseInfoW{
		HwndOwner:      g.mainHwnd,
		PszDisplayName: &displayBuf[0],
		LpszTitle:      utf16Ptr("Select Default Downloads Folder for GrabAll:"),
		UlFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle,
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return
	}
	defer procCoTaskMemFree.Call(pidl)

	pathBuf := make([]uint16, 520)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	if ok == 0 {
		return
	}
	chosen := syscall.UTF16ToString(pathBuf)
	if chosen != "" {
		g.cfg.mu.Lock()
		g.cfg.DownloadDir = chosen
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
		g.refreshStatusBar()
	}
}

func (g *winAppGUI) showTrayContextMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	procAppendMenuW.Call(hMenu, mfString, cmdShowMain, uintptr(unsafe.Pointer(utf16Ptr("Open GrabAll Window"))))
	procSetMenuDefaultItem.Call(hMenu, cmdShowMain, 0)
	procAppendMenuW.Call(hMenu, mfString, cmdAddURL, uintptr(unsafe.Pointer(utf16Ptr("Add New Download..."))))
	procAppendMenuW.Call(hMenu, mfString, cmdOpenDownloads, uintptr(unsafe.Pointer(utf16Ptr("Open Downloads Folder"))))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)

	turboFlags := uintptr(mfString | mfUnchecked)
	if g.cfg.GetSpeedMode() == SpeedModeTurbo {
		turboFlags = uintptr(mfString | mfChecked)
	}
	procAppendMenuW.Call(hMenu, turboFlags, cmdToggleTurbo, uintptr(unsafe.Pointer(utf16Ptr("⚡ Turbo Multi-Connection Speed (6x)"))))

	startFlags := uintptr(mfString | mfUnchecked)
	if isAutoStartEnabled() {
		startFlags = uintptr(mfString | mfChecked)
	}
	procAppendMenuW.Call(hMenu, startFlags, cmdToggleStartup, uintptr(unsafe.Pointer(utf16Ptr("Start with Windows"))))

	popFlags := uintptr(mfString | mfUnchecked)
	if g.cfg.GetShowProgressPopup() {
		popFlags = uintptr(mfString | mfChecked)
	}
	procAppendMenuW.Call(hMenu, popFlags, cmdTogglePopup, uintptr(unsafe.Pointer(utf16Ptr("Show Download Progress Popup"))))

	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, cmdCheckUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for Updates..."))))
	procAppendMenuW.Call(hMenu, mfString, cmdExit, uintptr(unsafe.Pointer(utf16Ptr("Exit GrabAll"))))

	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(g.mainHwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightBtn|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, g.mainHwnd, 0)
	if cmd != 0 {
		g.handleCommand(cmd)
	}
}

// autoUpgradeSlowYtDlpIfNeeded checks on startup if GrabAll's bin/yt-dlp.exe is still the
// old single-file PyInstaller binary (which takes 60-90s unpacking 146 DLLs on 1GB RAM PCs),
// and seamlessly upgrades it once in the background to the pre-extracted yt-dlp_win.zip layout.
func (g *winAppGUI) autoUpgradeSlowYtDlpIfNeeded() {
	ytPath, err := resolveYtDlpBinary(g.cfg)
	if err != nil {
		return
	}
	if isManagedSlowWindowsYtDlp(ytPath) {
		g.postStatusMessage("⚡ Upgrading yt-dlp to Fast-Start Mode (one-time fix for 1-2m startup delay)...")
		upgraded, upErr := DownloadYtDlpStandalone(filepath.Dir(ytPath))
		g.postStatusMessage("")
		if upErr != nil {
			return
		}
		g.cfg.mu.Lock()
		g.cfg.YtDlpPath = upgraded
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
		g.refreshStatusBar()
		g.showTrayBalloon("Fast-Start Mode Enabled", "yt-dlp upgraded! Video downloads now start in ~2-4 seconds.")
	}
	// Pre-warm YouTube's preprocessed base.js solver cache silently in the background!
	PrewarmYouTubeSolverCache(g.cfg)
}

// checkFirstRunEnginePrompt asks the user on the very first launch if they want to install
// yt-dlp & FFmpeg. If they skip, it marks it as shown so it never nags again, and
// just-in-time auto-download handles it automatically whenever they grab a video.
func (g *winAppGUI) checkFirstRunEnginePrompt() {
	g.cfg.mu.RLock()
	shown := g.cfg.FirstRunPromptShown
	g.cfg.mu.RUnlock()
	if shown {
		return
	}

	ytPath, ytErr := resolveYtDlpBinary(g.cfg)
	ffmpegPath := resolveFFmpegBinary(g.cfg)

	// If yt-dlp is already present, don't nag
	if ytErr == nil && ytPath != "" {
		g.cfg.mu.Lock()
		g.cfg.FirstRunPromptShown = true
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
		return
	}

	// Mark prompt shown so it only asks once on fresh launch
	g.cfg.mu.Lock()
	g.cfg.FirstRunPromptShown = true
	g.cfg.mu.Unlock()
	_ = g.cfg.Save()

	time.Sleep(400 * time.Millisecond)

	msg := "Welcome to GrabAll Download Manager!\n\n" +
		"To enable 1-click video downloads from YouTube, TikTok, Facebook, and 1080p/4K audio merging, GrabAll needs its helper engines (yt-dlp & FFmpeg).\n\n" +
		"Would you like to install them now?\n\n" +
		"• Click 'Yes' to download and set them up now in the background.\n" +
		"• Click 'No' to skip. GrabAll will auto-download them whenever you grab your first video."

	res, _, _ := procMessageBoxW.Call(
		g.mainHwnd,
		uintptr(unsafe.Pointer(utf16Ptr(msg))),
		uintptr(unsafe.Pointer(utf16Ptr("First-Time Setup — GrabAll"))),
		mbYesNo|mbIconQuestion,
	)

	if res == idYes {
		go g.installAllEnginesInBackground(ytErr != nil, ffmpegPath == "")
	}
}

func (g *winAppGUI) installAllEnginesInBackground(needYtDlp, needFFmpeg bool) {
	binDir := filepath.Join(executableDir(), "bin")
	_ = os.MkdirAll(binDir, 0755)

	g.postStatusMessage("⚡ Setting up video engines (yt-dlp & FFmpeg)... please wait...")

	if needYtDlp {
		g.postStatusMessage("⚡ Downloading yt-dlp & QuickJS engine...")
		path, err := DownloadYtDlpStandalone(binDir)
		if err != nil {
			g.postStatusMessage("❌ Failed to download yt-dlp: " + err.Error())
			return
		}
		g.cfg.mu.Lock()
		g.cfg.YtDlpPath = path
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
	}

	if needFFmpeg {
		g.postStatusMessage("⚡ Downloading FFmpeg media merger...")
		path, err := DownloadFFmpegStandalone(binDir)
		if err != nil {
			g.postStatusMessage("❌ Failed to download FFmpeg: " + err.Error())
			return
		}
		g.cfg.mu.Lock()
		g.cfg.FFmpegPath = path
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
	}

	g.postStatusMessage("✅ All engines ready! YouTube, 1080p/4K & MP3 enabled.")
	g.refreshStatusBar()
	g.showTrayBalloon("GrabAll Engines Ready", "yt-dlp and FFmpeg are installed and ready. Click Grab in Chrome or Brave anytime!")
	PrewarmYouTubeSolverCache(g.cfg)
}

func (g *winAppGUI) showToolsPopupMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	ytPath, ytErr := resolveYtDlpBinary(g.cfg)
	ytLabel := "Update yt-dlp + QuickJS (Fast-Start Installed)"
	if ytErr != nil {
		ytLabel = "Install yt-dlp + QuickJS (Required for YouTube/TikTok/FB)"
	} else if !isFastUnpackedYtDlp(ytPath) {
		ytLabel = "⚡ Upgrade yt-dlp to Fast-Start Mode (yt-dlp_win.zip — fixes 1-2m delay)"
	}
	ffLabel := "Update FFmpeg (Installed)"
	if resolveFFmpegBinary(g.cfg) == "" {
		ffLabel = "Install FFmpeg (Needed for 1080p+ HD & MP3 Audio)"
	}

	procAppendMenuW.Call(hMenu, mfString, cmdInstallYtDlp, uintptr(unsafe.Pointer(utf16Ptr(ytLabel))))
	procAppendMenuW.Call(hMenu, mfString, cmdInstallFFmpeg, uintptr(unsafe.Pointer(utf16Ptr(ffLabel))))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, cmdChangeDir, uintptr(unsafe.Pointer(utf16Ptr("Change Default Save Folder..."))))
	procAppendMenuW.Call(hMenu, mfString, cmdCheckUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for GrabAll PC Updates..."))))
	procAppendMenuW.Call(hMenu, mfString, cmdOpenWeb, uintptr(unsafe.Pointer(utf16Ptr("Open Web UI in Browser"))))

	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightBtn|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, g.mainHwnd, 0)
	if cmd != 0 {
		g.handleCommand(cmd)
	}
}

func (g *winAppGUI) showRowContextMenu(taskID string) {
	snap := g.findSnapshot(taskID)
	if snap == nil {
		return
	}

	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	openState := uintptr(mfString | mfGrayed)
	openLabel := "Open / Play File"
	if snap.Status == StatusCompleted {
		openState = uintptr(mfString | mfEnabled)
	} else if snap.Status == StatusDownloading || snap.Status == StatusPaused {
		openState = uintptr(mfString | mfEnabled)
		openLabel = "🎬 Watch / Play While Downloading"
	}
	procAppendMenuW.Call(hMenu, openState, cmdOpenFile, uintptr(unsafe.Pointer(utf16Ptr(openLabel))))
	procAppendMenuW.Call(hMenu, mfString, cmdOpenFolder, uintptr(unsafe.Pointer(utf16Ptr("Open Containing Folder"))))
	procAppendMenuW.Call(hMenu, mfString, cmdShowProgress, uintptr(unsafe.Pointer(utf16Ptr("Show Progress Window"))))

	if snap.Status != StatusCompleted {
		effMode := snap.SpeedMode
		if effMode != SpeedModeRegular && effMode != SpeedModeTurbo {
			effMode = g.cfg.GetSpeedMode()
		}
		procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
		if effMode == SpeedModeTurbo {
			procAppendMenuW.Call(hMenu, mfString, cmdTaskSwitchRegular, uintptr(unsafe.Pointer(utf16Ptr("🎬 Switch to Regular Mode (Watch While Downloading)"))))
		} else {
			procAppendMenuW.Call(hMenu, mfString, cmdTaskSwitchTurbo, uintptr(unsafe.Pointer(utf16Ptr("⚡ Switch to Turbo Speed (6x Multi-Connection)"))))
		}
	}

	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, cmdResume, uintptr(unsafe.Pointer(utf16Ptr("Resume / Retry"))))
	procAppendMenuW.Call(hMenu, mfString, cmdPause, uintptr(unsafe.Pointer(utf16Ptr("Pause"))))
	procAppendMenuW.Call(hMenu, mfString, cmdCancel, uintptr(unsafe.Pointer(utf16Ptr("Cancel"))))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, cmdCopyURL, uintptr(unsafe.Pointer(utf16Ptr("Copy Download URL"))))
	procAppendMenuW.Call(hMenu, mfString, cmdDelete, uintptr(unsafe.Pointer(utf16Ptr("Remove from List"))))
	procAppendMenuW.Call(hMenu, mfString, cmdDeleteDisk, uintptr(unsafe.Pointer(utf16Ptr("Delete from List + Disk"))))

	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightBtn|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, g.mainHwnd, 0)
	if cmd != 0 {
		g.handleCommand(cmd)
	}
}

func (g *winAppGUI) handleCommand(cmdID uintptr) {
	selID := g.getSelectedTaskID()

	switch cmdID {
	case cmdShowMain:
		g.showMainWindow()

	case cmdAddURL:
		g.openAddURLDialog()

	case cmdToolsMenu:
		g.showToolsPopupMenu()

	case cmdResume:
		if selID != "" {
			g.qm.ResumeTask(selID)
		}

	case cmdPause:
		if selID != "" {
			g.qm.PauseTask(selID)
		}

	case cmdCancel:
		if selID != "" {
			g.qm.CancelTask(selID)
		}

	case cmdDelete:
		if selID != "" {
			g.qm.DeleteTask(selID, false)
			g.syncListView()
		}

	case cmdDeleteDisk:
		if selID != "" {
			ret, _, _ := procMessageBoxW.Call(
				g.mainHwnd,
				uintptr(unsafe.Pointer(utf16Ptr("Delete this download from both the queue list AND your disk?"))),
				uintptr(unsafe.Pointer(utf16Ptr("Confirm File Deletion"))),
				mbYesNo|mbIconWarning,
			)
			if ret == idYes {
				g.qm.DeleteTask(selID, true)
				g.syncListView()
			}
		}

	case cmdOpenFolder:
		if selID != "" {
			fp, dir := resolveTaskRevealPaths(g.cfg, g.qm, selID)
			openFolderOrSelectFile(fp, dir)
		} else {
			openFolderOrSelectFile("", g.cfg.GetDownloadDir())
		}

	case cmdOpenFile:
		if selID != "" {
			fp, dir := resolveTaskRevealPaths(g.cfg, g.qm, selID)
			if fp == "" || openFileDefault(fp) != nil {
				openFolderOrSelectFile(fp, dir)
			}
		}

	case cmdShowProgress:
		if selID != "" {
			g.openOrFocusProgressPopup(selID, false)
		}

	case cmdCopyURL:
		if snap := g.findSnapshot(selID); snap != nil {
			writeClipboardText(g.mainHwnd, snap.URL)
		}

	case cmdClearDone:
		g.qm.ClearCompleted()
		g.syncListView()

	case cmdOpenDownloads:
		openFolderOrSelectFile("", g.cfg.GetDownloadDir())

	case cmdChangeDir:
		g.browseDownloadFolder()

	case cmdCopyToken:
		writeClipboardText(g.mainHwnd, g.cfg.GetToken())
		procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr("Auth Token copied to clipboard!\n\n(Note: Chrome/Brave extension v2.0+ pairs automatically — manual token paste is no longer required.)"))),
			uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))),
			mbOK|mbIconInfo,
		)

	case cmdInstallYtDlp:
		go func() {
			g.postStatusMessage("Installing Fast-Start yt-dlp_win.zip + QuickJS (qjs.exe)... please wait...")
			binDir := filepath.Join(executableDir(), "bin")
			path, err := DownloadYtDlpStandalone(binDir)
			if err != nil {
				g.postStatusMessage("")
				procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Failed to install yt-dlp:\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll Error"))), mbOK|mbIconError)
				return
			}
			g.cfg.mu.Lock()
			g.cfg.YtDlpPath = path
			g.cfg.mu.Unlock()
			_ = g.cfg.Save()
			g.postStatusMessage("")
			g.showTrayBalloon("Fast-Start yt-dlp + QuickJS Ready", "Installed unpacked build to "+path)
			go PrewarmYouTubeSolverCache(g.cfg)
		}()

	case cmdInstallFFmpeg:
		go func() {
			g.postStatusMessage("Downloading static FFmpeg (ffmpeg.exe)... please wait...")
			binDir := filepath.Join(executableDir(), "bin")
			path, err := DownloadFFmpegStandalone(binDir)
			if err != nil {
				g.postStatusMessage("")
				procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Failed to install FFmpeg:\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll Error"))), mbOK|mbIconError)
				return
			}
			g.cfg.mu.Lock()
			g.cfg.FFmpegPath = path
			g.cfg.mu.Unlock()
			_ = g.cfg.Save()
			g.postStatusMessage("")
			g.showTrayBalloon("FFmpeg Ready", "1080p+ HD merging & MP3 extraction enabled!")
		}()

	case cmdToggleTurbo:
		if g.cfg.GetSpeedMode() == SpeedModeTurbo {
			g.cfg.SetSpeedMode(SpeedModeRegular)
		} else {
			g.cfg.SetSpeedMode(SpeedModeTurbo)
		}
		g.refreshOptionMenuChecks()
		g.refreshStatusBar()

	case cmdTaskSwitchRegular:
		if selID != "" {
			g.qm.SetTaskSpeedMode(selID, SpeedModeRegular)
		}

	case cmdTaskSwitchTurbo:
		if selID != "" {
			g.qm.SetTaskSpeedMode(selID, SpeedModeTurbo)
		}

	case cmdToggleStartup:
		next := !isAutoStartEnabled()
		if err := setAutoStartEnabled(next); err != nil {
			procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr("Could not update startup setting:\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))), mbOK|mbIconError)
		}
		g.refreshOptionMenuChecks()

	case cmdTogglePopup:
		g.cfg.SetShowProgressPopup(!g.cfg.GetShowProgressPopup())
		g.refreshOptionMenuChecks()

	case cmdToggleAutoUpdate:
		g.cfg.SetAutoCheckUpdates(!g.cfg.GetAutoCheckUpdates())
		g.refreshOptionMenuChecks()

	case cmdConc1, cmdConc2, cmdConc3, cmdConc4:
		n := int(cmdID-cmdConc1) + 1
		g.cfg.mu.Lock()
		g.cfg.MaxConcurrent = n
		g.cfg.mu.Unlock()
		_ = g.cfg.Save()
		g.refreshOptionMenuChecks()
		g.qm.triggerLoop()

	case cmdOpenWeb:
		openURLDefault(fmt.Sprintf("http://127.0.0.1:%d", g.cfg.Port))

	case cmdCheckUpdate:
		go g.checkForAppUpdate(true, false)

	case cmdAbout:
		procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("GrabAll Download Manager v%s (Native Win32 Edition)\n\nUltra-lightweight high-speed downloader built for low-RAM PCs.\n• Native Win32 GUI (~14 MB RAM, zero WebView/Electron bloat)\n• Turbo 6x Parallel Engine + Switchable Regular Watch-Live Mode\n• Clean Video Hover Grabber + Dedicated Playlist Downloader", AppVersion)))),
			uintptr(unsafe.Pointer(utf16Ptr("About GrabAll"))),
			mbOK|mbIconInfo,
		)

	case cmdExit:
		g.quitting = true
		for hwnd := range g.popups {
			procDestroyWindow.Call(hwnd)
		}
		g.removeTrayIcon()
		g.qm.Stop()
		g.qm.save()
		go g.srv.Shutdown()
		procDestroyWindow.Call(g.mainHwnd)
	}
}

func mainWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	g := appGUI
	if g == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmCreate:
		g.mainHwnd = hwnd
		g.onCreateMainWindow(hwnd)
		return 0

	case wmSize:
		g.layoutMainWindow(hwnd)
		return 0

	case wmGetMinMaxInfo:
		if lParam != 0 {
			var mmi winMinMaxInfo
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&mmi)), lParam, unsafe.Sizeof(mmi))
			mmi.PtMinTrackSize.X = int32(g.scale(680))
			mmi.PtMinTrackSize.Y = int32(g.scale(340))
			procRtlMoveMemory.Call(lParam, uintptr(unsafe.Pointer(&mmi)), unsafe.Sizeof(mmi))
		}
		return 0

	case wmCtlColorStatic:
		procSetBkMode.Call(wParam, transparentBkMode)
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmCommand:
		cmdID := wParam & 0xFFFF
		g.handleCommand(cmdID)
		return 0

	case wmNotify:
		if lParam != 0 {
			var hdr nmHdr
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&hdr)), lParam, unsafe.Sizeof(hdr))
			if hdr.IdFrom == idcListView {
				switch hdr.Code {
				case nmDblClk:
					var act nmItemActivate
					procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&act)), lParam, unsafe.Sizeof(act))
					row := int(act.IItem)
					if row >= 0 && row < len(g.rows) {
						taskID := g.rows[row].id
						if snap := g.findSnapshot(taskID); snap != nil {
							if snap.Status == StatusCompleted {
								fp, dir := resolveTaskRevealPaths(g.cfg, g.qm, taskID)
								if fp == "" || openFileDefault(fp) != nil {
									openFolderOrSelectFile(fp, dir)
								}
							} else if snap.Status == StatusError {
								ret, _, _ := procMessageBoxW.Call(
									hwnd,
									uintptr(unsafe.Pointer(utf16Ptr("Download Error:\n\n"+snap.ErrorMessage+"\n\nRetry this download now?"))),
									uintptr(unsafe.Pointer(utf16Ptr("Download Error"))),
									mbYesNo|mbIconWarning,
								)
								if ret == idYes {
									g.qm.ResumeTask(taskID)
								}
							} else {
								g.openOrFocusProgressPopup(taskID, false)
							}
						}
					}
				case nmRClick:
					var act nmItemActivate
					procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&act)), lParam, unsafe.Sizeof(act))
					row := int(act.IItem)
					if row >= 0 && row < len(g.rows) {
						g.selectRowIndex(row)
						g.showRowContextMenu(g.rows[row].id)
					}
				}
			}
		}
		return 0

	case wmAppTray:
		switch uint32(lParam) {
		case wmLButtonUp, wmLButtonDblClk:
			g.showMainWindow()
		case wmRButtonUp:
			g.showTrayContextMenu()
		}
		return 0

	case wmAppShowMain:
		g.showMainWindow()
		return 0

	case wmAppQueueUpdate:
		g.updateQueued.Store(false)
		g.syncListView()
		return 0

	case wmAppTaskAdded:
		g.eventsMu.Lock()
		ids := append([]string(nil), g.pendingTaskIDs...)
		g.pendingTaskIDs = g.pendingTaskIDs[:0]
		g.eventsMu.Unlock()

		g.syncListView()
		if g.cfg.GetShowProgressPopup() {
			for _, id := range ids {
				g.openOrFocusProgressPopup(id, true)
			}
		}
		return 0

	case wmAppPlaylistAdd:
		g.eventsMu.Lock()
		notes := append([]string(nil), g.pendingPlaylist...)
		g.pendingPlaylist = g.pendingPlaylist[:0]
		g.eventsMu.Unlock()

		g.syncListView()
		g.showMainWindow()
		for _, n := range notes {
			g.showTrayBalloon("Playlist Added to GrabAll", n)
		}
		return 0

	case wmAppStatusMsg:
		g.refreshStatusBar()
		return 0

	case wmAppRestartForUpdate:
		g.performGracefulRestartForUpdate()
		return 0

	case wmClose:
		if !g.quitting {
			procShowWindow.Call(hwnd, swHide)
			if !g.shownTrayHint {
				g.shownTrayHint = true
				g.showTrayBalloon("GrabAll is running in the tray", "Click Grab in Chrome/Brave anytime. Right-click the tray icon to Exit.")
			}
			return 0
		}

	case wmDestroy:
		if g.quitting {
			procPostQuitMessage.Call(0)
			return 0
		}
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func progressWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	g := appGUI
	if g == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmCtlColorStatic:
		procSetBkMode.Call(wParam, transparentBkMode)
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmCommand:
		pop, ok := g.popups[hwnd]
		if !ok {
			return 0
		}
		snap := g.findSnapshot(pop.taskID)
		cmdID := wParam & 0xFFFF

		switch cmdID {
		case idcProgBtnPrimary:
			if snap == nil {
				return 0
			}
			switch snap.Status {
			case StatusCompleted:
				fp, dir := resolveTaskRevealPaths(g.cfg, g.qm, pop.taskID)
				if fp == "" || openFileDefault(fp) != nil {
					openFolderOrSelectFile(fp, dir)
				}
				g.closeProgressPopup(hwnd)
			case StatusPaused, StatusError, StatusCanceled:
				g.qm.ResumeTask(pop.taskID)
			default:
				g.qm.PauseTask(pop.taskID)
			}

		case idcProgBtnSecondary:
			if snap == nil {
				return 0
			}
			if snap.Status == StatusCompleted || snap.Status == StatusError || snap.Status == StatusCanceled {
				fp, dir := resolveTaskRevealPaths(g.cfg, g.qm, pop.taskID)
				openFolderOrSelectFile(fp, dir)
			} else {
				g.qm.CancelTask(pop.taskID)
			}

		case idcProgBtnClose:
			g.closeProgressPopup(hwnd)
		}
		return 0

	case wmClose:
		g.closeProgressPopup(hwnd)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func addDlgWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	g := appGUI
	if g == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case wmCtlColorStatic:
		procSetBkMode.Call(wParam, transparentBkMode)
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmCommand:
		cmdID := wParam & 0xFFFF
		switch cmdID {
		case idcAddBtnStart:
			if g.addDlg == nil {
				return 0
			}
			rawURL := strings.TrimSpace(getWindowString(g.addDlg.editURL))
			if rawURL == "" {
				procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr("Please paste a valid HTTP/HTTPS URL."))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))), mbOK|mbIconWarning)
				return 0
			}
			selIdx, _, _ := procSendMessageW.Call(g.addDlg.comboQual, cbGetCurSel, 0, 0)
			qual := "best"
			if int(selIdx) >= 0 && int(selIdx) < len(qualityPresets) {
				qual = qualityPresets[int(selIdx)].value
			}
			chk, _, _ := procSendMessageW.Call(g.addDlg.chkPlay, bmGetCheck, 0, 0)
			isPlaylist := chk == bstChecked || isPurePlaylistURL(rawURL)
			chkStream, _, _ := procSendMessageW.Call(g.addDlg.chkStream, bmGetCheck, 0, 0)
			speedMode := SpeedModeTurbo
			if chkStream == bstChecked {
				speedMode = SpeedModeRegular
			}
			plStart, _ := strconv.Atoi(strings.TrimSpace(getWindowString(g.addDlg.editStart)))
			plCount, _ := strconv.Atoi(strings.TrimSpace(getWindowString(g.addDlg.editCount)))
			if plStart < 1 {
				plStart = 1
			}
			if plCount < 0 {
				plCount = 0
			}

			g.addDlg = nil
			procDestroyWindow.Call(hwnd)

			req := AddRequest{
				URL:           rawURL,
				Engine:        EngineAuto,
				Quality:       qual,
				SpeedMode:     speedMode,
				Playlist:      isPlaylist,
				PlaylistStart: plStart,
				PlaylistCount: plCount,
			}

			if isPlaylist {
				go func() {
					g.postStatusMessage("Scanning playlist entries with yt-dlp...")
					_, _, err := g.qm.AddPlaylistTasks(context.Background(), req)
					g.postStatusMessage("")
					if err != nil {
						procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Playlist error:\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))), mbOK|mbIconError)
					}
				}()
			} else {
				if _, err := g.qm.AddTask(req); err != nil {
					procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Failed to add download:\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))), mbOK|mbIconError)
				}
			}

		case idcAddBtnCancel:
			g.addDlg = nil
			procDestroyWindow.Call(hwnd)
		}
		return 0

	case wmClose:
		g.addDlg = nil
		procDestroyWindow.Call(hwnd)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func (g *winAppGUI) checkForAppUpdate(manual bool, startInTray bool) {
	if !g.updating.CompareAndSwap(false, true) {
		if manual {
			procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr("An update check or installation is already in progress."))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll Update"))), mbOK|mbIconInfo)
		}
		return
	}
	defer g.updating.Store(false)

	if g.srv == nil || g.srv.updater == nil {
		return
	}

	if manual {
		g.postStatusMessage("Checking for GrabAll PC updates...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	info, err := g.srv.updater.CheckForUpdate(ctx, manual)
	cancel()

	if manual {
		g.postStatusMessage("")
	}

	if err != nil {
		if manual {
			procMessageBoxW.Call(
				g.mainHwnd,
				uintptr(unsafe.Pointer(utf16Ptr("Could not check for updates:\n\n"+err.Error()))),
				uintptr(unsafe.Pointer(utf16Ptr("GrabAll Update Check"))),
				mbOK|mbIconWarning,
			)
		}
		return
	}

	if !info.UpdateAvailable {
		if manual {
			procMessageBoxW.Call(
				g.mainHwnd,
				uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("GrabAll PC is up to date! (v%s)", info.CurrentVersion)))),
				uintptr(unsafe.Pointer(utf16Ptr("GrabAll Update Check"))),
				mbOK|mbIconInfo,
			)
		}
		return
	}

	if !manual && startInTray {
		g.showTrayBalloon(
			fmt.Sprintf("GrabAll PC Update Available (v%s)", info.LatestVersion),
			"Open Help -> Check for Updates to download and install.",
		)
		return
	}

	prompt := fmt.Sprintf(
		"A new version of GrabAll PC is available!\n\nCurrent Version: v%s\nLatest Version: v%s\nPackage: %s\n\nDownload and install the update now? (GrabAll will restart automatically.)",
		info.CurrentVersion, info.LatestVersion, info.FileName,
	)
	res, _, _ := procMessageBoxW.Call(
		g.mainHwnd,
		uintptr(unsafe.Pointer(utf16Ptr(prompt))),
		uintptr(unsafe.Pointer(utf16Ptr("GrabAll Update Available"))),
		mbYesNo|mbIconQuestion,
	)
	if res == idYes {
		g.runAppUpdateInstall(info)
	}
}

func (g *winAppGUI) runAppUpdateInstall(info *UpdateInfo) {
	if g.qm != nil && g.qm.countDownloading() > 0 {
		res, _, _ := procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr("Downloads are currently active.\n\nPause active downloads and install the update now?"))),
			uintptr(unsafe.Pointer(utf16Ptr("Active Downloads"))),
			mbYesNo|mbIconWarning,
		)
		if res != idYes {
			return
		}
		for _, t := range g.qm.GetAll() {
			if t.Status == StatusDownloading {
				g.qm.PauseTask(t.ID)
			}
		}
	}

	if !info.CanAutoUpdate {
		msg := fmt.Sprintf("GrabAll v%s is installed in a protected folder that cannot be updated automatically.\n\nOpen the release download page in your browser?", info.LatestVersion)
		res, _, _ := procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr(msg))),
			uintptr(unsafe.Pointer(utf16Ptr("Manual Update Required"))),
			mbYesNo|mbIconInfo,
		)
		if res == idYes && info.ReleaseURL != "" {
			openURLDefault(info.ReleaseURL)
		}
		return
	}

	g.postStatusMessage(fmt.Sprintf("Downloading GrabAll v%s update...", info.LatestVersion))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err := g.srv.updater.ApplyUpdate(ctx, info, func(stage string, downloaded, total int64) {
		switch stage {
		case "downloading":
			if total > 0 {
				pct := float64(downloaded) / float64(total) * 100.0
				g.postStatusMessage(fmt.Sprintf("Downloading GrabAll v%s... %.0f%%", info.LatestVersion, pct))
			} else if downloaded > 0 {
				g.postStatusMessage(fmt.Sprintf("Downloading GrabAll v%s... %.1f MB", info.LatestVersion, float64(downloaded)/float64(1<<20)))
			}
		case "extracting":
			g.postStatusMessage(fmt.Sprintf("Installing GrabAll v%s...", info.LatestVersion))
		case "restarting":
			g.postStatusMessage(fmt.Sprintf("Restarting GrabAll v%s...", info.LatestVersion))
		}
	})

	if err != nil {
		g.postStatusMessage("")
		msg := fmt.Sprintf("Failed to install update v%s:\n\n%s\n\nOpen the release download page in your browser instead?", info.LatestVersion, err.Error())
		res, _, _ := procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr(msg))),
			uintptr(unsafe.Pointer(utf16Ptr("Update Failed"))),
			mbYesNo|mbIconError,
		)
		if res == idYes && info.ReleaseURL != "" {
			openURLDefault(info.ReleaseURL)
		}
		return
	}

	g.postStatusMessage(fmt.Sprintf("Restarting GrabAll v%s...", info.LatestVersion))
	if g.mainHwnd != 0 {
		procPostMessageW.Call(g.mainHwnd, wmAppRestartForUpdate, 0, 0)
	}
}

func (g *winAppGUI) performGracefulRestartForUpdate() {
	g.quitting = true
	for hwnd := range g.popups {
		procDestroyWindow.Call(hwnd)
	}
	g.removeTrayIcon()
	g.qm.Stop()
	g.qm.save()
	g.srv.Shutdown()
	releaseSingleInstanceLock()

	if exePath, err := currentExecutablePath(); err == nil && exePath != "" {
		_ = startDetachedExecutable(exePath, buildRestartArgs(os.Args[1:], os.Getpid()))
	}
	procDestroyWindow.Call(g.mainHwnd)
}
