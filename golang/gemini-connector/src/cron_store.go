package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var errCronNotFound = errors.New("cron: not found")

// cronClock abstracts wall-clock reads so tests can drive scheduling
// deterministically.
type cronClock interface{ Now() time.Time }

type realCronClock struct{}

func (realCronClock) Now() time.Time { return time.Now() }

// CronStore is the SQLite-backed source of truth for the cron subsystem.
// A single connection serializes writers; every job mutation is scoped by the
// ownership tuple directly inside SQL so cross-owner access can never leak
// through application-level mistakes alone.
type CronStore struct {
	db *sql.DB
}

// cronDatabasePath resolves <exe>/../context/cron.db, matching where
// transcripts already live.
func cronDatabasePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(exePath), "..", "context")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "cron.db"), nil
}

func OpenCronStore(path string) (*CronStore, error) {
	dsn := "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &CronStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *CronStore) Close() error {
	return s.db.Close()
}

// newCronToken returns a fresh opaque token plus its stored hash form.
func newCronToken() (string, string, error) {
	buf := make([]byte, cronTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(buf)
	return token, cronTokenHash(token), nil
}

func unixNow(clock cronClock) int64 {
	return clock.Now().Unix()
}

// --- Audit ---

func (s *CronStore) Audit(actor *CronOwner, event, severity, detail string) {
	var p, c, u any
	if actor != nil {
		p, c, u = actor.Platform, actor.ChatID, actor.UserID
	}
	ts := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO cron_audit_log (ts, actor_platform, actor_chat_id, actor_user_id, event, severity, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, ts, p, c, u, event, severity, truncateRunes(detail, 2000))
	if err != nil {
		log.Printf("cron audit write failed: %v", err)
	}
}

// --- Kill switch ---

func (s *CronStore) Killed() (bool, error) {
	var killed bool
	err := s.db.QueryRow("SELECT killed FROM cron_scheduler_state WHERE id = 1").Scan(&killed)
	return killed, err
}

func (s *CronStore) SetKilled(killed bool) error {
	_, err := s.db.Exec("UPDATE cron_scheduler_state SET killed = ? WHERE id = 1", killed)
	return err
}

// --- Idempotency ---

func (s *CronStore) GetIdempotentResponse(key string) (string, bool) {
	var resp string
	err := s.db.QueryRow("SELECT response_text FROM cron_idempotency WHERE key = ?", key).Scan(&resp)
	return resp, err == nil
}

func (s *CronStore) PutIdempotency(key, response string) {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO cron_idempotency (key, response_text, created_at) VALUES (?, ?, ?)`,
		key, response, time.Now().Unix())
	if err != nil {
		log.Printf("cron idempotency write failed: %v", err)
	}
}

// --- Jobs ---

const cronJobColumns = `id, task_prompt, enabled, revision, created_at, updated_at`

func scanCronJob(row interface{ Scan(...any) error }) (CronJobRecord, error) {
	var j CronJobRecord
	var enabled int
	var created, updated int64
	if err := row.Scan(&j.ID, &j.TaskPrompt, &enabled, &j.Revision, &created, &updated); err != nil {
		return j, err
	}
	j.Enabled = enabled == 1
	j.CreatedAt = time.Unix(created, 0)
	j.UpdatedAt = time.Unix(updated, 0)
	return j, nil
}

func ownerWhere(prefix string, o CronOwner) (string, []any) {
	return fmt.Sprintf("%splatform = ? AND %schat_id = ? AND %suser_id = ?",
		prefix, prefix, prefix), []any{o.Platform, o.ChatID, o.UserID}
}

// loadTriggers reads all triggers for the given jobs and groups them by job ID.
func (s *CronStore) loadTriggers(jobIDs []int64) (map[int64][]StoredTrigger, error) {
	out := make(map[int64][]StoredTrigger)
	if len(jobIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT job_id, kind, spec, COALESCE(next_run_at, 0), COALESCE(fired_at, 0),
			id FROM cron_triggers WHERE job_id IN (%s) ORDER BY id`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t StoredTrigger
		var jobID int64
		var specJSON string
		if err := rows.Scan(&jobID, &t.Kind, &specJSON, &t.NextRunAt, &t.FiredAt, &t.ID); err != nil {
			return nil, err
		}
		var parsed struct {
			Cron string `json:"cron"`
			At   string `json:"at"`
			TZ   string `json:"timezone"`
		}
		if err := json.Unmarshal([]byte(specJSON), &parsed); err == nil {
			t.Cron, t.At, t.Timezone = parsed.Cron, parsed.At, parsed.TZ
		}
		out[jobID] = append(out[jobID], t)
	}
	return out, rows.Err()
}

