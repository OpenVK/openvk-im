package core

import (
	"context"
	"encoding/json"
	"net/http"
	urlPkg "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	lp_models "ovk-im/src/models/longpoll"
	"ovk-im/src/repo/chat"
	redis "ovk-im/src/repo/redis"
	"ovk-im/src/repo/search"
	"ovk-im/src/transport/broadcaster"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type BaseHandler struct {
	DB          *gorm.DB
	LPRepo      *redis.Repo
	Broadcaster *broadcaster.Broadcaster
	SearchRepo  *search.Repository
}

type VKError struct {
	ErrorCode     int            `json:"error_code"`
	ErrorMsg      string         `json:"error_msg"`
	RequestParams []RequestParam `json:"request_params,omitempty"`
}

type RequestParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (b *BaseHandler) Get(c *gin.Context, key string) string {
	if val := c.Query(key); val != "" {
		return strings.TrimSpace(val)
	}
	return strings.TrimSpace(c.PostForm(key))
}

func (b *BaseHandler) GetDefault(c *gin.Context, key, fallback string) string {
	if val := b.Get(c, key); val != "" {
		return val
	}
	return fallback
}

func (b *BaseHandler) GetInt64(c *gin.Context, key string, fallback int64) int64 {
	val := b.Get(c, key)
	if val == "" {
		return fallback
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func (b *BaseHandler) GetUint64(c *gin.Context, key string, fallback uint64) uint64 {
	val := b.Get(c, key)
	if val == "" {
		return fallback
	}
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func (b *BaseHandler) GetInt(c *gin.Context, key string, fallback int) int {
	val := b.Get(c, key)
	if val == "" {
		return fallback
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return n
}

func (b *BaseHandler) GetBool(c *gin.Context, key string, fallback bool) bool {
	val := strings.ToLower(b.Get(c, key))
	switch val {
	case "1", "true":
		return true
	case "0", "false":
		return false
	default:
		return fallback
	}
}

func (b *BaseHandler) GetPeerID(c *gin.Context) int64 {
	if uID := b.GetInt64(c, "user_id", 0); uID != 0 {
		return uID
	}
	if cID := b.GetInt64(c, "chat_id", 0); cID > 0 {
		return 2000000000 + cID
	}
	return b.GetInt64(c, "peer_id", 0)
}

func GetApiV(c *gin.Context) db_models.ApiV {
	if val, exists := c.Get("apiV"); exists {
		if ver, ok := val.(db_models.ApiV); ok {
			return ver
		}
	}
	return ExtractApiV(c)
}

func ExtractApiV(c *gin.Context) db_models.ApiV {
	vStr := c.Query("v")
	if vStr == "" {
		vStr = c.PostForm("v")
	}
	return db_models.ParseApiV(vStr)
}

func (b *BaseHandler) Error(c *gin.Context, httpStatus int, code int, msg string) {
	c.JSON(httpStatus, gin.H{
		"error": VKError{
			ErrorCode: code,
			ErrorMsg:  msg,
		},
	})
}

func (h *BaseHandler) Reject(c *gin.Context, errorCode int, errorMsg string) {
	params := make([]RequestParam, 0)
	for k, v := range c.Request.URL.Query() {
		params = append(params, RequestParam{Key: k, Value: v[0]})
	}

	c.JSON(http.StatusOK, gin.H{
		"error": VKError{
			ErrorCode:     errorCode,
			ErrorMsg:      errorMsg,
			RequestParams: params,
		},
	})
}

var (
	reAttachment   = regexp.MustCompile(`^(?:(photo|video|audio|doc|wall|market|poll|question|gift)-?\d+_\d+(?:_[a-zA-Z0-9]+)?|sticker\d+)$`)
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s\)]+)\)`)
	rePlainURL     = regexp.MustCompile(`(?:^|[\s\(\[\{<]|&gt;)(https?://[^\s<>"'\]\)]+)`)
)

type LinkAttachment struct {
	Type string                 `json:"type"`
	Link map[string]interface{} `json:"link"`
}

func ExtractLinksFromText(text string) []LinkAttachment {
	if text == "" {
		return nil
	}

	seenURLs := make(map[string]bool)
	var links []LinkAttachment

	// 1. Extract Markdown links [title](url)
	mdMatches := reMarkdownLink.FindAllStringSubmatch(text, -1)
	for _, m := range mdMatches {
		if len(m) >= 3 {
			title := strings.TrimSpace(m[1])
			url := strings.TrimSpace(m[2])
			if url != "" && !seenURLs[url] {
				seenURLs[url] = true
				links = append(links, LinkAttachment{
					Type: "link",
					Link: map[string]interface{}{
						"url":         url,
						"title":       title,
						"description": url,
					},
				})
			}
		}
	}

	// 2. Extract plain URLs
	plainMatches := rePlainURL.FindAllStringSubmatch(text, -1)
	for _, m := range plainMatches {
		if len(m) >= 2 {
			url := strings.TrimSpace(m[1])
			if url != "" && !seenURLs[url] {
				seenURLs[url] = true
				title := url
				if u, err := urlPkg.Parse(url); err == nil && u.Host != "" {
					title = u.Host
				}
				links = append(links, LinkAttachment{
					Type: "link",
					Link: map[string]interface{}{
						"url":         url,
						"title":       title,
						"description": url,
					},
				})
			}
		}
	}

	return links
}

func BuildFinalAttachments(attachmentStr string, text string) string {
	links := ExtractLinksFromText(text)
	if len(links) == 0 {
		return attachmentStr
	}

	var allItems []interface{}

	if strings.TrimSpace(attachmentStr) != "" {
		var jsonArr []interface{}
		if err := json.Unmarshal([]byte(attachmentStr), &jsonArr); err == nil {
			allItems = append(allItems, jsonArr...)
		} else {
			parts := strings.Split(attachmentStr, ",")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					allItems = append(allItems, p)
				}
			}
		}
	}

	for _, l := range links {
		allItems = append(allItems, l)
	}

	bytes, err := json.Marshal(allItems)
	if err != nil {
		return attachmentStr
	}
	return string(bytes)
}

func IsValidAttachments(attachmentStr string) bool {
	if attachmentStr == "" {
		return true
	}

	parts := strings.Split(attachmentStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)

		if part == "" {
			continue
		}

		if !reAttachment.MatchString(part) {
			return false
		}
	}
	return true
}

func ParseAttachmentsForLongPoll(attachmentStr string) map[string]interface{} {
	res := make(map[string]interface{})
	if attachmentStr == "" {
		return res
	}

	parts := strings.Split(attachmentStr, ",")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		idx := strconv.Itoa(i + 1)

		typeEnd := 0
		for j, char := range part {
			if (char >= '0' && char <= '9') || char == '-' {
				typeEnd = j
				break
			}
		}

		if typeEnd > 0 {
			res["attach"+idx+"_type"] = part[:typeEnd]
			res["attach"+idx] = part[typeEnd:]
		}
	}

	return res
}

func (r *BaseHandler) BroadcastChatSomethingChanged(ctx *gin.Context, peerID int64, actorID int64) {
	go func(pID, aID int64) {
		ctx := context.Background()
		var memberIDs []int64

		icID := chat.GetInternalChatID(pID, aID)

		db.Instance.Model(&db_models.ConversationMember{}).
			Where("internal_chat_id = ? AND left_at IS NULL", icID).
			Pluck("user_id", &memberIDs)

		for _, uid := range memberIDs {
			self := 0
			if uid == actorID {
				self = 1
			}

			lpEvent := lp_models.ChatSomethingChangedEvent{
				ChatID: peerID,
				Self:   uint8(self),
			}

			r.LPRepo.PushEvent(ctx, uid, "chat_something_changed", lpEvent)
			r.Broadcaster.Notify(uid)
		}
	}(peerID, actorID)
}

func (r *BaseHandler) BroadcastMarkAsRead(ctx context.Context, chatID string, userID int64, lastReadID uint64) {
	currentPeerID := chat.DerivePeerID(chatID, userID)

	// Resolve lastReadID to the last *incoming* message the user has read.
	// If the user marked their own outgoing message (e.g. after sending it),
	// we should not emit a false read receipt to the other party.
	var effectiveLastReadID uint64
	db.Instance.Table("messages").
		Where("chat_id = ? AND local_id <= ? AND from_id != ?", chatID, lastReadID, userID).
		Select("COALESCE(MAX(local_id), 0)").
		Row().Scan(&effectiveLastReadID)

	var unreadCount int64
	unreadQ := db.Instance.Table("messages").
		Where("chat_id = ? AND local_id > ? AND from_id != ?", chatID, lastReadID, userID)
	unreadQ = db_models.BuildVisibilityFilter(unreadQ, chatID, userID)
	unreadQ.Count(&unreadCount)

	r.LPRepo.PushEvent(ctx, userID, "read_income_before", lp_models.ReadIncomeBeforeEvent{
		PeerID:  currentPeerID,
		LocalID: lastReadID,
		Count:   int(unreadCount),
	})
	r.Broadcaster.Notify(userID)

	r.BroadcastCounterUpdate(ctx, userID)

	if effectiveLastReadID == 0 {
		// Nothing incoming was read — no need to notify other members
		return
	}

	go func(cID string, uID int64, lrID uint64, origPeerID int64) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var members []int64
		if strings.HasPrefix(cID, "c") {
			_ = db.Instance.Model(&db_models.ConversationMember{}).
				Where("internal_chat_id = ? AND user_id != ? AND left_at IS NULL", cID, uID).
				Pluck("user_id", &members).Error
		} else if strings.HasPrefix(cID, "dm") {
			parts := strings.Split(cID[2:], "_")
			if len(parts) == 2 {
				id1, _ := strconv.ParseInt(parts[0], 10, 64)
				id2, _ := strconv.ParseInt(parts[1], 10, 64)
				if id1 == uID {
					members = []int64{id2}
				} else {
					members = []int64{id1}
				}
			}
		}

		var targetPeerID int64
		if strings.HasPrefix(cID, "c") {
			targetPeerID = origPeerID
		} else {
			targetPeerID = uID
		}

		for _, mID := range members {
			var outUnreadCount int64
			outQ := db.Instance.Table("messages").
				Where("chat_id = ? AND local_id > ? AND from_id = ?", cID, lrID, mID)
			outQ = db_models.BuildVisibilityFilter(outQ, cID, mID)
			outQ.Count(&outUnreadCount)

			r.LPRepo.PushEvent(bgCtx, mID, "read_outcome_before", lp_models.ReadOutcomeBeforeEvent{
				PeerID:  targetPeerID,
				LocalID: lrID,
				Count:   int(outUnreadCount),
			})
			r.Broadcaster.Notify(mID)
		}
	}(chatID, userID, effectiveLastReadID, currentPeerID)
}

