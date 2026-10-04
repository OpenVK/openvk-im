package chat

import (
	"errors"
	dbx "ovk-im/src/db"
	db_models "ovk-im/src/models/db"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrGroupNoPermission = errors.New("community can not start conversation")
	ErrChatNotFound      = errors.New("conversation not found")
)

func getDB(tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return dbx.Instance
}

func IsUserInChat(tx *gorm.DB, chatID string, userID int64) (bool, error) {
	var count int64
	err := getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ? AND left_at IS NULL", chatID, userID).
		Count(&count).Error
	return count > 0, err
}

func GetMember(tx *gorm.DB, chatID string, userID int64) (*db_models.ConversationMember, error) {
	var member db_models.ConversationMember
	err := getDB(tx).Where("internal_chat_id = ? AND user_id = ?", chatID, userID).First(&member).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &member, nil
}

func NextLocalID(tx *gorm.DB, chatID string, fromID int64) (uint64, error) {
	var conv db_models.Conversation
	var maxID uint64

	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("internal_id = ?", chatID).
		First(&conv).Error

	isNewConv := false
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			isNewConv = true
		} else {
			return 0, err
		}
	}

	err = tx.Table("messages").
		Unscoped().
		Where("chat_id = ?", chatID).
		Select("COALESCE(MAX(local_id), 0)").
		Row().
		Scan(&maxID)

	if err != nil {
		return 0, err
	}

	if conv.LastMessageID > maxID {
		maxID = conv.LastMessageID
	}

	nextID := maxID + 1

	if isNewConv {
		newConv := db_models.Conversation{
			InternalID:    chatID,
			LastMessageID: nextID,
		}
		if err := tx.Create(&newConv).Error; err != nil {
			return 0, err
		}
	} else {
		err = tx.Table("conversations").
			Where("id = ?", conv.ID).
			Update("last_message_id", nextID).Error
		if err != nil {
			return 0, err
		}
	}

	return nextID, nil
}

func MarkAsRead(tx *gorm.DB, chatID string, userID int64, messageID uint64) error {
	var conv db_models.Conversation
	if getDB(tx).Select("last_message_id").Where("internal_id = ?", chatID).First(&conv).Error == nil && conv.LastMessageID > 0 {
		if messageID > conv.LastMessageID {
			messageID = conv.LastMessageID
		}
	}
	return getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ?", chatID, userID).
		Where("last_read_id < ?", messageID).
		Update("last_read_id", messageID).Error
}

func GetActiveMemberIDs(tx *gorm.DB, chatID string) ([]int64, error) {
	var ids []int64
	err := getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND left_at IS NULL", chatID).
		Pluck("user_id", &ids).Error
	return ids, err
}

func GetConversation(tx *gorm.DB, chatID string) (*db_models.Conversation, error) {
	var conv db_models.Conversation
	err := getDB(tx).Where("internal_id = ?", chatID).First(&conv).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &conv, nil
}

func CreateConversation(ownerID int64, userIDs []int64, groupTitle string) (*db_models.Conversation, error) {
	var conv db_models.Conversation
	err := dbx.Instance.Transaction(func(tx *gorm.DB) error {
		conv = db_models.Conversation{
			OwnerID:   &ownerID,
			Title:     groupTitle,
			Settings:  []byte("{}"),
			CreatedAt: time.Now(),
		}

		if err := tx.Create(&conv).Error; err != nil {
			return err
		}

		conv.InternalID = "c" + strconv.FormatUint(conv.ID, 10)

		if err := tx.Table("conversations").Where("id = ?", conv.ID).Updates(map[string]interface{}{
			"internal_id": conv.InternalID,
		}).Error; err != nil {
			return err
		}

		chatID := conv.InternalID

		var members []db_models.ConversationMember
		var periods []db_models.ConversationMemberPeriod

		addMember := func(uID int64, isAdmin bool) {
			members = append(members, db_models.ConversationMember{
				InternalChatID: chatID,
				UserID:         uID,
				IsAdmin:        isAdmin,
				JoinedAt:       time.Now(),
				InvitedBy:      ownerID,
			})
			periods = append(periods, db_models.ConversationMemberPeriod{
				InternalChatID: chatID,
				UserID:         uID,
				StartLocalID:   1,
				EndLocalID:     nil,
			})
		}

		addMember(ownerID, true)

		for _, uID := range userIDs {
			if uID == ownerID {
				continue
			}
			addMember(uID, false)
		}

		if err := tx.Create(&periods).Error; err != nil {
			return err
		}

		return tx.Create(&members).Error
	})

	return &conv, err
}

