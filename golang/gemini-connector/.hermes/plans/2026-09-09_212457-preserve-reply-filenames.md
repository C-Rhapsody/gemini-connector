# Gemini connector 파일명 오탐 방지 Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** 설명에 포함된 파일명·경로가 첨부파일 처리 때문에 삭제되지 않도록 한다.

**Architecture:** 답변 본문과 첨부파일 수집을 분리한다. 첨부 수집은 유지하되 응답 본문에 대한 filePathPattern + strings.ReplaceAll 삭제를 제거한다. Markdown 변환·분할·첨부 전송 및 정리 정책은 변경하지 않는다.

**Tech Stack:** Go, 기존 TelegramAdapter, Go testing.

---

## 범위와 확인 근거

프로젝트: C:/Users/bitflow/.gemini-bot/golang/gemini-connector

- src/telegram.go:290-295: AttachAfter가 지정되면 첨부 수집 결과와 무관하게 패턴에 맞는 모든 문자열을 삭제한다. 현재 소스를 다시 읽어 확인했다.
- src/gemini.go:187-190: 단독 파일명도 매칭하는 정규식이다.
- 앞선 조사에서 원문에는 _0000.mp4가 존재하고 삭제 처리 후 빈 백틱이 남는 현상이 재현되었다. 기존 Advisor는 이 원인 분석을 검토했으며, 이번 변경안 자체를 승인한 것은 아니다.
- 이번 요청은 기획만이다. 소스 수정, 빌드, 배포, 프로세스 재시작, Telegram 전송을 실행하지 않는다.

## 대안 비교

1. 권장: 본문 파일명/경로 삭제를 없앤다. 내용 보존을 확실히 하고 수정 범위가 작다. 첨부와 파일명 설명이 함께 보이고 로컬 경로도 원문에 있으면 그대로 표시되는 절충이 있다.
2. 비권장: 절대 경로만 매칭하도록 정규식을 강화한다. 상대 파일명 문제는 줄지만 설명용 절대 경로를 여전히 삭제한다. 파일 존재 확인도 설명과 첨부 의도를 구별하지 못한다.
3. 향후 선택: 구조화된 첨부 메타데이터를 사용한다. 사람이 읽는 본문은 건드리지 않고 명시적 첨부 표시용 필드만 별도 처리한다. 이번 결함 수정에는 과한 범위이다.

## 변경 계약

- 일반 설명, 인라인 코드, 코드 블록, 표, URL에 등장하는 파일명 및 경로를 첨부 처리 이유로 삭제하지 않는다.
- AttachAfter는 첨부 수집 활성화에만 사용한다.
- 실제 첨부가 발견되거나 전송돼도 본문의 관련 파일명은 유지한다.
- 첨부 수집/제외/전송/성공 후 정리 로직은 그대로 둔다.
- 본문 경로 숨김 기능을 새로 추가하지 않는다. 필요하다면 별도 요구사항으로 합의한다.

## Task 1: 백업 및 테스트 경계 확인

**Files:** src/telegram.go, src/telegram_deliverable_test.go, src/controller_test.go, src/telegram_html_test.go, src/telegram_split_test.go

1. 적용 승인을 받은 뒤 관련 파일과 기존 변경 상태를 읽는다.
2. 수정할 기존 파일의 타임스탬프 백업을 만들고 원본과 크기·SHA-256 일치를 확인한다.
3. TelegramAdapter.Send를 네트워크 없이 검증할 수 있는 기존 mock/transport 주입점을 확인한다. 없으면 최소 테스트용 transport seam을 설계한다. 테스트에서 운영 토큰·네트워크·실제 파일 삭제를 사용하지 않는다.

## Task 2: 전송 경계의 실패 회귀 테스트 작성

**Test:** src/telegram_deliverable_test.go 또는 새 src/telegram_reply_preservation_test.go

