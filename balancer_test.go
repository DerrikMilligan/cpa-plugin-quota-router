package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

type fakeHost struct {
	mu      sync.Mutex
	entries []pluginapi.HostAuthFileEntry
	logs    []string
}

func (h *fakeHost) listAuth() ([]pluginapi.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.HostAuthFileEntry(nil), h.entries...), nil
}

func (h *fakeHost) getAuth(authIndex string) (json.RawMessage, error) {
	return json.RawMessage(fmt.Sprintf(`{"type":"claude","access_token":"token-%s"}`, authIndex)), nil
}

func (h *fakeHost) log(level, message string, _ map[string]any) {
	h.mu.Lock()
	h.logs = append(h.logs, level+": "+message)
	h.mu.Unlock()
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newTestRuntime builds a runtime over accounts named by email; usage maps
// "email" to {fiveHour, weekly} percent used.
func newTestRuntime(t *testing.T, cfg pluginConfig, usage map[string][2]float64) (*pluginRuntime, *clock) {
	t.Helper()
	host := &fakeHost{}
	byToken := make(map[string][2]float64)
	i := 0
	for email, used := range usage {
		i++
		index := fmt.Sprintf("%d", i)
		host.entries = append(host.entries, pluginapi.HostAuthFileEntry{
			ID: authIDFor(email), AuthIndex: index, Name: authIDFor(email), Provider: "claude",
			Path: "/auth/" + authIDFor(email), Email: email,
		})
		byToken["token-"+index] = used
	}
	c := &clock{now: testNow}
	fetch := func(_ context.Context, token string, _ time.Duration) (usageResult, string) {
		used, ok := byToken[token]
		if !ok {
			return usageResult{}, pollErrorUnauthorized
		}
		return usageResult{
			FiveHour: windowReading{Used: used[0], ResetAt: c.Now().Add(3 * time.Hour)},
			Weekly:   windowReading{Used: used[1], ResetAt: c.Now().Add(72 * time.Hour)},
		}, ""
	}
	r := newPluginRuntime(host, fetch, c.Now)
	r.config.Store(&cfg)
	r.refreshAll(context.Background(), false)
	return r, c
}

func authIDFor(email string) string { return "claude-" + email + ".json" }

func candidatesFor(emails ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(emails))
	for _, email := range emails {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: authIDFor(email), Provider: "claude"})
	}
	return out
}

func pickFor(t *testing.T, r *pluginRuntime, session string, emails ...string) (string, *envelopeError) {
	t.Helper()
	req := pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Model:      "claude-opus-5-5",
		Candidates: candidatesFor(emails...),
	}
	if session != "" {
		req.Options.Headers = map[string][]string{"X-Claude-Code-Session-Id": {session}}
	}
	resp, errEnv := r.pick(req)
	if errEnv != nil {
		return "", errEnv
	}
	if !resp.Handled {
		t.Fatalf("pick was not handled")
	}
	return strings.TrimSuffix(strings.TrimPrefix(resp.AuthID, "claude-"), ".json"), nil
}

func mustPick(t *testing.T, r *pluginRuntime, session string, emails ...string) string {
	t.Helper()
	got, errEnv := pickFor(t, r, session, emails...)
	if errEnv != nil {
		t.Fatalf("pick failed: %s", errEnv.Message)
	}
	return got
}

func TestPrefersTheAccountWithTheMostUsageLeft(t *testing.T) {
	// The owner's example: A has 50% weekly left, B 80%; both have a fresh 5h window.
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 50},
		"b@x.com": {0, 20},
	})
	if got := mustPick(t, r, "", "a@x.com", "b@x.com"); got != "b@x.com" {
		t.Fatalf("picked %s, want b@x.com (weekly headroom 70 beats 40)", got)
	}
}

func TestFiveHourWindowCountsToo(t *testing.T) {
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {70, 10}, // 5h headroom 20
		"b@x.com": {10, 40}, // weekly headroom 50
	})
	if got := mustPick(t, r, "", "a@x.com", "b@x.com"); got != "b@x.com" {
		t.Fatalf("picked %s, want b@x.com", got)
	}
}

