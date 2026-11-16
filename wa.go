package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type App struct {
	ctx        context.Context
	cli        *whatsmeow.Client
	container  *sqlstore.Container
	retries    map[string]chan *events.MediaRetry
	db         *DB
	hub        *Hub
	dataDir    string
	mediaDir   string
	addr       string
	token      string // the window's full-access key
	agentToken string // read, draft and propose only; written to the endpoint file
	props      *proposals
	claude     *claudeBridge
	win        *Window

	mu       sync.Mutex
	state    string
	qr       string
	openChat string
	focused  bool

	nameMu     sync.Mutex
	names      map[string]string
	contacts   map[types.JID]types.ContactInfo
	contactsAt time.Time

	memberMu    sync.Mutex
	memberCache map[string]memberList

	lastToast map[string]time.Time
	histAsked map[string]time.Time
}

func (a *App) setState(state, qr string) {
	a.mu.Lock()
	a.state, a.qr = state, qr
	a.mu.Unlock()
	a.hub.Send(map[string]any{"t": "state", "state": state, "qr": qr})
}

func (a *App) onEvent(raw any) {
	switch evt := raw.(type) {
	case *events.Message:
		a.handleMessage(evt, true)
	case *events.HistorySync:
		a.handleHistory(evt)
	case *events.Receipt:
		a.handleReceipt(evt)
	case *events.PushName:
		if evt.NewPushName != "" {
			a.setName(a.toPN(evt.JID).String(), evt.NewPushName)
		}
	case *events.Contact, *events.AppStateSyncComplete:
		// The address book arrives in app state sync, often after the chat list was first drawn.
		a.invalidateContacts()
		a.hub.Send(map[string]any{"t": "chats"})
	case *events.Connected:
		a.setState("online", "")
		a.mu.Lock()
		focused := a.focused
		a.mu.Unlock()
		go a.setAvailable(focused)
		go a.loadGroups()
		if a.cli.Store.ID != nil && !a.cli.Store.LID.IsEmpty() {
			a.db.MergeChat(a.cli.Store.LID.ToNonAD().String(), a.cli.Store.ID.ToNonAD().String())
		}
		go a.claude.onConnected()
		go a.resyncSettingsOnce()
	case *events.Disconnected:
		a.setState("offline", "")
	case *events.StreamReplaced:
		a.setState("replaced", "")
	case *events.LoggedOut:
		a.setState("loggedout", "")
	case *events.JoinedGroup:
		a.saveGroupInfo(&evt.GroupInfo)
		a.hub.Send(map[string]any{"t": "chats"})
	case *events.GroupInfo:
		if evt.Name != nil {
			a.setChatName(evt.JID, evt.Name.Name)
		}
		a.groupNotice(evt)
		if evt.Join != nil || evt.Leave != nil || evt.Promote != nil || evt.Demote != nil || evt.Announce != nil || evt.Ephemeral != nil {
			go a.refreshGroup(evt.JID)
		}
		if evt.Link != nil || evt.Unlink != nil { // a group moved into or out of a community
			go a.loadGroups()
		}
	case *events.MarkChatAsRead:
		jid := a.toPN(evt.JID).String()
		if evt.Action.GetRead() {
			a.db.SetChatField(jid, "unread", 0)
		} else if n, _ := a.db.ChatUnread(jid); n == 0 {
			a.db.SetChatField(jid, "unread", 1) // marked unread on the phone
		}
		a.hub.Send(map[string]any{"t": "chats"})
	case *events.ChatPresence:
		info := types.MessageInfo{MessageSource: evt.MessageSource}
		ev := map[string]any{"t": "typing", "chat": a.chatJID(&info).String(),
			"on": evt.State == types.ChatPresenceComposing, "audio": evt.Media == types.ChatPresenceMediaAudio}
		if evt.IsGroup {
			ev["name"] = strings.TrimPrefix(a.displayName(a.senderJID(&info), ""), "~")
		}
		a.hub.Send(ev)
	case *events.MediaRetry:
		a.mu.Lock()
		ch := a.retries[evt.MessageID]
		a.mu.Unlock()
		if ch != nil {
			select {
			case ch <- evt:
			default:
			}
		}
	case *events.Mute:
		until := int64(0)
		if evt.Action.GetMuted() {
			until = normTS(evt.Action.GetMuteEndTimestamp())
			if until <= 0 {
				until = math.MaxInt64
			}
		}
		a.db.SetChatField(a.toPN(evt.JID).String(), "muted_until", until)
		a.hub.Send(map[string]any{"t": "chats"})
	case *events.Archive:
		a.db.SetChatField(a.toPN(evt.JID).String(), "archived", evt.Action.GetArchived())
		a.hub.Send(map[string]any{"t": "chats"})
	case *events.Pin:
		a.db.SetChatField(a.toPN(evt.JID).String(), "pinned", evt.Action.GetPinned())
		a.hub.Send(map[string]any{"t": "chats"})
	}
}

