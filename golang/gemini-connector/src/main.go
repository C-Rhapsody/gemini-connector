package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	mu                sync.Mutex
	envPath           string
	lastUrlFetch      string
	consecUrlFetch    int
	lastInvalidArgs   string
	consecInvalidArgs int
	lastGenericErr    string
	consecGenericErr  int
	lastGenericReset  time.Time
	resetting         bool
}

func (c *Config) ConversationID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AgyConversationID
}

// genericResetMinInterval limits how often repeated unknown errors may trigger
// an automatic session reset, preventing reset cascades when many messages
// fail for the same environmental reason.
const genericResetMinInterval = 10 * time.Minute

// recordStuckError tracks consecutive identical errors per category and
// reports when the threshold for automatic conversation recovery is reached.
// Categories: "url_fetch" (repeated URL fetch failures), "invalid_args"
// (repeated conversation-state validation failures) and "generic" (any other
// repeating error_status). Generic resets are additionally rate-limited by
// genericResetMinInterval because the failing prompt is not replayed and the
// underlying cause is often environmental rather than session state.
func (c *Config) recordStuckError(category string, detail string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	var last *string
	var count *int
	switch category {
	case "url_fetch":
		last, count = &c.lastUrlFetch, &c.consecUrlFetch
	case "generic":
		last, count = &c.lastGenericErr, &c.consecGenericErr
	default:
		last, count = &c.lastInvalidArgs, &c.consecInvalidArgs
	}
	if detail != "" && detail == *last {
		*count++
	} else {
		*last = detail
		*count = 1
	}
	if *count >= 2 && !c.resetting {
		if category == "generic" {
			if time.Since(c.lastGenericReset) < genericResetMinInterval {
				*count = 1 // rate-limited: start counting over instead of firing
				return false
			}
			c.lastGenericReset = time.Now()
		}
		c.resetting = true
		return true
	}
	return false
}

func (c *Config) applyNewConversation(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AgyConversationID = id
	c.lastUrlFetch = ""
	c.consecUrlFetch = 0
	c.lastInvalidArgs = ""
	c.consecInvalidArgs = 0
	c.lastGenericErr = ""
	c.consecGenericErr = 0
	c.resetting = false
}

// invalidArgsHint is appended to the first invalid-arguments error so the
// user knows the connector will self-heal on a repeat.
const invalidArgsHint = "\n\n(동일 오류가 반복되면 새 대화로 자동 전환됩니다. 즉시 전환하려면 /new)"

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
	StopDone               string `json:"StopDone"`
	StopDoneWithQueued     string `json:"StopDoneWithQueued"`
	StopNothing            string `json:"StopNothing"`
	QueuedNotice           string `json:"QueuedNotice"`
	ImageUsage             string `json:"ImageUsage"`
	ImageKeyMissing        string `json:"ImageKeyMissing"`
	ImageGenerating        string `json:"ImageGenerating"`
	ImageTimeout           string `json:"ImageTimeout"`
	ImageFail              string `json:"ImageFail"`
	ImageTranslateTemplate string `json:"ImageTranslateTemplate"`
	ImageFiltered          string `json:"ImageFiltered"`
}

