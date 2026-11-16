package main

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
)

//go:embed ui
var uiFS embed.FS

type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func (h *Hub) Send(v any) {
	b, _ := json.Marshal(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- b:
		default: // a stalled page shouldn't block WhatsApp event handling
		}
	}
}

func (h *Hub) sub() chan []byte {
	c := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) unsub(c chan []byte) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.serveIndex)
	mux.HandleFunc("GET /emoji.js", serveEmoji)
	mux.HandleFunc("GET /api/state", a.apiState)
	mux.HandleFunc("GET /api/chats", a.apiChats)
	mux.HandleFunc("GET /api/messages", a.apiMessages)
	mux.HandleFunc("GET /api/events", a.apiEvents)
	mux.HandleFunc("GET /media", a.apiMedia)
	mux.HandleFunc("GET /avatar", a.apiAvatar)
	mux.HandleFunc("POST /api/send", a.apiSend)
	mux.HandleFunc("POST /api/upload", a.apiUpload)
	mux.HandleFunc("POST /api/react", a.apiReact)
	mux.HandleFunc("POST /api/read", a.apiRead)
	mux.HandleFunc("POST /api/view", a.apiView)
	mux.HandleFunc("POST /api/open", a.apiOpen)
	mux.HandleFunc("POST /api/openurl", a.apiOpenURL)
	mux.HandleFunc("POST /api/relink", a.apiRelink)
	mux.HandleFunc("POST /api/history", a.apiHistory)
	mux.HandleFunc("POST /api/title", a.apiTitle)
	mux.HandleFunc("GET /api/search", a.apiSearch)
	mux.HandleFunc("GET /api/members", a.apiMembers)
	mux.HandleFunc("POST /api/draft", a.apiDraft)
	mux.HandleFunc("POST /api/propose", a.apiPropose)
	mux.HandleFunc("GET /api/proposal", a.apiProposal)
	mux.HandleFunc("GET /api/proposals", a.apiProposals)
	mux.HandleFunc("POST /api/decide", a.apiDecide)
	mux.HandleFunc("GET /api/proposal-file", a.apiProposalFile)
	mux.HandleFunc("GET /api/media-file", a.apiMediaFile)
	mux.HandleFunc("POST /api/typing", a.apiTyping)
	mux.HandleFunc("POST /api/vote", a.apiVote)
	mux.HandleFunc("GET /api/contacts", a.apiContacts)
	mux.HandleFunc("POST /api/lookup", a.apiLookup)
	mux.HandleFunc("POST /api/reveal", a.apiReveal)
	return a.guard(mux)
}

// agentAllowed is everything the agent key may do: read (attachments too), draft, and propose messages for approval.
// Sending, reacting, approving and marking read stay with the window's own key.
var agentAllowed = map[string]bool{
	"GET /api/state": true, "GET /api/chats": true, "GET /api/messages": true, "GET /api/search": true,
	"POST /api/history": true, "POST /api/draft": true, "POST /api/propose": true, "GET /api/proposal": true,
	"GET /api/media-file": true,
}

// guard blocks other web pages in the user's browser from driving this local server:
// the Host check stops DNS rebinding, the token stops cross-site requests.
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.addr {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/emoji.js" {
			tok := r.Header.Get("X-Token")
			if tok == "" {
				tok = r.URL.Query().Get("t")
			}
			switch {
			case subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) == 1:
			case subtle.ConstantTimeCompare([]byte(tok), []byte(a.agentToken)) == 1 && agentAllowed[r.Method+" "+r.URL.Path]:
			default:
				http.Error(w, "not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (a *App) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func serveEmoji(w http.ResponseWriter, r *http.Request) {
	b, _ := uiFS.ReadFile("ui/emoji.js")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (a *App) apiState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, map[string]any{"state": a.state, "qr": a.qr})
}

func (a *App) apiChats(w http.ResponseWriter, r *http.Request) {
	chats, err := a.db.Chats()
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, c := range chats {
		if j, err := types.ParseJID(c.JID); err == nil {
			c.Group = j.Server == types.GroupServer
			c.Name = a.displayName(j, c.Name)
		}
	}
	sort.SliceStable(chats, func(i, j int) bool { return chats[i].Pinned && !chats[j].Pinned })
	if chats == nil {
		chats = []*Chat{}
	}
	writeJSON(w, chats)
}

func (a *App) apiMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	beforeID := q.Get("before_id")
	if before == 0 {
		before, beforeID = 1<<62, "~"
	}
	msgs, err := a.db.Messages(q.Get("chat"), before, beforeID, 60)
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, m := range msgs {
		a.decorate(m)
	}
	if msgs == nil {
		msgs = []*Msg{}
	}
	writeJSON(w, msgs)
}

