package messages

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"

	"github.com/gin-gonic/gin"
)

func Search(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)
	apiV := core.GetApiV(c)

	q := c.Query("q")
	if q == "" {
		r.Reject(c, 100, "One of the parameters is missing: q")
		return
	}

	peerID, _ := strconv.ParseInt(c.Query("peer_id"), 10, 64)
	count, _ := strconv.Atoi(c.DefaultQuery("count", "20"))
	if count > 100 {
		count = 100
	} else if count < 1 {
		count = 20
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	previewLen, _ := strconv.Atoi(c.DefaultQuery("preview_length", "0"))

	var messageIDs []uint64
	var err error

	uIDParam, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	targetChatID := chat.ResolveChatID(c.Query("chat_id"), peerID, uIDParam, currentUserID)

	if targetChatID != "" {
		messageIDs, err = r.SearchRepo.SearchMessages(targetChatID, q)
	} else if currentUserID == 0 {
		var textMsgIDs []uint64
		if r.SearchRepo.WordsCount(q) > 0 {
			_ = db.Instance.Model(&db_models.MessageSearchIndex{}).
				Select("message_id").
				Where("word_hash IN ?", r.SearchRepo.PrepareHashes(q)).
				Group("message_id").
				Having("COUNT(DISTINCT word_hash) = ?", r.SearchRepo.WordsCount(q)).
				Pluck("message_id", &textMsgIDs).Error
		}

		var chatMsgIDs []uint64
		var matchedChatIDs []string
		_ = db.Instance.Model(&db_models.Conversation{}).
			Where("title LIKE ?", "%"+q+"%").
			Pluck("internal_id", &matchedChatIDs).Error

		if len(matchedChatIDs) > 0 {
			qDel := db.Instance.Model(&db_models.Message{}).Where("chat_id IN ?", matchedChatIDs)
			if currentUserID != 0 {
				qDel = qDel.Where("deleted_at IS NULL")
			}
			_ = qDel.Pluck("id", &chatMsgIDs).Error
		}

		idSet := make(map[uint64]bool)
		for _, id := range textMsgIDs {
			if !idSet[id] {
				idSet[id] = true
				messageIDs = append(messageIDs, id)
			}
		}
		for _, id := range chatMsgIDs {
			if !idSet[id] {
				idSet[id] = true
				messageIDs = append(messageIDs, id)
			}
		}
	} else {
		var myChatIDs []string
		db.Instance.Model(&db_models.ConversationMember{}).
			Where("user_id = ?", currentUserID).
			Pluck("internal_chat_id", &myChatIDs)

		if len(myChatIDs) == 0 {
			c.JSON(http.StatusOK, gin.H{"response": gin.H{"count": 0, "items": []interface{}{}}})
			return
		}

		var textMsgIDs []uint64
		if r.SearchRepo.WordsCount(q) > 0 {
			_ = db.Instance.Model(&db_models.MessageSearchIndex{}).
				Select("message_id").
				Where("chat_id IN ? AND word_hash IN ?", myChatIDs, r.SearchRepo.PrepareHashes(q)).
				Group("message_id").
				Having("COUNT(DISTINCT word_hash) = ?", r.SearchRepo.WordsCount(q)).
				Pluck("message_id", &textMsgIDs).Error
		}

		var chatMsgIDs []uint64
		var matchedChatIDs []string
		_ = db.Instance.Model(&db_models.Conversation{}).
			Where("internal_id IN ? AND title LIKE ?", myChatIDs, "%"+q+"%").
			Pluck("internal_id", &matchedChatIDs).Error

		if len(matchedChatIDs) > 0 {
			qDel := db.Instance.Model(&db_models.Message{}).Where("chat_id IN ?", matchedChatIDs)
			if currentUserID != 0 {
				qDel = qDel.Where("deleted_at IS NULL")
			}
			_ = qDel.Pluck("id", &chatMsgIDs).Error
		}

		idSet := make(map[uint64]bool)
		for _, id := range textMsgIDs {
			if !idSet[id] {
				idSet[id] = true
				messageIDs = append(messageIDs, id)
			}
		}
		for _, id := range chatMsgIDs {
			if !idSet[id] {
				idSet[id] = true
				messageIDs = append(messageIDs, id)
			}
		}
	}

	if err != nil || len(messageIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"response": gin.H{"count": 0, "items": []interface{}{}}})
		return
	}

	var msgs []db_models.Message
	query := db.Instance.Where("id IN ?", messageIDs)
	query = db_models.BuildVisibilityFilter(query, targetChatID, currentUserID)

	if dateParam := c.Query("date"); dateParam != "" {
		if t, err := time.Parse("02012006", dateParam); err == nil {
			query = query.Where("created_at < ?", t)
		}
	}

	var totalFound int64
	query.Model(&db_models.Message{}).Count(&totalFound)

	err = query.Preload("Conversation").Order("created_at DESC").Limit(count).Offset(offset).Find(&msgs).Error
	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	var chatIDsToFetchMembers []string
	var targetChatIDs []string
	chatIDsMap := make(map[string]bool)

	for _, m := range msgs {
		if !chatIDsMap[m.ChatID] {
			chatIDsMap[m.ChatID] = true
			targetChatIDs = append(targetChatIDs, m.ChatID)
		}
		if strings.HasPrefix(m.ChatID, "c") {
			chatIDsToFetchMembers = append(chatIDsToFetchMembers, m.ChatID)
		}
	}

	preloadedMap := db_models.PreloadNestedMessages(db.Instance, msgs, 10)
	importantMap := db_models.PreloadImportantMapFromMessages(db.Instance, currentUserID, msgs, preloadedMap)

	readCache := make(map[string][]db_models.MemberReadState)
	if len(targetChatIDs) > 0 {
		var memberStates []struct {
			InternalChatID string `gorm:"column:internal_chat_id"`
			UserID         int64  `gorm:"column:user_id"`
			LastReadID     uint64 `gorm:"column:last_read_id"`
		}
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id, last_read_id").
			Where("internal_chat_id IN ?", targetChatIDs).
			Scan(&memberStates)
		for _, ms := range memberStates {
			readCache[ms.InternalChatID] = append(readCache[ms.InternalChatID], db_models.MemberReadState{
				UserID:     ms.UserID,
				LastReadID: ms.LastReadID,
			})
		}
	}

	chatMembersMap := make(map[string][]int64)
	adminMap := make(map[string]int64)
	if len(chatIDsToFetchMembers) > 0 {
		type ChatMember struct {
			InternalChatID string
			UserID         int64
			IsAdmin        bool
		}
		var members []ChatMember
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id, is_admin").
			Where("internal_chat_id IN ? AND left_at IS NULL", chatIDsToFetchMembers).
			Order("joined_at ASC").
			Find(&members)

		for _, mem := range members {
			chatMembersMap[mem.InternalChatID] = append(chatMembersMap[mem.InternalChatID], mem.UserID)
			if mem.IsAdmin && adminMap[mem.InternalChatID] == 0 {
				adminMap[mem.InternalChatID] = mem.UserID
			}
		}
	}

	var responseItems any

	if apiV.IsOlderThan(5, 80) {
		legacyItems := make([]db_models.VKApiMessageLegacy, 0, len(msgs))
		for _, m := range msgs {
			msgPeerID := chat.DerivePeerID(m.ChatID, currentUserID)
			vkMsg := m.ToVKApiStructBatchLegacy(db.Instance, 10, currentUserID, msgPeerID, preloadedMap, readCache, nil, importantMap)
			if previewLen > 0 {
				vkMsg.Body = core.TruncateWords(vkMsg.Body, previewLen)
			}
			if strings.HasPrefix(m.ChatID, "c") {
				if mems, ok := chatMembersMap[m.ChatID]; ok {
					vkMsg.UsersCount = len(mems)
					activeCount := 10
					if len(mems) < activeCount {
						activeCount = len(mems)
					}
					vkMsg.ChatActive = mems[:activeCount]
				}
				if adm, ok := adminMap[m.ChatID]; ok && vkMsg.AdminID == 0 {
					vkMsg.AdminID = adm
				}
			}
			legacyItems = append(legacyItems, vkMsg)
		}
		responseItems = legacyItems
	} else {
		modernItems := make([]db_models.VKApiMessage, 0, len(msgs))
		for _, m := range msgs {
			msgPeerID := chat.DerivePeerID(m.ChatID, currentUserID)
			vkMsg := m.ToVKApiStructBatch(db.Instance, 10, currentUserID, msgPeerID, preloadedMap, readCache, nil, importantMap)
			if previewLen > 0 {
				vkMsg.Text = core.TruncateWords(vkMsg.Text, previewLen)
			}
			modernItems = append(modernItems, vkMsg)
		}
		responseItems = modernItems
	}

	result := gin.H{
		"count": totalFound,
		"items": responseItems,
	}

	if c.Query("extended") == "1" {
		var userIDs []int64
		var groupIDs []int64
		var chatIDs []int64

		if apiV.IsOlderThan(5, 80) {
			if legacyList, ok := responseItems.([]db_models.VKApiMessageLegacy); ok {
				core.CollectAllEntityIDsLegacy(legacyList, &userIDs, &groupIDs, &chatIDs)
			}
			for _, mems := range chatMembersMap {
				for _, memID := range mems {
					if memID > 0 {
						userIDs = append(userIDs, memID)
					}
				}
			}
		} else {
			if modernList, ok := responseItems.([]db_models.VKApiMessage); ok {
				core.CollectAllEntityIDs(modernList, &userIDs, &groupIDs)
			}
		}

		result["profiles"] = core.UniqueIDs(userIDs)
		result["groups"] = core.UniqueIDs(groupIDs)
		if len(chatIDs) > 0 {
			result["chats"] = core.UniqueIDs(chatIDs)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"response": result,
	})
}