func (r *BaseHandler) SendDeleteEvent(uid int64, chatID string, localID uint64, flags uint64, forAll bool) {
	go func() {
		ctx := context.Background()
		if forAll {
			var members []int64
			r.DB.Model(&db_models.ConversationMember{}).Where("internal_chat_id = ?", chatID).Pluck("user_id", &members)

			for _, memberID := range members {
				peerID := chat.DerivePeerID(chatID, memberID)

				// 1. msg_set_flags (event 2)
				setFlagsEvent := lp_models.MsgSetFlagsEvent{
					MessageID: localID,
					Mask:      lp_models.MessageFlags{Value: lp_models.FlagDeleted | lp_models.FlagDeleteForAll},
					PeerID:    peerID,
				}
				r.LPRepo.PushEvent(ctx, memberID, "msg_set_flags", setFlagsEvent)

				// 2. msg_delete (event 0)
				delEvent := lp_models.MsgDeleteEvent{
					MessageID: localID,
				}
				r.LPRepo.PushEvent(ctx, memberID, "msg_delete", delEvent)

				// 3. msg_replace_flags (event 1)
				replaceFlagsEvent := lp_models.MsgReplaceFlagsEvent{
					MessageID: localID,
					Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
					PeerID:    peerID,
				}
				r.LPRepo.PushEvent(ctx, memberID, "msg_replace_flags", replaceFlagsEvent)

				r.Broadcaster.Notify(memberID)
			}
		} else {
			peerID := chat.DerivePeerID(chatID, uid)

			// 1. msg_set_flags (event 2)
			setFlagsEvent := lp_models.MsgSetFlagsEvent{
				MessageID: localID,
				Mask:      lp_models.MessageFlags{Value: lp_models.FlagDeleted},
				PeerID:    peerID,
			}
			r.LPRepo.PushEvent(ctx, uid, "msg_set_flags", setFlagsEvent)

			// 2. msg_delete (event 0)
			delEvent := lp_models.MsgDeleteEvent{
				MessageID: localID,
			}
			r.LPRepo.PushEvent(ctx, uid, "msg_delete", delEvent)

			// 3. msg_replace_flags (event 1)
			replaceFlagsEvent := lp_models.MsgReplaceFlagsEvent{
				MessageID: localID,
				Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
				PeerID:    peerID,
			}
			r.LPRepo.PushEvent(ctx, uid, "msg_replace_flags", replaceFlagsEvent)

			r.Broadcaster.Notify(uid)
		}
	}()
}