func (a *App) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	c := a.hub.sub()
	defer a.hub.unsub(c)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": hi\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-c:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9@._-]`)

func extFor(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	switch strings.TrimSpace(base) {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/png":
		return ".png"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	}
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

var downloads singleflight.Group

// mediaFile downloads and decrypts a message's attachment once and caches it on disk.
func (a *App) mediaFile(m *Msg) (string, error) {
	if m.Deleted || len(m.Raw) == 0 {
		return "", errors.New("no media")
	}
	path := a.cachePath(m)
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		os.Chtimes(path, now, now) // marks it as recently used for pruneMedia
		return path, nil
	}
	_, err, _ := downloads.Do(path, func() (any, error) {
		var msg waE2E.Message
		if err := proto.Unmarshal(m.Raw, &msg); err != nil {
			return nil, err
		}
		part, setPath := mediaPart(&msg)
		if part == nil {
			return nil, whatsmeow.ErrNothingDownloadableFound
		}
		data, err := a.cli.Download(a.ctx, part)
		if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
			errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
			var path string
			if path, err = a.reupload(m, part.GetMediaKey()); err == nil {
				setPath(path)
				if raw, err := proto.Marshal(&msg); err == nil {
					a.db.SetRaw(m.Chat, m.ID, raw)
				}
				data, err = a.cli.Download(a.ctx, part)
			}
		}
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		tmp := path + ".part"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
		go a.pruneMedia()
		return nil, nil
	})
	return path, err
}

// mediaPart finds a message's attachment and returns a way to point it at a new server path.
func mediaPart(msg *waE2E.Message) (whatsmeow.DownloadableMessage, func(string)) {
	msg = unwrapChild(msg)
	switch {
	case msg.ImageMessage != nil:
		x := msg.ImageMessage
		return x, func(p string) { x.DirectPath = &p }
	case msg.PtvMessage != nil:
		x := msg.PtvMessage
		return x, func(p string) { x.DirectPath = &p }
	case msg.VideoMessage != nil:
		x := msg.VideoMessage
		return x, func(p string) { x.DirectPath = &p }
	case msg.AudioMessage != nil:
		x := msg.AudioMessage
		return x, func(p string) { x.DirectPath = &p }
	case msg.DocumentMessage != nil:
		x := msg.DocumentMessage
		return x, func(p string) { x.DirectPath = &p }
	case msg.StickerMessage != nil:
		x := msg.StickerMessage
		return x, func(p string) { x.DirectPath = &p }
	}
	return nil, nil
}

// reupload asks the phone to put an expired attachment back on WhatsApp's servers and returns its new path.
func (a *App) reupload(m *Msg, mediaKey []byte) (string, error) {
	chat, err := types.ParseJID(m.Chat)
	if err != nil {
		return "", err
	}
	sender, _ := types.ParseJID(m.Sender)
	info := &types.MessageInfo{ID: m.ID, MessageSource: types.MessageSource{
		Chat: chat, Sender: sender, IsFromMe: m.FromMe, IsGroup: chat.Server == types.GroupServer,
	}}
	ch := make(chan *events.MediaRetry, 1)
	a.mu.Lock()
	a.retries[m.ID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.retries, m.ID)
		a.mu.Unlock()
	}()
	if err := a.cli.SendMediaRetryReceipt(a.ctx, info, mediaKey); err != nil {
		return "", err
	}
	select {
	case evt := <-ch:
		res, err := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
		if err != nil {
			return "", err
		}
		if res.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || res.GetDirectPath() == "" {
			return "", fmt.Errorf("the phone no longer has this file (%s)", res.GetResult())
		}
		return res.GetDirectPath(), nil
	case <-time.After(30 * time.Second):
		return "", errors.New("the phone didn't answer")
	}
}

