# Gemini Telegram Connector (Event-Driven Controller)

> "가난한 자를 위한 openclaw"

이 프로젝트는 Go 언어로 작성된 독립적인 텔레그램 커넥터 프로그램으로, 텔레그램과 **agy (Antigravity CLI)**를 연결하는 **이벤트 주도형 컨트롤러(Event-Driven Controller)**입니다. 
텔레그램 메시지가 인입될 때만 단발성으로 AI를 깨우는 극도로 가볍고 안정적인 구조를 가집니다.

## 필수 요구 사항 (Prerequisites)

이 커넥터를 빌드하고 실행하기 위해서는 시스템에 다음 소프트웨어가 설치되어 있어야 합니다.
-   **Go:** 커넥터 소스 코드를 컴파일하기 위해 필요합니다. (v1.25 이상 권장)
-   **Git:** Go 패키지 의존성을 다운로드하기 위해 필요합니다.
-   **agy (Antigravity CLI):** 커넥터가 백그라운드에서 호출할 실제 AI 에이전트입니다.
    -   Windows: `irm https://antigravity.google/cli/install.ps1 | iex`
    -   Linux/macOS: `curl -fsSL https://antigravity.google/cli/install.sh | bash`

## 주요 기능 (Features)

-   **Event-Driven 아키텍처:** 커넥터가 텔레그램 롱폴링을 전담하며, 메시지를 수신하는 즉시 `os/exec`를 통해 `agy`를 백그라운드에서 트리거합니다. (AI의 무한 대기 불필요)
-   **세션 상태 유지 (Stateful):** agy의 `--conversation <ID>` 기능을 활용하여, 프로세스가 켜지고 꺼짐을 반복해도 이전 대화의 맥락(Context)을 완벽하게 기억합니다.
-   **마크다운 → 텔레그램 HTML 변환:** agy가 반환하는 마크다운 응답을 텔레그램 HTML 포맷으로 자동 변환합니다. 굵게/기울임/인라인 코드/코드 블록/링크/인용구는 태그로 변환되고, 제목·목록·이미지는 텔레그램에서 지원하는 형태로 대체됩니다. HTML 전송이 실패하면 일반 텍스트로 자동 폴백(fallback)합니다.
-   **다중 미디어(앨범) 버퍼링:** 텔레그램에서 여러 장의 사진이나 파일이 동시에 전송될 경우, 2초간의 디바운스(Debounce) 버퍼링을 거쳐 단 하나의 통합 프롬프트로 AI에게 전달합니다.
-   **지능형 재시도 및 방어 로직:** 텔레그램 API의 `429 Too Many Requests` (Rate Limit) 에러를 감지하고 `Retry-After` 헤더를 분석하여 안전하게 재시도합니다.
-   **메시지 외부화 (Externalization):** 커넥터가 출력하는 모든 환영 메시지 및 에러 문구는 `messages.json` 파일에서 관리되므로 소스 코드 수정 없이 문구 변경이 가능합니다.
-   **대화형 세션 선택 (TUI Helper):** `.env` 파일에 `AGY_CONVERSATION_ID`가 없을 경우, agy의 로컬 세션 캐시(`~/.gemini/antigravity-cli/cache/last_conversations.json`)를 읽어 워크스페이스별 대화 목록을 페이지 단위(10개씩)로 보여줍니다. 사용자는 번호 입력만으로 간편하게 세션을 등록할 수 있으며, `[c]`로 새 대화를 만들거나 `[m]`으로 ID를 직접 입력할 수도 있습니다.

## 프로젝트 구조 (Directory Structure)

소스 코드와 실행 파일, 그리고 데이터가 명확히 분리되어 관리됩니다.

```text
[Project Root]/
├── .gemini/             # Gemini/agy 공용 설정 폴더
│   ├── settings.json    # agy 설정 파일
│   ├── gemini.md        # AI의 핵심 시스템 프롬프트 및 가동 원칙 정의 파일
│   └── personality.md   # AI의 페르소나(정체성 및 말투) 설정 파일
└── golang/gemini-connector/
    ├── src/
    │   ├── main.go            # 커넥터의 핵심 소스 코드
    │   ├── gemini.go          # agy CLI 호출 및 JSON 응답 파싱
    │   ├── session.go         # 대화형 세션 선택 TUI + 새 대화 생성
    │   ├── telegram.go        # 텔레그램 어댑터 (롱폴링, 미디어, 청크 분할)
    │   ├── telegram_html.go   # 마크다운 → 텔레그램 HTML 변환 (goldmark)
    │   ├── teams.go           # Teams 어댑터 (선택)
    │   ├── go.mod             # Go 패키지 의존성 파일
    │   ├── .env               # 환경 변수 (토큰, Chat ID, 대화 ID) - 실행 시 참조
    │   └── messages.json      # 외부화된 안내/에러 문구 템플릿 - 실행 시 참조
    ├── bin/
    │   ├── gemini-connector_windows_x64.exe # 컴파일된 실행 파일 (Windows 64bit)
    │   ├── gemini-connector_linux_x64       # 컴파일된 실행 파일 (Linux 64bit)
    │   └── bot.log            # 커넥터 구동 시 생성되는 실행 및 에러 로그
    └── downloads/             # 텔레그램으로 수신된 미디어 파일(이미지, 음성 등) 임시 저장소
```