func AddUserToConversation(chatID string, userID int64, inviterID int64, text string, action string, actionMid int64, actionText string, canSeeHistory ...bool) (*db_models.Message, error) {
	if !strings.HasPrefix(chatID, "c") {
		return nil, errors.New("cannot add user to direct message")
	}

	if actionMid == 0 {
		actionMid = inviterID
	}

	seeHistory := false
	if len(canSeeHistory) > 0 && canSeeHistory[0] {
		seeHistory = true
	}

	var msg *db_models.Message

	err := dbx.Instance.Transaction(func(tx *gorm.DB) error {
		var member db_models.ConversationMember
		findErr := tx.Where("internal_chat_id = ? AND user_id = ?", chatID, userID).First(&member).Error

		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		if findErr == nil && member.LeftAt == nil {
			return errors.New("user is already in the conversation")
		}

		if findErr == nil && member.LeftAt != nil {
			var lastKickMsg db_models.Message
			if errK := tx.Where("chat_id = ? AND action = ? AND action_mid = ?", chatID, "chat_kick_user", userID).Order("local_id DESC").First(&lastKickMsg).Error; errK == nil && lastKickMsg.ID > 0 {
				if lastKickMsg.FromID != userID {
					// User was kicked from this chat
					if inviterID == userID {
						return errors.New("you have been kicked from this chat")
					}
					if !IsChatOwner(tx, chatID, inviterID) && !IsChatModerator(tx, chatID, inviterID) {
						return errors.New("only administrators can re-invite kicked users")
					}
				}
			}
		}

		isNewMember := errors.Is(findErr, gorm.ErrRecordNotFound)
		if isNewMember && inviterID == userID && action != "chat_invite_user_by_link" {
			return errors.New("cannot self-join a conversation you were never a member of")
		}

		localID, err := NextLocalID(tx, chatID, inviterID)
		if err != nil {
			return err
		}

		msg = &db_models.Message{
			FromID:     inviterID,
			ChatID:     chatID,
			LocalID:    localID,
			Text:       db_models.EncryptedJSON(text),
			Action:     action,
			ActionMid:  actionMid,
			ActionText: actionText,
			CreatedAt:  time.Now(),
		}

		if err := tx.Create(msg).Error; err != nil {
			return err
		}

		startLocalID := localID
		if seeHistory {
			startLocalID = 1
		}

		// Create a new active period starting from this service message (or 1 if history visible)
		newPeriod := db_models.ConversationMemberPeriod{
			InternalChatID: chatID,
			UserID:         userID,
			StartLocalID:   startLocalID,
			EndLocalID:     nil,
		}
		if err := tx.Create(&newPeriod).Error; err != nil {
			return err
		}

		if isNewMember {
			newMember := db_models.ConversationMember{
				InternalChatID: chatID,
				UserID:         userID,
				IsAdmin:        false,
				JoinedAt:       time.Now(),
				InvitedBy:      inviterID,
				StartMessageID: uint64(msg.ID),
				LastMessageID:  localID,
			}
			if err := tx.Create(&newMember).Error; err != nil {
				return err
			}
		} else {
			err = tx.Model(&db_models.ConversationMember{}).
				Where("internal_chat_id = ? AND user_id = ?", chatID, userID).
				Updates(map[string]interface{}{
					"left_at":          gorm.Expr("NULL"),
					"invited_by":       inviterID,
					"joined_at":        time.Now(),
					"start_message_id": uint64(msg.ID),
					"last_message_id":  localID,
				}).Error
			if err != nil {
				return err
			}
		}

		err = tx.Table("conversation_members").
			Where("internal_chat_id = ? AND left_at IS NULL AND user_id != ?", chatID, userID).
			Update("last_message_id", localID).Error

		return err
	})

	if err != nil {
		return nil, err
	}

	return msg, nil
}

