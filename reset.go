package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// resetRequest is the body of POST /v0/management/plugins/quota-balancer/reset.
type resetRequest struct {
	Account string `json:"account"` // auth ID or email
	GrantID string `json:"grant_id"`
}

type resetResponse struct {
	Result  string `json:"result"`
	Message string `json:"message"`
	GrantID string `json:"grant_id,omitempty"`
}

var resetMessages = map[string]string{
	"reset":         "Reset applied. The cleared windows read 0 until the next usage read confirms.",
	"already_used":  "Anthropic reports this reset was already applied.",
	"not_limited":   "Anthropic declined: the account is not at a limit, so the reset was not used.",
	"cooldown":      "Anthropic declined: resets are cooling down for this account.",
	"ineligible":    "Anthropic declined: the account is not eligible for this reset.",
	"unavailable":   "Anthropic declined: the reset is unavailable.",
	"rate_limited":  "Anthropic rate-limited the request; the reset was not used. Try again later.",
	"auth_error":    "Anthropic rejected the account credential; the reset was not used.",
	"unknown":       "The outcome is unknown (the request may have been applied). It was not retried; check the card after the next usage read before trying again.",
	"not_usable":    "This reset is not usable now.",
	"in_progress":   "A reset for this account is already in progress.",
	"no_account":    "No such account.",
	"no_credential": "The account credential or organization ID could not be read.",
}

var resetHTTPClient = &http.Client{
	Timeout: resetClaimTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type resetLocks struct {
	mu     sync.Mutex
	active map[string]bool
}

func (l *resetLocks) acquire(authID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == nil {
		l.active = map[string]bool{}
	}
	if l.active[authID] {
		return false
	}
	l.active[authID] = true
	return true
}

func (l *resetLocks) release(authID string) {
	l.mu.Lock()
	delete(l.active, authID)
	l.mu.Unlock()
}

func respond(result, grantID string) resetResponse {
	return resetResponse{Result: result, Message: resetMessages[result], GrantID: grantID}
}

// useReset spends one of an account's usage-limit resets. Like
// cpa-usage-keeper it never retries: a dropped connection may arrive after
// Anthropic has already applied the reset.
func (r *pluginRuntime) useReset(ctx context.Context, req resetRequest) resetResponse {
	authID, sample, ok := r.findAccount(req.Account)
	if !ok {
		return respond("no_account", "")
	}
	now := r.now()
	grantID := strings.TrimSpace(req.GrantID)
	selected, usable := sample.Grants.selectGrant(now)
	if !usable || (grantID != "" && grantID != selected.ID) {
		return respond("not_usable", grantID)
	}
	grantID = selected.ID
	if !r.resets.acquire(authID) {
		return respond("in_progress", grantID)
	}
	defer r.resets.release(authID)

	rawAuth, err := r.host.getAuth(sample.AuthIndex)
	if err != nil {
		return respond("no_credential", grantID)
	}
	var credential struct {
		AccessToken      string `json:"access_token"`
		OrganizationUUID string `json:"organization_uuid"`
	}
	if json.Unmarshal(rawAuth, &credential) != nil || strings.TrimSpace(credential.AccessToken) == "" ||
		!uuidPattern.MatchString(credential.OrganizationUUID) || !grantIDPattern.MatchString(grantID) {
		return respond("no_credential", grantID)
	}

	result := r.claimReset(ctx, credential.AccessToken, strings.ToLower(credential.OrganizationUUID), grantID)
	r.log("info", "quota balancer reset requested", map[string]any{"auth_id": authID, "grant_id": grantID, "result": result})
	if result == "reset" || result == "already_used" {
		r.applyReset(authID, selected, r.now())
	}
	return respond(result, grantID)
}

func (r *pluginRuntime) claimReset(ctx context.Context, token, organizationID, grantID string) string {
	requestID, err := newRequestID()
	if err != nil {
		return "unknown"
	}
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": grantID, "request_id": requestID})
	ctx, cancel := context.WithTimeout(ctx, resetClaimTimeout)
	defer cancel()
	url := claudeAPIBase + "/api/organizations/" + organizationID + "/reset_rate_limits"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	req.Header.Set("User-Agent", claudeCLIUserAgent)
	resp, err := resetHTTPClient.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "rate_limited"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "auth_error"
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "unknown"
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes))
	if err != nil {
		return "unknown"
	}
	var payload struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "unknown"
	}
	switch payload.Result {
	case "reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable":
		return payload.Result
	}
	return "unknown"
}

// applyReset zeroes the windows the grant clears so routing can use the
// account at once, spends the grant locally, and queues a confirming read.
func (r *pluginRuntime) applyReset(authID string, grant resetGrant, at time.Time) {
	r.cache.update(authID, func(s *quotaSample) {
		for _, window := range grant.Clears {
			switch window {
			case "five_hour":
				s.FiveHour.Used = 0
			case "seven_day":
				s.Weekly.Used = 0
			}
		}
		s.HeaderAt = at
		s.RefusedAt = time.Time{}
		s.BackoffUntil = time.Time{}
		s.ForceRead = true
		if s.Grants != nil {
			copied := *s.Grants
			copied.Grants = append([]resetGrant(nil), s.Grants.Grants...)
			for i := range copied.Grants {
				if copied.Grants[i].ID == grant.ID && copied.Grants[i].ResetsLeft > 0 {
					copied.Grants[i].ResetsLeft--
				}
			}
			s.Grants = &copied
		}
	})
	r.requestRefresh()
}

// findAccount resolves an auth ID or (case-insensitive) email.
func (r *pluginRuntime) findAccount(account string) (string, quotaSample, bool) {
	account = strings.TrimSpace(account)
	if account == "" {
		return "", quotaSample{}, false
	}
	if sample, ok := r.cache.snapshot(account); ok {
		return account, sample, true
	}
	for id, sample := range r.cache.all() {
		if strings.EqualFold(sample.Email, account) {
			return id, sample, true
		}
	}
	return "", quotaSample{}, false
}

func newRequestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
