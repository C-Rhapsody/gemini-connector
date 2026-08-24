package main

import (
	"fmt"
)

const cronSchemaV1 = `
CREATE TABLE IF NOT EXISTS cron_jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	platform    TEXT NOT NULL,
	chat_id     TEXT NOT NULL,
	user_id     TEXT NOT NULL,
	task_prompt TEXT NOT NULL,
	enabled     INTEGER NOT NULL DEFAULT 1,
	revision    INTEGER NOT NULL DEFAULT 1,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	deleted_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cron_jobs_owner ON cron_jobs(platform, chat_id, user_id);

CREATE TABLE IF NOT EXISTS cron_triggers (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id      INTEGER NOT NULL REFERENCES cron_jobs(id),
	kind        TEXT NOT NULL,
	spec        TEXT NOT NULL,
	next_run_at INTEGER,
	fired_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cron_triggers_due ON cron_triggers(next_run_at);

CREATE TABLE IF NOT EXISTS cron_target_refs (
	token_hash TEXT PRIMARY KEY,
	platform   TEXT NOT NULL,
	chat_id    TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	job_id     INTEGER NOT NULL,
	revision   INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
);

CREATE TABLE IF NOT EXISTS cron_confirmations (
	token_hash       TEXT PRIMARY KEY,
	cancel_hash      TEXT NOT NULL UNIQUE,
	platform         TEXT NOT NULL,
	chat_id          TEXT NOT NULL,
	user_id          TEXT NOT NULL,
	action           TEXT NOT NULL,
	payload_json     TEXT NOT NULL,
	payload_hash     TEXT NOT NULL,
	message_id       INTEGER NOT NULL DEFAULT 0,
	expires_at       INTEGER NOT NULL,
	used_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_cron_confirmations_cancel ON cron_confirmations(cancel_hash);

CREATE TABLE IF NOT EXISTS cron_idempotency (
	key           TEXT PRIMARY KEY,
	response_text TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cron_executions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	trigger_id    INTEGER NOT NULL REFERENCES cron_triggers(id),
	scheduled_for INTEGER NOT NULL,
	status        TEXT NOT NULL,
	error         TEXT,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	UNIQUE(trigger_id, scheduled_for)
);

CREATE TABLE IF NOT EXISTS cron_audit_log (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	ts              INTEGER NOT NULL,
	actor_platform  TEXT,
	actor_chat_id   TEXT,
	actor_user_id   TEXT,
	event           TEXT NOT NULL,
	severity        TEXT NOT NULL,
	detail          TEXT
);

CREATE TABLE IF NOT EXISTS cron_scheduler_state (
	id     INTEGER PRIMARY KEY CHECK (id = 1),
	killed INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO cron_scheduler_state (id, killed) VALUES (1, 0);
`

// migrate brings the database up to the current schema version. Version 1 is
// the initial layout; future migrations chain from user_version.
func (s *CronStore) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("cron schema version read: %w", err)
	}
	switch {
	case version > cronSchemaVersionNum:
		return fmt.Errorf("cron database is newer (v%d) than this binary supports (v%d)", version, cronSchemaVersionNum)
	case version == cronSchemaVersionNum:
		return nil
	case version == 0:
		if _, err := s.db.Exec(cronSchemaV1); err != nil {
			return fmt.Errorf("cron schema create: %w", err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", cronSchemaVersionNum)); err != nil {
			return fmt.Errorf("cron schema stamp: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid cron schema version %d", version)
	}
}

const cronSchemaVersionNum = 1