// normTS accepts either seconds or milliseconds; WhatsApp uses both depending on the field.
func normTS[T int64 | uint64](v T) int64 {
	if uint64(v) > math.MaxInt64 {
		return math.MaxInt64
	}
	n := int64(v)
	if n > 1e12 {
		n /= 1000
	}
	return n
}

// toPN maps a LID (WhatsApp's hidden per-user id) to the phone-number JID when known,
// so one person doesn't show up as two chats.
func (a *App) toPN(jid types.JID) types.JID {
	jid = jid.ToNonAD()
	// Our own LID is rarely in the LID map, so check it directly or "You" splits into two chats.
	if jid.Server == types.HiddenUserServer && a.cli.Store.ID != nil && jid.User == a.cli.Store.LID.User {
		return a.cli.Store.ID.ToNonAD()
	}
	if jid.Server == types.HiddenUserServer && a.cli.Store.LIDs != nil {
		if pn, err := a.cli.Store.LIDs.GetPNForLID(a.ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

func (a *App) chatJID(info *types.MessageInfo) types.JID {
	chat := info.Chat.ToNonAD()
	if chat.Server == types.HiddenUserServer && !info.IsGroup {
		alt := info.SenderAlt
		if info.IsFromMe {
			alt = info.RecipientAlt
		}
		if alt.Server == types.DefaultUserServer {
			return alt.ToNonAD()
		}
	}
	return a.toPN(chat)
}

func (a *App) senderJID(info *types.MessageInfo) types.JID {
	if info.IsFromMe && a.cli.Store.ID != nil {
		return a.cli.Store.ID.ToNonAD()
	}
	s := info.Sender.ToNonAD()
	if s.Server == types.HiddenUserServer && info.SenderAlt.Server == types.DefaultUserServer {
		return info.SenderAlt.ToNonAD()
	}
	return a.toPN(s)
}

func skipChat(j types.JID) bool {
	return j.Server == types.BroadcastServer || j.Server == types.NewsletterServer || j.IsEmpty()
}

func (a *App) handleMessage(evt *events.Message, live bool) {
	chat := a.chatJID(&evt.Info)
	if skipChat(chat) {
		return
	}
	chatS := chat.String()
	m := evt.Message
	sender := a.senderJID(&evt.Info)
	if evt.Info.PushName != "" && !evt.Info.IsFromMe {
		a.setName(sender.String(), evt.Info.PushName)
	}

	if r := m.GetReactionMessage(); r != nil {
		who := sender.String()
		if evt.Info.IsFromMe {
			who = selfMarker
		}
		a.db.SetReaction(chatS, r.GetKey().GetID(), who, r.GetText())
		a.pushMsg(chatS, r.GetKey().GetID())
		return
	}
	if pu := m.GetPollUpdateMessage(); pu != nil {
		pollID := pu.GetPollCreationMessageKey().GetID()
		vote, err := a.cli.DecryptPollVote(a.ctx, evt)
		if err != nil {
			logf("poll vote in %s: %v", chatS, err)
			return
		}
		who := sender.String()
		if evt.Info.IsFromMe {
			who = selfMarker
		}
		a.db.SetVote(chatS, pollID, who, hexes(vote.GetSelectedOptions()))
		a.pushMsg(chatS, pollID)
		return
	}
	if p := m.GetProtocolMessage(); p != nil {
		id := p.GetKey().GetID()
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			a.db.MarkDeleted(chatS, id)
			a.db.RefreshPreview(chatS)
			a.pushMsg(chatS, id)
			a.hub.Send(map[string]any{"t": "chats"})
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			_, text, _ := describe(p.GetEditedMessage())
			a.db.EditText(chatS, id, text)
			a.db.RefreshPreview(chatS)
			a.pushMsg(chatS, id)
			a.hub.Send(map[string]any{"t": "chats"})
		case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			exp := p.GetEphemeralExpiration()
			a.db.SetChatField(chatS, "ephemeral", exp)
			a.addNotice(chatS, evt.Info.ID, evt.Info.Timestamp.Unix(), ephemeralText(exp))
		}
		return
	}

	rec := buildMsg(chatS, evt.Info.ID, sender.String(), evt.Info.IsFromMe, evt.Info.Timestamp.Unix(), m)
	if rec == nil {
		return
	}
	// A message with a timer means the chat has disappearing messages on, even if we missed the switch.
	if rec.ExpiresAt > 0 && live && a.db.ChatInt(chatS, "ephemeral") == 0 {
		a.db.SetChatField(chatS, "ephemeral", rec.ExpiresAt-rec.TS)
	}
	inserted, err := a.db.InsertMsg(rec)
	if err != nil {
		logf("insert %s: %v", rec.ID, err)
		return
	}
	if !inserted {
		return
	}

	addUnread := 0
	if live && !rec.FromMe {
		a.mu.Lock()
		viewing := a.openChat == chatS && a.focused
		a.mu.Unlock()
		if viewing {
			go a.sendRead(chat, []string{rec.ID}, []string{rec.Sender})
		} else {
			addUnread = 1
		}
	}
	a.db.TouchChat(chatS, rec.TS, withNames(preview(rec), a.mentionNames(rec.Raw)), addUnread)

	if !live {
		return
	}
	a.claude.observe(rec)
	a.pushMsg(chatS, rec.ID)
	a.hub.Send(map[string]any{"t": "chats"})
	// Messages queued while the PC slept arrive as live ones; only fresh ones get a notification.
	if addUnread > 0 && time.Since(evt.Info.Timestamp) < 2*time.Minute {
		a.notify(chat, rec)
	}
}

func (a *App) pushMsg(chat, id string) {
	m, err := a.db.GetMsg(chat, id)
	if err != nil {
		return
	}
	a.db.attachReactions(chat, []*Msg{m})
	a.decorate(m)
	a.hub.Send(map[string]any{"t": "msg", "chat": chat, "msg": m})
}

func (a *App) decorate(m *Msg) {
	if !m.FromMe && strings.HasSuffix(m.Chat, "@"+types.GroupServer) {
		if j, err := types.ParseJID(m.Sender); err == nil {
			m.SenderName = a.displayName(j, "")
		}
	}
	m.Mentions = a.mentionNames(m.Raw)
	if m.QuoteFrom != "" {
		if j, err := types.ParseJID(m.QuoteFrom); err == nil {
			j = a.toPN(j.ToNonAD())
			if a.cli.Store.ID != nil && j.User == a.cli.Store.ID.User {
				m.QuoteName = "You"
			} else {
				m.QuoteName = strings.TrimPrefix(a.displayName(j, ""), "~")
			}
		}
	}
	if m.Kind == "poll" {
		m.Poll = a.pollOptions(m)
	}
}

// pollOptions counts the votes for each option of a poll message.
func (a *App) pollOptions(m *Msg) []PollOption {
	var msg waE2E.Message
	if proto.Unmarshal(m.Raw, &msg) != nil {
		return nil
	}
	poll := pollOf(&msg)
	if poll == nil {
		return nil
	}
	votes := a.db.Votes(m.Chat, m.ID)
	var out []PollOption
	for _, o := range poll.GetOptions() {
		sum := sha256.Sum256([]byte(o.GetOptionName()))
		h := hex.EncodeToString(sum[:])
		opt := PollOption{Name: o.GetOptionName()}
		for voter, chosen := range votes {
			for _, c := range chosen {
				if c == h {
					opt.Count++
					opt.Mine = opt.Mine || voter == selfMarker
				}
			}
		}
		out = append(out, opt)
	}
	return out
}

func pollOf(m *waE2E.Message) *waE2E.PollCreationMessage {
	m = unwrapChild(m)
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessageV3(), m.GetPollCreationMessage(),
		m.GetPollCreationMessageV2(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func hexes(list [][]byte) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = hex.EncodeToString(b)
	}
	return out
}

// keyAuthor turns the key of a reaction or vote stored in history into the sender value live ones use.
func (a *App) keyAuthor(chat types.JID, key *waCommon.MessageKey) string {
	if key.GetFromMe() {
		return selfMarker
	}
	who := key.GetParticipant()
	if who == "" {
		who = key.GetRemoteJID()
	}
	j, err := types.ParseJID(who)
	if err != nil || j.User == "" {
		return chat.String()
	}
	return a.toPN(j.ToNonAD()).String()
}

// addNotice stores a grey system line in a chat, like "Anna joined".
func (a *App) addNotice(chat, id string, ts int64, text string) {
	rec := &Msg{Chat: chat, ID: id, Sender: chat, TS: ts, Kind: "system", Text: text}
	if ok, _ := a.db.InsertMsg(rec); ok {
		a.db.TouchChat(chat, ts, "", 0)
		a.pushMsg(chat, id)
	}
}

func ephemeralText(seconds uint32) string {
	if seconds == 0 {
		return "Disappearing messages turned off"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "Disappearing messages turned on: 24 hours"
		}
		return fmt.Sprintf("Disappearing messages turned on: %d days", days)
	default:
		return fmt.Sprintf("Disappearing messages turned on: %d hours", int(d.Hours()))
	}
}

// groupNotice shows group changes that happen while the app runs; history brings its own.
func (a *App) groupNotice(evt *events.GroupInfo) {
	chat := evt.JID.String()
	by := ""
	if evt.Sender != nil {
		s := *evt.Sender
		if evt.SenderPN != nil && !evt.SenderPN.IsEmpty() {
			s = *evt.SenderPN
		}
		by = strings.TrimPrefix(a.displayName(a.toPN(s.ToNonAD()), ""), "~")
	}
	names := func(list []types.JID) string {
		var out []string
		for _, j := range list {
			out = append(out, strings.TrimPrefix(a.displayName(a.toPN(j.ToNonAD()), ""), "~"))
		}
		return strings.Join(out, ", ")
	}
	var lines []string
	if len(evt.Join) > 0 {
		if by != "" && !(len(evt.Join) == 1 && evt.Sender != nil && evt.Join[0].User == evt.Sender.User) && evt.JoinReason != "invite" {
			lines = append(lines, by+" added "+names(evt.Join))
		} else {
			lines = append(lines, names(evt.Join)+" joined")
		}
	}
	if len(evt.Leave) > 0 {
		if by != "" && !(len(evt.Leave) == 1 && evt.Sender != nil && evt.Leave[0].User == evt.Sender.User) {
			lines = append(lines, by+" removed "+names(evt.Leave))
		} else {
			lines = append(lines, names(evt.Leave)+" left")
		}
	}
	if evt.Name != nil {
		lines = append(lines, strings.TrimSpace(by+" changed the group name to “"+evt.Name.Name+"”"))
	}
	if evt.Topic != nil {
		lines = append(lines, strings.TrimSpace(by+" changed the group description"))
	}
	if evt.Announce != nil {
		if evt.Announce.IsAnnounce {
			lines = append(lines, "Only admins can send messages now")
		} else {
			lines = append(lines, "Everyone can send messages now")
		}
	}
	if evt.Ephemeral != nil {
		exp := uint32(0)
		if evt.Ephemeral.IsEphemeral {
			exp = evt.Ephemeral.DisappearingTimer
		}
		lines = append(lines, ephemeralText(exp))
	}
	ts := evt.Timestamp.Unix()
	for i, text := range lines {
		sum := sha256.Sum256([]byte(fmt.Sprint(chat, ts, i, text)))
		a.addNotice(chat, "notice-"+hex.EncodeToString(sum[:8]), ts, text)
	}
	if len(lines) > 0 {
		a.hub.Send(map[string]any{"t": "chats"})
	}
}

func (a *App) refreshGroup(jid types.JID) {
	if g, err := a.cli.GetGroupInfo(a.ctx, jid); err == nil {
		a.saveGroupInfo(g)
		a.hub.Send(map[string]any{"t": "chats"})
	}
}

// setAvailable mirrors WhatsApp Web: online while the window is in front, which also brings typing
// indicators; offline otherwise, so the phone keeps getting notifications.
func (a *App) setAvailable(on bool) {
	if !a.cli.IsConnected() || a.cli.Store.PushName == "" {
		return
	}
	p := types.PresenceUnavailable
	if on {
		p = types.PresenceAvailable
	}
	if err := a.cli.SendPresence(a.ctx, p); err != nil {
		logf("presence: %v", err)
	}
}

func (a *App) handleHistory(evt *events.HistorySync) {
	convs := evt.Data.GetConversations()
	onDemand := evt.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND
	var total, kept, unparsable int
	skipped := map[string]int{}
	for _, c := range convs {
		jid, err := types.ParseJID(c.GetID())
		if err != nil {
			continue
		}
		if jid.Server == types.HiddenUserServer && c.GetPnJID() != "" {
			if pn, err := types.ParseJID(c.GetPnJID()); err == nil {
				jid = pn
			}
		}
		norm := a.toPN(jid)
		if skipChat(norm) {
			continue
		}
		hms := c.GetMessages()
		// Store oldest first, so messages from the same second keep their real order.
		sort.SliceStable(hms, func(i, j int) bool {
			return hms[i].GetMessage().GetMessageTimestamp() < hms[j].GetMessage().GetMessageTimestamp()
		})
		for _, hm := range hms {
			total++
			msgEvt, err := a.cli.ParseWebMessage(jid, hm.GetMessage())
			if err != nil {
				unparsable++
				continue
			}
			if k, _, _ := describe(msgEvt.Message); k != "" {
				kept++
			} else if a.saveNotice(msgEvt, hm.GetMessage()) {
				kept++
				continue
			} else if s := hm.GetMessage().GetMessageStubType(); s != 0 {
				skipped[s.String()]++
			} else {
				skipped["other"]++
			}
			a.handleMessage(msgEvt, false)
			if web := hm.GetMessage(); len(web.GetReactions()) > 0 || len(web.GetPollUpdates()) > 0 {
				chat := a.chatJID(&msgEvt.Info)
				for _, r := range web.GetReactions() {
					a.db.SetReaction(chat.String(), msgEvt.Info.ID, a.keyAuthor(chat, r.GetKey()), r.GetText())
				}
				for _, u := range web.GetPollUpdates() {
					a.db.SetVote(chat.String(), msgEvt.Info.ID, a.keyAuthor(chat, u.GetPollUpdateMessageKey()),
						hexes(u.GetVote().GetSelectedOptions()))
				}
			}
			if msgEvt.Info.IsFromMe {
				status := 0
				switch s := hm.GetMessage().GetStatus(); {
				case s >= waWeb.WebMessageInfo_READ:
					status = 2
				case s == waWeb.WebMessageInfo_DELIVERY_ACK:
					status = 1
				}
				if status > 0 {
					a.db.RaiseStatus(a.chatJID(&msgEvt.Info).String(), msgEvt.Info.ID, status)
				}
			}
		}
		if onDemand {
			// These replies carry only messages; their empty chat settings must not overwrite real ones.
			a.hub.Send(map[string]any{"t": "history", "chat": norm.String()})
			continue
		}
		name := c.GetName()
		if name == "" {
			name = c.GetDisplayName()
		}
		muted := int64(0)
		if c.GetMuteEndTime() > 0 {
			muted = normTS(c.GetMuteEndTime())
		}
		a.db.SetChatMeta(norm.String(), name, muted, c.GetArchived(), c.GetPinned() > 0, int(c.GetUnreadCount()))
		if norm.Server != types.GroupServer {
			a.db.SetChatField(norm.String(), "ephemeral", c.GetEphemeralExpiration())
		}
		if ts := c.GetConversationTimestamp(); ts > 0 {
			a.db.TouchChat(norm.String(), normTS(ts), "", 0)
		}
	}
	logf("history sync (%s): %d conversations, %d messages, %d shown, %d unparsable, skipped %v",
		evt.Data.GetSyncType(), len(convs), total, kept, unparsable, skipped)
	a.db.FillPreviews()
	a.invalidateContacts()
	a.hub.Send(map[string]any{"t": "chats"})
}

func (a *App) handleReceipt(evt *events.Receipt) {
	info := types.MessageInfo{MessageSource: evt.MessageSource}
	chat := a.chatJID(&info).String()
	if evt.Type == types.ReceiptTypeReadSelf {
		// Read on another device (usually the phone).
		a.db.SetChatField(chat, "unread", 0)
		a.hub.Send(map[string]any{"t": "chats"})
		return
	}
	if evt.IsFromMe {
		return
	}
	status := 0
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		status = 1
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
		status = 2
	default:
		return
	}
	reader := a.toPN(evt.Sender.ToNonAD()).String()
	members := int(a.db.ChatInt(chat, "members"))
	for _, id := range evt.MessageIDs {
		s := status
		// In a group the ticks turn blue only once everyone else has read it.
		if s == 2 && evt.IsGroup && (members < 2 || a.db.AddReader(chat, id, reader) < members-1) {
			s = 1
		}
		if changed, _ := a.db.RaiseStatus(chat, id, s); changed {
			a.pushMsg(chat, id)
		}
	}
}