func TestNeverRoutesToAnAccountAtItsCeiling(t *testing.T) {
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"five@x.com":   {90, 10},
		"weekly@x.com": {0, 90},
		"ok@x.com":     {85, 85},
	})
	for i := 0; i < 20; i++ {
		if got := mustPick(t, r, fmt.Sprintf("s%d", i), "five@x.com", "weekly@x.com", "ok@x.com"); got != "ok@x.com" {
			t.Fatalf("picked %s, want ok@x.com", got)
		}
	}
}

func TestRefusesWhenEveryAccountIsAtItsCeiling(t *testing.T) {
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {92, 10},
		"b@x.com": {5, 95},
	})
	_, errEnv := pickFor(t, r, "", "a@x.com", "b@x.com")
	if errEnv == nil || errEnv.Code != exhaustedErrorCode {
		t.Fatalf("want %s, got %#v", exhaustedErrorCode, errEnv)
	}
	if !strings.Contains(errEnv.Message, "a@x.com: 5h at 92%") || !strings.Contains(errEnv.Message, "b@x.com: weekly at 95%") {
		t.Fatalf("error should name each account's reason: %s", errEnv.Message)
	}
}

func TestPerAccountCeilingOverridesTheDefault(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Accounts = map[string]ceilings{"a@x.com": {FiveHour: 60, Weekly: 90}}
	r, _ := newTestRuntime(t, cfg, map[string][2]float64{
		"a@x.com": {65, 0},
		"b@x.com": {80, 80},
	})
	if got := mustPick(t, r, "", "a@x.com", "b@x.com"); got != "b@x.com" {
		t.Fatalf("picked %s; a@x.com is over its own 60%% 5h ceiling", got)
	}
}

func TestLowerCeilingMeansLessTraffic(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SpreadBand = 0
	cfg.Accounts = map[string]ceilings{"careful@x.com": {FiveHour: 50, Weekly: 50}}
	r, _ := newTestRuntime(t, cfg, map[string][2]float64{
		"careful@x.com": {10, 10}, // headroom 40
		"plenty@x.com":  {30, 30}, // headroom 60
	})
	if got := mustPick(t, r, "", "careful@x.com", "plenty@x.com"); got != "plenty@x.com" {
		t.Fatalf("picked %s, want plenty@x.com", got)
	}
}

func TestConversationStaysOnItsAccountWhileUnderTheCeiling(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 20},
		"b@x.com": {0, 22},
	})
	first := mustPick(t, r, "conv", "a@x.com", "b@x.com")
	other := map[string]string{"a@x.com": "b@x.com", "b@x.com": "a@x.com"}[first]
	// The other account pulls ahead by less than the rebalance margin.
	r.handleUsage(usageRecord{AuthID: authIDFor(first), ResponseHeaders: rateHeaders(0.10, 0.40)})
	c.advance(time.Minute)
	if got := mustPick(t, r, "conv", "a@x.com", "b@x.com"); got != first {
		t.Fatalf("conversation moved to %s; it should keep its prompt cache on %s (other %s)", got, first, other)
	}
}

func TestConversationMovesOnceItsAccountHitsTheCeiling(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 20},
		"b@x.com": {0, 20},
	})
	first := mustPick(t, r, "conv", "a@x.com", "b@x.com")
	r.handleUsage(usageRecord{AuthID: authIDFor(first), ResponseHeaders: rateHeaders(0.90, 0.30)})
	c.advance(time.Second)
	if got := mustPick(t, r, "conv", "a@x.com", "b@x.com"); got == first {
		t.Fatalf("conversation stayed on %s at its 5h ceiling", first)
	}
}

