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
	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmSetFocus       = 0x0007
	wmKillFocus      = 0x0008
	wmSetText        = 0x000C
	wmPaint          = 0x000F
	wmClose          = 0x0010
	wmQuit           = 0x0012
	wmEraseBkgnd     = 0x0014
	wmGetMinMaxInfo  = 0x0024
	wmDrawItem       = 0x002B
	wmMeasureItem    = 0x002C
	wmSetFont        = 0x0030
	wmNotify         = 0x004E
	wmSetIcon        = 0x0080
	wmCommand        = 0x0111
	wmSysCommand     = 0x0112
	wmTimer          = 0x0113
	wmCtlColorEdit   = 0x0133
	wmCtlColorListBox = 0x0134
	wmCtlColorBtn    = 0x0135
	wmCtlColorStatic = 0x0138
	wmLButtonUp      = 0x0202
	wmLButtonDblClk  = 0x0203
	wmRButtonUp      = 0x0205

	wmApp            = 0x8000
	wmAppTray        = wmApp + 1
	wmAppQueueUpdate = wmApp + 2
	wmAppTaskAdded   = wmApp + 3
	wmAppPlaylistAdd = wmApp + 4
	wmAppShowMain         = wmApp + 5
	wmAppStatusMsg        = wmApp + 6
	wmAppRestartForUpdate = wmApp + 7
	wmAppRetryTray        = wmApp + 8
	wmAppAddPreviewReady  = wmApp + 9

	// Button / Edit / Static / Combo Styles
	bsPushButton    = 0x00000000
	bsDefPushButton = 0x00000001
	bsAutoCheckBox  = 0x00000003
	bsOwnerDraw     = 0x0000000B
	bmGetCheck      = 0x00F0
	bmSetCheck      = 0x00F1
	bstChecked      = 1
	bstUnchecked    = 0

	odsSelected = 0x0001
	odsDisabled = 0x0004
	odsFocus    = 0x0010
	odsHotLight = 0x0040

	odtMenu    = 1
	odtListBox = 2
	odtButton  = 4

	psSolid   = 0
	nullBrush = 5

	esAutoHScroll = 0x0080
	emSetCueBanner = 0x1501
	enChange       = 0x0300

	ssLeftNoWrap  = 0x000C
	ssEndEllipsis = 0x4000

	lbsNotify           = 0x0001
	lbsOwnerDrawFixed   = 0x0010
	lbsHasStrings       = 0x0040
	lbsNoIntegralHeight = 0x0100
	lbAddString         = 0x0180
	lbInsertString      = 0x0181
	lbDeleteString      = 0x0182
	lbSetCurSel         = 0x0186
	lbGetCurSel         = 0x0188
	lbSetItemHeight     = 0x01A0
	lbnSelChange        = 1

	cbsDropDownList = 0x0003
	cbAddString     = 0x0143
	cbGetCurSel     = 0x0147
	cbResetContent  = 0x014B
	cbSetCurSel     = 0x014E
	cbnSelChange    = 1

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
	lvmSetBkColor               = lvmFirst + 1
	lvmDeleteItem               = lvmFirst + 8
	lvmDeleteAllItems           = lvmFirst + 9
	lvmGetNextItem              = lvmFirst + 12
	lvmRedrawItems              = lvmFirst + 21
	lvmSetColumnWidth           = lvmFirst + 30
	lvmGetHeader                = lvmFirst + 31
	lvmSetTextColor             = lvmFirst + 36
	lvmSetTextBkColor           = lvmFirst + 38
	lvmSetItemState             = lvmFirst + 43
	lvmGetItemState             = lvmFirst + 44
	lvmSetExtendedListViewStyle = lvmFirst + 54
	lvmGetSubItemRect           = lvmFirst + 56
	lvmInsertItemW              = lvmFirst + 77
	lvmInsertColumnW            = lvmFirst + 97
	lvmSetItemTextW             = lvmFirst + 116
	lvirBounds                  = 0

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

	nmDblClk     = ^uint32(2)  // -3
	nmRClick     = ^uint32(4)  // -5
	nmCustomDraw = ^uint32(11) // -12

	cddsPrePaint        = 0x00000001
	cddsItemPrePaint    = 0x00010001
	cddsSubItem         = 0x00020000
	cdrfDoDefault       = 0x00000000
	cdrfNewFont         = 0x00000002
	cdrfSkipDefault     = 0x00000004
	cdrfNotifyItemDraw  = 0x00000020
	cdrfNotifySubItemDraw = 0x00000020

	dtLeft        = 0x00000000
	dtCenter      = 0x00000001
	dtRight       = 0x00000002
	dtVCenter     = 0x00000004
	dtSingleLine  = 0x00000020
	dtEndEllipsis = 0x00008000

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
	cmdCheckYtDlpUpdate  = 1033
	cmdShowMain          = 1040
	cmdToolsMenu         = 1041
	cmdGlobalLimit0      = 1050
	cmdGlobalLimit512    = 1051
	cmdGlobalLimit1024   = 1052
	cmdGlobalLimit2048   = 1053
	cmdGlobalLimit5120   = 1054
	cmdGlobalLimit10240  = 1055
	cmdTaskLimit0        = 1060
	cmdTaskLimit512      = 1061
	cmdTaskLimit1024     = 1062
	cmdTaskLimit2048     = 1063
	cmdTaskLimit5120     = 1064
	cmdTaskLimit10240    = 1065
	cmdThemeAuto         = 1070
	cmdThemeLight        = 1071
	cmdThemeDark         = 1072
	cmdWhenDoneNone      = 1075
	cmdWhenDoneOpenFile  = 1076
	cmdWhenDoneOpenFolder = 1077
	cmdWhenDoneSleep      = 1078
	cmdWhenDoneShutdown   = 1079
	cmdMenuTasks          = 1085
	cmdMenuTools          = 1086
	cmdMenuOptions        = 1087
	cmdMenuHelp           = 1088
	cmdExit               = 1099

	idcListView   = 2001
	idcStatus     = 2002
	idcSidebar    = 2003
	idcSearchEdit = 2004

	// Progress Popup Control IDs
	idcProgBtnPrimary    = 3001
	idcProgBtnSecondary  = 3002
	idcProgBtnClose      = 3003
	idcProgComboWhenDone = 3004

	// Add Dialog Control IDs
	idcAddEditURL     = 4001
	idcAddComboQual   = 4002
	idcAddChkPlay     = 4003
	idcAddBtnStart    = 4004
	idcAddBtnCancel   = 4005
	idcAddEditStart   = 4006
	idcAddEditCount   = 4007
	idcAddChkStream   = 4008
	idcAddBtnPreview  = 4009
	idcAddChkSubs     = 4010
	idcAddEditSubLang = 4011
	idcAddChkChapters = 4012
)

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modGdi32    = syscall.NewLazyDLL("gdi32.dll")
	modComctl32 = syscall.NewLazyDLL("comctl32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modOle32    = syscall.NewLazyDLL("ole32.dll")
	modDwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	modUxtheme  = syscall.NewLazyDLL("uxtheme.dll")
	modMsimg32  = syscall.NewLazyDLL("msimg32.dll")

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
	procFindWindowW            = modUser32.NewProc("FindWindowW")
	procRegisterWindowMessageW = modUser32.NewProc("RegisterWindowMessageW")
	procFillRect               = modUser32.NewProc("FillRect")
	procFrameRect              = modUser32.NewProc("FrameRect")
	procDrawTextW              = modUser32.NewProc("DrawTextW")
	procInvalidateRect         = modUser32.NewProc("InvalidateRect")
	procBeginPaint             = modUser32.NewProc("BeginPaint")
	procEndPaint               = modUser32.NewProc("EndPaint")
	procSetClassLongPtrW       = modUser32.NewProc("SetClassLongPtrW")
	procSetWindowLongPtrW      = modUser32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW        = modUser32.NewProc("CallWindowProcW")
	procGetWindowRect          = modUser32.NewProc("GetWindowRect")

	procGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
	procCreateMutexW     = modKernel32.NewProc("CreateMutexW")
	procGlobalAlloc      = modKernel32.NewProc("GlobalAlloc")
	procGlobalLock       = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock     = modKernel32.NewProc("GlobalUnlock")
	procGlobalSize       = modKernel32.NewProc("GlobalSize")
	procRtlMoveMemory    = modKernel32.NewProc("RtlMoveMemory")

	procCreateFontW        = modGdi32.NewProc("CreateFontW")
	procCreateDIBSection   = modGdi32.NewProc("CreateDIBSection")
	procCreateBitmap       = modGdi32.NewProc("CreateBitmap")
	procCreateCompatibleDC = modGdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = modGdi32.NewProc("DeleteDC")
	procDeleteObject       = modGdi32.NewProc("DeleteObject")
	procGetDeviceCaps    = modGdi32.NewProc("GetDeviceCaps")
	procSetBkMode        = modGdi32.NewProc("SetBkMode")
	procCreateSolidBrush = modGdi32.NewProc("CreateSolidBrush")
	procCreatePen        = modGdi32.NewProc("CreatePen")
	procSelectObject     = modGdi32.NewProc("SelectObject")
	procGetStockObject   = modGdi32.NewProc("GetStockObject")
	procMoveToEx         = modGdi32.NewProc("MoveToEx")
	procLineTo           = modGdi32.NewProc("LineTo")
	procEllipse          = modGdi32.NewProc("Ellipse")
	procRectangle        = modGdi32.NewProc("Rectangle")
	procRoundRect        = modGdi32.NewProc("RoundRect")
	procSetTextColor     = modGdi32.NewProc("SetTextColor")
	procSetBkColor       = modGdi32.NewProc("SetBkColor")

	procInitCommonControlsEx = modComctl32.NewProc("InitCommonControlsEx")
	procDwmSetWindowAttr     = modDwmapi.NewProc("DwmSetWindowAttribute")
	procSetWindowTheme       = modUxtheme.NewProc("SetWindowTheme")
	procSetPreferredAppMode  = modUxtheme.NewProc("#135")
	procFlushMenuThemes      = modUxtheme.NewProc("#136")
	procAlphaBlend           = modMsimg32.NewProc("AlphaBlend")

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

type nmCustomDrawInfo struct {
	Hdr         nmHdr
	DwDrawStage uint32
	Hdc         uintptr
	Rc          winRect
	DwItemSpec  uintptr
	UItemState  uint32
	LItemlParam uintptr
}

type nmLvCustomDraw struct {
	Nmcd        nmCustomDrawInfo
	ClrText     uint32
	ClrTextBk   uint32
	ISubItem    int32
	DwItemType  uint32
	ClrFace     uint32
	IIconEffect int32
	IIconPhase  int32
	IPartId     int32
	IStateId    int32
	RcText      winRect
	UAlign      uint32
}

type paintStruct struct {
	Hdc        uintptr
	FErase     int32
	RcPaint    winRect
	FRestore   int32
	FIncUpdate int32
	RgbReserved [32]byte
}

type uiThemeColors struct {
	Window    uint32
	Surface   uint32
	SurfaceAlt uint32
	Selected  uint32
	Track     uint32
	Border    uint32
	FillBlue  uint32
	FillGreen uint32
	FillAmber uint32
	FillRed   uint32
	Text      uint32
	TextMuted uint32
	TextOnBar uint32
	TextGreen uint32
	TextRed   uint32
}

func rgbColor(r, g, b uint8) uint32 {
	return uint32(r) | (uint32(g) << 8) | (uint32(b) << 16)
}

type drawItemStruct struct {
	CtlType    uint32
	CtlID      uint32
	ItemID     uint32
	ItemAction uint32
	ItemState  uint32
	HwndItem   uintptr
	Hdc        uintptr
	RcItem     winRect
	ItemData   uintptr
}

type mainColumnDef struct {
	title string
	width int
	right bool
}

var mainListColumns = []mainColumnDef{
	{"File Name", 205, false},
	{"Size", 82, true},
	{"Status", 98, false},
	{"Time left", 64, true},
	{"Transfer rate", 90, true},
	{"Quality", 60, false},
}

func getThemeColors(dark bool) uiThemeColors {
	if dark {
		return uiThemeColors{
			Window:     rgbColor(37, 37, 38),
			Surface:    rgbColor(30, 30, 30),
			SurfaceAlt: rgbColor(40, 40, 43),
			Selected:   rgbColor(55, 55, 61),
			Track:      rgbColor(46, 46, 50),
			Border:     rgbColor(62, 62, 66),
			FillBlue:   rgbColor(37, 99, 235),
			FillGreen:  rgbColor(16, 155, 108),
			FillAmber:  rgbColor(217, 119, 6),
			FillRed:    rgbColor(220, 38, 38),
			Text:       rgbColor(228, 228, 231),
			TextMuted:  rgbColor(161, 161, 170),
			TextOnBar:  rgbColor(255, 255, 255),
			TextGreen:  rgbColor(74, 222, 128),
			TextRed:    rgbColor(248, 113, 113),
		}
	}
	return uiThemeColors{
		Window:     rgbColor(243, 244, 246),
		Surface:    rgbColor(255, 255, 255),
		SurfaceAlt: rgbColor(248, 249, 251),
		Selected:   rgbColor(219, 234, 254),
		Track:      rgbColor(226, 232, 240),
		Border:     rgbColor(203, 213, 225),
		FillBlue:   rgbColor(96, 165, 250),
		FillGreen:  rgbColor(110, 231, 183),
		FillAmber:  rgbColor(252, 211, 77),
		FillRed:    rgbColor(252, 165, 165),
		Text:       rgbColor(17, 24, 39),
		TextMuted:  rgbColor(100, 116, 139),
		TextOnBar:  rgbColor(17, 24, 39),
		TextGreen:  rgbColor(21, 128, 61),
		TextRed:    rgbColor(185, 28, 28),
	}
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
	id       string
	cols     [6]string
	status   string
	progress float64
}

type progressPopup struct {
	taskID         string
	hwnd           uintptr
	lblTitle       uintptr
	lblURL         uintptr
	lblStatus      uintptr
	lblSize        uintptr
	lblSpeed       uintptr
	progBar        uintptr
	lblConns       uintptr
	comboWhenDone  uintptr
	segRect        winRect
	btnPrimary     uintptr
	btnSecondary   uintptr
	btnClose       uintptr
	lastTitle      string
	lastStatus     string
	lastSize       string
	lastSpeed      string
	lastConns      string
	lastWhenDone   string
	lastPos        int
	lastMode       string // "active", "paused", "completed", "error"
	lastSegments   []SegmentProgress
	lastTotalBytes int64
	lastStatusCode string
}

type addDialogControls struct {
	hwnd        uintptr
	editURL     uintptr
	btnPreview  uintptr
	lblPreview  uintptr
	comboQual   uintptr
	chkPlay     uintptr
	chkStream   uintptr
	editStart   uintptr
	editCount   uintptr
	chkSubs     uintptr
	editSubLang uintptr
	chkChapters uintptr
	previewMu   sync.Mutex
	lastPreview *MediaPreview
	previewErr  string
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

var speedLimitPresets = []struct {
	kbps  int64
	label string
}{
	{0, "Unlimited (Full Speed)"},
	{512, "512 KB/s"},
	{1024, "1 MB/s (1024 KB/s)"},
	{2048, "2 MB/s (2048 KB/s)"},
	{5120, "5 MB/s (5120 KB/s)"},
	{10240, "10 MB/s (10240 KB/s)"},
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
	fontSmall uintptr

	mainHwnd          uintptr
	sidebarHwnd       uintptr
	searchHwnd        uintptr
	searchBoxRc       winRect
	origSearchWndProc uintptr
	listHwnd          uintptr
	headerHwnd        uintptr
	origListWndProc   uintptr
	statusHwnd        uintptr
	toolbarIcons      map[uintptr]uintptr
	toolbarIconSz     int
	searchIconBmp     uintptr
	searchIconSz      int
	hMenuTasks      uintptr
	hMenuTools      uintptr
	hMenuOpt        uintptr
	hMenuHelp       uintptr
	hMenuSpeedLimit uintptr
	hMenuTheme      uintptr
	hMenuWhenDone   uintptr
	menuBtns        []toolbarButtonDef
	toolbar         []toolbarButtonDef
	toolbarEndX     int
	rows            []listRowCache

	activeCategory  string
	searchQuery     string
	sidebarLabels   [10]string
	updatingSidebar bool

	darkMode     bool
	colors       uiThemeColors
	hbrWindow    uintptr
	hbrSurface   uintptr
	hbrSurfaceAlt uintptr
	hbrSelected  uintptr
	hbrTrack     uintptr
	hbrBorder    uintptr
	hbrFillBlue  uintptr
	hbrFillGreen uintptr
	hbrFillAmber uintptr
	hbrFillRed   uintptr

	popups   map[uintptr]*progressPopup // hwnd -> popup
	taskPop  map[string]uintptr         // taskID -> hwnd
	addDlg   *addDialogControls

	updateQueued      atomic.Bool
	updating          atomic.Bool
	ytdlpUpdating     atomic.Bool
	trayAdded         atomic.Bool
	msgTaskbarCreated uint32

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
		cfg:            cfg,
		qm:             qm,
		srv:            srv,
		dpi:            96,
		activeCategory: CategoryAll,
		popups:         make(map[uintptr]*progressPopup),
		taskPop:        make(map[string]uintptr),
	}
	appGUI = g

	qm.SetWhenDoneHandler(func(action string, lastTask *DownloadTask) {
		if action == WhenDoneShutdown && !g.quitting && g.trayAdded.Load() {
			g.showTrayBalloon("Downloads Complete", "Shutting down PC in 30 seconds...")
		}
		executeWhenDoneAction(g.cfg, g.qm, action, lastTask)
	})

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

func (g *winAppGUI) destroyThemeBrushes() {
	ptrs := []*uintptr{
		&g.hbrWindow, &g.hbrSurface, &g.hbrSurfaceAlt, &g.hbrSelected,
		&g.hbrTrack, &g.hbrBorder, &g.hbrFillBlue, &g.hbrFillGreen,
		&g.hbrFillAmber, &g.hbrFillRed, &g.searchIconBmp,
	}
	for _, p := range ptrs {
		if *p != 0 {
			procDeleteObject.Call(*p)
			*p = 0
		}
	}
	for k, bmp := range g.toolbarIcons {
		if bmp != 0 {
			procDeleteObject.Call(bmp)
		}
		delete(g.toolbarIcons, k)
	}
}

func (g *winAppGUI) rebuildThemeBrushes(dark bool) {
	g.destroyThemeBrushes()
	g.darkMode = dark
	c := getThemeColors(dark)
	g.colors = c

	mk := func(cr uint32) uintptr {
		h, _, _ := procCreateSolidBrush.Call(uintptr(cr))
		return h
	}
	g.hbrWindow = mk(c.Window)
	g.hbrSurface = mk(c.Surface)
	g.hbrSurfaceAlt = mk(c.SurfaceAlt)
	g.hbrSelected = mk(c.Selected)
	g.hbrTrack = mk(c.Track)
	g.hbrBorder = mk(c.Border)
	g.hbrFillBlue = mk(c.FillBlue)
	g.hbrFillGreen = mk(c.FillGreen)
	g.hbrFillAmber = mk(c.FillAmber)
	g.hbrFillRed = mk(c.FillRed)

	// Pre-render crisp 4x4 supersampled anti-aliased toolbar & search icons for current theme
	if g.toolbarIcons == nil {
		g.toolbarIcons = make(map[uintptr]uintptr, 8)
	}
	g.toolbarIconSz = g.scale(30)
	if g.toolbarIconSz < 24 {
		g.toolbarIconSz = 24
	}
	for _, cmdID := range []uintptr{cmdAddURL, cmdResume, cmdPause, cmdCancel, cmdDelete, cmdOpenFolder, cmdClearDone, cmdToolsMenu} {
		g.toolbarIcons[cmdID] = createAntiAliasedIconBitmap(cmdID, g.toolbarIconSz, dark)
	}
	g.searchIconSz = g.scale(15)
	if g.searchIconSz < 14 {
		g.searchIconSz = 14
	}
	g.searchIconBmp = createAntiAliasedIconBitmap(0, g.searchIconSz, dark)
}

func (g *winAppGUI) applyWindowDarkChrome(hwnd uintptr) {
	if hwnd == 0 || procDwmSetWindowAttr.Find() != nil {
		return
	}
	var val int32
	if g.darkMode {
		val = 1
	}
	// DWMWA_USE_IMMERSIVE_DARK_MODE = 20 (Win11 / Win10 20H1+), fallback 19
	r, _, _ := procDwmSetWindowAttr.Call(hwnd, 20, uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
	if r != 0 {
		procDwmSetWindowAttr.Call(hwnd, 19, uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
	}
}

func enableWindowsDarkMenus(dark bool) {
	if procSetPreferredAppMode.Find() == nil {
		// PreferredAppMode: 0=Default, 1=AllowDark, 2=ForceDark, 3=ForceLight
		mode := uintptr(3)
		if dark {
			mode = 2
		}
		procSetPreferredAppMode.Call(mode)
	}
	if procFlushMenuThemes.Find() == nil {
		procFlushMenuThemes.Call()
	}
}

func (g *winAppGUI) applyTheme() {
	dark := resolveDarkMode(g.cfg.GetTheme(), isSystemDarkMode())
	enableWindowsDarkMenus(dark)
	g.rebuildThemeBrushes(dark)

	const gclHbrBackground = ^uintptr(9) // GCLP_HBRBACKGROUND (-10)
	if procSetClassLongPtrW.Find() == nil && g.hbrWindow != 0 {
		for _, wnd := range []uintptr{g.mainHwnd} {
			if wnd != 0 {
				procSetClassLongPtrW.Call(wnd, gclHbrBackground, g.hbrWindow)
			}
		}
	}

	g.applyWindowDarkChrome(g.mainHwnd)
	themeStr := "Explorer"
	hdrTheme := "ItemsView"
	if dark {
		themeStr = "DarkMode_Explorer"
		hdrTheme = "DarkMode_ItemsView"
	}
	if procSetWindowTheme.Find() == nil {
		for _, ctrl := range []uintptr{g.listHwnd, g.sidebarHwnd, g.searchHwnd} {
			if ctrl != 0 {
				procSetWindowTheme.Call(ctrl, uintptr(unsafe.Pointer(utf16Ptr(themeStr))), 0)
			}
		}
		if g.headerHwnd != 0 {
			procSetWindowTheme.Call(g.headerHwnd, uintptr(unsafe.Pointer(utf16Ptr(hdrTheme))), 0)
		}
	}

	if g.listHwnd != 0 {
		procSendMessageW.Call(g.listHwnd, lvmSetBkColor, 0, uintptr(g.colors.Surface))
		procSendMessageW.Call(g.listHwnd, lvmSetTextBkColor, 0, uintptr(g.colors.Surface))
		procSendMessageW.Call(g.listHwnd, lvmSetTextColor, 0, uintptr(g.colors.Text))
		procInvalidateRect.Call(g.listHwnd, 0, 1)
	}
	if g.headerHwnd != 0 {
		procInvalidateRect.Call(g.headerHwnd, 0, 1)
	}
	if g.sidebarHwnd != 0 {
		procInvalidateRect.Call(g.sidebarHwnd, 0, 1)
	}
	if g.searchHwnd != 0 {
		procInvalidateRect.Call(g.searchHwnd, 0, 1)
		procUpdateWindow.Call(g.searchHwnd)
	}
	if g.statusHwnd != 0 {
		procInvalidateRect.Call(g.statusHwnd, 0, 1)
	}
	for _, b := range g.menuBtns {
		if b.hwnd != 0 {
			procInvalidateRect.Call(b.hwnd, 0, 1)
		}
	}
	for _, b := range g.toolbar {
		if b.hwnd != 0 {
			procInvalidateRect.Call(b.hwnd, 0, 1)
		}
	}
	if g.mainHwnd != 0 {
		procInvalidateRect.Call(g.mainHwnd, 0, 1)
		procUpdateWindow.Call(g.mainHwnd)
	}
	for hwnd := range g.popups {
		g.applyWindowDarkChrome(hwnd)
		procInvalidateRect.Call(hwnd, 0, 1)
	}
	if g.addDlg != nil && g.addDlg.hwnd != 0 {
		g.applyWindowDarkChrome(g.addDlg.hwnd)
		procInvalidateRect.Call(g.addDlg.hwnd, 0, 1)
	}
	g.refreshOptionMenuChecks()
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
		uintptr(int32(-g.scale(13))), 0, 0, 0,
		600, 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(fontFace)),
	)
	g.fontSmall, _, _ = procCreateFontW.Call(
		uintptr(int32(-g.scale(11))), 0, 0, 0,
		400, 0, 0, 0, 1, 0, 0, 5, 0,
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

	winW := g.scale(810)
	winH := g.scale(490)
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

	g.darkMode = resolveDarkMode(g.cfg.GetTheme(), isSystemDarkMode())
	enableWindowsDarkMenus(g.darkMode)
	g.rebuildThemeBrushes(g.darkMode)

	g.buildMainMenu()
	if tbMsg, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16Ptr("TaskbarCreated")))); tbMsg != 0 {
		g.msgTaskbarCreated = uint32(tbMsg)
	}
	syncAutoStartPathIfEnabled()
	g.refreshOptionMenuChecks()

	g.mainHwnd, _, _ = procCreateWindowExW.Call(
		wsExAppWindow,
		uintptr(unsafe.Pointer(mainCls)),
		uintptr(unsafe.Pointer(utf16Ptr("GrabAll Download Manager"))),
		wsOverlappedWindow|wsClipChildren,
		uintptr(posX), uintptr(posY), uintptr(winW), uintptr(winH),
		0,
		0,
		g.hInst,
		0,
	)

	if g.iconBig != 0 {
		procSendMessageW.Call(g.mainHwnd, wmSetIcon, iconBigVal, g.iconBig)
	}
	if g.iconSmall != 0 {
		procSendMessageW.Call(g.mainHwnd, wmSetIcon, iconSmallVal, g.iconSmall)
	}
	g.applyTheme()

	if !g.addTrayIcon() {
		go func() {
			for i := 0; i < 30; i++ {
				time.Sleep(2 * time.Second)
				if g.quitting || g.trayAdded.Load() {
					return
				}
				if g.mainHwnd != 0 {
					procPostMessageW.Call(g.mainHwnd, wmAppRetryTray, 0, 0)
				}
			}
		}()
	}
	g.syncListView()
	g.refreshStatusBar()
	if g.srv != nil && g.srv.ytdlpUpdater != nil {
		g.srv.ytdlpUpdater.AddStatusListener(func(info *YtDlpUpdateInfo) {
			if info == nil {
				return
			}
			if info.Updating && info.StatusText != "" {
				g.postStatusMessage("⚡ " + info.StatusText)
			} else {
				g.postStatusMessage("")
			}
		})
	}
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
	go func() {
		time.Sleep(9 * time.Second)
		if g.cfg.GetAutoCheckUpdates() {
			g.checkForYtDlpUpdate(false, startInTray)
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
	// Tasks Menu
	hTasks, _, _ := procCreatePopupMenu.Call()
	g.hMenuTasks = hTasks
	procAppendMenuW.Call(hTasks, mfString, cmdAddURL, uintptr(unsafe.Pointer(utf16Ptr("Add New Download...	Ctrl+N"))))
	procAppendMenuW.Call(hTasks, mfString, cmdClearDone, uintptr(unsafe.Pointer(utf16Ptr("Clear Completed Downloads"))))
	procAppendMenuW.Call(hTasks, mfString, cmdOpenDownloads, uintptr(unsafe.Pointer(utf16Ptr("Open Downloads Folder"))))
	procAppendMenuW.Call(hTasks, mfSeparator, 0, 0)
	procAppendMenuW.Call(hTasks, mfString, cmdExit, uintptr(unsafe.Pointer(utf16Ptr("Exit GrabAll"))))

	// Tools Menu
	hTools, _, _ := procCreatePopupMenu.Call()
	g.hMenuTools = hTools
	procAppendMenuW.Call(hTools, mfString, cmdCheckYtDlpUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for yt-dlp Updates..."))))
	procAppendMenuW.Call(hTools, mfString, cmdInstallYtDlp, uintptr(unsafe.Pointer(utf16Ptr("Install / Update yt-dlp + QuickJS (YouTube Engine)"))))
	procAppendMenuW.Call(hTools, mfString, cmdInstallFFmpeg, uintptr(unsafe.Pointer(utf16Ptr("Install / Update FFmpeg (HD Audio+Video Merger)"))))
	procAppendMenuW.Call(hTools, mfSeparator, 0, 0)
	procAppendMenuW.Call(hTools, mfString, cmdChangeDir, uintptr(unsafe.Pointer(utf16Ptr("Change Default Save Folder..."))))
	procAppendMenuW.Call(hTools, mfString, cmdCopyToken, uintptr(unsafe.Pointer(utf16Ptr("Copy Extension Auth Token"))))

	// Options Menu
	hOpt, _, _ := procCreatePopupMenu.Call()
	g.hMenuOpt = hOpt
	procAppendMenuW.Call(hOpt, mfString, cmdToggleTurbo, uintptr(unsafe.Pointer(utf16Ptr("⚡ Turbo Multi-Connection Speed (6x Faster — uncheck for Watch-Live 1x)"))))

	hSpeed, _, _ := procCreatePopupMenu.Call()
	g.hMenuSpeedLimit = hSpeed
	for idx, preset := range speedLimitPresets {
		procAppendMenuW.Call(hSpeed, mfString, uintptr(cmdGlobalLimit0+idx), uintptr(unsafe.Pointer(utf16Ptr(preset.label))))
	}
	procAppendMenuW.Call(hOpt, mfPopup, hSpeed, uintptr(unsafe.Pointer(utf16Ptr("Global Speed Limit"))))

	hTheme, _, _ := procCreatePopupMenu.Call()
	g.hMenuTheme = hTheme
	procAppendMenuW.Call(hTheme, mfString, cmdThemeDark, uintptr(unsafe.Pointer(utf16Ptr("Dark Mode (Default)"))))
	procAppendMenuW.Call(hTheme, mfString, cmdThemeLight, uintptr(unsafe.Pointer(utf16Ptr("Light Mode"))))
	procAppendMenuW.Call(hTheme, mfString, cmdThemeAuto, uintptr(unsafe.Pointer(utf16Ptr("Auto (Follow Windows)"))))
	procAppendMenuW.Call(hOpt, mfPopup, hTheme, uintptr(unsafe.Pointer(utf16Ptr("Theme"))))

	hWD, _, _ := procCreatePopupMenu.Call()
	g.hMenuWhenDone = hWD
	for idx, preset := range whenDonePresets {
		procAppendMenuW.Call(hWD, mfString, uintptr(cmdWhenDoneNone+idx), uintptr(unsafe.Pointer(utf16Ptr(preset.Label))))
	}
	procAppendMenuW.Call(hOpt, mfPopup, hWD, uintptr(unsafe.Pointer(utf16Ptr("When All Downloads Finish"))))

	procAppendMenuW.Call(hOpt, mfSeparator, 0, 0)
	procAppendMenuW.Call(hOpt, mfString, cmdToggleStartup, uintptr(unsafe.Pointer(utf16Ptr("Start GrabAll Automatically with Windows"))))
	procAppendMenuW.Call(hOpt, mfString, cmdTogglePopup, uintptr(unsafe.Pointer(utf16Ptr("Show Download Progress Popup on New Downloads"))))
	procAppendMenuW.Call(hOpt, mfString, cmdToggleAutoUpdate, uintptr(unsafe.Pointer(utf16Ptr("Automatically Check for GrabAll & yt-dlp Updates on Startup"))))
	procAppendMenuW.Call(hOpt, mfSeparator, 0, 0)
	procAppendMenuW.Call(hOpt, mfString, cmdConc1, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 1"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc2, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 2"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc3, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 3"))))
	procAppendMenuW.Call(hOpt, mfString, cmdConc4, uintptr(unsafe.Pointer(utf16Ptr("Max Concurrent Downloads: 4"))))

	// Help Menu
	hHelp, _, _ := procCreatePopupMenu.Call()
	g.hMenuHelp = hHelp
	procAppendMenuW.Call(hHelp, mfString, cmdOpenWeb, uintptr(unsafe.Pointer(utf16Ptr("Open Web UI in Browser"))))
	procAppendMenuW.Call(hHelp, mfString, cmdCheckUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for Updates..."))))
	procAppendMenuW.Call(hHelp, mfString, cmdAbout, uintptr(unsafe.Pointer(utf16Ptr("About GrabAll"))))

	g.refreshOptionMenuChecks()
	return 0
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

	if g.hMenuSpeedLimit != 0 {
		curLimit := g.cfg.GetSpeedLimitKBps()
		for idx, preset := range speedLimitPresets {
			flag := uintptr(mfByCommand | mfUnchecked)
			if curLimit == preset.kbps {
				flag = uintptr(mfByCommand | mfChecked)
			}
			procCheckMenuItem.Call(g.hMenuSpeedLimit, uintptr(cmdGlobalLimit0+idx), flag)
		}
	}

	if g.hMenuTheme != 0 {
		curTheme := g.cfg.GetTheme()
		themeCmds := []struct {
			id  uintptr
			val string
		}{
			{cmdThemeAuto, ThemeAuto},
			{cmdThemeLight, ThemeLight},
			{cmdThemeDark, ThemeDark},
		}
		for _, tc := range themeCmds {
			flag := uintptr(mfByCommand | mfUnchecked)
			if curTheme == tc.val {
				flag = uintptr(mfByCommand | mfChecked)
			}
			procCheckMenuItem.Call(g.hMenuTheme, tc.id, flag)
		}
	}

	if g.hMenuWhenDone != 0 {
		curWD := g.qm.GetWhenDoneAction()
		for idx, preset := range whenDonePresets {
			flag := uintptr(mfByCommand | mfUnchecked)
			if curWD == preset.Value {
				flag = uintptr(mfByCommand | mfChecked)
			}
			procCheckMenuItem.Call(g.hMenuWhenDone, uintptr(cmdWhenDoneNone+idx), flag)
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
	// Custom dark top menu strip (eliminates white Win32 system menu bar in dark mode)
	g.menuBtns = []toolbarButtonDef{
		{id: cmdMenuTasks, label: "Tasks", width: 52},
		{id: cmdMenuTools, label: "Tools", width: 50},
		{id: cmdMenuOptions, label: "Options", width: 62},
		{id: cmdMenuHelp, label: "Help", width: 48},
	}
	mx := g.scale(4)
	menuH := g.scale(24)
	for i := range g.menuBtns {
		mw := g.scale(g.menuBtns[i].width)
		g.menuBtns[i].hwnd = g.createChildControl(
			"BUTTON", g.menuBtns[i].label,
			wsChild|wsVisible|bsOwnerDraw, 0,
			mx, 0, mw, menuH,
			hwnd, g.menuBtns[i].id, false,
		)
		mx += mw + g.scale(2)
	}

	// Owner-draw flat anti-aliased vector icon toolbar (classic compact download manager style)
	g.toolbar = []toolbarButtonDef{
		{id: cmdAddURL, label: "Add URL", width: 64},
		{id: cmdResume, label: "Resume", width: 60},
		{id: cmdPause, label: "Pause", width: 58},
		{id: cmdCancel, label: "Stop", width: 56},
		{id: cmdDelete, label: "Delete", width: 58},
		{id: cmdOpenFolder, label: "Folder", width: 58},
		{id: cmdClearDone, label: "Clear", width: 56},
		{id: cmdToolsMenu, label: "Engine", width: 60},
	}

	x := g.scale(6)
	y := g.scale(28)
	btnH := g.scale(58)
	gap := g.scale(2)

	for i := range g.toolbar {
		bw := g.scale(g.toolbar[i].width)
		g.toolbar[i].hwnd = g.createChildControl(
			"BUTTON", g.toolbar[i].label,
			wsChild|wsVisible|wsTabStop|bsOwnerDraw, 0,
			x, y, bw, btnH,
			hwnd, g.toolbar[i].id, false,
		)
		x += bw + gap
	}
	g.toolbarEndX = x

	// Borderless Live Search Edit Box seated inside a flat theme-colored search container
	g.searchHwnd = g.createChildControl(
		"EDIT", "",
		wsChild|wsVisible|wsTabStop|esAutoHScroll, 0,
		x+g.scale(26), g.scale(48), g.scale(160), g.scale(18),
		hwnd, idcSearchEdit, false,
	)
	if g.searchHwnd != 0 && procSetWindowLongPtrW.Find() == nil {
		const gwlpWndProc = ^uintptr(3) // -4
		g.origSearchWndProc, _, _ = procSetWindowLongPtrW.Call(g.searchHwnd, gwlpWndProc, syscall.NewCallback(searchEditSubProc))
	}

	// Left Categories Sidebar (owner-draw dark LISTBOX with category icons)
	g.sidebarHwnd = g.createChildControl(
		"LISTBOX", "",
		wsChild|wsVisible|wsTabStop|wsVScroll|lbsNotify|lbsOwnerDrawFixed|lbsHasStrings|lbsNoIntegralHeight, wsExClientEdge,
		g.scale(6), g.scale(92), g.scale(142), g.scale(336),
		hwnd, idcSidebar, false,
	)
	if g.sidebarHwnd != 0 {
		procSendMessageW.Call(g.sidebarHwnd, lbSetItemHeight, 0, uintptr(g.scale(23)))
	}
	for idx, cat := range uiCategories {
		label := fmt.Sprintf("%s (0)", cat.Label)
		g.sidebarLabels[idx] = label
		procSendMessageW.Call(g.sidebarHwnd, lbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(g.sidebarHwnd, lbSetCurSel, 0, 0)

	// Main Download ListView (SysListView32)
	lvStyle := uint32(wsChild | wsVisible | wsTabStop | lvsReport | lvsSingleSel | lvsShowSelAlways)
	g.listHwnd = g.createChildControl(
		"SysListView32", "",
		lvStyle, wsExClientEdge,
		g.scale(154), g.scale(92), g.scale(634), g.scale(336),
		hwnd, idcListView, false,
	)

	exStyle := uintptr(lvsExFullRowSelect | lvsExDoubleBuffer)
	procSendMessageW.Call(g.listHwnd, lvmSetExtendedListViewStyle, exStyle, exStyle)

	for idx, c := range mainListColumns {
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

	if g.listHwnd != 0 {
		g.headerHwnd, _, _ = procSendMessageW.Call(g.listHwnd, lvmGetHeader, 0, 0)
		if procSetWindowLongPtrW.Find() == nil {
			const gwlpWndProc = ^uintptr(3) // -4
			g.origListWndProc, _, _ = procSetWindowLongPtrW.Call(g.listHwnd, gwlpWndProc, syscall.NewCallback(listViewSubProc))
		}
	}

	// Bottom Status Bar Text
	g.statusHwnd = g.createChildControl(
		"STATIC", "Ready",
		wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0,
		g.scale(8), g.scale(428), g.scale(760), g.scale(18),
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

	pad := g.scale(6)
	topY := g.scale(92)
	statusH := g.scale(18)
	listH := clientH - topY - statusH - g.scale(10)
	if listH < g.scale(80) {
		listH = g.scale(80)
	}
	fullW := clientW - pad*2
	if fullW < g.scale(100) {
		fullW = g.scale(100)
	}

	if g.searchHwnd != 0 {
		avail := clientW - pad - g.toolbarEndX - g.scale(10)
		boxW := g.scale(220)
		if avail < boxW {
			boxW = avail
		}
		if boxW < g.scale(110) {
			boxW = g.scale(110)
		}
		boxX := clientW - pad - boxW
		if boxX < g.toolbarEndX+g.scale(6) {
			boxX = g.toolbarEndX + g.scale(6)
			if clientW-pad-boxX > g.scale(70) {
				boxW = clientW - pad - boxX
			}
		}
		boxY := g.scale(43)
		boxH := g.scale(28)
		g.searchBoxRc = winRect{
			Left:   int32(boxX),
			Top:    int32(boxY),
			Right:  int32(boxX + boxW),
			Bottom: int32(boxY + boxH),
		}
		iconPad := g.scale(26)
		editW := boxW - iconPad - g.scale(6)
		if editW < g.scale(40) {
			editW = g.scale(40)
		}
		editH := g.scale(18)
		editY := boxY + (boxH-editH)/2
		procMoveWindow.Call(g.searchHwnd, uintptr(boxX+iconPad), uintptr(editY), uintptr(editW), uintptr(editH), 1)
		procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(&g.searchBoxRc)), 1)
	}

	sidebarW := g.scale(142)
	colGap := g.scale(6)
	if g.sidebarHwnd != 0 {
		procMoveWindow.Call(g.sidebarHwnd, uintptr(pad), uintptr(topY), uintptr(sidebarW), uintptr(listH), 1)
	}

	listX := pad + sidebarW + colGap
	listW := clientW - pad - listX
	if listW < g.scale(120) {
		listW = g.scale(120)
	}
	if g.listHwnd != 0 {
		procMoveWindow.Call(g.listHwnd, uintptr(listX), uintptr(topY), uintptr(listW), uintptr(listH), 1)
		fixedCols := g.scale(82 + 98 + 64 + 90 + 60 + 20)
		titleW := listW - fixedCols
		if titleW < g.scale(140) {
			titleW = g.scale(140)
		}
		procSendMessageW.Call(g.listHwnd, lvmSetColumnWidth, 0, uintptr(titleW))
	}
	if g.statusHwnd != 0 {
		procMoveWindow.Call(g.statusHwnd, uintptr(pad+2), uintptr(topY+listH+g.scale(4)), uintptr(fullW-4), uintptr(statusH), 1)
	}
}

func (g *winAppGUI) addTrayIcon() bool {
	if g.trayAdded.Load() {
		return true
	}
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = g.mainHwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmAppTray
	nid.HIcon = g.iconSmall
	copyUTF16Fixed(nid.SzTip[:], "GrabAll Download Manager — Ready")
	ret, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if ret != 0 {
		g.trayAdded.Store(true)
		return true
	}
	return false
}

func (g *winAppGUI) removeTrayIcon() {
	g.trayAdded.Store(false)
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

// syncListView performs a flicker-free cell diff update on SysListView32,
// updates category sidebar counts, and updates open Progress Popups.
func (g *winAppGUI) syncListView() {
	if g.listHwnd == 0 {
		return
	}
	tasks := g.qm.GetAll()
	prevSelectedID := g.getSelectedTaskID()

	taskMap := make(map[string]*DownloadTask, len(tasks))
	for _, t := range tasks {
		taskMap[t.ID] = t
	}

	if g.sidebarHwnd != 0 {
		counts := countTasksByCategory(tasks)
		activeIdx := 0
		var changedIdxs []int
		newLabels := make([]string, len(uiCategories))
		for idx, cat := range uiCategories {
			if cat.Key == g.activeCategory {
				activeIdx = idx
			}
			cnt := counts[cat.Key]
			if cnt > 0 || cat.Key == CategoryAll {
				newLabels[idx] = fmt.Sprintf("%s (%d)", cat.Label, cnt)
			} else {
				newLabels[idx] = cat.Label
			}
			if idx < len(g.sidebarLabels) && g.sidebarLabels[idx] != newLabels[idx] {
				changedIdxs = append(changedIdxs, idx)
			}
		}
		if len(changedIdxs) > 0 {
			g.updatingSidebar = true
			for _, idx := range changedIdxs {
				procSendMessageW.Call(g.sidebarHwnd, lbDeleteString, uintptr(idx), 0)
				procSendMessageW.Call(g.sidebarHwnd, lbInsertString, uintptr(idx), uintptr(unsafe.Pointer(utf16Ptr(newLabels[idx]))))
				g.sidebarLabels[idx] = newLabels[idx]
			}
			procSendMessageW.Call(g.sidebarHwnd, lbSetCurSel, uintptr(activeIdx), 0)
			g.updatingSidebar = false
		}
	}

	visible := filterTasksForUI(tasks, g.activeCategory, g.searchQuery)

	// Adjust row count
	for len(g.rows) > len(visible) {
		last := len(g.rows) - 1
		procSendMessageW.Call(g.listHwnd, lvmDeleteItem, uintptr(last), 0)
		g.rows = g.rows[:last]
	}

	newSelectedRow := -1

	for i, t := range visible {
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
			eta,
			speed,
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
			g.rows = append(g.rows, listRowCache{
				id:       t.ID,
				cols:     cols,
				status:   t.Status,
				progress: t.Progress,
			})
			procSendMessageW.Call(g.listHwnd, lvmRedrawItems, uintptr(i), uintptr(i))
		} else {
			needsRedraw := g.rows[i].id != t.ID || g.rows[i].status != t.Status || g.rows[i].progress != t.Progress
			g.rows[i].id = t.ID
			g.rows[i].status = t.Status
			g.rows[i].progress = t.Progress
			for sub := 0; sub < 6; sub++ {
				if g.rows[i].cols[sub] != cols[sub] {
					subItem := lvItemW{
						ISubItem: int32(sub),
						PszText:  utf16Ptr(cols[sub]),
					}
					procSendMessageW.Call(g.listHwnd, lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&subItem)))
					g.rows[i].cols[sub] = cols[sub]
					needsRedraw = true
				}
			}
			if needsRedraw {
				procSendMessageW.Call(g.listHwnd, lvmRedrawItems, uintptr(i), uintptr(i))
			}
		}

		if prevSelectedID != "" && t.ID == prevSelectedID {
			newSelectedRow = i
		}
	}

	if newSelectedRow >= 0 && g.getSelectedTaskID() != prevSelectedID {
		g.selectRowIndex(newSelectedRow)
	}

	// Update any open Mini Progress Windows (across all tasks, even if filtered out)
	for _, pop := range g.popups {
		if t, ok := taskMap[pop.taskID]; ok {
			g.updateProgressPopup(pop, t)
		}
	}

	g.refreshStatusBar()
}

func (g *winAppGUI) handleListViewCustomDraw(lParam uintptr) uintptr {
	if lParam == 0 || g.listHwnd == 0 {
		return cdrfDoDefault
	}
	var lvcd nmLvCustomDraw
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&lvcd)), lParam, unsafe.Sizeof(lvcd))

	switch lvcd.Nmcd.DwDrawStage {
	case cddsPrePaint:
		return cdrfNotifyItemDraw
	case cddsItemPrePaint:
		return cdrfNotifySubItemDraw
	case cddsItemPrePaint | cddsSubItem:
		rowIdx := int(lvcd.Nmcd.DwItemSpec)
		subIdx := int(lvcd.ISubItem)
		if rowIdx < 0 || rowIdx >= len(g.rows) {
			return cdrfDoDefault
		}
		row := g.rows[rowIdx]
		state, _, _ := procSendMessageW.Call(g.listHwnd, lvmGetItemState, uintptr(rowIdx), lvisSelected)
		selected := (state & lvisSelected) != 0

		if subIdx != 2 {
			rc := winRect{Top: int32(subIdx), Left: lvirBounds}
			ok, _, _ := procSendMessageW.Call(g.listHwnd, lvmGetSubItemRect, uintptr(rowIdx), uintptr(unsafe.Pointer(&rc)))
			if ok == 0 || rc.Right <= rc.Left || rc.Bottom <= rc.Top {
				rc = lvcd.Nmcd.Rc
			}
			if rc.Right <= rc.Left || rc.Bottom <= rc.Top {
				return cdrfDoDefault
			}
			if subIdx == 0 && len(mainListColumns) > 1 {
				// lvirBounds for subitem 0 spans the whole row; clip to column 0 width using subitem 1's left edge.
				rc1 := winRect{Top: 1, Left: lvirBounds}
				if ok1, _, _ := procSendMessageW.Call(g.listHwnd, lvmGetSubItemRect, uintptr(rowIdx), uintptr(unsafe.Pointer(&rc1))); ok1 != 0 && rc1.Left > rc.Left {
					rc.Right = rc1.Left
				}
			}

			rowBg := g.hbrSurface
			if selected {
				rowBg = g.hbrSelected
			} else if rowIdx%2 == 1 {
				rowBg = g.hbrSurfaceAlt
			}
			if rowBg != 0 {
				procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&rc)), rowBg)
			}

			// Subtle horizontal row hairline & vertical column divider
			if g.hbrBorder != 0 {
				bLine := winRect{Left: rc.Left, Top: rc.Bottom - 1, Right: rc.Right, Bottom: rc.Bottom}
				procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&bLine)), g.hbrBorder)
			}

			textColor := g.colors.Text
			if subIdx == 3 || subIdx == 4 {
				switch row.status {
				case StatusCompleted:
					textColor = g.colors.TextGreen
				case StatusError, StatusCanceled:
					textColor = g.colors.TextRed
				}
			} else if subIdx == 5 {
				textColor = g.colors.TextMuted
			}

			textRc := winRect{
				Left:   rc.Left + int32(g.scale(6)),
				Top:    rc.Top,
				Right:  rc.Right - int32(g.scale(6)),
				Bottom: rc.Bottom - 1,
			}
			if subIdx == 0 {
				// Subtle colored status indicator dot next to filename
				dotColor := g.colors.FillBlue
				switch row.status {
				case StatusCompleted:
					dotColor = g.colors.FillGreen
				case StatusPaused:
					dotColor = g.colors.FillAmber
				case StatusError, StatusCanceled:
					dotColor = g.colors.FillRed
				}
				dotSz := int32(g.scale(7))
				dotX := rc.Left + int32(g.scale(6))
				dotY := rc.Top + (rc.Bottom-rc.Top-dotSz)/2
				g.fillEllipseColor(lvcd.Nmcd.Hdc, dotX, dotY, dotX+dotSz, dotY+dotSz, dotColor)
				textRc.Left = dotX + dotSz + int32(g.scale(6))
			}

			if subIdx < len(row.cols) && row.cols[subIdx] != "" && textRc.Right > textRc.Left {
				if u16, err := syscall.UTF16FromString(row.cols[subIdx]); err == nil && len(u16) > 1 {
					oldFont, _, _ := procSelectObject.Call(lvcd.Nmcd.Hdc, g.fontUI)
					procSetBkMode.Call(lvcd.Nmcd.Hdc, transparentBkMode)
					procSetTextColor.Call(lvcd.Nmcd.Hdc, uintptr(textColor))
					flags := uintptr(dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis)
					if subIdx < len(mainListColumns) && mainListColumns[subIdx].right {
						flags = uintptr(dtRight | dtVCenter | dtSingleLine | dtEndEllipsis)
					}
					procDrawTextW.Call(
						lvcd.Nmcd.Hdc,
						uintptr(unsafe.Pointer(&u16[0])),
						uintptr(len(u16)-1),
						uintptr(unsafe.Pointer(&textRc)),
						flags,
					)
					if oldFont != 0 {
						procSelectObject.Call(lvcd.Nmcd.Hdc, oldFont)
					}
				}
			}
			return cdrfSkipDefault
		}

		rc := winRect{Top: 2, Left: lvirBounds}
		ok, _, _ := procSendMessageW.Call(g.listHwnd, lvmGetSubItemRect, uintptr(rowIdx), uintptr(unsafe.Pointer(&rc)))
		if ok == 0 || rc.Right <= rc.Left || rc.Bottom <= rc.Top {
			rc = lvcd.Nmcd.Rc
		}
		if rc.Right <= rc.Left || rc.Bottom <= rc.Top {
			return cdrfDoDefault
		}

		rowBg := g.hbrSurface
		if selected {
			rowBg = g.hbrSelected
		} else if rowIdx%2 == 1 {
			rowBg = g.hbrSurfaceAlt
		}
		if rowBg != 0 {
			procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&rc)), rowBg)
		}

		padX := int32(g.scale(6))
		padY := int32(g.scale(3))
		if rc.Right-rc.Left <= padX*2+12 {
			padX = 2
		}
		if rc.Bottom-rc.Top <= padY*2+6 {
			padY = 1
		}
		barRc := winRect{
			Left:   rc.Left + padX,
			Top:    rc.Top + padY,
			Right:  rc.Right - padX,
			Bottom: rc.Bottom - padY,
		}
		if barRc.Right <= barRc.Left || barRc.Bottom <= barRc.Top {
			return cdrfSkipDefault
		}

		if g.hbrTrack != 0 {
			procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&barRc)), g.hbrTrack)
		}

		pct := row.progress
		if row.status == StatusCompleted {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}

		fillBrush := g.hbrFillBlue
		switch row.status {
		case StatusCompleted:
			fillBrush = g.hbrFillGreen
		case StatusPaused:
			fillBrush = g.hbrFillAmber
		case StatusError, StatusCanceled:
			fillBrush = g.hbrFillRed
		}
		if pct > 0 && fillBrush != 0 {
			fillW := int32(float64(barRc.Right-barRc.Left) * pct / 100.0)
			if fillW < 2 && pct > 0 {
				fillW = 2
			}
			if fillW > barRc.Right-barRc.Left {
				fillW = barRc.Right - barRc.Left
			}
			fillRc := winRect{
				Left:   barRc.Left,
				Top:    barRc.Top,
				Right:  barRc.Left + fillW,
				Bottom: barRc.Bottom,
			}
			procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&fillRc)), fillBrush)
		}

		if g.hbrBorder != 0 {
			procFrameRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&barRc)), g.hbrBorder)
			bLine := winRect{Left: rc.Left, Top: rc.Bottom - 1, Right: rc.Right, Bottom: rc.Bottom}
			procFillRect.Call(lvcd.Nmcd.Hdc, uintptr(unsafe.Pointer(&bLine)), g.hbrBorder)
		}

		if label := row.cols[2]; label != "" {
			if u16, err := syscall.UTF16FromString(label); err == nil && len(u16) > 1 {
				oldFont, _, _ := procSelectObject.Call(lvcd.Nmcd.Hdc, g.fontUI)
				procSetBkMode.Call(lvcd.Nmcd.Hdc, transparentBkMode)
				procSetTextColor.Call(lvcd.Nmcd.Hdc, uintptr(g.colors.TextOnBar))
				textRc := barRc
				textRc.Left += 4
				textRc.Right -= 4
				procDrawTextW.Call(
					lvcd.Nmcd.Hdc,
					uintptr(unsafe.Pointer(&u16[0])),
					uintptr(len(u16)-1),
					uintptr(unsafe.Pointer(&textRc)),
					dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis,
				)
				if oldFont != 0 {
					procSelectObject.Call(lvcd.Nmcd.Hdc, oldFont)
				}
			}
		}

		return cdrfSkipDefault
	}

	return cdrfDoDefault
}