func (a *App) sendRead(chat types.JID, ids, senders []string) {
	if len(ids) == 0 {
		return
	}
	var extra []types.ReceiptType
	// Respect the account's "read receipts off" setting: tell our own devices only.
	if a.cli.GetPrivacySettings(a.ctx).ReadReceipts == types.PrivacySettingNone {
		extra = append(extra, types.ReceiptTypeReadSelf)
	}
	bySender := map[string][]types.MessageID{}
	for i, id := range ids {
		bySender[senders[i]] = append(bySender[senders[i]], id)
	}
	for s, group := range bySender {
		sender := types.EmptyJID
		if chat.Server == types.GroupServer {
			sender, _ = types.ParseJID(s)
		}
		if err := a.cli.MarkRead(a.ctx, group, time.Now(), chat, sender, extra...); err != nil {
			logf("mark read %s: %v", chat, err)
		}
	}
}

// resyncSettingsOnce refetches mutes, pins and archives from the phone. Early versions let the history
// import blank them out, so installs from before that fix need one full pass.
func (a *App) resyncSettingsOnce() {
	marker := filepath.Join(a.dataDir, "settings-resynced")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	for _, p := range []appstate.WAPatchName{appstate.WAPatchRegularHigh, appstate.WAPatchRegular, appstate.WAPatchRegularLow} {
		if err := a.cli.FetchAppState(a.ctx, p, true, false); err != nil {
			logf("settings resync %s: %v", p, err)
			return
		}
	}
	os.WriteFile(marker, nil, 0o644)
	logf("settings resynced")
	a.hub.Send(map[string]any{"t": "chats"})
}

