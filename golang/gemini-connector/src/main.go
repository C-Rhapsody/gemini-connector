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
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

// --- Configuration & Messages ---

type Config struct {
	ActiveMessengers  []string
	TelegramBotToken  string
	TelegramChatID    int64
	TeamsTenantID     string
	TeamsAppID        string
	TeamsAppSecret    string
	TeamsChatID       string
	AgyConversationID string

	mu             sync.Mutex
	envPath        string
	lastErrDetail  string
	consecErrCount int
	resetting      bool
}

func (c *Config) ConversationID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AgyConversationID
}

// recordUrlFetchError tracks consecutive identical URL fetch failures.
// It returns true when the same failure repeats twice and a reset is not
// already in progress.
func (c *Config) recordUrlFetchError(detail string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if detail != "" && detail == c.lastErrDetail {
		c.consecErrCount++
	} else {
		c.lastErrDetail = detail
		c.consecErrCount = 1
	}
	if c.consecErrCount >= 2 && !c.resetting {
		c.resetting = true
		return true
	}
	return false
}

func (c *Config) applyNewConversation(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AgyConversationID = id
	c.lastErrDetail = ""
	c.consecErrCount = 0
	c.resetting = false
}

type Messages struct {
	StartupWelcome         string `json:"StartupWelcome"`
	CommandStartHelp       string `json:"CommandStartHelp"`
	CommandUnknown         string `json:"CommandUnknown"`
	ErrorMediaNotSupported string `json:"ErrorMediaNotSupported"`
	ErrorMediaDownloadFail string `json:"ErrorMediaDownloadFail"`
	ErrorMissingUUID       string `json:"ErrorMissingUUID"`
	ErrorCLIFailure        string `json:"ErrorCLIFailure"`
	ErrorJSONParseFail     string `json:"ErrorJSONParseFail"`
	ErrorSystemResponse    string `json:"ErrorSystemResponse"`
	ErrorEmptyResponse     string `json:"ErrorEmptyResponse"`
	DefaultMediaPrompt     string `json:"DefaultMediaPrompt"`
}

var defaultMessages = Messages{
	StartupWelcome:         "🔔 agy 텔레그램 커넥터 가동 완료. 메시지를 보내면 agy가 처리합니다.",
	CommandStartHelp:       "agy 텔레그램 커넥터 가동 중. 메시지를 보내면 agy가 처리합니다.\n\n사용 가능 명령어:\n/help - 도움말 및 명령어 목록\n/new (또는 /reset) - 이전 대화를 요약해 새 agy 대화 세션으로 전환\n/status - 현재 대화 ID와 기록된 턴 수 표시\n/summary - 최근 대화 내용 미리보기\n/list - 캐시된 agy 대화 목록\n/switch <ID> - 지정한 대화로 전환\n/version - 커넥터 및 agy 버전",
	CommandUnknown:         "알 수 없는 명령어입니다. /help 를 입력하면 사용 가능한 명령어를 확인할 수 있습니다.",
	ErrorMediaNotSupported: "⚠️ 현재 시스템은 동영상, 음성 및 일반 문서 파일 분석을 지원하지 않습니다. 텍스트 및 이미지 파일만 전송해 주십시오.",
	ErrorMediaDownloadFail: "미디어 다운로드에 실패했습니다.",
	ErrorMissingUUID:       "❌ 봇 설정 오류: .env 파일에 AGY_CONVERSATION_ID가 설정되지 않았습니다.",
	ErrorCLIFailure:        "❌ 시스템 실행 오류 발생.\n\nError: %v\n\nLog: ...%s",
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

	// Active messengers (default: telegram only)
	activeStr := os.Getenv("ACTIVE_MESSENGERS")
	var activeMessengers []string
	if activeStr == "" {
		activeMessengers = []string{"telegram"}
	} else {
		for _, m := range strings.Split(activeStr, ",") {
			activeMessengers = append(activeMessengers, strings.TrimSpace(m))
		}
	}

	// Telegram
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatIDStr := os.Getenv("TELEGRAM_CHAT_ID")
	var chatID int64
	if chatIDStr != "" {
		parsedID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err == nil {
			chatID = parsedID
		}
	}

	// Teams
	teamsTenantID := os.Getenv("TEAMS_TENANT_ID")
	teamsAppID := os.Getenv("TEAMS_APP_ID")
	teamsAppSecret := os.Getenv("TEAMS_APP_SECRET")
	teamsChatID := os.Getenv("TEAMS_CHAT_ID")

	// Antigravity (agy) conversation
	convID := strings.TrimSpace(os.Getenv("AGY_CONVERSATION_ID"))
	if convID == "" {
		log.Println("Warning: AGY_CONVERSATION_ID is not set. Bot will not be able to trigger AI.")
	}

	return &Config{
		ActiveMessengers:  activeMessengers,
		TelegramBotToken:  token,
		TelegramChatID:    chatID,
		TeamsTenantID:     teamsTenantID,
		TeamsAppID:        teamsAppID,
		TeamsAppSecret:    teamsAppSecret,
		TeamsChatID:       teamsChatID,
		AgyConversationID: convID,
		envPath:           envPath,
	}, nil
}

