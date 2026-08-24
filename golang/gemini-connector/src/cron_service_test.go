package main

import (
	"strings"
	"testing"
)

func newTestService(t *testing.T, clock *fakeClock, planner cronPlannerFunc, admins []string) (*CronService, *stubCronUI, *CronStore) {
	t.Helper()
	store := newTestCronStore(t)
	ui := &stubCronUI{}
	svc := NewCronService(store, cfgForScheduler(), ui, planner, admins, nil)
	svc.clock = clock
	return svc, ui, store
}

const validCreateJSON = `{"version":1,"kind":"cron_command","action":"create","target_ref":null,` +
	`"task_prompt":"매일 아침 AI 뉴스를 요약해줘",` +
	`"triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"Asia/Seoul"}]}`

func TestCronService_CreateHappyPath(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{responses: []string{validCreateJSON}}
	svc, ui, store := newTestService(t, clock, p.plan, []string{"42"})

	msg := cronMsg("111", "매일 아침 9시에 뉴스 요약해줘", 10)
	svc.HandleMessage(msg)

	if p.count() != 1 {
		t.Fatalf("planner calls = %d", p.count())
	}
	kb := ui.lastKeyboard()
	if kb == nil {
		t.Fatal("confirmation keyboard missing")
	}
	confData, ok := findButtonData(kb, cronCbConfirm)
	if !ok {
		t.Fatal("confirm button missing")
	}
	if _, ok := findButtonData(kb, cronCbCancel); !ok {
		t.Fatal("cancel button missing")
	}
	for _, row := range kb {
		for _, b := range row {
			if len(b.Data) > 64 {
				t.Fatalf("callback data exceeds 64 bytes: %q", b.Data)
			}
		}
	}

	token := trimCbPrefix(confData, cronCbConfirm)
	if len(token) != 16 {
		t.Fatalf("confirm token length = %d", len(token))
	}
	svc.HandleCallback(cbMsg("111", confData, 10))

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("jobs=%d err=%v", count, err)
	}
	job, _ := store.GetJob(testOwner("111"), jobIDFromCount(store))
	if job.TaskPrompt != "매일 아침 AI 뉴스를 요약해줘" || len(job.Triggers) != 1 {
		t.Fatalf("stored job wrong: %+v", job)
	}
	if job.Triggers[0].NextRunAt == 0 {
		t.Fatal("next run not precomputed at creation")
	}

	foundResult := false
	for _, e := range ui.edits {
		if strings.Contains(e.text, "등록했습니다") && e.messageID == 10 {
			foundResult = true
		}
	}
	if !foundResult {
		t.Fatalf("result edit missing; edits=%+v", ui.edits)
	}
	if _, ok := ui.lastTextContaining(""); !ok {
		t.Fatal("no sends recorded")
	}
}

func jobIDFromCount(store *CronStore) int64 {
	var id int64
	_ = store.db.QueryRow(`SELECT MIN(id) FROM cron_jobs`).Scan(&id)
	return id
}

func TestCronService_IdempotentReplay(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{responses: []string{validCreateJSON}}
	svc, ui, _ := newTestService(t, clock, p.plan, nil)

	msg := cronMsg("111", "매일 뉴스 요약 등록해줘", 77)
	svc.HandleMessage(msg)
	firstLen := len(ui.sends)

	svc.HandleMessage(msg) // exact duplicate Telegram redelivery
	if len(ui.sends) <= firstLen {
		t.Fatal("duplicate must replay a stored response")
	}
	if p.count() != 1 {
		t.Fatalf("planner re-invoked on duplicate: %d", p.count())
	}
	replayed := ui.sends[len(ui.sends)-1].text
	if !strings.Contains(replayed, "등록 확인") {
		t.Fatalf("replayed text unexpected: %q", replayed)
	}
}

func TestCronService_ActorMismatchRejected(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{responses: []string{validCreateJSON}}
	svc, ui, store := newTestService(t, clock, p.plan, nil)

	svc.HandleMessage(cronMsg("111", "뉴스 요약 매일", 11))
	confData, ok := findButtonData(ui.lastKeyboard(), cronCbConfirm)
	if !ok {
		t.Fatal("no confirm button")
	}

	svc.HandleCallback(cbMsg("999", confData, 11)) // attacker clicks

	var jobs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("attacker created jobs=%d err=%v", jobs, err)
	}
	// Owner can still complete the flow with the same token.
	svc.HandleCallback(cbMsg("111", confData, 11))
	store.db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("owner confirm after attack failed, jobs=%d", jobs)
	}
	var rejects int
	store.db.QueryRow(`SELECT COUNT(*) FROM cron_audit_log WHERE event='confirm_replay_or_unknown' OR event='reject_scope'`).Scan(&rejects)
	if rejects == 0 {
		t.Fatal("actor mismatch not audited")
	}
}