func (a *App) loadGroups() {
	groups, err := a.cli.GetJoinedGroups(a.ctx)
	if err != nil {
		logf("groups: %v", err)
		return
	}
	known := map[types.JID]bool{}
	for _, g := range groups {
		a.saveGroupInfo(g)
		known[g.JID] = true
	}
	// A community you belong to through one of its groups isn't always in the joined list; fetch its name.
	for _, g := range groups {
		if p := g.LinkedParentJID; !p.IsEmpty() && !known[p] {
			known[p] = true
			if info, err := a.cli.GetGroupInfo(a.ctx, p); err == nil {
				a.saveGroupInfo(info)
			} else {
				a.db.SetGroupInfo(p.String(), "", "", true, false)
			}
		}
	}
	a.hub.Send(map[string]any{"t": "chats"})
}

func (a *App) saveGroupInfo(g *types.GroupInfo) {
	parent := ""
	if !g.LinkedParentJID.IsEmpty() {
		parent = g.LinkedParentJID.String()
	}
	a.db.SetGroupInfo(g.JID.String(), g.Name, parent, g.IsParent, g.IsDefaultSubGroup)
	admin := false
	if id := a.cli.Store.ID; id != nil {
		for _, p := range g.Participants {
			if p.JID.User == id.User || p.PhoneNumber.User == id.User || (!a.cli.Store.LID.IsEmpty() && p.LID.User == a.cli.Store.LID.User) {
				admin = p.IsAdmin || p.IsSuperAdmin
			}
		}
	}
	exp := uint32(0)
	if g.IsEphemeral {
		exp = g.DisappearingTimer
	}
	a.db.SetGroupState(g.JID.String(), len(g.Participants), g.IsAnnounce, admin, exp)
}

