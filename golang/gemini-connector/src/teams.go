package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type TeamsAdapter struct {
	tenantID    string
	appID       string
	appSecret   string
	chatID      string
	msgs        *Messages
	msgChan     chan InternalMessage
	accessToken string
	tokenExpiry time.Time
	tokenMutex  sync.Mutex
	lastSeenTime time.Time
	httpClient  *http.Client
}

// OAuth2 token response
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// Graph API structures
type graphMessagesResponse struct {
	Value []graphMessage `json:"value"`
}

type graphMessage struct {
	ID              string     `json:"id"`
	CreatedDateTime string     `json:"createdDateTime"`
	Body            graphBody  `json:"body"`
	From            *graphFrom `json:"from"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphFrom struct {
	Application *graphIdentity `json:"application"`
	User        *graphIdentity `json:"user"`
}

type graphIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func NewTeamsAdapter(tenantID, appID, appSecret, chatID string, msgs *Messages) *TeamsAdapter {
	return &TeamsAdapter{
		tenantID:   tenantID,
		appID:      appID,
		appSecret:  appSecret,
		chatID:     chatID,
		msgs:       msgs,
		msgChan:    make(chan InternalMessage, 100),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (t *TeamsAdapter) Init() error {
	if err := t.refreshToken(); err != nil {
		return fmt.Errorf("teams auth failed: %v", err)
	}
	t.lastSeenTime = time.Now().UTC()
	log.Printf("Teams adapter initialized for chat: %s", t.chatID)
	return nil
}

func (t *TeamsAdapter) Listen() (<-chan InternalMessage, error) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			t.pollMessages()
		}
	}()
	return t.msgChan, nil
}

func (t *TeamsAdapter) Send(chatID string, text string) error {
	token, err := t.getToken()
	if err != nil {
		return fmt.Errorf("teams token error: %v", err)
	}

	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/chats/%s/messages", chatID)

	body := map[string]interface{}{
		"body": map[string]string{
			"contentType": "text",
			"content":     text,
		},
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("Teams send error (%d): %s", resp.StatusCode, string(respBody))
		return fmt.Errorf("teams send failed: %d", resp.StatusCode)
	}

	return nil
}

func (t *TeamsAdapter) StartTyping(chatID string) (stop func()) {
	// Graph API with application permissions does not support typing indicators
	return func() {}
}

func (t *TeamsAdapter) GetFile(fileID string) (string, error) {
	return "", fmt.Errorf("teams file download not yet supported")
}

// --- OAuth2 Token Management ---

func (t *TeamsAdapter) refreshToken() error {
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", t.tenantID)

	data := url.Values{
		"client_id":     {t.appID},
		"client_secret": {t.appSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
		"grant_type":    {"client_credentials"},
	}

	resp, err := t.httpClient.PostForm(tokenURL, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token request failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return err
	}

	t.tokenMutex.Lock()
	t.accessToken = tokenResp.AccessToken
	// 만료 1분 전에 갱신되도록 여유 확보
	t.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn-60) * time.Second)
	t.tokenMutex.Unlock()

	log.Println("Teams OAuth token refreshed.")
	return nil
}

func (t *TeamsAdapter) getToken() (string, error) {
	t.tokenMutex.Lock()
	if time.Now().Before(t.tokenExpiry) {
		token := t.accessToken
		t.tokenMutex.Unlock()
		return token, nil
	}
	t.tokenMutex.Unlock()

	if err := t.refreshToken(); err != nil {
		return "", err
	}

	t.tokenMutex.Lock()
	token := t.accessToken
	t.tokenMutex.Unlock()
	return token, nil
}

// --- Message Polling ---

func (t *TeamsAdapter) pollMessages() {
	token, err := t.getToken()
	if err != nil {
		log.Printf("Teams token error: %v", err)
		return
	}

	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/chats/%s/messages?$top=20", t.chatID)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("Teams poll request error: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		log.Printf("Teams poll error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Teams poll error (%d): %s", resp.StatusCode, string(body))
		return
	}

	var msgResp graphMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&msgResp); err != nil {
		log.Printf("Teams JSON decode error: %v", err)
		return
	}

	// Graph API returns newest first — collect new messages and reverse for chronological order
	var newMessages []graphMessage
	for _, msg := range msgResp.Value {
		created, err := time.Parse(time.RFC3339Nano, msg.CreatedDateTime)
		if err != nil {
			continue
		}
		if !created.After(t.lastSeenTime) {
			break // older messages follow, stop here
		}

		// Skip messages from the bot itself
		if msg.From != nil && msg.From.Application != nil && msg.From.Application.ID == t.appID {
			continue
		}

		newMessages = append(newMessages, msg)
	}

	// Process in chronological order (oldest first)
	for i := len(newMessages) - 1; i >= 0; i-- {
		msg := newMessages[i]

		created, _ := time.Parse(time.RFC3339Nano, msg.CreatedDateTime)
		if created.After(t.lastSeenTime) {
			t.lastSeenTime = created
		}

		content := msg.Body.Content
		if content == "" {
			continue
		}

		// Teams often sends HTML even for plain text
		if msg.Body.ContentType == "html" {
			content = stripHTMLTags(content)
			content = html.UnescapeString(content)
		}
		content = strings.TrimSpace(content)

		if content == "" {
			continue
		}

		var userID string
		if msg.From != nil && msg.From.User != nil {
			userID = msg.From.User.ID
		}

		t.msgChan <- InternalMessage{
			Platform: "teams",
			UserID:   userID,
			ChatID:   t.chatID,
			Content:  content,
		}
	}
}

func stripHTMLTags(s string) string {
	var result strings.Builder
	inTag := false
	for _, ch := range s {
		if ch == '<' {
			inTag = true
			continue
		}
		if ch == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(ch)
		}
	}
	return result.String()
}
