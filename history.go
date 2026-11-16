package main

import (
	"errors"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// requestHistory asks the phone for the 50 messages before the oldest one we hold for a chat.
// The phone answers later with an on-demand history sync, handled in handleHistory.
func (a *App) requestHistory(chat string) error {
	if a.cli.Store.ID == nil || !a.cli.IsConnected() {
		return errors.New("not connected")
	}
	if _, err := types.ParseJID(chat); chat == "" || err != nil {
		return errors.New("not a chat") // a malformed request makes the phone report a failed sync
	}
	a.mu.Lock()
	if time.Since(a.histAsked[chat]) < 20*time.Second {
		a.mu.Unlock()
		return nil
	}
	a.histAsked[chat] = time.Now()
	a.mu.Unlock()

	jid, err := types.ParseJID(chat)
	if err != nil || jid.User == "" || skipChat(jid) {
		return errors.New("not a chat")
	}
	anchor := &types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, Timestamp: time.Now()}
	var id string
	var fromMe bool
	var ts int64
	if a.db.QueryRow(`SELECT id, from_me, ts FROM messages WHERE chat=? ORDER BY ts, id LIMIT 1`, chat).Scan(&id, &fromMe, &ts) == nil {
		anchor.ID, anchor.IsFromMe, anchor.Timestamp = id, fromMe, time.Unix(ts, 0)
	}
	_, err = a.cli.SendMessage(a.ctx, a.cli.Store.ID.ToNonAD(), a.cli.BuildHistorySyncRequest(anchor, 50),
		whatsmeow.SendRequestExtra{Peer: true})
	logf("asked phone for history of %s before %q: %v", chat, anchor.ID, err)
	return err
}

func (a *App) apiHistory(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.requestHistory(req.Chat); err != nil {
		fail(w, 503, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
