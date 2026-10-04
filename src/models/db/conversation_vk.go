package db_models

import (
	"strconv"
	"strings"

	"ovk-im/src/db"

	"gorm.io/gorm"
)

func ResolveGlobalMsgID(tx *gorm.DB, chatID string, localID uint64, lastCMID uint64, lastMsgID uint64) uint64 {
	if localID == 0 {
		return 0
	}
	if lastCMID > 0 && localID >= lastCMID && lastMsgID > 0 {
		return lastMsgID
	}
	var msgID uint64
	dbRef := tx
	if dbRef == nil {
		dbRef = db.Instance
	}
	if dbRef != nil {
		dbRef.Table("messages").Select("id").Where("chat_id = ? AND local_id = ?", chatID, localID).Scan(&msgID)
		if msgID == 0 {
			dbRef.Table("messages").Select("id").Where("chat_id = ? AND local_id <= ?", chatID, localID).Order("local_id DESC").Limit(1).Scan(&msgID)
		}
	}
	if msgID == 0 {
		return localID
	}
	return msgID
}

// VKApiConversation represents the VK API conversation object.
type VKApiConversation struct {
	Peer                      VKApiPeer       `json:"peer"`
	InRead                    uint64          `json:"in_read"`
	OutRead                   uint64          `json:"out_read"`
	InReadCmid                uint64          `json:"in_read_cmid,omitempty"`
	OutReadCmid               uint64          `json:"out_read_cmid,omitempty"`
	UnreadCount               int             `json:"unread_count"`
	Important                 bool            `json:"important,omitempty"`
	Unanswered                bool            `json:"unanswered,omitempty"`
	PushSettings              *VKPushSettings `json:"push_settings,omitempty"`
	CanWrite                  VKCanWrite      `json:"can_write"`
	ChatSettings              *VKChatSettings `json:"chat_settings,omitempty"`
	LastMessageID             uint64          `json:"last_message_id,omitempty"`
	LastConversationMessageID uint64          `json:"last_conversation_message_id,omitempty"`
}

type VKApiPeer struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	LocalID int64  `json:"local_id"`
}

type VKPushSettings struct {
	DisabledUntil   int64 `json:"disabled_until"`
	DisabledForever bool  `json:"disabled_forever,omitempty"`
	NoSound         bool  `json:"no_sound,omitempty"`
	Sound           bool  `json:"sound"`
}

type VKCanWrite struct {
	Allowed bool `json:"allowed"`
	Reason  int  `json:"reason,omitempty"`
}

type VKChatSettings struct {
	MembersCount  int         `json:"members_count"`
	Title         string      `json:"title"`
	PinnedMessage interface{} `json:"pinned_message,omitempty"`
	State         string      `json:"state"`
	Photo         interface{} `json:"photo,omitempty"`
	ActiveIDs     interface{} `json:"active_ids,omitempty"`
}

// VKApiChat represents the VK API Chat object (VK API < 5.80 or messages.getChat).
type VKApiChat struct {
	ID           int64           `json:"id"`
	Type         string          `json:"type"` // "chat"
	Title        string          `json:"title"`
	AdminID      int64           `json:"admin_id"`
	Users        []int64         `json:"users"`
	MembersCount int             `json:"members_count,omitempty"`
	PushSettings *VKPushSettings `json:"push_settings,omitempty"`
	Photo50      string          `json:"photo_50,omitempty"`
	Photo100     string          `json:"photo_100,omitempty"`
	Photo200     string          `json:"photo_200,omitempty"`
	Left         int             `json:"left,omitempty"`
	Kicked       int             `json:"kicked,omitempty"`
}

func derivePeerId(chatID string, currentUserID int64) int64 {
	if strings.HasPrefix(chatID, "c") {
		id, _ := strconv.ParseInt(chatID[1:], 10, 64)
		return id + 2000000000
	}

	if strings.HasPrefix(chatID, "g") {
		parts := strings.Split(chatID[1:], "_")
		if len(parts) < 2 {
			return 0
		}
		id1, _ := strconv.ParseInt(parts[0], 10, 64)
		id2, _ := strconv.ParseInt(parts[1], 10, 64)

		if id1 == currentUserID {
			return id2
		}
		return id1
	}

	if strings.HasPrefix(chatID, "dm") {
		parts := strings.Split(chatID[2:], "_")
		id1, _ := strconv.ParseInt(parts[0], 10, 64)
		id2, _ := strconv.ParseInt(parts[1], 10, 64)
		if id1 == currentUserID {
			return id2
		}
		return id1
	}
	return 0
}

