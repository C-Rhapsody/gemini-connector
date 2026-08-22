package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// quotaCooldown holds the state of an active Gemini quota exhaustion (429
// RESOURCE_EXHAUSTED / QUOTA_EXHAUSTED). While it is active, the connector
// does not spawn agy at all and instead replies with the original error text
// whose time fields are refreshed to the remaining time.
type quotaCooldown struct {
	ResetAt   time.Time `json:"reset_at"`
	Model     string    `json:"model,omitempty"`
	RawDetail string    `json:"raw_detail"`
}

var (
	quotaState    *quotaCooldown
	quotaMu       sync.Mutex
	quotaNow      = time.Now // overridable in tests
	quotaRestored bool       // guards one-time disk restore
)

var (
	quotaStatusRe         = regexp.MustCompile(`RESOURCE_EXHAUSTED|QUOTA_EXHAUSTED`)
	quota429Re            = regexp.MustCompile(`429`)
	quotaModelRe          = regexp.MustCompile(`"model":\s*"([^"]+)"`)
	quotaResetTimestampRe = regexp.MustCompile(`"quotaResetTimeStamp":\s*"([^"]+)"`)
	quotaResetDelayRe     = regexp.MustCompile(`"quotaResetDelay":\s*"([^"]+)"`)
	quotaRetryDelayRe     = regexp.MustCompile(`"retryDelay":\s*"([^"]+)"`)
	quotaResetAfterRe     = regexp.MustCompile(`reset after ((\d+)h)?((\d+)m)?((\d+(?:\.\d+)?)s)?`)
	quotaResetAfterTxtRe  = regexp.MustCompile(`(reset after )\d+h\d+m\d+(?:\.\d+)?s`)
	quotaResetDelayValRe  = regexp.MustCompile(`("quotaResetDelay":\s*")[^"]*(")`)
	quotaRetryDelayValRe  = regexp.MustCompile(`("retryDelay":\s*")[^"]*(")`)
)

func isQuotaExhaustedDetail(detail string) bool {
	return quota429Re.MatchString(detail) && quotaStatusRe.MatchString(detail)
}

func quotaCooldownPath() string {
	dir := transcriptDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "quota_cooldown.json")
}

// QuotaCapture inspects an agy error detail and, when it reports quota
// exhaustion, stores a cooldown until the reset time. Returns true when a
// cooldown was captured. Resolution order: absolute timestamp first, then
// relative delays.
func QuotaCapture(detail string) bool {
	if !isQuotaExhaustedDetail(detail) {
		return false
	}
	now := quotaNow()
	var resetAt time.Time

	if m := quotaResetTimestampRe.FindStringSubmatch(detail); m != nil {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			resetAt = t
		}
	}
	if resetAt.IsZero() {
		for _, re := range []*regexp.Regexp{quotaResetDelayRe, quotaRetryDelayRe} {
			if m := re.FindStringSubmatch(detail); m != nil {
				if d, err := time.ParseDuration(m[1]); err == nil && d > 0 {
					resetAt = now.Add(d)
					break
				}
			}
		}
	}
	if resetAt.IsZero() {
		if m := quotaResetAfterRe.FindStringSubmatch(detail); m != nil {
			h := atoiDefault(m[2])
			min := atoiDefault(m[4])
			secF := atofDefault(m[6])
			d := time.Duration(h)*time.Hour + time.Duration(min)*time.Minute +
				time.Duration(secF*float64(time.Second))
			if d > 0 {
				resetAt = now.Add(d)
			}
		}
	}
	if resetAt.IsZero() || !resetAt.After(now) {
		log.Printf("Quota exhaustion seen but no usable reset time found; not entering cooldown")
		return false
	}

	q := &quotaCooldown{ResetAt: resetAt, RawDetail: detail}
	if m := quotaModelRe.FindStringSubmatch(detail); m != nil {
		q.Model = m[1]
	}

	quotaMu.Lock()
	quotaState = q
	quotaMu.Unlock()
	saveQuotaCooldown(q)
	log.Printf("Quota cooldown captured until %s (model: %s)", resetAt.Format(time.RFC3339), q.Model)
	return true
}

// RestoreQuotaCooldown reloads a persisted cooldown after a restart and
// discards it when already expired. It returns the remaining duration (0 if
// none).
func RestoreQuotaCooldown() time.Duration {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if quotaRestored {
		return remainingQuotaLocked()
	}
	quotaRestored = true

	path := quotaCooldownPath()
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var q quotaCooldown
	if err := json.Unmarshal(data, &q); err != nil || q.ResetAt.IsZero() {
		_ = os.Remove(path)
		return 0
	}
	if !q.ResetAt.After(quotaNow()) {
		_ = os.Remove(path)
		return 0
	}
	quotaState = &q
	return q.ResetAt.Sub(quotaNow())
}

// QuotaActive reports whether agy calls are currently blocked.
func QuotaActive() bool {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	return remainingQuotaLocked() > 0
}

// QuotaRemaining returns how long until the cooldown lifts (0 when inactive).
func QuotaRemaining() time.Duration {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	return remainingQuotaLocked()
}

func remainingQuotaLocked() time.Duration {
	if quotaState == nil {
		return 0
	}
	rem := quotaState.ResetAt.Sub(quotaNow())
	if rem <= 0 {
		return 0
	}
	return rem
}

// QuotaClear drops the cooldown entirely (e.g. after a successful turn or a
// manual reset). The persisted file is removed only when a state existed.
func QuotaClear() {
	quotaMu.Lock()
	had := quotaState != nil
	quotaState = nil
	quotaMu.Unlock()
	if had {
		if path := quotaCooldownPath(); path != "" {
			_ = os.Remove(path)
		}
		log.Println("Quota cooldown cleared")
	}
}

// QuotaRefreshedDetail returns the stored raw error text with its three time
// fields replaced by the current remaining time. The absolute timestamp stays
// untouched.
func QuotaRefreshedDetail() string {
	quotaMu.Lock()
	q := quotaState
	quotaMu.Unlock()
	if q == nil {
		return ""
	}
	rem := q.ResetAt.Sub(quotaNow())
	if rem < 0 {
		rem = 0
	}
	rs := formatQuotaDuration(rem)
	out := q.RawDetail
	out = quotaResetAfterTxtRe.ReplaceAllString(out, "${1}"+rs)
	out = quotaResetDelayValRe.ReplaceAllString(out, "${1}"+rs+"${2}")
	out = quotaRetryDelayValRe.ReplaceAllString(out, "${1}"+fmt.Sprintf("%ds", int(math.Ceil(rem.Seconds())))+"${2}")
	return out
}

// formatQuotaDuration renders a duration as h/m/s with seconds rounded up,
// matching the style agy uses ("2h57m19s").
func formatQuotaDuration(d time.Duration) string {
	total := int(math.Ceil(d.Seconds()))
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
}

func saveQuotaCooldown(q *quotaCooldown) {
	path := quotaCooldownPath()
	if path == "" {
		return
	}
	if dir := transcriptDir(); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("Failed to persist quota cooldown: %v", err)
	}
}

func atoiDefault(s string) int {
	var v int
	fmt.Sscanf(s, "%d", &v)
	return v
}

func atofDefault(s string) float64 {
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}
