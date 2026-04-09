package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type SessionInfo struct {
	UUID  string
	Title string
	Time  string
}

func interactiveSessionSelect(reader *bufio.Reader) (string, error) {
	_, err := exec.LookPath("gemini")
	if err != nil {
		return "", fmt.Errorf("gemini-cli is not installed or not in PATH. Please run 'npm install -g @google/gemini-cli'")
	}

	for {
		fmt.Println("\n🔍 Fetching Gemini sessions...")
		cmd := exec.Command("gemini", "--list-sessions")
		cmd.Dir = findProjectRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to fetch sessions: %v", err)
		}

		sessions := parseSessions(string(out))

		if len(sessions) == 0 {
			fmt.Println("💡 No existing sessions found. Creating a new session...")
			if err := createNewSession(); err != nil {
				return "", err
			}
			continue
		}

		const pageSize = 10
		page := 0
		totalPages := (len(sessions) + pageSize - 1) / pageSize

		for {
			start := page * pageSize
			end := start + pageSize
			if end > len(sessions) {
				end = len(sessions)
			}

			fmt.Printf("\n=== 🤖 Select Gemini Session (Page %d/%d) ===\n", page+1, totalPages)
			for i := start; i < end; i++ {
				fmt.Printf("[%d] %s (%s) [%s]\n", i+1, truncateString(sessions[i].Title, 20), sessions[i].Time, sessions[i].UUID)
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
				if err := createNewSession(); err != nil {
					fmt.Printf("❌ Error: %v\n", err)
				}
				break
			} else if input == "m" {
				fmt.Print("✍️ Enter UUID manually: ")
				mInput, _ := reader.ReadString('\n')
				return strings.TrimSpace(mInput), nil
			} else if input == "x" {
				fmt.Println("👋 Exiting gemini-connector. Goodbye!")
				os.Exit(0)
			}

			idx, err := strconv.Atoi(input)
			if err == nil && idx >= 1 && idx <= len(sessions) {
				selected := sessions[idx-1]
				fmt.Printf("✅ Selected: %s\n", selected.UUID)
				return selected.UUID, nil
			}

			fmt.Println("❌ Invalid input. Please try again.")
		}
	}
}

func parseSessions(output string) []SessionInfo {
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	cleanOut := ansiRegex.ReplaceAllString(output, "")

	var sessions []SessionInfo
	lines := strings.Split(cleanOut, "\n")

	re := regexp.MustCompile(`^\s*\d+\.\s*(.*?)\s*\(([^)]+)\)\s*\[([a-fA-F0-9\-]{36})\]`)

	for _, line := range lines {
		matches := re.FindStringSubmatch(line)
		if len(matches) == 4 {
			title := strings.TrimSpace(matches[1])
			if title == "" {
				title = "(No Title)"
			}
			sessions = append(sessions, SessionInfo{
				Title: title,
				Time:  matches[2],
				UUID:  matches[3],
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return getTimeWeight(sessions[i].Time) < getTimeWeight(sessions[j].Time)
	})

	return sessions
}

func getTimeWeight(t string) int {
	t = strings.ToLower(t)
	if strings.Contains(t, "just now") {
		return 0
	}

	fields := strings.Fields(t)
	if len(fields) < 2 {
		return 9999999
	}

	val, _ := strconv.Atoi(fields[0])
	unit := fields[1]

	multiplier := 1
	if strings.Contains(unit, "sec") {
		multiplier = 1
	} else if strings.Contains(unit, "min") {
		multiplier = 60
	} else if strings.Contains(unit, "hour") {
		multiplier = 3600
	} else if strings.Contains(unit, "day") {
		multiplier = 86400
	} else if strings.Contains(unit, "week") {
		multiplier = 604800
	} else if strings.Contains(unit, "month") {
		multiplier = 2592000
	} else if strings.Contains(unit, "year") {
		multiplier = 31536000
	}

	return val * multiplier
}

func createNewSession() error {
	fmt.Println("⏳ Generating a new Gemini session...")

	prompt := "Telegram Connector is you. Reply Only with 'Telegram Connector Ready.'"
	cmd := exec.Command("gemini", "-p", prompt)
	cmd.Dir = findProjectRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run gemini-cli: %v", err)
	}

	fmt.Println("✅ Session creation command finished.")
	return nil
}
