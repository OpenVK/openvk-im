package lp_models

import (
	"encoding/json"
	"fmt"
	db_models "ovk-im/src/models/db"
	"strconv"
	"strings"
	"time"
)

type Envelope struct {
	TS      uint64            `json:"ts"`
	Updates []json.RawMessage `json:"updates"`
	PTS     uint64            `json:"pts"`
	Failed  int               `json:"failed,omitempty"`
	MinVer  int               `json:"min_version,omitempty"`
	MaxVer  int               `json:"max_version,omitempty"`
}

func (e Envelope) MarshalJSON() ([]byte, error) {
	type alias Envelope
	a := alias(e)
	if a.Updates == nil {
		a.Updates = []json.RawMessage{}
	}
	return json.Marshal(a)
}

type LPConfig struct {
	Version   int
	Mode      uint32
	Described int
}

func (c LPConfig) HasAttachments() bool     { return (c.Mode & 2) != 0 }
func (c LPConfig) HasExtended() bool        { return (c.Mode & 8) != 0 }
func (c LPConfig) HasPTS() bool             { return (c.Mode & 32) != 0 }
func (c LPConfig) HasExtra() bool           { return (c.Mode & 64) != 0 }
func (c LPConfig) ReturnRandomID() bool     { return (c.Mode & 128) != 0 }
func (c LPConfig) ReturnCustomFields() bool { return (c.Mode & 256) != 0 }

type VKEvent interface {
	ToSlice(cfg LPConfig) interface{}
}

func Pack(e VKEvent, cfg LPConfig) ([]byte, error) {
	return json.Marshal(e.ToSlice(cfg))
}

type VKGetLongPollHistoryResponse struct {
	History  []interface{}          `json:"history"`
	Messages VKApiMessagesWithCount `json:"messages"`
	Profiles []int64                `json:"profiles"`
	Groups   []int64                `json:"groups"`
	NewPTS   uint64                 `json:"new_pts"`
	More     int                    `json:"more,omitempty"`
}

type VKApiMessagesWithCount struct {
	Count int                      `json:"count"`
	Items []db_models.VKApiMessage `json:"items"`
}

// Attachments

type LPAttachments struct {
	Items        []LPAttachmentItem
	ItemsPayload []interface{}
	Source       string
	Mid          string
	Emoji        bool
	From         int64
	ReplyTo      uint64
	Fwd          string
	CMID         uint64
	Mention      bool
	Muted        bool
}
type LPAttachmentItem struct {
	Type string
	Data string
}

func (a LPAttachments) ToMap() map[string]interface{} {
	res := make(map[string]interface{})

	for i, item := range a.Items {
		idx := strconv.Itoa(i + 1)
		res["attach"+idx+"_type"] = item.Type
		res["attach"+idx] = item.Data
	}

	if a.Source != "" {
		res["source_act"] = a.Source
	}
	if a.Mid != "" {
		res["source_mid"] = a.Mid
	}
	if a.Emoji {
		res["emoji"] = 1
	}
	if a.From != 0 {
		res["from"] = a.From
	}
	if a.ReplyTo != 0 {
		res["reply_to"] = a.ReplyTo
		res["reply"] = a.ReplyTo
	}
	if a.Fwd != "" {
		res["fwd"] = a.Fwd
	}
	if a.CMID != 0 {
		res["conversation_message_id"] = a.CMID
		res["cmid"] = a.CMID
	}
	if a.Mention {
		res["mention"] = 1
	}
	if a.Muted {
		res["muted"] = 1
	}

	return res
}

func NewLPAttachments(raw string) LPAttachments {
	lpa := LPAttachments{Items: []LPAttachmentItem{}}
	if raw == "" {
		return lpa
	}

	parts := strings.Split(raw, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		typeEnd := strings.IndexAny(part, "0123456789-")
		if typeEnd > 0 {
			lpa.Items = append(lpa.Items, LPAttachmentItem{
				Type: part[:typeEnd],
				Data: part[typeEnd:],
			})
		}
	}
	return lpa
}

