package main

import (
	"strings"
	"sync"
	"time"
)

type quotaSample struct {
	AuthIndex string
	Name      string
	Email     string
	Identity  string
	HasSample bool
	FiveHour  windowReading
	Weekly    windowReading
	// PolledAt is the last successful usage-endpoint read; HeaderAt the last
	// rate-limit headers seen on a proxied response for this account.
	PolledAt          time.Time
	HeaderAt          time.Time
	RefusedAt         time.Time
	LastAttemptAt     time.Time
	LastErrorCategory string
	// BackoffUntil holds usage reads for an account the endpoint rate-limited;
	// RateLimits counts consecutive 429s to grow the backoff.
	BackoffUntil time.Time
	RateLimits   int
	// Grants are the usage-limit resets from the last read that reported them.
	Grants *resetGrants
	// ForceRead asks the worker to read this account on its next pass.
	ForceRead bool
	// GrantsChecked is set by this runtime's first successful endpoint read,
	// so a restart reads each account once promptly for its grants.
	GrantsChecked bool
}

// used returns a window's percent used, reading 0 once the window has reset:
// a rolled-over window starts empty.
func (w windowReading) used(now time.Time) float64 {
	if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
		return 0
	}
	return w.Used
}

func (s quotaSample) seenAt() time.Time {
	if s.HeaderAt.After(s.PolledAt) {
		return s.HeaderAt
	}
	return s.PolledAt
}

func (s quotaSample) fresh(now time.Time, maxStaleness time.Duration) bool {
	return s.HasSample && now.Sub(s.seenAt()) <= maxStaleness
}

// refused reports an upstream rate-limit refusal not yet cleared by a later read.
func (s quotaSample) refused() bool {
	return !s.RefusedAt.IsZero() && !s.seenAt().After(s.RefusedAt)
}

type quotaCache struct {
	mu      sync.Mutex
	samples map[string]quotaSample
	// version counts changes so the state file is written only when needed.
	version uint64
}

func (c *quotaCache) reconcile(auths []physicalClaudeAuth) {
	keep := make(map[string]struct{}, len(auths))
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, auth := range auths {
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		keep[auth.ID] = struct{}{}
		sample := c.samples[auth.ID]
		if sample.Identity != "" && auth.Identity != "" && sample.Identity != auth.Identity {
			sample = quotaSample{}
		}
		sample.AuthIndex = auth.AuthIndex
		sample.Name = auth.Name
		sample.Email = auth.Email
		sample.Identity = auth.Identity
		c.samples[auth.ID] = sample
	}
	for authID := range c.samples {
		if _, ok := keep[authID]; !ok {
			delete(c.samples, authID)
		}
	}
}

func (c *quotaCache) update(authID string, apply func(*quotaSample)) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	apply(&sample)
	c.samples[authID] = sample
	c.version++
	c.mu.Unlock()
}

func (c *quotaCache) recordAttempt(authID string, at time.Time) {
	c.update(authID, func(s *quotaSample) {
		s.LastAttemptAt = at
		s.LastErrorCategory = ""
		// A forced read is one attempt, even a failed one, so it never loops.
		s.ForceRead = false
	})
}

func (c *quotaCache) recordPoll(authID string, result usageResult, at time.Time) {
	c.update(authID, func(s *quotaSample) {
		s.HasSample = true
		s.FiveHour, s.Weekly = result.FiveHour, result.Weekly
		s.PolledAt = at
		s.LastErrorCategory = ""
		s.BackoffUntil, s.RateLimits = time.Time{}, 0
		s.ForceRead = false
		s.GrantsChecked = true
		if result.Grants != nil {
			s.Grants = result.Grants
		}
	})
}

// recordRateLimit backs an account off the usage endpoint: the server's
// Retry-After when given, else the poll interval doubled per consecutive 429,
// capped at maxRateLimitBackoff.
func (c *quotaCache) recordRateLimit(authID string, retryAfter, pollInterval time.Duration, at time.Time) time.Duration {
	var wait time.Duration
	c.update(authID, func(s *quotaSample) {
		s.RateLimits++
		wait = retryAfter
		if wait <= 0 {
			wait = pollInterval
			for i := 1; i < s.RateLimits && wait < maxRateLimitBackoff; i++ {
				wait *= 2
			}
		}
		wait = min(wait, maxRateLimitBackoff)
		s.BackoffUntil = at.Add(wait)
	})
	return wait
}

// recordHeaders applies the windows a proxied response reported. A sample
// with no poll yet becomes known only once both windows have been seen.
func (c *quotaCache) recordHeaders(authID string, fiveHour, weekly *windowReading, at time.Time) {
	if fiveHour == nil && weekly == nil {
		return
	}
	c.update(authID, func(s *quotaSample) {
		if !s.HasSample && (fiveHour == nil || weekly == nil) {
			return
		}
		merge := func(into *windowReading, from *windowReading) {
			if from == nil {
				return
			}
			resetAt := from.ResetAt
			if resetAt.IsZero() {
				resetAt = into.ResetAt
			}
			*into = windowReading{Used: from.Used, ResetAt: resetAt}
		}
		merge(&s.FiveHour, fiveHour)
		merge(&s.Weekly, weekly)
		s.HasSample = true
		s.HeaderAt = at
	})
}

func (c *quotaCache) recordRefusal(authID string, at time.Time) {
	c.update(authID, func(s *quotaSample) { s.RefusedAt = at })
}

func (c *quotaCache) recordFailure(authID, category string) {
	c.update(authID, func(s *quotaSample) { s.LastErrorCategory = category })
}

func (c *quotaCache) snapshot(authID string) (quotaSample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.samples[authID]
	return sample, ok
}

func (c *quotaCache) currentVersion() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *quotaCache) all() map[string]quotaSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]quotaSample, len(c.samples))
	for id, sample := range c.samples {
		out[id] = sample
	}
	return out
}
