package messages

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	lp_models "ovk-im/src/models/longpoll"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/chats"
	"ovk-im/src/transport/endpoints/core"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type MessagesHandler struct {
	core.BaseHandler
}

type ForwardPayload struct {
	OwnerID                int64    `json:"owner_id"`
	PeerID                 int64    `json:"peer_id"`
	ConversationMessageIDs []uint64 `json:"conversation_message_ids"`
	MessageIDs             []uint64 `json:"message_ids"`
	IsReply                bool     `json:"is_reply"`
}

type MultiPeerResponse struct {
	PeerID                int64         `json:"peer_id"`
	MessageID             uint64        `json:"message_id,omitempty"`
	ConversationMessageID uint64        `json:"conversation_message_id,omitempty"`
	Error                 *core.VKError `json:"error,omitempty"`
}

var reInvisibleSpaces = regexp.MustCompile(`[\s\x{200b}\x{feff}\x{00a0}\x{200c}\x{200d}]+`)

type MentionItem struct {
	UserID      int64
	MentionType string
}

var (
	reBracketMention = regexp.MustCompile(`\[(id|club|public|all|online)(\d+)?(?:\|([^\]]*))?\]`)
	reWordMention    = regexp.MustCompile(`(?:^|[\s(])([@*])(all|online|id\d+)\b`)
)

func parseMentions(text string) []MentionItem {
	var result []MentionItem
	seen := make(map[string]bool)

	// 1. Match [id123|Name], [club123|Name], [all|Всем], [online|Онлайн]
	matches := reBracketMention.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		tagType := strings.ToLower(m[1])
		rawID := m[2]

		switch tagType {
		case "id":
			if id, err := strconv.ParseInt(rawID, 10, 64); err == nil && id > 0 {
				key := fmt.Sprintf("user:%d", id)
				if !seen[key] {
					seen[key] = true
					result = append(result, MentionItem{UserID: id, MentionType: "user"})
				}
			}
		case "club", "public":
			if id, err := strconv.ParseInt(rawID, 10, 64); err == nil && id > 0 {
				key := fmt.Sprintf("user:%d", -id)
				if !seen[key] {
					seen[key] = true
					result = append(result, MentionItem{UserID: -id, MentionType: "user"})
				}
			}
		case "all":
			if !seen["all"] {
				seen["all"] = true
				result = append(result, MentionItem{UserID: 0, MentionType: "all"})
			}
		case "online":
			if !seen["online"] {
				seen["online"] = true
				result = append(result, MentionItem{UserID: -1, MentionType: "online"})
			}
		}
	}

	// 2. Match @all, @online, @id123, *all, *online, *id123
	wordMatches := reWordMention.FindAllStringSubmatch(text, -1)
	for _, m := range wordMatches {
		target := strings.ToLower(m[2])
		if target == "all" {
			if !seen["all"] {
				seen["all"] = true
				result = append(result, MentionItem{UserID: 0, MentionType: "all"})
			}
		} else if target == "online" {
			if !seen["online"] {
				seen["online"] = true
				result = append(result, MentionItem{UserID: -1, MentionType: "online"})
			}
		} else if strings.HasPrefix(target, "id") {
			if id, err := strconv.ParseInt(target[2:], 10, 64); err == nil && id > 0 {
				key := fmt.Sprintf("user:%d", id)
				if !seen[key] {
					seen[key] = true
					result = append(result, MentionItem{UserID: id, MentionType: "user"})
				}
			}
		}
	}

	return result
}

