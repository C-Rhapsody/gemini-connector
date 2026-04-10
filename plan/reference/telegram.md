# Telegram 환경변수 가이드

> **⚠️ 보안 경고:** 발급받은 토큰이나 Chat ID를 이 문서 또는 기타 공개 파일에 직접 기록하지 마세요. 반드시 `.env` 파일에만 보관하고, `.env`가 `.gitignore`에 포함되어 있는지 확인하세요.

## 필요 환경변수

| 변수명 | 필수 | 설명 |
|--------|------|------|
| `TELEGRAM_BOT_TOKEN` | 필수 | 봇 API 토큰 |
| `TELEGRAM_CHAT_ID` | 선택 | 허용할 채팅방 ID (미설정 시 모든 채팅 수신) |

---

## TELEGRAM_BOT_TOKEN 발급

1. 텔레그램에서 **@BotFather** 검색 후 대화 시작
2. `/newbot` 명령어 입력
3. 봇 이름(display name) 입력 (예: `My Gemini Bot`)
4. 봇 유저네임(username) 입력 — 반드시 `Bot`으로 끝나야 함 (예: `my_gemini_bot`)
5. BotFather가 토큰을 반환함:
   ```
   Use this token to access the HTTP API:
   1234567890:ABCdefGHIjklMNOpqrsTUVwxyz
   ```
6. 해당 토큰을 `.env`의 `TELEGRAM_BOT_TOKEN`에 입력

### 토큰 재발급

기존 토큰이 노출되었거나 분실한 경우:
1. @BotFather에서 `/revoke` 명령어 입력
2. 대상 봇 선택
3. 새 토큰이 발급됨 (기존 토큰은 즉시 무효화)

---

## TELEGRAM_CHAT_ID 확인

### 방법 1: 봇에게 메시지 전송 후 API로 확인

1. 텔레그램에서 생성한 봇을 검색하여 `/start` 전송
2. 브라우저에서 다음 URL 접속:
   ```
   https://api.telegram.org/bot<YOUR_TOKEN>/getUpdates
   ```
3. JSON 응답에서 `"chat":{"id": 51867851}` 형태로 Chat ID 확인

### 방법 2: @userinfobot 활용

1. 텔레그램에서 **@userinfobot** 검색 후 대화 시작
2. 아무 메시지 전송
3. 봇이 본인의 `Id` (= Chat ID)를 반환

### 방법 3: 그룹 채팅의 Chat ID

1. 봇을 그룹에 초대
2. 그룹에서 아무 메시지 전송
3. 위 `getUpdates` API로 확인 — 그룹 Chat ID는 음수(예: `-1001234567890`)

> **Privacy Mode 참고:** 텔레그램 봇은 기본적으로 Privacy Mode가 활성화되어 있어, 그룹 채팅에서 `/`으로 시작하는 명령어와 봇을 직접 멘션한 메시지만 수신합니다. 모든 그룹 메시지를 수신하려면 @BotFather에서 `/setprivacy` → 대상 봇 선택 → `Disable`로 변경하세요.