// --- Flags ---

type MessageFlag uint64

const (
	FlagUnread       MessageFlag = 1
	FlagOutbox       MessageFlag = 2
	FlagReplied      MessageFlag = 4
	FlagImportant    MessageFlag = 8
	FlagChat         MessageFlag = 16
	FlagFriends      MessageFlag = 32
	FlagSpam         MessageFlag = 64
	FlagDeleteForAll MessageFlag = 64
	FlagDeleted      MessageFlag = 128
	FlagFixed        MessageFlag = 256
	FlagMedia        MessageFlag = 512
	FlagHidden       MessageFlag = 65536
)

type MessageFlags struct {
	Value MessageFlag
}

func (f MessageFlags) Has(flag MessageFlag) bool {
	return (f.Value & flag) != 0
}

func (f *MessageFlags) Add(flag MessageFlag) {
	f.Value |= flag
}

func (f MessageFlags) ToUint64() uint64 {
	return uint64(f.Value)
}

type ChatFlag uint64

const (
	FlagChatImportant  ChatFlag = 1
	FlagChatUnanswered ChatFlag = 2
)

type ChatFlags struct {
	Value ChatFlag
}

func (f ChatFlags) Has(flag ChatFlag) bool {
	return (f.Value & flag) != 0
}

func (f *ChatFlags) Add(flag ChatFlag) {
	f.Value |= flag
}

func (f ChatFlags) ToUint64() uint64 {
	return uint64(f.Value)
}

// --- Events ---
/*
  web archive marks:

  v0 https://web.archive.org/web/20131031232329/http://vk.com/dev/using_longpoll

  ^^^ v0 Требует больших костыльных доработок чтобы работало. Поэтому совместимость не гарантирована.

  v1 https://web.archive.org/web/20170205084502/https://vk.com/dev/using_longpoll

  v2 https://web.archive.org/web/20190405024658/https://vk.com/dev/using_longpoll

  v3 - latest
*/

// v0
type MsgDeleteEvent struct {
	MessageID uint64
}

func (e MsgDeleteEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":       "msg.delete",
			"code":       0,
			"message_id": e.MessageID,
		}
	}
	return []interface{}{0, e.MessageID, 0}
}

// v0
type MsgReplaceFlagsEvent struct {
	MessageID uint64
	Flags     MessageFlags
	PeerID    int64
}

func (e MsgReplaceFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":       "msg.flags.replace",
			"code":       1,
			"message_id": e.MessageID,
			"flags":      e.Flags.Value,
			"peer_id":    e.PeerID,
		}
	}
	return []interface{}{1, e.MessageID, e.Flags.ToUint64(), e.PeerID}
}

// v0
type MsgSetFlagsEvent struct {
	MessageID uint64
	Mask      MessageFlags
	PeerID    int64
}

func (e MsgSetFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":       "msg.flags.set",
			"code":       2,
			"message_id": e.MessageID,
			"mask":       e.Mask.Value,
			"peer_id":    e.PeerID,
		}
	}
	return []interface{}{2, e.MessageID, e.Mask.ToUint64(), e.PeerID}
}

// v0
type MsgResetFlagsEvent struct {
	MessageID uint64
	Mask      MessageFlags
	PeerID    int64
}

func (e MsgResetFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":       "msg.flags.reset",
			"code":       3,
			"message_id": e.MessageID,
			"mask":       e.Mask.Value,
			"peer_id":    e.PeerID,
		}
	}
	return []interface{}{3, e.MessageID, e.Mask.ToUint64(), e.PeerID}
}

// v0
type NewMessageEvent struct {
	MessageID   uint64
	Flags       MessageFlags
	MinorID     int64
	PeerID      int64
	Timestamp   int
	Text        string
	Attachments *LPAttachments
	RandomID    int
}

