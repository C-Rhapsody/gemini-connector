package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

// --- Configuration & Messages ---

type Config struct {
	TelegramBotToken  string
	TelegramChatID    int64
	GeminiSessionUUID string
}

type Messages struct {
	StartupWelcome         string `json:"StartupWelcome"`
	CommandStartHelp       string `json:"CommandStartHelp"`
	CommandUnknown         string `json:"CommandUnknown"`
	ErrorMediaNotSupported string `json:"ErrorMediaNotSupported"`
	ErrorMediaDownloadFail string `json:"ErrorMediaDownloadFail"`
	ErrorMissingUUID       string `json:"ErrorMissingUUID"`
	ErrorCLIFailure        string `json:"ErrorCLIFailure"`
	ErrorNoValidJSON       string `json:"ErrorNoValidJSON"`
	ErrorJSONParseFail     string `json:"ErrorJSONParseFail"`
	ErrorSystemResponse    string `json:"ErrorSystemResponse"`
	ErrorEmptyResponse     string `json:"ErrorEmptyResponse"`
	DefaultMediaPrompt     string `json:"DefaultMediaPrompt"`
}

var defaultMessages = Messages{
	StartupWelcome:         "🔔 텔레그램 컨트롤러 모드 가동 완료. 명령을 기다립니다.\n\n━━━━━━━━━━━━━\ngemini-connector 가동 완료",
	CommandStartHelp:       "텔레그램 컨트롤러 모드 가동 중. 메시지를 입력하시면 처리합니다.",
	CommandUnknown:         "알 수 없는 명령어입니다.",
	ErrorMediaNotSupported: "⚠️ 현재 시스템은 동영상, 음성 및 일반 문서 파일 분석을 지원하지 않습니다. 텍스트 및 이미지 파일만 전송해 주십시오.",
	ErrorMediaDownloadFail: "미디어 다운로드에 실패했습니다.",
	ErrorMissingUUID:       "❌ 봇 설정 오류: .env 파일에 GEMINI_SESSION_UUID가 설정되지 않았습니다.",
	ErrorCLIFailure:        "❌ 시스템 실행 오류 발생.\n\nError: %v\n\nLog: ...%s",
	ErrorNoValidJSON:       "❌ 시스템 응답에서 유효한 데이터를 찾지 못했습니다.",
	ErrorJSONParseFail:     "❌ 시스템 응답을 해석하는 데 실패했습니다.",
	ErrorSystemResponse:    "⚠️ 시스템 응답 오류: %s",
	ErrorEmptyResponse:     "⚠️ 명령이 빈 응답을 반환했습니다.",
	DefaultMediaPrompt:     "Analyze the attached media file(s) comprehensively. Describe the contents, text, and context in detail. Please provide the final response in Korean.",
}

// Global variables for Album Buffering
var (
	albumBuffer = make(map[string][]*tgbotapi.Message)
	albumTimer  = make(map[string]*time.Timer)
	albumMutex  sync.Mutex
)

func loadMessages(exeDir string) (*Messages, error) {
	msgPath := filepath.Join(exeDir, "messages.json")
	data, err := os.ReadFile(msgPath)
	if err != nil {
		if os.IsNotExist(err) {
			defaultData, _ := json.MarshalIndent(defaultMessages, "", "  ")
			if writeErr := os.WriteFile(msgPath, defaultData, 0644); writeErr != nil {
				log.Printf("Warning: Failed to create messages.json: %v", writeErr)
				return &defaultMessages, nil
			}
			log.Println("Created messages.json with default values.")
			return &defaultMessages, nil
		}
		return &defaultMessages, fmt.Errorf("failed to read messages.json: %v", err)
	}

	var msgs Messages
	if err := json.Unmarshal(data, &msgs); err != nil {
		log.Printf("Warning: Failed to parse messages.json (%v). Using defaults.", err)
		return &defaultMessages, nil
	}
	return &msgs, nil
}