func (a *App) setChatName(jid types.JID, name string) {
	if name == "" {
		return
	}
	a.db.SetChatField(jid.ToNonAD().String(), "name", name)
	a.hub.Send(map[string]any{"t": "chats"})
}

func (a *App) setName(jid, name string) {
	a.nameMu.Lock()
	if a.names[jid] == name {
		a.nameMu.Unlock()
		return
	}
	a.names[jid] = name
	a.nameMu.Unlock()
	a.db.SetName(jid, name)
}

func (a *App) invalidateContacts() {
	a.nameMu.Lock()
	a.contacts = nil
	a.nameMu.Unlock()
}

func (a *App) contact(jid types.JID) (types.ContactInfo, bool) {
	a.nameMu.Lock()
	defer a.nameMu.Unlock()
	if a.cli.Store.Contacts == nil { // not linked yet
		return types.ContactInfo{}, false
	}
	ttl := time.Minute
	if len(a.contacts) == 0 {
		ttl = 5 * time.Second // still syncing; don't hold on to an empty address book
	}
	if a.contacts == nil || time.Since(a.contactsAt) > ttl {
		all, err := a.cli.Store.Contacts.GetAllContacts(a.ctx)
		if err != nil {
			logf("contacts: %v", err)
		} else {
			a.contacts, a.contactsAt = all, time.Now()
		}
	}
	c, ok := a.contacts[jid]
	return c, ok
}

