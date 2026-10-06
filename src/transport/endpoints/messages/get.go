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

func Get(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	out, _ := strconv.Atoi(c.DefaultQuery("out", "0"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	count, _ := strconv.Atoi(c.DefaultQuery("count", "20"))
	if count > 200 {
		count = 200
	}
	timeOffset, _ := strconv.Atoi(c.DefaultQuery("time_offset", "0"))
	filters, _ := strconv.Atoi(c.DefaultQuery("filters", "0"))
	lastMessageID, _ := strconv.ParseUint(c.DefaultQuery("last_message_id", "0"), 10, 64)
	previewLength, _ := strconv.Atoi(c.DefaultQuery("preview_length", "0"))

	var query *gorm.DB
	if currentUserID == 0 {
		query = dbx.Instance.Table("messages")
	} else {
		query = dbx.Instance.Table("messages").
			Joins("JOIN conversation_members ON conversation_members.internal_chat_id = messages.chat_id AND conversation_members.user_id = ? AND conversation_members.left_at IS NULL", currentUserID).
			Where("messages.deleted_at IS NULL").
			Where("messages.local_id > COALESCE(conversation_members.deleted_before_id, 0)")

		if out == 1 {
			query = query.Where("messages.from_id = ?", currentUserID)
		} else {
			query = query.Where("messages.from_id != ?", currentUserID)
		}
	}

	if timeOffset > 0 {
		since := time.Now().Add(-time.Duration(timeOffset) * time.Second)
		query = query.Where("messages.created_at >= ?", since)
	}

	if (filters & 1) != 0 {
		if out == 1 {
			query = query.Where("messages.local_id > COALESCE((SELECT MAX(cm2.last_read_id) FROM conversation_members cm2 WHERE cm2.internal_chat_id = messages.chat_id AND cm2.user_id != messages.from_id AND cm2.left_at IS NULL), 0)")
		} else {
			query = query.Where("messages.local_id > conversation_members.last_read_id")
		}
	}
	if (filters & 2) != 0 {
		query = query.Where("messages.chat_id NOT LIKE 'c%'")
	}
	if (filters & 8) != 0 {
		query = query.Where("messages.id IN (SELECT message_id FROM important_messages WHERE user_id = ?)", currentUserID)
	}

	if lastMessageID > 0 {
		query = query.Where("messages.id < ?", lastMessageID)
	}

	var totalCount int64
	query.Count(&totalCount)

	var dbMessages []db_models.Message
	err := query.Preload("Conversation").Order("messages.created_at DESC, messages.id DESC").
		Limit(count).Offset(offset).
		Find(&dbMessages).Error

	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	chatIDsToFetchMembers := make([]string, 0)
	msgIDs := make([]uint64, len(dbMessages))
	for i, m := range dbMessages {
		msgIDs[i] = m.ID
		if strings.HasPrefix(m.ChatID, "c") {
			chatIDsToFetchMembers = append(chatIDsToFetchMembers, m.ChatID)
		}
	}

	importantMap := make(map[uint64]bool)
	if len(msgIDs) > 0 {
		var importantIDs []uint64
		dbx.Instance.Table("important_messages").
			Where("user_id = ? AND message_id IN ?", currentUserID, msgIDs).
			Pluck("message_id", &importantIDs)
		for _, id := range importantIDs {
			importantMap[id] = true
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
		dbx.Instance.Table("conversation_members").
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

	legacyItems := make([]db_models.VKApiMessageLegacy, 0, len(dbMessages))
	for _, m := range dbMessages {
		mPeerID := chat.DerivePeerID(m.ChatID, currentUserID)
		vkMsg := m.ToVKApiStructLegacy(dbx.Instance, 5, currentUserID, mPeerID)
		if previewLength > 0 {
			vkMsg.Body = core.TruncateWords(vkMsg.Body, previewLength)
		}
		if importantMap[m.ID] {
			vkMsg.Important = true
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

	response := gin.H{
		"count": totalCount,
		"items": legacyItems,
	}

	if c.Query("extended") == "1" {
		var userIDs []int64
		var groupIDs []int64
		var chatIDs []int64

		uMap := make(map[int64]struct{})
		gMap := make(map[int64]struct{})
		cMap := make(map[int64]struct{})

		var scanLegacy func(m db_models.VKApiMessageLegacy)
		scanLegacy = func(m db_models.VKApiMessageLegacy) {
			if m.UserID > 0 && m.UserID < 2000000000 {
				uMap[m.UserID] = struct{}{}
			} else if m.UserID < 0 {
				gMap[-m.UserID] = struct{}{}
			}
			if m.FromID > 0 {
				uMap[m.FromID] = struct{}{}
			} else if m.FromID < 0 {
				gMap[-m.FromID] = struct{}{}
			}
			if m.AdminID > 0 {
				uMap[m.AdminID] = struct{}{}
			}
			if m.ActionMid > 0 {
				uMap[m.ActionMid] = struct{}{}
			}
			if m.ChatID > 0 {
				cMap[m.ChatID] = struct{}{}
			}
			for _, uid := range m.ChatActive {
				if uid > 0 {
					uMap[uid] = struct{}{}
				}
			}
			for _, fwd := range m.ForwardMessages {
				scanLegacy(fwd)
			}
		}

		for _, m := range legacyItems {
			scanLegacy(m)
		}

		for _, mems := range chatMembersMap {
			for _, memID := range mems {
				if memID > 0 {
					uMap[memID] = struct{}{}
				}
			}
		}

		for id := range uMap {
			userIDs = append(userIDs, id)
		}
		for id := range gMap {
			groupIDs = append(groupIDs, id)
		}
		for id := range cMap {
			chatIDs = append(chatIDs, id)
		}

		response["profiles"] = userIDs
		response["groups"] = groupIDs
		response["chats"] = chatIDs
	}

	c.JSON(http.StatusOK, gin.H{
		"response": response,
	})
}

func GetByID(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)
	apiV := core.GetApiV(c)

	idsStr := c.Query("message_ids")
	if idsStr == "" {
		r.Reject(c, 100, "One of the parameters is missing: message_ids")
		return
	}

	idStrings := strings.Split(idsStr, ",")
	var ids []uint64
	for _, s := range idStrings {
		if id, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		r.Reject(c, 100, "Invalid message_ids")
		return
	}

	previewLength, _ := strconv.Atoi(c.DefaultQuery("preview_length", "0"))

	var dbMessages []db_models.Message
	var query *gorm.DB
	if currentUserID == 0 {
		query = dbx.Instance.Where("id IN ?", ids)
	} else {
		query = dbx.Instance.Where("id IN ? AND chat_id IN (SELECT internal_chat_id FROM conversation_members WHERE user_id = ? AND left_at IS NULL)",
			ids, currentUserID)
	}
	query = db_models.BuildVisibilityFilter(query, "", currentUserID)
	err := query.Preload("Conversation").Find(&dbMessages).Error

	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	preloadedMap := db_models.PreloadNestedMessages(dbx.Instance, dbMessages, 10)
	importantMap := db_models.PreloadImportantMapFromMessages(dbx.Instance, currentUserID, dbMessages, preloadedMap)

	var chatIDsToFetchMembers []string
	var targetChatIDs []string
	chatIDsMap := make(map[string]bool)

	for _, m := range dbMessages {
		if !chatIDsMap[m.ChatID] {
			chatIDsMap[m.ChatID] = true
			targetChatIDs = append(targetChatIDs, m.ChatID)
		}
		if strings.HasPrefix(m.ChatID, "c") {
			chatIDsToFetchMembers = append(chatIDsToFetchMembers, m.ChatID)
		}
	}

	readCache := make(map[string][]db_models.MemberReadState)
	if len(targetChatIDs) > 0 {
		var memberStates []struct {
			InternalChatID string `gorm:"column:internal_chat_id"`
			UserID         int64  `gorm:"column:user_id"`
			LastReadID     uint64 `gorm:"column:last_read_id"`
		}
		dbx.Instance.Table("conversation_members").
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
		dbx.Instance.Table("conversation_members").
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
		legacyItems := make([]db_models.VKApiMessageLegacy, 0, len(dbMessages))
		for _, m := range dbMessages {
			mPeerID := chat.DerivePeerID(m.ChatID, currentUserID)
			vkMsg := m.ToVKApiStructBatchLegacy(dbx.Instance, 10, currentUserID, mPeerID, preloadedMap, readCache, nil, importantMap)
			if previewLength > 0 {
				vkMsg.Body = core.TruncateWords(vkMsg.Body, previewLength)
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
		modernItems := make([]db_models.VKApiMessage, 0, len(dbMessages))
		for _, m := range dbMessages {
			mPeerID := chat.DerivePeerID(m.ChatID, currentUserID)
			vkMsg := m.ToVKApiStructBatch(dbx.Instance, 10, currentUserID, mPeerID, preloadedMap, readCache, nil, importantMap)
			if previewLength > 0 {
				vkMsg.Text = core.TruncateWords(vkMsg.Text, previewLength)
			}
			modernItems = append(modernItems, vkMsg)
		}
		responseItems = modernItems
	}

	response := gin.H{
		"count": len(dbMessages),
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

		response["profiles"] = core.UniqueIDs(userIDs)
		response["groups"] = core.UniqueIDs(groupIDs)
		if len(chatIDs) > 0 {
			response["chats"] = core.UniqueIDs(chatIDs)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"response": response,
	})
}

func GetByConversationMessageID(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	peerIDStr := c.Query("peer_id")
	cmidsStr := c.Query("conversation_message_ids")

	if (peerIDStr == "" && c.Query("chat_id") == "") || cmidsStr == "" {
		r.Reject(c, 100, "One of the parameters is missing: peer_id or conversation_message_ids")
		return
	}

	peerID, _ := strconv.ParseInt(peerIDStr, 10, 64)
	uIDParam, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	chatID := chat.ResolveChatID(c.Query("chat_id"), peerID, uIDParam, currentUserID)
	if chatID == "" && peerID != 0 {
		chatID = chat.GetInternalChatID(peerID, currentUserID)
	}

	if currentUserID != 0 {
		inChat, _ := chat.IsUserInChat(dbx.Instance, chatID, currentUserID)
		if !inChat {
			r.Reject(c, 917, "Access denied")
			return
		}
	}


	idStrings := strings.Split(cmidsStr, ",")
	var localIDs []uint64
	for _, s := range idStrings {
		if id, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64); err == nil {
			localIDs = append(localIDs, id)
		}
	}

	var dbMessages []db_models.Message
	cmQuery := dbx.Instance.Where("chat_id = ? AND local_id IN ?", chatID, localIDs)
	cmQuery = db_models.BuildVisibilityFilter(cmQuery, chatID, currentUserID)
	err := cmQuery.Find(&dbMessages).Error

	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	items := make([]db_models.VKApiMessage, 0)
	for _, m := range dbMessages {
		items = append(items, m.ToVKApiStruct(dbx.Instance, 5, currentUserID, peerID))
	}

	response := gin.H{
		"count": len(items),
		"items": items,
	}

	if c.Query("extended") == "1" {
		var userIDs []int64
		var groupIDs []int64

		core.CollectAllEntityIDs(items, &userIDs, &groupIDs)

		response["profiles"] = userIDs
		response["groups"] = groupIDs
	}

	c.JSON(http.StatusOK, gin.H{
		"response": response,
	})
}