func Send(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		r.Reject(c, 15, "Access denied: Unauthorized")
		return
	}
	currentUserID, ok := val.(int64)
	if !ok || currentUserID == 0 {
		r.Reject(c, 15, "Access denied: Invalid user session")
		return
	}

	groupID := r.GetInt64(c, "group_id", 0)
	senderID := currentUserID
	if groupID > 0 {
		senderID = -groupID
	}

	rawMessage := r.Get(c, "message")
	cleanMessage := strings.TrimSpace(reInvisibleSpaces.ReplaceAllString(rawMessage, " "))
	var message string
	if cleanMessage != "" {
		message = strings.Trim(rawMessage, " \t\r\n\u200b\ufeff\u00a0\u200c\u200d")
	}

	forwardMessagesRaw := r.Get(c, "forward_messages")
	if forwardMessagesRaw == "" {
		forwardMessagesRaw = r.Get(c, "fwd_messages")
	}
	forwardRaw := r.Get(c, "forward")

	attachment := r.Get(c, "attachment")
	stickerID := r.GetInt64(c, "sticker_id", 0)

	var attachParts []string
	if attachment != "" {
		for _, p := range strings.Split(attachment, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				attachParts = append(attachParts, trimmed)
			}
		}
	}

	stickerCount := 0
	otherAttachCount := 0

	for _, part := range attachParts {
		if strings.HasPrefix(part, "sticker") {
			stickerCount++
		} else {
			otherAttachCount++
		}
	}

	if stickerID > 0 {
		stickerCount++
	}

	if stickerCount > 0 {
		if stickerCount > 1 {
			r.Reject(c, 100, "Only one sticker can be sent per message")
			return
		}

		if otherAttachCount > 0 {
			r.Reject(c, 100, "Stickers cannot be sent with other attachments")
			return
		}

		if message != "" {
			r.Reject(c, 100, "Stickers cannot be sent with text")
			return
		}

		if forwardMessagesRaw != "" {
			r.Reject(c, 100, "Stickers cannot be sent with forwarded messages")
			return
		}

		if stickerID > 0 {
			attachment = fmt.Sprintf("sticker%d", stickerID)
		}
	}

	if attachment != "" && !core.IsValidAttachments(attachment) {
		r.Reject(c, 100, "Invalid attachment format")
		return
	}

	replyToStr := r.Get(c, "reply_to")

	if forwardRaw != "" && forwardMessagesRaw == "" {
		var fwdPayload ForwardPayload
		if err := json.Unmarshal([]byte(forwardRaw), &fwdPayload); err == nil {
			if fwdPayload.IsReply && replyToStr == "" {
				if len(fwdPayload.ConversationMessageIDs) > 0 {
					replyToStr = strconv.FormatUint(fwdPayload.ConversationMessageIDs[0], 10)
				} else if len(fwdPayload.MessageIDs) > 0 {
					replyToStr = strconv.FormatUint(fwdPayload.MessageIDs[0], 10)
				}
			} else if !fwdPayload.IsReply {
				if len(fwdPayload.MessageIDs) > 0 {
					var idStrs []string
					for _, id := range fwdPayload.MessageIDs {
						idStrs = append(idStrs, strconv.FormatUint(id, 10))
					}
					forwardMessagesRaw = strings.Join(idStrs, ",")
				} else if len(fwdPayload.ConversationMessageIDs) > 0 {
					srcPeerID := fwdPayload.PeerID
					if srcPeerID == 0 {
						srcPeerID = r.GetPeerID(c)
					}
					srcChatID := chat.GetInternalChatID(srcPeerID, senderID)

					var isMember bool
					db.Instance.Model(&db_models.ConversationMember{}).
						Select("count(*) > 0").
						Where("internal_chat_id = ? AND user_id = ? AND left_at IS NULL", srcChatID, senderID).
						Scan(&isMember)
					if !isMember {
						r.Reject(c, 917, "You don't have permission to forward one or more of these messages")
						return
					}

					var globalIDs []uint64
					q := db.Instance.Model(&db_models.Message{}).
						Where("chat_id = ? AND local_id IN ?", srcChatID, fwdPayload.ConversationMessageIDs)
					q = db_models.BuildVisibilityFilter(q, srcChatID, senderID)
					q.Order("local_id ASC").Pluck("id", &globalIDs)

					if len(globalIDs) != len(fwdPayload.ConversationMessageIDs) {
						r.Reject(c, 917, "You don't have permission to forward one or more of these messages")
						return
					}

					var idStrs []string
					for _, id := range globalIDs {
						idStrs = append(idStrs, strconv.FormatUint(id, 10))
					}
					forwardMessagesRaw = strings.Join(idStrs, ",")
				}
			}
		}
	}

	if message == "" && attachment == "" && replyToStr == "" && forwardMessagesRaw == "" {
		r.Reject(c, 100, "Message text is empty or invalid")
		return
	}

	if len(message) > 9000 {
		r.Reject(c, 914, "Message is too long")
		return
	}

	if forwardMessagesRaw != "" {
		rawIDs := strings.Split(forwardMessagesRaw, ",")
		if len(rawIDs) > 100 {
			r.Reject(c, 100, "Too many forward_messages")
			return
		}

		var validForwardIDs []uint64
		for _, rawID := range rawIDs {
			id, err := strconv.ParseUint(strings.TrimSpace(rawID), 10, 64)
			if err != nil || id == 0 {
				r.Reject(c, 100, "Invalid ID in forward_messages")
				return
			}
			validForwardIDs = append(validForwardIDs, id)
		}

		type msgRef struct {
			ID     uint64 `gorm:"column:id"`
			ChatID string `gorm:"column:chat_id"`
		}
		var refs []msgRef
		db.Instance.Model(&db_models.Message{}).
			Select("id, chat_id").
			Where("id IN ?", validForwardIDs).
			Where("chat_id IN (SELECT internal_chat_id FROM conversation_members WHERE user_id = ? AND left_at IS NULL)", senderID).
			Scan(&refs)

		if len(refs) != len(validForwardIDs) {
			r.Reject(c, 917, "You don't have permission to forward one or more of these messages")
			return
		}

		chatToIDs := make(map[string][]uint64)
		for _, ref := range refs {
			chatToIDs[ref.ChatID] = append(chatToIDs[ref.ChatID], ref.ID)
		}

		for chID, ids := range chatToIDs {
			var visibleCount int64
			subQ := db.Instance.Model(&db_models.Message{}).
				Where("chat_id = ? AND id IN ?", chID, ids)
			subQ = db_models.BuildVisibilityFilter(subQ, chID, senderID)
			subQ.Count(&visibleCount)

			if visibleCount != int64(len(ids)) {
				r.Reject(c, 917, "You don't have permission to forward one or more of these messages")
				return
			}
		}
	}

	attachmentsJSON := r.Get(c, "attachments_json")

	peerIDsRaw := r.Get(c, "peer_ids")
	if peerIDsRaw != "" {
		pIDStrs := strings.Split(peerIDsRaw, ",")
		if len(pIDStrs) > 25 {
			r.Reject(c, 913, "Too many recipients")
			return
		}

		var results []MultiPeerResponse
		for _, pStr := range pIDStrs {
			pID, _ := strconv.ParseInt(strings.TrimSpace(pStr), 10, 64)
			if pID == 0 {
				continue
			}

			randID := rand.Int63()
			mID, cmID, errCode, errMsg := executeSendMessage(c.Request.Context(), r, pID, senderID, message, attachment, replyToStr, forwardMessagesRaw, randID, attachmentsJSON)
			if errCode != 0 {
				results = append(results, MultiPeerResponse{
					PeerID: pID,
					Error: &core.VKError{
						ErrorCode: errCode,
						ErrorMsg:  errMsg,
					},
				})
			} else {
				results = append(results, MultiPeerResponse{
					PeerID:                pID,
					MessageID:             mID,
					ConversationMessageID: cmID,
				})
			}
		}

		c.JSON(http.StatusOK, gin.H{"response": results})
		return
	}

	peerID := r.GetPeerID(c)
	if peerID == 0 {
		r.Reject(c, 100, "One of the parameters specified was missing or invalid: no recipient")
		return
	}

	randomID := r.GetInt64(c, "random_id", 0)
	if randomID == 0 {
		randomID = r.GetInt64(c, "guid", 0)
	}
	if randomID == 0 {
		randomID = rand.Int63()
	}

	finalMessageID, _, errCode, errMsg := executeSendMessage(c.Request.Context(), r, peerID, senderID, message, attachment, replyToStr, forwardMessagesRaw, randomID, attachmentsJSON)
	if errCode != 0 {
		r.Reject(c, errCode, errMsg)
		return
	}

	c.JSON(http.StatusOK, gin.H{"response": finalMessageID})
}

