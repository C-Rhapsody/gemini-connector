package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func cfgForScheduler() *Config {
	return &Config{TelegramChatID: 42, AgyConversationID: "conv-test"}
}

// seedDueJob inserts an enabled job whose single periodic trigger is due in
// dueIn relative to the fake clock.
func seedDueJob(t *testing.T, store *CronStore, clock *fakeClock, owner CronOwner, cronExpr string, dueIn time.Duration) int64 {
	t.Helper()
	id, err := store.CreateJob(owner, "예약 작업", []StoredTrigger{mkPeriodic(cronExpr, clock.Now().Add(dueIn).Unix())}, clock)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

type execRecorder struct {
	mu   sync.Mutex
	runs []ScheduledExecution
	fn   func(ctx context.Context, ex ScheduledExecution) error
}

func (r *execRecorder) execute(ctx context.Context, ex ScheduledExecution) error {
	r.mu.Lock()
	r.runs = append(r.runs, ex)
	r.mu.Unlock()
	if r.fn != nil {
		return r.fn(ctx, ex)
	}
	return nil
}

func (r *execRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func newTestScheduler(store *CronStore, clock *fakeClock, ui Messenger, rec *execRecorder) (*CronScheduler, *agyTurnQueue) {
	q := newAgyTurnQueue()
	sched := NewCronScheduler(store, cfgForScheduler(), ui, rec.execute)
	sched.clock = clock
	sched.SetQueue(q)
	return sched, q
}

func TestCronScheduler_ClaimRunAndDedup(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	ui := &stubCronUI{}
	rec := &execRecorder{}
	sched, q := newTestScheduler(store, clock, ui, rec)

	owner := testOwner("900")
	seedDueJob(t, store, clock, owner, "*/15 * * * *", 0)

	sched.tickOnce()
	waitQueueIdle(t, q)
	if got := rec.count(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}

	// Same instant again: slot already consumed by the unique execution row.
	sched.tickOnce()
	waitQueueIdle(t, q)
	if got := rec.count(); got != 1 {
		t.Fatalf("duplicate claim: runs = %d", got)
	}

	var status string
	if err := store.db.QueryRow(`SELECT status FROM cron_executions`).Scan(&status); err != nil || status != ExecStatusSuccess {
		t.Fatalf("execution status = %q err=%v", status, err)
	}
	var next int64
	if err := store.db.QueryRow(`SELECT next_run_at FROM cron_triggers`).Scan(&next); err != nil || next <= clock.Now().Unix() {
		t.Fatalf("periodic not advanced: next=%d now=%d err=%v", next, clock.Now().Unix(), err)
	}
	if ownerChat := ui.sends; false {
		_ = ownerChat // executor stub sends nothing; UI untouched
	}
}

func TestCronScheduler_KillSwitchFreezesClaims(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	ui := &stubCronUI{}
	rec := &execRecorder{}
	sched, q := newTestScheduler(store, clock, ui, rec)

	store.SetKilled(true)
	seedDueJob(t, store, clock, testOwner("901"), "*/15 * * * *", 0)
	sched.tickOnce()
	waitQueueIdle(t, q)
	if rec.count() != 0 {
		t.Fatal("killed scheduler must not run jobs")
	}

	store.SetKilled(false)
	sched.tickOnce()
	waitQueueIdle(t, q)
	if rec.count() != 1 {
		t.Fatal("resume must allow claims again")
	}
}

func TestCronScheduler_StopCancelsLedger(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)
	ui := &stubCronUI{}

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	rec := &execRecorder{fn: func(ctx context.Context, ex ScheduledExecution) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	sched, q := newTestScheduler(store, clock, ui, rec)

	seedDueJob(t, store, clock, testOwner("902"), "*/15 * * * *", 0)
	seedDueJob(t, store, clock, testOwner("903"), "*/15 * * * *", 0)

	sched.tickOnce()
	<-entered // first executor is running inside the queue

	q.StopActive() // cancels running ctx and drops queued job (onDrop fires)
	close(release)
	waitQueueIdle(t, q)

	var cancelled int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM cron_executions WHERE status = ?`, ExecStatusCancelled).Scan(&cancelled); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cancelled != 2 {
		t.Fatalf("cancelled rows = %d, want 2", cancelled)
	}
}

func TestCronScheduler_ReconcileSkipsMissedSlots(t *testing.T) {
	store := newTestCronStore(t)
	clock := newFakeClock(t)

	oncePast := StoredTrigger{Kind: TriggerKindOnce, At: clock.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Timezone: "UTC"}
	onceID, _ := store.CreateJob(testOwner("910"), "once", []StoredTrigger{oncePast}, clock)
	staleSlot := clock.Now().Add(-time.Hour).Unix()
	_, _ = store.db.Exec(`UPDATE cron_triggers SET next_run_at = ? WHERE job_id = ?`, staleSlot, onceID)

	perID, _ := store.CreateJob(testOwner("911"), "periodic", []StoredTrigger{mkPeriodic("*/15 * * * *", clock.Now().Add(-6*time.Hour).Unix())}, clock)

	freshNext := clock.Now().Add(time.Hour).Unix()
	futID, _ := store.CreateJob(testOwner("912"), "future", []StoredTrigger{mkPeriodic("0 12 * * *", freshNext)}, clock)

	n, err := store.ReconcileMissed(clock.Now(), cronSkipThreshold)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected >=2 fixes, got %d", n)
	}

	var skipped int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM cron_executions WHERE status=? AND trigger_id IN (SELECT id FROM cron_triggers WHERE job_id=?)`,
		ExecStatusSkipped, onceID).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("once skip rows=%d err=%v", skipped, err)
	}
	var fired int64
	if err := store.db.QueryRow(`SELECT COALESCE(fired_at,0) FROM cron_triggers WHERE job_id=?`, onceID).Scan(&fired); err != nil || fired == 0 {
		t.Fatalf("once trigger not retired: fired=%d err=%v", fired, err)
	}

	var perNext int64
	if err := store.db.QueryRow(`SELECT next_run_at FROM cron_triggers WHERE job_id=?`, perID).Scan(&perNext); err != nil || perNext <= clock.Now().Unix() {
		t.Fatalf("periodic slot still stale: %d err=%v", perNext, err)
	}

	var futNext int64
	if err := store.db.QueryRow(`SELECT next_run_at FROM cron_triggers WHERE job_id=?`, futID).Scan(&futNext); err != nil || futNext != freshNext {
		t.Fatalf("future slot changed: %d != %d err=%v", futNext, freshNext, err)
	}
}

