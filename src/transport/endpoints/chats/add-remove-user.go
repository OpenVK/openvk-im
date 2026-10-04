package chats

import (
	"context"
	"net/http"
	db_models "ovk-im/src/models/db"
	lp_models "ovk-im/src/models/longpoll"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"
	"strconv"

	"github.com/gin-gonic/gin"
)

func AddChatUser(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	peerID, _ := strconv.ParseInt(c.Query("peer_id"), 10, 64)
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	if peerID == 0 || userID == 0 {
		r.Reject(c, 100, "One of the parameters is missing: peer_id or user_id")
		return
	}

	if peerID < 2000000000 {
		r.Reject(c, 15, "Access denied: cannot add user to direct message")
		return
	}
	chatID := chat.GetInternalChatID(peerID, currentUserID)

	conv, errC := chat.GetConversation(nil, chatID)
	if errC != nil || conv == nil {
		r.Reject(c, 917, "Chat not found")
		return
	}

	var messageText string
	action := "chat_invite_user"

	if currentUserID == userID {
		// User is returning to chat (self-rejoin)
		var targetMember db_models.ConversationMember
		if errM := r.DB.Where("internal_chat_id = ? AND user_id = ?", chatID, userID).First(&targetMember).Error; errM != nil {
			// User was NEVER a member of this chat -> cannot self-join without invite link!
			r.Reject(c, 15, "Access denied: you cannot join this chat (you were never a member)")
			return
		}

		if targetMember.LeftAt == nil {
			r.Reject(c, 15, "User is already in the chat")
			return
		}

		var lastKickMsg db_models.Message
		if errK := r.DB.Where("chat_id = ? AND action = ? AND action_mid = ?", chatID, "chat_kick_user", userID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
			if lastKickMsg.FromID != userID {
				r.Reject(c, 15, "Access denied: you have been kicked from this chat")
				return
			}
		}

		messageText = "returned to the chat"
	} else {
		// Inviter is inviting another user
		inviterMember, errI := chat.GetMember(nil, chatID, currentUserID)
		if errI != nil || inviterMember == nil || inviterMember.LeftAt != nil {
			r.Reject(c, 15, "Access denied: you are not in this chat")
			return
		}

		isOwner := conv.OwnerID != nil && currentUserID == *conv.OwnerID
		isAdmin := inviterMember.IsAdmin || isOwner
		perms := ParseChatPermissions([]byte(conv.Settings))
		if !CheckPermission(perms.Invite, isOwner, isAdmin, true) {
			r.Reject(c, 15, "Access denied: you don't have permission to invite users to this chat")
			return
		}

		var targetMember db_models.ConversationMember
		if errM := r.DB.Where("internal_chat_id = ? AND user_id = ?", chatID, userID).First(&targetMember).Error; errM == nil {
			if targetMember.LeftAt == nil {
				r.Reject(c, 15, "User is already in the chat")
				return
			}

			var lastKickMsg db_models.Message
			if errK := r.DB.Where("chat_id = ? AND action = ? AND action_mid = ?", chatID, "chat_kick_user", userID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
				if lastKickMsg.FromID != userID {
					if !isAdmin {
						r.Reject(c, 15, "Access denied: only administrators can invite kicked users")
						return
					}
				}
			}
		}

		messageText = "invited user " + strconv.FormatInt(userID, 10)
	}

	var visibleCount int64
	if vcStr := c.Query("visible_messages_count"); vcStr != "" {
		if vc, err := strconv.ParseInt(vcStr, 10, 64); err == nil && vc > 0 {
			visibleCount = vc
		}
	}
	canSeeHistory := visibleCount > 0

	msg, err := chat.AddUserToConversation(
		chatID,
		userID,
		currentUserID,
		messageText,
		action,
		userID,
		"",
		canSeeHistory,
	)

	if err != nil {
		r.Reject(c, 15, "Failed to add user: "+err.Error())
		return
	}

	participants, err := chat.GetActiveMemberIDs(nil, chatID)
	if err == nil {
		hasEmoji := false
		lpAttach := lp_models.LPAttachments{
			Source: "chat_invite_user",
			Mid:    strconv.FormatInt(userID, 10),
			From:   currentUserID,
			CMID:   msg.LocalID,
			Emoji:  hasEmoji,
		}

		baseEvent := lp_models.NewMessageEvent{
			MessageID:   uint64(msg.ID),
			PeerID:      peerID,
			Timestamp:   int(msg.CreatedAt.Unix()),
			Text:        messageText,
			Attachments: &lpAttach,
		}

		go func(parts []int64, event lp_models.NewMessageEvent) {
			ctx := context.Background()
			for _, uID := range parts {
				userEvent := event
				userEvent.Flags = lp_models.MessageFlags{Value: event.Flags.Value}

				if uID == currentUserID {
					userEvent.Flags.Add(lp_models.FlagOutbox)
				} else {
					userEvent.Flags.Add(lp_models.FlagUnread)
				}

				_, _, err := r.LPRepo.PushEvent(ctx, uID, "new_msg", userEvent)
				if err == nil {
					r.Broadcaster.Notify(uID)
				}
				if uID != currentUserID {
					r.BroadcastCounterUpdate(ctx, uID)
				}
			}
		}(participants, baseEvent)
	}

	c.JSON(http.StatusOK, gin.H{
		"response": 1,
	})
}

