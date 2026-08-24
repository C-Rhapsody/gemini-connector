package main

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, text string, now time.Time) *cronCommandSpec {
	t.Helper()
	spec, err := parseCronCommand(text, now)
	if err != nil {
		t.Fatalf("parseCronCommand rejected %q: %v", text, err)
	}
	return spec
}

func wantReject(t *testing.T, text string, now time.Time, substr string) {
	t.Helper()
	if _, err := parseCronCommand(text, now); err == nil {
		t.Fatalf("expected rejection for %q", text)
	} else if !strings.Contains(err.Error(), substr) && substr != "" {
		t.Fatalf("error %q missing substring %q", err.Error(), substr)
	}
}

func TestExtractCronJSON(t *testing.T) {
	now := newFakeClock(t).Now()

	mustParse(t, `{"version":1,"kind":"cron_command","action":"pause","target_ref":null}`, now)
	mustParse(t, "```json\n{\"version\":1,\"kind\":\"cron_command\",\"action\":\"pause\",\"target_ref\":null}\n```", now)

	wantReject(t, "text before {\"version\":1,\"kind\":\"cron_command\",\"action\":\"pause\",\"target_ref\":null}", now, "")
	wantReject(t, "{\"version\":1,\"kind\":\"cron_command\",\"action\":\"pause\",\"target_ref\":null}\n{\"version\":1,\"kind\":\"cron_command\",\"action\":\"pause\",\"target_ref\":null}", now, "")
	wantReject(t, "```json\n{}\n```\n```json\n{}\n```", now, "2개 이상")
	wantReject(t, "{not json}", now, "")
}

const createBody = `"task_prompt":"매일 뉴스 요약","triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"Asia/Seoul"}]`

func TestParseCronCommand_ForbiddenKeysRejected(t *testing.T) {
	now := newFakeClock(t).Now()
	cases := []string{
		`{"version":1,"kind":"cron_command","action":"create","chat_id":"123",` + createBody + `}`,
		`{"version":1,"kind":"cron_command","action":"create","user_id":42,` + createBody + `}`,
		`{"version":1,"kind":"cron_command","action":"create","task_prompt":"x","triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"UTC","url":"http://evil"}]}`,
		`{"version":1,"kind":"cron_command","action":"create","nested":{"command":"rm -rf"},` + createBody + `}`,
	}
	for _, c := range cases {
		wantReject(t, c, now, "금지된 키")
	}
}

func TestParseCronCommand_StrictSchema(t *testing.T) {
	now := newFakeClock(t).Now()
	wantReject(t, `{"version":1,"kind":"cron_command","action":"create","enabled":true,`+createBody+`}`, now, "")
	wantReject(t, `{"version":2,"kind":"cron_command","action":"create",`+createBody+`}`, now, "version")
	wantReject(t, `{"version":1,"kind":"other","action":"create",`+createBody+`}`, now, "kind")
	wantReject(t, `{"version":1,"kind":"cron_command","action":"execute",`+createBody+`}`, now, "action")
}

func TestParseCronCommand_ActionFieldRules(t *testing.T) {
	now := newFakeClock(t).Now()
	base := `"version":1,"kind":"cron_command"`

	// create rules
	wantReject(t, `{`+base+`,"action":"create","target_ref":"0123456789abcdef",`+createBody+`}`, now, "create")
	wantReject(t, `{`+base+`,"action":"create"}`, now, "task_prompt")
	wantReject(t, `{`+base+`,"action":"create","task_prompt":"x","triggers":[]}`, now, "triggers")

	// modify needs at least one change
	wantReject(t, `{`+base+`,"action":"modify","target_ref":null}`, now, "하나 이상")
	// delete cannot change prompt
	wantReject(t, `{`+base+`,"action":"delete","target_ref":null,"task_prompt":"nope"}`, now, "변경할 수 없습니다")
	// pause with triggers rejected
	wantReject(t, `{`+base+`,"action":"pause","target_ref":null,"triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"UTC"}]}`, now, "변경할 수 없습니다")
	// empty target_ref string is invalid
	wantReject(t, `{`+base+`,"action":"delete","target_ref":""}`, now, "비어")
	// malformed ref pattern
	wantReject(t, `{`+base+`,"action":"delete","target_ref":"JOB-1"}`, now, "토큰")

	// happy paths
	mustParse(t, `{`+base+`,"action":"delete","target_ref":null}`, now)
	mustParse(t, `{`+base+`,"action":"resume","target_ref":"0123456789abcdef"}`, now)
}

