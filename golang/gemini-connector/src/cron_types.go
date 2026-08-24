package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// Schema and protocol constants for the /cron scheduled-task DSL. Go is the
// sole authority: agy only produces candidate JSON that must survive strict
// validation here before anything touches storage or the scheduler.
const (
	cronSchemaVersion = 1
	cronCommandKind   = "cron_command"

	CronActionCreate = "create"
	CronActionModify = "modify"
	CronActionDelete = "delete"
	CronActionPause  = "pause"
	CronActionResume = "resume"

	TriggerKindPeriodic = "periodic"
	TriggerKindOnce     = "once"

	ExecStatusPending   = "pending"
	ExecStatusRunning   = "running"
	ExecStatusSuccess   = "success"
	ExecStatusFailed    = "failed"
	ExecStatusSkipped   = "skipped"
	ExecStatusCancelled = "cancelled"
)

// Policy limits for the cron subsystem.
const (
	cronTokenBytes      = 8                // opaque token entropy; hex-encoded into callback data
	cronTokenTTL        = 10 * time.Minute // target_ref and confirmation lifetime
	cronMinInterval     = 15 * time.Minute // minimum periodic execution gap
	cronIntervalHorizon = 366 * 24 * time.Hour
	cronMaxGapProbe     = 100000 // upper bound of Next() probes when measuring gaps
	cronMaxPromptRunes  = 4000
	cronMaxTriggersJob  = 10
	cronMaxJobsUser     = 20
	cronMaxTriggersUser = 100
	// cronSkipThreshold: missed slots older than this are skipped (not
	// replayed) by reconcile; fresher ones are left for the tick loop.
	cronSkipThreshold = time.Minute
)

// Callback data prefixes. Telegram caps callback_data at 64 bytes; with an
// 8-byte hex token every prefix below stays far below the limit.
const (
	cronCbConfirm   = "cr:c:"
	cronCbCancel    = "cr:x:"
	cronCbSelectRef = "cr:s:" // format: cr:s:<confirmToken>:<targetRefToken>
)

// CronOwner is the ownership tuple every job query is scoped by.
type CronOwner struct {
	Platform string
	ChatID   string
	UserID   string
}

func (o CronOwner) complete() bool {
	return o.Platform != "" && o.ChatID != "" && o.UserID != ""
}

// StoredTrigger is a persisted trigger row. NextRunAt/FiredAt are unix
// seconds (0 = unset).
type StoredTrigger struct {
	ID        int64
	Kind      string
	Cron      string
	At        string
	Timezone  string
	NextRunAt int64
	FiredAt   int64
}

// SpecJSON returns the canonical persisted representation of the trigger.
func (t StoredTrigger) SpecJSON() string {
	b, err := json.Marshal(map[string]string{"kind": t.Kind, "cron": t.Cron, "at": t.At, "timezone": t.Timezone})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// CronJobRecord is a job plus its triggers as returned by the store.
type CronJobRecord struct {
	ID         int64
	TaskPrompt string
	Enabled    bool
	Revision   int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Triggers   []StoredTrigger
}

// Summary renders a one-line human-readable job description for lists,
// planner context and confirmation dialogs.
func (j CronJobRecord) Summary() string {
	state := "▶"
	if !j.Enabled {
		state = "⏸"
	}
	return fmt.Sprintf("#%d %s %s", j.ID, state, truncateRunes(j.TaskPrompt, 60))
}

// cronCommandSpec mirrors the wire DSL exactly. Unknown fields are rejected
// at decode time; per-action field rules are enforced in validation.
type cronCommandSpec struct {
	Version    int               `json:"version"`
	Kind       string            `json:"kind"`
	Action     string            `json:"action"`
	TargetRef  *string           `json:"target_ref,omitempty"`
	TaskPrompt *string           `json:"task_prompt,omitempty"`
	Triggers   []cronTriggerSpec `json:"triggers,omitempty"`
}

type cronTriggerSpec struct {
	Kind     string `json:"kind"`
	Cron     string `json:"cron,omitempty"`
	At       string `json:"at,omitempty"`
	Timezone string `json:"timezone"`
}

// cronConfirmPayload is the JSON stored alongside a confirmation token. It
// freezes the exact change the user is about to approve.
type cronConfirmPayload struct {
	Spec       cronCommandSpec `json:"spec"`
	JobID      int64           `json:"job,omitempty"`
	Revision   int64           `json:"rev,omitempty"`
	BeforeText string          `json:"before,omitempty"`
}

// cronValidationError carries a user-presentable rejection reason.
type cronValidationError struct{ msg string }

func (e *cronValidationError) Error() string { return e.msg }

func cronInvalid(format string, args ...interface{}) error {
	return &cronValidationError{msg: fmt.Sprintf(format, args...)}
}