func ensureEnvVars(envPath string) error {
	// Collect all env vars (existing values as defaults)
	vars := map[string]string{
		"ACTIVE_MESSENGERS":   os.Getenv("ACTIVE_MESSENGERS"),
		"TELEGRAM_BOT_TOKEN":  os.Getenv("TELEGRAM_BOT_TOKEN"),
		"TELEGRAM_CHAT_ID":    os.Getenv("TELEGRAM_CHAT_ID"),
		"TEAMS_TENANT_ID":     os.Getenv("TEAMS_TENANT_ID"),
		"TEAMS_APP_ID":        os.Getenv("TEAMS_APP_ID"),
		"TEAMS_APP_SECRET":    os.Getenv("TEAMS_APP_SECRET"),
		"TEAMS_CHAT_ID":       os.Getenv("TEAMS_CHAT_ID"),
		"AGY_CONVERSATION_ID": os.Getenv("AGY_CONVERSATION_ID"),
	}

	updated := false
	reader := bufio.NewReader(os.Stdin)
	headerShown := false

	showHeader := func() {
		if !headerShown {
			fmt.Println("\n=== Gemini Connector Setup ===")
			headerShown = true
		}
	}

	promptRequired := func(key, label string) error {
		showHeader()
		fmt.Printf("Enter %s (Required): ", label)
		v, _ := reader.ReadString('\n')
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("%s cannot be empty", label)
		}
		vars[key] = v
		updated = true
		return nil
	}

	promptOptional := func(key, label string) {
		showHeader()
		fmt.Printf("Enter %s (Optional, press Enter to skip): ", label)
		v, _ := reader.ReadString('\n')
		v = strings.TrimSpace(v)
		if v != "" {
			vars[key] = v
			updated = true
		}
	}

	// 1. Active messengers — infer default from existing env vars if not set
	if vars["ACTIVE_MESSENGERS"] == "" {
		// Detect which platforms already have tokens configured
		var detected []string
		if vars["TELEGRAM_BOT_TOKEN"] != "" {
			detected = append(detected, "telegram")
		}
		if vars["TEAMS_APP_ID"] != "" && vars["TEAMS_TENANT_ID"] != "" {
			detected = append(detected, "teams")
		}

		if len(detected) > 0 {
			// Existing config found — auto-detect without prompting
			vars["ACTIVE_MESSENGERS"] = strings.Join(detected, ",")
			updated = true
		} else {
			// No config at all — ask user
			showHeader()
			fmt.Print("Enter active messengers (comma-separated, e.g. telegram,teams) [default: telegram]: ")
			v, _ := reader.ReadString('\n')
			v = strings.TrimSpace(v)
			if v == "" {
				v = "telegram"
			}
			vars["ACTIVE_MESSENGERS"] = v
			updated = true
		}
	}

	activeList := strings.Split(vars["ACTIVE_MESSENGERS"], ",")
	activeSet := make(map[string]bool)
	for _, m := range activeList {
		activeSet[strings.TrimSpace(m)] = true
	}

	// 2. Telegram setup
	if activeSet["telegram"] {
		if vars["TELEGRAM_BOT_TOKEN"] == "" {
			if err := promptRequired("TELEGRAM_BOT_TOKEN", "Telegram Bot Token"); err != nil {
				return err
			}
		}
		if vars["TELEGRAM_CHAT_ID"] == "" {
			promptOptional("TELEGRAM_CHAT_ID", "Telegram Chat ID")
		}
	}

	// 3. Teams setup
	if activeSet["teams"] {
		fmt.Println("\n--- Teams Configuration ---")
		if vars["TEAMS_TENANT_ID"] == "" {
			if err := promptRequired("TEAMS_TENANT_ID", "Teams Tenant ID"); err != nil {
				return err
			}
		}
		if vars["TEAMS_APP_ID"] == "" {
			if err := promptRequired("TEAMS_APP_ID", "Teams App ID"); err != nil {
				return err
			}
		}
		if vars["TEAMS_APP_SECRET"] == "" {
			if err := promptRequired("TEAMS_APP_SECRET", "Teams App Secret"); err != nil {
				return err
			}
		}
		if vars["TEAMS_CHAT_ID"] == "" {
			if err := promptRequired("TEAMS_CHAT_ID", "Teams Chat ID"); err != nil {
				return err
			}
		}
	}

	// 4. Antigravity conversation ID
	if vars["AGY_CONVERSATION_ID"] == "" {
		newConvID, err := interactiveConversationSelect(reader)
		if err != nil {
			fmt.Printf("⚠️ Conversation selection error: %v\n", err)
			promptOptional("AGY_CONVERSATION_ID", "Antigravity Conversation ID")
		} else if newConvID != "" {
			vars["AGY_CONVERSATION_ID"] = newConvID
			updated = true
		}
	}

	// Write .env — only include sections for active messengers
	if updated {
		var envLines []string
		envLines = append(envLines, "# Global")
		envLines = append(envLines, fmt.Sprintf("ACTIVE_MESSENGERS=%s", vars["ACTIVE_MESSENGERS"]))

		if activeSet["telegram"] {
			envLines = append(envLines, "")
			envLines = append(envLines, "# Telegram")
			envLines = append(envLines, fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s", vars["TELEGRAM_BOT_TOKEN"]))
			envLines = append(envLines, fmt.Sprintf("TELEGRAM_CHAT_ID=%s", vars["TELEGRAM_CHAT_ID"]))
		}

		if activeSet["teams"] {
			envLines = append(envLines, "")
			envLines = append(envLines, "# Teams")
			envLines = append(envLines, fmt.Sprintf("TEAMS_TENANT_ID=%s", vars["TEAMS_TENANT_ID"]))
			envLines = append(envLines, fmt.Sprintf("TEAMS_APP_ID=%s", vars["TEAMS_APP_ID"]))
			envLines = append(envLines, fmt.Sprintf("TEAMS_APP_SECRET=%s", vars["TEAMS_APP_SECRET"]))
			envLines = append(envLines, fmt.Sprintf("TEAMS_CHAT_ID=%s", vars["TEAMS_CHAT_ID"]))
		}

		envLines = append(envLines, "")
		envLines = append(envLines, "# Antigravity (agy)")
		envLines = append(envLines, fmt.Sprintf("AGY_CONVERSATION_ID=%s", vars["AGY_CONVERSATION_ID"]))
		envLines = append(envLines, "")

		envContent := strings.Join(envLines, "\n")
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

// fanIn merges multiple InternalMessage channels into one.
func fanIn(channels ...<-chan InternalMessage) <-chan InternalMessage {
	merged := make(chan InternalMessage, 100)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(c <-chan InternalMessage) {
			defer wg.Done()
			for msg := range c {
				merged <- msg
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(merged)
	}()
	return merged
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

	if cfg.AgyConversationID == "" {
		log.Println("=========================================================")
		log.Println("WARNING: AGY_CONVERSATION_ID is missing in .env")
		log.Println("The bot will run, but it will NOT be able to trigger AI.")
		log.Println("Run 'agy' interactively once to authenticate, then restart the bot.")
		log.Println("=========================================================")
	} else {
		log.Printf("Target Antigravity Conversation ID: %s", cfg.AgyConversationID)
	}

	// Build adapters based on ACTIVE_MESSENGERS
	adapters := make(map[string]Messenger)
	var listenChannels []<-chan InternalMessage

	for _, name := range cfg.ActiveMessengers {
		switch name {
		case "telegram":
			adapters["telegram"] = NewTelegramAdapter(cfg.TelegramBotToken, cfg.TelegramChatID, msgs, cfg.ConversationID)
		case "teams":
			adapters["teams"] = NewTeamsAdapter(cfg.TeamsTenantID, cfg.TeamsAppID, cfg.TeamsAppSecret, cfg.TeamsChatID, msgs)
		default:
			log.Printf("Unknown messenger: %s (skipped)", name)
		}
	}

	if len(adapters) == 0 {
		log.Fatalf("No active messengers configured.")
	}

	// Init and Listen for all adapters
	for name, adapter := range adapters {
		if err := adapter.Init(); err != nil {
			log.Fatalf("%s adapter init error: %v", name, err)
		}
		ch, err := adapter.Listen()
		if err != nil {
			log.Fatalf("%s adapter listen error: %v", name, err)
		}
		listenChannels = append(listenChannels, ch)
		log.Printf("Adapter [%s] started.", name)
	}

	// Send startup welcome to each adapter's configured chat
	if tg, ok := adapters["telegram"]; ok && cfg.TelegramChatID != 0 {
		tg.Send(strconv.FormatInt(cfg.TelegramChatID, 10), msgs.StartupWelcome)
	}
	if teams, ok := adapters["teams"]; ok {
		teams.Send(cfg.TeamsChatID, msgs.StartupWelcome)
	}

	// Merge all adapter channels
	msgChan := fanIn(listenChannels...)

	log.Println("Waiting for messages...")

	for msg := range msgChan {
		go func(m InternalMessage) {
			adapter, ok := adapters[m.Platform]
			if !ok {
				log.Printf("No adapter for platform: %s", m.Platform)
				return
			}

			replyOpt := SendOptions{ReplyToMessageID: m.MessageID}

			switch m.Command {
			case "/reset":
				resetConversation(cfg, adapter, m.ChatID, m.MessageID, "", msgs)
				return
			case "/status":
				statusConversation(cfg, adapter, m.ChatID, m.MessageID, msgs)
				return
			case "/summary":
				summaryConversation(cfg, adapter, m.ChatID, m.MessageID, msgs)
				return
			case "/version":
				versionInfo(adapter, m.ChatID, m.MessageID, msgs)
				return
			case "/list":
				listConversations(cfg, adapter, m.ChatID, m.Args, m.MessageID, msgs)
				return
			case "/switch":
				switchConversation(cfg, adapter, m.ChatID, m.Args, m.MessageID, msgs)
				return
			}

			if cfg.ConversationID() == "" {
				adapter.Send(m.ChatID, msgs.ErrorMissingUUID, replyOpt)
				return
			}

			stop := adapter.StartTyping(m.ChatID)
			defer stop()

			// Turn start marker: files modified from this point on are
			// considered AI-produced and eligible for attachment delivery.
			turnStart := time.Now()

			response, err := executeAgy(m.Content, cfg.ConversationID())
			if err != nil {
				if ae, ok := err.(*AgyError); ok {
					switch ae.Type {
					case "cli_failure":
						adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorCLIFailure, ae.Err, ae.Detail), replyOpt)
					case "json_parse_fail":
						adapter.Send(m.ChatID, msgs.ErrorJSONParseFail, replyOpt)
					case "error_status":
						detail := ae.Detail
						if extractUrlFetchFailure(detail) != "" && cfg.recordUrlFetchError(detail) {
							log.Printf("Stuck conversation detected (repeated URL fetch failure), resetting session")
							resetConversation(cfg, adapter, m.ChatID, m.MessageID, m.Content, msgs)
							return
						}
						adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, detail), replyOpt)
					case "authentication_required":
						adapter.Send(m.ChatID, "⚠️ agy 인증이 필요합니다. 터미널에서 'agy'를 한 번 실행해 인증을 완료한 뒤 봇을 재시작하세요.", replyOpt)
					}
				}
				return
			}

			if response != "" {
				appendTranscript(cfg.ConversationID(), "user", m.Content)
				appendTranscript(cfg.ConversationID(), "assistant", response)
				adapter.Send(m.ChatID, response, SendOptions{ReplyToMessageID: m.MessageID, AttachAfter: turnStart})
			} else {
				adapter.Send(m.ChatID, msgs.ErrorEmptyResponse, replyOpt)
			}
		}(msg)
	}
}