func TestParseCronCommand_PeriodicRules(t *testing.T) {
	now := newFakeClock(t).Now()
	build := func(expr string) string {
		return `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t","triggers":[{"kind":"periodic","cron":"` + expr + `","timezone":"Asia/Seoul"}]}`
	}
	mustParse(t, build("0 9 * * *"), now)    // daily
	mustParse(t, build("*/15 * * * *"), now) // exactly minimum gap
	mustParse(t, build("0 12 1 1 *"), now)   // yearly (sparse passes probe budget)
	wantReject(t, build("* * * * * *"), now, "5개 필드")
	wantReject(t, build("@every 5m"), now, "@")
	wantReject(t, build("0 9 * * L"), now, "L/W/#")
	wantReject(t, build("*/10 * * * *"), now, "너무 짧습니다") // 56->0 gives 4-minute gap
	wantReject(t, build(""), now, "")
}

func TestParseCronCommand_OnceRules(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	build := func(at string) string {
		return `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t","triggers":[{"kind":"once","at":"` + at + `","timezone":"Asia/Seoul"}]}`
	}
	mustParse(t, build("2026-08-25T09:00:00+09:00"), now)
	wantReject(t, build("2026-08-24T09:00:00+09:00"), now, "미래")
	wantReject(t, build("2026-08-25T09:00:00Z"), now, "일치하지 않습니다") // Z vs KST offset
	// union exclusivity
	wantReject(t, `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t","triggers":[{"kind":"once","at":"2026-08-25T09:00:00+09:00","cron":"0 9 * * *","timezone":"Asia/Seoul"}]}`, now, "지정할 수 없습니다")
	wantReject(t, `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t","triggers":[{"kind":"periodic","at":"2026-08-25T09:00:00+09:00","timezone":"Asia/Seoul"}]}`, now, "지정할 수 없습니다")
}

func TestParseCronCommand_TimezoneRules(t *testing.T) {
	now := newFakeClock(t).Now()
	build := func(tz string) string {
		return `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t","triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"` + tz + `"}]}`
	}
	mustParse(t, build("Asia/Seoul"), now)
	mustParse(t, build(" UTC "), now)
	wantReject(t, build(""), now, "필요합니다")
	wantReject(t, build("Local"), now, "IANA")
	wantReject(t, build("Korea"), now, "IANA") // legacy alias without slash
	wantReject(t, build("Mars/Olympus"), now, "알 수 없는 timezone")
}

func TestSanitizeCronPrompt(t *testing.T) {
	out, changed, err := sanitizeCronPrompt("\u200b매일\u202e 보고\u0007서 정리\tx ")
	if err != nil || !changed {
		t.Fatalf("sanitize failed: %q %v %v", out, err, changed)
	}
	if strings.ContainsAny(out, "\u200b\u202e\u0007") {
		t.Fatalf("control chars survived: %q", out)
	}
	if _, _, err := sanitizeCronPrompt("   "); err == nil {
		t.Fatal("empty prompt must be rejected")
	}
	long := strings.Repeat("가", cronMaxPromptRunes+1)
	if _, _, err := sanitizeCronPrompt(long); err == nil {
		t.Fatal("over-budget prompt must be rejected")
	}
	if out, _, err := sanitizeCronPrompt("정상 프롬프트"); err != nil || out != "정상 프롬프트" {
		t.Fatalf("clean prompt altered: %q %v", out, err)
	}
}

func TestCronNextFromSpec(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	trig := StoredTrigger{Kind: TriggerKindPeriodic, Cron: "30 9 * * *", Timezone: "Asia/Seoul"}
	next, consume := cronNextFromSpec(trig.Kind, trig.SpecJSON(), now)
	if consume || next == nil || next.Before(now) {
		t.Fatalf("periodic next wrong: %v %v", consume, next)
	}

	once := StoredTrigger{Kind: TriggerKindOnce, At: "2026-08-24T12:00:00Z", Timezone: "UTC"}
	future, consume := cronNextFromSpec(once.Kind, once.SpecJSON(), now)
	if consume || future == nil {
		t.Fatalf("once future wrong: %v %v", consume, future)
	}
	past, consume := cronNextFromSpec(once.Kind, once.SpecJSON(), now.Add(3*time.Hour))
	if !consume || past != nil {
		t.Fatalf("once past wrong: %v %v", consume, past)
	}
}
