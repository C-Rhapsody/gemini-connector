package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
		newUUID, err := interactiveSessionSelect(reader)
		if err != nil {
			fmt.Printf("⚠️ Session selection error: %v\n", err)
			fmt.Print("Enter Gemini Session UUID manually (Required for AI, press Enter to skip): ")
			u, _ := reader.ReadString('\n')
			uuid = strings.TrimSpace(u)
			if uuid != "" {
				updated = true
			}
		} else if newUUID != "" {
			uuid = newUUID
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

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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
	logDir := filepath.Dir(exePathForLog)
	srcDir := filepath.Join(logDir, "..", "src")

	logPath := filepath.Join(logDir, "bot.log")
	logFile, logErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if logErr == nil {
		defer logFile.Close()
		log.SetOutput(logFile)

		// 5분 주기 로그 플러시 (비정상 종료 시 유실 최소화)
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				logFile.Sync()
			}
		}()
	} else {
		log.SetOutput(os.Stderr)
	}

	// 시그널 핸들링: 정상 종료 시 로그 플러시 보장
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %v. Shutting down...", sig)
		if logFile != nil {
			logFile.Sync()
			logFile.Close()
		}
		listener.Close()
		os.Exit(0)
	}()

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("Starting Gemini Connector...")

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

	// Initialize messenger adapter
	var adapter Messenger = NewTelegramAdapter(cfg.TelegramBotToken, cfg.TelegramChatID, msgs)
	if err := adapter.Init(); err != nil {
		log.Fatalf("Adapter Init Error: %v", err)
	}

	if cfg.TelegramChatID != 0 {
		adapter.Send(strconv.FormatInt(cfg.TelegramChatID, 10), msgs.StartupWelcome)
		log.Println("Startup message sent.")
	}

	msgChan, err := adapter.Listen()
	if err != nil {
		log.Fatalf("Listen Error: %v", err)
	}

	log.Println("Waiting for messages...")

	for msg := range msgChan {
		go func(m InternalMessage) {
			if cfg.GeminiSessionUUID == "" {
				adapter.Send(m.ChatID, msgs.ErrorMissingUUID)
				return
			}

			stop := adapter.StartTyping(m.ChatID)
			defer stop()

			response, err := executeGemini(m.Content, cfg.GeminiSessionUUID)
			if err != nil {
				if ge, ok := err.(*GeminiError); ok {
					switch ge.Type {
					case "cli_failure":
						adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorCLIFailure, ge.Err, ge.Detail))
					case "no_valid_json":
						adapter.Send(m.ChatID, msgs.ErrorNoValidJSON)
					case "json_parse_fail":
						adapter.Send(m.ChatID, msgs.ErrorJSONParseFail)
					case "system_error":
						adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, ge.Detail))
					}
				}
				return
			}

			if response != "" {
				adapter.Send(m.ChatID, response)
			} else {
				adapter.Send(m.ChatID, msgs.ErrorEmptyResponse)
			}
		}(msg)
	}
}