func TestDefaultExecutor_WrapperConversationPlainSend(t *testing.T) {
	cfg := cfgForScheduler()
	cfg.AgyConversationID = "conv-123"

	restore := swapCronAgyRunner(func(ctx context.Context, prompt, convID string) (string, error) {
		if convID != "conv-123" {
			t.Errorf("conversation = %q", convID)
		}
		if !strings.Contains(prompt, scheduledTaskHeader) || !strings.Contains(prompt, "뉴스를 요약해줘") {
			t.Errorf("prompt missing wrapper/task: %q", prompt)
		}
		return "요약 결과입니다", nil
	})
	defer restore()

	ui := &stubCronUI{}
	ex := defaultCronJobExecutor(cfg, ui)
	err := ex(context.Background(), ScheduledExecution{
		ExecutionID: 1,
		Slot:        time.Now(),
		Job:         CronJobRecord{ID: 5, TaskPrompt: "뉴스를 요약해줘", Enabled: true},
		Owner:       testOwner("920"),
	})
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	if len(ui.sends) != 1 || !ui.sends[0].opts.Plain {
		t.Fatalf("expected exactly one plain send, got %+v", ui.sends)
	}
	if !strings.Contains(ui.sends[0].text, "예약 작업 #5") || !strings.Contains(ui.sends[0].text, "요약 결과") {
		t.Fatalf("unexpected payload: %q", ui.sends[0].text)
	}
}

func TestDefaultExecutor_NoActiveConversationFails(t *testing.T) {
	cfg := &Config{TelegramChatID: 42} // empty AgyConversationID
	restore := swapCronAgyRunner(func(ctx context.Context, prompt, convID string) (string, error) {
		t.Fatal("agy must not be called without a conversation")
		return "", nil
	})
	defer restore()

	ui := &stubCronUI{}
	err := defaultCronJobExecutor(cfg, ui)(context.Background(), ScheduledExecution{
		Job:   CronJobRecord{ID: 6},
		Owner: testOwner("921"),
	})
	if err == nil || !strings.Contains(err.Error(), "no active conversation") {
		t.Fatalf("expected no-active-conversation failure, got %v", err)
	}
	if len(ui.sends) != 1 {
		t.Fatalf("failure notice missing, sends=%d", len(ui.sends))
	}
}
