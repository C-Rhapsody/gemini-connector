package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// ensureEnvVars runs the interactive first-run wizard: it prompts for any
// required variable missing from the environment, writes the .env file and
// reloads it. Non-interactive restarts find everything set and return fast.
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
