package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const testOrgUUID = "0b0c0d0e-1111-4222-8333-444455556666"

// liveCedarEmber is the shape Anthropic returned for an Enterprise account on 2026-10-13.
const liveCedarEmber = `{
 "eligible": true, "ineligible_reason": null, "at_limit": false, "exhausted": [],
 "grants": [{
   "id": "opus55-launch-ent-20260921",
   "label": "Claude Opus 5.5 launch: one usage-limit reset for Enterprise members",
   "resets_total": 1, "resets_left": 1,
   "starts_at": "2026-09-22T16:00:00+00:00", "ends_at": "2026-10-22T16:00:00+00:00",
   "clears": ["five_hour", "seven_day", "seven_day_overage_included"],
   "paused": false, "usable_now": true, "use_requires_limit": false,
   "percent_used": {"five_hour": 0, "seven_day": 90}, "blocking": [], "arm": null
 }],
 "next_grant_id": "opus55-launch-ent-20260921",
 "weekly_resets_at": "2026-10-15T02:00:00+00:00", "cooldown_until": null,
 "event_props": {"tier": "claude_enterprise"}
}`

func TestParsesTheLiveCedarEmberBlock(t *testing.T) {
	g := parseResetGrants(json.RawMessage(liveCedarEmber))
	if g == nil || !g.Eligible || g.available() != 1 || len(g.Grants) != 1 {
		t.Fatalf("parsed = %#v", g)
	}
	grant, ok := g.selectGrant(testNow)
	if !ok || grant.ID != "opus55-launch-ent-20260921" || grant.UseRequiresLimit || len(grant.Clears) != 3 {
		t.Fatalf("selected = %#v ok=%v", grant, ok)
	}
	view := g.view(testNow)
	if view.UsableGrantID != grant.ID || view.Grants[0].Unusable != "" || view.Grants[0].EndsAt != "2026-10-22T16:00:00Z" {
		t.Fatalf("view = %#v", view)
	}
}

func TestGrantUsabilityRules(t *testing.T) {
	base := func() *resetGrants { return parseResetGrants(json.RawMessage(liveCedarEmber)) }
	cases := map[string]func(*resetGrants){
		"only usable at the limit": func(g *resetGrants) { g.Grants[0].UseRequiresLimit = true },
		"cooling down":             func(g *resetGrants) { g.CooldownUntil = testNow.Add(time.Hour) },
		"expired":                  func(g *resetGrants) { g.Grants[0].EndsAt = testNow.Add(-time.Minute) },
		"paused by Anthropic":      func(g *resetGrants) { g.Grants[0].Paused = true },
		"no resets left":           func(g *resetGrants) { g.Grants[0].ResetsLeft = 0 },
		"account not eligible":     func(g *resetGrants) { g.Eligible = false },
	}
	for want, mutate := range cases {
		g := base()
		mutate(g)
		if got := g.unusableReason(g.Grants[0], testNow); got != want {
			t.Errorf("%s: got %q", want, got)
		}
		if _, ok := g.selectGrant(testNow); ok {
			t.Errorf("%s: an unusable grant was selected", want)
		}
	}
	at := base()
	at.Grants[0].UseRequiresLimit, at.AtLimit = true, true
	if _, ok := at.selectGrant(testNow); !ok {
		t.Fatalf("a limit-only grant should be usable at the limit")
	}
	for _, bad := range []string{`{"grants":[]}`, `{"eligible":true,"grants":[{"id":"BAD ID","resets_total":1,"resets_left":1}]}`,
		`{"eligible":true,"grants":[{"id":"a","resets_total":1,"resets_left":2}]}`, `not json`} {
		if parseResetGrants(json.RawMessage(bad)) != nil {
			t.Errorf("malformed block accepted: %s", bad)
		}
	}
}

func TestUsageReadCarriesGrants(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cedar_ember") != "1" || !strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") {
			http.Error(w, "missing cedar_ember query or CLI user agent", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":90,"resets_at":"2026-10-15T02:00:00Z"},"cedar_ember":` + liveCedarEmber + `}`))
	}))
	defer server.Close()
	result, category := newHTTPUsageFetcher(server.URL+"/api/oauth/usage?cedar_ember=1&skip_spend=1", nil).fetch(context.Background(), "tok", time.Second)
	if category != "" || result.Grants == nil || result.Grants.available() != 1 {
		t.Fatalf("result=%#v category=%q", result, category)
	}
}