func (e NewMessageEvent) ToSlice(cfg LPConfig) interface{} {
	peerID := e.PeerID
	switch {
	case cfg.Version == 0 && peerID < 0:
		peerID = 1000000000 + (-peerID)
	case cfg.Version == 1 && peerID > 2000000000:
		peerID = peerID - 2000000000
	}

	extraMap := map[string]interface{}{}
	if e.Attachments != nil {
		extraMap = e.Attachments.ToMap()
	}

	if !cfg.ReturnCustomFields() {
		delete(extraMap, "cmid")
		delete(extraMap, "conversation_message_id")
	}

	if cfg.Described == 2 {
		res := map[string]interface{}{
			"type":       "msg.new",
			"code":       4,
			"message_id": e.MessageID,
			"flags":      e.Flags.Value,
			"peer_id":    peerID,
			"timestamp":  e.Timestamp,
			"text":       e.Text,
		}

		if cfg.Version < 7 {
			res["subject"] = ""
		} else {
			res["conversation_message_id"] = e.MinorID
		}

		if cfg.HasAttachments() {
			res["attachments"] = extraMap
		}

		if cfg.ReturnRandomID() || cfg.Version >= 7 {
			res["random_id"] = e.RandomID
		}

		if cfg.ReturnCustomFields() {
			res["custom"] = map[string]interface{}{
				"minor_id": e.MinorID,
			}
		}

		return res
	}

	if cfg.Version == 0 {
		if cfg.HasExtended() {
			out := 0
			if (e.Flags.Value & 2) != 0 {
				out = 1
			}
			readState := 1
			if (e.Flags.Value & 1) != 0 {
				readState = 0
			}
			msgUID := peerID
			if peerID > 2000000000 && e.Attachments != nil && e.Attachments.From != 0 {
				msgUID = e.Attachments.From
			}
			msgObj := map[string]interface{}{
				"mid":        e.MessageID,
				"uid":        msgUID,
				"out":        out,
				"read_state": readState,
				"date":       e.Timestamp,
				"title":      " ... ",
				"body":       e.Text,
			}
			if peerID > 2000000000 {
				msgObj["chat_id"] = peerID - 2000000000
			}
			if e.Attachments != nil && len(e.Attachments.ItemsPayload) > 0 {
				msgObj["attachments"] = e.Attachments.ItemsPayload
			}
			return []interface{}{
				101,
				map[string]interface{}{
					"message":  msgObj,
					"profiles": []interface{}{},
				},
			}
		}

		res := []interface{}{
			4,
			e.MessageID,
			e.Flags.Value,
			peerID,
			e.Timestamp,
			" ... ",
			e.Text,
		}
		if cfg.HasAttachments() && len(extraMap) > 0 {
			// Format params as TSV: \t<char><key> <value>
			var paramsBuf strings.Builder
			for k, v := range extraMap {
				paramsBuf.WriteString(fmt.Sprintf("\t %s %v", k, v))
			}
			res = append(res, paramsBuf.String())
		}
		if cfg.ReturnRandomID() {
			res = append(res, e.RandomID)
		}
		if cfg.ReturnCustomFields() {
			res = append(res, map[string]interface{}{"minor_id": e.MinorID})
		}
		return res
	}

	if cfg.Version < 4 {
		res := []interface{}{
			4,
			e.MessageID,
			e.Flags.Value,
			peerID,
			e.Timestamp,
			"",
			e.Text,
		}
		if cfg.HasAttachments() {
			res = append(res, extraMap)
		}
		if cfg.ReturnRandomID() {
			res = append(res, e.RandomID)
		}
		if cfg.ReturnCustomFields() {
			res = append(res, map[string]interface{}{"minor_id": e.MinorID})
		}
		return res
	}

	titleObj := map[string]interface{}{
		"title": " ... ",
		"from":  extraMap["from"],
	}

	res := []interface{}{
		4,
		e.MessageID,
		e.Flags.Value,
		peerID,
		e.Timestamp,
		e.Text,
		titleObj,
		extraMap,
		e.RandomID,
		e.MinorID,
	}

	if cfg.ReturnCustomFields() {
		res = append(res, map[string]interface{}{
			"minor_id": e.MinorID,
		})
	}

	return res
}

