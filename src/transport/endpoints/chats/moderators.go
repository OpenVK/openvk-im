package chats

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	lp_models "ovk-im/src/models/longpoll"
	"ovk-im/src/repo/chat"
	"ovk-im/src/transport/endpoints/core"

	"github.com/gin-gonic/gin"
)

// SetChatModerator appoints an existing chat member as a moderator (is_admin = true).
// Only the conversation owner can appoint moderators.
func SetChatModerator(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		return
	}
	currentUserID := val.(int64)

	var peerID int64
	if pID := c.DefaultQuery("peer_id", c.PostForm("peer_id")); pID != "" {
		peerID, _ = strconv.ParseInt(pID, 10, 64)
	} else if cID := c.DefaultQuery("chat_id", c.PostForm("chat_id")); cID != "" {
		id, _ := strconv.ParseInt(cID, 10, 64)
		peerID = 2000000000 + id
	}

	userIDStr := c.DefaultQuery("user_id", c.PostForm("user_id"))
	targetUserID, _ := strconv.ParseInt(userIDStr, 10, 64)

	if peerID <= 2000000000 || targetUserID == 0 {
		r.Reject(c, 100, "One of the parameters is missing or invalid: peer_id and user_id are required")
		return
	}

	internalChatID := chat.GetInternalChatID(peerID, currentUserID)
	if !strings.HasPrefix(internalChatID, "c") {
		r.Reject(c, 15, "Access denied: target is not a group chat")
		return
	}

	conv, err := chat.GetConversation(nil, internalChatID)
	if err != nil || conv == nil {
		r.Reject(c, 917, "Chat not found")
		return
	}

	if conv.OwnerID == nil || *conv.OwnerID != currentUserID {
		r.Reject(c, 15, "Access denied: only the conversation owner can appoint moderators")
		return
	}

	if targetUserID == currentUserID || (conv.OwnerID != nil && targetUserID == *conv.OwnerID) {
		r.Reject(c, 15, "Target user is already the conversation owner")
		return
	}

	targetMember, err := chat.GetMember(nil, internalChatID, targetUserID)
	if err != nil || targetMember == nil || targetMember.LeftAt != nil {
		r.Reject(c, 15, "Target user is not an active member of this chat")
		return
	}

	if targetMember.IsAdmin {
		// Already moderator
		c.JSON(http.StatusOK, gin.H{"response": 1})
		return
	}

	err = db.Instance.Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ?", internalChatID, targetUserID).
		Update("is_admin", true).Error
	if err != nil {
		r.Reject(c, 10, "Internal server error: failed to update member role: "+err.Error())
		return
	}

	messageText := "appointed user " + strconv.FormatInt(targetUserID, 10) + " as moderator"
	msg, err := chat.CreateServiceMessage(
		currentUserID,
		internalChatID,
		messageText,
		"chat_moderator_add",
		targetUserID,
		"",
	)

	if err == nil {
		participants, err := chat.GetActiveMemberIDs(nil, internalChatID)
		if err == nil {
			lpAttach := lp_models.LPAttachments{
				Source: "chat_moderator_add",
				Mid:    strconv.FormatInt(targetUserID, 10),
				From:   currentUserID,
				CMID:   msg.LocalID,
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
				}
			}(participants, baseEvent)
		}
	}

	r.BroadcastChatSomethingChanged(c, peerID, currentUserID)

	c.JSON(http.StatusOK, gin.H{"response": 1})
}