func (g *winAppGUI) fillEllipseColor(hdc uintptr, left, top, right, bottom int32, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(color))
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procEllipse.Call(hdc, uintptr(left), uintptr(top), uintptr(right), uintptr(bottom))
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(br)
	procDeleteObject.Call(pen)
}

func (g *winAppGUI) drawStrokeLine(hdc uintptr, x1, y1, x2, y2 int32) {
	procMoveToEx.Call(hdc, uintptr(x1), uintptr(y1), 0)
	procLineTo.Call(hdc, uintptr(x2), uintptr(y2))
}

func (g *winAppGUI) drawVectorToolbarIcon(hdc uintptr, cmdID uintptr, cx, cy, r int32) {
	var accent uint32
	switch cmdID {
	case cmdAddURL:
		accent = rgbColor(74, 222, 128) // Emerald green (+)
	case cmdResume:
		accent = rgbColor(96, 165, 250) // Sky blue (down arrow in circle)
	case cmdPause:
		accent = rgbColor(251, 191, 36) // Amber (pause bars in circle)
	case cmdCancel:
		accent = rgbColor(248, 113, 113) // Coral red (stop square in circle)
	case cmdDelete:
		accent = rgbColor(251, 113, 133) // Rose (trash can)
	case cmdOpenFolder:
		accent = rgbColor(245, 158, 11) // Warm gold (folder)
	case cmdClearDone:
		accent = rgbColor(232, 121, 249) // Orchid (check box)
	default: // cmdToolsMenu / Engine
		accent = rgbColor(167, 139, 250) // Soft violet (gear)
	}
	if !g.darkMode {
		switch cmdID {
		case cmdAddURL:
			accent = rgbColor(22, 163, 74)
		case cmdResume:
			accent = rgbColor(37, 99, 235)
		case cmdPause:
			accent = rgbColor(217, 119, 6)
		case cmdCancel:
			accent = rgbColor(220, 38, 38)
		case cmdDelete:
			accent = rgbColor(225, 29, 72)
		case cmdOpenFolder:
			accent = rgbColor(202, 138, 4)
		case cmdClearDone:
			accent = rgbColor(162, 28, 175)
		default:
			accent = rgbColor(109, 40, 217)
		}
	}

	strokeW := int32(g.scale(2))
	if strokeW < 1 {
		strokeW = 1
	}
	pen, _, _ := procCreatePen.Call(psSolid, uintptr(strokeW), uintptr(accent))
	nullBr, _, _ := procGetStockObject.Call(nullBrush)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	oldBr, _, _ := procSelectObject.Call(hdc, nullBr)

	switch cmdID {
	case cmdAddURL:
		// Crisp bold plus (+)
		g.drawStrokeLine(hdc, cx-r+1, cy, cx+r-1, cy)
		g.drawStrokeLine(hdc, cx, cy-r+1, cx, cy+r-1)
	case cmdResume:
		// Circle with downward arrow
		procEllipse.Call(hdc, uintptr(cx-r), uintptr(cy-r), uintptr(cx+r), uintptr(cy+r))
		g.drawStrokeLine(hdc, cx, cy-r/2, cx, cy+r/2)
		g.drawStrokeLine(hdc, cx-r/3, cy+1, cx, cy+r/2)
		g.drawStrokeLine(hdc, cx+r/3, cy+1, cx, cy+r/2)
	case cmdPause:
		// Circle with two vertical pause bars
		procEllipse.Call(hdc, uintptr(cx-r), uintptr(cy-r), uintptr(cx+r), uintptr(cy+r))
		g.drawStrokeLine(hdc, cx-r/3, cy-r/2+1, cx-r/3, cy+r/2-1)
		g.drawStrokeLine(hdc, cx+r/3, cy-r/2+1, cx+r/3, cy+r/2-1)
	case cmdCancel:
		// Circle with inner stop square
		procEllipse.Call(hdc, uintptr(cx-r), uintptr(cy-r), uintptr(cx+r), uintptr(cy+r))
		sq := r / 2
		if sq < 3 {
			sq = 3
		}
		procRectangle.Call(hdc, uintptr(cx-sq), uintptr(cy-sq), uintptr(cx+sq), uintptr(cy+sq))
	case cmdDelete:
		// Minimal trash bin outline
		g.drawStrokeLine(hdc, cx-r+2, cy-r/2, cx+r-2, cy-r/2)
		g.drawStrokeLine(hdc, cx-r/3, cy-r+2, cx+r/3, cy-r+2)
		procRectangle.Call(hdc, uintptr(cx-r+4), uintptr(cy-r/2), uintptr(cx+r-4), uintptr(cy+r-1))
		g.drawStrokeLine(hdc, cx-2, cy-r/2+3, cx-2, cy+r-4)
		g.drawStrokeLine(hdc, cx+2, cy-r/2+3, cx+2, cy+r-4)
	case cmdOpenFolder:
		// Folder outline with top tab
		procRectangle.Call(hdc, uintptr(cx-r+1), uintptr(cy-r/2+1), uintptr(cx+r-1), uintptr(cy+r-2))
		g.drawStrokeLine(hdc, cx-r+1, cy-r/2+1, cx-r+3, cy-r+3)
		g.drawStrokeLine(hdc, cx-r+3, cy-r+3, cx-1, cy-r+3)
		g.drawStrokeLine(hdc, cx-1, cy-r+3, cx+1, cy-r/2+1)
	case cmdClearDone:
		// Rounded box with checkmark
		procRoundRect.Call(hdc, uintptr(cx-r+2), uintptr(cy-r+2), uintptr(cx+r-2), uintptr(cy+r-2), 4, 4)
		g.drawStrokeLine(hdc, cx-r/2+1, cy, cx-1, cy+r/3+1)
		g.drawStrokeLine(hdc, cx-1, cy+r/3+1, cx+r/2, cy-r/3)
	default: // Engine / gear cog
		outer := r - 2
		inner := r / 2
		if inner < 3 {
			inner = 3
		}
		procEllipse.Call(hdc, uintptr(cx-outer), uintptr(cy-outer), uintptr(cx+outer), uintptr(cy+outer))
		procEllipse.Call(hdc, uintptr(cx-inner), uintptr(cy-inner), uintptr(cx+inner), uintptr(cy+inner))
		g.drawStrokeLine(hdc, cx, cy-r, cx, cy-outer+1)
		g.drawStrokeLine(hdc, cx, cy+outer-1, cx, cy+r)
		g.drawStrokeLine(hdc, cx-r, cy, cx-outer+1, cy)
		g.drawStrokeLine(hdc, cx+outer-1, cy, cx+r, cy)
	}

	procSelectObject.Call(hdc, oldPen)
	procSelectObject.Call(hdc, oldBr)
	procDeleteObject.Call(pen)
}