func (a *App) apiMedia(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, err := a.db.GetMsg(q.Get("chat"), q.Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	path, err := a.mediaFile(m)
	if err != nil {
		fail(w, 410, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if m.Mime != "" {
		base, _, _ := strings.Cut(m.Mime, ";")
		w.Header().Set("Content-Type", base)
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

type sendReq struct {
	Chat  string `json:"chat"`
	Text  string `json:"text"`
	Quote string `json:"quote"`
	ID    string `json:"id"`
	Emoji string `json:"emoji"`
	File  string `json:"file"` // a path on this computer, for proposals
}

func (a *App) quoteContext(chat types.JID, quoteID string) *waE2E.ContextInfo {
	if quoteID == "" {
		return nil
	}
	q, err := a.db.GetMsg(chat.String(), quoteID)
	if err != nil || len(q.Raw) == 0 {
		return nil
	}
	var qm waE2E.Message
	if proto.Unmarshal(q.Raw, &qm) != nil {
		return nil
	}
	participant := q.Sender
	if q.FromMe && a.cli.Store.ID != nil {
		participant = a.cli.Store.ID.ToNonAD().String()
	}
	return &waE2E.ContextInfo{StanzaID: proto.String(quoteID), Participant: proto.String(participant), QuotedMessage: &qm}
}

func (a *App) textMessage(chat types.JID, text, quoteID string) *waE2E.Message {
	text, ci := a.outgoing(chat, text, quoteID)
	if ci != nil {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}}
	}
	return &waE2E.Message{Conversation: proto.String(text)}
}

// deliver sends a message and stores our copy, since WhatsApp doesn't echo our own sends back to us.
func (a *App) deliver(chat types.JID, msg *waE2E.Message) (*Msg, error) {
	resp, err := a.cli.SendMessage(a.ctx, chat, msg)
	if err != nil {
		return nil, err
	}
	me := ""
	if a.cli.Store.ID != nil {
		me = a.cli.Store.ID.ToNonAD().String()
	}
	rec := buildMsg(chat.String(), resp.ID, me, true, resp.Timestamp.Unix(), msg)
	if rec == nil {
		return nil, errors.New("unsupported message")
	}
	a.db.InsertMsg(rec)
	a.db.TouchChat(rec.Chat, rec.TS, withNames(preview(rec), a.mentionNames(rec.Raw)), 0)
	a.pushMsg(rec.Chat, rec.ID)
	a.hub.Send(map[string]any{"t": "chats"})
	return rec, nil
}

func (a *App) apiSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	chat, err := types.ParseJID(req.Chat)
	if err != nil || strings.TrimSpace(req.Text) == "" {
		fail(w, 400, errors.New("chat and text required"))
		return
	}
	rec, err := a.deliver(chat, a.textMessage(chat, req.Text, req.Quote))
	if err != nil {
		fail(w, 502, err)
		return
	}
	a.claude.observe(rec) // typing in the Claude group from this window counts too
	writeJSON(w, map[string]string{"id": rec.ID})
}

func (a *App) apiUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(w, 400, err)
		return
	}
	chat, err := types.ParseJID(r.FormValue("chat"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		fail(w, 400, err)
		return
	}
	if len(data) > maxUpload {
		fail(w, 413, errors.New("files over 100 MB can't be sent"))
		return
	}
	// The window works out a video's size, length and first frame, which Go can't decode on its own.
	var meta mediaMeta
	meta.Thumb, _ = base64.StdEncoding.DecodeString(r.FormValue("thumb"))
	fmt.Sscan(r.FormValue("w"), &meta.W)
	fmt.Sscan(r.FormValue("h"), &meta.H)
	fmt.Sscan(r.FormValue("seconds"), &meta.Seconds)
	rec, err := a.sendMedia(chat, data, hdr.Filename, r.FormValue("caption"), r.FormValue("quote"), meta)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]string{"id": rec.ID})
}

const maxUpload = 100 << 20

// fileMime goes by the extension first: sniffing calls a .docx a zip.
func fileMime(name string, data []byte) string {
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if mt == "" {
		mt = http.DetectContentType(data)
	}
	mt, _, _ = strings.Cut(mt, ";")
	return mt
}

// sendMedia uploads a file as a photo, video or document, by its type, and sends it.
type mediaMeta struct {
	Thumb         []byte
	W, H, Seconds uint32
}

