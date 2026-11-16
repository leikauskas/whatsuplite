package main

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// WhatsApp carries a mention as "@<number>" in the text plus the person's JID in ContextInfo.
// Incoming numbers are swapped for names; an outgoing "@Name" is swapped back for the number.

type member struct {
	JID   string `json:"jid"`
	Name  string `json:"name"`
	phone string
}

// mention is who an "@<number>" points at; JID is their phone-number JID when known, for opening a chat.
type mention struct {
	Name  string `json:"name"`
	JID   string `json:"jid"`
	Phone string `json:"phone,omitempty"`
}

type memberList struct {
	at   time.Time
	list []member
}

var mentionRe = regexp.MustCompile(`@(\d+)`)

func (a *App) members(chat types.JID) []member {
	if chat.Server != types.GroupServer || a.cli.Store.ID == nil {
		return nil
	}
	key := chat.String()
	a.memberMu.Lock()
	cached, ok := a.memberCache[key]
	a.memberMu.Unlock()
	if ok && time.Since(cached.at) < 10*time.Minute {
		return cached.list
	}
	info, err := a.cli.GetGroupInfo(a.ctx, chat)
	if err != nil {
		logf("group members %s: %v", key, err)
		return cached.list
	}
	list := make([]member, 0, len(info.Participants))
	for _, p := range info.Participants {
		if a.toPN(p.JID).User == a.cli.Store.ID.User {
			continue
		}
		list = append(list, member{
			JID:   p.JID.ToNonAD().String(),
			Name:  strings.TrimPrefix(a.displayName(p.JID, ""), "~"),
			phone: p.PhoneNumber.User,
		})
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	a.memberMu.Lock()
	if a.memberCache == nil {
		a.memberCache = map[string]memberList{}
	}
	a.memberCache[key] = memberList{at: time.Now(), list: list}
	a.memberMu.Unlock()
	return list
}

func (a *App) apiMembers(w http.ResponseWriter, r *http.Request) {
	chat, err := types.ParseJID(r.URL.Query().Get("chat"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	list := a.members(chat)
	if list == nil {
		list = []member{}
	}
	writeJSON(w, list)
}

// mentionNames maps the number in each "@<number>" of a stored message to that person.
func (a *App) mentionNames(raw []byte) map[string]mention {
	if len(raw) == 0 {
		return nil
	}
	var m waE2E.Message
	if proto.Unmarshal(raw, &m) != nil {
		return nil
	}
	_, _, ci := describe(&m)
	var names map[string]mention
	for _, s := range ci.GetMentionedJID() {
		j, err := types.ParseJID(s)
		if err != nil {
			continue
		}
		if names == nil {
			names = map[string]mention{}
		}
		pn := a.toPN(j).ToNonAD()
		mn := mention{Name: strings.TrimPrefix(a.displayName(j, ""), "~"), JID: pn.String()}
		if pn.Server == types.DefaultUserServer {
			mn.Phone = "+" + pn.User
		}
		names[j.User] = mn
	}
	return names
}

func withNames(text string, names map[string]mention) string {
	if len(names) == 0 {
		return text
	}
	return mentionRe.ReplaceAllStringFunc(text, func(t string) string {
		if n, ok := names[t[1:]]; ok {
			return "@" + n.Name
		}
		return t
	})
}

// mentionize rewrites "@Name" (or a typed "@<phone>") for members of a group into the form
// WhatsApp expects and returns the JIDs to list as mentioned.
func (a *App) mentionize(chat types.JID, text string) (string, []string) {
	if chat.Server != types.GroupServer || !strings.Contains(text, "@") {
		return text, nil
	}
	list := append([]member(nil), a.members(chat)...)
	// Longest names first, so "@Ann Lee" wins over "@Ann".
	sort.SliceStable(list, func(i, j int) bool { return len(list[i].Name) > len(list[j].Name) })
	var jids []string
	seen := map[string]bool{}
	add := func(jid string) {
		if !seen[jid] {
			seen[jid] = true
			jids = append(jids, jid)
		}
	}
	for _, mb := range list {
		j, err := types.ParseJID(mb.JID)
		if err != nil || mb.Name == "" {
			continue
		}
		if out, hit := replaceTag(text, "@"+mb.Name, "@"+j.User); hit {
			text = out
			add(mb.JID)
		}
	}
	text = mentionRe.ReplaceAllStringFunc(text, func(t string) string {
		num := t[1:]
		for _, mb := range list {
			j, _ := types.ParseJID(mb.JID)
			if num == j.User || (mb.phone != "" && num == mb.phone) {
				add(mb.JID)
				return "@" + j.User
			}
		}
		return t
	})
	return text, jids
}

// replaceTag replaces tag only where it ends a word, so "@Tom" leaves "@Tommy" alone.
func replaceTag(text, tag, repl string) (string, bool) {
	var b strings.Builder
	hit := false
	for {
		i := strings.Index(text, tag)
		if i < 0 {
			b.WriteString(text)
			return b.String(), hit
		}
		end := i + len(tag)
		next, _ := utf8.DecodeRuneInString(text[end:])
		b.WriteString(text[:i])
		if end < len(text) && (unicode.IsLetter(next) || unicode.IsDigit(next)) {
			b.WriteString(tag)
		} else {
			b.WriteString(repl)
			hit = true
		}
		text = text[end:]
	}
}

// outgoing prepares the text and ContextInfo (quote and mentions) for a message we send.
func (a *App) outgoing(chat types.JID, text, quoteID string) (string, *waE2E.ContextInfo) {
	text, mentioned := a.mentionize(chat, text)
	ci := a.quoteContext(chat, quoteID)
	if len(mentioned) > 0 {
		if ci == nil {
			ci = &waE2E.ContextInfo{}
		}
		ci.MentionedJID = mentioned
	}
	return text, ci
}