## 설치 및 설정 (Setup)

1.  **텔레그램 봇(Bot) 토큰 발급:**
    -   텔레그램에서 `@BotFather`와 대화하여 새 봇을 생성하고 `TELEGRAM_BOT_TOKEN`을 발급받습니다.

2.  **agy 설치 및 인증:**
    -   공식 설치 스크립트로 agy를 설치합니다. (Windows: `irm https://antigravity.google/cli/install.ps1 | iex`)
    -   터미널에서 `agy`를 한 번 실행하여 로그인 인증을 완료합니다.

3.  **대화 세션 설정 (Conversation ID):**
    -   **자동 설정 (권장):** 커넥터를 실행하면 자동으로 캐시된 대화 목록을 불러오는 TUI 헬퍼가 실행됩니다. 목록에서 번호를 선택하면 ID가 자동으로 `.env`에 저장됩니다. `[c]`를 누르면 새 대화가 생성됩니다.
    -   **수동 설정:** 터미널에서 agy로 대화를 생성한 뒤 반환되는 `conversation_id`를 직접 입력합니다.

4.  **환경 설정 (.env):**
    -   `src/` 폴더 내부에 `.env` 파일을 생성하거나, 커넥터를 최초 1회 실행하여 설정 마법사를 띄웁니다.
    -   설정 마법사에서 텔레그램 토큰을 입력하고, 이어서 나타나는 **대화 선택 화면**에서 사용할 대화를 고르면 모든 설정이 완료됩니다.
    ```ini
    ACTIVE_MESSENGERS=telegram
    TELEGRAM_BOT_TOKEN=your_telegram_bot_token
    TELEGRAM_CHAT_ID=your_chat_id
    AGY_CONVERSATION_ID=자동_또는_수동_입력_ID
    ```

5.  **AI 성격 및 가동 원칙 설정 (선택):**
    -   `.gemini/` 폴더 내에 제공된 `gemini.md_sample`과 `personality.md_sample` 파일을 참고하십시오.
    -   해당 파일들의 내용을 본인의 목적에 맞게 수정한 뒤, 파일명에서 `_sample`을 제거(`gemini.md`, `personality.md`로 이름 변경)하여 저장하시면 AI가 해당 규칙을 최우선으로 따르게 됩니다.

## 다운로드 및 실행 (Installation & Run)

본 커넥터는 직접 소스 코드를 빌드하여 사용하거나, 이미 빌드된 실행 파일을 다운로드하여 곧바로 사용할 수 있습니다.

### 방법 1: GitHub Releases에서 다운로드 (가장 쉬운 방법)
1. 프로젝트의 [Releases] 페이지로 이동합니다.
2. 본인의 운영체제(Windows, Linux 등)에 맞는 실행 파일(`gemini-connector_...`)을 다운로드하여 `golang/gemini-connector/bin/` 폴더 또는 원하는 곳에 배치합니다.

### 방법 2: 직접 빌드 (Build from Source)
Go 언어가 설치되어 있다면 프로젝트 구조에 맞춰 `src` 폴더의 소스를 직접 컴파일할 수 있습니다.

```bash
cd golang/gemini-connector/src
go build -o ../bin/gemini-connector_windows_x64.exe
```

### 실행 (Run)
컴파일하거나 다운로드한 실행 파일을 독립적으로 백그라운드에 실행해 두기만 하면 됩니다.

```bash
cd golang/gemini-connector/bin
./gemini-connector_windows_x64.exe
```
(팁: 서버 환경에서는 쉘을 점유하지 않도록 `Start-Process`(Windows)나 `nohup`(Linux) 등을 활용하여 백그라운드로 구동하십시오.)

## ⚠️ 주의 및 면책 조항 (Disclaimer)

본 커넥터는 사용자의 편의성과 완전한 자동화를 위해 **agy를 `--dangerously-skip-permissions` 플래그로 실행**합니다. 이는 AI가 판단한 모든 도구 사용 및 로컬 파일 시스템 제어(수정, 삭제 등) 권한이 사용자의 사전 승인(Confirm) 없이 즉시 실행됨을 의미합니다.

*   AI의 환각(Hallucination)이나 잘못된 판단으로 인해 발생할 수 있는 데이터 손실, 시스템 파일 변조, 보안 취약점 노출 등 **어떠한 형태의 직간접적 피해에 대해서도 개발자 및 기여자는 일절 책임을 지지 않습니다.**
*   이 코드를 실행하고 결과물을 사용하는 데 따르는 **모든 책임과 위험은 전적으로 사용자 본인에게 귀속됩니다.** 안전한 샌드박스 환경이나 제한된 권한의 컨테이너 내에서 구동하시길 강력히 권장합니다.
