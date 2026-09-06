package main

import (
	"os"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestIsSupportedInboundDocument(t *testing.T) {
	allowed := []string{
		"notes.txt", "README.md", "page.html", "doc.pdf", "old.doc", "new.docx",
		"text.rtf", "open.odt", "data.csv", "sheet.xls", "sheet.xlsx",
		"deck.ppt", "deck.pptx", "한글.hwp", "한글.hwpx",
		"NOTES.TXT", "PAGE.HTML", "CONTRACT.DOCX", "DATA.CSV", "한글.HWP",
	}
	for _, name := range allowed {
		if !isSupportedInboundDocument(name) {
			t.Errorf("expected supported: %q", name)
		}
	}

	denied := []string{
		"", "noext", "archive.zip", "app.exe", "data.json", "script.js",
		"image.png", "song.mp3", "clip.mp4", "notes.txt.exe", "conf.yaml",
	}
	for _, name := range denied {
		if isSupportedInboundDocument(name) {
			t.Errorf("expected rejected: %q", name)
		}
	}
}

func TestClassifyInboundMedia(t *testing.T) {
	cases := []struct {
		name string
		msg  *tgbotapi.Message
		want inboundMediaKind
	}{
		{"plain text", &tgbotapi.Message{}, inboundMediaNone},
		{"photo", &tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "p"}}}, inboundMediaPhoto},
		{"supported doc", &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d", FileName: "report.md"}}, inboundMediaDocument},
		{"uppercase doc", &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d", FileName: "REPORT.PDF"}}, inboundMediaDocument},
		{"hwp doc", &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d", FileName: "문서.hwp"}}, inboundMediaDocument},
		{"unsupported doc", &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d", FileName: "archive.zip"}}, inboundMediaUnsupported},
		{"no extension doc", &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d", FileName: "report"}}, inboundMediaUnsupported},
		{"video", &tgbotapi.Message{Video: &tgbotapi.Video{FileID: "v"}}, inboundMediaUnsupported},
		{"video note", &tgbotapi.Message{VideoNote: &tgbotapi.VideoNote{FileID: "n"}}, inboundMediaUnsupported},
		{"audio", &tgbotapi.Message{Audio: &tgbotapi.Audio{FileID: "a"}}, inboundMediaUnsupported},
		{"voice", &tgbotapi.Message{Voice: &tgbotapi.Voice{FileID: "o"}}, inboundMediaUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInboundMedia(tc.msg); got != tc.want {
				t.Fatalf("classify = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInboundFileName(t *testing.T) {
	photo := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}, Photo: []tgbotapi.PhotoSize{{FileID: "p"}}}
	if got := inboundFileName(photo, 123, 1); got != "123_7_01.jpg" {
		t.Fatalf("photo name = %q", got)
	}

	doc := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 123}, Document: &tgbotapi.Document{FileID: "d", FileName: "계약서.DOCX"}}
	if got := inboundFileName(doc, 123, 1); got != "123_7_01.docx" {
		t.Fatalf("doc name = %q", got)
	}

	md := &tgbotapi.Message{MessageID: 8, Document: &tgbotapi.Document{FileID: "d", FileName: "notes.MD"}}
	if got := inboundFileName(md, 123, 3); got != "123_8_03.md" {
		t.Fatalf("md name = %q", got)
	}

	plain := &tgbotapi.Message{MessageID: 9}
	if got := inboundFileName(plain, 123, 1); got != "" {
		t.Fatalf("plain message name = %q", got)
	}
}

func TestHandleIncomingMessageRejectsUnsupportedMedia(t *testing.T) {
	cases := []struct {
		name string
		msg  *tgbotapi.Message
	}{
		{"unsupported document", tgMessage(42, -7, "")},
		{"video", tgMessage(42, -7, "")},
	}
	cases[0].msg.Document = &tgbotapi.Document{FileID: "d", FileName: "evil.zip"}
	cases[1].msg.Video = &tgbotapi.Video{FileID: "v"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, ch := testAdapter(t)
			adapter.handleIncomingMessage(tc.msg)
			select {
			case ev := <-ch:
				t.Fatalf("rejected media reached agy: %+v", ev)
			default:
			}
		})
	}
}

// stubInboundDownload swaps the Telegram API seams so tests can download to
// the temp downloads dir without network access.
func stubInboundDownload(t *testing.T) {
	t.Helper()
	oldGet := getFileInfo
	oldDL := downloadFile
	getFileInfo = func(bot *tgbotapi.BotAPI, fileID string) (tgbotapi.File, error) {
		return tgbotapi.File{FileID: fileID, FilePath: "documents/original.docx"}, nil
	}
	downloadFile = func(url, destPath string) (int, error) {
		return 0, os.WriteFile(destPath, []byte("dummy"), 0o644)
	}
	t.Cleanup(func() {
		getFileInfo = oldGet
		downloadFile = oldDL
	})
}

func TestHandleIncomingMessageForwardsSupportedDocument(t *testing.T) {
	stubInboundDownload(t)

	adapter := &TelegramAdapter{
		msgs:    &Messages{DefaultMediaPrompt: "analyze me", ErrorMediaDownloadFail: "dl fail"},
		msgChan: make(chan InboundEvent, 10),
		bot:     &tgbotapi.BotAPI{Token: "testtoken"},
	}
	msg := tgMessage(42, -7, "")
	msg.Document = &tgbotapi.Document{FileID: "d1", FileName: "계약서.docx"}
	msg.Caption = "계약 내용을 분석해줘"
	adapter.handleIncomingMessage(msg)

	select {
	case ev := <-adapter.msgChan:
		for _, want := range []string{"[첨부파일:", "[원본파일명: 계약서.docx]", "계약 내용을 분석해줘"} {
			if !strings.Contains(ev.Content, want) {
				t.Fatalf("event content missing %q: %q", want, ev.Content)
			}
		}
		if len(ev.AttachmentPaths) != 1 {
			t.Fatalf("expected 1 attachment path, got %v", ev.AttachmentPaths)
		}
		if !strings.HasSuffix(ev.AttachmentPaths[0], "01.docx") {
			t.Fatalf("unexpected download path: %q", ev.AttachmentPaths[0])
		}
		if _, err := os.Stat(ev.AttachmentPaths[0]); err != nil {
			t.Fatalf("downloaded file missing: %v", err)
		}
	default:
		t.Fatal("no event produced for supported document")
	}
}

func TestHandleIncomingMessageDocumentWithoutCaptionUsesDefaultPrompt(t *testing.T) {
	stubInboundDownload(t)

	adapter := &TelegramAdapter{
		msgs:    &Messages{DefaultMediaPrompt: "analyze me", ErrorMediaDownloadFail: "dl fail"},
		msgChan: make(chan InboundEvent, 10),
		bot:     &tgbotapi.BotAPI{Token: "testtoken"},
	}
	msg := tgMessage(42, -7, "")
	msg.Document = &tgbotapi.Document{FileID: "d2", FileName: "README.md"}
	adapter.handleIncomingMessage(msg)

	select {
	case ev := <-adapter.msgChan:
		if !strings.Contains(ev.Content, "analyze me") {
			t.Fatalf("default prompt not applied: %q", ev.Content)
		}
		if !strings.Contains(ev.Content, "[원본파일명: README.md]") {
			t.Fatalf("original filename missing: %q", ev.Content)
		}
	default:
		t.Fatal("no event produced")
	}
}

func TestHandleIncomingMessagePhotoKeepsLegacyFormat(t *testing.T) {
	stubInboundDownload(t)

	adapter := &TelegramAdapter{
		msgs:    &Messages{DefaultMediaPrompt: "analyze me", ErrorMediaDownloadFail: "dl fail"},
		msgChan: make(chan InboundEvent, 10),
		bot:     &tgbotapi.BotAPI{Token: "testtoken"},
	}
	msg := tgMessage(42, -7, "")
	msg.Photo = []tgbotapi.PhotoSize{{FileID: "p1"}}
	adapter.handleIncomingMessage(msg)

	select {
	case ev := <-adapter.msgChan:
		if !strings.Contains(ev.Content, "[첨부파일: ") {
			t.Fatalf("photo prompt missing attachment: %q", ev.Content)
		}
		if strings.Contains(ev.Content, "[원본파일명:") {
			t.Fatalf("photo must not carry an original filename: %q", ev.Content)
		}
		if len(ev.AttachmentPaths) != 1 {
			t.Fatalf("expected 1 photo path, got %v", ev.AttachmentPaths)
		}
	default:
		t.Fatal("no event produced for photo")
	}
}

func TestProcessAlbumForwardsDocuments(t *testing.T) {
	stubInboundDownload(t)

	adapter := &TelegramAdapter{
		msgs:        &Messages{DefaultMediaPrompt: "analyze me"},
		msgChan:     make(chan InboundEvent, 10),
		bot:         &tgbotapi.BotAPI{Token: "testtoken"},
		albumBuffer: make(map[string][]*tgbotapi.Message),
	}

	m1 := tgMessage(42, -7, "")
	m1.MessageID = 1
	m1.Document = &tgbotapi.Document{FileID: "a", FileName: "one.md"}
	m1.Caption = "첫 번째 문서"

	m2 := tgMessage(42, -7, "")
	m2.MessageID = 2
	m2.Document = &tgbotapi.Document{FileID: "b", FileName: "two.html"}

	adapter.albumBuffer["g1"] = []*tgbotapi.Message{m1, m2}
	adapter.processAlbum("g1", -7)

	select {
	case ev := <-adapter.msgChan:
		for _, want := range []string{"[첨부파일:", "[원본파일명: one.md]", "[원본파일명: two.html]", "첫 번째 문서"} {
			if !strings.Contains(ev.Content, want) {
				t.Fatalf("album content missing %q: %q", want, ev.Content)
			}
		}
		if len(ev.AttachmentPaths) != 2 {
			t.Fatalf("expected 2 attachment paths, got %v", ev.AttachmentPaths)
		}
	default:
		t.Fatal("no album event produced")
	}
}
