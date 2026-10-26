package tray

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pSetMenuDefaultItem     = user32.NewProc("SetMenuDefaultItem")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pMessageBoxW            = user32.NewProc("MessageBoxW")
	pCreateIconIndirect     = user32.NewProc("CreateIconIndirect")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")

	pShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW    = shell32.NewProc("ShellExecuteW")
	pShellExecuteExW  = shell32.NewProc("ShellExecuteExW")

	pCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	pCreateBitmap     = gdi32.NewProc("CreateBitmap")
	pDeleteObject     = gdi32.NewProc("DeleteObject")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmContextMenu = 0x007B
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmUser        = 0x0400
	wmApp         = 0x8000

	ninSelect           = wmUser + 0
	ninKeySelect        = wmUser + 1
	ninBalloonUserClick = wmUser + 5

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage  = 0x01
	nifIcon     = 0x02
	nifTip      = 0x04
	nifInfo     = 0x10
	nifShowTip  = 0x80
	niifInfo    = 0x01
	niifWarning = 0x02
	niifError   = 0x03
	niifNoSound = 0x10

	notifyIconVersion4 = 4

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmBottomAlign = 0x0020

	mbOK                  = 0x0000
	mbYesNoCancel         = 0x0003
	mbYesNo               = 0x0004
	mbIconError           = 0x0010
	mbIconQuestion        = 0x0020
	mbIconWarning         = 0x0030
	mbIconInfo            = 0x0040
	mbDefButton2          = 0x0100
	mbSetForeground       = 0x10000
	mbTopmost             = 0x40000
	idYes                 = 6
	idNo                  = 7
	smCxSmIcon            = 49
	swShowNormal          = 1
	swHide                = 0
	seeMaskNoCloseProcess = 0x40
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	HWnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	private uint32
}

type point struct{ X, Y int32 }

// notifyIconData is NOTIFYICONDATAW.
type notifyIconData struct {
	Size            uint32
	HWnd            windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32 // union with uTimeout
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type iconInfo struct {
	IsIcon   int32
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type shellExecuteInfo struct {
	Size       uint32
	Mask       uint32
	HWnd       windows.HWND
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	InstApp    windows.Handle
	IDList     uintptr
	Class      *uint16
	KeyClass   windows.Handle
	HotKey     uint32
	Icon       windows.Handle
	Process    windows.Handle
}

func u16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// copyUTF16 fills a fixed-size field, truncating with an ellipsis if needed.
func copyUTF16(dst []uint16, s string) {
	u := windows.StringToUTF16(s) // includes the terminating NUL
	if len(u) > len(dst) {
		u = append(u[:len(dst)-2], '…', 0)
	}
	copy(dst, u)
	for i := len(u); i < len(dst); i++ {
		dst[i] = 0
	}
}

func shellNotify(op uint32, d *notifyIconData) bool {
	r, _, _ := pShellNotifyIconW.Call(uintptr(op), uintptr(unsafe.Pointer(d)))
	return r != 0
}

func postMessage(h windows.HWND, m uint32, w, l uintptr) {
	pPostMessageW.Call(uintptr(h), uintptr(m), w, l)
}

// messageBox shows a modal message box owned by nobody (so it gets its own taskbar
// button and can be found if it ends up behind other windows).
func messageBox(title, text string, flags uint32) int {
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16(title))), uintptr(flags|mbSetForeground|mbTopmost))
	return int(r)
}

// shellOpen opens a file, folder or URL with its default handler.
func shellOpen(target string) {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(target))), 0, 0, swShowNormal)
}

// runElevated runs exe with args through a UAC prompt and waits for it. It returns the
// exit code, or an error if the user declined the prompt.
func runElevated(exe, args string) (uint32, error) {
	sei := shellExecuteInfo{
		Mask: seeMaskNoCloseProcess, Verb: u16("runas"), File: u16(exe), Parameters: u16(args), Show: swHide,
	}
	sei.Size = uint32(unsafe.Sizeof(sei))
	if r, _, err := pShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		return 0, err
	}
	defer windows.CloseHandle(sei.Process)
	if _, err := windows.WaitForSingleObject(sei.Process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	err := windows.GetExitCodeProcess(sei.Process, &code)
	return code, err
}

func moduleHandle() windows.Handle {
	h, _, _ := pGetModuleHandleW.Call(0)
	return windows.Handle(h)
}
