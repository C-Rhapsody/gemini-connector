package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// cronPlannerFunc generates candidate JSON via agy. Injectable for tests.
type cronPlannerFunc func(ctx context.Context, prompt string) (string, error)

// CronSurface is everything the cron subsystem (service + scheduler) may call
// on the messaging side: plain sends, typing state and rich interactions.
// Telegram implements it; tests stub it. Cron code never depends on a
// concrete adapter type.
type CronSurface interface {
	Send(chatID string, text string, opts ...SendOptions) error
	StartTyping(chatID string) (stop func())
	SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error
	AnswerCallbackQuery(callbackID, text string)
	EditMessage(chatID string, messageID int, text string, kb InlineKeyboard)
}

// CronService implements the /cron command surface: planning, validation,
// target selection, confirmation buttons, idempotency and the kill switch.
// It owns no scheduling logic itself; successful writes nudge the scheduler.
type CronService struct {
	store          *CronStore
	convID         func() string
	missingConvMsg string
	ui             CronSurface
	planner        cronPlannerFunc
	clock          cronClock
	sched          *CronScheduler
	admins         map[string]bool
}

func NewCronService(store *CronStore, convID func() string, missingConvMsg string, ui CronSurface, planner cronPlannerFunc, admins []string, sched *CronScheduler) *CronService {
	adminSet := make(map[string]bool, len(admins))
	for _, id := range admins {
		adminSet[strings.TrimSpace(id)] = true
	}
	return &CronService{
		store:          store,
		convID:         convID,
		missingConvMsg: missingConvMsg,
		ui:             ui,
		planner:        planner,
		clock:          realCronClock{},
		sched:          sched,
		admins:         adminSet,
	}
}

func (s *CronService) ownerOf(m InboundEvent) CronOwner {
	return CronOwner{Platform: m.Platform, ChatID: m.ChatID, UserID: m.UserID}
}

const (
	cronHelpText = `🗓 예약 작업 (/cron)

/cron <요청> - 자연어로 예약 작업 생성/수정 (agy가 JSON 후보를 만들고 버튼으로 확인)
/cron list - 등록된 예약 작업 목록
/cron kill - 예약 실행 전면 중지 (관리자)
/cron resume-all - 예약 실행 재개 (관리자)
/cron help - 이 도움말

예시:
/cron 매일 아침 9시에 AI 뉴스를 요약해줘
/cron 내일 오전 10시에 보고서 초안 목록을 정리해줘
/cron 매일 뉴스 요약 작업을 일시정지해줘`

	cronDisabledNotice = "⚠️ cron 기능이 비활성화되어 있습니다 (--cron-disabled)."

	cronNotAdminNotice = "⛔ 이 명령은 관리자 전용입니다.\nCRON_ADMIN_TELEGRAM_USER_IDS 환경변경에 Telegram 사용자 ID를 등록하세요."
)

