# Microsoft Teams 환경변수 가이드

## 필요 환경변수

| 변수명 | 필수 | 설명 |
|--------|------|------|
| `TEAMS_TENANT_ID` | 필수 | Azure AD 테넌트 ID |
| `TEAMS_APP_ID` | 필수 | Azure AD 앱 등록 클라이언트 ID |
| `TEAMS_APP_SECRET` | 필수 | Azure AD 앱 클라이언트 시크릿 |
| `TEAMS_CHAT_ID` | 필수 | 모니터링 대상 Teams 채팅 ID |

---

## 1단계: Azure AD 앱 등록

1. [Azure Portal](https://portal.azure.com) 접속 → **Azure Active Directory** → **앱 등록** → **새 등록**
2. 앱 이름 입력 (예: `Gemini Connector`)
3. 지원되는 계정 유형: **이 조직 디렉터리의 계정만** 선택
4. 리디렉션 URI: 비워둠 (사용하지 않음)
5. **등록** 클릭

### TEAMS_APP_ID 확인
- 등록 완료 후 **개요** 페이지에서 **애플리케이션(클라이언트) ID** 복사

### TEAMS_TENANT_ID 확인
- 같은 **개요** 페이지에서 **디렉터리(테넌트) ID** 복사

---

## 2단계: 클라이언트 시크릿 생성

1. 앱 등록 페이지 → **인증서 및 암호** → **새 클라이언트 암호**
2. 설명 입력 (예: `gemini-connector`) 및 만료 기간 선택
3. **추가** 클릭
4. 생성된 **값**(Value)을 즉시 복사 — 이 페이지를 떠나면 다시 확인 불가

### TEAMS_APP_SECRET
- 위에서 복사한 클라이언트 암호 값

---

## 3단계: Microsoft Graph API 권한 설정

1. 앱 등록 페이지 → **API 권한** → **권한 추가**
2. **Microsoft Graph** → **애플리케이션 권한** 선택
3. 다음 권한 추가:
   - `Chat.ReadWrite.All` — 채팅 메시지 읽기 및 전송
4. **관리자 동의 부여** 버튼 클릭 (테넌트 관리자 권한 필요)

> **참고:** 애플리케이션 권한으로 채팅 메시지에 접근하려면 Microsoft 365 비즈니스 라이선스가 필요합니다.

---

## 4단계: Teams Chat ID 확인

### 방법 1: Graph Explorer 활용

1. [Graph Explorer](https://developer.microsoft.com/en-us/graph/graph-explorer) 접속 후 로그인
2. 다음 쿼리 실행:
   ```
   GET https://graph.microsoft.com/v1.0/me/chats
   ```
3. 응답에서 원하는 채팅의 `id` 값 확인 (예: `19:abc123...@thread.v2`)

### 방법 2: Teams 웹에서 URL 추출

1. [Teams 웹](https://teams.microsoft.com) 접속
2. 대상 채팅방 진입
3. 브라우저 URL에서 채팅 ID 추출:
   ```
   https://teams.microsoft.com/l/chat/19%3Aabc123...%40thread.v2/...
   ```
4. URL 디코딩: `%3A` → `:`, `%40` → `@`
5. 최종 Chat ID: `19:abc123...@thread.v2`

### 방법 3: PowerShell (Microsoft Graph 모듈)

```powershell
Install-Module Microsoft.Graph -Scope CurrentUser
Connect-MgGraph -Scopes "Chat.Read"
Get-MgChat | Select-Object Id, Topic, ChatType
```
