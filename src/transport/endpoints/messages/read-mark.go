package messages

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"
	"ovk-im/src/transport/endpoints/status"

	"github.com/gin-gonic/gin"
)

func MarkAsRead(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	peerID, _ := strconv.ParseInt(c.DefaultQuery("peer_id", c.PostForm("peer_id")), 10, 64)
	uIDParam, _ := strconv.ParseInt(c.DefaultQuery("user_id", c.PostForm("user_id")), 10, 64)
	if peerID == 0 && uIDParam != 0 {
		peerID = uIDParam
	}
	if peerID == 0 {
		if chatIDParam := c.DefaultQuery("chat_id", c.PostForm("chat_id")); chatIDParam != "" {
			if id, err := strconv.ParseInt(chatIDParam, 10, 64); err == nil {
				peerID = 2000000000 + id
			}
		}
	}

	startID, _ := strconv.ParseUint(c.DefaultQuery("start_message_id", c.PostForm("start_message_id")), 10, 64)
	idsStr := c.DefaultQuery("message_ids", c.PostForm("message_ids"))
	markAll := c.DefaultQuery("mark_conversation_as_read", c.PostForm("mark_conversation_as_read")) == "1"

	type markTask struct {
		maxLocalID uint64
		pID        int64
	}
	tasks := make(map[string]*markTask)

	if peerID == 0 && (markAll || (startID == 0 && idsStr == "")) {
		var unreadMembers []struct {
			InternalChatID string `gorm:"column:internal_chat_id"`
			ConvLastMsgID  uint64 `gorm:"column:conv_last_message_id"`
			MemLastMsgID   uint64 `gorm:"column:mem_last_message_id"`
		}

		db.Instance.Table("conversation_members").
			Select(`
				conversation_members.internal_chat_id,
				COALESCE(conversations.last_message_id, 0) as conv_last_message_id,
				COALESCE(conversation_members.last_message_id, 0) as mem_last_message_id
			`).
			Joins("LEFT JOIN conversations ON conversations.internal_id = conversation_members.internal_chat_id").
			Where("conversation_members.user_id = ? AND conversation_members.left_at IS NULL", currentUserID).
			Where("COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.last_read_id, 0)").
			Where("COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.deleted_before_id, 0)").
			Scan(&unreadMembers)

		for _, m := range unreadMembers {
			targetID := m.ConvLastMsgID
			if targetID == 0 || (m.MemLastMsgID > 0 && m.MemLastMsgID > targetID) {
				targetID = m.MemLastMsgID
			}
			if targetID > 0 {
				tasks[m.InternalChatID] = &markTask{
					maxLocalID: targetID,
					pID:        chat.DerivePeerID(m.InternalChatID, currentUserID),
				}
			}
		}
	}

	if startID != 0 {
		if peerID != 0 {
			chatID := chat.GetInternalChatID(peerID, currentUserID)
			var msg db_models.Message
			if err := db.Instance.Select("local_id").Where("chat_id = ? AND (local_id = ? OR id = ?)", chatID, startID, startID).Order("local_id DESC").First(&msg).Error; err == nil && msg.LocalID > 0 {
				tasks[chatID] = &markTask{maxLocalID: msg.LocalID, pID: peerID}
			} else {
				var conv db_models.Conversation
				if db.Instance.Select("last_message_id").Where("internal_id = ?", chatID).First(&conv).Error == nil && conv.LastMessageID > 0 {
					if startID <= conv.LastMessageID {
						tasks[chatID] = &markTask{maxLocalID: startID, pID: peerID}
					} else {
						var resolvedLocalID uint64
						db.Instance.Table("messages").Where("chat_id = ? AND id <= ?", chatID, startID).Select("COALESCE(MAX(local_id), 0)").Row().Scan(&resolvedLocalID)
						if resolvedLocalID > 0 {
							tasks[chatID] = &markTask{maxLocalID: resolvedLocalID, pID: peerID}
						} else {
							tasks[chatID] = &markTask{maxLocalID: conv.LastMessageID, pID: peerID}
						}
					}
				}
			}
		} else {
			var msg db_models.Message
			if err := db.Instance.Select("chat_id", "local_id").Where("id = ?", startID).First(&msg).Error; err == nil && msg.LocalID > 0 {
				tasks[msg.ChatID] = &markTask{
					maxLocalID: msg.LocalID,
					pID:        chat.DerivePeerID(msg.ChatID, currentUserID),
				}
			}
		}
	}

	if idsStr != "" {
		parts := strings.Split(idsStr, ",")
		for _, p := range parts {
			if id, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64); err == nil {
				var msg db_models.Message
				var qErr error
				if peerID != 0 {
					targetChatID := chat.GetInternalChatID(peerID, currentUserID)
					qErr = db.Instance.Select("chat_id", "local_id").Where("id = ? OR (chat_id = ? AND local_id = ?)", id, targetChatID, id).First(&msg).Error
				} else {
					qErr = db.Instance.Select("chat_id", "local_id").Where("id = ?", id).First(&msg).Error
				}

				if qErr == nil && msg.LocalID > 0 {
					if tasks[msg.ChatID] == nil {
						tasks[msg.ChatID] = &markTask{
							maxLocalID: msg.LocalID,
							pID:        chat.DerivePeerID(msg.ChatID, currentUserID),
						}
					} else if msg.LocalID > tasks[msg.ChatID].maxLocalID {
						tasks[msg.ChatID].maxLocalID = msg.LocalID
					}
				}
			}
		}
	}

	if peerID != 0 && (markAll || (startID == 0 && idsStr == "")) {
		chatID := chat.GetInternalChatID(peerID, currentUserID)
		var maxLocalID uint64
		db.Instance.Table("messages").Where("chat_id = ?", chatID).Select("COALESCE(MAX(local_id), 0)").Row().Scan(&maxLocalID)
		if maxLocalID > 0 {
			if tasks[chatID] == nil || maxLocalID > tasks[chatID].maxLocalID {
				tasks[chatID] = &markTask{maxLocalID: maxLocalID, pID: peerID}
			}
		}
	}

	// 1. Synchronously update database so that immediate follow-up requests see updated read state
	for cID, task := range tasks {
		_ = chat.MarkAsRead(db.Instance, cID, currentUserID, task.maxLocalID)
	}

	if currentUserID > 0 && r.LPRepo != nil && r.LPRepo.Client != nil {
		status.TouchUserActivity(c.Request.Context(), db.Instance, r.LPRepo.Client, r.LPRepo, r.Broadcaster, currentUserID)
	}

	// 2. Broadcast LongPoll events
	if len(tasks) > 0 {
		go func(tks map[string]*markTask, uid int64) {
			ctx := context.Background()
			for cID, task := range tks {
				r.BroadcastMarkAsRead(ctx, cID, uid, task.maxLocalID)
			}
		}(tasks, currentUserID)
	}

	c.JSON(http.StatusOK, gin.H{"response": 1})
}
