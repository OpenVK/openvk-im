package history

import (
	"net/http"
	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func GetHistory(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)
	apiV := core.GetApiV(c)

	var peerID int64
	var uIDParam int64
	if pID := c.Query("peer_id"); pID != "" {
		peerID, _ = strconv.ParseInt(pID, 10, 64)
	}
	if uID := c.Query("user_id"); uID != "" {
		uIDParam, _ = strconv.ParseInt(uID, 10, 64)
		if peerID == 0 {
			peerID = uIDParam
		}
	} else if chatIDParam := c.Query("chat_id"); chatIDParam != "" {
		if id, err := strconv.ParseInt(chatIDParam, 10, 64); err == nil && peerID == 0 {
			peerID = 2000000000 + id
		}
	}

	chatID := chat.ResolveChatID(c.Query("chat_id"), peerID, uIDParam, currentUserID)
	if chatID == "" {
		r.Reject(c, 100, "One of the parameters is missing: peer_id, user_id or chat_id")
		return
	}

	if peerID == 0 {
		peerID = chat.DerivePeerID(chatID, currentUserID)
	}

	isGroupChat := strings.HasPrefix(chatID, "c")

	var member *db_models.ConversationMember
	if currentUserID != 0 {
		if isGroupChat {
			var err error
			member, err = chat.GetMember(db.Instance, chatID, currentUserID)
			if err != nil || member == nil {
				r.Reject(c, 917, "You don't have access to this chat")
				return
			}
		} else {
			member, _ = chat.GetMember(db.Instance, chatID, currentUserID)
		}
	}

	count, _ := strconv.Atoi(c.DefaultQuery("count", "20"))
	if count > 200 {
		count = 200
	} else if count < 1 {
		count = 20
	}

	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	startID, _ := strconv.ParseInt(c.Query("start_message_id"), 10, 64)
	startCMID, _ := strconv.ParseInt(c.Query("start_cmid"), 10, 64)
	rev, _ := strconv.Atoi(c.DefaultQuery("rev", "0"))
	previewLength, _ := strconv.Atoi(c.DefaultQuery("preview_length", "0"))

	var startLocalID uint64
	if startCMID > 0 {
		startLocalID = uint64(startCMID)
	} else if startID > 0 {
		var foundMsg db_models.Message
		if err := db.Instance.Select("local_id").Where("chat_id = ? AND id = ?", chatID, startID).First(&foundMsg).Error; err == nil {
			startLocalID = foundMsg.LocalID
		} else if err := db.Instance.Select("local_id").Where("chat_id = ? AND local_id = ?", chatID, startID).First(&foundMsg).Error; err == nil {
			startLocalID = foundMsg.LocalID
		} else {
			startLocalID = uint64(startID)
		}
	}

	var msgs []db_models.Message
	query := db.Instance.Where("chat_id = ?", chatID)
	query = db_models.BuildVisibilityFilter(query, chatID, currentUserID)

	if startLocalID > 0 {
		if offset < 0 {
			absOffset := int(-offset)

			if rev == 1 {
				var minID uint64
				subQ := db.Instance.Model(&db_models.Message{}).Where("chat_id = ? AND local_id <= ?", chatID, startLocalID)
				subQ = db_models.BuildVisibilityFilter(subQ, chatID, currentUserID)
				if err := subQ.Select("local_id").Order("local_id DESC").Limit(1).Offset(absOffset-1).Scan(&minID).Error; err == nil && minID > 0 {
					query = query.Where("local_id >= ?", minID)
				}
			} else {
				var maxID uint64
				subQ := db.Instance.Model(&db_models.Message{}).Where("chat_id = ? AND local_id >= ?", chatID, startLocalID)
				subQ = db_models.BuildVisibilityFilter(subQ, chatID, currentUserID)
				if err := subQ.Select("local_id").Order("local_id ASC").Limit(1).Offset(absOffset-1).Scan(&maxID).Error; err == nil && maxID > 0 {
					query = query.Where("local_id <= ?", maxID)
				}
			}

			offset = 0
		} else {
			if rev == 1 {
				query = query.Where("local_id >= ?", startLocalID)
			} else {
				query = query.Where("local_id <= ?", startLocalID)
			}
		}
	}

	order := "local_id DESC"
	if rev == 1 {
		order = "local_id ASC"
	}

	var totalCount int64
	query.Model(&db_models.Message{}).Count(&totalCount)

	err := query.Preload("Conversation").Order(order).Limit(count).Offset(offset).Find(&msgs).Error
	if err != nil {
		r.Reject(c, 10, "Internal server error during DB query")
		return
	}

	readCache := make(map[string][]db_models.MemberReadState)
	var memberStates []db_models.MemberReadState
	db.Instance.Table("conversation_members").
		Select("user_id, last_read_id").
		Where("internal_chat_id = ?", chatID).
		Scan(&memberStates)
	readCache[chatID] = memberStates

	preloadedMap := db_models.PreloadNestedMessages(db.Instance, msgs, 10)
	importantMap := db_models.PreloadImportantMapFromMessages(db.Instance, currentUserID, msgs, preloadedMap)

	var chatMembers []int64
	var chatAdminID int64
	if isGroupChat {
		type ChatMember struct {
			UserID  int64
			IsAdmin bool
		}
		var members []ChatMember
		db.Instance.Table("conversation_members").
			Select("user_id, is_admin").
			Where("internal_chat_id = ? AND left_at IS NULL", chatID).
			Order("joined_at ASC").
			Find(&members)

		for _, mem := range members {
			chatMembers = append(chatMembers, mem.UserID)
			if mem.IsAdmin && chatAdminID == 0 {
				chatAdminID = mem.UserID
			}
		}
	}

	var responseItems any

	if apiV.IsOlderThan(5, 80) {
		legacyItems := make([]db_models.VKApiMessageLegacy, len(msgs))
		for i, m := range msgs {
			vkMsg := m.ToVKApiStructBatchLegacy(db.Instance, 10, currentUserID, peerID, preloadedMap, readCache, nil, importantMap)
			if previewLength > 0 {
				vkMsg.Body = core.TruncateWords(vkMsg.Body, previewLength)
			}
			if isGroupChat {
				vkMsg.UsersCount = len(chatMembers)
				activeCount := 10
				if len(chatMembers) < activeCount {
					activeCount = len(chatMembers)
				}
				vkMsg.ChatActive = chatMembers[:activeCount]
				if vkMsg.AdminID == 0 {
					vkMsg.AdminID = chatAdminID
				}
			}
			legacyItems[i] = vkMsg
		}
		responseItems = legacyItems
	} else {
		modernItems := make([]db_models.VKApiMessage, len(msgs))
		for i, m := range msgs {
			vkMsg := m.ToVKApiStructBatch(db.Instance, 10, currentUserID, peerID, preloadedMap, readCache, nil, importantMap)
			if previewLength > 0 {
				vkMsg.Text = core.TruncateWords(vkMsg.Text, previewLength)
			}
			modernItems[i] = vkMsg
		}
		responseItems = modernItems
	}

	var unreadCount int64
	if member != nil && member.LeftAt == nil {
		unreadQuery := db.Instance.Model(&db_models.Message{}).
			Where("chat_id = ? AND local_id > ? AND from_id != ?", chatID, member.LastReadID, currentUserID)
		if member.DeletedBeforeID > 0 {
			unreadQuery = unreadQuery.Where("local_id > ?", member.DeletedBeforeID)
		}
		unreadQuery = db_models.BuildVisibilityFilter(unreadQuery, chatID, currentUserID)
		unreadQuery.Count(&unreadCount)
	}

	response := gin.H{
		"count":  totalCount,
		"items":  responseItems,
		"unread": unreadCount,
	}

	if c.Query("extended") == "1" {
		var userIDs, groupIDs, chatIDs []int64

		if apiV.IsOlderThan(5, 80) {
			if legacyList, ok := responseItems.([]db_models.VKApiMessageLegacy); ok {
				core.CollectAllEntityIDsLegacy(legacyList, &userIDs, &groupIDs, &chatIDs)
			}
		} else {
			if modernList, ok := responseItems.([]db_models.VKApiMessage); ok {
				core.CollectAllEntityIDs(modernList, &userIDs, &groupIDs)
			}
		}

		if isGroupChat {
			var members []db_models.ConversationMember
			db.Instance.Where("internal_chat_id = ? AND left_at IS NULL", chatID).Find(&members)

			for _, m := range members {
				addID(m.UserID, &userIDs, &groupIDs, &chatIDs)
				addID(m.InvitedBy, &userIDs, &groupIDs, &chatIDs)
			}
		} else {
			participants := []int64{currentUserID, peerID}
			for _, p := range participants {
				addID(p, &userIDs, &groupIDs, &chatIDs)
			}
		}

		response["profiles"] = uniqueIDs(userIDs)
		response["groups"] = uniqueIDs(groupIDs)
		if isGroupChat && len(chatIDs) > 0 {
			response["chats"] = uniqueIDs(chatIDs)
		}

		if !apiV.IsOlderThan(5, 80) {
			convs := make([]gin.H, 0)
			if member != nil {
				var lastMsgID uint64 = 0
				var lastCMID uint64 = 0
				if len(msgs) > 0 {
					lastMsgID = msgs[0].ID
					lastCMID = msgs[0].LocalID
				}
				var outRead uint64 = 0
				if states, ok := readCache[chatID]; ok {
					for _, st := range states {
						if st.UserID != currentUserID && st.LastReadID > outRead {
							outRead = st.LastReadID
						}
					}
				}
				if outRead == 0 && strings.HasPrefix(chatID, "dm") {
					parts := strings.Split(chatID[2:], "_")
					if len(parts) == 2 && parts[0] == parts[1] {
						outRead = member.LastReadID
					}
				}

				pType := "user"
				if isGroupChat {
					pType = "chat"
				} else if peerID < 0 {
					pType = "group"
				}

				inReadID := chat.ResolveGlobalMsgID(db.Instance, chatID, member.LastReadID, lastCMID, lastMsgID)
				outReadID := chat.ResolveGlobalMsgID(db.Instance, chatID, outRead, lastCMID, lastMsgID)

				cObj := gin.H{
					"peer": gin.H{
						"id":   peerID,
						"type": pType,
					},
					"last_message_id":              lastMsgID,
					"last_conversation_message_id": lastCMID,
					"in_read":                      inReadID,
					"out_read":                     outReadID,
					"in_read_cmid":                 member.LastReadID,
					"out_read_cmid":                outRead,
					"unread_count":                 unreadCount,
					"important":                    (member.Flags & 1) != 0,
					"unanswered":                   (member.Flags & 2) != 0,
					"push_settings":                gin.H{"sound": 1, "disabled_until": 0},
					"can_write":                    gin.H{"allowed": true},
				}
				if isGroupChat {
					cObj["chat_settings"] = gin.H{
						"members_count": len(chatMembers),
						"title":         member.Conversation.Title,
						"admin_id":      chatAdminID,
						"state":         "in",
						"active_ids":    chatMembers,
					}
				}
				convs = append(convs, cObj)
			}
			response["conversations"] = convs
		}
	}

	c.JSON(http.StatusOK, gin.H{"response": response})
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
