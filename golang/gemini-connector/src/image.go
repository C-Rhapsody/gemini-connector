package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// imageCommand turns a Korean description into an image: agy translates the
// prompt into English, NVIDIA NIM generates the picture, and the result is
// delivered as a platform attachment. The local copy is removed only after a
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

	translated, err := executeAgy(ctx, buildImageTranslatePrompt(msgs.ImageTranslateTemplate, prompt), cfg.ConversationID(), AgyCallOptions{Profile: ProfileInteractive, BypassQuotaGate: true})
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

	att, ok := adapter.(AttachmentSender)
	if !ok {
		os.Remove(path)
		adapter.Send(chatID, msgs.ErrorMediaNotSupported, replyOpt)
		return
	}
	if err := att.SendAttachment(chatID, path, replyTo); err != nil {
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
