package db_migrate

import (
	"fmt"
	"log"
	"strings"
	env "ovk-im/src/config"
	dbx "ovk-im/src/db"
	dbm "ovk-im/src/models/db"
	"ovk-im/src/repo/chat"
	"ovk-im/src/repo/search"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

func CreateOrMigrateDB() {
	err := dbx.Instance.AutoMigrate(
		&dbm.Conversation{},
		&dbm.Message{},
		&dbm.ConversationMember{},
		&dbm.ChatInvite{},
		&dbm.ImState{},
		&dbm.MessageSearchIndex{},
		&dbm.ImportantMessage{},
		&dbm.ConversationMemberPeriod{},
		&dbm.DeletedMessage{},
		&dbm.ConversationMute{},
		&dbm.MessageMention{},
	)

	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	log.Println("\n\nDatabase initialized")
}

type LegacyMessage struct {
	ID            int64  `gorm:"column:id;primaryKey"`
	SenderType    string `gorm:"column:sender_type"`
	SenderID      uint64 `gorm:"column:sender_id"`
	RecipientType string `gorm:"column:recipient_type"`
	RecipientID   uint64 `gorm:"column:recipient_id"`
	Content       string `gorm:"column:content"`
	Created       int64  `gorm:"column:created"`
	Edited        int64  `gorm:"column:edited"`
	Ad            bool   `gorm:"column:ad"`
	Deleted       bool   `gorm:"column:deleted"`
	Unread        bool   `gorm:"column:unread"`
}

func (LegacyMessage) TableName() string {
	return "messages"
}

type AttachmentResult struct {
	MessageID int64 `gorm:"column:message_id"`
	Owner     int64 `gorm:"column:owner"`
	VirtualID int64 `gorm:"column:virtual_id"`
}

func MigrateFromLegacy() {
	log.Println("Starting stream + batch migration from OpenVK legacy messages...")

	user := env.Get("DB_USER", "root")
	pass := env.Get("DB_PASS", "")
	host := env.Get("DB_HOST", "127.0.0.1")
	port := env.Get("DB_PORT", "3306")
	socket := strings.TrimSpace(env.Get("DB_SOCKET", ""))

	var legacyDSN string
	if socket != "" {
		legacyDSN = fmt.Sprintf("%s:%s@unix(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", user, pass, socket, "openvk")
	} else {
		legacyDSN = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", user, pass, host, port, "openvk")
	}
	legacyDB, err := gorm.Open(mysql.Open(legacyDSN), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to legacy database: %v", err)
	}

	if dbx.Instance == nil {
		dbx.Connect()
	}
	dbx.Instance.Logger = dbx.Instance.Logger.LogMode(logger.Silent)

	secret := env.Get("SECRET_KEY", "error")
	if len(secret) < 64 {
		log.Println("SECRET_KEY .env variable is less than 64 bytes")
		return
	}
	searchRepo := search.NewRepository(dbx.Instance, []byte(secret))

	localIDCache := make(map[string]uint64)
	memberCache := make(map[string]bool)

	rows, err := legacyDB.Model(&LegacyMessage{}).Rows()
	if err != nil {
		log.Fatalf("Failed to open rows stream: %v", err)
	}
	defer rows.Close()

	batchSize := 2000
	buffer := make([]LegacyMessage, 0, batchSize)
	batchCount := 0

	for rows.Next() {
		var msg LegacyMessage
		if err := legacyDB.ScanRows(rows, &msg); err != nil {
			log.Printf("Scan row error: %v", err)
			continue
		}

		buffer = append(buffer, msg)

		if len(buffer) >= batchSize {
			batchCount++
			if err := processAndFlushBatch(legacyDB, dbx.Instance, buffer, searchRepo, localIDCache, memberCache); err != nil {
				log.Printf("Failed to flush batch %d: %v", batchCount, err)
			}
			buffer = buffer[:0]
			log.Printf("Processed batch %d", batchCount)
		}
	}

	if len(buffer) > 0 {
		batchCount++
		if err := processAndFlushBatch(legacyDB, dbx.Instance, buffer, searchRepo, localIDCache, memberCache); err != nil {
			log.Printf("Failed to flush final batch %d: %v", batchCount, err)
		}
		log.Printf("Processed final batch %d", batchCount)
	}

	log.Println("Stream migration finished successfully!")
}

func processAndFlushBatch(legacyDB, targetDB *gorm.DB, legacyMessages []LegacyMessage, searchRepo *search.Repository, localIDCache map[string]uint64, memberCache map[string]bool) error {
	return targetDB.Transaction(func(targetTx *gorm.DB) error {
		var messagesToInsert []dbm.Message
		var indexesToInsert []dbm.MessageSearchIndex
		var membersToUpsert []dbm.ConversationMember
		touchedChatsInBatch := make(map[string]bool)

		msgIDs := make([]int64, 0, len(legacyMessages))
		var missingChatIDs []string

		for _, msg := range legacyMessages {
			msgIDs = append(msgIDs, msg.ID)

			currentUserID := int64(msg.SenderID)
			peerID := int64(msg.RecipientID)
			chatID := chat.GetInternalChatID(peerID, currentUserID)

			if _, ok := localIDCache[chatID]; !ok {
				missingChatIDs = append(missingChatIDs, chatID)
				localIDCache[chatID] = 0
			}
		}

		if len(missingChatIDs) > 0 {
			type Result struct {
				ChatID string
				MaxID  uint64
			}
			var results []Result
			targetTx.Model(&dbm.Message{}).
				Select("chat_id, COALESCE(max(local_id), 0) as max_id").
				Where("chat_id IN ?", missingChatIDs).
				Group("chat_id").
				Scan(&results)

			for _, r := range results {
				localIDCache[r.ChatID] = r.MaxID
			}
		}

		var attachResults []AttachmentResult
		legacyDB.Table("attachments").
			Select("attachments.target_id as message_id, photos.owner, photos.virtual_id").
			Joins("JOIN photos ON photos.id = attachments.attachable_id").
			Where("attachments.target_type = ? AND attachments.attachable_type = ? AND attachments.target_id IN ?",
				"openvk\\Web\\Models\\Entities\\Message",
				"openvk\\Web\\Models\\Entities\\Photo",
				msgIDs,
			).Scan(&attachResults)

		attachmentsMap := make(map[int64]string)
		for _, ar := range attachResults {
			attachStr := fmt.Sprintf("photo%d_%d", ar.Owner, ar.VirtualID)
			if existing, ok := attachmentsMap[ar.MessageID]; ok {
				attachmentsMap[ar.MessageID] = existing + "," + attachStr
			} else {
				attachmentsMap[ar.MessageID] = attachStr
			}
		}

		for _, msg := range legacyMessages {
			currentUserID := int64(msg.SenderID)
			peerID := int64(msg.RecipientID)
			chatID := chat.GetInternalChatID(peerID, currentUserID)

			localIDCache[chatID]++
			currentLocalID := localIDCache[chatID]
			touchedChatsInBatch[chatID] = true

			usersToCheck := []int64{currentUserID}
			if peerID < 2000000000 && peerID != currentUserID {
				usersToCheck = append(usersToCheck, peerID)
			}

			for _, uid := range usersToCheck {
				cacheKey := fmt.Sprintf("%s_%d", chatID, uid)
				if !memberCache[cacheKey] {
					membersToUpsert = append(membersToUpsert, dbm.ConversationMember{
						InternalChatID: chatID,
						UserID:         uid,
						JoinedAt:       time.Unix(msg.Created, 0),
						IsAdmin:        true,
						LastMessageID:  currentLocalID,
					})
					memberCache[cacheKey] = true
				}
			}

			msgTime := time.Unix(msg.Created, 0)

			var updatedTime *time.Time
			if msg.Edited > 0 {
				t := time.Unix(msg.Edited, 0)
				updatedTime = &t
			}

			var deletedAt *time.Time
			var flags uint64 = 0
			if msg.Deleted {
				flags = 128
				now := time.Now()
				deletedAt = &now
			}

			messagesToInsert = append(messagesToInsert, dbm.Message{
				ChatID:      chatID,
				LocalID:     currentLocalID,
				FromID:      currentUserID,
				Text:        dbm.EncryptedJSON(msg.Content),
				Attachments: dbm.EncryptedJSON(attachmentsMap[msg.ID]),
				Flags:       flags,
				CreatedAt:   msgTime,
				EditedAt:    updatedTime,
				DeletedAt:   deletedAt,
			})
		}

		if len(membersToUpsert) > 0 {
			if err := targetTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&membersToUpsert).Error; err != nil {
				return err
			}
		}

		if len(messagesToInsert) > 0 {
			if err := targetTx.Create(&messagesToInsert).Error; err != nil {
				return err
			}
		}

		for i, m := range messagesToInsert {
			legacyMsg := legacyMessages[i]
			if !legacyMsg.Deleted && legacyMsg.Content != "" {
				idx := searchRepo.GenerateBlindIndexes(m.ID, m.ChatID, string(legacyMsg.Content))
				indexesToInsert = append(indexesToInsert, idx...)
			}
		}

		if len(indexesToInsert) > 0 {
			if err := targetTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&indexesToInsert).Error; err != nil {
				return err
			}
		}

		for chatID := range touchedChatsInBatch {
			lastID := localIDCache[chatID]
			targetTx.Model(&dbm.ConversationMember{}).
				Where("internal_chat_id = ?", chatID).
				Update("last_message_id", lastID)

			err := targetTx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "internal_id"}},
				DoUpdates: clause.Assignments(map[string]interface{}{"last_message_id": lastID}),
			}).Create(&dbm.Conversation{
				InternalID:    chatID,
				LastMessageID: lastID,
				CreatedAt:     time.Now(),
			}).Error

			if err != nil {
				return err
			}
		}

		return nil
	})
}