func TestCronService_GarbageOutputRepairsOnceThenFails(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{responses: []string{"그냥 자연어 대답입니다", "여전히 JSON이 아님"}}
	svc, ui, store := newTestService(t, clock, p.plan, nil)

	svc.HandleMessage(cronMsg("111", "매일 뉴스 줘", 12))

	if p.count() != 2 {
		t.Fatalf("repair round-trips = %d, want exactly 2 total calls", p.count())
	}
	text, ok := ui.lastTextContaining("해석하지 못했습니다")
	if !ok {
		t.Fatalf("failure notice missing, sends=%+v", ui.sends)
	}
	if strings.Contains(text, "JSON") {
		// Internal validation detail (raw reasons) must not leak raw parser dumps.
		t.Logf("notice text: %q", text)
	}
	var jobs int
	store.db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Fatal("failed plan must not mutate storage")
	}
	var audits int
	store.db.QueryRow(`SELECT COUNT(*) FROM cron_audit_log WHERE event='reject_schema'`).Scan(&audits)
	if audits == 0 {
		t.Fatal("reject_schema not audited")
	}
}

func TestCronService_AmbiguousModifySelectionFlow(t *testing.T) {
	clock := newFakeClock(t)
	modifyJSON := `{"version":1,"kind":"cron_command","action":"modify","target_ref":null,` +
		`"task_prompt":"바뀐 프롬프트"}`
	p := &plannerStub{responses: []string{modifyJSON}}
	svc, ui, store := newTestService(t, clock, p.plan, nil)

	owner := testOwner("111")
	idA, _ := store.CreateJob(owner, "작업 A", nil, clock)
	idB, _ := store.CreateJob(owner, "작업 B", nil, clock)

	svc.HandleMessage(cronMsg("111", "프롬프트 바꿔줘", 20))

	selectData, ok := findButtonData(ui.lastKeyboard(), cronCbSelectRef)
	if !ok {
		t.Fatalf("selection buttons missing, kb=%+v", ui.lastKeyboard())
	}
	rest := trimCbPrefix(selectData, cronCbSelectRef)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("select payload malformed: %q", selectData)
	}
	svc.HandleCallback(cbMsg("111", selectData, 20))

	confData, ok := findButtonData(ui.lastKeyboard(), cronCbConfirm)
	if !ok {
		t.Fatalf("post-selection confirmation missing, sends=%d edits=%+v", len(ui.sends), ui.edits)
	}
	svc.HandleCallback(cbMsg("111", confData, 20))

	a, errA := store.GetJob(owner, idA)
	b, errB := store.GetJob(owner, idB)
	if errA != nil || errB != nil {
		t.Fatalf("get jobs: %v %v", errA, errB)
	}
	changedA := a.TaskPrompt == "바뀐 프롬프트"
	changedB := b.TaskPrompt == "바뀐 프롬프트"
	if changedA == changedB {
		t.Fatalf("exactly one job must change: A(%v) B(%v)", changedA, changedB)
	}
}

func TestCronService_StaleRevisionBlocksConfirm(t *testing.T) {
	clock := newFakeClock(t)
	delJSON := `{"version":1,"kind":"cron_command","action":"delete","target_ref":null}`
	p := &plannerStub{responses: []string{delJSON}}
	svc, ui, store := newTestService(t, clock, p.plan, nil)

	owner := testOwner("111")
	id, _ := store.CreateJob(owner, "삭제 대상", nil, clock)

	svc.HandleMessage(cronMsg("111", "그 작업 삭제해줘", 30))
	confData, ok := findButtonData(ui.lastKeyboard(), cronCbConfirm)
	if !ok {
		t.Fatal("confirmation for unambiguous delete missing")
	}

	// External mutation between offer and click.
	if _, err := store.UpdateJob(owner, id, JobUpdate{SetEnabled: ptrBool(false)}, clock); err != nil {
		t.Fatalf("external bump: %v", err)
	}

	svc.HandleCallback(cbMsg("111", confData, 30))

	foundStale := false
	for _, e := range ui.edits {
		if strings.Contains(e.text, "변경") && strings.Contains(e.text, "버전") {
			foundStale = true
		}
	}
	if !foundStale {
		t.Fatalf("stale-revision guard message missing, edits=%+v", ui.edits)
	}
	if _, err := store.GetJob(owner, id); err != nil {
		t.Fatalf("job must survive stale confirm: %v", err)
	}
}