func IsChatOwner(tx *gorm.DB, chatID string, userID int64) bool {
	var conv db_models.Conversation
	if getDB(tx).Select("owner_id").Where("internal_id = ?", chatID).First(&conv).Error != nil || conv.OwnerID == nil {
		return false
	}
	return *conv.OwnerID == userID
}

func IsChatModerator(tx *gorm.DB, chatID string, userID int64) bool {
	member, err := GetMember(tx, chatID, userID)
	if err != nil || member == nil || !member.IsAdmin {
		return false
	}
	return !IsChatOwner(tx, chatID, userID)
}

func RemoveUserFromConversation(tx *gorm.DB, chatID string, userID int64) error {
	updates := map[string]interface{}{
		"left_at": time.Now(),
	}
	if !IsChatOwner(tx, chatID, userID) {
		updates["is_admin"] = false
	}
	return getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ?", chatID, userID).
		Updates(updates).Error
}

func CloseActiveMemberPeriod(tx *gorm.DB, chatID string, userID int64, endLocalID uint64) error {
	_ = getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ? AND user_id = ?", chatID, userID).
		Update("last_message_id", endLocalID).Error
	return getDB(tx).Model(&db_models.ConversationMemberPeriod{}).
		Where("internal_chat_id = ? AND user_id = ? AND end_local_id IS NULL", chatID, userID).
		Update("end_local_id", endLocalID).Error
}

func EnsureMemberPeriod(tx *gorm.DB, chatID string, userID int64, startLocalID uint64) error {
	var count int64
	err := getDB(tx).Model(&db_models.ConversationMemberPeriod{}).
		Where("internal_chat_id = ? AND user_id = ?", chatID, userID).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return getDB(tx).Create(&db_models.ConversationMemberPeriod{
			InternalChatID: chatID,
			UserID:         userID,
			StartLocalID:   startLocalID,
			EndLocalID:     nil,
		}).Error
	}
	return nil
}

/*
Мини документация по сервисным сообщениям:
chat_create       - Создана беседа пользователем (fromID & Mid)
chat_title_update - Обновлено название беседы пользователем (fromID & Mid)
chat_photo_remove - Удалена фотка беседы пользователем (fromID & Mid)
chat_photo_update - Обновлена фотка беседы пользователем (fromID & Mid)
chat_invite_user  - Пользователем fromID приглашём пользователь Mid
chat_kick_user    - Пользователем fromID исключён пользователь Mid
chat_pin_message  - Пользователь fromID закрепил сообщение Mid
chat_unpin_message - Пользователь fromID открепил сообщение Mid
chat_invite_user_by_link - Пользователь (fromID & Mid) зашёл в чат по ссылке приглашению
*/

func CreateServiceMessage(fromID int64, chatID string, text string, action string, actionMid int64, actionText string) (*db_models.Message, error) {
	if actionMid == 0 {
		actionMid = fromID
	}

	var msg *db_models.Message

	err := dbx.Instance.Transaction(func(tx *gorm.DB) error {
		localID, err := NextLocalID(tx, chatID, fromID)
		if err != nil {
			return err
		}

		msg = &db_models.Message{
			FromID:     fromID,
			ChatID:     chatID,
			LocalID:    localID,
			Text:       db_models.EncryptedJSON(text),
			Action:     action,
			ActionMid:  actionMid,
			ActionText: actionText,
			CreatedAt:  time.Now(),
		}

		if err := tx.Create(msg).Error; err != nil {
			return err
		}

		err = tx.Table("conversation_members").
			Where("internal_chat_id = ? AND left_at IS NULL", chatID).
			Update("last_message_id", localID).Error

		return err
	})

	if err != nil {
		return nil, err
	}

	return msg, nil
}

func GetInternalChatID(peerID int64, currentUserID int64) string {
	if peerID > 2000000000 {
		return "c" + strconv.FormatInt(peerID-2000000000, 10)
	}
	if peerID < 0 || currentUserID < 0 {
		var groupID, userID int64
		if peerID < 0 {
			groupID = peerID
			userID = currentUserID
		} else {
			groupID = currentUserID
			userID = peerID
		}
		return "g" + strconv.FormatInt(groupID, 10) + "_" + strconv.FormatInt(userID, 10)
	}

	u1, u2 := currentUserID, peerID
	if u1 > u2 {
		u1, u2 = u2, u1
	}
	return "dm" + strconv.FormatInt(u1, 10) + "_" + strconv.FormatInt(u2, 10)
}