// ListJobs returns active (non-deleted) jobs owned by the tuple, oldest first.
func (s *CronStore) ListJobs(o CronOwner) ([]CronJobRecord, error) {
	where, args := ownerWhere("", o)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT %s FROM cron_jobs WHERE %s AND deleted_at IS NULL ORDER BY id`, cronJobColumns, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CronJobRecord
	var ids []int64
	for rows.Next() {
		j, err := scanCronJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
		ids = append(ids, j.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	triggers, err := s.loadTriggers(ids)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		jobs[i].Triggers = triggers[jobs[i].ID]
	}
	return jobs, nil
}

// GetJob fetches one job strictly within the ownership scope.
func (s *CronStore) GetJob(o CronOwner, id int64) (*CronJobRecord, error) {
	where, args := ownerWhere("", o)
	row := s.db.QueryRow(
		fmt.Sprintf(`SELECT %s FROM cron_jobs WHERE %s AND deleted_at IS NULL AND id = ?`, cronJobColumns, where),
		append(args, id)...)
	j, err := scanCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errCronNotFound
	}
	if err != nil {
		return nil, err
	}
	triggers, err := s.loadTriggers([]int64{j.ID})
	if err != nil {
		return nil, err
	}
	j.Triggers = triggers[j.ID]
	return &j, nil
}

// insertTriggerRows writes normalized trigger rows with precomputed next-run
// slots. Caller must hold an open transaction.
func insertTriggerRows(tx *sql.Tx, jobID int64, trig []StoredTrigger) error {
	for _, t := range trig {
		next := any(nil)
		if t.NextRunAt > 0 {
			next = t.NextRunAt
		}
		if _, err := tx.Exec(
			`INSERT INTO cron_triggers (job_id, kind, spec, next_run_at) VALUES (?, ?, ?, ?)`,
			jobID, t.Kind, t.SpecJSON(), next); err != nil {
			return err
		}
	}
	return nil
}

// CreateJob inserts a new job with its triggers atomically, enforcing per-user
// job/trigger budgets inside the same transaction as the writes.
func (s *CronStore) CreateJob(o CronOwner, prompt string, trig []StoredTrigger, clock cronClock) (int64, error) {
	now := unixNow(clock)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	where, args := ownerWhere("", o)
	var jobCount int
	if err := tx.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*) FROM cron_jobs WHERE %s AND deleted_at IS NULL`, where), args...).Scan(&jobCount); err != nil {
		return 0, err
	}
	if jobCount >= cronMaxJobsUser {
		return 0, cronInvalid("예약 작업은 사용자당 최대 %d개입니다", cronMaxJobsUser)
	}
	var trigCount int
	w2, a2 := ownerWhere("j.", o)
	if err := tx.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*) FROM cron_triggers t JOIN cron_jobs j ON j.id = t.job_id WHERE %s AND j.deleted_at IS NULL`, w2), a2...).Scan(&trigCount); err != nil {
		return 0, err
	}
	if trigCount+len(trig) > cronMaxTriggersUser {
		return 0, cronInvalid("trigger는 사용자당 최대 %d개입니다", cronMaxTriggersUser)
	}

	res, err := tx.Exec(
		`INSERT INTO cron_jobs (platform, chat_id, user_id, task_prompt, enabled, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, 1, ?, ?)`,
		o.Platform, o.ChatID, o.UserID, prompt, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := insertTriggerRows(tx, id, trig); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// JobUpdate describes the changes to apply. Nil fields are left untouched;
// Triggers replaces the full set (replace-all semantics).
type JobUpdate struct {
	Prompt     *string
	Triggers   *[]StoredTrigger
	SetEnabled *bool
	ExpectRev  *int64
}

// UpdateJob applies the given change set within the ownership scope using an
// optimistic revision check; on success it bumps the revision.
func (s *CronStore) UpdateJob(o CronOwner, id int64, upd JobUpdate, clock cronClock) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	where, args := ownerWhere("", o)
	var rev int64
	err = tx.QueryRow(
		fmt.Sprintf(`SELECT revision FROM cron_jobs WHERE %s AND deleted_at IS NULL AND id = ?`, where),
		append(args, id)...).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errCronNotFound
	}
	if err != nil {
		return 0, err
	}
	if upd.ExpectRev != nil && *upd.ExpectRev != rev {
		return rev, cronInvalid("작업이 이미 변경되었습니다 (현재 버전 %d)", rev)
	}

	sets := []string{"revision = revision + 1", "updated_at = ?"}
	vals := []any{unixNow(clock)}
	if upd.Prompt != nil {
		sets = append(sets, "task_prompt = ?")
		vals = append(vals, *upd.Prompt)
	}
	if upd.SetEnabled != nil {
		v := 0
		if *upd.SetEnabled {
			v = 1
		}
		sets = append(sets, "enabled = ?")
		vals = append(vals, v)
	}
	q := fmt.Sprintf(`UPDATE cron_jobs SET %s WHERE %s AND deleted_at IS NULL AND id = ?`,
		strings.Join(sets, ", "), where)
	vals = append(vals, o.Platform, o.ChatID, o.UserID, id)
	res, err := tx.Exec(q, vals...)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errCronNotFound
	}

	if upd.Triggers != nil {
		if _, err := tx.Exec(`DELETE FROM cron_triggers WHERE job_id = ?`, id); err != nil {
			return 0, err
		}
		if err := insertTriggerRows(tx, id, *upd.Triggers); err != nil {
			return 0, err
		}
	}

	newRev := rev + 1
	return newRev, tx.Commit()
}

// SoftDeleteJob marks the job deleted; history and audit stay intact.
func (s *CronStore) SoftDeleteJob(o CronOwner, id int64, expectRev *int64, clock cronClock) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	where, args := ownerWhere("", o)
	var rev int64
	err = tx.QueryRow(
		fmt.Sprintf(`SELECT revision FROM cron_jobs WHERE %s AND deleted_at IS NULL AND id = ?`, where),
		append(args, id)...).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return errCronNotFound
	}
	if err != nil {
		return err
	}
	if expectRev != nil && *expectRev != rev {
		return cronInvalid("작업이 이미 변경되었습니다 (현재 버전 %d)", rev)
	}
	updateArgs := append([]any{unixNow(clock), unixNow(clock)}, args...)
	updateArgs = append(updateArgs, id)
	if _, err := tx.Exec(
		fmt.Sprintf(`UPDATE cron_jobs SET deleted_at = ?, enabled = 0, revision = revision + 1, updated_at = ?
			WHERE %s AND deleted_at IS NULL AND id = ?`, where),
		updateArgs...); err != nil {
		return err
	}
	return tx.Commit()
}

// --- Target refs ---

func (s *CronStore) PutTargetRef(owner CronOwner, jobID, revision int64, clock cronClock) (string, error) {
	token, hash, err := newCronToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(
		`INSERT INTO cron_target_refs (token_hash, platform, chat_id, user_id, job_id, revision, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hash, owner.Platform, owner.ChatID, owner.UserID, jobID, revision, unixNow(clock)+int64(cronTokenTTL/time.Second))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeTargetRef atomically burns a target_ref: single use, TTL-checked and
