package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // embed IANA zone database so Windows builds resolve zones identically
	"unicode"

	crcron "github.com/robfig/cron/v3"
)

// forbiddenCronKeys are key names that must never appear anywhere inside a
// cron command JSON tree. They either mirror internal identifiers (ownership
// tuple, job IDs) or smuggle execution capability, so a model attempting to
// widen its own authority is rejected wholesale instead of partially honored.
var forbiddenCronKeys = map[string]bool{
	"chat_id": true, "user_id": true, "platform": true, "credential": true,
	"token": true, "api_key": true, "db_path": true, "path": true,
	"file": true, "exec": true, "command": true, "sql": true,
	"url": true, "webhook": true, "job_id": true, "owner": true,
}

var cronTargetRefPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

// extractCronJSON isolates the single JSON object agy was required to emit.
// Exactly one object is accepted: bare text plus an object is rejected the
// same way as two fenced blocks, so trailing prose can never smuggle a second
// command past the parser.
func extractCronJSON(text string) ([]byte, error) {
	fenceRe := regexp.MustCompile("(?is)```(?:json)?\\s*(.*?)```")
	fences := fenceRe.FindAllStringSubmatch(text, -1)
	var candidate string
	switch {
	case len(fences) > 1:
		return nil, cronInvalid("응답에 JSON 블록이 2개 이상 있습니다")
	case len(fences) == 1:
		remainder := strings.TrimSpace(fenceRe.ReplaceAllString(text, ""))
		if remainder != "" {
			return nil, cronInvalid("JSON 코드펜스 밖에 추가 내용이 있습니다")
		}
		candidate = fences[0][1]
	default:
		spans := topLevelObjectSpans(text)
		if len(spans) != 1 {
			return nil, cronInvalid("단일 JSON 객체를 찾지 못했습니다")
		}
		span := spans[0]
		if strings.TrimSpace(text[:span[0]]) != "" || strings.TrimSpace(text[span[1]:]) != "" {
			return nil, cronInvalid("JSON 객체 앞뒤에 추가 내용이 있습니다")
		}
		candidate = text[span[0]:span[1]]
	}
	candidate = strings.TrimSpace(candidate)
	if !json.Valid([]byte(candidate)) {
		return nil, cronInvalid("올바른 JSON이 아닙니다")
	}
	return []byte(candidate), nil
}

// topLevelObjectSpans returns [start,end) byte ranges of depth-0 objects,
// ignoring braces inside JSON strings. The range index from `for ... range`
// over a string is already a byte offset.
func topLevelObjectSpans(text string) [][2]int {
	var spans [][2]int
	depth, start, inStr, esc := 0, -1, false, false
	for i, r := range text {
		switch {
		case esc:
			esc = false
		case r == '\\' && inStr:
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// string content
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			depth--
			if depth == 0 && start >= 0 {
				spans = append(spans, [2]int{start, i + 1})
				start = -1
			}
			if depth < 0 {
				return nil
			}
		}
	}
	return spans
}