// displayName picks the most human name available: address book, then group/chat name,
// then the name the person set for themselves, then the phone number.
func (a *App) displayName(jid types.JID, chatName string) string {
	if jid.Server == types.GroupServer {
		if chatName != "" {
			return chatName
		}
		return "Group"
	}
	jid = a.toPN(jid)
	if a.cli.Store.ID != nil && jid.User == a.cli.Store.ID.User {
		return "You"
	}
	c, ok := a.contact(jid)
	if ok && c.FullName != "" {
		return c.FullName
	}
	if ok && c.FirstName != "" {
		return c.FirstName
	}
	if chatName != "" {
		return chatName
	}
	a.nameMu.Lock()
	n := a.names[jid.String()]
	a.nameMu.Unlock()
	if n != "" {
		return "~" + n
	}
	if ok && c.PushName != "" {
		return "~" + c.PushName
	}
	if ok && c.BusinessName != "" {
		return c.BusinessName
	}
	// Newer contacts often carry their chosen name only under their LID entry.
	if jid.Server == types.DefaultUserServer && a.cli.Store.LIDs != nil {
		if lid, err := a.cli.Store.LIDs.GetLIDForPN(a.ctx, jid); err == nil && !lid.IsEmpty() {
			if lc, ok := a.contact(lid.ToNonAD()); ok && lc.PushName != "" {
				return "~" + lc.PushName
			}
		}
	}
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	return jid.User
}