func loadConfig() (*Config, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	srcDir := filepath.Join(exeDir, "..", "src")
	envPath := filepath.Join(srcDir, ".env")

	_ = godotenv.Overload(envPath)

	if err := ensureEnvVars(envPath); err != nil {
		return nil, err
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatIDStr := os.Getenv("TELEGRAM_CHAT_ID")
	var chatID int64
	if chatIDStr != "" {
		parsedID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err == nil {
			chatID = parsedID
		}
	}

	sessionUUID := strings.TrimSpace(os.Getenv("GEMINI_SESSION_UUID"))
	if sessionUUID == "" {
		log.Println("Warning: GEMINI_SESSION_UUID is not set. Bot will not be able to trigger AI.")
	}

	return &Config{
		TelegramBotToken:  token,
		TelegramChatID:    chatID,
		GeminiSessionUUID: sessionUUID,
	}, nil
}

func ensureEnvVars(envPath string) error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	uuid := os.Getenv("GEMINI_SESSION_UUID")

	updated := false
	reader := bufio.NewReader(os.Stdin)

	if token == "" {
		fmt.Println("\n=== Gemini Connector Setup ===")
		fmt.Print("Enter Telegram Bot Token (Required): ")
		t, _ := reader.ReadString('\n')
		token = strings.TrimSpace(t)
		if token == "" {
			return fmt.Errorf("bot token cannot be empty")
		}
		updated = true
	}

	if chatID == "" {
		if !updated {
			fmt.Println("\n=== Gemini Connector Setup ===")
		}
		fmt.Print("Enter Telegram Chat ID (Optional, press Enter to skip): ")
		c, _ := reader.ReadString('\n')
		chatID = strings.TrimSpace(c)
		if chatID != "" {
			updated = true
		}
	}

	if uuid == "" {
		if !updated {
			fmt.Println("\n=== Gemini Connector Setup ===")
		}
		fmt.Print("Enter Gemini Session UUID (Required for AI, press Enter to skip if unsure): ")
		u, _ := reader.ReadString('\n')
		uuid = strings.TrimSpace(u)
		if uuid != "" {
			updated = true
		}
	}

	if updated {
		envContent := fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%s\nGEMINI_SESSION_UUID=%s\n", token, chatID, uuid)
		if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
			return fmt.Errorf("failed to save .env file: %v", err)
		}
		fmt.Println("Configuration updated and saved to .env")
		_ = godotenv.Overload(envPath)
	}

	return nil
}

// --- Gemini CLI Trigger Logic ---

type GeminiResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

func handleIncomingMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, sessionUUID string, msgs *Messages) {
	if msg.IsCommand() {
		switch msg.Command() {
		case "start", "help":
			sendTelegramMsg(bot, msg.Chat.ID, msgs.CommandStartHelp)
		default:
			sendTelegramMsg(bot, msg.Chat.ID, msgs.CommandUnknown)
		}
		return
	}

	// 예외 처리: 동영상, 일반 문서, 음성 파일은 현재 시스템에서 안정적인 분석을 보장하지 않으므로 즉시 차단
	if msg.Video != nil || msg.VideoNote != nil || msg.Document != nil || msg.Audio != nil || msg.Voice != nil {
		sendTelegramMsg(bot, msg.Chat.ID, msgs.ErrorMediaNotSupported)
		return
	}

	// 앨범(MediaGroup) 처리 로직
	if msg.MediaGroupID != "" {
		albumMutex.Lock()
		albumBuffer[msg.MediaGroupID] = append(albumBuffer[msg.MediaGroupID], msg)

		// 기존 타이머가 있으면 멈추고 리셋 (Debounce)
		if timer, exists := albumTimer[msg.MediaGroupID]; exists {
			timer.Stop()
		}

		// 2초 동안 추가 메시지가 없으면 앨범 처리 시작
		albumTimer[msg.MediaGroupID] = time.AfterFunc(2*time.Second, func() {
			processAlbum(bot, msg.MediaGroupID, msg.Chat.ID, sessionUUID, msgs)
		})
		albumMutex.Unlock()
		return
	}

	// 단일 메시지 처리 로직
	processSingleMessage(bot, msg, sessionUUID, msgs)
}

func processSingleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, sessionUUID string, msgs *Messages) {
	prompt := msg.Text
	if msg.Caption != "" {
		prompt = msg.Caption
	}

	if msg.Photo != nil {
		mediaPath := downloadMediaWithRetry(bot, msg, msg.Chat.ID, 1) // 단일 파일은 순번 01
		if mediaPath != "" {
			if prompt == "" {
				prompt = msgs.DefaultMediaPrompt
			}
			// Windows 환경에서 gemini.cmd 실행 시 줄바꿈(\n)이 인수를 끊어먹는 문제를 방지하기 위해 공백으로 연결
			prompt = fmt.Sprintf("[첨부파일: %s] %s", mediaPath, prompt)
		} else {
			sendTelegramMsg(bot, msg.Chat.ID, msgs.ErrorMediaDownloadFail)
			return
		}
	}

	if prompt == "" {
		return
	}

	triggerGemini(bot, msg.Chat.ID, prompt, sessionUUID, msgs)
}