// scope-bound to the initiating owner. A scope mismatch is indistinguishable
// from a missing token.
func (s *CronStore) ConsumeTargetRef(token string, owner CronOwner, clock cronClock) (jobID, revision int64, err error) {
	hash := cronTokenHash(token)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var expires int64
	var used sql.NullInt64
	var rowPlatform, rowChat, rowUser string
	e := tx.QueryRow(
		`SELECT job_id, revision, expires_at, used_at, platform, chat_id, user_id FROM cron_target_refs WHERE token_hash = ?`,
		hash).Scan(&jobID, &revision, &expires, &used, &rowPlatform, &rowChat, &rowUser)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, 0, errCronNotFound
	}
	if e != nil {
		return 0, 0, e
	}
	if used.Valid {
		return 0, 0, errCronNotFound
	}
	if rowPlatform != owner.Platform || rowChat != owner.ChatID || rowUser != owner.UserID {
		return 0, 0, errCronNotFound
	}
	if unixNow(clock) > expires {
		return 0, 0, cronInvalid("대상 토큰이 만료되었습니다. /cron 을 다시 실행해 주세요")
	}
	_, e = tx.Exec(`UPDATE cron_target_refs SET used_at = ? WHERE token_hash = ?`, unixNow(clock), hash)
	if e != nil {
		return 0, 0, e
	}
	return jobID, revision, tx.Commit()
}

