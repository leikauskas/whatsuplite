package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"google.golang.org/protobuf/proto"
)

const appID = "WhatsUpLite"

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procShowWindow      = user32.NewProc("ShowWindow")
	procSetForeground   = user32.NewProc("SetForegroundWindow")
	procFlashWindowEx   = user32.NewProc("FlashWindowEx")
	procSetDpiContext   = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem = user32.NewProc("GetDpiForSystem")
)

func init() { runtime.LockOSThread() } // WebView2 must live on the main thread

func logf(format string, args ...any) { log.Printf(format, args...) }

type fileLogger struct{ module string }

func (l fileLogger) Errorf(msg string, args ...any) {
	log.Printf("[%s ERROR] "+msg, append([]any{l.module}, args...)...)
}
func (l fileLogger) Warnf(msg string, args ...any) {
	log.Printf("[%s WARN] "+msg, append([]any{l.module}, args...)...)
}
func (l fileLogger) Infof(msg string, args ...any)  {}
func (l fileLogger) Debugf(msg string, args ...any) {}
func (l fileLogger) Sub(module string) waLog.Logger { return fileLogger{l.module + "/" + module} }

type Window struct {
	wv   webview2.WebView
	hwnd uintptr
}

func (w *Window) SetTitle(t string) { w.wv.Dispatch(func() { w.wv.SetTitle(t) }) }

func (w *Window) Flash() {
	type flashInfo struct {
		size    uint32
		hwnd    uintptr
		flags   uint32
		count   uint32
		timeout uint32
	}
	fi := flashInfo{hwnd: w.hwnd, flags: 0x2 | 0xC} // FLASHW_TRAY | FLASHW_TIMERNOFG
	fi.size = uint32(unsafe.Sizeof(fi))
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

func appDataDir() string {
	if d := os.Getenv("WHATSUPLITE_DATA"); d != "" {
		return d
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), appID)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCP(os.Args[2:])
		return
	}
	dataDir := appDataDir()
	os.MkdirAll(dataDir, 0o755)
	hwndFile := filepath.Join(dataDir, "window")

	background, demo, openChat := false, false, ""
	for _, arg := range os.Args[1:] {
		switch {
		case arg == "--background":
			background = true
		case arg == "--demo": // screenshots and talks: shows the data folder offline, changes nothing on the system
			demo = true
		case strings.HasPrefix(arg, "whatsuplite:"): // a notification click
			if u, err := url.Parse(arg); err == nil {
				openChat = u.Query().Get("chat")
			}
		}
	}

	// One copy per data folder: two clients on one linked device would keep kicking each other off.
	lock := "Local\\" + appID
	if os.Getenv("WHATSUPLITE_DATA") != "" {
		lock += "-" + strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(strings.ToLower(dataDir)))), 16)
	}
	if _, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(lock)); err == windows.ERROR_ALREADY_EXISTS {
		if b, err := os.ReadFile(hwndFile); err == nil && !background {
			if h, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				handOver(uintptr(h), openChat)
			}
		}
		return
	}

	// Keep the log across restarts so a problem is still visible after reopening; start over past 5 MB.
	logPath := filepath.Join(dataDir, "whatsuplite.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 5<<20 {
		os.Remove(logPath)
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(f)
	}

	ctx := context.Background()
	db, err := openDB(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		log.Fatalf("messages db: %v", err)
	}
	if err := db.FillPreviews(); err != nil {
		logf("previews: %v", err)
	}
	container, err := sqlstore.New(ctx, "sqlite",
		"file:"+filepath.ToSlash(filepath.Join(dataDir, "session.db"))+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)",
		fileLogger{"db"})
	if err != nil {
		log.Fatalf("session db: %v", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("device: %v", err)
	}
	store.SetOSInfo(appID, [3]uint32{0, 1, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	// Without a full sync the phone sends about one message per chat, leaving most chats empty. The phone
	// only reads this when a device links, so an existing link has to be redone to get the history.
	store.DeviceProps.RequireFullSync = proto.Bool(true)

	newToken := func() string {
		b := make([]byte, 24)
		rand.Read(b)
		return hex.EncodeToString(b)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	a := &App{
		ctx: ctx, container: container, retries: map[string]chan *events.MediaRetry{}, db: db, dataDir: dataDir, mediaDir: filepath.Join(dataDir, "media"),
		hub: &Hub{subs: map[chan []byte]struct{}{}}, addr: ln.Addr().String(), token: newToken(), agentToken: newToken(),
		state: "connecting", contacts: nil, lastToast: map[string]time.Time{}, histAsked: map[string]time.Time{},
		props: &proposals{m: map[string]*Proposal{}},
	}
	a.claude = newClaudeBridge(a)
	// Agents find the running instance through this file. It holds the restricted agent key only;
	// the window's full key lives in memory and in the window's own address.
	os.WriteFile(filepath.Join(dataDir, "endpoint"), []byte("http://"+a.addr+"\n"+a.agentToken+"\n"), 0o600)
	if a.names, err = db.AllNames(); err != nil {
		a.names = map[string]string{}
	}
	a.cli = whatsmeow.NewClient(device, fileLogger{"wa"})
	a.cli.AddEventHandler(a.onEvent)
	go http.Serve(ln, a.routes())
	if demo {
		a.setState("online", "")
	} else {
		registerToastApp(filepath.Join(dataDir, "whatsuplite.ico"))
		go a.start()
		go a.pruneMedia()
	}

	// Without this Windows renders the window at 96 DPI and stretches it, which looks blurry on scaled screens.
	procSetDpiContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	scale := 1.0
	if dpi, _, _ := procGetDpiForSystem.Call(); dpi > 0 {
		scale = float64(dpi) / 96
	}
	winW, winH := int(1100*scale), int(760*scale)

	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(dataDir, "webview"),
		WindowOptions: webview2.WindowOptions{
			Title: appID, Width: uint(winW), Height: uint(winH), Center: true,
		},
	})
	if wv == nil {
		showError("WhatsUpLite needs the Microsoft Edge WebView2 runtime.")
		return
	}
	a.win = &Window{wv: wv, hwnd: uintptr(wv.Window())}
	if background {
		procShowWindow.Call(a.win.hwnd, 0) // started with Windows: tray only
	}
	icon := a.win.setIcon(dataDir, scale)
	a.win.styleTitleBar()
	a.setupTray(icon, scale)
	if !demo {
		registerLinks()
		firstRunStartup(dataDir)
		if startupEnabled() {
			setStartup(true) // keeps the entry pointing at this exe if it was moved or updated
		}
	}
	os.WriteFile(hwndFile, []byte(strconv.FormatUint(uint64(a.win.hwnd), 10)), 0o644)
	wv.SetSize(winW, winH, webview2.HintNone)
	page := fmt.Sprintf("http://%s/?t=%s", a.addr, a.token)
	if openChat != "" {
		page += "&open=" + url.QueryEscape(openChat)
	}
	wv.Navigate(page)
	wv.Run()

	removeTray()
	a.cli.Disconnect()
	os.Remove(hwndFile)
}

func (a *App) start() {
	if a.cli.Store.ID == nil {
		a.pair()
		return
	}
	for {
		err := a.cli.Connect()
		if err == nil {
			return
		}
		logf("connect: %v", err)
		a.setState("offline", "")
		time.Sleep(5 * time.Second)
	}
}

func (a *App) pair() {
	if a.cli.Store.ID != nil {
		go a.start()
		return
	}
	a.cli.Disconnect()
	if a.cli.Store.Deleted {
		// Unlinked from the phone: whatsmeow wiped the old device, and a client can't link again on it.
		a.cli.RemoveEventHandlers()
		a.cli = whatsmeow.NewClient(a.container.NewDevice(), fileLogger{"wa"})
		a.cli.AddEventHandler(a.onEvent)
	}
	ch, err := a.cli.GetQRChannel(context.Background())
	if err != nil {
		logf("qr channel: %v", err)
		a.setState("qr-expired", "")
		return
	}
	if err := a.cli.Connect(); err != nil {
		logf("connect for pairing: %v", err)
		a.setState("qr-expired", "")
		return
	}
	for item := range ch {
		switch item.Event {
		case "code":
			png, err := qrcode.Encode(item.Code, qrcode.Medium, 300)
			if err == nil {
				a.setState("qr", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png))
			}
		case "success":
			a.setState("syncing", "")
		default:
			logf("pairing: %s %v", item.Event, item.Error)
			a.setState("qr-expired", "")
		}
	}
}

