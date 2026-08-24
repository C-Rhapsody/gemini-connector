package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func ptrString(s string) *string { return &s }
func ptrInt64(i int64) *int64    { return &i }

func mkPeriodic(cron string, next int64) StoredTrigger {
	return StoredTrigger{Kind: TriggerKindPeriodic, Cron: cron, Timezone: "Asia/Seoul", NextRunAt: next}
}

func TestCronStore_MigrateIdempotent(t *testing.T) {
	store := newTestCronStore(t) // opens + migrates
	if _, err := store.Killed(); err != nil {
		t.Fatalf("scheduler state missing after migrate: %v", err)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != cronSchemaVersionNum {
		t.Fatalf("user_version = %d err=%v, want %d", version, err, cronSchemaVersionNum)
	}
}

func TestCronStore_JobCRUDAndOwnership(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	alice := testOwner("111")
	bob := testOwner("222")

	id, err := store.CreateJob(alice, "뉴스 요약", []StoredTrigger{mkPeriodic("0 9 * * *", clock.Now().Add(time.Hour).Unix())}, clock)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.CreateJob(bob, "bob 작업", nil, clock); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	jobs, err := store.ListJobs(alice)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("alice jobs = %v err=%v", jobs, err)
	}
	if jobs[0].ID != id || len(jobs[0].Triggers) != 1 {
		t.Fatalf("unexpected job %+v", jobs[0])
	}

	if _, err := store.GetJob(bob, id); !errors.Is(err, errCronNotFound) {
		t.Fatalf("cross-owner get must be not-found, got %v", err)
	}

	// Update bumps revision and replaces triggers wholesale.
	newTrig := []StoredTrigger{mkPeriodic("30 8 * * *", clock.Now().Add(2*time.Hour).Unix()), mkPeriodic("0 20 * * *", 0)}
	newRev, err := store.UpdateJob(alice, id, JobUpdate{Prompt: ptrString("수정됨"), Triggers: &newTrig}, clock)
	if err != nil || newRev != 2 {
		t.Fatalf("update rev=%d err=%v", newRev, err)
	}
	job, _ := store.GetJob(alice, id)
	if job.TaskPrompt != "수정됨" || len(job.Triggers) != 2 {
		t.Fatalf("after update: %+v", job)
	}

	// Stale revision is rejected.
	if _, err := store.UpdateJob(alice, id, JobUpdate{SetEnabled: ptrBool(false), ExpectRev: ptrInt64(1)}, clock); err == nil {
		t.Fatal("stale revision must fail")
	}

	// Soft delete hides the job.
	if err := store.SoftDeleteJob(alice, id, nil, clock); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	jobs, _ = store.ListJobs(alice)
	if len(jobs) != 0 {
		t.Fatalf("deleted job still listed")
	}
	if err := store.SoftDeleteJob(alice, id, nil, clock); !errors.Is(err, errCronNotFound) {
		t.Fatalf("double delete = %v", err)
	}
}

func ptrBool(b bool) *bool { return &b }