// v2
type UpdateMessageEvent struct {
	MessageID   uint64
	Mask        MessageFlags // Работает как set.
	PeerID      int64
	Timestamp   uint64
	NewText     string
	Attachments *LPAttachments
	Stub        uint8
}

func (e UpdateMessageEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		res := map[string]interface{}{
			"type":       "msg.update",
			"code":       5,
			"message_id": e.MessageID,
			"mask":       e.Mask.Value,
			"peer_id":    e.PeerID,
			"timestamp":  e.Timestamp,
			"text":       e.NewText,
		}

		if cfg.HasAttachments() {
			if e.Attachments == nil {
				res["attachments"] = map[string]interface{}{}
			} else {
				res["attachments"] = e.Attachments.ToMap()
			}
		}

		return res
	}

	res := []interface{}{
		5,
		e.MessageID,
		e.Mask.ToUint64(),
		e.PeerID,
		e.Timestamp,
		e.NewText,
	}

	if cfg.HasAttachments() {
		if e.Attachments == nil {
			res = append(res, map[string]interface{}{})
		} else {
			res = append(res, e.Attachments.ToMap())
		}
	}

	res = append(res, e.Stub)

	return res
}

// v1
type ReadIncomeBeforeEvent struct {
	PeerID  int64
	LocalID uint64
	Count   int
}

func (e ReadIncomeBeforeEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "read.incoming",
			"code":     6,
			"peer_id":  e.PeerID,
			"local_id": e.LocalID,
			"count":    e.Count,
		}
	}
	if cfg.Version >= 3 {
		return []interface{}{6, e.PeerID, e.LocalID, e.Count}
	}
	return []interface{}{6, e.PeerID, e.LocalID}
}

// v1
type ReadOutcomeBeforeEvent struct {
	PeerID  int64
	LocalID uint64
	Count   int
}

func (e ReadOutcomeBeforeEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "read.outgoing",
			"code":     7,
			"peer_id":  e.PeerID,
			"local_id": e.LocalID,
			"count":    e.Count,
		}
	}
	if cfg.Version >= 3 {
		return []interface{}{7, e.PeerID, e.LocalID, e.Count}
	}
	return []interface{}{7, e.PeerID, e.LocalID}
}

// v0
type GotOnlineEvent struct {
	UserID    int64  `json:"user_id"`
	Extra     int64  `json:"extra"`     // != 0; last byte =
	Timestamp uint64 `json:"timestamp"` // Last action at
}

func (e GotOnlineEvent) ToSlice(cfg LPConfig) interface{} {
	uid := e.UserID
	if uid > 0 {
		uid = -uid
	}
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":      "user.online",
			"code":      8,
			"user_id":   e.UserID,
			"extra":     e.Extra,
			"timestamp": e.Timestamp,
		}
	}
	return []interface{}{8, uid, e.Extra, e.Timestamp}
}

// v0
type GotOfflineEvent struct {
	UserID    int64  `json:"user_id"`
	Flags     int64  `json:"flags"`     // 0 - Left site; 1 - Timeout
	Timestamp uint64 `json:"timestamp"` // Last action at
}

func (e GotOfflineEvent) ToSlice(cfg LPConfig) interface{} {
	uid := e.UserID
	if uid > 0 {
		uid = -uid
	}
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":      "user.offline",
			"code":      9,
			"user_id":   e.UserID,
			"flags":     e.Flags,
			"timestamp": e.Timestamp,
		}
	}
	return []interface{}{9, uid, e.Flags, e.Timestamp}
}

// v1
type ChatResetFlagsEvent struct {
	PeerID int64
	Mask   uint64
}

