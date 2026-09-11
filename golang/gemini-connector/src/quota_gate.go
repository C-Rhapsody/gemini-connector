package main

import "log"

// quotaBlockErr returns the rejection error for gated calls during an active
// quota cooldown, or nil when the call may proceed.
func quotaBlockErr(bypass bool) *AgyError {
	if bypass || !QuotaActive() {
		return nil
	}
	log.Printf("agy call blocked by quota cooldown (%s remaining)", formatQuotaDuration(QuotaRemaining()))
	return &AgyError{Type: "quota_cooldown", Detail: QuotaRefreshedDetail()}
}