func ResolveChatID(chatIDParam string, peerID int64, userID int64, currentUserID int64) string {
	if strings.HasPrefix(chatIDParam, "c") || strings.HasPrefix(chatIDParam, "dm") || strings.HasPrefix(chatIDParam, "g") {
		return chatIDParam
	}
	if currentUserID == 0 && userID != 0 && peerID != 0 {
		return GetInternalChatID(peerID, userID)
	}
	if peerID != 0 {
		return GetInternalChatID(peerID, currentUserID)
	}
	if chatIDParam != "" {
		if id, err := strconv.ParseInt(chatIDParam, 10, 64); err == nil && id > 0 {
			return GetInternalChatID(2000000000+id, currentUserID)
		}
	}
	if userID != 0 {
		return GetInternalChatID(userID, currentUserID)
	}
	return ""
}


func DerivePeerID(chatID string, currentUserID int64) int64 {
	if strings.HasPrefix(chatID, "c") {
		id, _ := strconv.ParseInt(chatID[1:], 10, 64)
		return id + 2000000000
	}

	if strings.HasPrefix(chatID, "g") {
		parts := strings.Split(chatID[1:], "_")
		if len(parts) < 2 {
			return 0
		}
		id1, _ := strconv.ParseInt(parts[0], 10, 64)
		id2, _ := strconv.ParseInt(parts[1], 10, 64)

		if id1 == currentUserID {
			return id2
		}
		return id1
	}

	if strings.HasPrefix(chatID, "dm") {
		parts := strings.Split(chatID[2:], "_")
		id1, _ := strconv.ParseInt(parts[0], 10, 64)
		id2, _ := strconv.ParseInt(parts[1], 10, 64)
		if id1 == currentUserID {
			return id2
		}
		return id1
	}
	return 0
}

func RefreshChatLastMessage(tx *gorm.DB, internalChatID string) error {
	var lastMsg db_models.Message
	err := getDB(tx).Where("chat_id = ? AND (flags & 128) = 0", internalChatID).
		Order("local_id DESC").
		First(&lastMsg).Error

	var newLastID uint64 = 0
	if err == nil {
		newLastID = lastMsg.LocalID
	}

	if err := getDB(tx).Model(&db_models.Conversation{}).
		Where("internal_id = ?", internalChatID).
		Update("last_message_id", newLastID).Error; err != nil {
		return err
	}

	return getDB(tx).Model(&db_models.ConversationMember{}).
		Where("internal_chat_id = ?", internalChatID).
		Update("last_message_id", newLastID).Error
}

func ResolveGlobalMsgID(tx *gorm.DB, chatID string, localID uint64, lastCMID uint64, lastMsgID uint64) uint64 {
	if localID == 0 {
		return 0
	}
	if lastCMID > 0 && localID >= lastCMID && lastMsgID > 0 {
		return lastMsgID
	}
	var msgID uint64
	dbRef := tx
	if dbRef == nil {
		dbRef = dbx.Instance
	}
	if dbRef != nil {
		dbRef.Table("messages").Select("id").Where("chat_id = ? AND local_id = ?", chatID, localID).Scan(&msgID)
		if msgID == 0 {
			dbRef.Table("messages").Select("id").Where("chat_id = ? AND local_id <= ?", chatID, localID).Order("local_id DESC").Limit(1).Scan(&msgID)
		}
	}
	if msgID == 0 {
		return localID
	}
	return msgID
}

func CountUnreadConversations(tx *gorm.DB, userID int64) (int64, error) {
	if userID == 0 {
		return 0, nil
	}
	var count int64
	unreadConvQ := getDB(tx).Table("messages").
		Joins("JOIN conversation_members ON conversation_members.internal_chat_id = messages.chat_id AND conversation_members.user_id = ? AND conversation_members.left_at IS NULL", userID).
		Where("messages.from_id != ?", userID).
		Where("messages.local_id > conversation_members.last_read_id").
		Where("messages.local_id > COALESCE(conversation_members.deleted_before_id, 0)")
	unreadConvQ = db_models.BuildVisibilityFilter(unreadConvQ, "", userID)
	err := unreadConvQ.Select("COUNT(DISTINCT messages.chat_id)").Scan(&count).Error
	return count, err
}

