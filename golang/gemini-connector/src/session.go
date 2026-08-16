package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const agyConversationCacheRelPath = ".gemini/antigravity-cli/cache/last_conversations.json"

func agyInstallHint() string {
	switch runtime.GOOS {
	case "windows":
		return "Run PowerShell: irm https://antigravity.google/cli/install.ps1 | iex"
	case "darwin":
		return "Run: curl -fsSL https://antigravity.google/cli/install.sh | bash"
	default:
		return "Run: curl -fsSL https://antigravity.google/cli/install.sh | bash"
	}
}

func interactiveConversationSelect(reader *bufio.Reader) (string, error) {
	if _, err := exec.LookPath("agy"); err != nil {
		return "", fmt.Errorf("agy (Antigravity CLI) is not installed or not in PATH. %s", agyInstallHint())
	}

	for {
		fmt.Println("\n🔍 Fetching Antigravity conversations from local cache...")
		entries, err := loadConversationCache()
		if err != nil {
			fmt.Printf("⚠️ Could not read conversation cache: %v\n", err)
		}

		if len(entries) == 0 {
			fmt.Println("💡 No cached conversations found. Creating a new conversation...")
			if newID, cerr := createNewConversation(); cerr != nil {
				fmt.Printf("❌ Error: %v\n", cerr)
				fmt.Println("You can also enter a conversation ID manually.")
			} else if newID != "" {
				fmt.Printf("✅ New conversation created: %s\n", newID)
				return newID, nil
			}
			fmt.Print("✍️ Enter Antigravity Conversation ID (or [x] to exit): ")
			mInput, _ := reader.ReadString('\n')
			mInput = strings.TrimSpace(mInput)
			if strings.EqualFold(mInput, "x") || mInput == "" {
				return "", errors.New("no conversation selected")
			}
			return mInput, nil
		}

		const pageSize = 10
		page := 0
		totalPages := (len(entries) + pageSize - 1) / pageSize

		for {
			start := page * pageSize
			end := start + pageSize
			if end > len(entries) {
				end = len(entries)
			}

			fmt.Printf("\n=== 🤖 Select Antigravity Conversation (Page %d/%d) ===\n", page+1, totalPages)
			for i := start; i < end; i++ {
				conv := entries[i]
				id := truncateString(conv.ID, 36)
				path := truncateString(conv.Workspace, 60)
				fmt.Printf("[%d] %s   (%s)\n", i+1, id, path)
			}
			fmt.Println("-------------------------------------------------")

			opts := []string{}
			if page > 0 {
				opts = append(opts, "[p] Prev")
			}
			if page < totalPages-1 {
				opts = append(opts, "[n] Next")
			}
			opts = append(opts, "[r] Refresh", "[c] Create New", "[m] Manual Input", "[x] Exit")

			fmt.Println(strings.Join(opts, "   "))
			fmt.Print("👉 Select an option (Number or Letter): ")

			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))

			if input == "p" && page > 0 {
				page--
				continue
			} else if input == "n" && page < totalPages-1 {
				page++
				continue
			} else if input == "r" {
				break
			} else if input == "c" {
				newID, cerr := createNewConversation()
				if cerr != nil {
					fmt.Printf("❌ Error: %v\n", cerr)
					break
				}
				if newID != "" {
					fmt.Printf("✅ New conversation created: %s\n", newID)
					return newID, nil
				}
				break
			} else if input == "m" {
				fmt.Print("✍️ Enter Antigravity Conversation ID manually: ")
				mInput, _ := reader.ReadString('\n')
				return strings.TrimSpace(mInput), nil
			} else if input == "x" {
				fmt.Println("👋 Exiting gemini-connector. Goodbye!")
				os.Exit(0)
			}

			idx, err := strconv.Atoi(input)
			if err == nil && idx >= 1 && idx <= len(entries) {
				selected := entries[idx-1]
				fmt.Printf("✅ Selected: %s\n", selected.ID)
				return selected.ID, nil
			}

			fmt.Println("❌ Invalid input. Please try again.")
		}
	}
}

type ConversationEntry struct {
	Workspace string
	ID        string
}

func loadConversationCache() ([]ConversationEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not resolve user home directory: %w", err)
	}
	cachePath := filepath.Join(home, agyConversationCacheRelPath)

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid cache JSON: %w", err)
	}

	entries := make([]ConversationEntry, 0, len(raw))
	for ws, id := range raw {
		entries = append(entries, ConversationEntry{Workspace: ws, ID: id})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Workspace < entries[j].Workspace
	})
	return entries, nil
}

func createNewConversation() (string, error) {
	fmt.Println("⏳ Generating a new Antigravity conversation...")

	prompt := "This connector bridges Telegram to agy. Reply only with 'agy Connector Ready.'"
	cmd := exec.Command("agy", "--output-format", "json", "--dangerously-skip-permissions", "--print-timeout", "5m")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = findProjectRoot()

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run agy CLI: %w", err)
	}

	var result struct {
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("failed to parse agy response: %w (raw: %s)", err, string(out))
	}
	if result.ConversationID == "" {
		return "", fmt.Errorf("agy did not return a conversation_id (status: %s)", result.Status)
	}

	fmt.Println("✅ Conversation creation command finished.")
	return result.ConversationID, nil
}

// createNewConversationRuntime creates a fresh agy conversation during bot
// runtime. Unlike createNewConversation, it logs instead of printing to stdout.
func createNewConversationRuntime() (string, error) {
	id, _, err := createNewConversationRuntimeWithPrompt("This connector bridges Telegram to agy. Reply only with 'agy Connector Ready.'")
	return id, err
}

// createNewConversationRuntimeWithPrompt creates a fresh agy conversation and
// sends the given prompt as its first turn, returning the new conversation ID
// and the agy response text.
func createNewConversationRuntimeWithPrompt(prompt string) (string, string, error) {
	cmd := exec.Command("agy", "--output-format", "json", "--dangerously-skip-permissions", "--print-timeout", "5m")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = findProjectRoot()

	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("failed to run agy CLI: %w", err)
	}

	var result struct {
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
		Response       string `json:"response"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse agy response: %w (raw: %s)", err, string(out))
	}
	if result.ConversationID == "" {
		return "", "", fmt.Errorf("agy did not return a conversation_id (status: %s)", result.Status)
	}

	return result.ConversationID, result.Response, nil
}

// updateEnvKey updates a single KEY=value line in the .env file, preserving
// all other lines and comments.
func updateEnvKey(envPath string, key string, value string) error {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	keyPrefix := key + "="
	updated := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, keyPrefix) {
			lines[i] = keyPrefix + value
			updated = true
			break
		}
	}
	if !updated {
		lines = append(lines, keyPrefix+value)
	}

	return os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0600)
}
