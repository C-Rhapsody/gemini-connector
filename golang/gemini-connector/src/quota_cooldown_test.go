package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// makeQuotaDetail builds a realistic 429 error body whose reset timestamp is
// `offset` in the future, so tests stay valid regardless of wall-clock time.
// Fractional tails mirror the shape of real agy payloads.
func makeQuotaDetail(offset time.Duration) string {
	secsF := offset.Seconds()
	hms := fmt.Sprintf("%dh%02dm%02d", int(secsF)/3600, (int(secsF)%3600)/60, int(secsF)%60)
	tail := fmt.Sprintf("%.9f", secsF-math.Floor(secsF))[1:] // ".981701490"
	delay := hms + tail + "s"                                // e.g. "2h57m00.000000000s" like real payloads
	ts := quotaNow().Add(offset).UTC().Format(time.RFC3339)
	retry := fmt.Sprintf("%d%s", int(math.Ceil(secsF)), tail)
	return fmt.Sprintf(`failed to generate content: 429 Too Many Requests, body: {
"error": {
"code": 429,
"message": "You have exhausted your capacity on this model. Your quota will reset after %[1]s.",
"status": "RESOURCE_EXHAUSTED",
"details": [
{
"@type": "type.googleapis.com/google.rpc.ErrorInfo",
"reason": "QUOTA_EXHAUSTED",
"domain": "cloudcode-pa.googleapis.com",
"metadata": {
"uiMessage": "true",
"model": "gemini-3.1-flash-image",
"quotaResetDelay": "%[1]s",
"quotaResetTimeStamp": "%[2]s"
}
},
{
"@type": "type.googleapis.com/google.rpc.RetryInfo",
"retryDelay": "%[3]s"
}
]
}
}`, delay, ts, retry)
}

// resetQuotaState clears package state between tests and redirects the
// persistence directory.
func resetQuotaState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	transcriptDirOverride = dir
	t.Cleanup(func() {
		quotaMu.Lock()
		quotaState = nil
		quotaRestored = false
		quotaMu.Unlock()
		transcriptDirOverride = ""
	})
	quotaMu.Lock()
	quotaState = nil
	quotaRestored = true // keep tests from reading/writing disk implicitly
	quotaMu.Unlock()
}

func TestQuotaCapture_PrefersTimestamp(t *testing.T) {
	resetQuotaState(t)
	if !QuotaCapture(makeQuotaDetail(2*time.Hour + 57*time.Minute)) {
		t.Fatal("capture should succeed for a fresh sample detail")
	}
	rem := QuotaRemaining()
	expected := 2*time.Hour + 57*time.Minute
	if rem > expected || rem < expected-time.Minute {
		t.Fatalf("remaining %v should track the timestamp (~%v)", rem, expected)
	}
}

func TestQuotaCapture_FallsBackToDelays(t *testing.T) {
	resetQuotaState(t)
	detail := makeQuotaDetail(2 * time.Hour)
	noTs := regexp.MustCompile(`"quotaResetTimeStamp":[^\n]*\n`).ReplaceAllString(detail, "")
	if !strings.Contains(noTs, "quotaResetDelay") {
		t.Fatal("fixture lost the delay field")
	}
	if !QuotaCapture(noTs) {
		t.Fatal("capture should fall back to quotaResetDelay")
	}
	rem := QuotaRemaining()
	if rem <= 0 || rem > 2*time.Hour {
		t.Fatalf("remaining %v not consistent with ~%v delay", rem, 2*time.Hour)
	}
}

func TestQuotaCapture_MalformedIsIgnored(t *testing.T) {
	resetQuotaState(t)
	bad := "failed to generate content: 429 Too Many Requests, body: { RESOURCE_EXHAUSTED }"
	if QuotaCapture(bad) {
		t.Fatal("malformed 429 must not start a cooldown")
	}
	if QuotaActive() {
		t.Fatal("no cooldown expected")
	}
}

func TestQuotaCapture_NonQuotaErrorIgnored(t *testing.T) {
	resetQuotaState(t)
	if QuotaCapture("invalid arguments:\n- missing property 'toolSummary'") {
		t.Fatal("non-quota errors must not start a cooldown")
	}
}