var defaultMessages = Messages{
	StartupWelcome:         "🔔 agy 텔레그램 커넥터 가동 완료. 메시지를 보내면 agy가 처리합니다.",
	CommandStartHelp:       "agy 텔레그램 커넥터 가동 중. 메시지를 보내면 agy가 처리합니다.\n\n사용 가능 명령어:\n/help - 도움말 및 명령어 목록\n/image <묘사> - 묘사를 영어 프롬프트로 번역해 NVIDIA NIM으로 이미지 생성\n/new (또는 /reset) - 이전 대화를 요약해 새 agy 대화 세션으로 전환\n/clear - 대화 기록을 모두 지우고 완전히 새 세션 시작 (요약 이월 없음)\n/stop - 진행 중인 agy 작업과 대기열을 즉시 중지\n/status - 현재 대화 ID와 기록된 턴 수 표시\n/summary - 최근 대화 내용 미리보기\n/list - 캐시된 agy 대화 목록\n/switch <ID> - 지정한 대화로 전환\n/version - 커넥터 및 agy 버전",
	CommandUnknown:         "알 수 없는 명령어입니다. /help 를 입력하면 사용 가능한 명령어를 확인할 수 있습니다.",
	ErrorMediaNotSupported: "⚠️ 현재 시스템은 동영상, 음성 및 일반 문서 파일 분석을 지원하지 않습니다. 텍스트 및 이미지 파일만 전송해 주십시오.",
	ErrorMediaDownloadFail: "미디어 다운로드에 실패했습니다.",
	ErrorMissingUUID:       "❌ 봇 설정 오류: .env 파일에 AGY_CONVERSATION_ID가 설정되지 않았습니다.",
	ErrorCLIFailure:        "❌ 시스템 실행 오류 발생.\n\nError: %v\n\nLog: ...%s",
	ErrorJSONParseFail:     "❌ 시스템 응답을 해석하는 데 실패했습니다.",
	ErrorSystemResponse:    "⚠️ 시스템 응답 오류: %s",
	ErrorEmptyResponse:     "⚠️ 명령이 빈 응답을 반환했습니다.",
	DefaultMediaPrompt:     "Analyze the attached media file(s) comprehensively. Describe the contents, text, and context in detail. Please provide the final response in Korean.",
	StopDone:               "⛔ 진행 중인 agy 작업을 중지했습니다.\n새 메시지를 보내주세요.",
	StopDoneWithQueued:     "⛔ 진행 중인 agy 작업을 중지하고 대기 중인 %d개 요청을 취소했습니다.\n새 메시지를 보내주세요.",
	StopNothing:            "ℹ️ 현재 진행 중이거나 대기 중인 작업이 없습니다.",
	QueuedNotice:           "⏳ 현재 작업이 진행 중입니다.\n요청을 대기열에 추가했습니다. (%d번째)",
	ImageUsage:             "ℹ️ 사용법: /image <묘사>\n예: /image 창가에서 햇볕을 쬐는 고양이, 따뜻한 수채화 느낌",
	ImageKeyMissing:        "❌ 설정 오류: .env 파일에 NVIDIA_API_KEY가 설정되지 않았습니다.\n추가 후 gemini-connector를 재시작해야 적용됩니다.",
	ImageGenerating:        "⏳ 이미지를 생성하고 있습니다…",
	ImageTimeout:           "⏱️ NVIDIA 응답이 지연되어 시간 초과되었습니다. 잠시 후 다시 시도해 주세요.",
	ImageFail:              "❌ 이미지 생성 실패: %v",
	ImageTranslateTemplate: "Translate the following request into a detailed English prompt for a text-to-image model. Keep all visual details, style and composition. Reply with ONLY the English prompt text - no quotes, no explanations:\n\n%s",
	ImageFiltered:          "🚫 NVIDIA 안전 필터가 이 요청을 차단했습니다. 프롬프트 표현을 바꿔 다시 시도해 주세요.",
}

