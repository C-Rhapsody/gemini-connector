# Gemini Telegram Connector (Event-Driven Controller)

> "가난한 자를 위한 openclaw"

이 프로젝트는 Go 언어로 작성된 독립적인 텔레그램 커넥터 프로그램으로, 텔레그램과 **Gemini CLI**를 연결하는 **이벤트 주도형 컨트롤러(Event-Driven Controller)**입니다. 
텔레그램 메시지가 인입될 때만 단발성으로 AI를 깨우는 극도로 가볍고 안정적인 구조를 가집니다.

## 필수 요구 사항 (Prerequisites)

이 커넥터를 빌드하고 실행하기 위해서는 시스템에 다음 소프트웨어가 설치되어 있어야 합니다.
-   **Go:** 커넥터 소스 코드를 컴파일하기 위해 필요합니다. (v1.25 이상 권장)
-   **Git:** Go 패키지 의존성을 다운로드하기 위해 필요합니다.
-   **Gemini CLI:** 커넥터가 백그라운드에서 호출할 실제 AI 에이전트입니다. (`npm install -g @google/gemini-cli` 등으로 설치)

## 주요 기능 (Features)

-   **Event-Driven 아키텍처:** 커넥터가 텔레그램 롱폴링을 전담하며, 메시지를 수신하는 즉시 `os/exec`를 통해 `gemini-cli`를 백그라운드에서 트리거합니다. (AI의 무한 대기 불필요)
-   **세션 상태 유지 (Stateful):** Gemini CLI의 `--resume <UUID>` 기능을 활용하여, 프로세스가 켜지고 꺼짐을 반복해도 이전 대화의 맥락(Context)을 완벽하게 기억합니다.
-   **다중 미디어(앨범) 버퍼링:** 텔레그램에서 여러 장의 사진이나 파일이 동시에 전송될 경우, 2초간의 디바운스(Debounce) 버퍼링을 거쳐 단 하나의 통합 프롬프트로 AI에게 전달합니다.
-   **지능형 재시도 및 방어 로직:** 텔레그램 API의 `429 Too Many Requests` (Rate Limit) 에러를 감지하고 `Retry-After` 헤더를 분석하여 안전하게 재시도합니다.
-   **메시지 외부화 (Externalization):** 커넥터가 출력하는 모든 환영 메시지 및 에러 문구는 `messages.json` 파일에서 관리되므로 소스 코드 수정 없이 문구 변경이 가능합니다.

## 프로젝트 구조 (Directory Structure)

새로운 아키텍처에 따라 소스 코드와 실행 파일, 그리고 데이터가 명확히 분리되어 관리됩니다.

```text
[Project Root]/
├── .gemini/             # Gemini CLI의 전역 설정 및 세션 데이터가 저장되는 폴더
│   ├── settings.json    # Gemini CLI 환경 설정 파일
│   ├── gemini.md        # AI의 핵심 시스템 프롬프트 및 가동 원칙 정의 파일
│   └── personality.md   # AI의 페르소나(정체성 및 말투) 설정 파일
└── golang/gemini-connector/
    ├── src/
    │   ├── main.go          # 커넥터의 핵심 소스 코드
    │   ├── go.mod           # Go 패키지 의존성 파일
    │   ├── .env             # 환경 변수 (토큰, Chat ID, 세션 UUID) - 실행 시 참조
    │   └── messages.json    # 외부화된 안내/에러 문구 템플릿 - 실행 시 참조
    ├── bin/
    │   ├── gemini-connector.exe # 컴파일된 순수 실행 파일 (이곳에서 커넥터 구동)
    │   └── bot.log          # 커넥터 구동 시 생성되는 실행 및 에러 로그
    └── downloads/           # 텔레그램으로 수신된 미디어 파일(이미지, 음성 등) 임시 저장소
```

## 설치 및 설정 (Setup)

1.  **텔레그램 봇(Bot) 토큰 발급:**
    -   텔레그램에서 `@BotFather`와 대화하여 새 봇을 생성하고 `TELEGRAM_BOT_TOKEN`을 발급받습니다.

2.  **Gemini CLI 세션 생성 (UUID 획득):**
    -   터미널에서 텔레그램 전용으로 사용할 새로운 세션을 생성합니다.
        ```bash
        gemini -y -p "너는 나의 텔레그램 비서다."
        ```
    -   세션 목록을 조회하여 방금 생성된 세션의 고유 UUID를 복사합니다.
        ```bash
        gemini --list-sessions
        ```

3.  **환경 설정 (.env):**
    -   `src/` 폴더 내부에 `.env` 파일을 생성하거나, 커넥터를 최초 1회 실행하여 설정 마법사를 띄웁니다.
    -   다음과 같이 복사한 UUID와 토큰을 기입합니다.
    ```ini
    TELEGRAM_BOT_TOKEN=your_telegram_bot_token
    TELEGRAM_CHAT_ID=your_chat_id
    GEMINI_SESSION_UUID=your_gemini_session_uuid_here
    ```

## 사용법 (Usage)

### 빌드 (Build)
프로젝트 구조에 맞춰 `src` 폴더의 소스를 컴파일하여 `bin` 폴더에 실행 파일을 생성합니다.

```bash
cd golang/gemini-connector/src
go build -o ../bin/gemini-connector.exe
```

### 실행 (Run)
컴파일된 실행 파일을 독립적으로 백그라운드에 실행해 두기만 하면 됩니다.

```bash
cd golang/gemini-connector/bin
./gemini-connector.exe
```
(팁: 서버 환경에서는 쉘을 점유하지 않도록 `Start-Process`(Windows)나 `nohup`(Linux) 등을 활용하여 백그라운드로 구동하십시오.)

## ⚠️ 주의 및 면책 조항 (Disclaimer)

본 커넥터는 사용자의 편의성과 완전한 자동화를 위해 **Gemini CLI를 `YOLO(-y)` 모드로 강제 실행**합니다. 이는 AI가 판단한 모든 도구 사용 및 로컬 파일 시스템 제어(수정, 삭제 등) 권한이 사용자의 사전 승인(Confirm) 없이 즉시 실행됨을 의미합니다.

*   AI의 환각(Hallucination)이나 잘못된 판단으로 인해 발생할 수 있는 데이터 손실, 시스템 파일 변조, 보안 취약점 노출 등 **어떠한 형태의 직간접적 피해에 대해서도 개발자 및 기여자는 일절 책임을 지지 않습니다.**
*   이 코드를 실행하고 결과물을 사용하는 데 따르는 **모든 책임과 위험은 전적으로 사용자 본인에게 귀속됩니다.** 안전한 샌드박스 환경이나 제한된 권한의 컨테이너 내에서 구동하시길 강력히 권장합니다.