func (g *winAppGUI) drawCategoryMiniIcon(hdc uintptr, catKey string, x, y, sz int32) {
	var col uint32
	switch catKey {
	case CategoryAll:
		col = rgbColor(245, 197, 66)
	case CategoryActive:
		col = rgbColor(56, 189, 248)
	case CategoryVideo:
		col = rgbColor(96, 165, 250)
	case CategoryMusic:
		col = rgbColor(192, 132, 252)
	case CategoryArchives:
		col = rgbColor(251, 146, 60)
	case CategoryPrograms:
		col = rgbColor(74, 222, 128)
	case CategoryDocuments:
		col = rgbColor(148, 163, 184)
	case CategoryImages:
		col = rgbColor(244, 114, 182)
	case CategoryCompleted:
		col = rgbColor(52, 211, 153)
	default: // Failed
		col = rgbColor(248, 113, 113)
	}
	br, _, _ := procCreateSolidBrush.Call(uintptr(col))
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(col))
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(hdc, uintptr(x), uintptr(y+1), uintptr(x+sz), uintptr(y+sz-1), 3, 3)
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(br)
	procDeleteObject.Call(pen)
}

func (g *winAppGUI) drawOwnerItem(lParam uintptr) uintptr {
	if lParam == 0 {
		return 0
	}
	var dis drawItemStruct
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&dis)), lParam, unsafe.Sizeof(dis))
	rc := dis.RcItem
	hdc := dis.Hdc

	// 1. Custom Dark Categories Sidebar item
	if dis.CtlID == idcSidebar {
		idx := int(int32(dis.ItemID))
		if idx < 0 || idx >= len(uiCategories) {
			return 1
		}
		selected := (dis.ItemState & odsSelected) != 0
		bg := g.hbrSurface
		if selected {
			bg = g.hbrSelected
		}
		if bg != 0 {
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
		}
		if selected && g.hbrFillBlue != 0 {
			bar := winRect{Left: rc.Left, Top: rc.Top + 2, Right: rc.Left + int32(g.scale(3)), Bottom: rc.Bottom - 2}
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&bar)), g.hbrFillBlue)
		}
		icoSz := int32(g.scale(10))
		icoX := rc.Left + int32(g.scale(8))
		icoY := rc.Top + (rc.Bottom-rc.Top-icoSz)/2
		g.drawCategoryMiniIcon(hdc, uiCategories[idx].Key, icoX, icoY, icoSz)

		label := g.sidebarLabels[idx]
		if label == "" {
			label = uiCategories[idx].Label
		}
		textRc := winRect{
			Left:   icoX + icoSz + int32(g.scale(7)),
			Top:    rc.Top,
			Right:  rc.Right - int32(g.scale(4)),
			Bottom: rc.Bottom,
		}
		if u16, err := syscall.UTF16FromString(label); err == nil && len(u16) > 1 {
			font := g.fontUI
			if selected {
				font = g.fontBold
			}
			oldFont, _, _ := procSelectObject.Call(hdc, font)
			procSetBkMode.Call(hdc, transparentBkMode)
			procSetTextColor.Call(hdc, uintptr(g.colors.Text))
			procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&textRc)), dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
			if oldFont != 0 {
				procSelectObject.Call(hdc, oldFont)
			}
		}
		return 1
	}

	// 2. Dark Top Menu Strip Buttons (Tasks, Tools, Options, Help)
	cmdID := uintptr(dis.CtlID)
	if cmdID == cmdMenuTasks || cmdID == cmdMenuTools || cmdID == cmdMenuOptions || cmdID == cmdMenuHelp {
		pressed := (dis.ItemState & (odsSelected | odsHotLight)) != 0
		bg := g.hbrWindow
		if pressed {
			bg = g.hbrSelected
		}
		if bg != 0 {
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
		}
		label := getWindowString(dis.HwndItem)
		if u16, err := syscall.UTF16FromString(label); err == nil && len(u16) > 1 {
			oldFont, _, _ := procSelectObject.Call(hdc, g.fontUI)
			procSetBkMode.Call(hdc, transparentBkMode)
			procSetTextColor.Call(hdc, uintptr(g.colors.Text))
			procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&rc)), dtCenter|dtVCenter|dtSingleLine)
			if oldFont != 0 {
				procSelectObject.Call(hdc, oldFont)
			}
		}
		return 1
	}

	// 3. Flat Dark Vector Icon Toolbar Buttons
	if g.hbrWindow != 0 {
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), g.hbrWindow)
	}
	pressed := (dis.ItemState & odsSelected) != 0
	focused := (dis.ItemState & (odsFocus | odsHotLight)) != 0
	if pressed || focused {
		inRc := winRect{Left: rc.Left + 1, Top: rc.Top + 1, Right: rc.Right - 1, Bottom: rc.Bottom - 1}
		if g.hbrSelected != 0 {
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&inRc)), g.hbrSelected)
		}
		if g.hbrBorder != 0 {
			procFrameRect.Call(hdc, uintptr(unsafe.Pointer(&inRc)), g.hbrBorder)
		}
	}

	icoSz := int32(g.toolbarIconSz)
	if bmp := g.toolbarIcons[cmdID]; bmp != 0 && icoSz > 0 {
		dstX := (rc.Left+rc.Right-icoSz)/2
		dstY := rc.Top + int32(g.scale(5))
		if pressed {
			dstY++
		}
		blitAlphaBitmap(hdc, bmp, dstX, dstY, icoSz)
	} else {
		cx := (rc.Left + rc.Right) / 2
		cy := rc.Top + int32(g.scale(20))
		if pressed {
			cy++
		}
		r := int32(g.scale(12))
		g.drawVectorToolbarIcon(hdc, cmdID, cx, cy, r)
	}

	label := getWindowString(dis.HwndItem)
	if u16, err := syscall.UTF16FromString(label); err == nil && len(u16) > 1 {
		txtRc := winRect{
			Left:   rc.Left + 1,
			Top:    rc.Bottom - int32(g.scale(19)),
			Right:  rc.Right - 1,
			Bottom: rc.Bottom - int32(g.scale(3)),
		}
		if pressed {
			txtRc.Top++
			txtRc.Bottom++
		}
		oldFont, _, _ := procSelectObject.Call(hdc, g.fontSmall)
		procSetBkMode.Call(hdc, transparentBkMode)
		procSetTextColor.Call(hdc, uintptr(g.colors.Text))
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&txtRc)), dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis)
		if oldFont != 0 {
			procSelectObject.Call(hdc, oldFont)
		}
	}
	return 1
}

func searchEditSubProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	g := appGUI
	if g == nil || g.origSearchWndProc == 0 {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
	ret, _, _ := procCallWindowProcW.Call(g.origSearchWndProc, hwnd, uintptr(msg), wParam, lParam)
	switch msg {
	case wmPaint:
		if textLen, _, _ := procGetWindowTextLengthW.Call(hwnd); textLen == 0 {
			hdc, _, _ := procGetDC.Call(hwnd)
			if hdc != 0 {
				var rc winRect
				procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
				if u16, err := syscall.UTF16FromString("Search downloads..."); err == nil && len(u16) > 1 {
					oldFont, _, _ := procSelectObject.Call(hdc, g.fontUI)
					procSetBkMode.Call(hdc, transparentBkMode)
					procSetTextColor.Call(hdc, uintptr(g.colors.TextMuted))
					procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&rc)), dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
					if oldFont != 0 {
						procSelectObject.Call(hdc, oldFont)
					}
				}
				procReleaseDC.Call(hwnd, hdc)
			}
		}
	case wmSetFocus, wmKillFocus:
		procInvalidateRect.Call(hwnd, 0, 1)
	}
	return ret
}

func listViewSubProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	g := appGUI
	if g == nil || g.origListWndProc == 0 {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
	if msg == wmNotify && lParam != 0 && g.headerHwnd != 0 {
		var hdr nmHdr
		procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&hdr)), lParam, unsafe.Sizeof(hdr))
		if hdr.HWndFrom == g.headerHwnd && hdr.Code == nmCustomDraw {
			var cd nmCustomDrawInfo
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&cd)), lParam, unsafe.Sizeof(cd))
			switch cd.DwDrawStage {
			case cddsPrePaint:
				var hrc winRect
				procGetClientRect.Call(g.headerHwnd, uintptr(unsafe.Pointer(&hrc)))
				if g.hbrSurfaceAlt != 0 {
					procFillRect.Call(cd.Hdc, uintptr(unsafe.Pointer(&hrc)), g.hbrSurfaceAlt)
				}
				if g.hbrBorder != 0 {
					bLine := winRect{Left: hrc.Left, Top: hrc.Bottom - 1, Right: hrc.Right, Bottom: hrc.Bottom}
					procFillRect.Call(cd.Hdc, uintptr(unsafe.Pointer(&bLine)), g.hbrBorder)
				}
				return cdrfNotifyItemDraw
			case cddsItemPrePaint:
				colIdx := int(cd.DwItemSpec)
				rc := cd.Rc
				if g.hbrSurfaceAlt != 0 {
					procFillRect.Call(cd.Hdc, uintptr(unsafe.Pointer(&rc)), g.hbrSurfaceAlt)
				}
				if g.hbrBorder != 0 {
					rLine := winRect{Left: rc.Right - 1, Top: rc.Top + 3, Right: rc.Right, Bottom: rc.Bottom - 3}
					procFillRect.Call(cd.Hdc, uintptr(unsafe.Pointer(&rLine)), g.hbrBorder)
					bLine := winRect{Left: rc.Left, Top: rc.Bottom - 1, Right: rc.Right, Bottom: rc.Bottom}
					procFillRect.Call(cd.Hdc, uintptr(unsafe.Pointer(&bLine)), g.hbrBorder)
				}
				if colIdx >= 0 && colIdx < len(mainListColumns) {
					col := mainListColumns[colIdx]
					if u16, err := syscall.UTF16FromString(col.title); err == nil && len(u16) > 1 {
						oldFont, _, _ := procSelectObject.Call(cd.Hdc, g.fontUI)
						procSetBkMode.Call(cd.Hdc, transparentBkMode)
						procSetTextColor.Call(cd.Hdc, uintptr(g.colors.Text))
						txtRc := winRect{
							Left:   rc.Left + int32(g.scale(6)),
							Top:    rc.Top,
							Right:  rc.Right - int32(g.scale(6)),
							Bottom: rc.Bottom - 1,
						}
						flags := uintptr(dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis)
						if col.right {
							flags = uintptr(dtRight | dtVCenter | dtSingleLine | dtEndEllipsis)
						}
						procDrawTextW.Call(cd.Hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&txtRc)), flags)
						if oldFont != 0 {
							procSelectObject.Call(cd.Hdc, oldFont)
						}
					}
				}
				return cdrfSkipDefault
			}
		}
	}
	ret, _, _ := procCallWindowProcW.Call(g.origListWndProc, hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func (g *winAppGUI) showTopPopupMenu(btnIdx int, hPopup uintptr) {
	if hPopup == 0 || btnIdx < 0 || btnIdx >= len(g.menuBtns) {
		return
	}
	var rc winRect
	procGetWindowRect.Call(g.menuBtns[btnIdx].hwnd, uintptr(unsafe.Pointer(&rc)))
	procSetForegroundWindow.Call(g.mainHwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hPopup, tpmReturnCmd, uintptr(rc.Left), uintptr(rc.Bottom), 0, g.mainHwnd, 0)
	if cmd != 0 {
		g.handleCommand(cmd)
	}
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
	if g.srv != nil && g.srv.ytdlpUpdater != nil {
		ytInfo := g.srv.ytdlpUpdater.Status(g.cfg)
		if ytInfo.Updating && ytInfo.StatusText != "" {
			ytState = ytInfo.StatusText
		} else if !ytInfo.Installed {
			ytState = "Missing (click Engine Setup)"
		} else if !isFastUnpackedYtDlp(ytPath) {
			ytState = "Slow 1-file exe (click Engine Setup -> Upgrade)"
		} else if ytInfo.UpdateAvailable && ytInfo.LatestVersion != "" {
			if ytInfo.CurrentVersion != "" {
				ytState = fmt.Sprintf("v%s (Update Found: %s!)", ytInfo.CurrentVersion, ytInfo.LatestVersion)
			} else {
				ytState = fmt.Sprintf("Update Found: %s!", ytInfo.LatestVersion)
			}
		} else if ytInfo.CurrentVersion != "" {
			ytState = "v" + ytInfo.CurrentVersion
		}
	} else if ytErr != nil {
		ytState = "Missing (click Engine Setup)"
	} else if !isFastUnpackedYtDlp(ytPath) {
		ytState = "Slow 1-file exe (click Engine Setup -> Upgrade)"
	}
	ffState := "Ready"
	if resolveFFmpegBinary(g.cfg) == "" {
		ffState = "Optional (click Engine Setup for 1080p+/MP3)"
	}

	cookieSeg := ""
	if signedIn, cnt, _ := g.qm.YouTubeCookieStatus(); signedIn {
		cookieSeg = fmt.Sprintf("  |  YT Cookies: Signed In (%d)", cnt)
	}

	modeLabel := "Turbo (6x)"
	if g.cfg.GetSpeedMode() == SpeedModeRegular {
		modeLabel = "Regular (Watch Live)"
	}
	limitSeg := ""
	if lim := g.cfg.GetSpeedLimitKBps(); lim > 0 {
		limitSeg = fmt.Sprintf("   •   Limit: %s/s", formatBytesHuman(lim*1024))
	}
	wdSeg := ""
	if wd := g.qm.GetWhenDoneAction(); wd != WhenDoneNone {
		wdSeg = "   •   When done: " + whenDoneLabel(wd)
	}

	status := fmt.Sprintf(
		"Active: %d  |  Queued: %d  |  Completed: %d   •   Speed: %s%s%s   •   Folder: %s   •   yt-dlp: %s  |  FFmpeg: %s%s",
		active, queued, done,
		modeLabel, limitSeg, wdSeg,
		filepath.Clean(g.cfg.GetDownloadDir()),
		ytState, ffState, cookieSeg,
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
	h := g.scale(326)
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
	g.applyWindowDarkChrome(hwnd)

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

	pop.progBar = g.createChildControl("msctls_progress32", "", wsChild|wsVisible|pbsSmooth, 0, pad, g.scale(130), contentW, g.scale(20), hwnd, 0, false)
	procSendMessageW.Call(pop.progBar, pbmSetRange32, 0, 1000)

	pop.lblConns = g.createChildControl("STATIC", "Connections: preparing...", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(156), contentW, g.scale(18), hwnd, 0, false)
	pop.segRect = winRect{
		Left:   int32(pad),
		Top:    int32(g.scale(178)),
		Right:  int32(pad + contentW),
		Bottom: int32(g.scale(200)),
	}

	g.createChildControl("STATIC", "When all finish:", wsChild|wsVisible, 0, pad, g.scale(212), g.scale(112), g.scale(20), hwnd, 0, false)
	pop.comboWhenDone = g.createChildControl("COMBOBOX", "", wsChild|wsVisible|wsTabStop|cbsDropDownList|wsVScroll, 0, pad+g.scale(116), g.scale(209), contentW-g.scale(116), g.scale(180), hwnd, idcProgComboWhenDone, false)
	curWD := g.qm.GetWhenDoneAction()
	for idx, preset := range whenDonePresets {
		procSendMessageW.Call(pop.comboWhenDone, cbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(preset.Label))))
		if preset.Value == curWD {
			procSendMessageW.Call(pop.comboWhenDone, cbSetCurSel, uintptr(idx), 0)
		}
	}
	pop.lastWhenDone = curWD

	btnW := g.scale(106)
	btnH := g.scale(30)
	btnY := g.scale(246)
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

func segmentsEqual(a, b []SegmentProgress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Start != b[i].Start || a[i].Current != b[i].Current || a[i].End != b[i].End || a[i].Active != b[i].Active {
			return false
		}
	}
	return true
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

	connLine := "Connections: single stream"
	if n := len(t.Segments); n > 0 {
		activeSegs := 0
		for _, seg := range t.Segments {
			if seg.Active {
				activeSegs++
			}
		}
		if activeSegs > 0 {
			connLine = fmt.Sprintf("Connections: %d active / %d segments", activeSegs, n)
		} else {
			connLine = fmt.Sprintf("Connections: %d segments", n)
		}
	} else if t.Status == StatusCompleted {
		connLine = "Connections: finished"
	} else if t.Status == StatusPaused {
		connLine = "Connections: paused"
	} else if t.Status == StatusError || t.Status == StatusCanceled {
		connLine = "Connections: stopped"
	} else if t.Status == StatusQueued {
		connLine = "Connections: waiting in queue"
	}
	if pop.lastConns != connLine {
		setWindowString(pop.lblConns, connLine)
		pop.lastConns = connLine
	}

	curWD := g.qm.GetWhenDoneAction()
	if pop.comboWhenDone != 0 && pop.lastWhenDone != curWD {
		for idx, preset := range whenDonePresets {
			if preset.Value == curWD {
				procSendMessageW.Call(pop.comboWhenDone, cbSetCurSel, uintptr(idx), 0)
				break
			}
		}
		pop.lastWhenDone = curWD
	}

	if pop.lastStatusCode != t.Status || pop.lastTotalBytes != t.TotalBytes || !segmentsEqual(pop.lastSegments, t.Segments) {
		pop.lastStatusCode = t.Status
		pop.lastTotalBytes = t.TotalBytes
		if len(t.Segments) > 0 {
			pop.lastSegments = append([]SegmentProgress(nil), t.Segments...)
		} else {
			pop.lastSegments = nil
		}
		procInvalidateRect.Call(pop.hwnd, uintptr(unsafe.Pointer(&pop.segRect)), 0)
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

	w := g.scale(500)
	h := g.scale(372)
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
	g.applyWindowDarkChrome(hwnd)

	pad := g.scale(14)
	contentW := w - pad*2 - g.scale(14)
	prevBtnW := g.scale(98)

	g.createChildControl("STATIC", "Video / Playlist / File URL:", wsChild|wsVisible, 0, pad, g.scale(12), contentW, g.scale(18), hwnd, 0, true)
	editURL := g.createChildControl("EDIT", "", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad, g.scale(33), contentW-prevBtnW-g.scale(6), g.scale(26), hwnd, idcAddEditURL, false)
	btnPreview := g.createChildControl("BUTTON", "Preview Size", wsChild|wsVisible|wsTabStop|bsPushButton, 0, pad+contentW-prevBtnW, g.scale(32), prevBtnW, g.scale(28), hwnd, idcAddBtnPreview, false)
	lblPreview := g.createChildControl("STATIC", "Tip: Click Preview Size to check available resolutions, sizes, subtitles & chapters.", wsChild|wsVisible|ssLeftNoWrap|ssEndEllipsis, 0, pad, g.scale(63), contentW, g.scale(18), hwnd, 0, false)

	g.createChildControl("STATIC", "Format / Quality:", wsChild|wsVisible, 0, pad, g.scale(90), g.scale(120), g.scale(20), hwnd, 0, false)
	combo := g.createChildControl("COMBOBOX", "", wsChild|wsVisible|wsTabStop|cbsDropDownList|wsVScroll, 0, pad+g.scale(122), g.scale(86), contentW-g.scale(122), g.scale(220), hwnd, idcAddComboQual, false)
	for _, qp := range qualityPresets {
		procSendMessageW.Call(combo, cbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(qp.label))))
	}
	procSendMessageW.Call(combo, cbSetCurSel, 0, 0)

	chkPlay := g.createChildControl("BUTTON", "Download YouTube playlist (saves into subfolder)", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad, g.scale(122), contentW, g.scale(22), hwnd, idcAddChkPlay, false)

	g.createChildControl("STATIC", "Playlist Start #:", wsChild|wsVisible, 0, pad, g.scale(152), g.scale(105), g.scale(20), hwnd, 0, false)
	editStart := g.createChildControl("EDIT", "1", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad+g.scale(108), g.scale(149), g.scale(56), g.scale(24), hwnd, idcAddEditStart, false)

	g.createChildControl("STATIC", "Max Videos (0 = All):", wsChild|wsVisible, 0, pad+g.scale(182), g.scale(152), g.scale(145), g.scale(20), hwnd, 0, false)
	editCount := g.createChildControl("EDIT", "0", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad+g.scale(330), g.scale(149), g.scale(66), g.scale(24), hwnd, idcAddEditCount, false)

	chkStream := g.createChildControl("BUTTON", "Watch while downloading (Regular 1-stream live mode)", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad, g.scale(182), contentW, g.scale(22), hwnd, idcAddChkStream, false)
	if g.cfg.GetSpeedMode() == SpeedModeRegular {
		procSendMessageW.Call(chkStream, bmSetCheck, bstChecked, 0)
	}

	chkSubs := g.createChildControl("BUTTON", "Embed Subtitles", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad, g.scale(212), g.scale(130), g.scale(22), hwnd, idcAddChkSubs, false)
	g.createChildControl("STATIC", "Lang:", wsChild|wsVisible, 0, pad+g.scale(136), g.scale(215), g.scale(38), g.scale(18), hwnd, 0, false)
	editSubLang := g.createChildControl("EDIT", "en", wsChild|wsVisible|wsTabStop|esAutoHScroll, wsExClientEdge, pad+g.scale(176), g.scale(211), g.scale(64), g.scale(24), hwnd, idcAddEditSubLang, false)
	chkChapters := g.createChildControl("BUTTON", "Embed Chapters", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, pad+g.scale(258), g.scale(212), g.scale(150), g.scale(22), hwnd, idcAddChkChapters, false)

	btnW := g.scale(130)
	btnH := g.scale(32)
	btnY := g.scale(254)
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
		hwnd:        hwnd,
		editURL:     editURL,
		btnPreview:  btnPreview,
		lblPreview:  lblPreview,
		comboQual:   combo,
		chkPlay:     chkPlay,
		chkStream:   chkStream,
		editStart:   editStart,
		editCount:   editCount,
		chkSubs:     chkSubs,
		editSubLang: editSubLang,
		chkChapters: chkChapters,
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
	procAppendMenuW.Call(hMenu, mfString, cmdCheckYtDlpUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for yt-dlp Updates..."))))
	procAppendMenuW.Call(hMenu, mfString, cmdCheckUpdate, uintptr(unsafe.Pointer(utf16Ptr("Check for GrabAll Updates..."))))
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
	ytLabel := "Install / Update yt-dlp + QuickJS (Fast-Start Installed)"
	checkLabel := "Check for yt-dlp Updates..."
	if g.srv != nil && g.srv.ytdlpUpdater != nil {
		ytInfo := g.srv.ytdlpUpdater.Status(g.cfg)
		if ytInfo.Updating {
			ytLabel = "⏳ " + ytInfo.StatusText
		} else if !ytInfo.Installed {
			ytLabel = "Install yt-dlp + QuickJS (Required for YouTube/TikTok/FB)"
		} else if !isFastUnpackedYtDlp(ytPath) {
			ytLabel = "⚡ Upgrade yt-dlp to Fast-Start Mode (yt-dlp_win.zip — fixes 1-2m delay)"
		} else if ytInfo.UpdateAvailable && ytInfo.LatestVersion != "" {
			ytLabel = fmt.Sprintf("✨ Update yt-dlp Now (%s -> %s)", ytInfo.CurrentVersion, ytInfo.LatestVersion)
			checkLabel = fmt.Sprintf("Check yt-dlp Updates (Update Found: %s)", ytInfo.LatestVersion)
		} else if ytInfo.CurrentVersion != "" {
			ytLabel = fmt.Sprintf("Reinstall / Update yt-dlp (v%s Installed)", ytInfo.CurrentVersion)
		}
	} else if ytErr != nil {
		ytLabel = "Install yt-dlp + QuickJS (Required for YouTube/TikTok/FB)"
	} else if !isFastUnpackedYtDlp(ytPath) {
		ytLabel = "⚡ Upgrade yt-dlp to Fast-Start Mode (yt-dlp_win.zip — fixes 1-2m delay)"
	}
	ffLabel := "Update FFmpeg (Installed)"
	if resolveFFmpegBinary(g.cfg) == "" {
		ffLabel = "Install FFmpeg (Needed for 1080p+ HD & MP3 Audio)"
	}

	procAppendMenuW.Call(hMenu, mfString, cmdCheckYtDlpUpdate, uintptr(unsafe.Pointer(utf16Ptr(checkLabel))))
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
		if hTaskLim, _, _ := procCreatePopupMenu.Call(); hTaskLim != 0 {
			for idx, preset := range speedLimitPresets {
				flag := uintptr(mfString | mfUnchecked)
				if snap.SpeedLimitKBps == preset.kbps {
					flag = uintptr(mfString | mfChecked)
				}
				procAppendMenuW.Call(hTaskLim, flag, uintptr(cmdTaskLimit0+idx), uintptr(unsafe.Pointer(utf16Ptr(preset.label))))
			}
			procAppendMenuW.Call(hMenu, mfPopup, hTaskLim, uintptr(unsafe.Pointer(utf16Ptr("Task Speed Limit"))))
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

	case cmdMenuTasks:
		g.showTopPopupMenu(0, g.hMenuTasks)

	case cmdMenuTools:
		g.showTopPopupMenu(1, g.hMenuTools)

	case cmdMenuOptions:
		g.refreshOptionMenuChecks()
		g.showTopPopupMenu(2, g.hMenuOpt)

	case cmdMenuHelp:
		g.showTopPopupMenu(3, g.hMenuHelp)

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
		go g.runYtDlpUpdateInstall(true)

	case cmdCheckYtDlpUpdate:
		go g.checkForYtDlpUpdate(true, false)

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
		} else if next {
			g.postStatusMessage("Auto-start with Windows enabled (starts in system tray)")
		} else {
			g.postStatusMessage("Auto-start with Windows disabled")
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

	case cmdGlobalLimit0, cmdGlobalLimit512, cmdGlobalLimit1024, cmdGlobalLimit2048, cmdGlobalLimit5120, cmdGlobalLimit10240:
		idx := int(cmdID - cmdGlobalLimit0)
		if idx >= 0 && idx < len(speedLimitPresets) {
			g.qm.SetGlobalSpeedLimitKBps(speedLimitPresets[idx].kbps)
			g.refreshOptionMenuChecks()
			g.refreshStatusBar()
		}

	case cmdTaskLimit0, cmdTaskLimit512, cmdTaskLimit1024, cmdTaskLimit2048, cmdTaskLimit5120, cmdTaskLimit10240:
		idx := int(cmdID - cmdTaskLimit0)
		if selID != "" && idx >= 0 && idx < len(speedLimitPresets) {
			g.qm.SetTaskSpeedLimitKBps(selID, speedLimitPresets[idx].kbps)
		}

	case cmdThemeAuto, cmdThemeLight, cmdThemeDark:
		theme := ThemeAuto
		if cmdID == cmdThemeLight {
			theme = ThemeLight
		} else if cmdID == cmdThemeDark {
			theme = ThemeDark
		}
		g.cfg.SetTheme(theme)
		g.applyTheme()

	case cmdWhenDoneNone, cmdWhenDoneOpenFile, cmdWhenDoneOpenFolder, cmdWhenDoneSleep, cmdWhenDoneShutdown:
		idx := int(cmdID - cmdWhenDoneNone)
		if idx >= 0 && idx < len(whenDonePresets) {
			g.qm.SetWhenDoneAction(whenDonePresets[idx].Value)
			g.refreshOptionMenuChecks()
			g.refreshStatusBar()
		}

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
		g.destroyThemeBrushes()
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

	if g.msgTaskbarCreated != 0 && msg == g.msgTaskbarCreated && !g.quitting {
		g.trayAdded.Store(false)
		g.addTrayIcon()
		return 0
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
			mmi.PtMinTrackSize.Y = int32(g.scale(360))
			procRtlMoveMemory.Call(lParam, uintptr(unsafe.Pointer(&mmi)), unsafe.Sizeof(mmi))
		}
		return 0

	case wmDrawItem:
		return g.drawOwnerItem(lParam)

	case wmEraseBkgnd:
		if wParam != 0 && g.hbrWindow != 0 {
			var rc winRect
			procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
			procFillRect.Call(wParam, uintptr(unsafe.Pointer(&rc)), g.hbrWindow)
			if g.hbrBorder != 0 {
				// Crisp hairline separators below top menu strip and icon toolbar
				menuSep := winRect{Left: rc.Left, Top: int32(g.scale(25)), Right: rc.Right, Bottom: int32(g.scale(26))}
				procFillRect.Call(wParam, uintptr(unsafe.Pointer(&menuSep)), g.hbrBorder)
				toolSep := winRect{Left: rc.Left, Top: int32(g.scale(88)), Right: rc.Right, Bottom: int32(g.scale(89))}
				procFillRect.Call(wParam, uintptr(unsafe.Pointer(&toolSep)), g.hbrBorder)
			}
			// Flat theme-aware Search container box + anti-aliased magnifying glass icon
			if g.searchBoxRc.Right > g.searchBoxRc.Left && g.searchBoxRc.Bottom > g.searchBoxRc.Top {
				if g.hbrSurface != 0 {
					procFillRect.Call(wParam, uintptr(unsafe.Pointer(&g.searchBoxRc)), g.hbrSurface)
				}
				if g.hbrBorder != 0 {
					procFrameRect.Call(wParam, uintptr(unsafe.Pointer(&g.searchBoxRc)), g.hbrBorder)
				}
				if g.searchIconBmp != 0 && g.searchIconSz > 0 {
					icoSz := int32(g.searchIconSz)
					icoX := g.searchBoxRc.Left + int32(g.scale(7))
					icoY := g.searchBoxRc.Top + (g.searchBoxRc.Bottom-g.searchBoxRc.Top-icoSz)/2
					blitAlphaBitmap(wParam, g.searchIconBmp, icoX, icoY, icoSz)
				}
			}
			return 1
		}

	case wmCtlColorEdit, wmCtlColorListBox:
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Surface))
		if g.hbrSurface != 0 {
			return g.hbrSurface
		}

	case wmCtlColorStatic, wmCtlColorBtn:
		procSetBkMode.Call(wParam, transparentBkMode)
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Window))
		if g.hbrWindow != 0 {
			return g.hbrWindow
		}
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmCommand:
		cmdID := wParam & 0xFFFF
		notif := (wParam >> 16) & 0xFFFF
		if cmdID == idcSearchEdit {
			if notif == enChange {
				g.searchQuery = strings.TrimSpace(getWindowString(g.searchHwnd))
				if g.searchQuery == "" {
					procInvalidateRect.Call(g.searchHwnd, 0, 1)
				}
				g.syncListView()
			}
			return 0
		}
		if cmdID == idcSidebar {
			if notif == lbnSelChange && !g.updatingSidebar {
				idx, _, _ := procSendMessageW.Call(g.sidebarHwnd, lbGetCurSel, 0, 0)
				intIdx := int(int32(idx))
				if intIdx >= 0 && intIdx < len(uiCategories) {
					g.activeCategory = uiCategories[intIdx].Key
					g.syncListView()
				}
			}
			return 0
		}
		g.handleCommand(cmdID)
		return 0

	case wmNotify:
		if lParam != 0 {
			var hdr nmHdr
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&hdr)), lParam, unsafe.Sizeof(hdr))
			if hdr.IdFrom == idcListView {
				switch hdr.Code {
				case nmCustomDraw:
					return g.handleListViewCustomDraw(lParam)
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

	case wmAppRetryTray:
		if !g.trayAdded.Load() && !g.quitting {
			g.addTrayIcon()
		}
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
	case wmEraseBkgnd:
		if wParam != 0 && g.hbrWindow != 0 {
			var rc winRect
			procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
			procFillRect.Call(wParam, uintptr(unsafe.Pointer(&rc)), g.hbrWindow)
			return 1
		}

	case wmCtlColorEdit, wmCtlColorListBox:
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Surface))
		if g.hbrSurface != 0 {
			return g.hbrSurface
		}

	case wmCtlColorStatic, wmCtlColorBtn:
		procSetBkMode.Call(wParam, transparentBkMode)
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Window))
		if g.hbrWindow != 0 {
			return g.hbrWindow
		}
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmPaint:
		pop, ok := g.popups[hwnd]
		if !ok {
			break
		}
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 {
			rc := pop.segRect
			if rc.Right > rc.Left && rc.Bottom > rc.Top {
				if g.hbrTrack != 0 {
					procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), g.hbrTrack)
				}
				barW := rc.Right - rc.Left
				if len(pop.lastSegments) > 0 && pop.lastTotalBytes > 0 {
					totalF := float64(pop.lastTotalBytes)
					for _, seg := range pop.lastSegments {
						if seg.End < seg.Start || seg.Current <= seg.Start {
							continue
						}
						cur := seg.Current
						if cur > seg.End+1 {
							cur = seg.End + 1
						}
						x1 := rc.Left + int32(float64(seg.Start)/totalF*float64(barW))
						x2 := rc.Left + int32(float64(cur)/totalF*float64(barW))
						if x2 <= x1 {
							x2 = x1 + 2
						}
						if x1 < rc.Left {
							x1 = rc.Left
						}
						if x2 > rc.Right {
							x2 = rc.Right
						}
						brush := g.hbrFillGreen
						if seg.Active {
							brush = g.hbrFillBlue
						}
						if brush != 0 {
							segRc := winRect{Left: x1, Top: rc.Top + 1, Right: x2, Bottom: rc.Bottom - 1}
							procFillRect.Call(hdc, uintptr(unsafe.Pointer(&segRc)), brush)
						}
					}
				} else if pop.lastPos > 0 {
					fillW := int32(float64(barW) * float64(pop.lastPos) / 1000.0)
					if fillW > barW {
						fillW = barW
					}
					brush := g.hbrFillBlue
					switch pop.lastStatusCode {
					case StatusCompleted:
						brush = g.hbrFillGreen
					case StatusPaused:
						brush = g.hbrFillAmber
					case StatusError, StatusCanceled:
						brush = g.hbrFillRed
					}
					if fillW > 0 && brush != 0 {
						segRc := winRect{Left: rc.Left, Top: rc.Top + 1, Right: rc.Left + fillW, Bottom: rc.Bottom - 1}
						procFillRect.Call(hdc, uintptr(unsafe.Pointer(&segRc)), brush)
					}
				}
				if g.hbrBorder != 0 {
					procFrameRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), g.hbrBorder)
				}
			}
			procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
			return 0
		}

	case wmCommand:
		pop, ok := g.popups[hwnd]
		if !ok {
			return 0
		}
		snap := g.findSnapshot(pop.taskID)
		cmdID := wParam & 0xFFFF

		switch cmdID {
		case idcProgComboWhenDone:
			if ((wParam >> 16) & 0xFFFF) == cbnSelChange {
				selIdx, _, _ := procSendMessageW.Call(pop.comboWhenDone, cbGetCurSel, 0, 0)
				idx := int(int32(selIdx))
				if idx >= 0 && idx < len(whenDonePresets) {
					wd := g.qm.SetWhenDoneAction(whenDonePresets[idx].Value)
					pop.lastWhenDone = wd
					g.refreshOptionMenuChecks()
					g.refreshStatusBar()
				}
			}

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
	case wmEraseBkgnd:
		if wParam != 0 && g.hbrWindow != 0 {
			var rc winRect
			procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
			procFillRect.Call(wParam, uintptr(unsafe.Pointer(&rc)), g.hbrWindow)
			return 1
		}

	case wmCtlColorEdit, wmCtlColorListBox:
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Surface))
		if g.hbrSurface != 0 {
			return g.hbrSurface
		}

	case wmCtlColorStatic, wmCtlColorBtn:
		procSetBkMode.Call(wParam, transparentBkMode)
		procSetTextColor.Call(wParam, uintptr(g.colors.Text))
		procSetBkColor.Call(wParam, uintptr(g.colors.Window))
		if g.hbrWindow != 0 {
			return g.hbrWindow
		}
		br, _, _ := procGetSysColorBrush.Call(colorBtnFace)
		return br

	case wmAppAddPreviewReady:
		dlg := g.addDlg
		if dlg == nil || dlg.hwnd != hwnd {
			return 0
		}
		dlg.previewMu.Lock()
		prev := dlg.lastPreview
		pErr := dlg.previewErr
		dlg.previewMu.Unlock()

		procEnableWindow.Call(dlg.btnPreview, 1)
		setWindowString(dlg.btnPreview, "Preview Size")
		if pErr != "" {
			setWindowString(dlg.lblPreview, "Preview error: "+pErr)
			return 0
		}
		if prev == nil {
			return 0
		}

		sizeByQual := make(map[string]PreviewOption, len(prev.Options))
		for _, opt := range prev.Options {
			sizeByQual[opt.Quality] = opt
		}

		curSel, _, _ := procSendMessageW.Call(dlg.comboQual, cbGetCurSel, 0, 0)
		procSendMessageW.Call(dlg.comboQual, cbResetContent, 0, 0)
		for _, qp := range qualityPresets {
			label := qp.label
			lookupKey := qp.value
			if strings.HasPrefix(lookupKey, "audio-") {
				lookupKey = "audio-best"
			}
			if opt, ok := sizeByQual[lookupKey]; ok && opt.SizeBytes > 0 {
				prefix := ""
				if opt.Approx {
					prefix = "~"
				}
				label = fmt.Sprintf("%s  —  %s%s", qp.label, prefix, formatBytesHuman(opt.SizeBytes))
			}
			procSendMessageW.Call(dlg.comboQual, cbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
		}
		if int(curSel) >= 0 && int(curSel) < len(qualityPresets) {
			procSendMessageW.Call(dlg.comboQual, cbSetCurSel, curSel, 0)
		} else {
			procSendMessageW.Call(dlg.comboQual, cbSetCurSel, 0, 0)
		}

		metaBits := []string{}
		if prev.Title != "" {
			metaBits = append(metaBits, prev.Title)
		}
		if prev.DurationText != "" {
			metaBits = append(metaBits, prev.DurationText)
		}
		if len(prev.Chapters) > 0 {
			metaBits = append(metaBits, fmt.Sprintf("%d chapters", len(prev.Chapters)))
		}
		if len(prev.SubtitleLangs)+len(prev.AutoSubtitleLangs) > 0 {
			metaBits = append(metaBits, "subtitles available")
		}
		setWindowString(dlg.lblPreview, strings.Join(metaBits, "  •  "))
		return 0

	case wmCommand:
		cmdID := wParam & 0xFFFF
		switch cmdID {
		case idcAddBtnPreview:
			dlg := g.addDlg
			if dlg == nil {
				return 0
			}
			rawURL := strings.TrimSpace(getWindowString(dlg.editURL))
			if rawURL == "" {
				procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr("Please paste a valid HTTP/HTTPS URL first."))), uintptr(unsafe.Pointer(utf16Ptr("GrabAll"))), mbOK|mbIconWarning)
				return 0
			}
			procEnableWindow.Call(dlg.btnPreview, 0)
			setWindowString(dlg.btnPreview, "Checking...")
			setWindowString(dlg.lblPreview, "Fetching video resolutions, file sizes, subtitles & chapters...")
			targetHwnd := hwnd
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
				defer cancel()
				prev, err := g.qm.PreviewMedia(ctx, AddRequest{URL: rawURL})
				dlg.previewMu.Lock()
				dlg.lastPreview = prev
				if err != nil {
					dlg.previewErr = err.Error()
				} else {
					dlg.previewErr = ""
				}
				dlg.previewMu.Unlock()
				procPostMessageW.Call(targetHwnd, wmAppAddPreviewReady, 0, 0)
			}()
			return 0

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
			chkSubs, _, _ := procSendMessageW.Call(g.addDlg.chkSubs, bmGetCheck, 0, 0)
			embedSubs := chkSubs == bstChecked
			subLangs := normalizeSubLangs(getWindowString(g.addDlg.editSubLang))
			chkChaps, _, _ := procSendMessageW.Call(g.addDlg.chkChapters, bmGetCheck, 0, 0)
			embedChapters := chkChaps == bstChecked
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
				EmbedSubs:     embedSubs,
				SubLangs:      subLangs,
				EmbedChapters: embedChapters,
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
	g.destroyThemeBrushes()
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