// resetConversation summarizes the old conversation into a fresh agy
// conversation, persists the new ID to .env, replays the given prompt on the
// new session, and deletes the old session artifacts.
func resetConversation(cfg *Config, adapter Messenger, chatID string, replyTo int, replayPrompt string, msgs *Messages) {
	oldID := cfg.ConversationID()
	log.Printf("Resetting conversation. Old conversation ID: %s", oldID)

	replyOpt := SendOptions{ReplyToMessageID: replyTo}

	summaryPrompt := buildSummaryPrompt(oldID)
	if strings.TrimSpace(summaryPrompt) == "" {
		summaryPrompt = "This connector bridges Telegram to agy. Reply only with 'agy Connector Ready.'"
	}

	newID, _, err := createNewConversationRuntimeWithPrompt(summaryPrompt)
	if err != nil {
		log.Printf("Failed to create new conversation: %v", err)
		cfg.applyNewConversation(oldID)
		adapter.Send(chatID, fmt.Sprintf("⚠️ 새 대화 세션 생성에 실패했습니다: %v", err), replyOpt)
		return
	}

	if err := updateEnvKey(cfg.envPath, "AGY_CONVERSATION_ID", newID); err != nil {
		log.Printf("Failed to update .env with new conversation ID: %v", err)
	}
	cfg.applyNewConversation(newID)
	log.Printf("Conversation reset complete. New conversation ID: %s", newID)

	deleteConversationArtifacts(oldID)
	deleteTranscript(oldID)

	notice := fmt.Sprintf("⚠️ 이전 대화를 요약해 새 세션으로 전환했습니다. (새 세션 ID: %s)", truncateString(newID, 8))

	if replayPrompt != "" {
		response, rerr := executeAgy(replayPrompt, newID)
		if rerr == nil && response != "" {
			appendTranscript(newID, "user", replayPrompt)
			appendTranscript(newID, "assistant", response)
			adapter.Send(chatID, notice+"\n\n"+response, replyOpt)
			return
		}
		log.Printf("Replay on new conversation failed: %v", rerr)
	}

	adapter.Send(chatID, notice, replyOpt)
}

// deleteConversationArtifacts removes the old conversation's on-disk state
// (SQLite files and brain folder) in a best-effort manner.
func deleteConversationArtifacts(convID string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	base := filepath.Join(home, ".gemini", "antigravity-cli")

	for _, suffix := range []string{"", "-shm", "-wal"} {
		p := filepath.Join(base, "conversations", convID+".db"+suffix)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("Failed to delete old conversation file %s: %v", p, err)
		}
	}

	brainDir := filepath.Join(base, "brain", convID)
	if err := os.RemoveAll(brainDir); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to delete old conversation brain dir %s: %v", brainDir, err)
	}
}