func buildMsg(chat, id, sender string, fromMe bool, ts int64, m *waE2E.Message) *Msg {
	kind, text, ci := describe(m)
	if kind == "" {
		return nil
	}
	raw, _ := proto.Marshal(m)
	rec := &Msg{Chat: chat, ID: id, Sender: sender, FromMe: fromMe, TS: ts, Kind: kind, Text: text, Raw: raw}
	m = unwrapChild(m)
	switch {
	case m.GetPtvMessage() != nil:
		x := m.GetPtvMessage()
		rec.Mime, rec.Thumb, rec.W, rec.H = x.GetMimetype(), x.GetJPEGThumbnail(), int(x.GetWidth()), int(x.GetHeight())
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		rec.Mime, rec.Thumb, rec.W, rec.H = x.GetMimetype(), x.GetJPEGThumbnail(), int(x.GetWidth()), int(x.GetHeight())
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		rec.Mime, rec.Thumb, rec.W, rec.H = x.GetMimetype(), x.GetJPEGThumbnail(), int(x.GetWidth()), int(x.GetHeight())
	case m.GetAudioMessage() != nil:
		rec.Mime = m.GetAudioMessage().GetMimetype()
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		rec.Mime, rec.FileName, rec.Thumb = x.GetMimetype(), x.GetFileName(), x.GetJPEGThumbnail()
		if rec.FileName == "" {
			rec.FileName = x.GetTitle()
		}
	case m.GetStickerMessage() != nil:
		rec.Mime = m.GetStickerMessage().GetMimetype()
	}
	if exp := ci.GetExpiration(); exp > 0 {
		rec.ExpiresAt = ts + int64(exp)
	}
	if ci != nil && ci.GetStanzaID() != "" {
		rec.QuoteID = ci.GetStanzaID()
		rec.QuoteFrom = ci.GetParticipant()
		if q := ci.GetQuotedMessage(); q != nil {
			k, t, _ := describe(q)
			rec.QuoteText = preview(&Msg{Kind: k, Text: t})
		}
	}
	return rec
}

// unwrapChild opens the wrapper WhatsApp puts around each photo of an album.
func unwrapChild(m *waE2E.Message) *waE2E.Message {
	if c := m.GetAssociatedChildMessage().GetMessage(); c != nil {
		return c
	}
	return m
}

// notContent are the parts of a message that carry no visible content on their own:
// keys, receipts, votes, pins, wrappers whatsmeow already opened, and similar bookkeeping.
var notContent = map[protoreflect.Name]bool{
	"senderKeyDistributionMessage": true, "fastRatchetKeySenderKeyDistributionMessage": true,
	"messageContextInfo": true, "protocolMessage": true, "reactionMessage": true, "encReactionMessage": true,
	"pollUpdateMessage": true, "keepInChatMessage": true, "pinInChatMessage": true, "call": true, "chat": true,
	"deviceSentMessage": true, "viewOnceMessage": true, "viewOnceMessageV2": true, "viewOnceMessageV2Extension": true,
	"ephemeralMessage": true, "documentWithCaptionMessage": true, "editedMessage": true, "stickerSyncRmrMessage": true,
	"messageHistoryBundle": true, "messageHistoryNotice": true, "encCommentMessage": true, "encEventResponseMessage": true,
	"placeholderMessage": true, "secretEncryptedMessage": true, "albumMessage": true, "groupRootKeyShare": true,
	"rootSecretDistributeMessage": true, "statusMentionMessage": true, "groupStatusMentionMessage": true,
	"splitPaymentUpdateMessage": true, "acp2SettingMessage": true, "limitSharingMessage": true,
	"requestLocationUpdateMessage": true, "eventCoverImage": true, "statusNotificationMessage": true,
}

