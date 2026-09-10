# OpenAI 호환 Chat Completions API 어댑터

`gemini-connector`는 로컬 개발 도구 및 기존 OpenAI SDK 호환 애플리케이션과의 연동을 위해 OpenAI 호환 REST API 어댑터를 제공합니다.

---

## 1. 주요 특징 및 보안 모델

- **Opt-in 기본 비활성화**:
  - 기본적으로 API 서버는 비활성화되어 있으며, 명시적으로 `--openai-api` 플래그를 지정해야만 시작됩니다.
  - 리스너는 외부 네트워크에 노출되지 않고 루프백 인터페이스(`127.0.0.1:<--port>`)에만 바인딩됩니다.
- **API 키 인증 (Bearer Token)**:
  - 모든 API 요청은 HTTP Header에 `Authorization: Bearer <API_KEY>`를 포함해야 합니다.
  - API 키는 안전을 위해 최소 32 UTF-8 바이트 이상의 길이를 요구합니다.
- **API 키 주입 우선순위**:
  1. CLI 플래그: `--api-key <value>` (가장 높은 우선순위, 프로세스 목록/셸 히스토리 노출 위험으로 비권장)
  2. 환경 변수: 상속된 OS 환경변수 `OPENAI_COMPAT_API_KEY`
  3. `.env` 파일: `--env <path>` 또는 기본 경로 (`<실행 파일 디렉터리>/../src/.env`)의 `OPENAI_COMPAT_API_KEY`
  - 공백이거나 비어 있는 경우 Fail-closed 정책에 따라 즉시 시작이 거부됩니다.
- **독립형 키 생성기 (`--api-keygen`)**:
  - `gemini-connector --api-keygen`
  - 32바이트 암호학적 난수를 생성하여 URL-safe Base64 형태로 표준 출력에 출력하고 정상 종료(exit 0)합니다.
  - 다른 플래그와 함께 실행 시 격리 검증을 통해 즉시 에러가 발생합니다.

---

## 2. CLI 실행 옵션

| 플래그 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `--openai-api` | bool | `false` | OpenAI 호환 로컬 API 서버 활성화 |
| `--api-key` | string | `""` | API 인증용 Bearer 토큰 (비권장, 환경변수/`.env` 권장) |
| `--api-keygen` | bool | `false` | 신규 32바이트 URL-safe Base64 API 키 단독 생성 후 종료 |
| `--port` | int | `49152` | 로컬 바인딩 포트 (`127.0.0.1:<port>`) |
| `--env` | string | `""` | `.env` 파일 경로 (미지정 시 `<exe>/../src/.env`) |

---

## 3. 지원 엔드포인트

### 3.1. `GET /v1/models`
Antigravity CLI(`agy`)의 모델 목록을 조회하여 반환합니다.
- 캐시 정책: 5분 TTL
- 필터링: `gemini-*` 접두사를 가진 지원 모델만 반환

#### 요청 예시
```bash
curl http://127.0.0.1:49152/v1/models \
  -H "Authorization: Bearer <YOUR_API_KEY>"
```

#### 응답 예시
```json
{
  "object": "list",
  "data": [
    {
      "id": "gemini-3.8-flash-high",
      "object": "model",
      "created": 1773302400,
      "owned_by": "google"
    },
    {
      "id": "gemini-3.1-pro-high",
      "object": "model",
      "created": 1773302400,
      "owned_by": "google"
    }
  ]
}
```

---

### 3.2. `POST /v1/chat/completions`
채팅 완성 요청을 처리합니다. 스트리밍(SSE) 및 논스트리밍(JSON)을 모두 지원하며, Hermes Agent 등 OpenAI 호환 클라이언트와의 상호운용성을 제공합니다.

#### 필드 처리 방식 매트릭스 (Field Treatment Matrix)

