package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Closing the window hides it to the tray, so WhatsApp, notifications and the Claude bridge keep running.

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	procSetWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc   = user32.NewProc("CallWindowProcW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenu       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procPostMessage      = user32.NewProc("PostMessageW")
	procIsIconic         = user32.NewProc("IsIconic")
)

const (
	wmClose      = 0x0010
	wmCopyData   = 0x004A
	wmTray       = 0x8000 + 1 // WM_APP + 1
	wmLButtonUp  = 0x0202
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205
	nimAdd       = 0
	nimModify    = 1
	nimDelete    = 2
	nifMessage   = 1
	nifIcon      = 2
	nifTip       = 4
	menuOpen     = 1
	menuStartup  = 2
	menuQuit     = 3
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
)

type notifyIconData struct {
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
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

type copyData struct {
	DwData uintptr
	CbData uint32
	LpData uintptr
}

//go:embed icon/unread.ico
var unreadICO []byte

var (
	tray       *App
	oldProc    uintptr
	quitting   bool
	trayIcon   notifyIconData
	iconNormal uintptr
	iconUnread uintptr
	wndProcCbk = windows.NewCallback(wndProc)
)

func (a *App) setupTray(icon uintptr, scale float64) {
	tray = a
	iconNormal = icon
	path := filepath.Join(a.dataDir, "unread.ico")
	if os.WriteFile(path, unreadICO, 0o644) == nil {
		px := uintptr(16 * scale)
		iconUnread, _, _ = procLoadImage.Call(0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(path))), 1, px, px, 0x10)
	}
	oldProc, _, _ = procSetWindowLongPtr.Call(a.win.hwnd, ^uintptr(3), wndProcCbk) // GWLP_WNDPROC = -4
	trayIcon = notifyIconData{HWnd: a.win.hwnd, UID: 1, UFlags: nifMessage | nifIcon | nifTip, UCallbackMessage: wmTray, HIcon: icon}
	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	copy(trayIcon.SzTip[:], windows.StringToUTF16(appID))
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayIcon)))
}

// setTrayState shows the unread count in the tooltip and a dot on the icon while anything is unread.
func setTrayState(tip string, unread int) {
	if trayIcon.HWnd == 0 {
		return
	}
	trayIcon.HIcon = iconNormal
	if unread > 0 && iconUnread != 0 {
		trayIcon.HIcon = iconUnread
	}
	t := windows.StringToUTF16(tip)
	if len(t) > 127 {
		t = append(t[:126], 0)
	}
	trayIcon.SzTip = [128]uint16{}
	copy(trayIcon.SzTip[:], t)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&trayIcon)))
}

func removeTray() {
	if trayIcon.HWnd != 0 {
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayIcon)))
	}
}

func showWindow(hwnd uintptr) {
	procShowWindow.Call(hwnd, 5) // SW_SHOW
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, 9) // SW_RESTORE
	}
	procSetForeground.Call(hwnd)
}

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		if !quitting {
			procShowWindow.Call(hwnd, 0) // SW_HIDE
			return 0
		}
	case wmCopyData:
		// Another launch (a notification click, the Start menu) handing over a chat to open.
		// lp and LpData are addresses Windows owns for the length of this call; read them through
		// pointer-to-pointer casts so the copy happens before the call returns.
		cd := *(**copyData)(unsafe.Pointer(&lp))
		chat := ""
		if cd.CbData > 0 && cd.CbData < 512 {
			chat = string(unsafe.Slice(*(**byte)(unsafe.Pointer(&cd.LpData)), cd.CbData))
		}
		showWindow(hwnd)
		if chat != "" && tray != nil {
			tray.hub.Send(map[string]any{"t": "open", "chat": chat})
		}
		return 1
	case wmTray:
		switch lp & 0xFFFF {
		case wmLButtonUp, wmLButtonDbl:
			showWindow(hwnd)
		case wmRButtonUp:
			trayMenu(hwnd)
		}
		return 0
	}
	r, _, _ := procCallWindowProc.Call(oldProc, hwnd, msg, wp, lp)
	return r
}

func trayMenu(hwnd uintptr) {
	m, _, _ := procCreatePopupMenu.Call()
	defer procDestroyMenu.Call(m)
	add := func(flags, id uintptr, text string) {
		procAppendMenu.Call(m, flags, id, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
	}
	add(0, menuOpen, "Open "+appID)
	startup := uintptr(0)
	if startupEnabled() {
		startup = 0x8 // MF_CHECKED
	}
	add(startup, menuStartup, "Start with Windows")
	add(0x800, 0, "") // separator
	add(0, menuQuit, "Quit")
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForeground.Call(hwnd) // or the menu won't close when clicking elsewhere
	cmd, _, _ := procTrackPopupMenu.Call(m, 0x100|0x2, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0) // TPM_RETURNCMD|TPM_RIGHTBUTTON
	procPostMessage.Call(hwnd, 0, 0, 0)
	switch cmd {
	case menuOpen:
		showWindow(hwnd)
	case menuStartup:
		setStartup(!startupEnabled())
	case menuQuit:
		quitting = true
		removeTray()
		procPostMessage.Call(hwnd, wmClose, 0, 0)
	}
}

func startupEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appID)
	return err == nil
}

func setStartup(on bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if !on {
		k.DeleteValue(appID)
		return
	}
	exe, _ := os.Executable()
	k.SetStringValue(appID, `"`+exe+`" --background`)
}

// registerLinks lets a notification click launch whatsuplite://open?chat=... back into the app.
func registerLinks() {
	exe, _ := os.Executable()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\whatsuplite`, registry.SET_VALUE)
	if err != nil {
		return
	}
	k.SetStringValue("", "URL:"+appID)
	k.SetStringValue("URL Protocol", "")
	k.Close()
	if c, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\whatsuplite\shell\open\command`, registry.SET_VALUE); err == nil {
		c.SetStringValue("", `"`+exe+`" "%1"`)
		c.Close()
	}
}

// firstRunStartup turns on start-with-Windows once; after that the tray menu decides.
func firstRunStartup(dataDir string) {
	marker := filepath.Join(dataDir, "startup-set")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	setStartup(true)
	os.WriteFile(marker, nil, 0o644)
}

// handOver passes a chat to the running copy and brings its window forward.
func handOver(hwnd uintptr, chat string) {
	showWindow(hwnd)
	if chat == "" {
		return
	}
	b := []byte(chat)
	cd := copyData{CbData: uint32(len(b)), LpData: uintptr(unsafe.Pointer(&b[0]))}
	procSendMessage.Call(hwnd, wmCopyData, 0, uintptr(unsafe.Pointer(&cd)))
}
