package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icon/whatsuplite.ico
var iconICO []byte

var (
	procLoadImage   = user32.NewProc("LoadImageW")
	procSendMessage = user32.NewProc("SendMessageW")
	procDwmSetAttr  = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
)

// setIcon gives the window and taskbar button the WhatsUpLite icon and returns the small one for the tray.
// The file also serves the Start menu shortcut and notifications.
func (w *Window) setIcon(dataDir string, scale float64) (small uintptr) {
	path := filepath.Join(dataDir, "whatsuplite.ico")
	if err := os.WriteFile(path, iconICO, 0o644); err != nil {
		return 0
	}
	p, _ := windows.UTF16PtrFromString(path)
	for _, s := range []struct {
		kind uintptr // ICON_SMALL, ICON_BIG
		size float64
	}{{0, 16}, {1, 32}} {
		px := uintptr(s.size * scale)
		h, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), 1 /* IMAGE_ICON */, px, px, 0x10 /* LR_LOADFROMFILE */)
		if h != 0 {
			procSendMessage.Call(w.hwnd, 0x80 /* WM_SETICON */, s.kind, h)
			if s.kind == 0 {
				small = h
			}
		}
	}
	return small
}

// styleTitleBar paints the title bar in the app's own panel colour instead of the default white one.
func (w *Window) styleTitleBar() {
	dark := false
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		v, _, err := k.GetIntegerValue("AppsUseLightTheme")
		dark = err == nil && v == 0
		k.Close()
	}
	// COLORREF is 0x00BBGGRR; these match --panel and --text in ui/index.html.
	caption, text := uint32(0x00FFFFFF), uint32(0x001D1816)
	darkFlag := uint32(0)
	if dark {
		caption, text, darkFlag = 0x001F1B17, 0x00EDEAE8, 1
	}
	set := func(attr uintptr, v uint32) {
		procDwmSetAttr.Call(w.hwnd, attr, uintptr(unsafe.Pointer(&v)), 4)
	}
	set(20, darkFlag) // DWMWA_USE_IMMERSIVE_DARK_MODE
	set(35, caption)  // DWMWA_CAPTION_COLOR
	set(34, caption)  // DWMWA_BORDER_COLOR
	set(36, text)     // DWMWA_TEXT_COLOR
}
