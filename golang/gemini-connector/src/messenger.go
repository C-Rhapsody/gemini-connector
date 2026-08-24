package main

import "time"

// InboundKind classifies how the central Controller should route an event.
type InboundKind string

const (
	// EventMessage is free-form user content that becomes an agy turn.
	EventMessage InboundKind = "message"
	// EventCommand is a slash command routed through the command router.
	EventCommand InboundKind = "command"
	// EventInteraction is a UI callback (button press) routed through the
	// interaction router.
	EventInteraction InboundKind = "interaction"
)

// InboundEvent is the platform-agnostic event structure every adapter emits.
// Adapters translate platform payloads into events but never interpret their
// meaning: commands, callbacks and business logic are owned by the central
// Controller.
type InboundEvent struct {
	Platform string
	UserID   string
	ChatID   string
	// Kind selects the routing path. Adapters set it explicitly; the zero
	// value is resolved from legacy fields via RouteKind.
	Kind    InboundKind
	Content string
	Command string
	Args    string
	// MessageID is the platform message identifier, used to reply to the
	// originating message. Zero means "not applicable".
	MessageID int
	// CallbackID carries the platform callback query identifier for button
	// presses. Empty otherwise.
	CallbackID string
	// CallbackData carries the opaque payload attached to the pressed button.
	CallbackData string
}

// RouteKind resolves the routing category, deriving it from the populated
// fields when Kind was not set explicitly.
func (ev InboundEvent) RouteKind() InboundKind {
	if ev.Kind != "" {
		return ev.Kind
	}
	if ev.CallbackData != "" || ev.CallbackID != "" {
		return EventInteraction
	}
	if ev.Command != "" {
		return EventCommand
	}
	return EventMessage
}

// InlineButton is a platform-agnostic inline keyboard button that sends a
// fixed callback payload when pressed.
type InlineButton struct {
	Text string
	Data string
}

// InlineKeyboard is a matrix of buttons rendered under a message. A nil
// keyboard means "remove any existing keyboard" when editing.
type InlineKeyboard [][]InlineButton

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

// Messenger defines the lifecycle and base transport contract every messaging
// platform adapter implements.
type Messenger interface {
	Init() error
	Listen() (<-chan InboundEvent, error)
	Send(chatID string, text string, opts ...SendOptions) error
	StartTyping(chatID string) (stop func())
	GetFile(fileID string) (string, error)
}

// TypingSender is implemented by adapters that can surface a typing state.
type TypingSender interface {
	StartTyping(chatID string) (stop func())
}

// AttachmentSender is implemented by adapters able to upload local files as
// platform attachments (photos, videos, documents).
type AttachmentSender interface {
	SendAttachment(chatID string, path string, replyTo int) error
}

// InteractiveSender is implemented by adapters supporting rich interactions:
// inline keyboards, callback acknowledgment and in-place message edits.
type InteractiveSender interface {
	SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error
	AnswerCallbackQuery(callbackID, text string)
	EditMessage(chatID string, messageID int, text string, kb InlineKeyboard)
}
