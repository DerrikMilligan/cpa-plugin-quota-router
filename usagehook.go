package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const rateLimitHeaderPrefix = "Anthropic-Ratelimit-Unified-"

// usageRecord is the part of the host's usage.handle record this plugin reads.
type usageRecord struct {
	AuthID          string
	Failed          bool
	Failure         struct{ StatusCode int }
	ResponseHeaders http.Header
}

// handleUsage folds a completed request's rate-limit headers into its
// account's reading, so the account carrying traffic updates between polls.
func (r *pluginRuntime) handleUsage(record usageRecord) {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return
	}
	if _, known := r.cache.snapshot(authID); !known {
		return
	}
	now := r.now()
	headers := record.ResponseHeaders
	fiveHour, fiveRejected := headerWindow(headers, "5h")
	weekly, weeklyRejected := headerWindow(headers, "7d")
	r.cache.recordHeaders(authID, fiveHour, weekly, now)
	if fiveRejected || weeklyRejected || (record.Failed && record.Failure.StatusCode == http.StatusTooManyRequests) {
		r.cache.recordRefusal(authID, now)
		r.log("info", "quota balancer saw an upstream refusal", map[string]any{"auth_id": authID})
		r.requestRefresh()
	}
}

// headerWindow reads one unified rate-limit window. Utilization arrives as a
// fraction (0.42) and the reset as Unix seconds.
func headerWindow(headers http.Header, window string) (*windowReading, bool) {
	prefix := rateLimitHeaderPrefix + window + "-"
	rejected := strings.EqualFold(strings.TrimSpace(headerValue(headers, prefix+"Status")), "rejected")
	utilization, err := strconv.ParseFloat(strings.TrimSpace(headerValue(headers, prefix+"Utilization")), 64)
	if err != nil || !finite(utilization) || utilization < 0 {
		return nil, rejected
	}
	reading := windowReading{Used: min(utilization*100, 100)}
	if reset, err := strconv.ParseFloat(strings.TrimSpace(headerValue(headers, prefix+"Reset")), 64); err == nil && reset > 0 {
		reading.ResetAt = time.Unix(int64(reset), 0)
	}
	if rejected {
		reading.Used = 100
	}
	return &reading, rejected
}

func headerValue(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
