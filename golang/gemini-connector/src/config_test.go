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