1. AttachAfter에 유효한 시각을 넣고 답변에 인라인 코드 _0000.mp4가 있는 fixture를 작성한다.
2. mock Telegram API로 Send가 보낸 text를 수집한다. HTML 출력에서 <code>_0000.mp4</code>가 보존되는지 검증한다. 원문과 HTML 전체가 바이트 단위로 같아야 한다고 검사하지 않는다.
3. 첨부 수집 결과가 비어 있을 때에도 본문이 보존되는지 검사한다.
4. 아래 회귀 사례를 table-driven test로 추가한다.
   - 단독 _0000.mp4, result.csv, report.pdf
   - 동일 파일명 반복, 접두사가 겹치는 이름
   - 한글·공백·@가 들어간 파일명
   - Windows 절대 경로, 상대 경로, URL
   - 인라인 코드, 코드 블록, 표 안의 파일명
   - AttachAfter가 zero인 경로와 nonzero인 경로
   - 실제 첨부 후보가 있는 경우에도 본문 보존 및 첨부 유지
   - 사용자 첨부 제외 동작 유지
5. src에서 go test ./... -run 'ReplyPreservation' -count=1 -v 실행. 새 테스트 함수명을 TestReplyPreservation으로 시작하도록 정한다. 수정 전에는 본문 보존 assertion이 실패해야 한다.

## Task 3: 최소 수정

**Modify:** src/telegram.go:284-296

1. collectDeliverables 호출은 유지한다.
2. filePathPattern.FindAllString + strings.ReplaceAll 본문 삭제 루프를 제거한다.
3. 첨부 처리 블록의 TrimSpace도 제거해 첨부 처리 자체가 본문을 변형하지 않게 한다. 다른 기존 렌더링 처리는 유지한다.
4. 주석을 첨부 수집과 본문 보존 정책에 맞게 갱신한다.
5. 새 회귀 테스트를 다시 실행해 통과를 확인한다.
6. filePathPattern 참조를 전체 검색한다. src/gemini.go와 src/gemini_test.go의 정규식/전용 테스트가 완전히 불필요해진 경우만 제거하며 다른 호출처가 있으면 남긴다. imports는 사용 여부를 확인하고 최소 정리한다.

## Task 4: 회귀 검증과 독립 검토

**Commands (src 작업 디렉터리):**
- go test ./... -count=1
- go vet ./...

기대 결과는 모두 exit 0이다. 이는 계획상 기준이지 이번 턴의 실행 결과가 아니다.

HTML 및 plain fallback 모두 파일명을 유지하고 긴 메시지 분할 후에도 내용이 남는지 기존 테스트와 새 fixture로 검증한다. 첨부 전송·제외·정리 테스트도 유지한다. 실제 미디어 대신 임시 테스트 디렉터리를 사용한다.

원인 분석의 이전 Advisor 판정을 변경안 승인으로 재사용하지 않는다. 운영 적용 전 최종 diff와 테스트 증거로 Advisor 검토를 수행한다. 검토 실패 또는 인증 문제 시 검토 미완료로 보고한다.

## Task 5: 별도 승인 후 적용/배포

최종 변경 diff와 검증 결과를 보고한다. 소스 변경과 실행 바이너리 배포는 별도 단계로 다룬다. 승인 전 프로세스를 중단하거나 재시작하지 않는다. 실행 파일 교체 전 현재 바이너리 백업 및 복구 경로를 확보한다. 실제 Telegram 전송 검증은 별도 승인된 채팅/텍스트에 한정한다.

## 완료 기준

- _0000.mp4를 비롯한 위 회귀 입력에서 첨부 처리에 따른 문자열 삭제가 없다.
- 실제 첨부 수집/전송 기능과 사용자 첨부 제외 기능이 유지된다.
- 모든 테스트 및 go vet 통과, 최종 변경안 검토 상태가 명확하다.
- 원본 백업과 롤백 경로 확보. 승인 범위 밖 변경이 없다.

## 위험과 열린 결정

- 원문에 포함된 로컬 경로가 그대로 보인다. 경로 숨김이 필수라면 구현 전 별도 정책을 정해야 한다.
- 이번 범위는 파일명 삭제 결함뿐이다. 첨부 오수집, 파일 정리 정책, 표 렌더링 문제를 함께 수정하지 않는다.
- 사용자 승인 전 구현하지 않는다.