func (a *App) sendMedia(chat types.JID, data []byte, name, caption, quote string, meta mediaMeta) (*Msg, error) {
	mt := fileMime(name, data)
	caption, ci := a.outgoing(chat, caption, quote)
	kind := whatsmeow.MediaDocument
	switch mt {
	case "image/jpeg", "image/png", "image/webp":
		kind = whatsmeow.MediaImage
	case "video/mp4":
		kind = whatsmeow.MediaVideo
	}
	up, err := a.cli.Upload(a.ctx, data, kind)
	if err != nil {
		return nil, err
	}
	var msg *waE2E.Message
	switch kind {
	case whatsmeow.MediaImage:
		msg = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			Mimetype: proto.String(mt), Caption: proto.String(caption), ContextInfo: ci,
		}}
		// Without a size the receiving app guesses the shape and stretches the picture.
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			msg.ImageMessage.Width, msg.ImageMessage.Height = proto.Uint32(uint32(cfg.Width)), proto.Uint32(uint32(cfg.Height))
		}
		// The small preview shows while the photo downloads, and in replies and notifications.
		msg.ImageMessage.JPEGThumbnail = jpegThumb(data)
	case whatsmeow.MediaVideo:
		msg = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			Mimetype: proto.String(mt), Caption: proto.String(caption), ContextInfo: ci,
		}}
		if meta.W > 0 && meta.H > 0 {
			msg.VideoMessage.Width, msg.VideoMessage.Height = proto.Uint32(meta.W), proto.Uint32(meta.H)
		}
		if meta.Seconds > 0 {
			msg.VideoMessage.Seconds = proto.Uint32(meta.Seconds)
		}
		if len(meta.Thumb) > 0 {
			msg.VideoMessage.JPEGThumbnail = meta.Thumb
		}
	default:
		if mt == "" {
			mt = "application/octet-stream"
		}
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			Mimetype: proto.String(mt), FileName: proto.String(name), Title: proto.String(name),
			Caption: proto.String(caption), ContextInfo: ci,
		}}
	}
	rec, err := a.deliver(chat, msg)
	if err != nil {
		return nil, err
	}
	// Seed the cache with the file we already have so it shows without a round trip.
	p := a.cachePath(rec)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, data, 0o644)
	return rec, nil
}

// cachePath keeps a document's own file name so it opens in the right app.
func (a *App) cachePath(m *Msg) string {
	if m.Kind == "document" && m.FileName != "" {
		name := strings.Trim(unsafeChars.ReplaceAllString(m.FileName, "_"), ". ")
		if name == "" {
			name = "file" + extFor(m.Mime)
		}
		return filepath.Join(a.mediaDir, "docs", unsafeChars.ReplaceAllString(m.ID, "_"), name)
	}
	return filepath.Join(a.mediaDir, unsafeChars.ReplaceAllString(m.Chat+"_"+m.ID, "_")+extFor(m.Mime))
}

