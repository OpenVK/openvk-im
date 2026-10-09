package chats

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
)

type ChatPermissions struct {
	Invite           string `json:"invite"`
	ChangeInfo       string `json:"change_info"`
	ChangePin        string `json:"change_pin"`
	UseMassMentions  string `json:"use_mass_mentions"`
	SeeInviteLink    string `json:"see_invite_link"`
	ChangeInviteLink string `json:"change_invite_link"`
	Call             string `json:"call"`
	ChangeAdmins     string `json:"change_admins"`
}

func DefaultChatPermissions() ChatPermissions {
	return ChatPermissions{
		Invite:           "all",
		ChangeInfo:       "admin",
		ChangePin:        "admin",
		UseMassMentions:  "all",
		SeeInviteLink:    "admin",
		ChangeInviteLink: "owner",
		Call:             "all",
		ChangeAdmins:     "owner",
	}
}

func ParseChatPermissions(settingsBytes []byte) ChatPermissions {
	perms := DefaultChatPermissions()
	if len(settingsBytes) == 0 {
		return perms
	}

	var root map[string]interface{}
	if err := json.Unmarshal(settingsBytes, &root); err != nil {
		return perms
	}

	rawPerms, ok := root["permissions"]
	if !ok || rawPerms == nil {
		return perms
	}

	permsJSON, err := json.Marshal(rawPerms)
	if err != nil {
		return perms
	}

	_ = json.Unmarshal(permsJSON, &perms)

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

	return perms
}

func CheckPermission(level string, isOwner bool, isAdmin bool, isMember bool) bool {
	if !isMember {
		return false
	}
	if isOwner {
		return true
	}
	switch level {
	case "all":
		return true
	case "admin":
		return isAdmin
	case "owner":
		return isOwner
	default:
		return isAdmin
	}
}

func (p ChatPermissions) ToGinH() gin.H {
	return gin.H{
		"invite":             p.Invite,
		"change_info":        p.ChangeInfo,
		"change_pin":         p.ChangePin,
		"use_mass_mentions":  p.UseMassMentions,
		"see_invite_link":    p.SeeInviteLink,
		"change_invite_link": p.ChangeInviteLink,
		"call":               p.Call,
		"change_admins":      p.ChangeAdmins,
	}
}

func ComputeChatACL(p ChatPermissions, isOwner bool, isAdmin bool, isMember bool) gin.H {
	return gin.H{
		"can_invite":             CheckPermission(p.Invite, isOwner, isAdmin, isMember),
		"can_change_info":        CheckPermission(p.ChangeInfo, isOwner, isAdmin, isMember),
		"can_change_pin":         CheckPermission(p.ChangePin, isOwner, isAdmin, isMember),
		"can_promote_users":      CheckPermission(p.ChangeAdmins, isOwner, isAdmin, isMember),
		"can_see_invite_link":    CheckPermission(p.SeeInviteLink, isOwner, isAdmin, isMember),
		"can_change_invite_link": CheckPermission(p.ChangeInviteLink, isOwner, isAdmin, isMember),
		"can_moderate":           isMember && (isOwner || isAdmin),
		"can_copy_chat":          isMember && (isOwner || isAdmin),
		"can_call":               CheckPermission(p.Call, isOwner, isAdmin, isMember),
		"can_use_mass_mentions":  CheckPermission(p.UseMassMentions, isOwner, isAdmin, isMember),
		"can_change_owner":                    isOwner,
		"can_change_service_type":             isOwner,
		"can_change_style":                    isMember && (isOwner || isAdmin),
		"can_change_stickers_popup_autoplay":  isMember,
		"can_disable_forward_messages":        isMember && (isOwner || isAdmin),
		"can_disable_service_messages":        isMember && (isOwner || isAdmin),
		"can_finish_call":                     isMember && (isOwner || isAdmin),
		"can_forward_messages":                isMember,
		"can_hide":                            isMember,
		"can_receive_money":                   false,
		"can_send_money":                      false,
		"can_send_reactions":                  isMember,
		"can_write":                           isMember,
	}
}