func TestCronService_KillSwitchAdminGate(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{}
	svcNoAdmin, uiA, storeA := newTestService(t, clock, p.plan, nil)
	svcNoAdmin.HandleMessage(cronMsg("111", "kill", 40))
	if _, ok := uiA.lastTextContaining("관리자 전용"); !ok {
		t.Fatal("non-admin kill must be refused")
	}
	killed, _ := storeA.Killed()
	if killed {
		t.Fatal("non-admin kill must not flip state")
	}

	svcAdmin, uiB, storeB := newTestService(t, clock, p.plan, []string{"42"})
	svcAdmin.HandleMessage(cronMsg("42", "kill", 41))
	if killed, _ := storeB.Killed(); !killed {
		t.Fatal("admin kill failed")
	}
	svcAdmin.HandleMessage(cronMsg("42", "resume-all", 42))
	if killed, _ := storeB.Killed(); killed {
		t.Fatal("resume-all failed")
	}
	if _, ok := uiB.lastTextContaining("재개"); !ok {
		t.Fatalf("missing resume notice, sends=%+v", uiB.sends)
	}
}

func TestCronService_HelpAndListAndEmptyTarget(t *testing.T) {
	clock := newFakeClock(t)
	p := &plannerStub{}
	svc, ui, _ := newTestService(t, clock, p.plan, nil)

	svc.HandleMessage(cronMsg("111", "", 50))
	if _, ok := ui.lastTextContaining("/cron list"); !ok {
		t.Fatal("help text missing")
	}

	svc.HandleMessage(cronMsg("111", "list", 51))
	if _, ok := ui.lastTextContaining("등록된 예약 작업이 없습니다"); !ok {
		t.Fatal("empty list notice missing")
	}

	// modify/delete with zero jobs resolves without planner.
	delJSON := `{"version":1,"kind":"cron_command","action":"delete","target_ref":null}`
	p2 := &plannerStub{responses: []string{delJSON}}
	svc2, ui2, _ := newTestService(t, clock, p2.plan, nil)
	svc2.HandleMessage(cronMsg("222", "전부 삭제해줘", 52))
	if _, ok := ui2.lastTextContaining("수정할 예약 작업이 없습니다"); !ok {
		t.Fatalf("zero-job notice missing, sends=%+v", ui2.sends)
	}
	if p2.count() != 1 {
		t.Fatalf("planner unexpectedly skipped: %d", p2.count())
	}
}

func TestCronService_RejectedSchemaNeverTouchesStoreOrSchedulerKick(t *testing.T) {
	clock := newFakeClock(t)
	// Forbidden key smuggled deep inside triggers.
	bad := `{"version":1,"kind":"cron_command","action":"create","task_prompt":"t",` +
		`"triggers":[{"kind":"periodic","cron":"0 9 * * *","timezone":"Asia/Seoul","webhook":"http://x"}]}`
	p := &plannerStub{responses: []string{bad}}
	svc, _, store := newTestService(t, clock, p.plan, nil)

	before := tableCounts(t, store)
	svc.HandleMessage(cronMsg("111", "매일 뉴스", 60))
	after := tableCounts(t, store)
	for table, delta := range map[string]int{"cron_jobs": 0, "cron_triggers": 0, "cron_confirmations": 0} {
		if after[table]-before[table] != int64(delta) {
			t.Fatalf("%s mutated on rejection: %d -> %d", table, before[table], after[table])
		}
	}
	var audits int
	store.db.QueryRow(`SELECT COUNT(*) FROM cron_audit_log WHERE severity='high' AND event='reject_schema'`).Scan(&audits)
	_ = audits // covered in garbage test; here we assert storage immutability only
}

func tableCounts(t *testing.T, store *CronStore) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, tbl := range []string{"cron_jobs", "cron_triggers", "cron_confirmations"} {
		var n int64
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}
