package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Messages an agent wants to send wait here until the user approves, edits or discards them in the window.

const proposalTTL = 15 * time.Minute

type Proposal struct {
	ID        string    `json:"id"`
	Chat      string    `json:"chat"`
	ChatName  string    `json:"chat_name"`
	Text      string    `json:"text"`
	Quote     string    `json:"quote,omitempty"`
	QuoteText string    `json:"quote_text,omitempty"`
	File      string    `json:"file,omitempty"` // a path on this computer; Text is then its caption
	FileName  string    `json:"file_name,omitempty"`
	FileSize  int64     `json:"file_size,omitempty"`
	Mime      string    `json:"mime,omitempty"`
	Created   time.Time `json:"created"`
	Status    string    `json:"status"`          // pending, sent, discarded, expired, failed
	Final     string    `json:"final,omitempty"` // text actually sent
	Error     string    `json:"error,omitempty"`
	done      chan struct{}
}

type proposals struct {
	mu sync.Mutex
	m  map[string]*Proposal
}

func (a *App) apiPropose(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	jid, err := types.ParseJID(req.Chat)
	if err != nil || jid.User == "" || (strings.TrimSpace(req.Text) == "" && req.File == "") {
		fail(w, 400, errors.New("chat and text or file required"))
		return
	}
	idb := make([]byte, 8)
	rand.Read(idb)
	p := &Proposal{ID: hex.EncodeToString(idb), Chat: jid.String(), Text: req.Text, Quote: req.Quote,
		Created: time.Now(), Status: "pending", done: make(chan struct{})}
	if req.File != "" {
		if err := p.attach(req.File); err != nil {
			fail(w, 400, err)
			return
		}
	}
	name := ""
	if c, err := a.db.Chats(); err == nil {
		for _, ch := range c {
			if ch.JID == p.Chat {
				name = ch.Name
			}
		}
	}
	p.ChatName = strings.TrimPrefix(a.displayName(jid, name), "~")
	if req.Quote != "" {
		if q, err := a.db.GetMsg(p.Chat, req.Quote); err == nil {
			p.QuoteText = preview(q)
		}
	}
	a.props.mu.Lock()
	a.props.m[p.ID] = p
	a.props.mu.Unlock()
	time.AfterFunc(proposalTTL, func() { a.settle(p.ID, "expired", "", "") })

	a.hub.Send(map[string]any{"t": "approval", "proposal": p})
	if a.win != nil {
		a.win.Flash()
	}
	body := p.Text
	if p.File != "" {
		body = strings.TrimSpace("Attachment: " + p.FileName + "\n" + p.Text)
	}
	go toast("Approve message to "+p.ChatName, body, "")
	writeJSON(w, map[string]string{"id": p.ID})
}

// attach checks the file now, so a bad path fails at the agent instead of on the user's Send.
func (p *Proposal) attach(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("file must be an absolute path")
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("can't read the file: %v", err)
	}
	if !st.Mode().IsRegular() {
		return errors.New("file is not a regular file")
	}
	if st.Size() == 0 || st.Size() > maxUpload {
		return fmt.Errorf("file must be between 1 byte and %d MB", maxUpload>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("can't read the file: %v", err)
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	f.Close()
	p.File, p.FileName, p.FileSize = filepath.Clean(path), filepath.Base(path), st.Size()
	p.Mime = fileMime(p.FileName, head[:n])
	return nil
}

// apiProposalFile lets the approval card preview the attachment it is asking about.
func (a *App) apiProposalFile(w http.ResponseWriter, r *http.Request) {
	a.props.mu.Lock()
	p := a.props.m[r.URL.Query().Get("id")]
	a.props.mu.Unlock()
	if p == nil || p.File == "" {
		fail(w, 404, errors.New("no such attachment"))
		return
	}
	w.Header().Set("Content-Type", p.Mime)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, p.File)
}

func (a *App) settle(id, status, final, errText string) *Proposal {
	a.props.mu.Lock()
	p := a.props.m[id]
	if p == nil || p.Status != "pending" {
		a.props.mu.Unlock()
		return p
	}
	p.Status, p.Final, p.Error = status, final, errText
	close(p.done)
	a.props.mu.Unlock()
	a.hub.Send(map[string]any{"t": "approval_done", "id": id, "status": status})
	// Keep the outcome around long enough for the agent to collect it.
	time.AfterFunc(10*time.Minute, func() {
		a.props.mu.Lock()
		delete(a.props.m, id)
		a.props.mu.Unlock()
	})
	return p
}

// apiProposal waits up to 45 s for a decision so the agent can poll without hammering.
func (a *App) apiProposal(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a.props.mu.Lock()
	p := a.props.m[id]
	a.props.mu.Unlock()
	if p == nil {
		fail(w, 404, errors.New("unknown proposal"))
		return
	}
	select {
	case <-p.done:
	case <-time.After(45 * time.Second):
	case <-r.Context().Done():
		return
	}
	a.props.mu.Lock()
	defer a.props.mu.Unlock()
	writeJSON(w, p)
}

func (a *App) apiProposals(w http.ResponseWriter, r *http.Request) {
	a.props.mu.Lock()
	defer a.props.mu.Unlock()
	out := []*Proposal{}
	for _, p := range a.props.m {
		if p.Status == "pending" {
			out = append(out, p)
		}
	}
	writeJSON(w, out)
}

// apiDecide is called by the window only: the user pressed Send or Discard on the card.
func (a *App) apiDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Text   string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	a.props.mu.Lock()
	p := a.props.m[req.ID]
	a.props.mu.Unlock()
	if p == nil || p.Status != "pending" {
		fail(w, 409, errors.New("no longer waiting"))
		return
	}
	if req.Action != "send" {
		a.settle(p.ID, "discarded", "", "")
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	text := req.Text
	if strings.TrimSpace(text) == "" && p.File == "" {
		fail(w, 400, errors.New("empty message"))
		return
	}
	jid, _ := types.ParseJID(p.Chat)
	var err error
	if p.File != "" {
		var data []byte
		if data, err = os.ReadFile(p.File); err == nil && int64(len(data)) > maxUpload {
			err = errors.New("the file grew past the size limit")
		}
		if err == nil {
			_, err = a.sendMedia(jid, data, p.FileName, text, p.Quote)
		}
	} else {
		_, err = a.deliver(jid, a.textMessage(jid, text, p.Quote))
	}
	if err != nil {
		a.settle(p.ID, "failed", "", err.Error())
		fail(w, 502, err)
		return
	}
	a.settle(p.ID, "sent", text, "")
	writeJSON(w, map[string]bool{"ok": true})
}
