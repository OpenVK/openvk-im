package chats

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/account"
	"ovk-im/src/transport/endpoints/core"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func GetConversations(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID := val.(int64)

	offset := r.GetInt(c, "offset", 0)
	count := r.GetInt(c, "count", 20)
	if count > 200 {
		count = 200
	} else if count < 1 {
		count = 20
	}

	filter := r.GetDefault(c, "filter", "all")
	extended := r.GetBool(c, "extended", false)

	type ResultRow struct {
		db_models.ConversationMember
		ConvLastMessageID uint64 `gorm:"column:conv_last_message_id"`
		TotalCount        int64  `gorm:"column:total_count"`
	}

	var rows []ResultRow

	query := db.Instance.Table("conversation_members").
		Select(`
			conversation_members.*, 
			CASE 
				WHEN conversation_members.left_at IS NULL THEN COALESCE(conversations.last_message_id, conversation_members.last_message_id)
				ELSE COALESCE(NULLIF(conversation_members.last_message_id, 0), (SELECT MAX(end_local_id) FROM conversation_member_periods WHERE internal_chat_id = conversation_members.internal_chat_id AND user_id = conversation_members.user_id), conversations.last_message_id)
			END as conv_last_message_id,
			COUNT(*) OVER() as total_count
		`).
		Joins("LEFT JOIN (SELECT internal_id, MAX(last_message_id) as last_message_id, MAX(created_at) as created_at, MAX(pinned_msg_id) as pinned_msg_id FROM conversations GROUP BY internal_id) conversations ON conversations.internal_id = conversation_members.internal_chat_id").
		Joins(`LEFT JOIN messages ON messages.chat_id = conversation_members.internal_chat_id AND messages.local_id = (
			CASE 
				WHEN conversation_members.left_at IS NULL THEN COALESCE(conversations.last_message_id, conversation_members.last_message_id)
				ELSE COALESCE(NULLIF(conversation_members.last_message_id, 0), (SELECT MAX(end_local_id) FROM conversation_member_periods WHERE internal_chat_id = conversation_members.internal_chat_id AND user_id = conversation_members.user_id), conversations.last_message_id)
			END
		) AND messages.local_id > COALESCE(conversation_members.deleted_before_id, 0) AND messages.deleted_at IS NULL`).
		Where("conversation_members.user_id = ?", currentUserID).
		Where(`(
			(
				conversation_members.left_at IS NULL
				AND (
					conversation_members.internal_chat_id LIKE 'c%'
					OR (
						COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > 0
						AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.deleted_before_id, 0)
					)
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
		)`)

	if currentUserID == 0 {
		query = db.Instance.Table("conversations").
			Select(`
				conversations.internal_id as internal_chat_id,
				conversations.last_message_id as last_message_id,
				conversations.last_message_id as conv_last_message_id,
				COUNT(*) OVER() as total_count
			`).
			Joins("LEFT JOIN messages ON messages.chat_id = conversations.internal_id AND messages.local_id = conversations.last_message_id AND messages.deleted_at IS NULL")
	} else if filter == "unread" {
		query = query.Where("conversation_members.left_at IS NULL AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.last_read_id, 0) AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.deleted_before_id, 0)")
	}

	err := query.Order(`
		COALESCE(messages.created_at, conversation_members.joined_at, conversations.created_at, '1970-01-01 00:00:00') DESC,
		COALESCE(messages.id, 0) DESC,
		(
			CASE 
				WHEN conversation_members.left_at IS NULL THEN COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0)
				ELSE COALESCE(NULLIF(conversation_members.last_message_id, 0), (SELECT MAX(end_local_id) FROM conversation_member_periods WHERE internal_chat_id = conversation_members.internal_chat_id AND user_id = conversation_members.user_id), conversations.last_message_id, 0)
			END
		) DESC,
		conversation_members.internal_chat_id DESC
	`).
		Preload("Conversation", func(db *gorm.DB) *gorm.DB {
			return db.Order("conversations.last_message_id DESC, conversations.id DESC")
		}).
		Limit(count).Offset(offset).Find(&rows).Error

	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	if len(rows) == 0 {
		c.JSON(http.StatusOK, gin.H{"response": gin.H{"count": 0, "items": []interface{}{}, "unread_count": 0}})
		return
	}

	// Deduplicate rows by InternalChatID preserving order.
	// Duplicate rows can appear when the conversations table has more than one
	// row with the same internal_id (a known artifact of the legacy migration),
	// because GORM's Preload expands the result set for every matched Conversation.
	{
		seen := make(map[string]struct{}, len(rows))
		deduped := rows[:0]
		for _, row := range rows {
			if _, ok := seen[row.InternalChatID]; ok {
				continue
			}
			seen[row.InternalChatID] = struct{}{}
			deduped = append(deduped, row)
		}
		rows = deduped
	}

	if len(rows) == 0 {
		c.JSON(http.StatusOK, gin.H{"response": gin.H{"count": 0, "items": []interface{}{}, "unread_count": 0}})
		return
	}

	// Use a separate DISTINCT count instead of the window-function value, which
	// can be inflated when the conversations table contains duplicate internal_id
	// rows (a known artifact of the legacy migration).
	var totalCount int64
	if currentUserID == 0 {
		db.Instance.Table("conversations").Count(&totalCount)
	} else {
		totalCountQuery := db.Instance.Table("conversation_members").
			Select("COUNT(DISTINCT conversation_members.internal_chat_id)").
			Joins("LEFT JOIN (SELECT internal_id, MAX(last_message_id) as last_message_id FROM conversations GROUP BY internal_id) conversations ON conversations.internal_id = conversation_members.internal_chat_id").
			Where("conversation_members.user_id = ?", currentUserID).
			Where(`(
				(
					conversation_members.left_at IS NULL
					AND (
						conversation_members.internal_chat_id LIKE 'c%'
						OR (
							COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > 0
							AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.deleted_before_id, 0)
						)
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
			)`)
		if filter == "unread" {
			totalCountQuery = totalCountQuery.Where("conversation_members.left_at IS NULL AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.last_read_id, 0) AND COALESCE(conversations.last_message_id, conversation_members.last_message_id, 0) > COALESCE(conversation_members.deleted_before_id, 0)")
		}
		totalCountQuery.Scan(&totalCount)
	}
	var totalUnreadConversations int64
	if currentUserID != 0 {
		unreadConvQ := db.Instance.Table("messages").
			Joins("JOIN conversation_members ON conversation_members.internal_chat_id = messages.chat_id AND conversation_members.user_id = ? AND conversation_members.left_at IS NULL", currentUserID).
			Where("messages.from_id != ?", currentUserID).
			Where("messages.local_id > conversation_members.last_read_id").
			Where("messages.local_id > COALESCE(conversation_members.deleted_before_id, 0)")
		unreadConvQ = db_models.BuildVisibilityFilter(unreadConvQ, "", currentUserID)
		unreadConvQ.Select("COUNT(DISTINCT messages.chat_id)").Scan(&totalUnreadConversations)
	}

	numRows := len(rows)
	lastMsgKeys := make(map[string]uint64, numRows)
	unreadCheckIDs := make([]string, 0, numRows)
	chatIDsToFetchMembers := make([]string, 0, numRows)

	for _, row := range rows {
		effLastID := row.ConvLastMessageID
		if effLastID == 0 {
			effLastID = row.LastMessageID
		}

		if effLastID > 0 && effLastID > row.DeletedBeforeID {
			lastMsgKeys[row.InternalChatID] = effLastID
		}
		if row.LeftAt == nil && effLastID > row.LastReadID && effLastID > row.DeletedBeforeID {
			unreadCheckIDs = append(unreadCheckIDs, row.InternalChatID)
		}
		if getPeerType(row.InternalChatID) == "chat" {
			chatIDsToFetchMembers = append(chatIDsToFetchMembers, row.InternalChatID)
		}
	}

	msgMap := make(map[string]db_models.Message, len(lastMsgKeys))
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
		effLastID := row.ConvLastMessageID
		if effLastID == 0 {
			effLastID = row.LastMessageID
		}

		if effLastID > 0 && effLastID > row.DeletedBeforeID {
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

	unreadCounts := make(map[string]int64, len(unreadCheckIDs))
	if len(unreadCheckIDs) > 0 {
		type UnreadRes struct {
			ChatID string
			Cnt    int64
		}
		var results []UnreadRes

		unreadQ := db.Instance.Table("messages").
			Select("messages.chat_id, COUNT(messages.id) as cnt").
			Joins("JOIN conversation_members ON conversation_members.internal_chat_id = messages.chat_id").
			Where("conversation_members.user_id = ?", currentUserID).
			Where("messages.chat_id IN ?", unreadCheckIDs).
			Where("messages.from_id != ?", currentUserID).
			Where("messages.local_id > conversation_members.last_read_id").
			Where("messages.local_id > COALESCE(conversation_members.deleted_before_id, 0)")
		unreadQ = db_models.BuildVisibilityFilter(unreadQ, "", currentUserID)
		unreadQ.Group("messages.chat_id").Find(&results)

		for _, res := range results {
			unreadCounts[res.ChatID] = res.Cnt
		}
	}

	chatMembersMap := make(map[string][]int64, len(chatIDsToFetchMembers))
	adminMap := make(map[string]int64, len(chatIDsToFetchMembers))
	ownerMap := make(map[string]int64, len(chatIDsToFetchMembers))
	adminIDsMap := make(map[string][]int64, len(chatIDsToFetchMembers))
	permissionsMap := make(map[string]ChatPermissions, len(chatIDsToFetchMembers))

	if len(chatIDsToFetchMembers) > 0 {
		type ChatMember struct {
			InternalChatID string
			UserID         int64
		}
		var members []ChatMember
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id").
			Where("internal_chat_id IN ? AND left_at IS NULL", chatIDsToFetchMembers).
			Find(&members)

		for _, m := range members {
			chatMembersMap[m.InternalChatID] = append(chatMembersMap[m.InternalChatID], m.UserID)
		}

		type ChatMeta struct {
			InternalID string
			OwnerID    *int64
			Settings   db_models.EncryptedJSON
		}
		var metas []ChatMeta
		db.Instance.Table("conversations").
			Select("internal_id, owner_id, settings").
			Where("internal_id IN ?", chatIDsToFetchMembers).
			Find(&metas)

		for _, o := range metas {
			if o.OwnerID != nil {
				adminMap[o.InternalID] = *o.OwnerID
				ownerMap[o.InternalID] = *o.OwnerID
				adminIDsMap[o.InternalID] = []int64{*o.OwnerID}
			}
			permissionsMap[o.InternalID] = ParseChatPermissions(o.Settings)
		}

		var admins []ChatMember
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id").
			Where("internal_chat_id IN ? AND is_admin = ? AND left_at IS NULL", chatIDsToFetchMembers, true).
			Order("joined_at ASC").
			Find(&admins)

		for _, a := range admins {
			if _, exists := adminMap[a.InternalChatID]; !exists || adminMap[a.InternalChatID] == 0 {
				adminMap[a.InternalChatID] = a.UserID
			}
			found := false
			for _, aid := range adminIDsMap[a.InternalChatID] {
				if aid == a.UserID {
					found = true
					break
				}
			}
			if !found {
				adminIDsMap[a.InternalChatID] = append(adminIDsMap[a.InternalChatID], a.UserID)
			}
		}
	}

	initialMsgs := make([]db_models.Message, 0, len(msgMap))
	for _, msg := range msgMap {
		initialMsgs = append(initialMsgs, msg)
	}
	preloadedMap := db_models.PreloadNestedMessages(db.Instance, initialMsgs, 5)

	readCache := make(map[string][]db_models.MemberReadState)
	if len(lastMsgKeys) > 0 {
		var targetChatIDs []string
		for chatID := range lastMsgKeys {
			targetChatIDs = append(targetChatIDs, chatID)
		}
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

	responseItems := make([]gin.H, 0)
	var userIDs, groupIDs, chatIDs []int64

	for _, row := range rows {
		m := row.ConversationMember
		conv := m.Conversation
		if conv.InternalID == "" {
			db.Instance.Where("internal_id = ?", m.InternalChatID).Order("last_message_id DESC, id DESC").First(&conv)
		}
		pID := chat.DerivePeerID(m.InternalChatID, currentUserID)
		lastMsg, hasMsg := msgMap[m.InternalChatID]

		var msgVK interface{} = nil
		if hasMsg {
			msgVK = lastMsg.ToVKApiStructBatch(db.Instance, 1, currentUserID, pID, preloadedMap, readCache, nil, nil)
		}

		effLastID := row.ConvLastMessageID
		if effLastID == 0 {
			effLastID = m.LastMessageID
		}

		var majorID int64 = 0
		var minorID uint64 = effLastID
		var lastMsgID uint64 = 0
		var lastCMID uint64 = effLastID
		if hasMsg {
			majorID = lastMsg.CreatedAt.Unix()
			minorID = lastMsg.LocalID
			lastMsgID = lastMsg.ID
			lastCMID = lastMsg.LocalID
		}

		var outRead uint64 = 0
		if states, ok := readCache[m.InternalChatID]; ok {
			for _, st := range states {
				if st.UserID != currentUserID && st.LastReadID > outRead {
					outRead = st.LastReadID
				}
			}
		}
		if outRead == 0 && strings.HasPrefix(m.InternalChatID, "dm") {
			parts := strings.Split(m.InternalChatID[2:], "_")
			if len(parts) == 2 && parts[0] == parts[1] {
				outRead = m.LastReadID
			}
		}

		inReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, m.LastReadID, lastCMID, lastMsgID)
		outReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, outRead, lastCMID, lastMsgID)

		conversationObj := gin.H{
			"peer":                         gin.H{"id": pID, "type": getPeerType(m.InternalChatID)},
			"last_message_id":              lastMsgID,
			"last_conversation_message_id": lastCMID,
			"in_read":                      inReadID,
			"out_read":                     outReadID,
			"in_read_cmid":                 m.LastReadID,
			"out_read_cmid":                outRead,
			"important":                    (m.Flags & 1) != 0,
			"unanswered":                   (m.Flags & 2) != 0,
			"push_settings":                account.FetchPushSettings(currentUserID, pID),
			"sort_id": gin.H{
				"major_id": majorID,
				"minor_id": minorID,
			},
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
		conversationObj["can_write"] = canWriteObj

		if uCount, ok := unreadCounts[m.InternalChatID]; ok {
			conversationObj["unread_count"] = uCount
		} else {
			conversationObj["unread_count"] = 0
		}

		var pMsgVK interface{} = nil
		if conv.PinnedMsgID > 0 {
			var pMsg db_models.Message
			if err := db.Instance.Where("chat_id = ? AND local_id = ? AND deleted_at IS NULL", m.InternalChatID, conv.PinnedMsgID).First(&pMsg).Error; err == nil {
				pMsgVK = pMsg.ToVKApiStructBatch(db.Instance, 0, currentUserID, pID, preloadedMap, readCache, nil, nil)
			}
		}

		if getPeerType(m.InternalChatID) == "chat" {
			membersList := chatMembersMap[m.InternalChatID]
			if membersList == nil {
				membersList = []int64{}
			}
			ownerID := ownerMap[m.InternalChatID]
			adminIDs := adminIDsMap[m.InternalChatID]
			if adminIDs == nil {
				adminIDs = []int64{}
			}
			perms := permissionsMap[m.InternalChatID]
			isOwner := (ownerID > 0 && currentUserID == ownerID)
			isAdmin := isOwner
			if !isAdmin {
				for _, aid := range adminIDs {
					if aid == currentUserID {
						isAdmin = true
						break
					}
				}
			}
			isMember := stateStr == "in"

			chatSettingsObj := gin.H{
				"members":       membersList,
				"admin_id":      adminMap[m.InternalChatID],
				"owner_id":      ownerID,
				"admin_ids":     adminIDs,
				"permissions":   perms.ToGinH(),
				"acl":           ComputeChatACL(perms, isOwner, isAdmin, isMember),
				"state":         stateStr,
			}
			if pMsgVK != nil {
				chatSettingsObj["pinned_message"] = pMsgVK
			}
			conversationObj["chat_settings"] = chatSettingsObj
		} else if pMsgVK != nil {
			conversationObj["pinned_message"] = pMsgVK
		}

		responseItems = append(responseItems, gin.H{
			"conversation": conversationObj,
			"last_message": msgVK,
		})

		if extended {
			if hasMsg {
				addID(lastMsg.FromID, &userIDs, &groupIDs, &chatIDs)
			}
			addID(pID, &userIDs, &groupIDs, &chatIDs)

			if getPeerType(m.InternalChatID) == "chat" {
				if membersList, ok := chatMembersMap[m.InternalChatID]; ok {
					for _, memberID := range membersList {
						addID(memberID, &userIDs, &groupIDs, &chatIDs)
					}
				}
			}
		}
	}

	result := gin.H{
		"count":        totalCount,
		"items":        responseItems,
		"unread_count": totalUnreadConversations,
	}

	if extended {
		result["profiles"] = uniqueIDs(userIDs)
		result["groups"] = uniqueIDs(groupIDs)

		uniqueChatIDs := uniqueIDs(chatIDs)
		extendedChats := make([]gin.H, 0, len(uniqueChatIDs))
		internalIDs := make([]string, 0, len(uniqueChatIDs))

		for _, id := range uniqueChatIDs {
			if id > 2000000000 {
				internalIDs = append(internalIDs, fmt.Sprintf("c%d", id-2000000000))
			}
		}

		adminMapExt := make(map[string]int64, len(internalIDs))
		if len(internalIDs) > 0 {
			type ChatOwner struct {
				InternalID string
				OwnerID    *int64
			}
			var owners []ChatOwner
			db.Instance.Table("conversations").
				Select("internal_id, owner_id").
				Where("internal_id IN ?", internalIDs).
				Find(&owners)

			for _, o := range owners {
				if o.OwnerID != nil {
					adminMapExt[o.InternalID] = *o.OwnerID
				}
			}

			type ChatAdmin struct {
				InternalChatID string
				UserID         int64
			}
			var admins []ChatAdmin
			db.Instance.Table("conversation_members").
				Select("internal_chat_id, user_id").
				Where("internal_chat_id IN ? AND is_admin = ? AND left_at IS NULL", internalIDs, true).
				Order("joined_at ASC").
				Find(&admins)

			for _, a := range admins {
				if _, ok := adminMapExt[a.InternalChatID]; !ok || adminMapExt[a.InternalChatID] == 0 {
					adminMapExt[a.InternalChatID] = a.UserID
				}
			}
		}

		for _, id := range uniqueChatIDs {
			if id <= 2000000000 {
				continue
			}

			localID := id - 2000000000
			intKey := fmt.Sprintf("c%d", localID)

			members := chatMembersMap[intKey]
			if members == nil {
				members = []int64{}
			}

			extendedChats = append(extendedChats, gin.H{
				"id":          id,
				"type":        "chat",
				"admin_id":    adminMapExt[intKey],
				"left":        0,
				"kicked":      0,
				"title":       "",
				"description": "",
				"photo_50":    "",
				"photo_100":   "",
				"photo_200":   "",
				"members":     members,
			})
		}

		result["chats"] = extendedChats
	}

	c.JSON(http.StatusOK, gin.H{"response": result})
}

func GetConversationsById(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID := val.(int64)

	peerIDsStr := r.Get(c, "peer_ids")
	if peerIDsStr == "" {
		r.Reject(c, 100, "One of the parameters is missing: peer_ids")
		return
	}

	extended := r.GetBool(c, "extended", false)
	parts := strings.Split(peerIDsStr, ",")
	var targetChatIDs []string
	for _, p := range parts {
		pTrim := strings.TrimSpace(p)
		if strings.HasPrefix(pTrim, "dm") || strings.HasPrefix(pTrim, "c") || strings.HasPrefix(pTrim, "g") {
			targetChatIDs = append(targetChatIDs, pTrim)
		} else if id, err := strconv.ParseInt(pTrim, 10, 64); err == nil {
			internalID := chat.GetInternalChatID(id, currentUserID)
			targetChatIDs = append(targetChatIDs, internalID)
		}
	}

	var rows []db_models.ConversationMember
	var err error
	if currentUserID == 0 {
		var convs []db_models.Conversation
		err = db.Instance.Where("internal_id IN ?", targetChatIDs).Find(&convs).Error
		for _, conv := range convs {
			rows = append(rows, db_models.ConversationMember{
				InternalChatID: conv.InternalID,
				LastMessageID:  conv.LastMessageID,
				Conversation:   conv,
			})
		}
	} else {
		err = db.Instance.Where("user_id = ? AND internal_chat_id IN ?", currentUserID, targetChatIDs).
			Preload("Conversation", func(db *gorm.DB) *gorm.DB {
				return db.Order("conversations.last_message_id DESC, conversations.id DESC")
			}).
			Find(&rows).Error
		if err == nil {
			foundChatIDs := make(map[string]bool, len(rows))
			for _, r := range rows {
				foundChatIDs[r.InternalChatID] = true
			}
			for _, tID := range targetChatIDs {
				if !foundChatIDs[tID] && getPeerType(tID) == "chat" {
					var conv db_models.Conversation
					if errConv := db.Instance.Where("internal_id = ?", tID).First(&conv).Error; errConv == nil && conv.InternalID != "" {
						rows = append(rows, db_models.ConversationMember{
							UserID:         currentUserID,
							InternalChatID: tID,
							Conversation:   conv,
						})
					}
				}
			}
		}
	}

	if err != nil {
		r.Reject(c, 10, "Internal server error")
		return
	}

	lastMsgKeys := make(map[string]uint64)
	chatIDsToFetchMembers := make([]string, 0)
	for _, row := range rows {
		effLastID := row.Conversation.LastMessageID
		if effLastID == 0 {
			effLastID = row.LastMessageID
		}

		if effLastID > 0 && effLastID > row.DeletedBeforeID {
			lastMsgKeys[row.InternalChatID] = effLastID
		}
		if getPeerType(row.InternalChatID) == "chat" {
			chatIDsToFetchMembers = append(chatIDsToFetchMembers, row.InternalChatID)
		}
	}

	chatMembersMap := make(map[string][]int64)
	adminMap := make(map[string]int64, len(chatIDsToFetchMembers))
	ownerMap := make(map[string]int64, len(chatIDsToFetchMembers))
	adminIDsMap := make(map[string][]int64, len(chatIDsToFetchMembers))
	permissionsMap := make(map[string]ChatPermissions, len(chatIDsToFetchMembers))

	if len(chatIDsToFetchMembers) > 0 {
		type ChatMember struct {
			InternalChatID string
			UserID         int64
		}
		var members []ChatMember
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id").
			Where("internal_chat_id IN ? AND left_at IS NULL", chatIDsToFetchMembers).
			Find(&members)

		for _, m := range members {
			chatMembersMap[m.InternalChatID] = append(chatMembersMap[m.InternalChatID], m.UserID)
		}

		type ChatMeta struct {
			InternalID string
			OwnerID    *int64
			Settings   db_models.EncryptedJSON
		}
		var metas []ChatMeta
		db.Instance.Table("conversations").
			Select("internal_id, owner_id, settings").
			Where("internal_id IN ?", chatIDsToFetchMembers).
			Find(&metas)

		for _, o := range metas {
			if o.OwnerID != nil {
				adminMap[o.InternalID] = *o.OwnerID
				ownerMap[o.InternalID] = *o.OwnerID
				adminIDsMap[o.InternalID] = []int64{*o.OwnerID}
			}
			permissionsMap[o.InternalID] = ParseChatPermissions(o.Settings)
		}

		var admins []ChatMember
		db.Instance.Table("conversation_members").
			Select("internal_chat_id, user_id").
			Where("internal_chat_id IN ? AND is_admin = ? AND left_at IS NULL", chatIDsToFetchMembers, true).
			Order("joined_at ASC").
			Find(&admins)

		for _, a := range admins {
			if _, exists := adminMap[a.InternalChatID]; !exists || adminMap[a.InternalChatID] == 0 {
				adminMap[a.InternalChatID] = a.UserID
			}
			found := false
			for _, aid := range adminIDsMap[a.InternalChatID] {
				if aid == a.UserID {
					found = true
					break
				}
			}
			if !found {
				adminIDsMap[a.InternalChatID] = append(adminIDsMap[a.InternalChatID], a.UserID)
			}
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
		effLastID := row.Conversation.LastMessageID
		if effLastID == 0 {
			effLastID = row.LastMessageID
		}

		if effLastID > 0 && effLastID > row.DeletedBeforeID {
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

	initialMsgs := make([]db_models.Message, 0, len(msgMap))
	for _, msg := range msgMap {
		initialMsgs = append(initialMsgs, msg)
	}
	preloadedMap := db_models.PreloadNestedMessages(db.Instance, initialMsgs, 5)

	readCache := make(map[string][]db_models.MemberReadState)
	if len(lastMsgKeys) > 0 {
		var targetChatIDs []string
		for chatID := range lastMsgKeys {
			targetChatIDs = append(targetChatIDs, chatID)
		}
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

	unreadCountsById := make(map[string]int64, len(targetChatIDs))
	if len(targetChatIDs) > 0 && currentUserID != 0 {
		type UnreadRes struct {
			ChatID string
			Cnt    int64
		}
		var results []UnreadRes

		unreadQ := db.Instance.Table("messages").
			Select("messages.chat_id, COUNT(messages.id) as cnt").
			Joins("JOIN conversation_members ON conversation_members.internal_chat_id = messages.chat_id").
			Where("conversation_members.user_id = ?", currentUserID).
			Where("messages.chat_id IN ?", targetChatIDs).
			Where("messages.from_id != ?", currentUserID).
			Where("messages.local_id > conversation_members.last_read_id").
			Where("messages.local_id > COALESCE(conversation_members.deleted_before_id, 0)")
		unreadQ = db_models.BuildVisibilityFilter(unreadQ, "", currentUserID)
		unreadQ.Group("messages.chat_id").Find(&results)

		for _, res := range results {
			unreadCountsById[res.ChatID] = res.Cnt
		}
	}

	responseItems := make([]gin.H, 0)
	var userIDs, groupIDs, chatIDs []int64

	for _, m := range rows {
		pID := chat.DerivePeerID(m.InternalChatID, currentUserID)
		lastMsg, hasMsg := msgMap[m.InternalChatID]

		effLastID := m.Conversation.LastMessageID
		if effLastID == 0 {
			effLastID = m.LastMessageID
		}

		var majorID int64 = 0
		var minorID uint64 = effLastID
		var lastMsgID uint64 = 0
		var lastCMID uint64 = effLastID
		if hasMsg {
			majorID = lastMsg.CreatedAt.Unix()
			minorID = lastMsg.LocalID
			lastMsgID = lastMsg.ID
			lastCMID = lastMsg.LocalID
		}

		var outRead uint64 = 0
		if states, ok := readCache[m.InternalChatID]; ok {
			for _, st := range states {
				if st.UserID != currentUserID && st.LastReadID > outRead {
					outRead = st.LastReadID
				}
			}
		}
		if outRead == 0 && strings.HasPrefix(m.InternalChatID, "dm") {
			parts := strings.Split(m.InternalChatID[2:], "_")
			if len(parts) == 2 && parts[0] == parts[1] {
				outRead = m.LastReadID
			}
		}

		var uCount int64 = 0
		if uc, ok := unreadCountsById[m.InternalChatID]; ok {
			uCount = uc
		}

		inReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, m.LastReadID, lastCMID, lastMsgID)
		outReadID := chat.ResolveGlobalMsgID(db.Instance, m.InternalChatID, outRead, lastCMID, lastMsgID)

		convObj := gin.H{
			"peer":                         gin.H{"id": pID, "type": getPeerType(m.InternalChatID)},
			"last_message_id":              lastMsgID,
			"last_conversation_message_id": lastCMID,
			"in_read":                      inReadID,
			"out_read":                     outReadID,
			"in_read_cmid":                 m.LastReadID,
			"out_read_cmid":                outRead,
			"unread_count":                 uCount,
			"important":                    (m.Flags & 1) != 0,
			"unanswered":                   (m.Flags & 2) != 0,
			"push_settings":                account.FetchPushSettings(currentUserID, pID),
			"sort_id": gin.H{
				"major_id": majorID,
				"minor_id": minorID,
			},
		}

		conv := m.Conversation
		if conv.InternalID == "" {
			db.Instance.Where("internal_id = ?", m.InternalChatID).First(&conv)
		}
		var pMsgVK interface{} = nil
		if conv.PinnedMsgID > 0 {
			var pMsg db_models.Message
			if err := db.Instance.Where("chat_id = ? AND local_id = ? AND deleted_at IS NULL", m.InternalChatID, conv.PinnedMsgID).First(&pMsg).Error; err == nil {
				pMsgVK = pMsg.ToVKApiStructBatch(db.Instance, 0, currentUserID, pID, preloadedMap, readCache, nil, nil)
			}
		}

		canWriteObj := gin.H{"allowed": true}
		stateStr := "in"
		if getPeerType(m.InternalChatID) == "chat" {
			if m.JoinedAt.IsZero() || (m.UserID != currentUserID && currentUserID != 0) {
				stateStr = "out"
				canWriteObj = gin.H{"allowed": false, "reason": 917}
			} else if m.LeftAt != nil {
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
		}
		convObj["can_write"] = canWriteObj

		if getPeerType(m.InternalChatID) == "chat" {
			membersList := chatMembersMap[m.InternalChatID]
			if membersList == nil {
				membersList = []int64{}
			}
			ownerID := ownerMap[m.InternalChatID]
			adminIDs := adminIDsMap[m.InternalChatID]
			if adminIDs == nil {
				adminIDs = []int64{}
			}
			perms := permissionsMap[m.InternalChatID]
			isOwner := (ownerID > 0 && currentUserID == ownerID)
			isAdmin := isOwner
			if !isAdmin {
				for _, aid := range adminIDs {
					if aid == currentUserID {
						isAdmin = true
						break
					}
				}
			}
			isMember := stateStr == "in"

			chatSettingsObj := gin.H{
				"members":       membersList,
				"admin_id":      adminMap[m.InternalChatID],
				"owner_id":      ownerID,
				"admin_ids":     adminIDs,
				"permissions":   perms.ToGinH(),
				"acl":           ComputeChatACL(perms, isOwner, isAdmin, isMember),
				"state":         stateStr,
			}
			if pMsgVK != nil {
				chatSettingsObj["pinned_message"] = pMsgVK
			}
			convObj["chat_settings"] = chatSettingsObj
		} else if pMsgVK != nil {
			convObj["pinned_message"] = pMsgVK
		}

		responseItems = append(responseItems, convObj)

		if extended {
			addID(pID, &userIDs, &groupIDs, &chatIDs)
			if hasMsg {
				addID(lastMsg.FromID, &userIDs, &groupIDs, &chatIDs)
			}

			if getPeerType(m.InternalChatID) == "chat" {
				if membersList, ok := chatMembersMap[m.InternalChatID]; ok {
					for _, memberID := range membersList {
						addID(memberID, &userIDs, &groupIDs, &chatIDs)
					}
				}
			}
		}
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

func GetConversationMembers(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID := val.(int64)

	peerID := r.GetInt64(c, "peer_id", 0)
	uIDParam := r.GetInt64(c, "user_id", 0)
	internalChatId := chat.ResolveChatID(r.Get(c, "chat_id"), peerID, uIDParam, currentUserID)

	if internalChatId == "" && peerID == 0 {
		r.Reject(c, 100, "One of the parameters is missing: peer_id")
		return
	}
	if internalChatId == "" {
		internalChatId = chat.GetInternalChatID(peerID, currentUserID)
	}

	extended := r.GetBool(c, "extended", false)

	if currentUserID != 0 {
		var check db_models.ConversationMember
		err := db.Instance.Where("internal_chat_id = ? AND user_id = ? AND left_at IS NULL", internalChatId, currentUserID).First(&check).Error

		if err != nil {
			r.Reject(c, 917, "You don't have access to this chat")
			return
		}
	}

	var members []db_models.ConversationMember
	var userIDs, groupIDs, chatIDs []int64
	items := make([]gin.H, 0)

	if peerID > 2000000000 || strings.HasPrefix(internalChatId, "c") {
		var conv db_models.Conversation
		db.Instance.Select("owner_id, settings").Where("internal_id = ?", internalChatId).First(&conv)
		perms := ParseChatPermissions(conv.Settings)

		var ownerID int64
		if conv.OwnerID != nil {
			ownerID = *conv.OwnerID
		}

		db.Instance.Where("internal_chat_id = ? AND left_at IS NULL", internalChatId).Find(&members)

		var adminIDs []int64
		if ownerID > 0 {
			adminIDs = append(adminIDs, ownerID)
		}

		var isCallerOwner = (ownerID > 0 && currentUserID == ownerID)
		var isCallerAdmin = isCallerOwner

		for _, m := range members {
			if m.IsAdmin {
				found := false
				for _, aid := range adminIDs {
					if aid == m.UserID {
						found = true
						break
					}
				}
				if !found {
					adminIDs = append(adminIDs, m.UserID)
				}
			}
			if m.UserID == currentUserID && m.IsAdmin {
				isCallerAdmin = true
			}
		}

		for _, m := range members {
			item := gin.H{
				"member_id": m.UserID,
				"invited_by": m.InvitedBy,
				"join_date": m.JoinedAt.Unix(),
			}
			isOwner := (ownerID > 0 && m.UserID == ownerID)
			isAdmin := isOwner || m.IsAdmin

			canKick := false
			if m.UserID != currentUserID {
				if isCallerOwner {
					canKick = true
				} else if isCallerAdmin {
					canKick = !isAdmin
				}
			}

			if isOwner {
				item["is_admin"] = true
				item["is_owner"] = true
			} else if m.IsAdmin {
				item["is_admin"] = true
				item["is_moderator"] = true
			}
			item["can_kick"] = canKick

			items = append(items, item)

			if extended {
				addID(m.UserID, &userIDs, &groupIDs, &chatIDs)
				addID(m.InvitedBy, &userIDs, &groupIDs, &chatIDs)
			}
		}

		result := gin.H{
			"count": len(items),
			"items": items,
			"chat_settings": gin.H{
				"owner_id":      ownerID,
				"admin_id":      ownerID,
				"admin_ids":     adminIDs,
				"permissions":   perms.ToGinH(),
				"acl":           ComputeChatACL(perms, isCallerOwner, isCallerAdmin, true),
				"members_count": len(members),
				"state":         "in",
			},
		}

		if extended {
			result["profiles"] = uniqueIDs(userIDs)
			result["groups"] = uniqueIDs(groupIDs)
			result["chats"] = uniqueIDs(chatIDs)
		}

		c.JSON(http.StatusOK, gin.H{"response": result})
		return
	} else {
		participants := []int64{currentUserID, peerID}
		for _, p := range participants {
			items = append(items, gin.H{
				"member_id": p,
			})
			if extended {
				addID(p, &userIDs, &groupIDs, &chatIDs)
			}
		}
	}

	result := gin.H{
		"count": len(items),
		"items": items,
	}

	if extended {
		result["profiles"] = uniqueIDs(userIDs)
		result["groups"] = uniqueIDs(groupIDs)
		result["chats"] = uniqueIDs(chatIDs)
	}

	c.JSON(http.StatusOK, gin.H{"response": result})
}

func getPeerType(internalChatId string) string {
	if strings.HasPrefix(internalChatId, "c") {
		return "chat"
	}
	if strings.HasPrefix(internalChatId, "g") {
		return "community"
	}
	if strings.HasPrefix(internalChatId, "dm") {
		return "user"
	}
	return "unknown"
}

func uniqueIDs(ids []int64) []int64 {
	m := make(map[int64]bool)
	var res []int64
	for _, id := range ids {
		if !m[id] {
			m[id] = true
			res = append(res, id)
		}
	}
	return res
}

func buildInPairs(keys map[string]uint64) [][]interface{} {
	res := make([][]interface{}, 0, len(keys))
	for k, v := range keys {
		res = append(res, []interface{}{k, v})
	}
	return res
}

func addID(id int64, u *[]int64, g *[]int64, c *[]int64) {
	if id > 2000000000 {
		*c = append(*c, id)
	} else if id > 0 && id < 2000000000 {
		*u = append(*u, id)
	} else if id < 0 {
		*g = append(*g, -id)
	}
}

func GetChat(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID := val.(int64)

	chatIDStr := r.Get(c, "chat_id")
	chatIDsStr := r.Get(c, "chat_ids")

	if chatIDStr == "" && chatIDsStr == "" {
		if pID := r.GetInt64(c, "peer_id", 0); pID > 2000000000 {
			chatIDStr = strconv.FormatInt(pID-2000000000, 10)
		}
	}

	if chatIDStr == "" && chatIDsStr == "" {
		r.Reject(c, 100, "One of the parameters is missing: chat_id or chat_ids")
		return
	}

	var rawChatIDs []int64
	singleMode := false

	if chatIDsStr != "" {
		for _, s := range strings.Split(chatIDsStr, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
				if id > 2000000000 {
					id -= 2000000000
				}
				rawChatIDs = append(rawChatIDs, id)
			}
		}
	} else if chatIDStr != "" {
		if id, err := strconv.ParseInt(strings.TrimSpace(chatIDStr), 10, 64); err == nil && id > 0 {
			if id > 2000000000 {
				id -= 2000000000
			}
			rawChatIDs = append(rawChatIDs, id)
			singleMode = true
		}
	}

	rawChatIDs = core.UniqueIDs(rawChatIDs)
	if len(rawChatIDs) == 0 {
		r.Reject(c, 100, "Invalid chat_id or chat_ids")
		return
	}

	resultChats := make([]db_models.VKApiChat, 0, len(rawChatIDs))

	for _, localChatID := range rawChatIDs {
		internalChatID := "c" + strconv.FormatInt(localChatID, 10)

		var conv db_models.Conversation
		_ = db.Instance.Where("internal_id = ?", internalChatID).First(&conv).Error

		var members []db_models.ConversationMember
		db.Instance.Where("internal_chat_id = ? AND left_at IS NULL", internalChatID).Order("joined_at ASC").Find(&members)

		var isMember bool
		var leftState int
		var kickedState int
		var adminID int64

		if conv.OwnerID != nil && *conv.OwnerID > 0 {
			adminID = *conv.OwnerID
		}

		userIDs := make([]int64, 0, len(members))
		for _, m := range members {
			userIDs = append(userIDs, m.UserID)
			if m.UserID == currentUserID {
				isMember = true
			}
			if m.IsAdmin && adminID == 0 {
				adminID = m.UserID
			}
		}

		if !isMember && currentUserID != 0 {
			var check db_models.ConversationMember
			if err := db.Instance.Where("internal_chat_id = ? AND user_id = ?", internalChatID, currentUserID).First(&check).Error; err == nil {
				if check.LeftAt != nil {
					var lastKickMsg db_models.Message
					if errK := db.Instance.Where("chat_id = ? AND action = ? AND action_mid = ?", internalChatID, "chat_kick_user", currentUserID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
						if lastKickMsg.FromID != currentUserID {
							kickedState = 1
						} else {
							leftState = 1
						}
					} else {
						leftState = 1
					}
				} else {
					leftState = 1
				}
			} else {
				if singleMode {
					r.Reject(c, 917, "You don't have access to this chat")
					return
				}
				continue
			}
		}

		title := conv.Title
		if title == "" {
			title = fmt.Sprintf("Chat %d", localChatID)
		}

		chatObj := db_models.VKApiChat{
			ID:           localChatID,
			Type:         "chat",
			Title:        title,
			AdminID:      adminID,
			Users:        userIDs,
			MembersCount: len(userIDs),
			PushSettings: account.FetchPushSettings(currentUserID, 2000000000+localChatID),
		}

		if leftState > 0 {
			chatObj.Left = 1
		}
		if kickedState > 0 {
			chatObj.Kicked = 1
		}

		resultChats = append(resultChats, chatObj)
	}

	if singleMode {
		if len(resultChats) == 0 {
			r.Reject(c, 917, "You don't have access to this chat")
			return
		}
		c.JSON(http.StatusOK, gin.H{"response": resultChats[0]})
	} else {
		c.JSON(http.StatusOK, gin.H{"response": resultChats})
	}
}

func GetChatUsers(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID := val.(int64)

	chatIDStr := r.Get(c, "chat_id")
	if chatIDStr == "" {
		if pID := r.GetInt64(c, "peer_id", 0); pID > 2000000000 {
			chatIDStr = strconv.FormatInt(pID-2000000000, 10)
		}
	}

	if chatIDStr == "" {
		r.Reject(c, 100, "One of the parameters is missing: chat_id")
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil || chatID <= 0 {
		r.Reject(c, 100, "Invalid chat_id")
		return
	}
	if chatID > 2000000000 {
		chatID -= 2000000000
	}

	internalChatID := "c" + strconv.FormatInt(chatID, 10)

	if currentUserID != 0 {
		var check db_models.ConversationMember
		err := db.Instance.Where("internal_chat_id = ? AND user_id = ? AND left_at IS NULL", internalChatID, currentUserID).First(&check).Error
		if err != nil {
			r.Reject(c, 917, "You don't have access to this chat")
			return
		}
	}

	var members []db_models.ConversationMember
	db.Instance.Where("internal_chat_id = ? AND left_at IS NULL", internalChatID).Order("joined_at ASC").Find(&members)

	userIDs := make([]int64, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}

	c.JSON(http.StatusOK, gin.H{"response": userIDs})
}