func processAlbum(bot *tgbotapi.BotAPI, groupID string, chatID int64, sessionUUID string, msgs *Messages) {
	albumMutex.Lock()
	messages := albumBuffer[groupID]
	delete(albumBuffer, groupID)
	delete(albumTimer, groupID)
	albumMutex.Unlock()

	// 수신된 순서대로 보장하기 위해 MessageID로 정렬
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].MessageID < messages[j].MessageID
	})

	var combinedPrompt strings.Builder
	captionText := ""

	// 앨범의 최대 10장 이미지를 순차적으로 다운로드
	for i, msg := range messages {
		seqIndex := i + 1
		mediaPath := downloadMediaWithRetry(bot, msg, chatID, seqIndex)
		if mediaPath != "" {
			combinedPrompt.WriteString(fmt.Sprintf("[첨부파일: %s] ", mediaPath))
		} else {
			sendTelegramMsg(bot, chatID, fmt.Sprintf("⚠️ %d번째 미디어 다운로드에 실패했습니다.", seqIndex))
		}

		// 캡션(텍스트)은 보통 그룹 중 하나의 메시지에만 포함됨
		if msg.Caption != "" {
			captionText = msg.Caption
		} else if msg.Text != "" {
			captionText = msg.Text
		}
	}

	if captionText != "" {
		// 혹시 모를 기존 캡션 내의 줄바꿈 문자도 띄어쓰기로 치환하여 안정성 극대화
		safeCaption := strings.ReplaceAll(captionText, "\n", " ")
		combinedPrompt.WriteString(safeCaption)
	} else {
		combinedPrompt.WriteString(msgs.DefaultMediaPrompt)
	}

	triggerGemini(bot, chatID, combinedPrompt.String(), sessionUUID, msgs)
}

func triggerGemini(bot *tgbotapi.BotAPI, chatID int64, prompt string, sessionUUID string, msgs *Messages) {
	if sessionUUID == "" {
		sendTelegramMsg(bot, chatID, msgs.ErrorMissingUUID)
		return
	}

	stopTyping := make(chan struct{})
	defer close(stopTyping)
	go func(cID int64) {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		bot.Request(tgbotapi.NewChatAction(cID, tgbotapi.ChatTyping))
		for {
			select {
			case <-stopTyping:
				return
			case <-ticker.C:
				bot.Request(tgbotapi.NewChatAction(cID, tgbotapi.ChatTyping))
			}
		}
	}(chatID)

	log.Printf("Triggering Gemini CLI for message: %s", truncateString(prompt, 50))

	cmd := exec.Command("gemini", "-y", "-o", "json", "--resume", sessionUUID, "-p", prompt)

	searchDir, errExe := os.Executable()
	if errExe == nil {
		searchDir = filepath.Dir(searchDir)
		for {
			if info, err := os.Stat(filepath.Join(searchDir, ".gemini")); err == nil && info.IsDir() {
				cmd.Dir = searchDir
				break
			}
			parentDir := filepath.Dir(searchDir)
			if parentDir == searchDir {
				break
			}
			searchDir = parentDir
		}
	}

	outputBytes, err := cmd.CombinedOutput()

	if err != nil {
		log.Printf("Gemini CLI execution error: %v\nOutput: %s", err, string(outputBytes))
		errMsg := string(outputBytes)
		if len(errMsg) > 200 {
			errMsg = errMsg[len(errMsg)-200:]
		}
		sendTelegramMsg(bot, chatID, fmt.Sprintf(msgs.ErrorCLIFailure, err, errMsg))
		return
	}

	outputStr := string(outputBytes)
	re := regexp.MustCompile(`(?s){\s*"session_id"|{\s*"response"`)
	loc := re.FindStringIndex(outputStr)

	if loc == nil {
		log.Printf("No valid JSON structure found in output. Raw Output: %s", outputStr)
		sendTelegramMsg(bot, chatID, msgs.ErrorNoValidJSON)
		return
	}
	
	jsonStr := outputStr[loc[0]:]

	var result GeminiResponse
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		log.Printf("Failed to parse JSON response: %v\nCleaned JSON String: %s", err, jsonStr)
		sendTelegramMsg(bot, chatID, msgs.ErrorJSONParseFail)
		return
	}

	if result.Error != "" {
		log.Printf("Gemini CLI returned error in JSON: %s", result.Error)
		sendTelegramMsg(bot, chatID, fmt.Sprintf(msgs.ErrorSystemResponse, result.Error))
		return
	}

	if result.Response != "" {
		sendTelegramMsg(bot, chatID, result.Response)
	} else {
		sendTelegramMsg(bot, chatID, msgs.ErrorEmptyResponse)
	}
}

// --- Helper Functions ---

