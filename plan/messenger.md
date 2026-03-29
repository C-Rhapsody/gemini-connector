# Gemini Connector: Multi-Messenger Expansion Plan

## 1. 개요 (Overview)
현재 텔레그램(Telegram) 전용으로 설계된 `gemini-connector`를 확장하여 **Slack, Discord, Microsoft Teams** 등 다양한 엔터프라이즈 및 커뮤니티 메신저를 통합 지원하기 위한 기술적 계획을 수립한다.

## 2. 아키텍처 설계 방향 (Architecture Strategy)
기존의 메인 루프가 텔레그램에 종속된 구조를 탈피하여, 각 메신저를 '어댑터(Adapter)' 형태로 연결하는 **인터페이스 기반 구조**로 전환한다.

### 2.1 공통 메시지 인터페이스 (Universal Messenger Interface)
Go 언어의 인터페이스를 활용하여 메신저별 고유 로직을 추상화한다.
- `Init()`: 봇 인증 및 세션 초기화
- `Listen()`: 메시지 수신 채널 오픈
- `Send(targetID string, text string)`: 메시지 전송
- `GetFile(fileID string) (localPath string, err error)`: 미디어 파일 다운로드

### 2.2 통합 메시지 버스 (Internal Message Bus)
메신저 종류에 상관없이 내부적으로는 동일한 데이터 구조(`InternalMessage`)로 변환하여 Gemini CLI에게 전달한다.
- `Platform`: (Telegram | Slack | Discord | Teams)
- `UserID`: 각 플랫폼의 고유 사용자 식별자
- `ChatID`: 각 플랫폼의 채팅방 식별자
- `Content`: 텍스트 또는 파일 경로

## 3. 메신저별 연동 세부 계획 (Implementation Details)

### 3.1 Telegram (Completed / Existing)
- **연동 방식**: **Long Polling**
  - 현재 구현 완료된 방식으로, `GetUpdates` API를 주기적으로 호출하여 실시간 메시지를 수신함.
- **주요 라이브러리**: `github.com/go-telegram-bot-api/telegram-bot-api/v5`
- **현재 상태**: 단일 플랫폼 모드로 운영 중이며, 향후 공통 인터페이스(Phase 1)로 리팩토링 대상임.

### 3.2 Slack (Enterprise Standard)
- **연동 방식**: **Socket Mode** (추천) 또는 Webhook
  - Socket Mode 사용 시 외부로 노출된 공개 HTTPS 엔드포인트 없이도 방화벽 내부에서 가동 가능하여 보안에 유리함.
- **주요 라이브러리**: `github.com/slack-go/slack`
- **특이사항**: 슬랙 특유의 'App-level Token'과 'Bot Token' 이중 인증 구조 반영 필요.

### 3.3 Discord (Community Standard)
- **연동 방식**: **WebSocket Gateway**
  - 실시간 이벤트 스트리밍 방식인 Gateway API를 사용하여 고속 응답 구현.
- **주요 라이브러리**: `github.com/bwmarrin/discordgo`
- **특이사항**: 풍부한 Embed 메시지 기능을 활용하여 `gemini-cli`의 분석 결과를 더 미려하게 출력 가능.

### 3.4 Microsoft Teams (Collaboration Standard)
- **연동 방식**: **Bot Framework SDK** (또는 Outgoing Webhook)
  - 가장 까다로운 보안 및 엔드포인트 요구사항을 가짐. 
  - Azure Bot Service를 통하거나, `ngrok` 등을 활용한 로컬 터널링/전용 HTTPS 서버 가동 로직 추가 필요.
- **특이사항**: Adaptive Cards 포맷을 지원하여 복잡한 데이터 리포트를 시각화하기에 적합.

## 4. 설정 및 환경 변수 확장 (.env)
각 플랫폼별 활성화 여부 및 토큰을 독립적으로 관리하도록 `.env` 구조를 확장한다.
```ini
# Global
ACTIVE_MESSENGERS=telegram,slack,discord,teams

# Telegram (Existing)
TELEGRAM_BOT_TOKEN=...

# Slack
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...

# Discord
DISCORD_BOT_TOKEN=...

# Teams
TEAMS_APP_ID=...
TEAMS_APP_PASSWORD=...
```

## 5. 세션 관리 전략 (Multi-Platform Session)
- **ID 매핑**: `Platform + UserID`를 조합하여 Gemini CLI의 `--resume <UUID>`와 매핑한다. 
- 한 사용자가 슬랙과 디스코드에서 동시에 말을 걸어도 동일한 AI 맥락을 유지하도록 설계하거나, 플랫폼별로 격리된 세션을 제공하는 옵션을 구현한다.

## 6. 향후 추진 단계 (Roadmap)
1. **[Phase 1] Core Refactoring**: `main.go`에서 텔레그램 로직을 별도 패키지로 분리하고 인터페이스 정의.
2. **[Phase 2] Adapter Implementation**: Slack(Socket Mode) -> Discord -> Teams 순으로 어댑터 구현.
3. **[Phase 3] Shared Resource Management**: 미디어 다운로드 폴더 및 로그 시스템 통합 관리.

---
*이 계획서는 gemini-connector의 확장성을 보장하고 멀티 메신저 시대를 대비하기 위한 초석으로 활용된다.*