func TestQuotaRefreshedDetail_DecrementsAllTimeFields(t *testing.T) {
	resetQuotaState(t)
	if !QuotaCapture(makeQuotaDetail(2*time.Hour + 57*time.Minute)) {
		t.Fatal("capture failed")
	}
	got := QuotaRefreshedDetail()

	for _, frag := range []string{"reset after ", "quotaResetDelay\": \"", "retryDelay\": \""} {
		if !strings.Contains(got, frag) {
			t.Errorf("refreshed detail missing %q", frag)
		}
	}
	if regexp.MustCompile(`reset after 2h5[67]m\d+s`).FindString(got) == "" {
		t.Errorf("text field should show remaining ~2h57m with plain seconds: %q", got)
	}
	if !strings.Contains(got, `"model": "gemini-3.1-flash-image"`) {
		t.Errorf("non-time fields must stay untouched: %q", got)
	}
	if strings.Contains(got, "000000000") {
		t.Errorf("fractional tails should be replaced by whole seconds: %q", got)
	}
	if !regexp.MustCompile(`"retryDelay": "\d+s"`).MatchString(got) {
		t.Errorf("retryDelay should become remaining seconds: %q", got)
	}
}

func TestQuotaPersistence_RoundTripAndExpiry(t *testing.T) {
	resetQuotaState(t)
	quotaRestored = false
	if !QuotaCapture(makeQuotaDetail(2*time.Hour + 57*time.Minute)) {
		t.Fatal("capture failed")
	}
	path := filepath.Join(transcriptDirOverride, "quota_cooldown.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cooldown file not persisted: %v", err)
	}

	// Simulate restart.
	quotaMu.Lock()
	quotaState = nil
	quotaMu.Unlock()
	if QuotaActive() {
		t.Fatal("state should be empty before restore")
	}
	if RestoreQuotaCooldown() <= 0 {
		t.Fatal("restore should recover an active cooldown")
	}
	if !QuotaActive() {
		t.Fatal("restored cooldown should be active")
	}

	// Simulate restart after expiry: the on-disk state is what a restart
	// would read, so overwrite it with an expired cooldown.
	quotaMu.Lock()
	quotaState = nil
	quotaRestored = false
	quotaMu.Unlock()
	expiredData, _ := json.Marshal(quotaCooldown{ResetAt: quotaNow().Add(-time.Minute), RawDetail: "x"})
	if err := os.WriteFile(path, expiredData, 0644); err != nil {
		t.Fatalf("failed to write expired fixture: %v", err)
	}
	if RestoreQuotaCooldown() != 0 {
		t.Fatal("expired cooldown should restore as zero")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired cooldown file should be removed")
	}
}

func TestExecuteAgy_BlockedDuringCooldown(t *testing.T) {
	resetQuotaState(t)
	if !QuotaCapture(makeQuotaDetail(2*time.Hour + 57*time.Minute)) {
		t.Fatal("capture failed")
	}
	_, err := executeAgy(context.Background(), "hello", "")
	if err == nil {
		t.Fatal("executeAgy must fail during cooldown without spawning agy")
	}
	ae, ok := err.(*AgyError)
	if !ok || ae.Type != "quota_cooldown" {
		t.Fatalf("expected quota_cooldown error, got %#v", err)
	}
	if !strings.Contains(ae.Detail, "429 Too Many Requests") {
		t.Fatalf("detail should carry refreshed original text: %q", ae.Detail)
	}
}

func TestQuotaBlockErr(t *testing.T) {
	resetQuotaState(t)
	if quotaBlockErr(false) != nil {
		t.Fatal("inactive cooldown must not block")
	}
	if !QuotaCapture(makeQuotaDetail(2 * time.Hour)) {
		t.Fatal("capture failed")
	}
	if quotaBlockErr(true) != nil {
		t.Fatal("bypass must be allowed even during active cooldown")
	}
	ae := quotaBlockErr(false)
	if ae == nil || ae.Type != "quota_cooldown" {
		t.Fatalf("active cooldown must block with quota_cooldown error, got %#v", ae)
	}
}

func TestRecordStuckError_CategoriesAreIndependent(t *testing.T) {
	cfg := &Config{}
	if cfg.recordStuckError("invalid_args", "boom") {
		t.Fatal("first occurrence must not trigger")
	}
	if cfg.recordStuckError("url_fetch", "fetch failed") {
		t.Fatal("first occurrence of other category must not trigger")
	}
	if !cfg.recordStuckError("invalid_args", "boom") {
		t.Fatal("second identical invalid_args should trigger")
	}
	cfg.applyNewConversation("new-id")
	if cfg.lastInvalidArgs != "" || cfg.consecInvalidArgs != 0 ||
		cfg.lastUrlFetch != "" || cfg.consecUrlFetch != 0 {
		t.Fatal("applyNewConversation must clear stuck counters")
	}
	if cfg.recordStuckError("url_fetch", "fetch failed") {
		t.Fatal("counters restart after conversation change")
	}
}