func (r *BaseHandler) SendRestoreEvent(uid int64, chatID string, localID uint64, flags uint64, forAll bool) {
	go func() {
		ctx := context.Background()
		if forAll {
			var members []int64
			r.DB.Model(&db_models.ConversationMember{}).Where("internal_chat_id = ?", chatID).Pluck("user_id", &members)

			for _, memberID := range members {
				peerID := chat.DerivePeerID(chatID, memberID)

				// 1. msg_reset_flags (event 3)
				resetFlagsEvent := lp_models.MsgResetFlagsEvent{
					MessageID: localID,
					Mask:      lp_models.MessageFlags{Value: lp_models.FlagDeleted | lp_models.FlagDeleteForAll},
					PeerID:    peerID,
				}
				r.LPRepo.PushEvent(ctx, memberID, "msg_reset_flags", resetFlagsEvent)

				// 2. msg_replace_flags (event 1)
				replaceFlagsEvent := lp_models.MsgReplaceFlagsEvent{
					MessageID: localID,
					Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
					PeerID:    peerID,
				}
				r.LPRepo.PushEvent(ctx, memberID, "msg_replace_flags", replaceFlagsEvent)

				r.Broadcaster.Notify(memberID)
			}
		} else {
			peerID := chat.DerivePeerID(chatID, uid)

			// 1. msg_reset_flags (event 3)
			resetFlagsEvent := lp_models.MsgResetFlagsEvent{
				MessageID: localID,
				Mask:      lp_models.MessageFlags{Value: lp_models.FlagDeleted},
				PeerID:    peerID,
			}
			r.LPRepo.PushEvent(ctx, uid, "msg_reset_flags", resetFlagsEvent)

			// 2. msg_replace_flags (event 1)
			replaceFlagsEvent := lp_models.MsgReplaceFlagsEvent{
				MessageID: localID,
				Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
				PeerID:    peerID,
			}
			r.LPRepo.PushEvent(ctx, uid, "msg_replace_flags", replaceFlagsEvent)

			r.Broadcaster.Notify(uid)
		}
	}()
}