// --- Confirmations ---

// PutConfirmationPair creates a confirm/cancel button pair bound to one
// pending change. Both tokens die together whichever is used first.
func (s *CronStore) PutConfirmationPair(owner CronOwner, action, payloadJSON string, messageID int, clock cronClock) (confirmToken, cancelToken string, err error) {
	confirmToken, confirmHash, err := newCronToken()
	if err != nil {
		return "", "", err
	}
	cancelToken, cancelHash, err := newCronToken()
	if err != nil {
		return "", "", err
	}
	payloadHash := cronTokenHash(payloadJSON + "|" + action + "|" + owner.String())
	_, err = s.db.Exec(
		`INSERT INTO cron_confirmations
		 (token_hash, cancel_hash, platform, chat_id, user_id, action, payload_json, payload_hash, message_id, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		confirmHash, cancelHash, owner.Platform, owner.ChatID, owner.UserID,
		action, payloadJSON, payloadHash, messageID, unixNow(clock)+int64(cronTokenTTL/time.Second))
	if err != nil {
		return "", "", err
	}
	return confirmToken, cancelToken, nil
}

// ConsumedConfirmation describes which side of a pair was pressed and what
// was frozen into it.
type ConsumedConfirmation struct {
	Action      string
	PayloadJSON string
	MessageID   int
	Cancelled   bool
}

// ConsumeConfirmation burns a confirm or cancel token (either hash matches
// the same row) after verifying scope and TTL.
func (s *CronStore) ConsumeConfirmation(token string, owner CronOwner, clock cronClock) (*ConsumedConfirmation, error) {
	hash := cronTokenHash(token)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var row struct {
		tokenHash  string
		cancelHash string
		platform   string
		chatID     string
		userID     string
		action     string
		payload    string
		messageID  int
		expires    int64
		used       sql.NullInt64
	}
	e := tx.QueryRow(
		`SELECT token_hash, cancel_hash, platform, chat_id, user_id, action, payload_json, message_id, expires_at, used_at
		 FROM cron_confirmations WHERE token_hash = ? OR cancel_hash = ?`, hash, hash).Scan(
		&row.tokenHash, &row.cancelHash, &row.platform, &row.chatID, &row.userID,
		&row.action, &row.payload, &row.messageID, &row.expires, &row.used)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, errCronNotFound
	}
	if e != nil {
		return nil, e
	}
	if row.used.Valid {
		return nil, errCronNotFound
	}
	if row.platform != owner.Platform || row.chatID != owner.ChatID || row.userID != owner.UserID {
		// Scope mismatch: do not reveal state; caller audits separately via lookup failure path.
		return nil, errCronNotFound
	}
	if unixNow(clock) > row.expires {
		return nil, cronInvalid("확인 요청이 만료되었습니다. /cron 을 다시 실행해 주세요")
	}
	if _, e := tx.Exec(`UPDATE cron_confirmations SET used_at = ? WHERE token_hash = ? OR cancel_hash = ?`,
		unixNow(clock), row.tokenHash, row.cancelHash); e != nil {
		return nil, e
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ConsumedConfirmation{
		Action:      row.action,
		PayloadJSON: row.payload,
		MessageID:   row.messageID,
		Cancelled:   hash == row.cancelHash,
	}, nil
}

// --- Executions ---

// ScheduledExecution is a claimed due slot ready to be enqueued.
type ScheduledExecution struct {
	ExecutionID int64
	TriggerID   int64
	Slot        time.Time
	Job         CronJobRecord
	Owner       CronOwner
}

// ClaimDueExecutions atomically claims every due trigger slot: each claim
// inserts a unique execution row, advances the periodic schedule forward from
// the claimed slot (skip catch-up semantics) or retires the once trigger.
func (s *CronStore) ClaimDueExecutions(now time.Time) ([]ScheduledExecution, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT t.id, t.kind, t.spec, t.next_run_at,
		        j.id, j.task_prompt, j.enabled, j.revision, j.created_at, j.updated_at,
		        j.platform, j.chat_id, j.user_id
		 FROM cron_triggers t JOIN cron_jobs j ON j.id = t.job_id
		 WHERE t.fired_at IS NULL AND t.next_run_at IS NOT NULL AND t.next_run_at <= ?
		   AND j.enabled = 1 AND j.deleted_at IS NULL`, now.Unix())
	if err != nil {
		return nil, err
	}
	type dueRow struct {
		triggerID int64
		kind      string
		spec      string
		slot      int64
		job       CronJobRecord
		owner     CronOwner
	}
	var due []dueRow
	for rows.Next() {
		var r dueRow
		var enabled int
		var created, updated int64
		if err := rows.Scan(&r.triggerID, &r.kind, &r.spec, &r.slot,
			&r.job.ID, &r.job.TaskPrompt, &enabled, &r.job.Revision, &created, &updated,
			&r.owner.Platform, &r.owner.ChatID, &r.owner.UserID); err != nil {
			rows.Close()
			return nil, err
		}
		r.job.Enabled = true
		r.job.CreatedAt = time.Unix(created, 0)
		r.job.UpdatedAt = time.Unix(updated, 0)
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var claimed []ScheduledExecution
	for _, d := range due {
		slot := time.Unix(d.slot, 0)
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO cron_executions (trigger_id, scheduled_for, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`, d.triggerID, d.slot, ExecStatusPending, now.Unix(), now.Unix())
		if err != nil {
			return nil, err
		}
		inserted, _ := res.RowsAffected()
		if inserted == 0 {
			continue // another pass already claimed this exact slot
		}
		next, consume := cronNextFromSpec(d.kind, d.spec, slot.Add(time.Second))
		switch {
		case consume:
			if _, err := tx.Exec(`UPDATE cron_triggers SET fired_at = ? WHERE id = ?`, now.Unix(), d.triggerID); err != nil {
				return nil, err
			}
		case next != nil:
			if _, err := tx.Exec(`UPDATE cron_triggers SET next_run_at = ? WHERE id = ?`, next.Unix(), d.triggerID); err != nil {
				return nil, err
			}
		default:
			if _, err := tx.Exec(`UPDATE cron_triggers SET next_run_at = NULL WHERE id = ?`, d.triggerID); err != nil {
				return nil, err
			}
		}
		claimed = append(claimed, ScheduledExecution{
			ExecutionID: mustLastInsertID(res),
			TriggerID:   d.triggerID,
			Slot:        slot,
			Job:         d.job,
			Owner:       d.owner,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func mustLastInsertID(res sql.Result) int64 {
	id, err := res.LastInsertId()
	if err != nil {
		return 0
	}
	return id
}

func (s *CronStore) MarkExecution(id int64, status, errMsg string) {
	now := time.Now().Unix()
	if errMsg != "" {
		if _, err := s.db.Exec(`UPDATE cron_executions SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
			status, truncateRunes(errMsg, 500), now, id); err != nil {
			log.Printf("cron execution update failed: %v", err)
		}
		return
	}
	if _, err := s.db.Exec(`UPDATE cron_executions SET status = ?, updated_at = ? WHERE id = ?`,
		status, now, id); err != nil {
		log.Printf("cron execution update failed: %v", err)
	}
}

// InsertSkippedExecution records a missed once-slot without firing it.
func (s *CronStore) InsertSkippedExecution(triggerID int64, slot time.Time, now time.Time) {
	_, _ = s.db.Exec(
		`INSERT OR IGNORE INTO cron_executions (trigger_id, scheduled_for, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`, triggerID, slot.Unix(), ExecStatusSkipped, now.Unix(), now.Unix())
}

// ReconcileMissed repairs derived scheduler state from DB truth: unset
// schedules are computed, stale periodic slots jump forward from now (catch-up
// = skip) and missed once-slots are recorded as skipped and retired.
func (s *CronStore) ReconcileMissed(now time.Time, skipOlderThan time.Duration) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT t.id, t.kind, t.spec, COALESCE(t.next_run_at, 0), t.fired_at
		 FROM cron_triggers t JOIN cron_jobs j ON j.id = t.job_id
		 WHERE j.enabled = 1 AND j.deleted_at IS NULL AND t.fired_at IS NULL`)
	if err != nil {
		return 0, err
	}
	type fixup struct {
		id     int64
		kind   string
		spec   string
		next   int64
		retire bool // mark fired/retired instead of advancing
		advTo  *time.Time
	}
	var fixes []fixup
	for rows.Next() {
		var f fixup
		var fired sql.NullInt64
		if err := rows.Scan(&f.id, &f.kind, &f.spec, &f.next, &fired); err != nil {
			rows.Close()
			return 0, err
		}
		stale := f.next == 0 || time.Unix(f.next, 0).Before(now.Add(-skipOlderThan))
		if !stale {
			continue
		}
		if f.kind == TriggerKindOnce {
			f.retire = true
			fixes = append(fixes, f)
			continue
		}
		next, consume := cronNextFromSpec(f.kind, f.spec, now)
		if consume || next == nil {
			f.retire = true
			fixes = append(fixes, f)
			continue
		}
		f.advTo = next
		fixes = append(fixes, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	applied := 0
	for _, f := range fixes {
		if f.retire {
			if f.next != 0 && f.kind == TriggerKindOnce {
				s.InsertSkippedExecutionTx(tx, f.id, time.Unix(f.next, 0), now)
			}
			if _, err := tx.Exec(`UPDATE cron_triggers SET fired_at = ?, next_run_at = NULL WHERE id = ?`, now.Unix(), f.id); err != nil {
				return applied, err
			}
			applied++
			continue
		}
		if _, err := tx.Exec(`UPDATE cron_triggers SET next_run_at = ? WHERE id = ?`, f.advTo.Unix(), f.id); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, tx.Commit()
}

// InsertSkippedExecutionTx mirrors InsertSkippedExecution inside an open tx.
func (s *CronStore) InsertSkippedExecutionTx(tx *sql.Tx, triggerID int64, slot time.Time, now time.Time) {
	_, _ = tx.Exec(
		`INSERT OR IGNORE INTO cron_executions (trigger_id, scheduled_for, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`, triggerID, slot.Unix(), ExecStatusSkipped, now.Unix(), now.Unix())
}

// PurgeExpiredTokens lazily clears dead confirmation/target rows.
func (s *CronStore) PurgeExpiredTokens(now time.Time) {
	cutoff := now.Unix()
	_, _ = s.db.Exec(`DELETE FROM cron_target_refs WHERE expires_at < ? OR used_at IS NOT NULL`, cutoff-3600)
	_, _ = s.db.Exec(`DELETE FROM cron_confirmations WHERE expires_at < ? OR used_at IS NOT NULL`, cutoff-3600)
}

// String renders the owner tuple for logs and audit trails.
func (o CronOwner) String() string {
	return o.Platform + ":" + o.ChatID + ":" + o.UserID
}