func hasUnknownContent(m *waE2E.Message) bool {
	found := false
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		found = !notContent[fd.Name()]
		return !found
	})
	return found
}

func describe(m *waE2E.Message) (kind, text string, ci *waE2E.ContextInfo) {
	m = unwrapChild(m)
	switch {
	case m == nil:
		return "", "", nil
	case m.GetConversation() != "":
		return "text", m.GetConversation(), nil
	case m.GetExtendedTextMessage() != nil:
		x := m.GetExtendedTextMessage()
		return "text", x.GetText(), x.GetContextInfo()
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		return "image", x.GetCaption(), x.GetContextInfo()
	case m.GetPtvMessage() != nil:
		x := m.GetPtvMessage()
		return "video", x.GetCaption(), x.GetContextInfo()
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		if x.GetGifPlayback() {
			return "gif", x.GetCaption(), x.GetContextInfo()
		}
		return "video", x.GetCaption(), x.GetContextInfo()
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		if x.GetPTT() {
			return "voice", "", x.GetContextInfo()
		}
		return "audio", "", x.GetContextInfo()
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		return "document", x.GetCaption(), x.GetContextInfo()
	case m.GetStickerMessage() != nil:
		return "sticker", "", m.GetStickerMessage().GetContextInfo()
	case m.GetContactMessage() != nil:
		return "contact", m.GetContactMessage().GetDisplayName(), nil
	case m.GetContactsArrayMessage() != nil:
		return "contact", m.GetContactsArrayMessage().GetDisplayName(), nil
	case m.GetLocationMessage() != nil:
		x := m.GetLocationMessage()
		return "location", fmt.Sprintf("%.6f,%.6f %s", x.GetDegreesLatitude(), x.GetDegreesLongitude(), x.GetName()), x.GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		x := m.GetLiveLocationMessage()
		return "location", fmt.Sprintf("%.6f,%.6f Live location", x.GetDegreesLatitude(), x.GetDegreesLongitude()), nil
	}
	if x := m.GetEventMessage(); x != nil {
		text := "Event: " + x.GetName()
		if t := x.GetStartTime(); t > 0 {
			text += "\n" + time.Unix(t, 0).Local().Format("Mon 2 Jan 2006, 15:04")
		}
		if x.GetIsCanceled() {
			text += " (cancelled)"
		}
		if d := strings.TrimSpace(x.GetDescription()); d != "" {
			text += "\n" + d
		}
		return "event", text, x.GetContextInfo()
	}
	if x := m.GetGroupInviteMessage(); x != nil {
		text := "Group invite: " + x.GetGroupName()
		if c := strings.TrimSpace(x.GetCaption()); c != "" {
			text += "\n" + c
		}
		return "invite", text, x.GetContextInfo()
	}
	var poll *waE2E.PollCreationMessage
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessageV3(), m.GetPollCreationMessage(),
		m.GetPollCreationMessageV2(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			poll = p
			break
		}
	}
	if poll != nil {
		var b strings.Builder
		b.WriteString(poll.GetName())
		for _, o := range poll.GetOptions() {
			b.WriteString("\n• " + o.GetOptionName())
		}
		return "poll", b.String(), poll.GetContextInfo()
	}
	// Anything else with content still gets a bubble, so nothing arrives silently.
	if hasUnknownContent(m) {
		return "unsupported", "This message can't be shown here yet. Open it on your phone.", nil
	}
	return "", "", nil
}

func preview(m *Msg) string {
	label := map[string]string{
		"image": "Photo", "video": "Video", "gif": "GIF", "audio": "Audio", "voice": "Voice message",
		"document": "Document", "sticker": "Sticker", "contact": "Contact", "location": "Location", "poll": "Poll",
		"unsupported": "Message",
	}[m.Kind]
	text := strings.TrimSpace(m.Text)
	if m.Kind == "document" && m.FileName != "" {
		label = m.FileName
	}
	if m.Kind == "poll" {
		text, _, _ = strings.Cut(text, "\n")
	}
	switch {
	case label == "":
		return text
	case text == "" || m.Kind == "location":
		return label
	default:
		return label + ": " + text
	}
}