func (r *BaseHandler) SendFlagsUpdate(uid int64, chatID string, localID uint64, flags uint64, forAll bool) {
	go func() {
		if forAll {
			var members []int64
			r.DB.Model(&db_models.ConversationMember{}).Where("internal_chat_id = ?", chatID).Pluck("user_id", &members)

			for _, memberID := range members {
				event := lp_models.MsgReplaceFlagsEvent{
					MessageID: localID,
					Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
					PeerID:    chat.DerivePeerID(chatID, memberID),
				}

				r.LPRepo.PushEvent(context.Background(), memberID, "msg_replace_flags", event)
				r.Broadcaster.Notify(memberID)
			}
		} else {
			event := lp_models.MsgReplaceFlagsEvent{
				MessageID: localID,
				Flags:     lp_models.MessageFlags{Value: lp_models.MessageFlag(flags)},
				PeerID:    chat.DerivePeerID(chatID, uid),
			}

			r.LPRepo.PushEvent(context.Background(), uid, "msg_replace_flags", event)
			r.Broadcaster.Notify(uid)
		}
	}()
}

func (r *BaseHandler) SendUpdateEvent(peerID int64, localID uint64, text string, attachStr string, senderID int64) {
	internalChatID := chat.GetInternalChatID(peerID, senderID)

	go func(pID int64, lID uint64, txt, attach string, sID int64, chatId string) {
		lpAttach := lp_models.NewLPAttachments(attach)
		lpAttach.From = sID

		updateEvent := lp_models.UpdateMessageEvent{
			MessageID:   lID,
			PeerID:      pID,
			NewText:     txt,
			Attachments: &lpAttach,
			Timestamp:   uint64(time.Now().Unix()),
		}

		var recipients []int64

		if strings.HasPrefix(chatId, "c") {
			r.DB.Model(&db_models.ConversationMember{}).
				Where("internal_chat_id = ? AND left_at IS NULL", chatId).
				Pluck("user_id", &recipients)
		} else {
			recipients = append(recipients, sID)
			if pID != sID {
				recipients = append(recipients, pID)
			}
		}

		for _, uid := range recipients {
			r.LPRepo.PushEvent(context.Background(), uid, "msg_update", updateEvent)
			r.Broadcaster.Notify(uid)
		}
	}(peerID, localID, text, attachStr, senderID, internalChatID)
}