func TestConversationRebalancesWhenFarBehind(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.RebalanceMargin = 25
	r, c := newTestRuntime(t, cfg, map[string][2]float64{
		"a@x.com": {0, 20},
		"b@x.com": {0, 20},
	})
	first := mustPick(t, r, "conv", "a@x.com", "b@x.com")
	// Its account drops to 30 headroom while the other keeps 70.
	r.handleUsage(usageRecord{AuthID: authIDFor(first), ResponseHeaders: rateHeaders(0.60, 0.30)})
	c.advance(time.Second)
	if got := mustPick(t, r, "conv", "a@x.com", "b@x.com"); got == first {
		t.Fatalf("conversation stayed on %s 40 points behind", first)
	}
}

func TestNewConversationsSpreadAcrossSimilarAccounts(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 20},
		"b@x.com": {0, 24},
		"c@x.com": {0, 26},
		"d@x.com": {0, 70}, // well outside the spread band
	})
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		seen[mustPick(t, r, fmt.Sprintf("conv-%d", i), "a@x.com", "b@x.com", "c@x.com", "d@x.com")]++
		c.advance(time.Second)
	}
	if seen["a@x.com"] != 3 || seen["b@x.com"] != 3 || seen["c@x.com"] != 3 || seen["d@x.com"] != 0 {
		t.Fatalf("spread = %v, want 3/3/3/0", seen)
	}
}

func TestSessionlessRequestsRoundRobinWithinTheBand(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 20},
		"b@x.com": {0, 21},
	})
	first := mustPick(t, r, "", "a@x.com", "b@x.com")
	c.advance(time.Second)
	if second := mustPick(t, r, "", "a@x.com", "b@x.com"); second == first {
		t.Fatalf("both sessionless picks went to %s", first)
	}
}

func TestRejectedHeaderBlocksUntilAFreshRead(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 0},
		"b@x.com": {0, 50},
	})
	headers := rateHeaders(0.20, 0.10)
	headers.Set(rateLimitHeaderPrefix+"7d-Status", "rejected")
	r.handleUsage(usageRecord{AuthID: authIDFor("a@x.com"), ResponseHeaders: headers})
	if got := mustPick(t, r, "", "a@x.com", "b@x.com"); got != "b@x.com" {
		t.Fatalf("picked %s after a refusal", got)
	}
	c.advance(time.Minute)
	r.refreshAll(context.Background(), false)
	if view := r.status().Accounts[0]; view.Refused {
		t.Fatalf("a fresh read should clear the refusal: %#v", view)
	}
}

func TestUpstream429BlocksTheAccount(t *testing.T) {
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{
		"a@x.com": {0, 0},
		"b@x.com": {0, 50},
	})
	record := usageRecord{AuthID: authIDFor("a@x.com"), Failed: true}
	record.Failure.StatusCode = http.StatusTooManyRequests
	r.handleUsage(record)
	if got := mustPick(t, r, "", "a@x.com", "b@x.com"); got != "b@x.com" {
		t.Fatalf("picked %s after a 429", got)
	}
}

func TestStaleReadingsAreIneligible(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"a@x.com": {0, 0}})
	c.advance(defaultMaxStaleness + time.Second)
	_, errEnv := pickFor(t, r, "", "a@x.com")
	if errEnv == nil || !strings.Contains(errEnv.Message, "stale") {
		t.Fatalf("want a stale refusal, got %#v", errEnv)
	}
}

func TestHeadersKeepTheReadingFresh(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"a@x.com": {0, 0}})
	c.advance(defaultMaxStaleness - time.Minute)
	r.handleUsage(usageRecord{AuthID: authIDFor("a@x.com"), ResponseHeaders: rateHeaders(0.10, 0.10)})
	c.advance(2 * time.Minute)
	mustPick(t, r, "", "a@x.com")
}

func TestARolledOverWindowReadsEmpty(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"a@x.com": {95, 10}})
	if _, errEnv := pickFor(t, r, "", "a@x.com"); errEnv == nil {
		t.Fatalf("account at 95%% 5h should be refused")
	}
	// Past the 5h reset; a header keeps the reading fresh without re-reporting the window.
	c.advance(3*time.Hour + time.Second)
	headers := http.Header{}
	headers.Set(rateLimitHeaderPrefix+"7d-Utilization", "0.10")
	r.handleUsage(usageRecord{AuthID: authIDFor("a@x.com"), ResponseHeaders: headers})
	mustPick(t, r, "", "a@x.com")
}