func TestCronStore_Limits(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	owner := testOwner("333")
	for i := 0; i < cronMaxJobsUser; i++ {
		if _, err := store.CreateJob(owner, "job", nil, clock); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := store.CreateJob(owner, "overflow", nil, clock); err == nil || !strings.Contains(err.Error(), "최대") {
		t.Fatalf("expected budget rejection, got %v", err)
	}

	// Trigger budget counts across jobs.
	rich := testOwner("444")
	trig := []StoredTrigger{mkPeriodic("0 9 * * *", 0)}
	if _, err := store.CreateJob(rich, "j", trig, clock); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tooMany := make([]StoredTrigger, cronMaxTriggersUser)
	for i := range tooMany {
		tooMany[i] = mkPeriodic("0 12 * * *", 0)
	}
	if _, err := store.CreateJob(rich, "j2", tooMany, clock); err == nil || !strings.Contains(err.Error(), "trigger") {
		t.Fatalf("expected trigger budget rejection, got %v", err)
	}
}

func TestCronStore_TargetRefSingleUse(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	owner := testOwner("555")
	id, _ := store.CreateJob(owner, "t", nil, clock)

	token, err := store.PutTargetRef(owner, id, 1, clock)
	if err != nil || len(token) != cronTokenBytes*2 {
		t.Fatalf("put ref token=%q err=%v", token, err)
	}

	wrongScope := CronOwner{Platform: owner.Platform, ChatID: owner.ChatID, UserID: "evil"}
	if _, _, err := store.ConsumeTargetRef(token, wrongScope, clock); !errors.Is(err, errCronNotFound) {
		t.Fatalf("cross-owner consume must fail, got %v", err)
	}

	gotID, rev, err := store.ConsumeTargetRef(token, owner, clock)
	if err != nil || gotID != id || rev != 1 {
		t.Fatalf("consume = %d,%d,%v", gotID, rev, err)
	}
	if _, _, err := store.ConsumeTargetRef(token, owner, clock); !errors.Is(err, errCronNotFound) {
		t.Fatalf("replay must be rejected, got %v", err)
	}

	expired, _ := store.PutTargetRef(owner, id, 1, clock)
	clock.Advance(cronTokenTTL + time.Minute)
	if _, _, err := store.ConsumeTargetRef(expired, owner, clock); err == nil || errors.Is(err, errCronNotFound) {
		t.Fatalf("expired ref must surface TTL error, got %v", err)
	}
}

func TestCronStore_ConfirmationPairAtomic(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	owner := testOwner("666")

	conf, cancel, err := store.PutConfirmationPair(owner, "delete", `{"spec":{}}`, 7, clock)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if len(conf) != cronTokenBytes*2 || len(cancel) != cronTokenBytes*2 {
		t.Fatalf("token lengths %d/%d", len(conf), len(cancel))
	}

	// Cancel side burns both.
	got, err := store.ConsumeConfirmation(cancel, owner, clock)
	if err != nil || !got.Cancelled || got.MessageID != 7 {
		t.Fatalf("cancel consume = %+v err=%v", got, err)
	}
	if _, err := store.ConsumeConfirmation(conf, owner, clock); !errors.Is(err, errCronNotFound) {
		t.Fatalf("confirm after cancel must be dead, got %v", err)
	}

	// Scope mismatch cannot even observe existence.
	conf2, cancel2, _ := store.PutConfirmationPair(owner, "create", `{}`, 0, clock)
	evils := CronOwner{Platform: "telegram", ChatID: owner.ChatID, UserID: "999"}
	if _, err := store.ConsumeConfirmation(conf2, evils, clock); !errors.Is(err, errCronNotFound) {
		t.Fatalf("scope mismatch must hide row, got %v", err)
	}

	// Expiry path.
	conf3, _, _ := store.PutConfirmationPair(owner, "pause", `{}`, 0, clock)
	clock.Advance(cronTokenTTL + time.Minute)
	if _, err := store.ConsumeConfirmation(conf3, owner, clock); err == nil || errors.Is(err, errCronNotFound) {
		t.Fatalf("expired confirm must surface TTL error, got %v", err)
	}
	_ = cancel2
}

func TestCronStore_Idempotency(t *testing.T) {
	store := newTestCronStore(t)
	key := "k123"
	if v, ok := store.GetIdempotentResponse(key); ok {
		t.Fatalf("unexpected hit: %q", v)
	}
	store.PutIdempotency(key, "first response")
	store.PutIdempotency(key, "second ignored")
	if v, ok := store.GetIdempotentResponse(key); !ok || v != "first response" {
		t.Fatalf("got %q ok=%v", v, ok)
	}
}

func TestCronStore_AuditAndKillSwitch(t *testing.T) {
	store := newTestCronStore(t)
	owner := testOwner("777")
	store.Audit(&owner, "created", "info", "detail-한글")
	store.SetKilled(true)
	killed, err := store.Killed()
	if err != nil || !killed {
		t.Fatalf("killed=%v err=%v", killed, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM cron_audit_log WHERE event='created'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit rows=%d err=%v", count, err)
	}
}