func CollectAllEntityIDs(items []db_models.VKApiMessage, userIDs *[]int64, groupIDs *[]int64) {
	uMap := make(map[int64]struct{})
	gMap := make(map[int64]struct{})

	var scan func(m db_models.VKApiMessage)
	scan = func(m db_models.VKApiMessage) {
		if m.FromID > 0 {
			uMap[m.FromID] = struct{}{}
		} else if m.FromID < 0 {
			gMap[-m.FromID] = struct{}{}
		}

		if m.PeerID > 0 && m.PeerID < 2000000000 {
			uMap[m.PeerID] = struct{}{}
		} else if m.PeerID < 0 {
			gMap[-m.PeerID] = struct{}{}
		}

		if m.ReplyMessage != nil {
			scan(*m.ReplyMessage)
		}

		for _, fwd := range m.ForwardMessages {
			scan(fwd)
		}
	}

	for _, itm := range items {
		scan(itm)
	}

	for id := range uMap {
		*userIDs = append(*userIDs, id)
	}
	for id := range gMap {
		*groupIDs = append(*groupIDs, id)
	}
}

func CollectAllEntityIDsLegacy(items []db_models.VKApiMessageLegacy, userIDs *[]int64, groupIDs *[]int64, chatIDs *[]int64) {
	uMap := make(map[int64]struct{})
	gMap := make(map[int64]struct{})
	cMap := make(map[int64]struct{})

	var scan func(m db_models.VKApiMessageLegacy)
	scan = func(m db_models.VKApiMessageLegacy) {
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
			scan(fwd)
		}
	}

	for _, itm := range items {
		scan(itm)
	}

	for id := range uMap {
		*userIDs = append(*userIDs, id)
	}
	for id := range gMap {
		*groupIDs = append(*groupIDs, id)
	}
	if chatIDs != nil {
		for id := range cMap {
			*chatIDs = append(*chatIDs, id)
		}
	}
}

