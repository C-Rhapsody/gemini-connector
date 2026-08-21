package main

import "time"

// InternalMessage is the platform-agnostic message structure.
type InternalMessage struct {
	Platform string
	UserID   string
	ChatID   string
	Content  string
	Command  string
	Args     string
	// MessageID is the platform message identifier, used to reply to the
	// originating message. Zero means "not applicable".
	MessageID int
}

// SendOptions carries optional sending behavior for Messenger.Send. The zero
// value sends text with markdown-to-HTML conversion and no reply-to target.
type SendOptions struct {
	// ReplyToMessageID replies to the given platform message when non-zero.
	ReplyToMessageID int
	// Plain sends the text verbatim, skipping markdown-to-HTML conversion.
	Plain bool
	// AttachAfter enables attachment delivery for AI-produced files modified
	// at or after this moment (the turn start). Zero disables delivery.
	// Only set for AI response payloads.
	AttachAfter time.Time
}

// Messenger defines the common interface for all messaging platform adapters.
type Messenger interface {
	Init() error
	Listen() (<-chan InternalMessage, error)
	Send(chatID string, text string, opts ...SendOptions) error
	StartTyping(chatID string) (stop func())
	GetFile(fileID string) (string, error)
}