func (c *Conversation) ToVKApiStruct(tx *gorm.DB, currentUserID int64, member *ConversationMember, activeIDs []int64) VKApiConversation {
	peerType := "user"
	PeerID := derivePeerId(c.InternalID, currentUserID)

	localID := PeerID
	if PeerID > 2000000000 {
		peerType = "chat"
		localID = PeerID - 2000000000
	} else if PeerID < 0 {
		peerType = "group"
		localID = -PeerID
	}

	var inReadCmid uint64 = 0
	var outReadCmid uint64 = 0
	if member != nil {
		inReadCmid = member.LastReadID
	}

	lastCMID := c.LastMessageID
	var lastMsgID uint64 = 0
	dbRef := tx
	if dbRef == nil {
		dbRef = db.Instance
	}
	if dbRef != nil && lastCMID > 0 {
		dbRef.Table("messages").Select("id").Where("chat_id = ? AND local_id = ?", c.InternalID, lastCMID).Scan(&lastMsgID)
	}

	inReadID := ResolveGlobalMsgID(tx, c.InternalID, inReadCmid, lastCMID, lastMsgID)
	outReadID := ResolveGlobalMsgID(tx, c.InternalID, outReadCmid, lastCMID, lastMsgID)

	conv := VKApiConversation{
		Peer: VKApiPeer{
			ID:      PeerID,
			Type:    peerType,
			LocalID: localID,
		},
		LastMessageID:             lastMsgID,
		LastConversationMessageID: lastCMID,
		InRead:                    inReadID,
		OutRead:                   outReadID,
		InReadCmid:                inReadCmid,
		OutReadCmid:               outReadCmid,
	}

	if member != nil && !member.JoinedAt.IsZero() {
		if member.LeftAt != nil {
			conv.CanWrite = VKCanWrite{
				Allowed: false,
				Reason:  916,
			}
		} else {
			conv.CanWrite = VKCanWrite{
				Allowed: true,
			}
		}
	} else {
		conv.CanWrite = VKCanWrite{
			Allowed: false,
			Reason:  917,
		}
	}

	var unreadCount int64
	if member != nil && !member.JoinedAt.IsZero() && member.LeftAt == nil {
		unreadQ := tx.Model(&Message{}).
			Where("chat_id = ? AND local_id > ? AND from_id != ?", c.InternalID, member.LastReadID, currentUserID)
		if member.DeletedBeforeID > 0 {
			unreadQ = unreadQ.Where("local_id > ?", member.DeletedBeforeID)
		}
		unreadQ = BuildVisibilityFilter(unreadQ, c.InternalID, currentUserID)
		unreadQ.Count(&unreadCount)
	}
	conv.UnreadCount = int(unreadCount)

	if peerType == "chat" {
		settings := VKChatSettings{
			Title: c.Title,
		}

		if member != nil && !member.JoinedAt.IsZero() {
			if member.LeftAt != nil {
				settings.State = "left"
				conv.CanWrite.Reason = 916
				var lastKickMsg Message
				if errK := tx.Where("chat_id = ? AND action = ? AND action_mid = ?", c.InternalID, "chat_kick_user", currentUserID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
					if lastKickMsg.FromID != currentUserID {
						settings.State = "kicked"
						conv.CanWrite.Reason = 915
					}
				}
			} else {
				settings.State = "in"
			}
		} else {
			settings.State = "out"
			conv.CanWrite.Reason = 917
		}

		var mCount int64
		tx.Model(&ConversationMember{}).Where("internal_chat_id = ? AND left_at IS NULL", c.InternalID).Count(&mCount)
		settings.MembersCount = int(mCount)

		if len(activeIDs) > 6 {
			settings.ActiveIDs = activeIDs[:6]
		} else {
			settings.ActiveIDs = activeIDs
		}

		if c.PinnedMsgID > 0 {
			var pMsg Message
			if err := tx.Where("chat_id = ? AND local_id = ?", c.InternalID, c.PinnedMsgID).First(&pMsg).Error; err == nil {
				res := pMsg.ToVKApiStruct(tx, 0, currentUserID, PeerID)
				settings.PinnedMessage = res
			}
		}

		conv.ChatSettings = &settings
	}

	return conv
}

func (c *Conversation) ToVKApiChat(tx *gorm.DB, currentUserID int64, member *ConversationMember, userIDs []int64) VKApiChat {
	var chatID int64
	if strings.HasPrefix(c.InternalID, "c") {
		chatID, _ = strconv.ParseInt(c.InternalID[1:], 10, 64)
	}

	var adminID int64
	if c.OwnerID != nil {
		adminID = *c.OwnerID
	}

	if userIDs == nil && tx != nil && c.InternalID != "" {
		tx.Model(&ConversationMember{}).
			Where("internal_chat_id = ? AND left_at IS NULL", c.InternalID).
			Pluck("user_id", &userIDs)
	}

	if userIDs == nil {
		userIDs = []int64{}
	}

	chatObj := VKApiChat{
		ID:           chatID,
		Type:         "chat",
		Title:        c.Title,
		AdminID:      adminID,
		Users:        userIDs,
		MembersCount: len(userIDs),
	}

	if member != nil && member.LeftAt != nil {
		var lastKickMsg Message
		if errK := tx.Where("chat_id = ? AND action = ? AND action_mid = ?", c.InternalID, "chat_kick_user", currentUserID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
			if lastKickMsg.FromID != currentUserID {
				chatObj.Kicked = 1
			} else {
				chatObj.Left = 1
			}
		} else {
			chatObj.Left = 1
		}
	}

	return chatObj
}

func (c *Conversation) ToVKApiStructVersioned(tx *gorm.DB, currentUserID int64, member *ConversationMember, activeIDs []int64, apiV ApiV) any {
	if apiV.IsOlderThan(5, 80) && strings.HasPrefix(c.InternalID, "c") {
		return c.ToVKApiChat(tx, currentUserID, member, activeIDs)
	}
	return c.ToVKApiStruct(tx, currentUserID, member, activeIDs)
}

// VKApiHistoryAttachment represents history attachment item in VK API response.
type VKApiHistoryAttachment struct {
	MessageID  uint64      `json:"message_id"`
	Attachment interface{} `json:"attachment"`
}