func executeSendMessage(
	ctx context.Context,
	r *core.BaseHandler,
	peerID int64,
	senderID int64,
	message string,
	attachment string,
	replyToStr string,
	forwardMessagesRaw string,
	randomID int64,
	attachmentsJSON string,
) (mID uint64, cmID uint64, errCode int, errMsg string) {
	internalChatID := chat.GetInternalChatID(peerID, senderID)
	isGroupChat := strings.HasPrefix(internalChatID, "c")

	if senderID < 0 && peerID < 0 {
		return 0, 0, 100, "Communities cannot send messages to other communities"
	}

	switch {
	case isGroupChat:
		inChat, err := chat.IsUserInChat(nil, internalChatID, senderID)
		if err != nil || !inChat {
			return 0, 0, 917, "You don't have access to this chat"
		}
	case peerID > 0 && senderID < 0:
		var exists bool
		db.Instance.Model(&db_models.Message{}).
			Select("count(*) > 0").
			Where("chat_id = ? AND from_id = ?", internalChatID, peerID).
			Scan(&exists)
		if !exists {
			return 0, 0, 901, "Can't send messages for users without permission"
		}
	}

	redisKey := fmt.Sprintf("rid:%d:%d:%d", senderID, peerID, randomID)
	if randomID != 0 {
		existingVal, err := r.LPRepo.Client.Get(ctx, redisKey).Result()
		if err == nil && existingVal != "" {
			if existingVal == "processing" {
				return 0, 0, 100, "Message with this random_id is currently being processed"
			}
			if cachedMID, pErr := strconv.ParseUint(existingVal, 10, 64); pErr == nil {
				return cachedMID, cachedMID, 0, ""
			}
		}

		acquired, err := r.LPRepo.Client.SetNX(ctx, redisKey, "processing", 15*time.Second).Result()
		if err != nil || !acquired {
			return 0, 0, 100, "Message with this random_id is already being processed"
		}
	}

	var isSuccess bool
	defer func() {
		if !isSuccess && randomID != 0 {
			_ = r.LPRepo.Client.Del(context.Background(), redisKey).Err()
		}
	}()

	var replyTo uint64
	if replyToStr != "" {
		id, err := strconv.ParseUint(replyToStr, 10, 64)
		if err != nil || id == 0 {
			return 0, 0, 100, "Invalid reply_to parameter"
		}

		var replyExists bool
		q := db.Instance.Model(&db_models.Message{}).
			Select("count(*) > 0").
			Where("chat_id = ? AND (local_id = ? OR id = ?)", internalChatID, id, id)
		q = db_models.BuildVisibilityFilter(q, internalChatID, senderID)
		q.Scan(&replyExists)

		if !replyExists {
			return 0, 0, 100, "Replied message not found in this chat or access denied"
		}
		replyTo = id
	}

	var validMentions []MentionItem
	if message != "" {
		parsedMentions := parseMentions(message)
		if len(parsedMentions) > 0 {
			canUseMassMentions := true
			if isGroupChat {
				var conv db_models.Conversation
				if err := db.Instance.Where("internal_id = ?", internalChatID).First(&conv).Error; err == nil {
					perms := chats.ParseChatPermissions(conv.Settings)
					var member db_models.ConversationMember
					mErr := db.Instance.Where("internal_chat_id = ? AND user_id = ?", internalChatID, senderID).First(&member).Error
					isOwner := (conv.OwnerID != nil && *conv.OwnerID == senderID)
					isAdmin := (mErr == nil && member.IsAdmin)
					isMember := (mErr == nil && (member.LeftAt == nil || member.LeftAt.IsZero()))
					canUseMassMentions = chats.CheckPermission(perms.UseMassMentions, isOwner, isAdmin, isMember)
				}
			}

			for _, m := range parsedMentions {
				if m.MentionType == "all" || m.MentionType == "online" {
					if canUseMassMentions {
						validMentions = append(validMentions, m)
					}
				} else {
					validMentions = append(validMentions, m)
				}
			}
		}
	}

	var finalLocalID uint64
	var finalMessageID uint64

	err := db.Instance.Transaction(func(tx *gorm.DB) error {
		localID, err := chat.NextLocalID(tx, internalChatID, senderID)
		if err != nil {
			return err
		}
		finalLocalID = localID

		if !isGroupChat {
			tx.FirstOrCreate(&db_models.ConversationMember{
				InternalChatID: internalChatID,
				UserID:         senderID,
				JoinedAt:       time.Now(),
				IsAdmin:        true,
			})
			chat.EnsureMemberPeriod(tx, internalChatID, senderID, 1)

			if peerID != senderID {
				tx.FirstOrCreate(&db_models.ConversationMember{
					InternalChatID: internalChatID,
					UserID:         peerID,
					JoinedAt:       time.Now(),
					IsAdmin:        true,
				})
				chat.EnsureMemberPeriod(tx, internalChatID, peerID, 1)
			}
		}

		finalAttach := core.BuildFinalAttachments(attachment, message)

		newMessage := db_models.Message{
			ChatID:          internalChatID,
			LocalID:         localID,
			FromID:          senderID,
			Text:            db_models.EncryptedJSON(message),
			Attachments:     db_models.EncryptedJSON(finalAttach),
			ReplyTo:         &replyTo,
			RandomID:        randomID,
			ForwardMessages: forwardMessagesRaw,
			CreatedAt:       time.Now(),
		}

		if err := tx.Create(&newMessage).Error; err != nil {
			return err
		}
		finalMessageID = newMessage.ID

		for _, m := range validMentions {
			mentionRecord := db_models.MessageMention{
				MessageID:   newMessage.ID,
				ChatID:      internalChatID,
				PeerID:      peerID,
				FromID:      senderID,
				UserID:      m.UserID,
				MentionType: m.MentionType,
				CreatedAt:   time.Now(),
			}
			if err := tx.Create(&mentionRecord).Error; err != nil {
				return err
			}
		}

		if err := tx.Model(&db_models.Conversation{}).
			Where("internal_id = ?", internalChatID).
			Update("last_message_id", localID).Error; err != nil {
			return err
		}

		if err := tx.Model(&db_models.ConversationMember{}).
			Where("internal_chat_id = ? AND user_id = ?", internalChatID, senderID).
			Updates(map[string]interface{}{
				"last_message_id": localID,
				"last_read_id":    localID,
			}).Error; err != nil {
			return err
		}

		if message != "" {
			indexes := r.SearchRepo.GenerateBlindIndexes(newMessage.ID, internalChatID, message)
			if len(indexes) > 0 {
				if err := tx.Create(&indexes).Error; err != nil {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		switch err {
		case chat.ErrGroupNoPermission:
			return 0, 0, 901, "Can't send messages for users without permission"
		case chat.ErrChatNotFound:
			return 0, 0, 917, "Chat not found or access denied"
		default:
			log.Printf("[Messages.Send Error] sender=%d, peer=%d: %v", senderID, peerID, err)
			return 0, 0, 10, "Internal server error"
		}
	}

	isSuccess = true
	if randomID != 0 {
		r.LPRepo.Client.Set(ctx, redisKey, finalMessageID, 24*time.Hour)
	}

	lpAttach := lp_models.NewLPAttachments(attachment)
	lpAttach.From = senderID
	lpAttach.CMID = finalLocalID
	if replyTo != 0 {
		lpAttach.ReplyTo = replyTo
	}
	if forwardMessagesRaw != "" {
		lpAttach.Fwd = forwardMessagesRaw
	}
	if attachmentsJSON != "" {
		var rawItems []interface{}
		if err := json.Unmarshal([]byte(attachmentsJSON), &rawItems); err == nil {
			lpAttach.ItemsPayload = rawItems
		}
	}

	lpEvent := lp_models.NewMessageEvent{
		MessageID:   finalMessageID,
		MinorID:     int64(finalLocalID),
		Flags:       lp_models.MessageFlags{Value: 0},
		PeerID:      peerID,
		Timestamp:   int(time.Now().Unix()),
		Text:        message,
		Attachments: &lpAttach,
		RandomID:    int(randomID),
	}

	var recipients []int64
	if isGroupChat {
		db.Instance.Model(&db_models.ConversationMember{}).
			Where("internal_chat_id = ? AND (left_at IS NULL OR left_at = 0)", internalChatID).
			Pluck("user_id", &recipients)
	} else {
		recipients = append(recipients, senderID)
		if peerID != senderID {
			recipients = append(recipients, peerID)
		}
	}

	go func(rcps []int64, event lp_models.NewMessageEvent, sID int64, mentions []MentionItem) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Messages.Send Panic] %v", rec)
			}
		}()

		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		baseFlags := event.Flags.Value
		if isGroupChat {
			baseFlags |= lp_models.FlagChat
		}
		if attachment != "" || forwardMessagesRaw != "" || replyTo != 0 {
			baseFlags |= lp_models.FlagMedia
		}

		var stopTypingEvent lp_models.VKEvent
		var stopEventType string
		if isGroupChat {
			stopEventType = "is_chat_typing"
			var chatID int64 = peerID
			if peerID > 2000000000 {
				chatID = peerID - 2000000000
			}
			stopTypingEvent = &lp_models.IsChatTypingEvent{
				UserID: sID,
				ChatID: chatID,
				Flags:  0,
			}
		} else {
			stopEventType = "is_dm_typing"
			stopTypingEvent = &lp_models.IsDMTypingEvent{
				UserID: sID,
				Flags:  0,
			}
		}

		var mutes []db_models.ConversationMute
		db.Instance.Where("user_id IN (?) AND peer_id = ?", rcps, peerID).Find(&mutes)
		muteMap := make(map[int64]db_models.ConversationMute)
		for _, m := range mutes {
			muteMap[m.UserID] = m
		}

		nowUnix := time.Now().Unix()
		hasAllMention := false
		hasOnlineMention := false
		mentionedUserIDs := make(map[int64]bool)

		for _, m := range mentions {
			if m.MentionType == "all" {
				hasAllMention = true
			} else if m.MentionType == "online" {
				hasOnlineMention = true
			} else if m.MentionType == "user" {
				mentionedUserIDs[m.UserID] = true
			}
		}

		for _, uid := range rcps {
			userEvent := event
			userEvent.Flags = lp_models.MessageFlags{Value: baseFlags}

			mute, hasMute := muteMap[uid]
			isMuted := false
			if hasMute {
				if mute.DisabledUntil == -1 || (mute.DisabledUntil > 0 && mute.DisabledUntil > nowUnix) || !mute.Sound {
					isMuted = true
				}
			}

			isMentioned := false
			if uid != sID {
				if mentionedUserIDs[uid] && (!hasMute || !mute.DisabledMentions) {
					isMentioned = true
				} else if hasAllMention && (!hasMute || (!mute.DisabledMassMentions && !mute.DisabledMentions)) {
					isMentioned = true
				} else if hasOnlineMention && (!hasMute || (!mute.DisabledMassMentions && !mute.DisabledMentions)) {
					score, err := r.LPRepo.Client.ZScore(bgCtx, "im:online_users", strconv.FormatInt(uid, 10)).Result()
					if err == nil && (nowUnix-int64(score) <= 300) {
						isMentioned = true
					}
				}
			}

			var userLPAttach lp_models.LPAttachments
			if event.Attachments != nil {
				userLPAttach = *event.Attachments
			}
			userLPAttach.Muted = isMuted
			userLPAttach.Mention = isMentioned
			userEvent.Attachments = &userLPAttach

			if uid == sID {
				userEvent.Flags.Add(lp_models.FlagOutbox)
				userEvent.PeerID = peerID
			} else {
				userEvent.Flags.Add(lp_models.FlagUnread)
				if isGroupChat {
					userEvent.PeerID = peerID
				} else {
					userEvent.PeerID = sID
				}

				_ = r.LPRepo.PushEphemeralEvent(bgCtx, uid, stopEventType, stopTypingEvent)
			}

			_, _, pushErr := r.LPRepo.PushEvent(bgCtx, uid, "new_msg", userEvent)
			if pushErr == nil {
				r.Broadcaster.Notify(uid)
				r.LPRepo.Client.Publish(bgCtx, "lp_updates", strconv.FormatInt(uid, 10))
			}

			if uid != senderID {
				r.BroadcastCounterUpdate(bgCtx, uid)
			}
		}
		r.BroadcastCounterUpdate(bgCtx, sID)
	}(recipients, lpEvent, senderID, validMentions)

	return finalMessageID, finalLocalID, 0, ""
}