func TestUnroutedModelsAndProvidersFallToTheHost(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Models = []string{"claude-fable-5-1"}
	r, _ := newTestRuntime(t, cfg, map[string][2]float64{"a@x.com": {0, 0}})
	resp, _ := r.pick(pluginapi.SchedulerPickRequest{Provider: "claude", Model: "claude-opus-5-5", Candidates: candidatesFor("a@x.com")})
	if resp.Handled {
		t.Fatalf("an unlisted model should fall to the host")
	}
	resp, _ = r.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Model: "claude-fable-5-1", Candidates: candidatesFor("a@x.com")})
	if resp.Handled {
		t.Fatalf("a non-Claude provider should fall to the host")
	}
}

func TestHeaderWindowParsesFractionsAndResets(t *testing.T) {
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	headers.Set("anthropic-ratelimit-unified-5h-reset", "1792000000")
	reading, rejected := headerWindow(headers, "5h")
	if rejected || reading == nil || reading.Used != 42 || reading.ResetAt.Unix() != 1792000000 {
		t.Fatalf("got %#v rejected=%v", reading, rejected)
	}
	if reading, _ := headerWindow(headers, "7d"); reading != nil {
		t.Fatalf("absent window should read nil, got %#v", reading)
	}
}

func TestConfigParsesCeilingsOverridesAndModels(t *testing.T) {
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(`
models: ["*"]
five-hour-ceiling: 85
weekly-ceiling: 88
accounts:
  Kphair@isolvedhcm.com:
    five-hour-ceiling: 60
poll-interval: 1s
`)})
	cfg, err := decodeLifecycleConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models != nil || cfg.Default != (ceilings{85, 88}) {
		t.Fatalf("defaults = %#v models=%v", cfg.Default, cfg.Models)
	}
	if got := cfg.ceilingsFor("kphair@isolvedhcm.com", "", ""); got != (ceilings{60, 88}) {
		t.Fatalf("override = %#v, want unset fields to inherit the default", got)
	}
	if cfg.PollInterval != minimumPollInterval || cfg.MaxStaleness != defaultMaxStaleness {
		t.Fatalf("poll=%v staleness=%v", cfg.PollInterval, cfg.MaxStaleness)
	}
	for _, bad := range []string{"five-hour-ceiling: 0", "weekly-ceiling: 101", "accounts: {a: {weekly-ceiling: -1}}", "spread-band: -1", "poll-interval: soon"} {
		raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(bad)})
		if _, err := decodeLifecycleConfig(raw); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestUsageEndpointParsesTheLiveShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") != anthropicOAuthBeta {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":5.0,"resets_at":"2026-10-13T15:39:59.665563+00:00"},` +
			`"seven_day":{"utilization":17.0,"resets_at":"2026-10-18T05:59:59.665587+00:00"},"seven_day_opus":null}`))
	}))
	defer server.Close()
	result, category := newHTTPUsageFetcher(server.URL, nil).fetch(context.Background(), "tok", time.Second)
	if category != "" || result.FiveHour.Used != 5 || result.Weekly.Used != 17 || result.Weekly.ResetAt.Day() != 18 {
		t.Fatalf("result=%#v category=%q", result, category)
	}
}

func TestUsageEndpointTreatsAMissingFiveHourWindowAsUnstarted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":null,"seven_day":{"utilization":40,"resets_at":"2026-10-18T05:59:59Z"}}`))
	}))
	defer server.Close()
	result, category := newHTTPUsageFetcher(server.URL, nil).fetch(context.Background(), "tok", time.Second)
	if category != "" || result.FiveHour.Used != 0 || result.Weekly.Used != 40 {
		t.Fatalf("result=%#v category=%q", result, category)
	}
}

