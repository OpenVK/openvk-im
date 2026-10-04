package custom

import (
	"net/http"
	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"

	"github.com/gin-gonic/gin"
)

func GetUnreadMessages(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	userID := val.(int64)

	var totalUnread int64

	unreadQ := db.Instance.Table("messages").
		Joins("JOIN conversation_members cm ON cm.internal_chat_id = messages.chat_id AND cm.user_id = ? AND cm.left_at IS NULL", userID).
		Where("messages.from_id != ?", userID).
		Where("messages.local_id > cm.last_read_id").
		Where("messages.local_id > COALESCE(cm.deleted_before_id, 0)")
	unreadQ = db_models.BuildVisibilityFilter(unreadQ, "", userID)

	err := unreadQ.Select("COUNT(messages.id)").Scan(&totalUnread).Error

	if err != nil {
		r.Reject(c, 500, "Database error")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"response": gin.H{
			"messages": totalUnread,
		},
	})
}

func GetUnreadConversations(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	userID := val.(int64)

	count, err := chat.CountUnreadConversations(db.Instance, userID)
	if err != nil {
		r.Reject(c, 500, "Database error")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"response": gin.H{
			"count": count,
		},
	})
}
