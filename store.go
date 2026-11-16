package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	wmu sync.Mutex // SQLite allows one writer; serialising here avoids SQLITE_BUSY churn
}

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	last_ts INTEGER NOT NULL DEFAULT 0,
	last_text TEXT NOT NULL DEFAULT '',
	unread INTEGER NOT NULL DEFAULT 0,
	muted_until INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	chat TEXT NOT NULL,
	id TEXT NOT NULL,
	sender TEXT NOT NULL,
	from_me INTEGER NOT NULL,
	ts INTEGER NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	file_name TEXT NOT NULL DEFAULT '',
	mime TEXT NOT NULL DEFAULT '',
	w INTEGER NOT NULL DEFAULT 0,
	h INTEGER NOT NULL DEFAULT 0,
	thumb BLOB,
	raw BLOB,
	quote_id TEXT NOT NULL DEFAULT '',
	quote_text TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	edited INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages (chat, ts, id);
CREATE TABLE IF NOT EXISTS reactions (
	chat TEXT NOT NULL,
	msg_id TEXT NOT NULL,
	sender TEXT NOT NULL,
	emoji TEXT NOT NULL,
	PRIMARY KEY (chat, msg_id, sender)
);
CREATE TABLE IF NOT EXISTS names (jid TEXT PRIMARY KEY, name TEXT NOT NULL);
`

func openDB(path string) (*DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Added after the first release; ALTER fails harmlessly when the column already exists.
	for _, col := range []string{
		`parent TEXT NOT NULL DEFAULT ''`, `is_parent INTEGER NOT NULL DEFAULT 0`, `default_sub INTEGER NOT NULL DEFAULT 0`,
	} {
		db.Exec(`ALTER TABLE chats ADD COLUMN ` + col)
	}
	return &DB{DB: db}, nil
}

type Msg struct {
	Chat       string             `json:"chat"`
	ID         string             `json:"id"`
	Sender     string             `json:"sender"`
	SenderName string             `json:"sender_name,omitempty"`
	FromMe     bool               `json:"from_me"`
	TS         int64              `json:"ts"`
	Kind       string             `json:"kind"`
	Text       string             `json:"text"`
	FileName   string             `json:"file_name,omitempty"`
	Mime       string             `json:"mime,omitempty"`
	W          int                `json:"w,omitempty"`
	H          int                `json:"h,omitempty"`
	Thumb      []byte             `json:"thumb,omitempty"`
	Raw        []byte             `json:"-"`
	QuoteID    string             `json:"quote_id,omitempty"`
	QuoteText  string             `json:"quote_text,omitempty"`
	Mentions   map[string]mention `json:"mentions,omitempty"` // keyed by the number in "@<number>"
	Status     int                `json:"status"`
	Edited     bool               `json:"edited,omitempty"`
	Deleted    bool               `json:"deleted,omitempty"`
	Reactions  []Reaction         `json:"reactions,omitempty"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine,omitempty"`
}