// notify shows a Windows toast for a new message when the user isn't looking at that chat.
func (a *App) notify(chat types.JID, m *Msg) {
	names := a.mentionNames(m.Raw)
	if _, muted := a.db.ChatUnread(m.Chat); muted > time.Now().Unix() && !a.mentionsMe(names) {
		return
	}
	a.mu.Lock()
	if time.Since(a.lastToast[m.Chat]) < 4*time.Second {
		a.mu.Unlock()
		return
	}
	a.lastToast[m.Chat] = time.Now()
	a.mu.Unlock()

	chats, _ := a.db.Chats()
	title := ""
	for _, c := range chats {
		if c.JID == m.Chat {
			title = a.displayName(chat, c.Name)
		}
	}
	body := withNames(preview(m), names)
	if chat.Server == types.GroupServer {
		if s, err := types.ParseJID(m.Sender); err == nil {
			body = strings.TrimPrefix(a.displayName(s, ""), "~") + ": " + body
		}
	}
	if a.win != nil {
		a.win.Flash()
	}
	go toast(strings.TrimPrefix(title, "~"), body, m.Chat)
}

// mentionsMe is true when a message @-mentions the user; those notify even in muted chats, as on the phone.
func (a *App) mentionsMe(names map[string]mention) bool {
	if a.cli.Store.ID == nil {
		return false
	}
	me := a.cli.Store.ID.ToNonAD().String()
	for _, n := range names {
		if n.JID == me {
			return true
		}
	}
	return false
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// toast shows a Windows notification; clicking it opens the given chat through the whatsuplite: link.
func toast(title, body, chat string) {
	if len([]rune(body)) > 200 {
		body = string([]rune(body)[:200]) + "…"
	}
	launch := "whatsuplite://open"
	if chat != "" {
		launch += "?chat=" + url.QueryEscape(chat)
	}
	var attr strings.Builder
	xml.EscapeText(&attr, []byte(launch))
	doc := `<toast activationType="protocol" launch="` + attr.String() + `"><visual><binding template="ToastGeneric"><text>` +
		xmlText(title) + `</text><text>` + xmlText(body) + `</text></binding></visual></toast>`
	ps := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml('` + strings.ReplaceAll(doc, "'", "''") + `')
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + appID + `').Show([Windows.UI.Notifications.ToastNotification]::new($x))`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "-")
	cmd.Stdin = strings.NewReader(ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Run(); err != nil {
		logf("toast: %v", err)
	}
}

// registerToastApp gives toasts a "WhatsUpLite" sender name; unregistered app ids are silently dropped.
func registerToastApp(icon string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+appID, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("DisplayName", appID)
	k.SetStringValue("IconUri", icon)
}

func showError(msg string) {
	windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr(appID), 0x10)
}