func (r *BaseHandler) SendChatFlagsUpdate(userID int64, peerID int64, flags uint64) {
	event := lp_models.ChatReplaceFlagsEvent{
		PeerID: peerID,
		Flags:  flags,
	}
	r.LPRepo.PushEvent(context.Background(), userID, "chat_replace_flags", event)
	r.Broadcaster.Notify(userID)
}

func (r *BaseHandler) UpdateChatFlags(userID int64, peerID int64, mask uint64, mode string) error {
	internalChatID := chat.GetInternalChatID(peerID, userID)

	var member db_models.ConversationMember
	err := r.DB.Where("user_id = ? AND internal_chat_id = ?", userID, internalChatID).First(&member).Error
	if err != nil {
		return err
	}

	newFlags := uint64(member.Flags)
	switch mode {
	case "set":
		newFlags |= mask
	case "reset":
		newFlags &= ^mask
	case "replace":
		newFlags = mask
	}

	err = r.DB.Model(&db_models.ConversationMember{}).
		Where("user_id = ? AND internal_chat_id = ?", userID, internalChatID).
		Update("flags", newFlags).Error
	if err != nil {
		return err
	}

	r.SendChatFlagsUpdate(userID, peerID, newFlags)

	return nil
}

func (r *BaseHandler) BackgroundDeleteChat(userID int64, peerID int64, internalChatID string) {
	tx := r.DB.Begin()

	var maxLocalID uint64
	tx.Table("messages").
		Where("chat_id = ?", internalChatID).
		Select("COALESCE(MAX(local_id), 0)").
		Row().
		Scan(&maxLocalID)

	updates := map[string]interface{}{
		"deleted_before_id": maxLocalID,
		"last_read_id":      maxLocalID,
	}

	err := tx.Model(&db_models.ConversationMember{}).
		Where("user_id = ? AND internal_chat_id = ?", userID, internalChatID).
		Updates(updates).Error

	if err != nil {
		tx.Rollback()
		return
	}

	tx.Commit()

	r.SendChatDeleteEvent(userID, peerID)
	r.BroadcastCounterUpdate(context.Background(), userID)
}

func (r *BaseHandler) SendChatDeleteEvent(userID int64, peerID int64) {
	event := lp_models.MassDeleteMessagesEvent{
		PeerID: peerID,
	}
	r.LPRepo.PushEvent(context.Background(), userID, "chat_delete_all", event)
	r.Broadcaster.Notify(userID)
}

func (r *BaseHandler) BroadcastCounterUpdate(ctx context.Context, userID int64) {
	if userID == 0 || r.LPRepo == nil {
		return
	}

	count, err := chat.CountUnreadConversations(db.Instance, userID)
	if err != nil {
		return
	}

	r.LPRepo.PushEvent(ctx, userID, "counter", lp_models.CounterUpdateEvent{
		Count: uint(count),
	})
	if r.Broadcaster != nil {
		r.Broadcaster.Notify(userID)
	}
}

func TruncateWords(text string, length int) string {
	if length <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= length {
		return text
	}

	nextRune := runes[length]
	if unicode.IsSpace(nextRune) {
		trimmed := strings.TrimRightFunc(string(runes[:length]), unicode.IsSpace)
		return trimmed + "…"
	}

	lastSpaceIdx := -1
	for i := length - 1; i >= 0; i-- {
		if unicode.IsSpace(runes[i]) {
			lastSpaceIdx = i
			break
		}
	}

	if lastSpaceIdx > 0 {
		trimmed := strings.TrimRightFunc(string(runes[:lastSpaceIdx]), unicode.IsSpace)
		return trimmed + "…"
	}

	return string(runes[:length]) + "…"
}

func UniqueIDs(ids []int64) []int64 {
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
