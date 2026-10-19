package store

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Message roles. RoleSystem marks the fixed texts the runner writes for
// failed jobs.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Chat limits enforced by the store.
const (
	MaxConversationTitle    = 100   // runes
	MaxUserMessageBytes     = 16384 // role user
	MaxAnswerMessageBytes   = 65536 // roles assistant and system
	MaxConversationsPerUser = 200
	MaxRecentMessages       = 50
)

// Conversation is a chat thread of one user.
type Conversation struct {
	ID        string
	UserID    string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time // moves with every message and rename
}

// Message is one turn of a conversation. JobID, if set, is the job that
// answers (or was started by) the message.
type Message struct {
	ID             string
	ConversationID string
	UserID         string
	Role           string
	Content        string
	JobID          string
	CreatedAt      time.Time
}

// ConversationFilter narrows ListConversations. The list order is
// updated_at DESC, id DESC.
type ConversationFilter struct {
	Limit int // <= 0 means 50; the maximum is 200
	// Before keeps only the conversations strictly after this key in the
	// list order (UpdatedAt and ID of the last one of the previous page).
	Before *PageKey
}

// MessageFilter narrows ListMessages. The list order is created_at DESC,
// id DESC (newest first).
type MessageFilter struct {
	Limit int // <= 0 means 50; the maximum is 200
	// Before keeps only the messages strictly after this key in the list
	// order (CreatedAt and ID of the last one of the previous page).
	Before *PageKey
}

// ChangeMarks are cheap change indicators of one user's data, used to tell
// whether anything moved since the last look. Times are unix milliseconds;
// a user with nothing has zeros.
type ChangeMarks struct {
	JobsAt     int64 // newest job update
	MessagesAt int64 // newest message or conversation change
	PolicyAt   int64 // policy update
	AuditSeq   int64 // last audit sequence number
}

// ValidateTitle reports whether s is an acceptable conversation title: at
// most MaxConversationTitle runes of valid UTF-8 without control, line
// separator or bidirectional-control characters. An empty title is allowed.
func ValidateTitle(s string) error {
	if !utf8.ValidString(s) {
		return Invalid("the title is not valid UTF-8")
	}
	if utf8.RuneCountInString(s) > MaxConversationTitle {
		return Invalid("the title is longer than %d characters", MaxConversationTitle)
	}
	if strings.IndexFunc(s, isControlRune) >= 0 {
		return Invalid("the title contains a control character")
	}
	return nil
}

// isControlRune reports C0 and C1 controls, line and paragraph separators
// and bidirectional controls (the same set as tools.isControl).
func isControlRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// ValidateMessage checks role and content of a message.
func ValidateMessage(role, content string) error {
	var max int
	switch role {
	case RoleUser:
		max = MaxUserMessageBytes
	case RoleAssistant, RoleSystem:
		max = MaxAnswerMessageBytes
	default:
		return Invalid("unknown message role")
	}
	switch {
	case content == "":
		return Invalid("a message needs content")
	case len(content) > max:
		return Invalid("the message is longer than %d bytes", max)
	case !utf8.ValidString(content):
		return Invalid("the message is not valid UTF-8")
	}
	return nil
}