// applyDefaults fills fields missing from an older messages.json so that new
// features work without requiring users to regenerate the file.
func (m *Messages) applyDefaults() {
	d := &defaultMessages
	if m.StartupWelcome == "" {
		m.StartupWelcome = d.StartupWelcome
	}
	if m.CommandStartHelp == "" {
		m.CommandStartHelp = d.CommandStartHelp
	}
	if m.CommandUnknown == "" {
		m.CommandUnknown = d.CommandUnknown
	}
	if m.ErrorMediaNotSupported == "" {
		m.ErrorMediaNotSupported = d.ErrorMediaNotSupported
	}
	if m.ErrorMediaDownloadFail == "" {
		m.ErrorMediaDownloadFail = d.ErrorMediaDownloadFail
	}
	if m.ErrorMissingUUID == "" {
		m.ErrorMissingUUID = d.ErrorMissingUUID
	}
	if m.ErrorCLIFailure == "" {
		m.ErrorCLIFailure = d.ErrorCLIFailure
	}
	if m.ErrorJSONParseFail == "" {
		m.ErrorJSONParseFail = d.ErrorJSONParseFail
	}
	if m.ErrorSystemResponse == "" {
		m.ErrorSystemResponse = d.ErrorSystemResponse
	}
	if m.ErrorEmptyResponse == "" {
		m.ErrorEmptyResponse = d.ErrorEmptyResponse
	}
	if m.DefaultMediaPrompt == "" {
		m.DefaultMediaPrompt = d.DefaultMediaPrompt
	}
	if m.StopDone == "" {
		m.StopDone = d.StopDone
	}
	if m.StopDoneWithQueued == "" {
		m.StopDoneWithQueued = d.StopDoneWithQueued
	}
	if m.StopNothing == "" {
		m.StopNothing = d.StopNothing
	}
	if m.QueuedNotice == "" {
		m.QueuedNotice = d.QueuedNotice
	}
	if m.ImageUsage == "" {
		m.ImageUsage = d.ImageUsage
	}
	if m.ImageKeyMissing == "" {
		m.ImageKeyMissing = d.ImageKeyMissing
	}
	if m.ImageGenerating == "" {
		m.ImageGenerating = d.ImageGenerating
	}
	if m.ImageTimeout == "" {
		m.ImageTimeout = d.ImageTimeout
	}
	if m.ImageFail == "" {
		m.ImageFail = d.ImageFail
	}
	if m.ImageTranslateTemplate == "" {
		m.ImageTranslateTemplate = d.ImageTranslateTemplate
	}
	if m.ImageFiltered == "" {
		m.ImageFiltered = d.ImageFiltered
	}
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
	msgs.applyDefaults()
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

	// Restore a persisted quota cooldown across restarts.
	if rem := RestoreQuotaCooldown(); rem > 0 {
		log.Printf("Quota cooldown restored from disk: %s remaining", formatQuotaDuration(rem))
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

	// Serializes agy work; /stop cancels the running job and drops queued ones.
	turnQ := newAgyTurnQueue()

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
			case "/stop":
				active, dropped := turnQ.StopActive()
				switch {
				case active && dropped > 0:
					adapter.Send(m.ChatID, fmt.Sprintf(msgs.StopDoneWithQueued, dropped), replyOpt)
				case active:
					adapter.Send(m.ChatID, msgs.StopDone, replyOpt)
				default:
					adapter.Send(m.ChatID, msgs.StopNothing, replyOpt)
				}
				return
			case "/reset":
				enqueueTurn(turnQ, adapter, m.ChatID, m.MessageID, msgs, func(ctx context.Context) {
					resetConversation(ctx, cfg, adapter, m.ChatID, m.MessageID, "", msgs)
				})
				return
			case "/clear":
				enqueueTurn(turnQ, adapter, m.ChatID, m.MessageID, msgs, func(ctx context.Context) {
					clearConversation(ctx, cfg, adapter, m.ChatID, m.MessageID, msgs)
				})
				return
			case "/image":
				enqueueTurn(turnQ, adapter, m.ChatID, m.MessageID, msgs, func(ctx context.Context) {
					imageCommand(ctx, cfg, adapter, m.ChatID, m.MessageID, m.Args, msgs)
				})
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
				enqueueTurn(turnQ, adapter, m.ChatID, m.MessageID, msgs, func(ctx context.Context) {
					switchConversation(cfg, adapter, m.ChatID, m.Args, m.MessageID, msgs)
				})
				return
			}

			if cfg.ConversationID() == "" {
				adapter.Send(m.ChatID, msgs.ErrorMissingUUID, replyOpt)
				return
			}

			// Fast path during quota cooldown: reply immediately without
			// touching the turn queue. Jobs that are already queued get the
			// same treatment inside executeAgy.
			if QuotaActive() {
				adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
				return
			}

			ahead := turnQ.Enqueue(func(ctx context.Context) {
				stop := adapter.StartTyping(m.ChatID)
				defer stop()

				// Turn start marker: files modified from this point on are
				// considered AI-produced and eligible for attachment delivery.
				turnStart := time.Now()

				response, err := executeAgy(ctx, m.Content, cfg.ConversationID())
				if err != nil {
					if ctx.Err() != nil {
						// Cancelled via /stop: stay silent, the stop notice
						// already went out.
						log.Printf("agy turn cancelled by /stop: %s", truncateString(m.Content, 50))
						return
					}
					if ae, ok := err.(*AgyError); ok {
						switch ae.Type {
						case "cli_failure":
							adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorCLIFailure, ae.Err, ae.Detail), replyOpt)
						case "json_parse_fail":
							adapter.Send(m.ChatID, msgs.ErrorJSONParseFail, replyOpt)
						case "quota_cooldown":
							adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, ae.Detail), replyOpt)
						case "error_status":
							detail := ae.Detail
							// 429 quota exhaustion: start/refresh cooldown and
							// answer with the decremented error text.
							if QuotaCapture(detail) {
								adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
								return
							}
							// agy occasionally reports ERROR because an
							// intermediate tool failed (e.g. its built-in
							// grep_search exiting non-zero) while the model
							// still finished the turn; the brain transcript
							// then holds the real answer.
							if salvaged := salvageTurnResponse(cfg.ConversationID(), m.Content, turnStart); salvaged != "" {
								log.Printf("agy reported an error (%s) but the turn completed; delivering the recovered response", truncateString(detail, 80))
								appendTranscript(cfg.ConversationID(), "user", m.Content)
								appendTranscript(cfg.ConversationID(), "assistant", salvaged)
								adapter.Send(m.ChatID, salvaged, SendOptions{ReplyToMessageID: m.MessageID, AttachAfter: turnStart})
								return
							}
							if extractUrlFetchFailure(detail) != "" && cfg.recordStuckError("url_fetch", detail) {
								log.Printf("Stuck conversation detected (repeated URL fetch failure), resetting session")
								resetConversation(ctx, cfg, adapter, m.ChatID, m.MessageID, m.Content, msgs)
								return
							}
							// Conversation-state validation failures poison every
							// subsequent turn; auto-reset after a repeat.
							if strings.Contains(detail, "invalid arguments") {
								if cfg.recordStuckError("invalid_args", detail) {
									log.Printf("Repeated invalid-arguments errors, auto-resetting session")
									resetConversation(ctx, cfg, adapter, m.ChatID, m.MessageID, m.Content, msgs)
									return
								}
								adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, detail+invalidArgsHint), replyOpt)
								return
							}
							// Any other repeating error_status: fall back to a fresh
							// session WITHOUT replaying the failing prompt — such
							// errors are often environmental or model-behavioural,
							// so replaying would just burn quota in a loop.
							if cfg.recordStuckError("generic", detail) {
								log.Printf("Repeated error detected, auto-resetting session without replay")
								resetConversation(ctx, cfg, adapter, m.ChatID, m.MessageID, "", msgs)
								return
							}
							adapter.Send(m.ChatID, fmt.Sprintf(msgs.ErrorSystemResponse, detail), replyOpt)
						case "authentication_required":
							adapter.Send(m.ChatID, "⚠️ agy 인증이 필요합니다. 터미널에서 'agy'를 한 번 실행해 인증을 완료한 뒤 봇을 재시작하세요.", replyOpt)
						}
					}
					return
				}

				QuotaClear()

				if ctx.Err() != nil {
					log.Printf("agy turn cancelled by /stop (after completion): %s", truncateString(m.Content, 50))
					return
				}
				if response != "" {
					appendTranscript(cfg.ConversationID(), "user", m.Content)
					appendTranscript(cfg.ConversationID(), "assistant", response)
					adapter.Send(m.ChatID, response, SendOptions{ReplyToMessageID: m.MessageID, AttachAfter: turnStart})
				} else {
					adapter.Send(m.ChatID, msgs.ErrorEmptyResponse, replyOpt)
				}
			})
			if ahead > 0 {
				adapter.Send(m.ChatID, fmt.Sprintf(msgs.QueuedNotice, ahead), replyOpt)
			}
		}(msg)
	}
}

