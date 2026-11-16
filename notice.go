package main

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// saveNotice stores group system events ("Ana joined", "Group renamed") that arrive in history
// as stubs with no message body. Community main chats contain little else.
func (a *App) saveNotice(evt *events.Message, web *waWeb.WebMessageInfo) bool {
	chat := a.chatJID(&evt.Info)
	if skipChat(chat) {
		return false
	}
	params := web.GetMessageStubParameters()
	people := func() string {
		var names []string
		for _, p := range params {
			if j, err := types.ParseJID(p); err == nil && j.User != "" {
				names = append(names, strings.TrimPrefix(a.displayName(j, ""), "~"))
			}
		}
		if len(names) == 0 {
			return "Someone"
		}
		return strings.Join(names, ", ")
	}
	var text string
	switch web.GetMessageStubType() {
	case waWeb.WebMessageInfo_GROUP_CREATE:
		text = "Group created"
	case waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT:
		text = "Group name changed"
		if len(params) > 0 {
			text += " to “" + params[0] + "”"
		}
	case waWeb.WebMessageInfo_GROUP_CHANGE_ICON:
		text = "Group icon changed"
	case waWeb.WebMessageInfo_GROUP_CHANGE_DESCRIPTION:
		text = "Group description changed"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE:
		text = people() + " joined"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE:
		text = people() + " left"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE:
		text = people() + " was removed"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_LINKED_GROUP_JOIN:
		text = people() + " joined from the community"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_CHANGE_NUMBER:
		text = people() + " changed their phone number"
	case waWeb.WebMessageInfo_COMMUNITY_LINK_SUB_GROUP:
		text = "A group was added to the community"
	default:
		return false
	}
	rec := &Msg{Chat: chat.String(), ID: evt.Info.ID, Sender: a.senderJID(&evt.Info).String(),
		FromMe: evt.Info.IsFromMe, TS: evt.Info.Timestamp.Unix(), Kind: "system", Text: text}
	if ok, _ := a.db.InsertMsg(rec); ok {
		a.db.TouchChat(rec.Chat, rec.TS, "", 0)
	}
	return true
}
