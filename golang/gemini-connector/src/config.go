package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// --- Configuration ---

type Config struct {
	ActiveMessengers         []string
	TelegramBotToken         string
	TelegramChatID           int64
	TeamsTenantID            string
	TeamsAppID               string
	TeamsAppSecret           string
	TeamsChatID              string
	AgyConversationID        string
	CronAdminTelegramUserIDs []string

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

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// resolveEnvPath determines which .env file the connector uses. An empty
// flag value keeps the historical default next to the executable; explicit
// relative paths are interpreted against the current working directory,
// like any other CLI argument.
func resolveEnvPath(flagValue string, exeDir string) string {
	if flagValue == "" {
		return filepath.Join(exeDir, "..", "src", ".env")
	}
	if filepath.IsAbs(flagValue) {
		return filepath.Clean(flagValue)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Clean(flagValue)
	}
	return filepath.Join(cwd, flagValue)
}

func loadConfig(envFlag string) (*Config, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %v", err)
	}
	envPath := resolveEnvPath(envFlag, filepath.Dir(exePath))
	log.Printf("Using .env file: %s", envPath)

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

	// Cron admin allowlist (optional; no wizard prompt by design).
	var cronAdmins []string
	if raw := os.Getenv("CRON_ADMIN_TELEGRAM_USER_IDS"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if id := strings.TrimSpace(part); id != "" {
				cronAdmins = append(cronAdmins, id)
			}
		}
	}

	return &Config{
		ActiveMessengers:         activeMessengers,
		TelegramBotToken:         token,
		TelegramChatID:           chatID,
		TeamsTenantID:            teamsTenantID,
		TeamsAppID:               teamsAppID,
		TeamsAppSecret:           teamsAppSecret,
		TeamsChatID:              teamsChatID,
		AgyConversationID:        convID,
		CronAdminTelegramUserIDs: cronAdmins,
		envPath:                  envPath,
	}, nil
}
