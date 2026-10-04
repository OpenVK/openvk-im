package chats

import (
	"net/http"
	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"
	"strings"

	"github.com/gin-gonic/gin"
)

/*
Данный метод возвращает все беседы с юзерами, так как по имени поиск невозможен.
Однако сортирует все беседы с чатами.
*/

func SearchConversations(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	q := strings.TrimSpace(c.DefaultQuery("q", ""))
	extended := c.Query("extended") == "1"

	var rows []db_models.ConversationMember

	var err error
	if currentUserID == 0 {
		var convs []db_models.Conversation
		cQuery := db.Instance.Model(&db_models.Conversation{})
		if q != "" {
			cQuery = cQuery.Where("title LIKE ?", "%"+q+"%")
		}
		err = cQuery.Find(&convs).Error
		for _, conv := range convs {
			rows = append(rows, db_models.ConversationMember{
				InternalChatID: conv.InternalID,
				LastMessageID:  conv.LastMessageID,
				Conversation:   conv,
			})
		}
	} else {
		query := db.Instance.Table("conversation_members").
			Joins("LEFT JOIN (SELECT internal_id, MAX(last_message_id) as last_message_id, MAX(title) as title FROM conversations GROUP BY internal_id) conversations ON conversations.internal_id = conversation_members.internal_chat_id").
			Where("conversation_members.user_id = ?", currentUserID).
			Where(`(
				(
					conversation_members.left_at IS NULL
					AND (
						conversation_members.internal_chat_id LIKE 'c%'
						OR COALESCE(conversation_members.deleted_before_id, 0) = 0
						OR COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > conversation_members.deleted_before_id
					)
				)
				OR
				(
					conversation_members.left_at IS NOT NULL
					AND conversation_members.internal_chat_id LIKE 'c%'
					AND (
						COALESCE(conversation_members.deleted_before_id, 0) = 0
						OR
						EXISTS (
							SELECT 1 FROM messages m
							WHERE m.chat_id = conversation_members.internal_chat_id
								AND m.deleted_at IS NULL
								AND m.local_id > COALESCE(conversation_members.deleted_before_id, 0)
								AND (
									NOT EXISTS (SELECT 1 FROM conversation_member_periods p0 WHERE p0.internal_chat_id = conversation_members.internal_chat_id AND p0.user_id = conversation_members.user_id)
									OR EXISTS (SELECT 1 FROM conversation_member_periods p WHERE p.internal_chat_id = conversation_members.internal_chat_id AND p.user_id = conversation_members.user_id AND m.local_id >= p.start_local_id AND (p.end_local_id IS NULL OR m.local_id <= p.end_local_id))
								)
						)
					)
				)
			)`).
			Where(
				db.Instance.Where("conversation_members.internal_chat_id NOT LIKE ?", "c%").
					Or("conversations.title LIKE ?", "%"+q+"%"),
			)

		err = query.Order("conversation_members.last_message_id DESC").
			Preload("Conversation").
			Find(&rows).Error
	}


	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	lastMsgKeys := make(map[string]uint64)
	for _, row := range rows {
		if row.LastMessageID > 0 && row.LastMessageID > row.DeletedBeforeID {
			lastMsgKeys[row.InternalChatID] = row.LastMessageID
		}
	}

	msgMap := make(map[string]db_models.Message)
	if len(lastMsgKeys) > 0 {
		var lastMessages []db_models.Message
		q := db.Instance.Where("(messages.chat_id, messages.local_id) IN ?", buildInPairs(lastMsgKeys))
		q = db_models.BuildVisibilityFilter(q, "", currentUserID)
		q.Find(&lastMessages)
		for _, msg := range lastMessages {
			msgMap[msg.ChatID] = msg
		}
	}

	for _, row := range rows {
		if row.LastMessageID > 0 && row.LastMessageID > row.DeletedBeforeID {
			if _, ok := msgMap[row.InternalChatID]; !ok {
				var latestVisible db_models.Message
				vQ := db.Instance.Table("messages").Where("messages.chat_id = ?", row.InternalChatID)
				vQ = db_models.BuildVisibilityFilter(vQ, row.InternalChatID, currentUserID)
				if err := vQ.Order("messages.local_id DESC").First(&latestVisible).Error; err == nil && latestVisible.ID > 0 {
					msgMap[row.InternalChatID] = latestVisible
				}
			}
		}
	}

	responseItems := make([]gin.H, 0)
	var userIDs, groupIDs, chatIDs []int64

	for _, m := range rows {
		pID := chat.DerivePeerID(m.InternalChatID, currentUserID)
		lastMsg, hasMsg := msgMap[m.InternalChatID]

		addID(pID, &userIDs, &groupIDs, &chatIDs)
		if hasMsg {
			addID(lastMsg.FromID, &userIDs, &groupIDs, &chatIDs)
		}

		canWriteObj := gin.H{"allowed": true}
		stateStr := "in"
		if getPeerType(m.InternalChatID) == "chat" && m.LeftAt != nil {
			stateStr = "left"
			canWriteObj = gin.H{"allowed": false, "reason": 916}
			var lastKickMsg db_models.Message
			if errK := db.Instance.Where("chat_id = ? AND action = ? AND action_mid = ?", m.InternalChatID, "chat_kick_user", currentUserID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
				if lastKickMsg.FromID != currentUserID {
					stateStr = "kicked"
					canWriteObj = gin.H{"allowed": false, "reason": 915}
				}
			}
		}

		var lastMsgID uint64 = 0
		var lastCMID uint64 = m.LastMessageID
		if hasMsg {
			lastMsgID = lastMsg.ID
			lastCMID = lastMsg.LocalID
		}
		inReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, m.LastReadID, lastCMID, lastMsgID)
		outReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, m.LastMessageID, lastCMID, lastMsgID)

		convObj := gin.H{
			"peer":                         gin.H{"id": pID, "type": getPeerType(m.InternalChatID)},
			"last_message_id":              lastMsgID,
			"last_conversation_message_id": lastCMID,
			"in_read":                      inReadID,
			"out_read":                     outReadID,
			"in_read_cmid":                 m.LastReadID,
			"out_read_cmid":                m.LastMessageID,
			"can_write":                    canWriteObj,
		}
		if getPeerType(m.InternalChatID) == "chat" {
			convObj["chat_settings"] = gin.H{
				"state": stateStr,
			}
		}

		var msgVK interface{} = nil
		if hasMsg {
			msgVK = lastMsg.ToVKApiStructBatch(db.Instance, 1, currentUserID, pID, nil, nil, nil, nil)
		}

		responseItems = append(responseItems, gin.H{
			"conversation": convObj,
			"last_message": msgVK,
		})
	}

	result := gin.H{
		"count": len(responseItems),
		"items": responseItems,
	}

	if extended {
		result["profiles"] = uniqueIDs(userIDs)
		result["groups"] = uniqueIDs(groupIDs)
		result["chats"] = uniqueIDs(chatIDs)
	}

	c.JSON(http.StatusOK, gin.H{"response": result})
}