// RemoveChatModerator dismisses a moderator back to a regular member (is_admin = false).
// Only the conversation owner can dismiss moderators.
func RemoveChatModerator(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		return
	}
	currentUserID := val.(int64)

	var peerID int64
	if pID := c.DefaultQuery("peer_id", c.PostForm("peer_id")); pID != "" {
		peerID, _ = strconv.ParseInt(pID, 10, 64)
	} else if cID := c.DefaultQuery("chat_id", c.PostForm("chat_id")); cID != "" {
		id, _ := strconv.ParseInt(cID, 10, 64)
		peerID = 2000000000 + id
	}

	userIDStr := c.DefaultQuery("user_id", c.PostForm("user_id"))
	targetUserID, _ := strconv.ParseInt(userIDStr, 10, 64)

	if peerID <= 2000000000 || targetUserID == 0 {
		r.Reject(c, 100, "One of the parameters is missing or invalid: peer_id and user_id are required")
		return
	}

	internalChatID := chat.GetInternalChatID(peerID, currentUserID)
	if !strings.HasPrefix(internalChatID, "c") {
		r.Reject(c, 15, "Access denied: target is not a group chat")
		return
	}

	conv, err := chat.GetConversation(nil, internalChatID)
	if err != nil || conv == nil {
		r.Reject(c, 917, "Chat not found")
		return
	}

	if conv.OwnerID == nil || *conv.OwnerID != currentUserID {
		r.Reject(c, 15, "Access denied: only the conversation owner can dismiss moderators")
		return
	}

	if targetUserID == currentUserID || (conv.OwnerID != nil && targetUserID == *conv.OwnerID) {
		r.Reject(c, 15, "Cannot dismiss conversation owner")
		return
	}

	targetMember, err := chat.GetMember(nil, internalChatID, targetUserID)
	if err != nil || targetMember == nil || targetMember.LeftAt != nil {
		r.Reject(c, 15, "Target user is not an active member of this chat")
		return
	}

	if !targetMember.IsAdmin {
		// Already not moderator
		c.JSON(http.StatusOK, gin.H{"response": 1})
		return
	}

	err = db.Instance.Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ?", internalChatID, targetUserID).
		Update("is_admin", false).Error
	if err != nil {
		r.Reject(c, 10, "Internal server error: failed to update member role: "+err.Error())
		return
	}

	messageText := "dismissed user " + strconv.FormatInt(targetUserID, 10) + " from moderators"
	msg, err := chat.CreateServiceMessage(
		currentUserID,
		internalChatID,
		messageText,
		"chat_moderator_remove",
		targetUserID,
		"",
	)

	if err == nil {
		participants, err := chat.GetActiveMemberIDs(nil, internalChatID)
		if err == nil {
			lpAttach := lp_models.LPAttachments{
				Source: "chat_moderator_remove",
				Mid:    strconv.FormatInt(targetUserID, 10),
				From:   currentUserID,
				CMID:   msg.LocalID,
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
				}
			}(participants, baseEvent)
		}
	}

	r.BroadcastChatSomethingChanged(c, peerID, currentUserID)

	c.JSON(http.StatusOK, gin.H{"response": 1})
}

// GetChatModerators returns the list of moderators and the owner of the chat.
func GetChatModerators(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		return
	}
	currentUserID := val.(int64)

	var peerID int64
	if pID := c.DefaultQuery("peer_id", c.PostForm("peer_id")); pID != "" {
		peerID, _ = strconv.ParseInt(pID, 10, 64)
	} else if cID := c.DefaultQuery("chat_id", c.PostForm("chat_id")); cID != "" {
		id, _ := strconv.ParseInt(cID, 10, 64)
		peerID = 2000000000 + id
	}

	if peerID <= 2000000000 {
		r.Reject(c, 100, "One of the parameters is missing or invalid: peer_id must be a group chat (> 2000000000)")
		return
	}

	internalChatID := chat.GetInternalChatID(peerID, currentUserID)
	if !strings.HasPrefix(internalChatID, "c") {
		r.Reject(c, 15, "Access denied: target is not a group chat")
		return
	}

	if currentUserID != 0 {
		inChat, _ := chat.IsUserInChat(nil, internalChatID, currentUserID)
		if !inChat {
			r.Reject(c, 917, "You don't have access to this chat")
			return
		}
	}

	conv, err := chat.GetConversation(nil, internalChatID)
	if err != nil || conv == nil {
		r.Reject(c, 917, "Chat not found")
		return
	}

	var ownerID int64
	if conv.OwnerID != nil {
		ownerID = *conv.OwnerID
	}

	var members []db_models.ConversationMember
	db.Instance.Where("internal_chat_id = ? AND is_admin = ? AND left_at IS NULL", internalChatID, true).
		Order("joined_at ASC").
		Find(&members)

	moderatorIDs := make([]int64, 0)
	for _, m := range members {
		if m.UserID != ownerID {
			moderatorIDs = append(moderatorIDs, m.UserID)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"response": gin.H{
			"owner_id":      ownerID,
			"count":         len(moderatorIDs),
			"items":         moderatorIDs,
			"moderator_ids": moderatorIDs,
		},
	})
}