func RemoveChatUser(c *gin.Context, r *core.BaseHandler) {
	val, _ := c.Get("userID")
	currentUserID := val.(int64)

	peerID, _ := strconv.ParseInt(c.Query("peer_id"), 10, 64)
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	if peerID == 0 {
		r.Reject(c, 100, "One of the parameters is missing: peer_id")
		return
	}

	if userID == 0 {
		userID = currentUserID
	}

	if peerID < 2000000000 {
		r.Reject(c, 15, "Access denied: cannot remove user from direct message")
		return
	}
	chatID := chat.GetInternalChatID(peerID, currentUserID)

	if userID != currentUserID {
		member, err := chat.GetMember(nil, chatID, currentUserID)
		if err != nil || member == nil {
			r.Reject(c, 15, "Access denied: you are not in this chat")
			return
		}

		if !member.IsAdmin {
			r.Reject(c, 15, "Access denied: you must be an admin or moderator to kick other users")
			return
		}

		conv, err := chat.GetConversation(nil, chatID)
		if err != nil || conv == nil {
			r.Reject(c, 917, "Chat not found")
			return
		}

		if conv.OwnerID != nil && userID == *conv.OwnerID {
			r.Reject(c, 15, "Access denied: cannot kick conversation owner")
			return
		}

		isCallerOwner := conv.OwnerID != nil && currentUserID == *conv.OwnerID
		if !isCallerOwner {
			targetMember, _ := chat.GetMember(nil, chatID, userID)
			if targetMember != nil && targetMember.IsAdmin {
				r.Reject(c, 15, "Access denied: moderators cannot kick other moderators")
				return
			}
		}
	}

	isActive, err := chat.IsUserInChat(nil, chatID, userID)
	if err != nil || !isActive {
		r.Reject(c, 15, "User is not in the chat")
		return
	}

	err = chat.RemoveUserFromConversation(nil, chatID, userID)
	if err != nil {
		r.Reject(c, 10, "Internal server error: failed to remove user: "+err.Error())
		return
	}

	messageText := "kicked user " + strconv.FormatInt(userID, 10)
	if userID == currentUserID {
		messageText = "left the chat"
	}

	msg, err := chat.CreateServiceMessage(
		currentUserID,
		chatID,
		messageText,
		"chat_kick_user",
		userID,
		"",
	)

	if err == nil {
		chat.CloseActiveMemberPeriod(nil, chatID, userID, msg.LocalID)

		participants, err := chat.GetActiveMemberIDs(nil, chatID)
		if err == nil {
			notifyList := append(participants, userID)

			hasEmoji := false
			lpAttach := lp_models.LPAttachments{
				Source: "chat_kick_user",
				Mid:    strconv.FormatInt(userID, 10),
				From:   currentUserID,
				CMID:   msg.LocalID,
				Emoji:  hasEmoji,
			}

			baseEvent := lp_models.NewMessageEvent{
				MessageID:   uint64(msg.ID),
				PeerID:      peerID,
				Timestamp:   int(msg.CreatedAt.Unix()),
				Text:        messageText,
				Attachments: &lpAttach,
			}

			go func(parts []int64, event lp_models.NewMessageEvent) {
				ctx := context.Background()
				for _, uID := range parts {
					userEvent := event
					userEvent.Flags = lp_models.MessageFlags{Value: event.Flags.Value}

					if uID == currentUserID {
						userEvent.Flags.Add(lp_models.FlagOutbox)
					} else {
						userEvent.Flags.Add(lp_models.FlagUnread)
					}

					_, _, err := r.LPRepo.PushEvent(ctx, uID, "new_msg", userEvent)
					if err == nil {
						r.Broadcaster.Notify(uID)
					}
					if uID != currentUserID {
						r.BroadcastCounterUpdate(ctx, uID)
					}
				}
				r.BroadcastCounterUpdate(ctx, userID)
			}(notifyList, baseEvent)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"response": 1,
	})
}
