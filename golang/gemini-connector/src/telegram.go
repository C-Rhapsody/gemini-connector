package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type TelegramAdapter struct {
	bot         *tgbotapi.BotAPI
	token       string
	chatID      int64
	msgs        *Messages
	albumBuffer map[string][]*tgbotapi.Message
	albumTimer  map[string]*time.Timer
	albumMutex  sync.Mutex
	msgChan     chan InternalMessage
}

func NewTelegramAdapter(token string, chatID int64, msgs *Messages) *TelegramAdapter {
	return &TelegramAdapter{
		token:       token,
		chatID:      chatID,
		msgs:        msgs,
		albumBuffer: make(map[string][]*tgbotapi.Message),
		albumTimer:  make(map[string]*time.Timer),
		msgChan:     make(chan InternalMessage, 100),
	}
}

func (t *TelegramAdapter) Init() error {
	bot, err := tgbotapi.NewBotAPI(t.token)
	if err != nil {
		return fmt.Errorf("bot init error: %v", err)
	}
	t.bot = bot
	log.Printf("Bot Authorized as: %s", bot.Self.UserName)
	return nil
}

func (t *TelegramAdapter) Listen() (<-chan InternalMessage, error) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := t.bot.GetUpdatesChan(u)

	go func() {
		for update := range updates {
			if update.Message == nil {
				continue
			}

			if t.chatID != 0 && update.Message.Chat.ID != t.chatID {
				log.Printf("Ignored unauthorized message from Chat ID: %d", update.Message.Chat.ID)
				continue
			}

			go t.handleIncomingMessage(update.Message)
		}
		close(t.msgChan)
	}()

	return t.msgChan, nil
}

func (t *TelegramAdapter) Send(chatID string, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat ID: %s", chatID)
	}

	runes := []rune(text)
	chunkSize := 4000

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunkText := string(runes[i:end])
		msg := tgbotapi.NewMessage(id, chunkText)
		if _, sendErr := t.bot.Send(msg); sendErr != nil {
			log.Printf("Failed to send message chunk to %d: %v", id, sendErr)
			return sendErr
		}
	}
	return nil
}

func (t *TelegramAdapter) StartTyping(chatID string) (stop func()) {
	id, _ := strconv.ParseInt(chatID, 10, 64)
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		t.bot.Request(tgbotapi.NewChatAction(id, tgbotapi.ChatTyping))
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				t.bot.Request(tgbotapi.NewChatAction(id, tgbotapi.ChatTyping))
			}
		}
	}()

	return func() { close(done) }
}

func (t *TelegramAdapter) GetFile(fileID string) (string, error) {
	file, err := t.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", err
	}

	fileURL := file.Link(t.bot.Token)
	exePath, _ := os.Executable()
	downloadsDir := filepath.Join(filepath.Dir(exePath), "..", "downloads")
	_ = os.MkdirAll(downloadsDir, 0755)
	destPath := filepath.Join(downloadsDir, filepath.Base(file.FilePath))

	_, dlErr := downloadFile(fileURL, destPath)
	if dlErr != nil {
		return "", dlErr
	}
	return destPath, nil
}

// --- Internal message routing ---

func (t *TelegramAdapter) handleIncomingMessage(msg *tgbotapi.Message) {
	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	if msg.IsCommand() {
		switch msg.Command() {
		case "start", "help":
			t.Send(chatID, t.msgs.CommandStartHelp)
		default:
			t.Send(chatID, t.msgs.CommandUnknown)
		}
		return
	}

	if msg.Video != nil || msg.VideoNote != nil || msg.Document != nil || msg.Audio != nil || msg.Voice != nil {
		t.Send(chatID, t.msgs.ErrorMediaNotSupported)
		return
	}

	if msg.MediaGroupID != "" {
		t.albumMutex.Lock()
		t.albumBuffer[msg.MediaGroupID] = append(t.albumBuffer[msg.MediaGroupID], msg)

		if timer, exists := t.albumTimer[msg.MediaGroupID]; exists {
			timer.Stop()
		}

		t.albumTimer[msg.MediaGroupID] = time.AfterFunc(2*time.Second, func() {
			t.processAlbum(msg.MediaGroupID, msg.Chat.ID)
		})
		t.albumMutex.Unlock()
		return
	}

	t.processSingleMessage(msg)
}