func (g *winAppGUI) checkForYtDlpUpdate(manual bool, startInTray bool) {
	if g.srv == nil || g.srv.ytdlpUpdater == nil {
		return
	}
	if !g.ytdlpUpdating.CompareAndSwap(false, true) {
		if manual {
			procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr("A yt-dlp check or update is already in progress."))), uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update"))), mbOK|mbIconInfo)
		}
		return
	}
	defer g.ytdlpUpdating.Store(false)

	if manual {
		g.postStatusMessage("Checking for yt-dlp updates...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	info, err := g.srv.ytdlpUpdater.CheckForUpdate(ctx, g.cfg, manual)
	cancel()

	if manual {
		g.postStatusMessage("")
	}
	g.refreshStatusBar()

	if err != nil {
		if manual {
			procMessageBoxW.Call(
				g.mainHwnd,
				uintptr(unsafe.Pointer(utf16Ptr("Could not check for yt-dlp updates:\n\n"+err.Error()))),
				uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update Check"))),
				mbOK|mbIconWarning,
			)
		}
		return
	}

	if !info.UpdateAvailable {
		if manual {
			verLabel := info.CurrentVersion
			if verLabel == "" {
				verLabel = info.LatestVersion
			}
			procMessageBoxW.Call(
				g.mainHwnd,
				uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("yt-dlp is up to date! (v%s)", verLabel)))),
				uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update Check"))),
				mbOK|mbIconInfo,
			)
		}
		return
	}

	if !manual {
		cur := info.CurrentVersion
		if cur == "" {
			cur = "not installed"
		}
		g.showTrayBalloon(
			fmt.Sprintf("yt-dlp Update Found (%s)", info.LatestVersion),
			fmt.Sprintf("Current: %s -> Latest: %s. Click Engine -> Update yt-dlp to install.", cur, info.LatestVersion),
		)
		return
	}

	curLabel := "Not Installed"
	if info.CurrentVersion != "" {
		curLabel = "v" + info.CurrentVersion
	}
	prompt := fmt.Sprintf(
		"A new version of yt-dlp is available!\n\nCurrent Version: %s\nLatest Version: v%s\n\nDownload and install the update now?",
		curLabel, info.LatestVersion,
	)
	res, _, _ := procMessageBoxW.Call(
		g.mainHwnd,
		uintptr(unsafe.Pointer(utf16Ptr(prompt))),
		uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update Found"))),
		mbYesNo|mbIconQuestion,
	)
	if res == idYes {
		g.runYtDlpUpdateInstallLocked(true)
	}
}

