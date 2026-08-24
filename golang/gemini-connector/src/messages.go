package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// --- Localizable messages ---

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
	ImageTranslateTemplate: "Translate the following request into a detailed English prompt for a text-to-image model. Keep all visual details, style and composition. The final prompt must be at most 750 characters long. Reply with ONLY the English prompt text - no quotes, no explanations:\n\n%s",
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