func rateHeaders(fiveHour, weekly float64) http.Header {
	h := http.Header{}
	h.Set(rateLimitHeaderPrefix+"5h-Utilization", fmt.Sprintf("%.2f", fiveHour))
	h.Set(rateLimitHeaderPrefix+"7d-Utilization", fmt.Sprintf("%.2f", weekly))
	return h
}

func TestAuthFileRewriteKeepsTheReading(t *testing.T) {
	r, c := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"a@x.com": {10, 10}})
	host := r.host.(*fakeHost)
	host.mu.Lock()
	host.entries[0].Size, host.entries[0].ModTime = 999, c.Now()
	host.mu.Unlock()
	entries, _ := host.listAuth()
	r.cache.reconcile(physicalClaudeAuths(entries))
	if sample, _ := r.cache.snapshot(authIDFor("a@x.com")); !sample.HasSample {
		t.Fatalf("a host rewrite of the auth file discarded the reading")
	}
	host.mu.Lock()
	host.entries[0].Email = "someone-else@x.com"
	host.mu.Unlock()
	entries, _ = host.listAuth()
	r.cache.reconcile(physicalClaudeAuths(entries))
	if sample, _ := r.cache.snapshot(authIDFor("a@x.com")); sample.HasSample {
		t.Fatalf("a different account in the slot kept the old reading")
	}
}

func TestFirstPickWaitsForTheStartupRead(t *testing.T) {
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{{
		ID: authIDFor("a@x.com"), AuthIndex: "1", Provider: "claude", Path: "/auth/a", Email: "a@x.com",
	}}}
	release := make(chan struct{})
	fetch := func(_ context.Context, _ string, _ time.Duration) (usageResult, string) {
		<-release
		return usageResult{Weekly: windowReading{Used: 10}}, ""
	}
	r := newPluginRuntime(host, fetch, time.Now)
	r.applyConfig(defaultPluginConfig())
	defer r.shutdown()
	time.AfterFunc(100*time.Millisecond, func() { close(release) })
	if got := mustPick(t, r, "", "a@x.com"); got != "a@x.com" {
		t.Fatalf("picked %s", got)
	}
}

func TestOnDemandRefreshDoesNotHammerAnUnreadableAccount(t *testing.T) {
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{{
		ID: authIDFor("a@x.com"), AuthIndex: "1", Provider: "claude", Path: "/auth/a", Email: "a@x.com",
	}}}
	calls := 0
	fetch := func(context.Context, string, time.Duration) (usageResult, string) {
		calls++
		return usageResult{}, pollErrorServer
	}
	c := &clock{now: testNow}
	r := newPluginRuntime(host, fetch, c.Now)
	r.refreshAll(context.Background(), false)
	for i := 0; i < 5; i++ {
		r.refreshAll(context.Background(), true)
	}
	if calls != 1 {
		t.Fatalf("usage endpoint read %d times within the refresh gap, want 1", calls)
	}
	c.advance(minimumRefreshGap)
	r.refreshAll(context.Background(), true)
	if calls != 2 {
		t.Fatalf("calls = %d after the gap, want 2", calls)
	}
}

// The host HTML-escapes plugin metadata before the Management Center shows
// it, and the panel prints the entities literally, so these characters would
// render as &#34; and friends.
func TestConfigFieldDescriptionsSurviveHTMLEscaping(t *testing.T) {
	for _, field := range pluginRegistration().Metadata.ConfigFields {
		if strings.ContainsAny(field.Description, `"'<>&`) {
			t.Errorf("%s description contains an HTML-escaped character: %s", field.Name, field.Description)
		}
	}
}

