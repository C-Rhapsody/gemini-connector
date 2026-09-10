package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnvPath(t *testing.T) {
	fakeExeDir := filepath.Join(string(os.PathSeparator), "opt", "bot", "bin")

	t.Run("empty keeps default next to executable", func(t *testing.T) {
		want := filepath.Join(fakeExeDir, "..", "src", ".env")
		if got := resolveEnvPath("", fakeExeDir); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("absolute path used as-is", func(t *testing.T) {
		abs := filepath.Join(os.TempDir(), "custom.env")
		if got := resolveEnvPath(abs, fakeExeDir); got != filepath.Clean(abs) {
			t.Fatalf("got %q, want %q", got, filepath.Clean(abs))
		}
	})

	t.Run("relative resolved against working directory", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(cwd, "configs", "prod.env")
		if got := resolveEnvPath(filepath.Join("configs", "prod.env"), fakeExeDir); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestUpdateEnvKey_CustomPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(envPath, []byte("TELEGRAM_BOT_TOKEN=tok\nAGY_CONVERSATION_ID=old-id\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := updateEnvKey(envPath, "AGY_CONVERSATION_ID", "new-id"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "TELEGRAM_BOT_TOKEN=tok\nAGY_CONVERSATION_ID=new-id\n"
	if got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestParseTelegramRichConfig(t *testing.T) {
	setEnv := func(k, v string) func() {
		old, exists := os.LookupEnv(k)
		if v == "" {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, v)
		}
		return func() {
			if exists {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		}
	}

	t.Run("absent TELEGRAM_RICH_MESSAGES defaults to false", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "")()
		enabled, escape, err := parseTelegramRichConfig(12345)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if enabled || escape != "" {
			t.Fatalf("expected false, empty; got %v, %q", enabled, escape)
		}
	})

	t.Run("explicit false without escape succeeds", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "false")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "")()
		enabled, escape, err := parseTelegramRichConfig(12345)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if enabled || escape != "" {
			t.Fatalf("expected false, empty; got %v, %q", enabled, escape)
		}
	})

	t.Run("explicit true with valid raw escape", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "true")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "raw")()
		enabled, escape, err := parseTelegramRichConfig(12345)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !enabled || escape != "raw" {
			t.Fatalf("expected true, raw; got %v, %q", enabled, escape)
		}
	})

	t.Run("explicit true with valid numeric escape", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "true")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "numeric")()
		enabled, escape, err := parseTelegramRichConfig(12345)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !enabled || escape != "numeric" {
			t.Fatalf("expected true, numeric; got %v, %q", enabled, escape)
		}
	})

	t.Run("explicit true with invalid escape fails closed", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "true")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "invalid_escape")()
		_, _, err := parseTelegramRichConfig(12345)
		if err == nil {
			t.Fatal("expected error for invalid escape value, got nil")
		}
	})

	t.Run("explicit true with missing escape fails closed", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "true")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "")()
		_, _, err := parseTelegramRichConfig(12345)
		if err == nil {
			t.Fatal("expected error for missing escape value, got nil")
		}
	})

	t.Run("explicit true with zero chat ID fails closed", func(t *testing.T) {
		defer setEnv("TELEGRAM_RICH_MESSAGES", "true")()
		defer setEnv("TELEGRAM_RICH_MATH_ESCAPE", "raw")()
		_, _, err := parseTelegramRichConfig(0)
		if err == nil {
			t.Fatal("expected error for zero chat ID, got nil")
		}
	})
}