func scanForbiddenKeys(root any) error {
	switch node := root.(type) {
	case map[string]any:
		for k, v := range node {
			lk := strings.ToLower(k)
			if forbiddenCronKeys[lk] {
				return cronInvalid("금지된 키가 포함되어 있습니다: %s", k)
			}
			if err := scanForbiddenKeys(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range node {
			if err := scanForbiddenKeys(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseCronCommand runs the full Go-side pipeline: extraction, forbidden-key
// scan, strict decode and semantic validation. The returned spec carries the
// normalized (sanitized) prompt and triggers.
func parseCronCommand(text string, now time.Time) (*cronCommandSpec, error) {
	raw, err := extractCronJSON(text)
	if err != nil {
		return nil, err
	}

	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, cronInvalid("JSON 파싱 실패: %v", err)
	}
	if err := scanForbiddenKeys(generic); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec cronCommandSpec
	if err := dec.Decode(&spec); err != nil {
		return nil, cronInvalid("알 수 없는 필드 또는 잘못된 형식: %v", err)
	}
	if dec.More() {
		return nil, cronInvalid("JSON 객체 뒤에 데이터가 더 있습니다")
	}
	if err := validateCronCommand(&spec, now); err != nil {
		return nil, err
	}
	return &spec, nil
}

func validateCronCommand(spec *cronCommandSpec, now time.Time) error {
	if spec.Version != cronSchemaVersion {
		return cronInvalid("version은 %d이어야 합니다", cronSchemaVersion)
	}
	if spec.Kind != cronCommandKind {
		return cronInvalid("kind는 %s이어야 합니다", cronCommandKind)
	}
	switch spec.Action {
	case CronActionCreate, CronActionModify, CronActionDelete, CronActionPause, CronActionResume:
	default:
		return cronInvalid("지원하지 않는 action입니다: %q", spec.Action)
	}
	if spec.TargetRef != nil && *spec.TargetRef != "" && !cronTargetRefPattern.MatchString(*spec.TargetRef) {
		return cronInvalid("target_ref는 시스템이 발급한 토큰이어야 합니다")
	}

	needsTarget := spec.Action != CronActionCreate
	if needsTarget && spec.TargetRef != nil && *spec.TargetRef == "" {
		return cronInvalid("target_ref가 비어 있습니다")
	}

	switch spec.Action {
	case CronActionCreate:
		if spec.TargetRef != nil {
			return cronInvalid("create에서는 target_ref를 지정할 수 없습니다")
		}
		if spec.TaskPrompt == nil || len(*spec.TaskPrompt) == 0 {
			return cronInvalid("create에는 task_prompt가 필요합니다")
		}
		if len(spec.Triggers) == 0 {
			return cronInvalid("create에는 triggers가 최소 1개 필요합니다")
		}
	case CronActionModify:
		hasPrompt := spec.TaskPrompt != nil && strings.TrimSpace(*spec.TaskPrompt) != ""
		if !hasPrompt && len(spec.Triggers) == 0 {
			return cronInvalid("modify는 task_prompt 또는 triggers 중 하나 이상 필요합니다")
		}
	default:
		if spec.TaskPrompt != nil {
			return cronInvalid("%s에서는 task_prompt를 변경할 수 없습니다", spec.Action)
		}
		if len(spec.Triggers) > 0 {
			return cronInvalid("%s에서는 triggers를 변경할 수 없습니다", spec.Action)
		}
	}

	if len(spec.Triggers) > cronMaxTriggersJob {
		return cronInvalid("trigger는 작업당 최대 %d개입니다", cronMaxTriggersJob)
	}
	for i := range spec.Triggers {
		if _, err := normalizeCronTrigger(&spec.Triggers[i], now); err != nil {
			return fmt.Errorf("triggers[%d]: %w", i, err)
		}
	}
	if spec.TaskPrompt != nil {
		prompt, _, err := sanitizeCronPrompt(*spec.TaskPrompt)
		if err != nil {
			return err
		}
		spec.TaskPrompt = &prompt
	}
	return nil
}

// normalizeCronTrigger validates one trigger union member and returns the
// canonical stored form.
func normalizeCronTrigger(t *cronTriggerSpec, now time.Time) (*StoredTrigger, error) {
	loc, err := resolveCronTimezone(t.Timezone)
	if err != nil {
		return nil, err
	}
	tzName := loc.String()
	switch t.Kind {
	case TriggerKindPeriodic:
		if t.At != "" {
			return nil, cronInvalid("periodic trigger에는 at을 지정할 수 없습니다")
		}
		expr, err := validateCronExpression(t.Cron)
		if err != nil {
			return nil, err
		}
		sched, perr := crcron.ParseStandard(expr)
		if perr != nil {
			return nil, cronInvalid("cron 표현식 파싱 실패: %v", perr)
		}
		if gap := minimalScheduleGap(sched, now); gap > 0 && gap < cronMinInterval {
			return nil, cronInvalid("실행 간격이 너무 짧습니다 (%s). 최소 %s입니다", gap.Truncate(time.Second), cronMinInterval)
		}
		t.Cron = expr
		t.Timezone = tzName
		return &StoredTrigger{Kind: TriggerKindPeriodic, Cron: expr, Timezone: tzName}, nil
	case TriggerKindOnce:
		if t.Cron != "" {
			return nil, cronInvalid("once trigger에는 cron을 지정할 수 없습니다")
		}
		at, err := parseCronOnceAt(t.At, loc, now)
		if err != nil {
			return nil, err
		}
		t.At = at.Format(time.RFC3339)
		t.Timezone = tzName
		return &StoredTrigger{Kind: TriggerKindOnce, At: t.At, Timezone: tzName}, nil
	default:
		return nil, cronInvalid("trigger kind는 periodic 또는 once여야 합니다")
	}
}

// validateCronExpression enforces a conservative 5-field subset: no seconds
// field, no @descriptors and no L/W/# extensions.
func validateCronExpression(expr string) (string, error) {
	expr = strings.Join(strings.Fields(expr), " ")
	if expr == "" {
		return "", cronInvalid("cron 표현식이 비어 있습니다")
	}
	if strings.ContainsAny(expr, "@") {
		return "", cronInvalid("@ 기반 cron 기술자는 지원하지 않습니다")
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", cronInvalid("cron 표현식은 정확히 5개 필드여야 합니다 (분 시 일 월 요일)")
	}
	for _, f := range fields {
		if strings.ContainsAny(f, "LW#") {
			return "", cronInvalid("L/W/# 확장 표현은 지원하지 않습니다")
		}
	}
	return expr, nil
}

// minimalScheduleGap probes successive fire times to find the shortest gap.
// Schedules that do not fire twice within the probe budget (e.g. yearly) are
// treated as sparse and pass; dense violations surface within the first few
// probes because robfig steps minute-by-minute from `now`.
func minimalScheduleGap(sched crcron.Schedule, now time.Time) time.Duration {
	minGap := time.Duration(0)
	prev := sched.Next(now)
	for i := 0; i < cronMaxGapProbe; i++ {
		next := sched.Next(prev)
		if next.After(now.Add(cronIntervalHorizon)) {
			break
		}
		gap := next.Sub(prev)
		if minGap == 0 || gap < minGap {
			minGap = gap
			if minGap < cronMinInterval {
				return minGap
			}
		}
		prev = next
	}
	return minGap
}

// resolveCronTimezone accepts IANA zone names only. Blank names, "Local" and
// legacy aliases without a slash ("Korea", "ROK", ...) are rejected so both
// the planner and stored rows speak unambiguous zone identifiers.
func resolveCronTimezone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, cronInvalid("timezone이 필요합니다")
	}
	if name == "UTC" {
		return time.UTC, nil
	}
	if !strings.Contains(name, "/") {
		return nil, cronInvalid("IANA timezone 이름을 사용하세요 (예: Asia/Seoul): %q", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, cronInvalid("알 수 없는 timezone입니다: %q", name)
	}
	return loc, nil
}

// parseCronOnceAt validates an RFC3339 instant against its declared zone:
// the offset must match what the IANA zone would use at that instant, and
// the moment must lie in the future (with a small clock-skew allowance).
func parseCronOnceAt(at string, loc *time.Location, now time.Time) (time.Time, error) {
	at = strings.TrimSpace(at)
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, cronInvalid("at은 오프셋이 포함된 RFC3339 형식이어야 합니다: %v", err)
	}
	expected := t.In(loc)
	_, gotOffset := t.Zone()
	_, wantOffset := expected.Zone()
	if wantOffset != gotOffset {
		return time.Time{}, cronInvalid("at의 UTC 오프셋(%s)이 timezone %s의 오프셋(%s)과 일치하지 않습니다",
			formatOffsetSeconds(gotOffset), loc, formatOffsetSeconds(wantOffset))
	}
	if !t.After(now.Add(-60 * time.Second)) {
		return time.Time{}, cronInvalid("at은 미래 시각이어야 합니다")
	}
	return t.UTC(), nil
}

func formatOffsetSeconds(sec int) string {
	sign := "+"
	if sec < 0 {
		sign = "-"
		sec = -sec
	}
	return fmt.Sprintf("%s%02d:%02d", sign, sec/3600, (sec%3600)/60)
}

// sanitizeCronPrompt strips control, zero-width and bidi-override characters
// so a scheduled prompt cannot hide directional tricks, and enforces the
// rune budget.
func sanitizeCronPrompt(raw string) (string, bool, error) {
	var b strings.Builder
	changed := false
	for _, r := range raw {
		switch {
		case unicode.IsControl(r) && r != '\n' && r != '\t':
			changed = true
		case r >= '\u200b' && r <= '\u200f',
			r >= '\u202a' && r <= '\u202e',
			r >= '\u2066' && r <= '\u2069',
			r == '\ufeff':
			changed = true
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", changed, cronInvalid("task_prompt가 비어 있습니다")
	}
	if len([]rune(out)) > cronMaxPromptRunes {
		return "", changed, cronInvalid("task_prompt가 너무 깁니다 (최대 %d 자)", cronMaxPromptRunes)
	}
	return out, changed, nil
}

// cronTokenHash derives the stored lookup key for opaque tokens; raw tokens
// are never persisted.
func cronTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// cronNextFromSpec advances a stored trigger to its next occurrence after
// `after`. For once triggers it reports consume=true when the instant has
// arrived (or passed) so callers retire the trigger instead of refiring it.
func cronNextFromSpec(kind, specJSON string, after time.Time) (*time.Time, bool) {
	var stored struct {
		Cron string `json:"cron"`
		At   string `json:"at"`
		TZ   string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(specJSON), &stored); err != nil {
		return nil, true
	}
	switch kind {
	case TriggerKindPeriodic:
		sched, err := crcron.ParseStandard(stored.Cron)
		if err != nil {
			return nil, true
		}
		next := sched.Next(after)
		return &next, false
	case TriggerKindOnce:
		at, err := time.Parse(time.RFC3339, stored.At)
		if err != nil {
			return nil, true
		}
		if !after.Before(at) {
			return nil, true
		}
		return &at, false
	default:
		return nil, true
	}
}