func TestSidebarPageIsStaticAndLocked(t *testing.T) {
	r, _ := newTestRuntime(t, defaultPluginConfig(), map[string][2]float64{"secret-person@x.com": {10, 10}})
	resp := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: uiResourcePath})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("ui route: status=%d headers=%v", resp.StatusCode, resp.Headers)
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	// Resource routes are unauthenticated: the page must not carry account data.
	if strings.Contains(string(resp.Body), "secret-person") {
		t.Fatalf("the unauthenticated page embedded account data")
	}
	status := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v8/management" + managementStatusRoute})
	if status.StatusCode != http.StatusOK || !strings.Contains(string(status.Body), "secret-person@x.com") {
		t.Fatalf("status route: %d %s", status.StatusCode, status.Body)
	}
	if missing := r.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-balancer/other"}); missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path served %d", missing.StatusCode)
	}
}

func TestRegistrationAddsTheSidebarMenu(t *testing.T) {
	reg := managementRegistration()
	if len(reg.Resources) != 1 || reg.Resources[0].Path != uiResourcePath || reg.Resources[0].Menu == "" {
		t.Fatalf("resources = %#v", reg.Resources)
	}
}

func TestStatusMarksOverriddenAccounts(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Accounts = map[string]ceilings{"a@x.com": {FiveHour: 70, Weekly: 90}}
	r, _ := newTestRuntime(t, cfg, map[string][2]float64{"a@x.com": {0, 0}, "b@x.com": {0, 0}})
	for _, account := range r.status().Accounts {
		if want := account.Email == "a@x.com"; account.Override != want {
			t.Errorf("%s override = %v, want %v", account.Email, account.Override, want)
		}
	}
}

func TestRateLimitedAccountsBackOff(t *testing.T) {
	host := &fakeHost{entries: []pluginapi.HostAuthFileEntry{{
		ID: authIDFor("a@x.com"), AuthIndex: "1", Provider: "claude", Path: "/auth/a", Email: "a@x.com",
	}}}
	calls, limited := 0, true
	fetch := func(context.Context, string, time.Duration) (usageResult, string) {
		calls++
		if limited {
			return usageResult{}, pollErrorRateLimited
		}
		return usageResult{Weekly: windowReading{Used: 10}}, ""
	}
	c := &clock{now: testNow}
	r := newPluginRuntime(host, fetch, c.Now)
	cfg := defaultPluginConfig()
	r.config.Store(&cfg)
	poll := func(onDemand bool) { r.refreshAll(context.Background(), onDemand) }

	poll(false) // 429: back off one poll interval
	c.advance(time.Minute)
	poll(false)
	poll(true)
	if calls != 1 {
		t.Fatalf("polled %d times inside the backoff, want 1", calls)
	}
	c.advance(time.Minute + time.Second) // past 2m
	poll(false)                          // second 429: back off 4m
	c.advance(3 * time.Minute)
	poll(false)
	if calls != 2 {
		t.Fatalf("backoff did not double: calls=%d", calls)
	}
	if view := r.status().Accounts[0]; view.BackoffUntil == "" {
		t.Fatalf("status should report the backoff: %#v", view)
	}
	c.advance(2 * time.Minute)
	limited = false
	poll(false)
	if sample, _ := r.cache.snapshot(authIDFor("a@x.com")); !sample.HasSample || !sample.BackoffUntil.IsZero() || sample.RateLimits != 0 {
		t.Fatalf("a successful read should clear the backoff: %#v", sample)
	}
}

func TestRateLimitBackoffHonoursRetryAfterAndCaps(t *testing.T) {
	var q quotaCache
	q.samples = map[string]quotaSample{}
	if got := q.recordRateLimit("a", 90*time.Second, 2*time.Minute, testNow); got != 90*time.Second {
		t.Fatalf("Retry-After ignored: %v", got)
	}
	for i := 0; i < 10; i++ {
		q.recordRateLimit("b", 0, 2*time.Minute, testNow)
	}
	if got := q.recordRateLimit("b", 0, 2*time.Minute, testNow); got != maxRateLimitBackoff {
		t.Fatalf("backoff not capped: %v", got)
	}
	if got := parseRetryAfter("120", testNow); got != 2*time.Minute {
		t.Fatalf("parseRetryAfter seconds = %v", got)
	}
}