| 필드 | 처리 방식 | 설명 |
|---|---|---|
| `model` | **Mapped** | 필수. 카탈로그 검증 후 agy `--model`로 전달 |
| `messages` | **Mapped** | `developer`, `system`, `user`, `assistant`, `tool` 역할 지원. JSON 배열로 직렬화하여 agy 프롬프트로 전달 |
| `stream` | **Mapped** | `true` 시 agy `--output-format stream-json`을 사용하여 실시간 SSE 스트리밍 |
| `response_format` | **Mapped** | `text`: 일반 텍스트<br>`json_object`: `{"type":"object"}` 스키마 매핑<br>`json_schema`: 내부 스키마를 임시 파일로 격리 저장하여 agy `--json-schema`로 안전 전달 후 자동 삭제 |
| `stream_options.include_usage`<br>`include_usage` | **Mapped** | agy가 보고한 실제 토큰 사용량이 있을 경우 `[DONE]` 직전 usage 청크 전송. 측정값이 없으면 0을 날조하지 않고 생략 |
| `stop` | **Mapped** | 단일 문자열 또는 배열. 일치하는 시퀀스 직전에서 논스트리밍/스트리밍 텍스트 절단 및 `finish_reason: "stop"` 반환 |
| `max_tokens`<br>`max_completion_tokens` | **Normalized & Ignored** | 양수 정수 검증(0 이하 400 에러). 둘 다 있을 경우 `max_completion_tokens` 우선. **단, agy CLI에 출력 토큰 제한 옵션이 없어 upstream에 강제되지는 않음 (호환성 힌트로만 수용)** |
| `tools`<br>`tool_choice` | **Compatibility Fallback** | Hermes 등 도구 호출 클라이언트의 400 에러를 방지하기 위해 요청은 수용하되 **Text-only Completion Fallback**으로 동작 (가짜 tool_calls 미생성). 대화 기록의 `role: "tool"` 및 `tool_call_id`는 온전히 수용됨 |
| `functions`<br>`function_call` | **Normalized** | legacy 함수 호출을 `tools` / `tool_choice` 구조로 자동 정규화 후 동일한 Text-only Fallback 적용 |
| `parallel_tool_calls` | **Ignored** | 호환성 수용 후 no-op |
| `metadata`, `user` | **Ignored** | 호환성 수용 후 no-op. 감사 로그 및 프롬프트에 기록하지 않음 |
| `logprobs`, `top_logprobs` | **Ignored** | 호환성 수용 후 no-op. 가짜 확률값을 생성하지 않음 |
| `temperature`, `top_p`, `seed`<br>`presence_penalty`, `frequency_penalty` | **Ignored** | 호환성 수용 후 no-op |
| `n` (`n > 1`) | **Rejected (400)** | `n > 1`은 지원되지 않으며 400 에러 반환 |

#### 논스트리밍 요청 예시
```bash
curl http://127.0.0.1:49152/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <YOUR_API_KEY>" \
  -d '{
    "model": "gemini-3.8-flash-high",
    "messages": [
      {"role": "system", "content": "You are a concise assistant."},
      {"role": "user", "content": "Hello!"}
    ],
    "max_tokens": 100
  }'
```

#### 논스트리밍 응답 예시 (실제 토큰 usage 포함)
```json
{
  "id": "chatcmpl-0123456789abcdef",
  "object": "chat.completion",
  "created": 1773302400,
  "model": "gemini-3.8-flash-high",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I assist you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 15612,
    "completion_tokens": 102,
    "total_tokens": 15714,
    "completion_tokens_details": {
      "reasoning_tokens": 101
    }
  }
}
```
*(참고: 업스트림에서 실제 토큰 사용량이 제공되지 않는 경우 `usage` 필드는 가짜 0 토큰을 반환하지 않고 생략됩니다.)*

#### 스트리밍 요청 예시 (`stream_options.include_usage`)
```bash
curl -N http://127.0.0.1:49152/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <YOUR_API_KEY>" \
  -d '{
    "model": "gemini-3.8-flash-high",
    "messages": [
      {"role": "user", "content": "Count from 1 to 3"}
    ],
    "stream": true,
    "stream_options": {
      "include_usage": true
    }
  }'
```

#### 스트리밍 SSE 출력 예시
```
data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1773302400,"model":"gemini-3.8-flash-high","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1773302400,"model":"gemini-3.8-flash-high","choices":[{"index":0,"delta":{"content":"1, "},"finish_reason":null}]}

data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1773302400,"model":"gemini-3.8-flash-high","choices":[{"index":0,"delta":{"content":"2, 3"},"finish_reason":null}]}

data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1773302400,"model":"gemini-3.8-flash-high","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-0123456789abcdef","object":"chat.completion.chunk","created":1773302400,"model":"gemini-3.8-flash-high","choices":[],"usage":{"prompt_tokens":15612,"completion_tokens":102,"total_tokens":15714,"completion_tokens_details":{"reasoning_tokens":101}}}

data: [DONE]
```

---

## 4. 큐 및 자원 보호 (Resource Protection)

- **API 큐 최대 수용 한도 (`N=4`)**:
  - 텔레그램 봇 작업과의 충돌 및 과부하를 방지하기 위해 최대 4개의 대기/실행 API 작업만 허용합니다.
  - 대기열 초과 시 `429 Too Many Requests`를 반환합니다.
- **격리된 작업 취소 정책**:
  - 텔레그램 사용자(`/stop` 또는 신규 질의 등)의 세션 취소 동작은 진행 중인 API 호출을 중단시키지 않습니다.
- **Graceful Shutdown & Drain**:
  - 프로세스 종료 시그널 수신 시 활성 API 연결 완료를 위해 드레인 대기 상태로 전환되며, 신규 요청에 대해 `503 Service Unavailable`을 반환합니다.

---

## 5. 감사 로깅 (Audit Logging)

- **로그 저장 위치**: `<실행 파일 디렉터리>/../src/.openai_api.log`
- **용량 제한**: 10 MiB 초과 시 로깅이 안전하게 중단됩니다 (무한 증식 방지).
- **개인정보 보호 (PII)**:
  - 프롬프트 내용 및 응답 본문은 일체 기록하지 않습니다.
  - 타임스탬프, 클라이언트 IP, HTTP 메서드, 경로, 상태 코드, 소요 시간, 토큰 카운트, 분류된 User-Agent만 기록됩니다.