// enqueueTurn queues a state-changing command (/reset, /switch) on the agy
// turn queue and notifies the user when it has to wait.
func enqueueTurn(q *agyTurnQueue, adapter Messenger, chatID string, replyTo int, msgs *Messages, run func(ctx context.Context)) {
	ahead := q.Enqueue(run)
	if ahead > 0 {
		adapter.Send(chatID, fmt.Sprintf(msgs.QueuedNotice, ahead), SendOptions{ReplyToMessageID: replyTo})
	}
}

// resetConversation summarizes the old conversation into a fresh agy
// conversation, persists the new ID to .env, replays the given prompt on the
// new session, and deletes the old session artifacts. Cancelling ctx aborts
// the underlying agy calls silently.
func resetConversation(ctx context.Context, cfg *Config, adapter Messenger, chatID string, replyTo int, replayPrompt string, msgs *Messages) {
	oldID := cfg.ConversationID()
	log.Printf("Resetting conversation. Old conversation ID: %s", oldID)

	replyOpt := SendOptions{ReplyToMessageID: replyTo}

	summaryPrompt := buildSummaryPrompt(oldID)
	if strings.TrimSpace(summaryPrompt) == "" {
		summaryPrompt = "This connector bridges Telegram to agy. Reply only with 'agy Connector Ready.'"
	}

	// Commands and self-healing outrank the quota cooldown (see /clear).
	newID, _, err := createNewConversationRuntimeWithPrompt(ctx, summaryPrompt, AgyCallOptions{BypassQuotaGate: true})
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("Conversation reset cancelled by /stop")
			return
		}
		if ae, ok := err.(*AgyError); ok && ae.Type == "quota_cooldown" {
			// Creating a conversation is an agy call too; respect cooldown.
			adapter.Send(chatID, fmt.Sprintf(msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
			return
		}
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
	QuotaClear()

	deleteConversationArtifacts(oldID)
	deleteTranscript(oldID)

	notice := fmt.Sprintf("⚠️ 이전 대화를 요약해 새 세션으로 전환했습니다. (새 세션 ID: %s)", truncateString(newID, 8))

	if replayPrompt != "" {
		response, rerr := executeAgy(ctx, replayPrompt, newID, AgyCallOptions{BypassQuotaGate: true})
		if rerr == nil && response != "" && ctx.Err() == nil {
			appendTranscript(newID, "user", replayPrompt)
			appendTranscript(newID, "assistant", response)
			adapter.Send(chatID, notice+"\n\n"+response, replyOpt)
			return
		}
		if ctx.Err() != nil {
			log.Printf("Replay on new conversation cancelled by /stop")
			return
		}
		if rerr != nil {
			log.Printf("Replay on new conversation failed: %v", rerr)
		}
	}

	adapter.Send(chatID, notice, replyOpt)
}

// clearConversation deletes the current session's artifacts (DB, brain
// folder, transcript) and starts a completely fresh conversation without any
// summary carry-over. The new session is created first; the old artifacts are
// only removed after success, so a failure (or /stop cancellation) leaves the
// existing session intact.
func clearConversation(ctx context.Context, cfg *Config, adapter Messenger, chatID string, replyTo int, msgs *Messages) {
	oldID := cfg.ConversationID()
	log.Printf("Clearing conversation %s (no summary carry-over)", truncateString(oldID, 8))

	replyOpt := SendOptions{ReplyToMessageID: replyTo}

	// User commands outrank the quota cooldown: attempt execution even while
	// it is active. A repeated 429 is captured back into the cooldown.
	bootstrap := "This connector bridges Telegram to agy. Reply only with 'agy Connector Ready.'"
	newID, _, err := createNewConversationRuntimeWithPrompt(ctx, bootstrap, AgyCallOptions{BypassQuotaGate: true})
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("Conversation clear cancelled by /stop")
			return
		}
		if ae, ok := err.(*AgyError); ok && ae.Type == "quota_cooldown" {
			adapter.Send(chatID, fmt.Sprintf(msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
			return
		}
		log.Printf("Failed to create replacement conversation: %v", err)
		adapter.Send(chatID, fmt.Sprintf("⚠️ 새 세션 생성에 실패했습니다. 기존 세션이 유지됩니다: %v", err), replyOpt)
		return
	}

	if err := updateEnvKey(cfg.envPath, "AGY_CONVERSATION_ID", newID); err != nil {
		log.Printf("Failed to update .env with new conversation ID: %v", err)
	}
	cfg.applyNewConversation(newID)

	if oldID != "" {
		deleteConversationArtifacts(oldID)
		deleteTranscript(oldID)
	}
	log.Printf("Conversation cleared. New conversation ID: %s", newID)
	// The successful agy call proves quota is available again; drop any stale
	// cooldown so regular chat turns are not blocked unnecessarily.
	QuotaClear()
	adapter.Send(chatID, fmt.Sprintf("🗑️ 대화 기록을 모두 지우고 새 세션을 시작했습니다. (새 세션 ID: %s)", truncateString(newID, 8)), replyOpt)
}

// imageCommand turns a Korean description into an image: agy translates the
// prompt into English, NVIDIA NIM generates the picture, and the result is
// delivered as a Telegram photo. The local copy is removed only after a
// successful send.
func imageCommand(ctx context.Context, cfg *Config, adapter Messenger, chatID string, replyTo int, args string, msgs *Messages) {
	replyOpt := SendOptions{ReplyToMessageID: replyTo}

	prompt := strings.TrimSpace(args)
	if prompt == "" {
		adapter.Send(chatID, msgs.ImageUsage, replyOpt)
		return
	}
	apiKey := os.Getenv("NVIDIA_API_KEY")
	if apiKey == "" {
		adapter.Send(chatID, msgs.ImageKeyMissing, replyOpt)
		return
	}

	stop := adapter.StartTyping(chatID)
	defer stop()

	translated, err := executeAgy(ctx, buildImageTranslatePrompt(msgs.ImageTranslateTemplate, prompt), cfg.ConversationID(), AgyCallOptions{BypassQuotaGate: true})
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("/image cancelled by /stop during translation")
			return
		}
		if ae, ok := err.(*AgyError); ok && ae.Type == "quota_cooldown" {
			adapter.Send(chatID, fmt.Sprintf(msgs.ErrorSystemResponse, ae.Detail), replyOpt)
			return
		}
		adapter.Send(chatID, fmt.Sprintf(msgs.ImageFail, err), replyOpt)
		return
	}

	adapter.Send(chatID, msgs.ImageGenerating, replyOpt)

	img, err := generateNimImage(ctx, apiKey, cleanTranslatedPrompt(translated))
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("/image cancelled by /stop during generation")
			return
		}
		if errors.Is(err, errNimContentFiltered) {
			adapter.Send(chatID, msgs.ImageFiltered, replyOpt)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			adapter.Send(chatID, msgs.ImageTimeout, replyOpt)
			return
		}
		adapter.Send(chatID, fmt.Sprintf(msgs.ImageFail, err), replyOpt)
		return
	}

	path, err := saveGeneratedImage(img)
	if err != nil {
		adapter.Send(chatID, fmt.Sprintf(msgs.ImageFail, err), replyOpt)
		return
	}

	tg, ok := adapter.(*TelegramAdapter)
	if !ok {
		os.Remove(path)
		adapter.Send(chatID, msgs.ErrorMediaNotSupported, replyOpt)
		return
	}
	id, perr := strconv.ParseInt(chatID, 10, 64)
	if perr != nil {
		os.Remove(path)
		adapter.Send(chatID, fmt.Sprintf(msgs.ImageFail, perr), replyOpt)
		return
	}
	if err := tg.sendAttachment(id, path, replyTo); err != nil {
		// Keep the local file for troubleshooting; delivery can be retried.
		log.Printf("Failed to deliver generated image %s: %v", path, err)
		adapter.Send(chatID, fmt.Sprintf(msgs.ImageFail, err), replyOpt)
		return
	}
	if rmErr := os.Remove(path); rmErr != nil {
		log.Printf("Failed to remove delivered image %s: %v", path, rmErr)
	} else {
		log.Printf("Image delivered and local copy removed: %s", path)
	}
}

// buildImageTranslatePrompt injects the user request into the configurable
// translation template. Templates missing the %s placeholder get the request
// appended, so an edited template can never silently drop it.
func buildImageTranslatePrompt(template string, request string) string {
	if !strings.Contains(template, "%s") {
		template += "\n\n%s"
	}
	return strings.ReplaceAll(template, "%s", request)
}

// cleanTranslatedPrompt strips whitespace and wrapping quotes that agy may
// add around the translated prompt.
func cleanTranslatedPrompt(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"“”'`")
	return strings.TrimSpace(s)
}

// saveGeneratedImage writes the decoded image into the shared downloads
// directory with an extension derived from its magic bytes.
func saveGeneratedImage(img []byte) (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(exePath), "..", "downloads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("image_%d%s", time.Now().UnixMilli(), imageFileExt(img)))
	if err := os.WriteFile(path, img, 0644); err != nil {
		return "", err
	}
	return path, nil
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
