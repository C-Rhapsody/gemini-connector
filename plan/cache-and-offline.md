# Gemini Connector: Cache & Offline Strategy (Hybrid AI)

## 1. 개요 (Overview)
본 계획은 `llama.cpp`와 **Gemma 4 (.gguf)** 모델을 Go 바이너리에 임베딩하여, 인터넷 연결 여부에 상관없이 가동 가능한 **하이브리드 AI 구조**를 설계하는 것을 목표로 한다. 

## 2. 핵심 아키텍처 (Core Architecture)

### 2.1 로컬 엔진 임베딩 (Embedded Local Engine)
- **llama.cpp**: OS별로 빌드된 `llama.cpp` 실행 파일을 Go의 `//go:embed` 기능을 사용하여 바이너리 내부에 포함한다.
- **Gemma 4 (.gguf)**: 경량화된 .gguf 포맷의 모델 파일을 함께 임베딩하거나, 최초 실행 시 지정된 경로로 자동 다운로드/추출하는 로직을 구현한다.
- **동적 추출**: 바이너리 실행 시 시스템 임시 디렉토리에 실행 파일과 모델을 추출하여 즉시 가동 준비를 마친다.

### 2.2 하이브리드 라우팅 로직 (Tiered Response Logic)
메시지가 인입되면 설정된 모드에 따라 다음과 같이 처리한다.

#### **A. 온라인 모드 (Online Mode)**
1. **1차 판별 (Local First)**: 모든 메시지는 먼저 로컬의 **Gemma 4**에게 전달된다.
2. **복잡도 분석 (Processing Threshold)**: Gemma 4가 처리할 수 있는 수준은 다음과 같이 정의한다.
   - **Gemma 4 처리 대상**: 단순 인사, 날짜/시간 확인, 시스템 상태 보고, 단답형 상식, 정해진 페르소나에 따른 일상 대화.
   - **Gemini Pro 전송 대상**: 복잡한 코드 분석, 논리적 추론, 최신 외부 정보 검색, 다단계 작업 수행.
3. **선별적 응답**: 
   - Gemma 4가 처리 가능한 수준인 경우: 즉시 로컬에서 응답하고 종료.
   - Gemma 4가 처리하기 벅찬 복잡한 논리나 최신 정보가 필요한 경우: 원본 메시지를 **Gemini 3.1 Pro**에게 전달하여 최종 응답을 생성한다.

### 2.3 클라우드 장애 대응 (Cloud Failover Logic)
Gemini API 호출 시 서버 용량 부족(No capacity available - 429) 등의 이유로 실패할 경우의 대응책을 수립한다.
- **지능형 재시도**: `429 Too Many Requests` 에러 수신 시 지수 백오프(Exponential Backoff)를 적용하여 최대 3회 재시도한다.
- **로컬 폴백 (Local Fallback)**: 모든 재시도가 실패하거나 서버 장애가 지속될 경우, 해당 메시지를 즉시 로컬 **Gemma 4**로 우회시켜 "서버 부하로 인해 로컬 엔진이 대신 응답함"을 고지하고 답변을 생성한다.

## 3. 기술적 해결 과제 (Technical Challenges)
1. **강제 로컬 처리**: 외부 네트워크 연결이 끊기거나 사용자가 오프라인 모드를 활성화한 경우, 모든 메시지는 **Gemma 4**가 단독으로 처리한다.
2. **성능 최적화**: 로컬 리소스(CPU/GPU) 상황에 맞춰 모델의 양자화(Quantization) 수준을 조정한다.

## 3. 기술적 해결 과제 (Technical Challenges)

### 3.1 바이너리 크기 최적화
- 모델 파일(.gguf)의 크기가 수 GB에 달할 수 있으므로, 전체를 `embed` 할지 아니면 핵심 엔진만 포함하고 모델은 별도 관리할지에 대한 벤치마크가 필요하다.

### 3.2 llama.cpp 제어
- Go 언어에서 `os/exec`를 통해 추출된 `llama.cpp`와 통신(stdio 또는 로컬 서버 방식)하는 인터페이스를 구현한다.

### 3.3 응답 일관성 유지
- 로컬 모델(Gemma)과 클라우드 모델(Gemini)이 동일한 페르소나(`personality.md`)와 운영 지침(`gemini.md`)을 준수하도록 로컬 프롬프트를 정교하게 튜닝한다.

## 4. 향후 추진 단계 (Roadmap)
1. **[Step 1] Environment Setup**: 각 OS별(Windows/Linux) llama.cpp 바이너리 확보 및 임베딩 테스트.
2. **[Step 2] Routing Engine**: 메시지 복잡도를 판별하여 Gemini와 Gemma 사이를 중계하는 라우팅 코드 구현.
3. **[Step 3] Offline UI/UX**: 오프라인 상태임을 사용자에게 알리고 로컬 엔진으로 전환하는 알림 로직 추가.

---
*이 계획은 클라우드 의존도를 낮추고 응답 속도를 혁신적으로 개선하며, 극한의 환경에서도 AI 비서의 연속성을 보장하기 위함이다.*
