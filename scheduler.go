package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type physicalClaudeAuth struct {
	ID        string
	AuthIndex string
	Name      string
	Email     string
	Identity  string
}

func physicalClaudeAuths(entries []pluginapi.HostAuthFileEntry) []physicalClaudeAuth {
	auths := make([]physicalClaudeAuth, 0, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if provider != "claude" || entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") || entry.RuntimeOnly || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" || id != entry.ID || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		auths = append(auths, physicalClaudeAuth{
			ID:        entry.ID,
			AuthIndex: entry.AuthIndex,
			Name:      strings.TrimSpace(entry.Name),
			Email:     strings.ToLower(strings.TrimSpace(entry.Email)),
			Identity:  physicalAuthIdentity(entry),
		})
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths
}

// physicalAuthIdentity changes only when a different account takes the slot.
// The host rewrites auth files on refreshes and status changes, so size and
// modification time must not count, or every write would discard the reading.
func physicalAuthIdentity(entry pluginapi.HostAuthFileEntry) string {
	return fmt.Sprintf("index:%q|path:%q|account:%q|email:%q",
		entry.AuthIndex,
		entry.Path,
		strings.TrimSpace(entry.Account),
		strings.ToLower(strings.TrimSpace(entry.Email)),
	)
}

// accountView is one account's routing state at a moment.
type accountView struct {
	ID       string   `json:"id"`
	Email    string   `json:"email,omitempty"`
	Name     string   `json:"name,omitempty"`
	Ceilings ceilings `json:"ceilings"`
	// Override reports ceilings set by an accounts entry, not the defaults.
	Override bool    `json:"override"`
	Known    bool    `json:"known"`
	Fresh    bool    `json:"fresh"`
	Refused  bool    `json:"refused,omitempty"`
	FiveHour float64 `json:"five_hour_used"`
	Weekly   float64 `json:"weekly_used"`
	// Headroom = weekly room x reset urgency + five-hour-weight x 5-hour room,
	// where a window's room is its ceiling minus its usage (never below 0).
	Headroom        float64     `json:"headroom"`
	WeeklyRoom      float64     `json:"weekly_room"`
	ResetUrgency    float64     `json:"reset_urgency"`
	FiveHourRoom    float64     `json:"five_hour_room"`
	Eligible        bool        `json:"eligible"`
	Reason          string      `json:"reason,omitempty"`
	LiveSessions    int         `json:"live_sessions"`
	FiveHourResetAt string      `json:"five_hour_reset_at,omitempty"`
	WeeklyResetAt   string      `json:"weekly_reset_at,omitempty"`
	SeenAt          string      `json:"seen_at,omitempty"`
	LastError       string      `json:"last_error,omitempty"`
	BackoffUntil    string      `json:"backoff_until,omitempty"`
	HostStatus      string      `json:"host_status,omitempty"`
	Resets          *grantsView `json:"resets,omitempty"`
}

func evaluate(authID string, sample quotaSample, cfg pluginConfig, now time.Time) accountView {
	resolved, override := cfg.accountCeilings(sample.Email, authID, sample.Name)
	view := accountView{
		ID:        authID,
		Email:     sample.Email,
		Name:      sample.Name,
		Ceilings:  resolved,
		Override:  override,
		Known:     sample.HasSample,
		Fresh:     sample.fresh(now, cfg.MaxStaleness),
		Refused:   sample.refused(),
		LastError: sample.LastErrorCategory,
	}
	if now.Before(sample.BackoffUntil) {
		view.BackoffUntil = formatTime(sample.BackoffUntil)
	}
	view.Resets = sample.Grants.view(now)
	if !sample.HasSample {
		view.Reason = "no usage reading yet"
		return view
	}
	view.FiveHour = sample.FiveHour.used(now)
	view.Weekly = sample.Weekly.used(now)
	view.WeeklyRoom = math.Max(0, view.Ceilings.Weekly-view.Weekly)
	view.FiveHourRoom = math.Max(0, view.Ceilings.FiveHour-view.FiveHour)
	view.ResetUrgency = resetUrgency(sample.Weekly.ResetAt, now, cfg.ResetWeight)
	view.Headroom = view.WeeklyRoom*view.ResetUrgency + cfg.FiveHourWeight*view.FiveHourRoom
	view.FiveHourResetAt = formatTime(sample.FiveHour.ResetAt)
	view.WeeklyResetAt = formatTime(sample.Weekly.ResetAt)
	view.SeenAt = formatTime(sample.seenAt())
	switch {
	case !view.Fresh:
		view.Reason = "usage reading is stale"
	case view.Refused:
		view.Reason = "refused upstream; waiting for a fresh reading"
	case view.FiveHour >= view.Ceilings.FiveHour:
		view.Reason = fmt.Sprintf("5h at %.0f%% (ceiling %.0f%%)", view.FiveHour, view.Ceilings.FiveHour)
	case view.Weekly >= view.Ceilings.Weekly:
		view.Reason = fmt.Sprintf("weekly at %.0f%% (ceiling %.0f%%)", view.Weekly, view.Ceilings.Weekly)
	default:
		view.Eligible = true
	}
	return view
}

// resetUrgency scales weekly room by how soon it expires: (7 days / days
// until the weekly reset)^weight, with days held between 1 and 7, so a full
// week left counts once and a day or less counts up to 7 times. An unknown
// or passed reset counts once.
func resetUrgency(resetAt, now time.Time, weight float64) float64 {
	if weight == 0 || resetAt.IsZero() || !now.Before(resetAt) {
		return 1
	}
	left := min(max(resetAt.Sub(now), 24*time.Hour), weeklyWindow)
	return math.Pow(float64(weeklyWindow)/float64(left), weight)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (r *pluginRuntime) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, *envelopeError) {
	cfg := r.loadedConfig()
	if !cfg.Enabled || !isClaudeRequest(req) || !cfg.routesModel(req.Model) {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	views, byID, best, unread := r.candidateViews(req, cfg)
	if len(views) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if best == noHeadroom && unread {
		// Only missing readings stand in the way (startup, a new account):
		// wait briefly for a poll pass rather than refuse.
		r.requestRefresh()
		r.waitForPoll(min(cfg.RequestTimeout, maxFirstReadWait))
	}

	r.pickMu.Lock()
	defer r.pickMu.Unlock()
	now := r.now()
	views, byID, best, _ = r.candidateViews(req, cfg)

	key := sessionKey(req.Options.Headers, req.Model)
	if key != "" && best != noHeadroom {
		if boundID, ok := r.sessions.get(key, now, cfg.AffinityTTL); ok {
			if bound, present := byID[boundID]; present && bound.Eligible && bound.Headroom+cfg.RebalanceMargin >= best {
				r.sessions.bind(key, boundID, now)
				return pluginapi.SchedulerPickResponse{AuthID: boundID, Handled: true}, nil
			}
		}
	}

	// An account the usage endpoint cannot read gets one new conversation per
	// probeInterval so its usage arrives in that response's rate-limit headers.
	if probe, ok := r.probeCandidate(views, cfg, now); ok {
		r.lastProbe[probe.ID] = now
		r.lastPicked[probe.ID] = now
		if key != "" {
			r.sessions.bind(key, probe.ID, now)
		}
		r.log("info", "quota balancer probing an account with no usage reading", map[string]any{"auth_id": probe.ID})
		return pluginapi.SchedulerPickResponse{AuthID: probe.ID, Handled: true}, nil
	}
	if best == noHeadroom {
		return pluginapi.SchedulerPickResponse{}, exhaustedError(views)
	}

	// Accounts within the spread band of the best headroom count as equal; the
	// one carrying the fewest live conversations, then the one picked longest
	// ago, takes the new conversation.
	var band []accountView
	for _, view := range views {
		if view.Eligible && view.Headroom >= best-cfg.SpreadBand {
			band = append(band, view)
		}
	}
	sort.Slice(band, func(i, j int) bool {
		a, b := band[i], band[j]
		if a.LiveSessions != b.LiveSessions {
			return a.LiveSessions < b.LiveSessions
		}
		if lastA, lastB := r.lastPicked[a.ID], r.lastPicked[b.ID]; !lastA.Equal(lastB) {
			return lastA.Before(lastB)
		}
		if a.Headroom != b.Headroom {
			return a.Headroom > b.Headroom
		}
		return a.ID < b.ID
	})
	chosen := band[0].ID
	r.lastPicked[chosen] = now
	if key != "" {
		r.sessions.bind(key, chosen, now)
	}
	return pluginapi.SchedulerPickResponse{AuthID: chosen, Handled: true}, nil
}

const (
	noHeadroom       = -math.MaxFloat64
	maxFirstReadWait = 5 * time.Second
	probeInterval    = 5 * time.Minute
)

// probeCandidate picks an account with no reading that may carry a probe: the
// host reports it active, its credential has not been rejected, and it has
// not been probed within probeInterval. Least recently probed wins.
func (r *pluginRuntime) probeCandidate(views []accountView, cfg pluginConfig, now time.Time) (accountView, bool) {
	if !cfg.ProbeUnread {
		return accountView{}, false
	}
	var chosen accountView
	found := false
	for _, view := range views {
		if view.Known || view.Refused || !hostHealthy(view.HostStatus) {
			continue
		}
		switch view.LastError {
		case pollErrorUnauthorized, pollErrorForbidden, pollErrorInvalidCredential, pollErrorMissingToken:
			continue
		}
		if now.Sub(r.lastProbe[view.ID]) < probeInterval {
			continue
		}
		if !found || r.lastProbe[view.ID].Before(r.lastProbe[chosen.ID]) ||
			(r.lastProbe[view.ID].Equal(r.lastProbe[chosen.ID]) && view.ID < chosen.ID) {
			chosen, found = view, true
		}
	}
	return chosen, found
}

func hostHealthy(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "active", "ok", "ready":
		return true
	}
	return false
}

// candidateViews evaluates the Claude candidates. best is the highest eligible
// headroom (noHeadroom when none qualifies); unread reports a candidate with
// no reading yet, which also queues a refresh.
func (r *pluginRuntime) candidateViews(req pluginapi.SchedulerPickRequest, cfg pluginConfig) ([]accountView, map[string]accountView, float64, bool) {
	now := r.now()
	live := r.sessions.live(now, cfg.AffinityTTL)
	var views []accountView
	byID := make(map[string]accountView)
	best, unread := noHeadroom, false
	for _, candidate := range req.Candidates {
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if (provider != "" && provider != "claude") || candidate.ID == "" {
			continue
		}
		sample, _ := r.cache.snapshot(candidate.ID)
		if !sample.HasSample {
			unread = true
		}
		view := evaluate(candidate.ID, sample, cfg, now)
		view.LiveSessions = live[candidate.ID]
		view.HostStatus = candidate.Status
		views = append(views, view)
		byID[view.ID] = view
		if view.Eligible && view.Headroom > best {
			best = view.Headroom
		}
	}
	if unread {
		r.requestRefresh()
	}
	return views, byID, best, unread
}

func exhaustedError(views []accountView) *envelopeError {
	reasons := make([]string, 0, len(views))
	for _, view := range views {
		label := view.Email
		if label == "" {
			label = view.ID
		}
		reasons = append(reasons, label+": "+view.Reason)
	}
	sort.Strings(reasons)
	return &envelopeError{
		Code:    exhaustedErrorCode,
		Message: "no Claude account is under its usage ceiling: " + strings.Join(reasons, "; "),
	}
}

func isClaudeRequest(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "claude") {
		return true
	}
	if strings.TrimSpace(req.Provider) != "" || len(req.Providers) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Providers[0]), "claude")
}

