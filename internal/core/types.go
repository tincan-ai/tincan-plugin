package core

import (
	"crypto/rand"

	"encoding/hex"
	"encoding/json"

	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"time"
)

type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Next    map[string]any `json:"next,omitempty"`
}

func (e *Error) Error() string { return e.Message }
func Fail(status int, code, msg string) error {
	return &Error{Status: status, Code: code, Message: msg}
}
func ID(prefix string) string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b)
}

type Agent struct {
	EncryptionMode string     `json:"encryption_mode"`
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	Name           string     `json:"name"`
	Profile        string     `json:"profile"`
	Admin          bool       `json:"admin"`
	A2A            bool       `json:"a2a_enabled"`
	ConnectedAt    *time.Time `json:"connected_at"`
	BrowserOnly    bool       `json:"browser_only"`
}
type Room struct {
	Archived bool   `json:"archived"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
}
type Channel struct {
	Archived    bool   `json:"archived"`
	ID          string `json:"id"`
	RoomID      string `json:"room_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Private     bool   `json:"private"`
}
type Attachment struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes"`
}
type MessageContext struct {
	RoomID      string `json:"room_id"`
	RoomName    string `json:"room_name"`
	ChannelName string `json:"channel_name"`
	Private     bool   `json:"private"`
}
type ReplyPreview struct {
	ID        string `json:"id"`
	AgentName string `json:"agent_name"`
	Text      string `json:"text"`
}
type Message struct {
	EncryptionError string          `json:"encryption_error,omitempty"`
	Encrypted       *e2ee.Envelope  `json:"encrypted,omitempty"`
	Context         *MessageContext `json:"context,omitempty"`
	ReplyPreview    *ReplyPreview   `json:"reply_preview,omitempty"`
	Seq             int64           `json:"seq"`
	ID              string          `json:"id"`
	ChannelID       string          `json:"channel_id"`
	AgentID         string          `json:"agent_id"`
	AgentName       string          `json:"agent_name"`
	Text            string          `json:"text"`
	Metadata        json.RawMessage `json:"metadata"`
	Attachments     []Attachment    `json:"attachments"`
	Mentions        []string        `json:"mentions"`
	ReplyTo         *string         `json:"reply_to"`
	CreatedAt       time.Time       `json:"created_at"`
}
type SendInput struct {
	Encrypted      *e2ee.Envelope  `json:"encrypted,omitempty"`
	ChannelID      string          `json:"channel_id"`
	Text           string          `json:"text"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	AttachmentIDs  []string        `json:"attachment_ids,omitempty"`
	Mentions       []string        `json:"mentions,omitempty"`
	ReplyTo        *string         `json:"reply_to,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
}
