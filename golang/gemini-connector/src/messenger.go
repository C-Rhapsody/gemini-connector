package main

// InternalMessage is the platform-agnostic message structure.
type InternalMessage struct {
	Platform string
	UserID   string
	ChatID   string
	Content  string
}

// Messenger defines the common interface for all messaging platform adapters.
type Messenger interface {
	Init() error
	Listen() (<-chan InternalMessage, error)
	Send(chatID string, text string) error
	StartTyping(chatID string) (stop func())
	GetFile(fileID string) (string, error)
}