func (e ChatResetFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "chat.flags.reset",
			"code":    10,
			"peer_id": e.PeerID,
			"mask":    e.Mask,
		}
	}
	return []interface{}{10, e.PeerID, e.Mask}
}

// v1
type ChatReplaceFlagsEvent struct {
	PeerID int64
	Flags  uint64
}

func (e ChatReplaceFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "chat.flags.replace",
			"code":    11,
			"peer_id": e.PeerID,
			"flags":   e.Flags,
		}
	}
	return []interface{}{11, e.PeerID, e.Flags}
}

// v1
type ChatSetFlagsEvent struct {
	PeerID int64
	Mask   uint64
}

func (e ChatSetFlagsEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "chat.flags.set",
			"code":    12,
			"peer_id": e.PeerID,
			"mask":    e.Mask,
		}
	}
	return []interface{}{12, e.PeerID, e.Mask}
}

// v2
type MassDeleteMessagesEvent struct {
	PeerID  int64
	LocalID uint64
}

func (e MassDeleteMessagesEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "msg.delete.mass",
			"code":     13,
			"peer_id":  e.PeerID,
			"local_id": e.LocalID,
		}
	}
	return []interface{}{13, e.PeerID, e.LocalID}
}

// v3
type MassRestoreMessagesEvent struct {
	PeerID  int64
	LocalID uint64
}

func (e MassRestoreMessagesEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "msg.restore.mass",
			"code":     14,
			"peer_id":  e.PeerID,
			"local_id": e.LocalID,
		}
	}
	return []interface{}{14, e.PeerID, e.LocalID}
}

// Indicates a global change in dialogue history. Local cache should be re-synced via messages.getHistory.
// v3
type StateSyncEvent struct {
	PeerID  int64
	MajorID uint64
}

func (e StateSyncEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "sync.state",
			"code":     20,
			"peer_id":  e.PeerID,
			"major_id": e.MajorID,
		}
	}
	return []interface{}{20, e.PeerID, e.MajorID}
}

// Indicates minor dialogue metadata changes. Does not usually require full history re-sync.
// v3
type MetaDataSyncEvent struct {
	PeerID  int64
	MinorID uint64
}

func (e MetaDataSyncEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":     "sync.metadata",
			"code":     21,
			"peer_id":  e.PeerID,
			"minor_id": e.MinorID,
		}
	}
	return []interface{}{21, e.PeerID, e.MinorID}
}

// v0
type ChatSomethingChangedEvent struct {
	ChatID int64
	Self   uint8 // Is triggered by same user 1 or 0
}

func (e ChatSomethingChangedEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "chat.update.unknown",
			"code":    51,
			"chat_id": e.ChatID,
			"self":    e.Self,
		}
	}
	return []interface{}{51, e.ChatID, e.Self}
}

// v3
type ChatUpdateEvent struct {
	TypeID int64
	PeerID int64
}

func (e ChatUpdateEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "chat.update",
			"code":    52,
			"type_id": e.TypeID,
			"peer_id": e.PeerID,
		}
	}
	return []interface{}{52, e.TypeID, e.PeerID}
}

// v0
type IsDMTypingEvent struct {
	UserID int64
	Flags  uint8 // 0 - Stopped, 1 - Typing, 2 - Audiomessage
}

func (e IsDMTypingEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		flag_info := "stopped"
		switch e.Flags {
		case 1:
			flag_info = "typing"
		case 2:
			flag_info = "audiomessage"
		}
		return map[string]interface{}{
			"type":      "dm.activity",
			"code":      61,
			"user_id":   e.UserID,
			"flag":      e.Flags,
			"flag_info": flag_info,
		}
	}

	if cfg.Version >= 3 {
		code := 63
		if e.Flags == 2 {
			code = 64
		}

		userIDs := []int64{e.UserID}
		totalCount := 1
		if e.Flags == 0 {
			userIDs = []int64{}
			totalCount = 0
		}

		return []interface{}{
			code,
			e.UserID,
			userIDs,
			totalCount,
			time.Now().Unix(),
		}
	}

	return []interface{}{61, e.UserID, e.Flags}
}