func (g *winAppGUI) runYtDlpUpdateInstall(interactive bool) {
	if !g.ytdlpUpdating.CompareAndSwap(false, true) {
		if interactive {
			procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr("A yt-dlp update is already in progress."))), uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update"))), mbOK|mbIconInfo)
		}
		return
	}
	defer g.ytdlpUpdating.Store(false)
	g.runYtDlpUpdateInstallLocked(interactive)
}

func (g *winAppGUI) runYtDlpUpdateInstallLocked(interactive bool) {
	if g.srv == nil || g.srv.ytdlpUpdater == nil {
		return
	}
	force := false
	if g.qm != nil && g.qm.countDownloading() > 0 {
		if !interactive {
			return
		}
		res, _, _ := procMessageBoxW.Call(
			g.mainHwnd,
			uintptr(unsafe.Pointer(utf16Ptr("Downloads are currently active.\n\nPause active downloads and update yt-dlp now?"))),
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
		force = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	info, err := g.srv.ytdlpUpdater.Update(ctx, g.cfg, g.qm, force, func(st *YtDlpUpdateInfo) {
		if st != nil && st.StatusText != "" {
			g.postStatusMessage("⚡ " + st.StatusText)
		}
	})
	g.postStatusMessage("")
	g.refreshStatusBar()

	if err != nil {
		if interactive {
			procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr("Failed to update yt-dlp:\n\n"+err.Error()))), uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Update Error"))), mbOK|mbIconError)
		}
		return
	}

	verMsg := "yt-dlp + QuickJS updated and ready!"
	if info != nil && info.CurrentVersion != "" {
		verMsg = fmt.Sprintf("yt-dlp updated to v%s (+ QuickJS ready)!", info.CurrentVersion)
	}
	g.showTrayBalloon("yt-dlp Updated", verMsg)
	if interactive {
		procMessageBoxW.Call(g.mainHwnd, uintptr(unsafe.Pointer(utf16Ptr(verMsg))), uintptr(unsafe.Pointer(utf16Ptr("yt-dlp Updated"))), mbOK|mbIconInfo)
	}
}