func (a *App) apiReact(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	chat, err := types.ParseJID(req.Chat)
	if err != nil {
		fail(w, 400, err)
		return
	}
	m, err := a.db.GetMsg(req.Chat, req.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	sender := types.EmptyJID
	if m.FromMe && a.cli.Store.ID != nil {
		sender = a.cli.Store.ID.ToNonAD()
	} else {
		sender, _ = types.ParseJID(m.Sender)
	}
	if _, err := a.cli.SendMessage(a.ctx, chat, a.cli.BuildReaction(chat, sender, req.ID, req.Emoji)); err != nil {
		fail(w, 502, err)
		return
	}
	a.db.SetReaction(req.Chat, req.ID, selfMarker, req.Emoji)
	a.pushMsg(req.Chat, req.ID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiRead(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	chat, err := types.ParseJID(req.Chat)
	if err != nil {
		fail(w, 400, err)
		return
	}
	unread, _ := a.db.ChatUnread(req.Chat)
	if unread > 0 {
		n := min(unread, 100)
		ids, senders, _ := a.db.LatestIncoming(req.Chat, n)
		go a.sendRead(chat, ids, senders)
		a.db.SetChatField(req.Chat, "unread", 0)
		a.hub.Send(map[string]any{"t": "chats"})
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiView(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat    string `json:"chat"`
		Focused bool   `json:"focused"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	a.mu.Lock()
	changed := a.focused != req.Focused
	a.openChat, a.focused = req.Chat, req.Focused
	a.mu.Unlock()
	if changed {
		go a.setAvailable(req.Focused)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiTyping(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
		On   bool   `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	jid, err := types.ParseJID(req.Chat)
	if err != nil {
		fail(w, 400, err)
		return
	}
	state := types.ChatPresencePaused
	if req.On {
		state = types.ChatPresenceComposing
	}
	a.cli.SendChatPresence(a.ctx, jid, state, types.ChatPresenceMediaText)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiVote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat    string   `json:"chat"`
		ID      string   `json:"id"`
		Options []string `json:"options"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	m, err := a.db.GetMsg(req.Chat, req.ID)
	if err != nil || m.Kind != "poll" {
		fail(w, 404, errors.New("poll not found"))
		return
	}
	chat, _ := types.ParseJID(m.Chat)
	sender, _ := types.ParseJID(m.Sender)
	info := &types.MessageInfo{ID: m.ID, MessageSource: types.MessageSource{
		Chat: chat, Sender: sender, IsFromMe: m.FromMe, IsGroup: chat.Server == types.GroupServer,
	}}
	msg, err := a.cli.BuildPollVote(a.ctx, info, req.Options)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if _, err := a.cli.SendMessage(a.ctx, chat, msg); err != nil {
		fail(w, 502, err)
		return
	}
	a.db.SetVote(m.Chat, m.ID, selfMarker, hexes(whatsmeow.HashPollOptions(req.Options)))
	a.pushMsg(m.Chat, m.ID)
	writeJSON(w, map[string]bool{"ok": true})
}

// apiContacts lists the address book for starting a new chat.
func (a *App) apiContacts(w http.ResponseWriter, r *http.Request) {
	all, err := a.cli.Store.Contacts.GetAllContacts(a.ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	type entry struct {
		JID  string `json:"jid"`
		Name string `json:"name"`
	}
	list := []entry{}
	for jid, c := range all {
		jid = a.toPN(jid)
		if jid.Server != types.DefaultUserServer {
			continue
		}
		name := c.FullName
		for _, n := range []string{c.FirstName, c.BusinessName, c.PushName} {
			if name == "" {
				name = n
			}
		}
		if name != "" {
			list = append(list, entry{jid.String(), name})
		}
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	writeJSON(w, list)
}

// apiLookup checks that a phone number has WhatsApp and returns its chat.
func (a *App) apiLookup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, req.Phone)
	if len(digits) < 6 {
		fail(w, 400, errors.New("type the number with its country code"))
		return
	}
	res, err := a.cli.IsOnWhatsApp(a.ctx, []string{"+" + digits})
	if err != nil {
		fail(w, 502, err)
		return
	}
	if len(res) == 0 || !res[0].IsIn {
		fail(w, 404, errors.New("this number isn't on WhatsApp"))
		return
	}
	writeJSON(w, map[string]string{"jid": res[0].JID.ToNonAD().String()})
}

// apiReveal shows a downloaded file in Explorer, so it can be copied or saved somewhere else.
func (a *App) apiReveal(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	m, err := a.db.GetMsg(req.Chat, req.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	path, err := a.mediaFile(m)
	if err != nil {
		fail(w, 410, err)
		return
	}
	if err := exec.Command("explorer.exe", "/select,", path).Start(); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// jpegThumb makes the small preview WhatsApp sends with a photo, at most 100 px on its long side.
func jpegThumb(data []byte) []byte {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	scale := 100.0 / float64(max(b.Dx(), b.Dy()))
	if scale > 1 {
		scale = 1
	}
	w, h := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// Average each block of source pixels; plain sampling makes thumbnails of screenshots noisy.
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			var r, g, bl, n uint64
			for sy := y0; sy < max(y1, y0+1); sy += max(1, (y1-y0)/4) {
				for sx := x0; sx < max(x1, x0+1); sx += max(1, (x1-x0)/4) {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, bl, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), n+1
				}
			}
			dst.Set(x, y, color.RGBA64{uint16(r / n), uint16(g / n), uint16(bl / n), 0xffff})
		}
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, dst, &jpeg.Options{Quality: 70}) != nil {
		return nil
	}
	return out.Bytes()
}

// Extensions that would run code when opened; we show these in Explorer instead of launching them.
var runnable = map[string]bool{
	".exe": true, ".bat": true, ".cmd": true, ".com": true, ".scr": true, ".ps1": true, ".vbs": true,
	".js": true, ".jse": true, ".wsf": true, ".msi": true, ".lnk": true, ".hta": true, ".jar": true,
	".reg": true, ".cpl": true, ".pif": true, ".url": true, ".appx": true, ".msix": true,
}

func shellOpen(target string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

func (a *App) apiOpen(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	m, err := a.db.GetMsg(req.Chat, req.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	path, err := a.mediaFile(m)
	if err != nil {
		fail(w, 410, err)
		return
	}
	if runnable[strings.ToLower(filepath.Ext(path))] {
		err = exec.Command("explorer.exe", "/select,", path).Start()
	} else {
		err = shellOpen(path)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiOpenURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		fail(w, 400, errors.New("only web links"))
		return
	}
	if err := shellOpen(u.String()); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiRelink(w http.ResponseWriter, r *http.Request) {
	go a.pair()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiTitle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title  string `json:"title"`
		Unread int    `json:"unread"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if a.win != nil {
		a.win.SetTitle(req.Title)
	}
	setTrayState(req.Title, req.Unread)
	writeJSON(w, map[string]bool{"ok": true})
}