type statusResponse struct {
	Enabled         bool                `json:"enabled"`
	Models          []string            `json:"models"`
	Default         ceilings            `json:"default_ceilings"`
	Overrides       map[string]ceilings `json:"account_ceilings,omitempty"`
	FiveHourWeight  float64             `json:"five_hour_weight"`
	ResetWeight     float64             `json:"reset_weight"`
	SpreadBand      float64             `json:"spread_band"`
	RebalanceMargin float64             `json:"rebalance_margin"`
	PollInterval    string              `json:"poll_interval"`
	MaxStaleness    string              `json:"max_staleness"`
	Accounts        []accountView       `json:"accounts"`
}

func (r *pluginRuntime) status() statusResponse {
	cfg := r.loadedConfig()
	now := r.now()
	live := r.sessions.live(now, cfg.AffinityTTL)
	accounts := make([]accountView, 0)
	for id, sample := range r.cache.all() {
		view := evaluate(id, sample, cfg, now)
		view.LiveSessions = live[id]
		accounts = append(accounts, view)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	models := cfg.Models
	if len(models) == 0 {
		models = []string{"*"}
	}
	return statusResponse{
		Enabled:         cfg.Enabled,
		Models:          models,
		Default:         cfg.Default,
		Overrides:       cfg.Accounts,
		FiveHourWeight:  cfg.FiveHourWeight,
		ResetWeight:     cfg.ResetWeight,
		SpreadBand:      cfg.SpreadBand,
		RebalanceMargin: cfg.RebalanceMargin,
		PollInterval:    cfg.PollInterval.String(),
		MaxStaleness:    cfg.MaxStaleness.String(),
		Accounts:        accounts,
	}
}

func (r *pluginRuntime) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	get := strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet)
	switch {
	case get && path == uiResourcePath:
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            []string{"text/html; charset=utf-8"},
				"Cache-Control":           []string{"no-store"},
				"X-Content-Type-Options":  []string{"nosniff"},
				"Content-Security-Policy": []string{uiContentSecurityPolicy},
			},
			Body: uiPage,
		}
	case strings.EqualFold(strings.TrimSpace(req.Method), http.MethodPost) && strings.HasSuffix(path, managementResetRoute):
		var body resetRequest
		if json.Unmarshal(req.Body, &body) != nil {
			raw, _ := json.Marshal(map[string]string{"error": "invalid body"})
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Headers: jsonHeaders(), Body: raw}
		}
		raw, _ := json.Marshal(r.useReset(context.Background(), body))
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: raw}
	case get && strings.HasSuffix(path, managementStatusRoute):
		body, _ := json.Marshal(r.status())
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: body}
	default:
		body, _ := json.Marshal(map[string]string{"error": "not found"})
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Headers: jsonHeaders(), Body: body}
	}
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
}
