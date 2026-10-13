package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Claude usage-limit resets ("cedar_ember" grants) arrive in the usage
// endpoint's cedar_ember block when it is asked for with ?cedar_ember=1.
// The parsing mirrors cpa-usage-keeper: a malformed block is dropped whole
// and never affects the usage reading.

var (
	grantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	uuidPattern    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type resetGrant struct {
	ID               string    `json:"id"`
	Label            string    `json:"label,omitempty"`
	ResetsTotal      int       `json:"resets_total"`
	ResetsLeft       int       `json:"resets_left"`
	StartsAt         time.Time `json:"starts_at,omitzero"`
	EndsAt           time.Time `json:"ends_at,omitzero"`
	Clears           []string  `json:"clears"`
	Paused           bool      `json:"paused,omitempty"`
	UsableNow        bool      `json:"usable_now"`
	UseRequiresLimit bool      `json:"use_requires_limit"`
}

type resetGrants struct {
	Eligible         bool         `json:"eligible"`
	IneligibleReason string       `json:"ineligible_reason,omitempty"`
	AtLimit          bool         `json:"at_limit"`
	Grants           []resetGrant `json:"grants"`
	NextGrantID      string       `json:"next_grant_id,omitempty"`
	CooldownUntil    time.Time    `json:"cooldown_until,omitzero"`
}

func parseResetGrants(raw json.RawMessage) *resetGrants {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var block struct {
		Eligible         *bool   `json:"eligible"`
		IneligibleReason *string `json:"ineligible_reason"`
		AtLimit          *bool   `json:"at_limit"`
		Grants           []struct {
			ID            string   `json:"id"`
			Label         string   `json:"label"`
			Total         *int     `json:"resets_total"`
			Left          *int     `json:"resets_left"`
			Starts        *string  `json:"starts_at"`
			Ends          *string  `json:"ends_at"`
			Clears        []string `json:"clears"`
			Paused        *bool    `json:"paused"`
			Usable        *bool    `json:"usable_now"`
			RequiresLimit *bool    `json:"use_requires_limit"`
		} `json:"grants"`
		Next     *string `json:"next_grant_id"`
		Cooldown *string `json:"cooldown_until"`
	}
	if json.Unmarshal(raw, &block) != nil || block.Eligible == nil || len(block.Grants) > 64 {
		return nil
	}
	cooldown, ok := optionalTime(block.Cooldown)
	if !ok {
		return nil
	}
	status := &resetGrants{Eligible: *block.Eligible, AtLimit: block.AtLimit != nil && *block.AtLimit, CooldownUntil: cooldown, Grants: []resetGrant{}}
	if block.IneligibleReason != nil {
		status.IneligibleReason = sanitizeLabel(*block.IneligibleReason, 40)
	}
	seen := map[string]bool{}
	for _, g := range block.Grants {
		starts, okStart := optionalTime(g.Starts)
		ends, okEnd := optionalTime(g.Ends)
		if !grantIDPattern.MatchString(g.ID) || seen[g.ID] || g.Total == nil || g.Left == nil ||
			*g.Total < 0 || *g.Left < 0 || *g.Left > *g.Total || *g.Total > 1000 || !okStart || !okEnd {
			return nil
		}
		seen[g.ID] = true
		clears := make([]string, 0, len(g.Clears))
		for _, c := range g.Clears {
			if c = sanitizeLabel(c, 40); c != "" {
				clears = append(clears, c)
			}
		}
		status.Grants = append(status.Grants, resetGrant{
			ID: g.ID, Label: sanitizeLabel(g.Label, 120),
			ResetsTotal: *g.Total, ResetsLeft: *g.Left,
			StartsAt: starts, EndsAt: ends, Clears: clears,
			Paused:           g.Paused != nil && *g.Paused,
			UsableNow:        g.Usable != nil && *g.Usable,
			UseRequiresLimit: g.RequiresLimit == nil || *g.RequiresLimit,
		})
	}
	if block.Next != nil && seen[*block.Next] {
		status.NextGrantID = *block.Next
	}
	return status
}

func optionalTime(value *string) (time.Time, bool) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339Nano, *value)
	return at, err == nil
}

// sanitizeLabel flattens control characters and whitespace and caps length.
func sanitizeLabel(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)), " ")
	if runes := []rune(value); len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

// unusableReason is empty when the grant can be used now, else why not.
func (s *resetGrants) unusableReason(g resetGrant, now time.Time) string {
	switch {
	case s == nil || !s.Eligible:
		return "account not eligible"
	case g.ResetsLeft <= 0:
		return "no resets left"
	case g.Paused:
		return "paused by Anthropic"
	case !g.StartsAt.IsZero() && now.Before(g.StartsAt):
		return "not started yet"
	case !g.EndsAt.IsZero() && !now.Before(g.EndsAt):
		return "expired"
	case !s.CooldownUntil.IsZero() && now.Before(s.CooldownUntil):
		return "cooling down"
	case g.UseRequiresLimit && !s.AtLimit:
		return "only usable at the limit"
	case !g.UsableNow:
		return "not usable now"
	}
	return ""
}

// selectGrant picks the grant a reset would use: Anthropic's next_grant_id
// when usable, else the first usable one by ID.
func (s *resetGrants) selectGrant(now time.Time) (resetGrant, bool) {
	if s == nil {
		return resetGrant{}, false
	}
	var usable []resetGrant
	for _, g := range s.Grants {
		if s.unusableReason(g, now) != "" {
			continue
		}
		if g.ID == s.NextGrantID {
			return g, true
		}
		usable = append(usable, g)
	}
	if len(usable) == 0 {
		return resetGrant{}, false
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].ID < usable[j].ID })
	return usable[0], true
}

func (s *resetGrants) available() int {
	if s == nil {
		return 0
	}
	total := 0
	for _, g := range s.Grants {
		total += g.ResetsLeft
	}
	return total
}

// grantsView is what the status endpoint reports per account.
type grantsView struct {
	Eligible         bool        `json:"eligible"`
	IneligibleReason string      `json:"ineligible_reason,omitempty"`
	AtLimit          bool        `json:"at_limit"`
	Available        int         `json:"available"`
	UsableGrantID    string      `json:"usable_grant_id,omitempty"`
	CooldownUntil    string      `json:"cooldown_until,omitempty"`
	Grants           []grantLine `json:"grants"`
}

type grantLine struct {
	ID          string   `json:"id"`
	Label       string   `json:"label,omitempty"`
	ResetsLeft  int      `json:"resets_left"`
	ResetsTotal int      `json:"resets_total"`
	EndsAt      string   `json:"ends_at,omitempty"`
	Clears      []string `json:"clears"`
	Unusable    string   `json:"unusable,omitempty"`
}

func (s *resetGrants) view(now time.Time) *grantsView {
	if s == nil {
		return nil
	}
	v := &grantsView{
		Eligible: s.Eligible, IneligibleReason: s.IneligibleReason, AtLimit: s.AtLimit,
		Available: s.available(), CooldownUntil: formatTime(s.CooldownUntil), Grants: []grantLine{},
	}
	if g, ok := s.selectGrant(now); ok {
		v.UsableGrantID = g.ID
	}
	for _, g := range s.Grants {
		v.Grants = append(v.Grants, grantLine{
			ID: g.ID, Label: g.Label, ResetsLeft: g.ResetsLeft, ResetsTotal: g.ResetsTotal,
			EndsAt: formatTime(g.EndsAt), Clears: g.Clears, Unusable: s.unusableReason(g, now),
		})
	}
	return v
}