func (t *TelegramAdapter) processSingleMessage(msg *tgbotapi.Message) {
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	prompt := msg.Text
	if msg.Caption != "" {
		prompt = msg.Caption
	}

	if msg.Photo != nil {
		mediaPath := t.downloadMediaWithRetry(msg, msg.Chat.ID, 1)
		if mediaPath != "" {
			if prompt == "" {
				prompt = t.msgs.DefaultMediaPrompt
			}
			prompt = fmt.Sprintf("[첨부파일: %s] %s", mediaPath, prompt)
		} else {
			t.Send(chatID, t.msgs.ErrorMediaDownloadFail)
			return
		}
	}

	if prompt == "" {
		return
	}

	userID := ""
	if msg.From != nil {
		userID = strconv.FormatInt(msg.From.ID, 10)
	}

	t.msgChan <- InternalMessage{
		Platform: "telegram",
		UserID:   userID,
		ChatID:   chatID,
		Content:  prompt,
	}
}

func (t *TelegramAdapter) processAlbum(groupID string, chatID int64) {
	t.albumMutex.Lock()
	messages := t.albumBuffer[groupID]
	delete(t.albumBuffer, groupID)
	delete(t.albumTimer, groupID)
	t.albumMutex.Unlock()

	chatIDStr := strconv.FormatInt(chatID, 10)

	sort.Slice(messages, func(i, j int) bool {
		return messages[i].MessageID < messages[j].MessageID
	})

	var combinedPrompt strings.Builder
	captionText := ""

	for i, msg := range messages {
		seqIndex := i + 1
		mediaPath := t.downloadMediaWithRetry(msg, chatID, seqIndex)
		if mediaPath != "" {
			combinedPrompt.WriteString(fmt.Sprintf("[첨부파일: %s] ", mediaPath))
		} else {
			t.Send(chatIDStr, fmt.Sprintf("⚠️ %d번째 미디어 다운로드에 실패했습니다.", seqIndex))
		}

		if msg.Caption != "" {
			captionText = msg.Caption
		} else if msg.Text != "" {
			captionText = msg.Text
		}
	}

	if captionText != "" {
		safeCaption := strings.ReplaceAll(captionText, "\n", " ")
		combinedPrompt.WriteString(safeCaption)
	} else {
		combinedPrompt.WriteString(t.msgs.DefaultMediaPrompt)
	}

	var userID string
	if len(messages) > 0 && messages[0].From != nil {
		userID = strconv.FormatInt(messages[0].From.ID, 10)
	}

	t.msgChan <- InternalMessage{
		Platform: "telegram",
		UserID:   userID,
		ChatID:   chatIDStr,
		Content:  combinedPrompt.String(),
	}
}

// --- Media download ---

func (t *TelegramAdapter) downloadMediaWithRetry(msg *tgbotapi.Message, chatID int64, seqIndex int) string {
	var fileID string
	var ext string

	if msg.Photo != nil && len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		fileID = photo.FileID
		ext = ".jpg"
	}

	if fileID == "" {
		return ""
	}

	fileName := fmt.Sprintf("%d_%d_%02d%s", chatID, msg.MessageID, seqIndex, ext)

	var fileURL string
	var err error

	for attempt := 1; attempt <= 3; attempt++ {
		file, apiErr := t.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if apiErr == nil {
			fileURL = file.Link(t.bot.Token)
			break
		}
		err = apiErr
		log.Printf("Attempt %d: Error getting file config: %v", attempt, err)

		if tErr, ok := err.(*tgbotapi.Error); ok && tErr.Code == 429 {
			retryAfter := 5
			if tErr.ResponseParameters.RetryAfter > 0 {
				retryAfter = tErr.ResponseParameters.RetryAfter
			}
			log.Printf("Rate limited (429) getting file config. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	if fileURL == "" {
		return ""
	}

	log.Printf("Downloading media from: %s", fileURL)

	exePath, _ := os.Executable()
	downloadsDir := filepath.Join(filepath.Dir(exePath), "..", "downloads")
	_ = os.MkdirAll(downloadsDir, 0755)
	destPath := filepath.Join(downloadsDir, fileName)

	for attempt := 1; attempt <= 3; attempt++ {
		retryAfter, dlErr := downloadFile(fileURL, destPath)
		if dlErr == nil {
			log.Printf("Media downloaded successfully: %s", destPath)
			return destPath
		}
		log.Printf("Attempt %d: Error downloading file: %v", attempt, dlErr)

		if retryAfter > 0 {
			log.Printf("Rate limited (429) downloading file. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	return ""
}

func downloadFile(url string, destPath string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfterStr := resp.Header.Get("Retry-After")
		if retryAfter, err := strconv.Atoi(retryAfterStr); err == nil {
			return retryAfter, fmt.Errorf("429 Too Many Requests")
		}
		return 5, fmt.Errorf("429 Too Many Requests (unknown retry-after)")
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return 0, err
}