type Chat struct {
	JID        string `json:"jid"`
	Name       string `json:"name"`
	LastTS     int64  `json:"last_ts"`
	LastText   string `json:"last_text"`
	Unread     int    `json:"unread"`
	MutedUntil int64  `json:"muted_until,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
	Pinned     bool   `json:"pinned,omitempty"`
	Group      bool   `json:"group,omitempty"`
	Parent     string `json:"parent,omitempty"`      // community this group belongs to
	IsParent   bool   `json:"is_parent,omitempty"`   // this chat is a community
	DefaultSub bool   `json:"default_sub,omitempty"` // the community's announcements group
}

const msgCols = `chat,id,sender,from_me,ts,kind,text,file_name,mime,w,h,thumb,raw,quote_id,quote_text,status,edited,deleted`

func scanMsg(row interface{ Scan(...any) error }) (*Msg, error) {
	m := &Msg{}
	err := row.Scan(&m.Chat, &m.ID, &m.Sender, &m.FromMe, &m.TS, &m.Kind, &m.Text, &m.FileName, &m.Mime,
		&m.W, &m.H, &m.Thumb, &m.Raw, &m.QuoteID, &m.QuoteText, &m.Status, &m.Edited, &m.Deleted)
	return m, err
}

func (d *DB) InsertMsg(m *Msg) (bool, error) {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	res, err := d.Exec(`INSERT OR IGNORE INTO messages (`+msgCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Chat, m.ID, m.Sender, m.FromMe, m.TS, m.Kind, m.Text, m.FileName, m.Mime, m.W, m.H,
		m.Thumb, m.Raw, m.QuoteID, m.QuoteText, m.Status, m.Edited, m.Deleted)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DB) GetMsg(chat, id string) (*Msg, error) {
	return scanMsg(d.QueryRow(`SELECT `+msgCols+` FROM messages WHERE chat=? AND id=?`, chat, id))
}

// Messages returns up to limit messages older than the (beforeTS, beforeID) cursor, oldest first.
func (d *DB) Messages(chat string, beforeTS int64, beforeID string, limit int) ([]*Msg, error) {
	rows, err := d.Query(`SELECT `+msgCols+` FROM messages WHERE chat=? AND (ts, id) < (?, ?)
		ORDER BY ts DESC, id DESC LIMIT ?`, chat, beforeTS, beforeID, limit)
	if err != nil {
		return nil, err
	}
	var out []*Msg
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, d.attachReactions(chat, out)
}

func (d *DB) attachReactions(chat string, msgs []*Msg) error {
	if len(msgs) == 0 {
		return nil
	}
	byID := make(map[string]*Msg, len(msgs))
	args := []any{chat}
	for _, m := range msgs {
		byID[m.ID] = m
		args = append(args, m.ID)
	}
	q := `SELECT msg_id, emoji, sender FROM reactions WHERE chat=? AND msg_id IN (?` +
		strings.Repeat(",?", len(msgs)-1) + `) ORDER BY rowid`
	rows, err := d.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, emoji, sender string
		if err := rows.Scan(&id, &emoji, &sender); err != nil {
			return err
		}
		m := byID[id]
		found := false
		for i := range m.Reactions {
			if m.Reactions[i].Emoji == emoji {
				m.Reactions[i].Count++
				m.Reactions[i].Mine = m.Reactions[i].Mine || sender == selfMarker
				found = true
			}
		}
		if !found {
			m.Reactions = append(m.Reactions, Reaction{Emoji: emoji, Count: 1, Mine: sender == selfMarker})
		}
	}
	return nil
}

// selfMarker is the sender value stored for our own reactions, so "mine" survives a JID format change.
const selfMarker = "me"

func (d *DB) SetReaction(chat, msgID, sender, emoji string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if emoji == "" {
		_, err := d.Exec(`DELETE FROM reactions WHERE chat=? AND msg_id=? AND sender=?`, chat, msgID, sender)
		return err
	}
	_, err := d.Exec(`INSERT INTO reactions (chat,msg_id,sender,emoji) VALUES (?,?,?,?)
		ON CONFLICT(chat,msg_id,sender) DO UPDATE SET emoji=excluded.emoji`, chat, msgID, sender, emoji)
	return err
}

func (d *DB) MarkDeleted(chat, id string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`UPDATE messages SET deleted=1, text='', thumb=NULL, raw=NULL WHERE chat=? AND id=?`, chat, id)
	return err
}

func (d *DB) SetRaw(chat, id string, raw []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`UPDATE messages SET raw=? WHERE chat=? AND id=?`, raw, chat, id)
	return err
}

func (d *DB) EditText(chat, id, text string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`UPDATE messages SET text=?, edited=1 WHERE chat=? AND id=?`, text, chat, id)
	return err
}

// SetText fills in text found later, such as a voice note's transcript, without marking it edited.
func (d *DB) SetText(chat, id, text string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`UPDATE messages SET text=? WHERE chat=? AND id=?`, text, chat, id)
	return err
}

func (d *DB) RaiseStatus(chat, id string, status int) (bool, error) {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	res, err := d.Exec(`UPDATE messages SET status=? WHERE chat=? AND id=? AND status<?`, status, chat, id, status)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchChat moves a chat's preview forward when a newer message arrives.
func (d *DB) TouchChat(jid string, ts int64, preview string, addUnread int) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`INSERT INTO chats (jid,last_ts,last_text,unread) VALUES (?,?,?,?)
		ON CONFLICT(jid) DO UPDATE SET
			last_text = CASE WHEN excluded.last_ts >= chats.last_ts AND excluded.last_text != '' THEN excluded.last_text ELSE chats.last_text END,
			last_ts = MAX(chats.last_ts, excluded.last_ts),
			unread = chats.unread + excluded.unread`, jid, ts, preview, addUnread)
	return err
}

func (d *DB) SetChatMeta(jid, name string, mutedUntil int64, archived bool, pinned bool, unread int) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`INSERT INTO chats (jid,name,muted_until,archived,pinned,unread) VALUES (?,?,?,?,?,?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
			-- History often omits mute, pin and archive; those come from settings sync, so only add, never clear.
			muted_until = MAX(chats.muted_until, excluded.muted_until),
			archived = chats.archived OR excluded.archived,
			pinned = chats.pinned OR excluded.pinned,
			unread=excluded.unread`,
		jid, name, mutedUntil, archived, pinned, unread)
	return err
}

func (d *DB) SetChatField(jid, field string, value any) error {
	switch field {
	case "name", "muted_until", "archived", "pinned", "unread":
	default:
		panic("unknown chat field " + field)
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`INSERT INTO chats (jid,`+field+`) VALUES (?,?)
		ON CONFLICT(jid) DO UPDATE SET `+field+`=excluded.`+field, jid, value)
	return err
}

// MergeChat folds a chat stored under one id into another, for people seen under both their LID and number.
func (d *DB) MergeChat(from, into string) error {
	if from == into {
		return nil
	}
	var lastTS int64
	var lastText string
	var unread int
	if d.QueryRow(`SELECT last_ts, last_text, unread FROM chats WHERE jid=?`, from).Scan(&lastTS, &lastText, &unread) != nil {
		return nil // nothing stored under the old id
	}
	d.wmu.Lock()
	for _, q := range []string{
		`UPDATE OR IGNORE messages SET chat=? WHERE chat=?`,
		`UPDATE OR IGNORE reactions SET chat=? WHERE chat=?`,
	} {
		d.Exec(q, into, from)
	}
	d.Exec(`DELETE FROM messages WHERE chat=?`, from)
	d.Exec(`DELETE FROM reactions WHERE chat=?`, from)
	d.Exec(`DELETE FROM chats WHERE jid=?`, from)
	d.wmu.Unlock()
	return d.TouchChat(into, lastTS, lastText, unread)
}

func (d *DB) SetGroupInfo(jid, name, parent string, isParent, defaultSub bool) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`INSERT INTO chats (jid,name,parent,is_parent,default_sub) VALUES (?,?,?,?,?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
			parent=excluded.parent, is_parent=excluded.is_parent, default_sub=excluded.default_sub`,
		jid, name, parent, isParent, defaultSub)
	return err
}

func (d *DB) ChatUnread(jid string) (unread int, mutedUntil int64) {
	d.QueryRow(`SELECT unread, muted_until FROM chats WHERE jid=?`, jid).Scan(&unread, &mutedUntil)
	return
}

func (d *DB) Chats() ([]*Chat, error) {
	// Community groups are kept even without messages so the Communities view can list them.
	rows, err := d.Query(`SELECT jid,name,last_ts,last_text,unread,muted_until,archived,pinned,parent,is_parent,default_sub
		FROM chats WHERE last_ts > 0 OR is_parent = 1 OR parent != '' ORDER BY last_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c := &Chat{}
		if err := rows.Scan(&c.JID, &c.Name, &c.LastTS, &c.LastText, &c.Unread, &c.MutedUntil, &c.Archived, &c.Pinned,
			&c.Parent, &c.IsParent, &c.DefaultSub); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// LatestIncoming returns the newest messages from others, used to send read receipts.
func (d *DB) LatestIncoming(chat string, n int) (ids []string, senders []string, err error) {
	rows, err := d.Query(`SELECT id, sender FROM messages WHERE chat=? AND from_me=0 ORDER BY ts DESC LIMIT ?`, chat, n)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, s string
		if err := rows.Scan(&id, &s); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		senders = append(senders, s)
	}
	return ids, senders, nil
}

// FillPreviews gives chats with no preview the text of their newest message.
func (d *DB) FillPreviews() error {
	rows, err := d.Query(`SELECT c.jid, m.kind, m.text, m.file_name FROM chats c
		JOIN messages m ON m.rowid = (SELECT rowid FROM messages WHERE chat = c.jid AND deleted = 0 ORDER BY ts DESC, id DESC LIMIT 1)
		WHERE c.last_text = ''`)
	if err != nil {
		return err
	}
	type fill struct{ jid, text string }
	var fills []fill
	for rows.Next() {
		var jid string
		m := &Msg{}
		if err := rows.Scan(&jid, &m.Kind, &m.Text, &m.FileName); err != nil {
			rows.Close()
			return err
		}
		fills = append(fills, fill{jid, preview(m)})
	}
	rows.Close()
	d.wmu.Lock()
	defer d.wmu.Unlock()
	for _, f := range fills {
		d.Exec(`UPDATE chats SET last_text=? WHERE jid=?`, f.text, f.jid)
	}
	return nil
}

func (d *DB) SetName(jid, name string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.Exec(`INSERT INTO names (jid,name) VALUES (?,?) ON CONFLICT(jid) DO UPDATE SET name=excluded.name`, jid, name)
	return err
}

func (d *DB) AllNames() (map[string]string, error) {
	rows, err := d.Query(`SELECT jid, name FROM names`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var j, n string
		if err := rows.Scan(&j, &n); err != nil {
			return nil, err
		}
		out[j] = n
	}
	return out, nil
}
