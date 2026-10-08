package messages

import (
	"net/http"
	dbx "ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func Edit(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		return
	}
	currentUserID := val.(int64)

	peerID, _ := strconv.ParseInt(c.Query("peer_id"), 10, 64)
	messageID, _ := strconv.ParseUint(c.Query("message_id"), 10, 64)
	cmid, _ := strconv.ParseUint(c.Query("conversation_message_id"), 10, 64)
	if messageID == 0 && cmid != 0 {
		messageID = cmid
	}
	newMessageText, textExists := c.GetQuery("message")
	newAttachment, attachExists := c.GetQuery("attachment")
	keepForward := c.Query("keep_forward_messages") == "1"

	if messageID == 0 {
		r.Reject(c, 100, "One of the parameters is missing: message_id or conversation_message_id")
		return
	}

	var chatID string
	if peerID != 0 {
		chatID = chat.GetInternalChatID(peerID, currentUserID)
	}

	var msg db_models.Message
	var err error
	if chatID != "" {
		err = dbx.Instance.Where("chat_id = ? AND (local_id = ? OR id = ?)", chatID, messageID, messageID).First(&msg).Error
	} else {
		err = dbx.Instance.Where("id = ?", messageID).First(&msg).Error
		if err == nil {
			chatID = msg.ChatID
			peerID = chat.DerivePeerID(chatID, currentUserID)
		}
	}
	if err != nil {
		r.Reject(c, 910, "Can't edit this message, maybe it has been deleted")
		return
	}

	if msg.FromID != currentUserID {
		r.Reject(c, 15, "Access denied: you can only edit your own messages")
		return
	}

	existingAttach := string(msg.Attachments)
	if strings.HasPrefix(existingAttach, "sticker") || strings.HasPrefix(existingAttach, "gift") || strings.Contains(existingAttach, "\"sticker\"") || strings.Contains(existingAttach, "\"gift\"") {
		r.Reject(c, 920, "Can't edit message with special attachments")
		return
	}

	updates := make(map[string]interface{})

	finalText := string(msg.Text)
	if textExists {
		cleanMessage := strings.TrimSpace(reInvisibleSpaces.ReplaceAllString(newMessageText, " "))
		if cleanMessage == "" {
			newMessageText = ""
		} else {
			newMessageText = strings.Trim(newMessageText, " \t\r\n\u200b\ufeff\u00a0\u200c\u200d")
		}
		finalText = newMessageText
		updates["text"] = db_models.EncryptedJSON(newMessageText)
	}

	finalAttach := string(msg.Attachments)
	if attachExists {
		if newAttachment != "" && !core.IsValidAttachments(newAttachment) {
			r.Reject(c, 100, "Invalid attachment format")
			return
		}
		finalAttach = newAttachment
	}

	if textExists || attachExists {
		computedAttach := core.BuildFinalAttachments(finalAttach, finalText)
		updates["attachments"] = db_models.EncryptedJSON(computedAttach)
	}

	if len(finalText) == 0 && (finalAttach == "" || finalAttach == "[]") {
		r.Reject(c, 100, "Empty messages are not allowed")
		return
	}

	if !keepForward {
		updates["forward_messages"] = ""
	}

	now := time.Now()
	updates["edited_at"] = &now

	err = dbx.Instance.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&msg).Updates(updates).Error; err != nil {
			return err
		}

		if textExists {
			tx.Where("message_id = ?", msg.ID).Delete(&db_models.MessageSearchIndex{})

			indexes := r.SearchRepo.GenerateBlindIndexes(msg.ID, chatID, newMessageText)
			if len(indexes) > 0 {
				if err := tx.Create(&indexes).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		r.Reject(c, 10, "Internal server error during update")
		return
	}

	r.SendUpdateEvent(peerID, messageID, finalText, finalAttach, currentUserID)

	c.JSON(http.StatusOK, gin.H{
		"response": 1,
	})
}