func SetMemberRole(c *gin.Context, r *core.BaseHandler) {
	role := strings.ToLower(strings.TrimSpace(c.DefaultQuery("role", c.PostForm("role"))))
	if role == "" {
		r.Reject(c, 100, "One of the parameters is missing or invalid: role is required")
		return
	}

	if c.Query("user_id") == "" && c.PostForm("user_id") == "" {
		if mID := c.DefaultQuery("member_id", c.PostForm("member_id")); mID != "" {
			c.Request.URL.RawQuery += "&user_id=" + mID
		}
	}

	switch role {
	case "admin":
		SetChatModerator(c, r)
		return
	case "member":
		RemoveChatModerator(c, r)
		return
	}

	r.Reject(c, 100, "Invalid role: must be 'admin' or 'member'")
}

func SetChatPermissions(c *gin.Context, r *core.BaseHandler) {
	val, exists := c.Get("userID")
	if !exists || val == nil {
		return
	}
	currentUserID := val.(int64)

	var peerID int64
	if pID := c.DefaultQuery("peer_id", c.PostForm("peer_id")); pID != "" {
		peerID, _ = strconv.ParseInt(pID, 10, 64)
	} else if cID := c.DefaultQuery("chat_id", c.PostForm("chat_id")); cID != "" {
		id, _ := strconv.ParseInt(cID, 10, 64)
		peerID = 2000000000 + id
	}

	if peerID <= 2000000000 {
		r.Reject(c, 100, "One of the parameters is missing or invalid: peer_id must be a group chat (> 2000000000)")
		return
	}

	internalChatID := chat.GetInternalChatID(peerID, currentUserID)
	if !strings.HasPrefix(internalChatID, "c") {
		r.Reject(c, 15, "Access denied: target is not a group chat")
		return
	}

	conv, err := chat.GetConversation(nil, internalChatID)
	if err != nil || conv == nil {
		r.Reject(c, 917, "Chat not found")
		return
	}

	isOwner := conv.OwnerID != nil && *conv.OwnerID == currentUserID
	currentPerms := ParseChatPermissions(conv.Settings)

	callerMember, _ := chat.GetMember(nil, internalChatID, currentUserID)
	isCallerAdmin := isOwner || (callerMember != nil && callerMember.IsAdmin && callerMember.LeftAt == nil)

	canChangePerms := isOwner || (isCallerAdmin && currentPerms.ChangeAdmins == "admin")
	if !canChangePerms {
		r.Reject(c, 15, "Access denied: only the chat owner or administrators can change chat permissions")
		return
	}

	perms := currentPerms

	permsParam := c.DefaultQuery("permissions", c.PostForm("permissions"))
	if permsParam != "" {
		var pObj ChatPermissions
		if err := json.Unmarshal([]byte(permsParam), &pObj); err == nil {
			if pObj.Invite != "" {
				perms.Invite = pObj.Invite
			}
			if pObj.ChangeInfo != "" {
				perms.ChangeInfo = pObj.ChangeInfo
			}
			if pObj.ChangePin != "" {
				perms.ChangePin = pObj.ChangePin
			}
			if pObj.UseMassMentions != "" {
				perms.UseMassMentions = pObj.UseMassMentions
			}
			if pObj.SeeInviteLink != "" {
				perms.SeeInviteLink = pObj.SeeInviteLink
			}
			if pObj.ChangeInviteLink != "" {
				perms.ChangeInviteLink = pObj.ChangeInviteLink
			}
			if pObj.Call != "" {
				perms.Call = pObj.Call
			}
			if pObj.ChangeAdmins != "" {
				perms.ChangeAdmins = pObj.ChangeAdmins
			}
		}
	}

	if v := c.DefaultQuery("invite", c.PostForm("invite")); v != "" {
		perms.Invite = v
	}
	if v := c.DefaultQuery("change_info", c.PostForm("change_info")); v != "" {
		perms.ChangeInfo = v
	}
	if v := c.DefaultQuery("change_pin", c.PostForm("change_pin")); v != "" {
		perms.ChangePin = v
	}
	if v := c.DefaultQuery("use_mass_mentions", c.PostForm("use_mass_mentions")); v != "" {
		perms.UseMassMentions = v
	}
	if v := c.DefaultQuery("see_invite_link", c.PostForm("see_invite_link")); v != "" {
		perms.SeeInviteLink = v
	}
	if v := c.DefaultQuery("change_invite_link", c.PostForm("change_invite_link")); v != "" {
		perms.ChangeInviteLink = v
	} else if v := c.DefaultQuery("regenerate_link", c.PostForm("regenerate_link")); v != "" {
		perms.ChangeInviteLink = v
	}
	if v := c.DefaultQuery("call", c.PostForm("call")); v != "" {
		perms.Call = v
	}
	if v := c.DefaultQuery("change_admins", c.PostForm("change_admins")); v != "" {
		perms.ChangeAdmins = v
	}

	if perms.Invite != "all" && perms.Invite != "admin" && perms.Invite != "owner" {
		perms.Invite = "all"
	}
	if perms.ChangeInfo != "all" && perms.ChangeInfo != "admin" && perms.ChangeInfo != "owner" {
		perms.ChangeInfo = "admin"
	}
	if perms.ChangePin != "all" && perms.ChangePin != "admin" && perms.ChangePin != "owner" {
		perms.ChangePin = "admin"
	}
	if perms.UseMassMentions != "all" && perms.UseMassMentions != "admin" && perms.UseMassMentions != "owner" {
		perms.UseMassMentions = "all"
	}
	if perms.SeeInviteLink != "all" && perms.SeeInviteLink != "admin" && perms.SeeInviteLink != "owner" {
		perms.SeeInviteLink = "admin"
	}
	if perms.ChangeInviteLink != "all" && perms.ChangeInviteLink != "admin" && perms.ChangeInviteLink != "owner" {
		perms.ChangeInviteLink = "owner"
	}
	if perms.Call != "all" && perms.Call != "admin" && perms.Call != "owner" {
		perms.Call = "all"
	}
	if perms.ChangeAdmins != "owner" && perms.ChangeAdmins != "admin" {
		perms.ChangeAdmins = "owner"
	}

	var root map[string]interface{}
	if len(conv.Settings) > 0 {
		_ = json.Unmarshal(conv.Settings, &root)
	}
	if root == nil {
		root = make(map[string]interface{})
	}
	root["permissions"] = perms

	newSettingsBytes, err := json.Marshal(root)
	if err != nil {
		r.Reject(c, 10, "Failed to serialize settings: "+err.Error())
		return
	}

	err = db.Instance.Model(&db_models.Conversation{}).
		Where("internal_id = ?", internalChatID).
		Update("settings", db_models.EncryptedJSON(newSettingsBytes)).Error
	if err != nil {
		r.Reject(c, 10, "Internal server error: failed to update chat permissions: "+err.Error())
		return
	}

	r.BroadcastChatSomethingChanged(c, peerID, currentUserID)

	c.JSON(http.StatusOK, gin.H{
		"response": 1,
	})
}
