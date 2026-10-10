package auth

import (
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

// Credential denial cooldown: an upstream answering 402 or 403 has refused the
// account itself, not one model, so the whole credential cools down with backoff
// (30m, 1h, 2h, 4h, capped at 6h). A success or new credentials (including a
// routine token refresh) clear it. A 403 that names one model stays per-model.
// The backoff level is kept only in memory and in the live cooldown record, so it
// resets to 30m after a restart or when a quota cooldown rewrites Reason (fail open).
// 401 keeps its own handling: token refresh, then the terminal unauthorized state.
const (
	credentialDenialBackoffBase = 30 * time.Minute
	credentialDenialBackoffMax  = 6 * time.Hour
)

// credentialDenialReason names the credential-wide cooldown for a denial status,
// or returns "" when status is not a denial.
func credentialDenialReason(status int) string {
	switch status {
	case http.StatusPaymentRequired:
		return "payment_required"
	case http.StatusForbidden:
		return "forbidden"
	default:
		return ""
	}
}

func isCredentialDenialReason(reason string) bool {
	switch reason {
	case "payment_required", "forbidden":
		return true
	default:
		return false
	}
}

// isCredentialWideCooldown reports whether quota holds a live cooldown that blocks
// the credential for every model: a credential-scoped quota or a denial.
func isCredentialWideCooldown(quota QuotaState, now time.Time) bool {
	if !quota.Exceeded || !quota.NextRecoverAt.After(now) {
		return false
	}
	return quota.Reason == "credential_quota" || isCredentialDenialReason(quota.Reason)
}

// IsCredentialWideCooldown reports whether the auth is cooling down for every model.
func IsCredentialWideCooldown(auth *Auth, now time.Time) bool {
	return auth != nil && isCredentialWideCooldown(auth.Quota, now)
}

// applyCredentialDenialCooldown cools the whole credential after a denial status and
// returns the deadline. Denials that land inside a live window reuse it, so a burst
// of in-flight failures advances the ladder at most once per window.
func applyCredentialDenialCooldown(auth *Auth, status int, now time.Time) time.Time {
	reason := credentialDenialReason(status)
	if auth == nil || reason == "" {
		return time.Time{}
	}
	if auth.Quota.Reason == "credential_quota" && auth.Quota.Exceeded &&
		!auth.Quota.NextRecoverAt.Before(now.Add(credentialDenialBackoffBase)) {
		// A longer live credential quota window already covers the shortest denial
		// window: keep it rather than shorten it.
		auth.Unavailable = true
		auth.NextRetryAfter = auth.Quota.NextRecoverAt
		return auth.Quota.NextRecoverAt
	}
	level := 0
	if isCredentialDenialReason(auth.Quota.Reason) {
		level = auth.Quota.BackoffLevel
		if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
			auth.Quota.Reason = reason
			auth.Unavailable = true
			auth.NextRetryAfter = auth.Quota.NextRecoverAt
			return auth.Quota.NextRecoverAt
		}
	}
	if level < 0 {
		level = 0
	}
	cooldown := credentialDenialBackoffMax
	if level < 4 {
		cooldown = credentialDenialBackoffBase << level
	}
	if cooldown > credentialDenialBackoffMax {
		cooldown = credentialDenialBackoffMax
	}
	next := now.Add(cooldown).Round(0)
	auth.Quota.Exceeded = true
	auth.Quota.Reason = reason
	auth.Quota.NextRecoverAt = next
	auth.Quota.BackoffLevel = level + 1
	auth.Unavailable = true
	auth.NextRetryAfter = next
	account := SafeAccountName(auth)
	if account == "" {
		account = auth.ID
	}
	log.Warnf("credential cooldown: %s status=%d until=%s", account, status, next.UTC().Format(time.RFC3339))
	return next
}

// clearCredentialDenial drops a denial cooldown and its backoff level after a success.
func clearCredentialDenial(auth *Auth) bool {
	if auth == nil || !isCredentialDenialReason(auth.Quota.Reason) {
		return false
	}
	applyCooldownFields(&auth.Quota, QuotaState{})
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	return true
}