// HandleMessage processes an inbound /cron command.
func (s *CronService) HandleMessage(m InboundEvent) {
	if m.Platform != "telegram" {
		s.ui.Send(m.ChatID, "⚠️ cron 기능은 현재 Telegram에서만 지원됩니다.", SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	owner := s.ownerOf(m)
	if !owner.complete() {
		s.audit(&owner, "reject_scope", "high", "missing user identity on /cron")
		s.ui.Send(m.ChatID, "❌ 사용자 식별 정보가 없어 cron 명령을 처리할 수 없습니다.", SendOptions{ReplyToMessageID: m.MessageID})
		return
	}

	args := strings.TrimSpace(m.Args)
	fields := strings.Fields(args)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "", "help":
		s.ui.Send(m.ChatID, cronHelpText, SendOptions{ReplyToMessageID: m.MessageID})
		return
	case "list":
		s.cmdList(m, owner)
		return
	case "kill":
		s.cmdKill(m, owner, true)
		return
	case "resume-all":
		s.cmdKill(m, owner, false)
		return
	case "status":
		s.cmdStatus(m, owner)
		return
	default:
		s.planFlow(m, owner, args)
		return
	}
}

func (s *CronService) cmdList(m InboundEvent, owner CronOwner) {
	jobs, err := s.store.ListJobs(owner)
	if err != nil {
		s.ui.Send(m.ChatID, fmt.Sprintf("⚠️ 목록 조회 실패: %v", err), SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	if len(jobs) == 0 {
		s.ui.Send(m.ChatID, "📭 등록된 예약 작업이 없습니다.\n/cron 매일 아침 9시에 … 형태로 새 작업을 만들 수 있습니다.", SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🗓 예약 작업 %d개\n\n", len(jobs)))
	for _, j := range jobs {
		b.WriteString(j.Summary() + "\n")
		for _, t := range j.Triggers {
			b.WriteString("   ⏲ " + describeStoredTrigger(t))
			if t.NextRunAt > 0 && t.FiredAt == 0 {
				b.WriteString(" · 다음: " + time.Unix(t.NextRunAt, 0).Local().Format("2006-01-02 15:04"))
			}
			if t.FiredAt > 0 && t.Kind == TriggerKindOnce {
				b.WriteString(" · 실행 완료")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	s.ui.Send(m.ChatID, strings.TrimRight(b.String(), "\n"), SendOptions{Plain: true, ReplyToMessageID: m.MessageID})
}

func (s *CronService) cmdKill(m InboundEvent, owner CronOwner, kill bool) {
	if !s.admins[owner.UserID] {
		s.audit(&owner, "reject_admin", "high", "kill="+fmt.Sprint(kill))
		s.ui.Send(m.ChatID, cronNotAdminNotice, SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	if err := s.store.SetKilled(kill); err != nil {
		s.ui.Send(m.ChatID, fmt.Sprintf("⚠️ 상태 변경 실패: %v", err), SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	event, sev := "kill_on", "warning"
	text := "⛔ 예약 작업 실행을 중지했습니다. 새 슬롯은 실행되지 않습니다.\n재개: /cron resume-all"
	if !kill {
		event, sev = "kill_off", "info"
		text = "▶️ 예약 작업 실행을 재개했습니다."
	}
	s.audit(&owner, event, sev, "")
	s.ui.Send(m.ChatID, text, SendOptions{ReplyToMessageID: m.MessageID})
	if s.sched != nil {
		s.sched.Kick()
	}
}

func (s *CronService) cmdStatus(m InboundEvent, owner CronOwner) {
	killed, err := s.store.Killed()
	if err != nil {
		s.ui.Send(m.ChatID, fmt.Sprintf("⚠️ 상태 조회 실패: %v", err), SendOptions{ReplyToMessageID: m.MessageID})
		return
	}
	state := "▶️ 실행 중"
	if killed {
		state = "⛔ 중지됨"
	}
	s.ui.Send(m.ChatID, fmt.Sprintf("⚙️ cron 스케줄러 상태: %s", state), SendOptions{ReplyToMessageID: m.MessageID})
}

func (s *CronService) audit(actor *CronOwner, event, severity, detail string) {
	s.store.Audit(actor, event, severity, detail)
}

// --- Planner flow ---

func cronIdempotencyKey(m InboundEvent, args string) string {
	normalized := strings.Join(strings.Fields(args), " ")
	sum := sha256.Sum256([]byte(strings.Join([]string{
		m.Platform, m.ChatID, m.UserID,
		fmt.Sprint(m.MessageID), normalized,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

// planFlow turns natural language into a validated cron command through agy.
func (s *CronService) planFlow(m InboundEvent, owner CronOwner, args string) {
	replyOpt := SendOptions{ReplyToMessageID: m.MessageID}

	key := cronIdempotencyKey(m, args)
	if prev, ok := s.store.GetIdempotentResponse(key); ok {
		s.ui.Send(m.ChatID, prev, replyOpt)
		return
	}

	if s.convID() == "" {
		s.ui.Send(m.ChatID, s.missingConvMsg, replyOpt)
		return
	}

	jobs, err := s.store.ListJobs(owner)
	if err != nil {
		s.failPlan(m, key, fmt.Sprintf("⚠️ 작업 목록 조회 실패: %v", err))
		return
	}

	refs := make(map[int64]string, len(jobs))
	var lines []string
	for _, j := range jobs {
		token, terr := s.store.PutTargetRef(owner, j.ID, j.Revision, s.clock)
		if terr != nil {
			s.failPlan(m, key, fmt.Sprintf("⚠️ 대상 토큰 발급 실패: %v", terr))
			return
		}
		refs[j.ID] = token
		lines = append(lines, fmt.Sprintf("#%d ref=%s\n  프롬프트: %s\n  상태: %s",
			j.ID, token, truncateRunes(j.TaskPrompt, 80), enabledLabel(j.Enabled)))
		for _, t := range j.Triggers {
			lines[len(lines)-1] += "\n  주기: " + describeStoredTrigger(t)
		}
	}

	prompt := buildCronPlannerPrompt(lines, args, s.clock.Now())
	resp, perr := s.callPlanner(prompt)
	if perr != nil {
		s.failPlan(m, key, fmt.Sprintf("⚠️ 예약 작업 해석 실패: %v", perr))
		return
	}

	spec, verr := parseCronCommand(resp, s.clock.Now())
	if verr != nil {
		// One corrective round-trip, then hard stop.
		repairPrompt := fmt.Sprintf("%s\n\n[시스템] 직전 응답은 다음 이유로 거부되었습니다:\n%s\n"+
			"위 거부 사유를 수정한 단일 JSON 객체만 출력하세요. 다른 설명은 금지입니다.",
			prompt, verr.Error())
		resp2, rerr := s.callPlanner(repairPrompt)
		if rerr == nil {
			spec, verr = parseCronCommand(resp2, s.clock.Now())
		}
	}
	if verr != nil {
		s.audit(&owner, "reject_schema", "warning", verr.Error())
		s.failPlan(m, key, "❌ 예약 작업 요청을 해석하지 못했습니다.\n형식 예시는 /cron help 를 참고하고, 표현을 단순하게 바꿔 다시 시도해 주세요.")
		return
	}

	s.beginConfirm(m, owner, key, *spec, refs, jobs)
}

func (s *CronService) callPlanner(prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	return s.planner(ctx, prompt)
}

func (s *CronService) failPlan(m InboundEvent, key, text string) {
	s.remember(key, text)
	s.ui.Send(m.ChatID, text, SendOptions{ReplyToMessageID: m.MessageID})
}

// remember persists a deduplicated response; callback-initiated flows pass
// an empty key and skip storage.
func (s *CronService) remember(key, text string) {
	if key == "" {
		return
	}
	s.store.PutIdempotency(key, text)
}

// beginConfirm resolves the target (explicit ref / unambiguous single job /
// selection buttons) and posts the confirmation UI for write actions.
func (s *CronService) beginConfirm(m InboundEvent, owner CronOwner, key string, spec cronCommandSpec, refs map[int64]string, jobs []CronJobRecord) {
	replyOpt := SendOptions{ReplyToMessageID: m.MessageID}

	if spec.Action == CronActionCreate {
		s.postConfirmation(m, owner, key, spec, 0, 0, "", replyOpt)
		return
	}

	// Explicit reference from the model.
	if spec.TargetRef != nil && *spec.TargetRef != "" {
		jobID, rev, err := s.store.ConsumeTargetRef(*spec.TargetRef, owner, s.clock)
		if err != nil {
			s.handleTargetFailure(m, owner, key, spec, err)
			return
		}
		s.postConfirmationFromStore(m, owner, key, spec, jobID, rev, replyOpt)
		return
	}

	// No reference: zero or one owned job resolves without ambiguity.
	active := activeJobs(jobs)
	if len(active) == 0 {
		text := "📭 수정할 예약 작업이 없습니다."
		s.remember(key, text)
		s.ui.Send(m.ChatID, text, replyOpt)
		return
	}
	if len(active) == 1 {
		s.postConfirmationFromStore(m, owner, key, spec, active[0].ID, active[0].Revision, replyOpt)
		return
	}

	// Ambiguous: let the initiator pick via buttons bound to issued refs.
	payload, _ := json.Marshal(cronConfirmPayload{Spec: spec})
	ctok, xtok, err := s.store.PutConfirmationPair(owner, "select", string(payload), 0, s.clock)
	if err != nil {
		s.failPlan(m, key, fmt.Sprintf("⚠️ 선택 세션 생성 실패: %v", err))
		return
	}
	var kb InlineKeyboard
	var b strings.Builder
	b.WriteString("🔎 대상 예약 작업을 선택해 주세요:\n\n")
	for _, j := range jobs {
		ref, ok := refs[j.ID]
		if !ok {
			continue
		}
		data := cronCbSelectRef + ctok + ":" + ref
		if len(data) > 64 {
			continue
		}
		b.WriteString(j.Summary() + "\n")
		kb = append(kb, []InlineButton{{Text: "#" + fmt.Sprint(j.ID) + " 선택", Data: data}})
	}
	kb = append(kb, []InlineButton{{Text: "취소", Data: cronCbCancel + xtok}})
	text := strings.TrimSpace(b.String()) + "\n\n(10분 안에 선택해 주세요)"
	s.remember(key, text)
	s.ui.SendWithKeyboard(m.ChatID, text, kb)
	s.audit(&owner, "select_offered", "info", fmt.Sprintf("action=%s candidates=%d", spec.Action, len(active)))
}

func (s *CronService) handleTargetFailure(m InboundEvent, owner CronOwner, key string, spec cronCommandSpec, err error) {
	var text string
	switch {
	case errors.Is(err, errCronNotFound):
		s.audit(&owner, "reject_target_ref", "high", "unknown or reused target_ref")
		text = "❌ 유효하지 않은 대상 참조입니다. /cron 을 다시 실행해 주세요."
	default:
		text = "❌ " + err.Error()
	}
	s.remember(key, text)
	s.ui.Send(m.ChatID, text, SendOptions{ReplyToMessageID: m.MessageID})
}

// postConfirmationFromStore loads the live job and hands off to
// postConfirmation with a rendered before-state.
func (s *CronService) postConfirmationFromStore(m InboundEvent, owner CronOwner, key string, spec cronCommandSpec, jobID, rev int64, replyOpt SendOptions) {
	job, err := s.store.GetJob(owner, jobID)
	if err != nil || job.Revision != rev {
		text := "⚠️ 대상 작업이 최근 변경되었습니다. /cron 을 다시 실행해 주세요."
		if err != nil && !errors.Is(err, errCronNotFound) {
			text = fmt.Sprintf("⚠️ 작업 조회 실패: %v", err)
		}
		s.remember(key, text)
		s.ui.Send(m.ChatID, text, replyOpt)
		return
	}
	before := job.Summary()
	for _, t := range job.Triggers {
		before += "\n   ⏲ " + describeStoredTrigger(t)
	}
	s.postConfirmation(m, owner, key, spec, jobID, job.Revision, before, replyOpt)
}

// postConfirmation freezes the change into a confirm/cancel button pair.
func (s *CronService) postConfirmation(m InboundEvent, owner CronOwner, key string, spec cronCommandSpec, jobID, revision int64, beforeText string, replyOpt SendOptions) {
	payloadBytes, _ := json.Marshal(cronConfirmPayload{Spec: spec, JobID: jobID, Revision: revision, BeforeText: beforeText})
	ctok, xtok, err := s.store.PutConfirmationPair(owner, spec.Action, string(payloadBytes), 0, s.clock)
	if err != nil {
		s.failPlan(m, key, fmt.Sprintf("⚠️ 확인 세션 생성 실패: %v", err))
		return
	}
	kb := InlineKeyboard{{{Text: "✅ 확인", Data: cronCbConfirm + ctok}, {Text: "취소", Data: cronCbCancel + xtok}}}
	text := cronConfirmationText(spec, jobID, beforeText)
	s.remember(key, text)
	s.ui.SendWithKeyboard(m.ChatID, text+"\n\n(10분 안에 확인해 주세요)", kb)
	s.audit(&owner, "confirm_offered", "info", fmt.Sprintf("action=%s job=%d", spec.Action, jobID))
}

func cronConfirmationText(spec cronCommandSpec, jobID int64, beforeText string) string {
	var b strings.Builder
	switch spec.Action {
	case CronActionCreate:
		b.WriteString("🗓 새 예약 작업 등록 확인\n\n")
	case CronActionModify:
		fmt.Fprintf(&b, "✏️ 예약 작업 #%d 수정 확인\n\n[현재]\n%s\n\n[변경 후]\n", jobID, beforeText)
	case CronActionDelete:
		fmt.Fprintf(&b, "🗑 예약 작업 #%d 삭제 확인\n\n[대상]\n%s\n", jobID, beforeText)
	case CronActionPause:
		fmt.Fprintf(&b, "⏸ 예약 작업 #%d 일시정지 확인\n\n[대상]\n%s\n", jobID, beforeText)
	case CronActionResume:
		fmt.Fprintf(&b, "▶️ 예약 작업 #%d 재개 확인\n\n[대상]\n%s\n", jobID, beforeText)
	}
	if spec.TaskPrompt != nil && (spec.Action == CronActionCreate || spec.Action == CronActionModify) {
		b.WriteString("프롬프트: " + truncateRunes(*spec.TaskPrompt, 200) + "\n")
	}
	if len(spec.Triggers) > 0 {
		b.WriteString("주기:\n")
		for _, t := range spec.Triggers {
			b.WriteString("  - " + describeCronTriggerSpec(t) + "\n")
		}
	}
	if spec.Action == CronActionModify && len(spec.Triggers) == 0 {
		b.WriteString("주기: 유지\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// summarizeCronSpec renders the prompt and trigger lines of a spec for
// result messages (post-application).
func summarizeCronSpec(spec cronCommandSpec) string {
	var b strings.Builder
	if spec.TaskPrompt != nil {
		b.WriteString("프롬프트: " + truncateRunes(*spec.TaskPrompt, 200) + "\n")
	}
	if len(spec.Triggers) > 0 {
		b.WriteString("주기:\n")
		for _, t := range spec.Triggers {
			b.WriteString("  - " + describeCronTriggerSpec(t) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- Callbacks ---

// HandleCallback processes inline-keyboard presses routed as /cron-callback.
func (s *CronService) HandleCallback(m InboundEvent) {
	owner := s.ownerOf(m)
	data := m.CallbackData
	if data == "" {
		return
	}

	edit := func(text string) {
		s.ui.EditMessage(m.ChatID, m.MessageID, text, nil)
	}
	answer := func(text string) {
		s.ui.AnswerCallbackQuery(m.CallbackID, text)
	}

	switch {
	case strings.HasPrefix(data, cronCbSelectRef):
		parts := strings.SplitN(strings.TrimPrefix(data, cronCbSelectRef), ":", 2)
		if len(parts) != 2 {
			answer("잘못된 요청입니다")
			return
		}
		s.onSelectPressed(m, owner, parts[0], parts[1], edit, answer)
	case strings.HasPrefix(data, cronCbConfirm), strings.HasPrefix(data, cronCbCancel):
		token := ""
		cancelled := strings.HasPrefix(data, cronCbCancel)
		if cancelled {
			token = strings.TrimPrefix(data, cronCbCancel)
		} else {
			token = strings.TrimPrefix(data, cronCbConfirm)
		}
		s.onConfirmPressed(m, owner, token, edit, answer)
	default:
		answer("")
	}
}

func (s *CronService) onSelectPressed(m InboundEvent, owner CronOwner, confToken, refToken string, edit func(string), answer func(string)) {
	jobID, rev, err := s.store.ConsumeTargetRef(refToken, owner, s.clock)
	if err != nil {
		if errors.Is(err, errCronNotFound) {
			s.audit(&owner, "reject_target_ref", "high", "select ref unknown/reused")
			answer("만료되었거나 유효하지 않은 선택입니다")
			edit("⌛ 이 선택 목록은 만료되었거나 이미 처리되었습니다. /cron 을 다시 실행해 주세요.")
			return
		}
		answer(err.Error())
		edit("❌ " + err.Error())
		return
	}
	conf, err := s.store.ConsumeConfirmation(confToken, owner, s.clock)
	if err != nil {
		// Ref already burned; ask for a full retry to stay consistent.
		if errors.Is(err, errCronNotFound) {
			answer("이미 처리된 요청입니다")
			edit("⌛ 이미 처리되었거나 만료되었습니다. /cron 을 다시 실행해 주세요.")
			return
		}
		answer(err.Error())
		edit("❌ " + err.Error())
		return
	}
	if conf.Cancelled || conf.Action != "select" {
		answer("취소되었습니다")
		edit("취소되었습니다.")
		return
	}
	var payload cronConfirmPayload
	if err := json.Unmarshal([]byte(conf.PayloadJSON), &payload); err != nil {
		answer("내부 오류입니다")
		edit("❌ 저장된 요청을 해석하지 못했습니다. /cron 을 다시 실행해 주세요.")
		return
	}
	s.postConfirmationFromStore(m, owner, "", payload.Spec, jobID, rev, SendOptions{})
	answer("선택 완료 — 아래 확인 안내를 눌러주세요")
	// The new confirmation arrives as a separate keyboard message.
}

func (s *CronService) onConfirmPressed(m InboundEvent, owner CronOwner, token string, edit func(string), answer func(string)) {
	conf, err := s.store.ConsumeConfirmation(token, owner, s.clock)
	if err != nil {
		if errors.Is(err, errCronNotFound) {
			s.audit(&owner, "confirm_replay_or_unknown", "warning", token)
			answer("이미 처리되었거나 존재하지 않는 요청입니다")
			edit("⌛ 이미 처리되었거나 만료된 요청입니다.")
			return
		}
		if ve, ok := err.(*cronValidationError); ok {
			answer("만료됨")
			edit("⌛ " + ve.msg)
			return
		}
		answer(err.Error())
		edit("❌ " + err.Error())
		return
	}
	if conf.Cancelled {
		s.audit(&owner, "cancelled", "info", conf.Action)
		answer("취소되었습니다")
		edit("🚫 취소되었습니다.")
		return
	}

	var payload cronConfirmPayload
	if err := json.Unmarshal([]byte(conf.PayloadJSON), &payload); err != nil {
		answer("내부 오류입니다")
		edit("❌ 저장된 요청을 해석하지 못했습니다.")
		return
	}

	result, ok := s.applyConfirmed(owner, conf.Action, payload)
	if ok {
		answer("반영되었습니다")
	} else {
		answer("실패")
	}
	edit(result)
}

// applyConfirmed performs the frozen write inside one path, re-checking the
// optimistic revision captured at confirmation-build time.
func (s *CronService) applyConfirmed(owner CronOwner, action string, payload cronConfirmPayload) (string, bool) {
	spec := payload.Spec
	switch action {
	case CronActionCreate:
		trig := storedTriggersFromSpec(spec.Triggers, s.clock.Now())
		id, err := s.store.CreateJob(owner, derefString(spec.TaskPrompt), trig, s.clock)
		if err != nil {
			s.audit(&owner, "create_failed", "error", err.Error())
			return "❌ 등록 실패: " + err.Error(), false
		}
		s.audit(&owner, "created", "info", fmt.Sprintf("job=%d triggers=%d", id, len(trig)))
		s.nudgeScheduler()
		return fmt.Sprintf("✅ 예약 작업 #%d 을(를) 등록했습니다.\n%s", id, summarizeCronSpec(spec)), true

	case CronActionModify:
		job, err := s.store.GetJob(owner, payload.JobID)
		if err != nil {
			return "⚠️ 작업을 찾을 수 없습니다. /cron list 로 확인해 주세요.", false
		}
		if job.Revision != payload.Revision {
			s.audit(&owner, "reject_stale_revision", "warning", fmt.Sprintf("job=%d exp=%d cur=%d", payload.JobID, payload.Revision, job.Revision))
			return "⚠️ 그동안 작업이 변경되어 반영하지 않았습니다. /cron 을 다시 실행해 주세요.", false
		}
		upd := JobUpdate{ExpectRev: &job.Revision}
		if spec.TaskPrompt != nil {
			upd.Prompt = spec.TaskPrompt
		}
		if len(spec.Triggers) > 0 {
			trig := storedTriggersFromSpec(spec.Triggers, s.clock.Now())
			upd.Triggers = &trig
		}
		if _, err := s.store.UpdateJob(owner, payload.JobID, upd, s.clock); err != nil {
			s.audit(&owner, "modify_failed", "error", err.Error())
			return "❌ 수정 실패: " + err.Error(), false
		}
		s.audit(&owner, "modified", "info", fmt.Sprintf("job=%d", payload.JobID))
		s.nudgeScheduler()
		return "✅ 수정을 반영했습니다.\n\n[변경 후]\n" + summarizeCronSpec(spec), true

	case CronActionDelete:
		if err := s.store.SoftDeleteJob(owner, payload.JobID, &payload.Revision, s.clock); err != nil {
			if errors.Is(err, errCronNotFound) {
				return "⚠️ 이미 삭제되었거나 존재하지 않는 작업입니다.", false
			}
			s.audit(&owner, "delete_failed", "error", err.Error())
			return "❌ 삭제 실패: " + err.Error(), false
		}
		s.audit(&owner, "deleted", "info", fmt.Sprintf("job=%d", payload.JobID))
		s.nudgeScheduler()
		return fmt.Sprintf("🗑 예약 작업 #%d 을(를) 삭제했습니다.", payload.JobID), true

	case CronActionPause, CronActionResume:
		enable := action == CronActionResume
		upd := JobUpdate{ExpectRev: &payload.Revision, SetEnabled: &enable}
		if _, err := s.store.UpdateJob(owner, payload.JobID, upd, s.clock); err != nil {
			if errors.Is(err, errCronNotFound) {
				return "⚠️ 작업을 찾을 수 없습니다.", false
			}
			s.audit(&owner, "state_change_failed", "error", err.Error())
			return "❌ 상태 변경 실패: " + err.Error(), false
		}
		label := "일시정지"
		if enable {
			label = "재개"
		}
		s.audit(&owner, "state_changed", "info", fmt.Sprintf("job=%d paused=%v", payload.JobID, !enable))
		s.nudgeScheduler()
		return fmt.Sprintf("%s 예약 작업 #%d 을(를) %s했습니다.", stateIcon(enable), payload.JobID, label), true
	}
	return "❌ 알 수 없는 요청입니다.", false
}

func (s *CronService) nudgeScheduler() {
	if s.sched == nil {
		return
	}
	go func() {
		if _, err := s.sched.store.ReconcileMissed(s.clock.Now(), cronSkipThreshold); err != nil {
			log.Printf("cron reconcile after write failed: %v", err)
		}
		s.sched.Kick()
	}()
}

// --- Small helpers ---

func activeJobs(jobs []CronJobRecord) []CronJobRecord {
	out := make([]CronJobRecord, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j)
	}
	return out
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "▶ 활성"
	}
	return "⏸ 일시정지"
}

func stateIcon(enabled bool) string {
	if enabled {
		return "▶️"
	}
	return "⏸"
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// storedTriggersFromSpec converts validated DSL triggers into rows with a
// precomputed next-run slot.
func storedTriggersFromSpec(specs []cronTriggerSpec, now time.Time) []StoredTrigger {
	out := make([]StoredTrigger, 0, len(specs))
	for _, ts := range specs {
		st := StoredTrigger{Kind: ts.Kind, Cron: ts.Cron, At: ts.At, Timezone: ts.Timezone}
		next, consume := cronNextFromSpec(st.Kind, st.SpecJSON(), now)
		switch {
		case consume:
			st.FiredAt = now.Unix() // defensive: never schedule an already-past once slot
		case next != nil:
			st.NextRunAt = next.Unix()
		}
		out = append(out, st)
	}
	return out
}

func describeStoredTrigger(t StoredTrigger) string {
	if t.Kind == TriggerKindOnce {
		return fmt.Sprintf("일회성 %s (%s)", t.At, t.Timezone)
	}
	return fmt.Sprintf("반복 `%s` (%s)", t.Cron, t.Timezone)
}

func describeCronTriggerSpec(t cronTriggerSpec) string {
	if t.Kind == TriggerKindOnce {
		return fmt.Sprintf("일회성 %s (%s)", t.At, t.Timezone)
	}
	return fmt.Sprintf("반복 `%s` (%s)", t.Cron, t.Timezone)
}

// buildCronPlannerPrompt assembles the planner contract: output shape, rules,
// the current inventory with opaque refs, and the user request.
func buildCronPlannerPrompt(jobLines []string, userRequest string, now time.Time) string {
	var b strings.Builder
	b.WriteString("당신은 예약 작업 관리 플래너입니다. 사용자의 요청을 아래 JSON 규격 하나로 변환하는 것이 유일한 임무입니다.\n\n")
	b.WriteString("[출력 규격]\n")
	b.WriteString("- 코드펜스·설명 없이 정확히 하나의 JSON 객체만 출력합니다.\n")
	b.WriteString(`- 형식: {"version":1,"kind":"cron_command","action":"create|modify|delete|pause|resume","target_ref":문자열|null,"task_prompt":문자열(생략 가능),"triggers":[...](생략 가능)}` + "\n")
	b.WriteString("- triggers 각 원소는 둘 중 하나입니다:\n")
	b.WriteString(`  {"kind":"periodic","cron":"분 시 일 월 요일","timezone":"IANA이름"}` + "\n")
	b.WriteString(`  {"kind":"once","at":"RFC3339(+09:00 같은 오프셋 포함)","timezone":"IANA이름"}` + "\n")
	b.WriteString("- cron은 5필드만, 최소 15분 간격. @every, 초 필드, L/W/# 금지.\n")
	b.WriteString("- once의 at은 반드시 미래여야 하고 timezone의 오프셋과 일치해야 합니다.\n")
	b.WriteString("- create: target_ref=null, task_prompt+triggers 필수.\n")
	b.WriteString("- modify/delete/pause/resume: 대상이 명확하면 해당 ref 문자열을 target_ref로, 불명확하면 null.\n")
	b.WriteString("- modify의 triggers는 전체 교체입니다. 유지하려면 생략하세요.\n")
	b.WriteString("- task_prompt/triggers 외 필드를 modify/delete/pause/resume에 넣지 마세요.\n")
	b.WriteString("- version/kind/action/target_ref/task_prompt/triggers/timezone/cron/at/kind 외 어떤 키도 사용하지 마세요.\n\n")
	b.WriteString("[현재 서버 시각]\n" + now.Format("2006-01-02 15:04 (UTC 오프셋 -07:00)") + "\n")
	b.WriteString("timezone은 IANA 이름만 허용됩니다 (예: Asia/Seoul, UTC).\n\n")
	if len(jobLines) > 0 {
		b.WriteString("[등록된 예약 작업]\n" + strings.Join(jobLines, "\n") + "\n\n")
	} else {
		b.WriteString("[등록된 예약 작업]\n없음\n\n")
	}
	b.WriteString("[사용자 요청]\n" + userRequest + "\n")
	return b.String()
}
