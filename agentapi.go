package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// Endpoints used by the MCP server (see mcp.go) on top of the ones the window uses.

func (d *DB) Search(q, chat string, limit int) ([]*Msg, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	query := `SELECT ` + selCols + ` FROM messages WHERE deleted=0 AND kind != 'system' AND (text LIKE ? ESCAPE '\' OR file_name LIKE ? ESCAPE '\')`
	args := []any{like, like}
	if chat != "" {
		query += ` AND chat=?`
		args = append(args, chat)
	}
	query += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Msg
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (a *App) apiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))
	if text == "" {
		fail(w, 400, errors.New("q required"))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	msgs, err := a.db.Search(text, q.Get("chat"), limit)
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

// apiMediaFile downloads a message's attachment into the cache and says where it is, so an agent on this
// computer can look at it.
func (a *App) apiMediaFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, err := a.db.GetMsg(q.Get("chat"), q.Get("id"))
	if err != nil {
		fail(w, 404, errors.New("message not found in this chat"))
		return
	}
	if m.Kind == "text" || m.Kind == "system" {
		fail(w, 400, errors.New("this message has no attachment"))
		return
	}
	path, err := a.mediaFile(m)
	if err != nil {
		fail(w, 410, fmt.Errorf("couldn't download the attachment: %v", err))
		return
	}
	mt, _, _ := strings.Cut(m.Mime, ";")
	writeJSON(w, map[string]string{"path": path, "mime": mt, "kind": m.Kind, "file_name": m.FileName})
}

// apiDraft puts text in the message box of a chat for the user to review and send themselves.
func (a *App) apiDraft(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if _, err := types.ParseJID(req.Chat); err != nil || req.Chat == "" || strings.TrimSpace(req.Text) == "" {
		fail(w, 400, errors.New("chat and text required"))
		return
	}
	a.hub.Send(map[string]any{"t": "draft", "chat": req.Chat, "text": req.Text, "quote": req.Quote})
	if a.win != nil {
		a.win.Flash()
	}
	writeJSON(w, map[string]bool{"ok": true})
}