// resetFixture is a runtime whose one account sits at 90% weekly with a usable grant,
// talking to a fake Anthropic reset endpoint.
func resetFixture(t *testing.T, handler http.HandlerFunc) (*pluginRuntime, *atomic.Int64) {
	t.Helper()
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	previous := claudeAPIBase
	claudeAPIBase = server.URL
	t.Cleanup(func() { claudeAPIBase = previous })

	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"a@x.com": {0, 90}})
	r.cache.update(authIDFor("a@x.com"), func(s *quotaSample) { s.Grants = parseResetGrants(json.RawMessage(liveCedarEmber)) })
	return r, &posts
}

func TestUsingAResetPostsOnceAndFreesTheAccount(t *testing.T) {
	r, posts := resetFixture(t, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if req.Method != http.MethodPost || req.URL.Path != "/api/organizations/"+testOrgUUID+"/reset_rate_limits" ||
			req.Header.Get("Authorization") != "Bearer token-1" || req.Header.Get("anthropic-beta") != anthropicOAuthBeta ||
			body["program"] != "cedar_ember" || body["grant_id"] != "opus55-launch-ent-20260921" || !uuidPattern.MatchString(body["request_id"]) {
			http.Error(w, "bad reset request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"result":"reset"}`))
	})
	if _, errEnv := pickFor(t, r, "", "a@x.com"); errEnv == nil {
		t.Fatalf("setup: the account at 90%% weekly should be held")
	}
	got := r.useReset(context.Background(), resetRequest{Account: "A@X.com"})
	if got.Result != "reset" || posts.Load() != 1 {
		t.Fatalf("useReset = %#v, posts = %d", got, posts.Load())
	}
	if pick := mustPick(t, r, "", "a@x.com"); pick != "a@x.com" {
		t.Fatalf("account not routable after its reset")
	}
	sample, _ := r.cache.snapshot(authIDFor("a@x.com"))
	if sample.Weekly.used(r.now()) != 0 || sample.Grants.available() != 0 || !sample.ForceRead {
		t.Fatalf("after reset: weekly=%v left=%d forceRead=%v", sample.Weekly.Used, sample.Grants.available(), sample.ForceRead)
	}
	if again := r.useReset(context.Background(), resetRequest{Account: "a@x.com"}); again.Result != "not_usable" || posts.Load() != 1 {
		t.Fatalf("second use = %#v, posts = %d", again, posts.Load())
	}
}

func TestADroppedResetIsNeverRetried(t *testing.T) {
	r, posts := resetFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	got := r.useReset(context.Background(), resetRequest{Account: "a@x.com"})
	if got.Result != "unknown" || posts.Load() != 1 {
		t.Fatalf("useReset = %#v, posts = %d", got, posts.Load())
	}
	sample, _ := r.cache.snapshot(authIDFor("a@x.com"))
	if sample.Weekly.Used != 90 || sample.Grants.available() != 1 {
		t.Fatalf("an unknown outcome must not change the reading: %#v", sample)
	}
}

func TestUnusableResetSendsNothing(t *testing.T) {
	r, posts := resetFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"result":"reset"}`)) })
	r.cache.update(authIDFor("a@x.com"), func(s *quotaSample) { s.Grants.Grants[0].UseRequiresLimit = true })
	if got := r.useReset(context.Background(), resetRequest{Account: "a@x.com"}); got.Result != "not_usable" || posts.Load() != 0 {
		t.Fatalf("useReset = %#v, posts = %d", got, posts.Load())
	}
	if got := r.useReset(context.Background(), resetRequest{Account: "nobody@x.com"}); got.Result != "no_account" {
		t.Fatalf("unknown account = %#v", got)
	}
}

func TestResetManagementRoute(t *testing.T) {
	r, _ := resetFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"result":"not_limited"}`)) })
	resp := r.handleManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management" + managementResetRoute,
		Body: []byte(`{"account":"a@x.com","grant_id":"opus55-launch-ent-20260921"}`),
	})
	var out resetResponse
	if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &out) != nil || out.Result != "not_limited" || out.Message == "" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if bad := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management" + managementResetRoute, Body: []byte("nope")}); bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", bad.StatusCode)
	}
	if view := r.status().Accounts[0].Resets; view == nil || view.Available != 1 {
		t.Fatalf("status resets = %#v", view)
	}
}