func sendTelegramMsg(bot *tgbotapi.BotAPI, chatID int64, text string) {
	runes := []rune(text)
	chunkSize := 4000

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunkText := string(runes[i:end])
		msg := tgbotapi.NewMessage(chatID, chunkText)
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send message chunk to %d: %v", chatID, err)
		}
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// downloadMediaWithRetry downloads media with 3 retries and uses the {ChatID}_{Index} naming convention
func downloadMediaWithRetry(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, chatID int64, seqIndex int) string {
	var fileID string
	var ext string

	if msg.Photo != nil && len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		fileID = photo.FileID
		ext = ".jpg"
	} else if msg.Audio != nil {
		fileID = msg.Audio.FileID
		ext = filepath.Ext(msg.Audio.FileName)
		if ext == "" {
			ext = ".mp3"
		}
	} else if msg.Voice != nil {
		fileID = msg.Voice.FileID
		ext = ".ogg"
	}

	if fileID == "" {
		return ""
	}

	// 새로운 네이밍 규칙: {ChatID}_{MessageID}_{Index}.ext (캐시 오염 방지)
	fileName := fmt.Sprintf("%d_%d_%02d%s", chatID, msg.MessageID, seqIndex, ext)

	var fileURL string
	var err error

	// Retry loop for API and Network
	for attempt := 1; attempt <= 3; attempt++ {
		file, apiErr := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if apiErr == nil {
			fileURL = file.Link(bot.Token)
			break
		}
		err = apiErr
		log.Printf("Attempt %d: Error getting file config: %v", attempt, err)
		
		// 429 Too Many Requests 처리
		if tErr, ok := err.(*tgbotapi.Error); ok && tErr.Code == 429 {
			retryAfter := 5 // 기본 대기 시간
			if tErr.ResponseParameters.RetryAfter > 0 {
				retryAfter = tErr.ResponseParameters.RetryAfter
			}
			log.Printf("Rate limited (429) getting file config. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second) // 1s, 2s, 3s backoff
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

	// Retry loop for actual download
	for attempt := 1; attempt <= 3; attempt++ {
		retryAfter, err := downloadFile(fileURL, destPath)
		if err == nil {
			log.Printf("Media downloaded successfully: %s", destPath)
			return destPath
		}
		log.Printf("Attempt %d: Error downloading file: %v", attempt, err)
		
		if retryAfter > 0 {
			log.Printf("Rate limited (429) downloading file. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	return ""
}

// downloadFile returns retryAfter (seconds) and an error
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

// --- Main ---

func main() {
	portPtr := flag.Int("port", 49152, "Port number to use for single instance lock")
	flag.Parse()

	lockAddr := fmt.Sprintf("127.0.0.1:%d", *portPtr)
	listener, err := net.Listen("tcp", lockAddr)
	if err != nil {
		fmt.Printf("Error: gemini-connector is already running (failed to bind to port %s).\n", lockAddr)
		os.Exit(1)
	}
	defer listener.Close()

	exePathForLog, _ := os.Executable()
	logDir := filepath.Dir(exePathForLog) // bin directory
	srcDir := filepath.Join(logDir, "..", "src")
	
	logPath := filepath.Join(logDir, "bot.log")
	logFile, logErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if logErr == nil {
		log.SetOutput(logFile)
	} else {
		log.SetOutput(os.Stderr)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("Starting Gemini Telegram Controller Mode...")

	msgs, err := loadMessages(srcDir)
	if err != nil {
		log.Printf("Failed to load custom messages, using defaults: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Config Error: %v", err)
	}

	if cfg.GeminiSessionUUID == "" {
		log.Println("=========================================================")
		log.Println("WARNING: GEMINI_SESSION_UUID is missing in .env")
		log.Println("The bot will run, but it will NOT be able to trigger AI.")
		log.Println("Please run 'gemini --list-sessions' and add the UUID.")
		log.Println("=========================================================")
	} else {
		log.Printf("Target Gemini Session UUID: %s", cfg.GeminiSessionUUID)
	}

	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		log.Fatalf("Bot Init Error: %v", err)
	}
	log.Printf("Bot Authorized as: %s", bot.Self.UserName)

	if cfg.TelegramChatID != 0 {
		sendTelegramMsg(bot, cfg.TelegramChatID, msgs.StartupWelcome)
		log.Println("Startup message sent to Telegram.")
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	log.Println("Waiting for Telegram messages...")

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if cfg.TelegramChatID != 0 && update.Message.Chat.ID != cfg.TelegramChatID {
			log.Printf("Ignored unauthorized message from Chat ID: %d", update.Message.Chat.ID)
			continue
		}

		go handleIncomingMessage(bot, update.Message, cfg.GeminiSessionUUID, msgs)
	}
}