// v0
type IsChatTypingEvent struct {
	UserID int64
	ChatID int64
	Flags  uint8 // 0 - Stopped, 1 - Typing, 2 - Audiomessage
}

func (e IsChatTypingEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		flag_info := "stopped"
		switch e.Flags {
		case 1:
			flag_info = "typing"
		case 2:
			flag_info = "audiomessage"
		}
		return map[string]interface{}{
			"type":      "chat.typing",
			"code":      62,
			"user_id":   e.UserID,
			"chat_id":   e.ChatID,
			"flag":      e.Flags,
			"flag_info": flag_info,
		}
	}

	if cfg.Version >= 3 {
		code := 63
		if e.Flags == 2 {
			code = 64
		}

		peerID := int64(2000000000) + e.ChatID
		userIDs := []int64{e.UserID}
		totalCount := 1
		if e.Flags == 0 {
			userIDs = []int64{}
			totalCount = 0
		}

		return []interface{}{
			code,
			peerID,
			userIDs,
			totalCount,
			time.Now().Unix(),
		}
	}

	return []interface{}{62, e.UserID, e.ChatID}
}

// v3
type MultiUsersTypingEvent struct {
	UserIDs    []int64
	PeerID     int64
	TotalCount int
	Timestamp  int64
}

func (e MultiUsersTypingEvent) ToSlice(cfg LPConfig) interface{} {
	userIDs := e.UserIDs
	if userIDs == nil {
		userIDs = []int64{}
	}

	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":        "typing.multi",
			"code":        63,
			"user_ids":    userIDs,
			"peer_id":     e.PeerID,
			"total_count": e.TotalCount,
			"timestamp":   e.Timestamp,
		}
	}

	return []interface{}{63, userIDs, e.PeerID, e.TotalCount, e.Timestamp}
}

// v3
type MultiUsersAudioRecordingEvent struct {
	UserIDs    []int64
	PeerID     int64
	TotalCount int
	Timestamp  int64
}

func (e MultiUsersAudioRecordingEvent) ToSlice(cfg LPConfig) interface{} {
	userIDs := e.UserIDs
	if userIDs == nil {
		userIDs = []int64{}
	}

	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":        "audio_recording.multi",
			"code":        64,
			"user_ids":    userIDs,
			"peer_id":     e.PeerID,
			"total_count": e.TotalCount,
			"timestamp":   e.Timestamp,
		}
	}

	return []interface{}{64, userIDs, e.PeerID, e.TotalCount, e.Timestamp}
}

// ЗВОНКИ В ОПЕНВК?!
// v0
type MakingACallEvent struct {
	UserID int64
	CallID uint64
}

func (e MakingACallEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":    "call.start",
			"code":    70,
			"user_id": e.UserID,
			"call_id": e.CallID,
		}
	}
	return []interface{}{70, e.UserID, e.CallID}
}

// v1
type CounterUpdateEvent struct {
	Count uint
}

func (e CounterUpdateEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":  "counter.update",
			"code":  80,
			"count": e.Count,
		}
	}
	return []interface{}{80, e.Count}
}

// v1
type NotificationSetEvent struct {
	PeerID        int64
	Sound         uint8 // 0 - Notif off; 1 - Notif on;
	DisabledUntil int64 // Timestamp until; -1 - forever
}

func (e NotificationSetEvent) ToSlice(cfg LPConfig) interface{} {
	if cfg.Described == 2 {
		return map[string]interface{}{
			"type":           "notification.set",
			"code":           114,
			"peer_id":        e.PeerID,
			"sound":          e.Sound,
			"disabled_until": e.DisabledUntil,
		}
	}
	return []interface{}{114, e.PeerID, e.Sound, e.DisabledUntil}
}
