package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostClient interface {
	listAuth() ([]pluginapi.HostAuthFileEntry, error)
	getAuth(authIndex string) (json.RawMessage, error)
	log(level, message string, fields map[string]any)
}

type claudeCredential struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

// minimumRefreshGap keeps on-demand refreshes (refusals, new accounts) from
// re-reading an account more often than this.
const minimumRefreshGap = 20 * time.Second

type pluginRuntime struct {
	lifecycleMu sync.Mutex
	pickMu      sync.Mutex
	config      atomic.Pointer[pluginConfig]
	cache       quotaCache
	sessions    *sessionStore
	lastPicked  map[string]time.Time
	host        hostClient
	fetch       usageFetcher
	now         func() time.Time
	wake        chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
	// instanceMarker names a per-process file the configured instance writes
	// instanceToken into. The host keeps a hot-reloaded plugin's previous
	// versions loaded without shutting them down, so each poll loop checks the
	// marker and stops once a newer instance has claimed it. Empty disables it.
	instanceMarker string
	instanceToken  []byte
	// pollMu guards pollDone, which is closed and replaced after every
	// refresh pass so picks can wait for fresh readings.
	pollMu   sync.Mutex
	pollDone chan struct{}
}

func newPluginRuntime(host hostClient, fetch usageFetcher, now func() time.Time) *pluginRuntime {
	if now == nil {
		now = time.Now
	}
	runtime := &pluginRuntime{
		cache:      quotaCache{samples: make(map[string]quotaSample)},
		sessions:   newSessionStore(defaultMaxSessions),
		lastPicked: make(map[string]time.Time),
		host:       host,
		fetch:      fetch,
		now:        now,
		pollDone:   make(chan struct{}),
	}
	cfg := defaultPluginConfig()
	runtime.config.Store(&cfg)
	return runtime
}

func newInstanceToken() []byte {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return []byte(pluginVersion + ":" + hex.EncodeToString(raw))
}

func defaultInstanceMarker() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("cpa-%s-%d.active", pluginName, os.Getpid()))
}

// claimInstance records this instance as the one that should poll.
func (r *pluginRuntime) claimInstance() {
	if r.instanceMarker == "" {
		return
	}
	tmp := r.instanceMarker + ".tmp"
	if err := os.WriteFile(tmp, r.instanceToken, 0o600); err == nil {
		_ = os.Rename(tmp, r.instanceMarker)
	}
}

// superseded reports a newer instance's claim. A missing or unreadable
// marker does not stop polling.
func (r *pluginRuntime) superseded() bool {
	if r.instanceMarker == "" {
		return false
	}
	current, err := os.ReadFile(r.instanceMarker)
	return err == nil && len(current) > 0 && !bytes.Equal(current, r.instanceToken)
}

func (r *pluginRuntime) loadedConfig() pluginConfig {
	if cfg := r.config.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func (r *pluginRuntime) applyConfig(cfg pluginConfig) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	previous := r.loadedConfig()
	r.config.Store(&cfg)
	if previous.AffinityTTL != cfg.AffinityTTL {
		r.sessions.reset()
	}
	fields := map[string]any{
		"five_hour_ceiling": cfg.Default.FiveHour,
		"weekly_ceiling":    cfg.Default.Weekly,
		"account_overrides": len(cfg.Accounts),
		"models":            cfg.Models,
		"poll_interval":     cfg.PollInterval.String(),
	}
	if !cfg.Enabled {
		r.stopLocked()
		return
	}
	r.claimInstance()
	if r.cancel != nil {
		if previous.Enabled && previous.PollInterval == cfg.PollInterval {
			r.log("info", "quota balancer reconfigured", fields)
			return
		}
		// Restart so a changed poll interval takes effect at once.
		r.stopLocked()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.wake, r.cancel, r.done = make(chan struct{}, 1), cancel, make(chan struct{})
	go r.refreshLoop(ctx, cfg.PollInterval, r.wake, r.done)
	r.log("info", "quota balancer started", fields)
}

func (r *pluginRuntime) shutdown() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stopLocked()
}

func (r *pluginRuntime) stopLocked() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.wake, r.cancel, r.done = nil, nil, nil
}

// requestRefresh asks the worker for an extra pass now; passes skip accounts
// read within minimumRefreshGap.
func (r *pluginRuntime) requestRefresh() {
	r.lifecycleMu.Lock()
	wake := r.wake
	r.lifecycleMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// waitForPoll blocks until the next refresh pass finishes or timeout passes.
func (r *pluginRuntime) waitForPoll(timeout time.Duration) {
	r.pollMu.Lock()
	done := r.pollDone
	r.pollMu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (r *pluginRuntime) refreshLoop(ctx context.Context, interval time.Duration, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	onDemand := false
	for {
		if r.superseded() {
			r.log("info", "quota balancer superseded by a newer plugin instance; polling stopped", nil)
			return
		}
		r.refreshAll(ctx, onDemand)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			onDemand = false
		case <-wake:
			onDemand = true
		}
	}
}

func (r *pluginRuntime) refreshAll(ctx context.Context, onDemand bool) {
	defer func() {
		r.pollMu.Lock()
		close(r.pollDone)
		r.pollDone = make(chan struct{})
		r.pollMu.Unlock()
	}()
	if r.host == nil || r.fetch == nil || ctx.Err() != nil {
		return
	}
	entries, err := r.host.listAuth()
	if err != nil {
		r.log("warn", "quota balancer auth discovery failed", map[string]any{"category": pollErrorAuthList})
		return
	}
	auths := physicalClaudeAuths(entries)
	r.cache.reconcile(auths)
	cfg := r.loadedConfig()
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		sample, _ := r.cache.snapshot(auth.ID)
		if r.now().Before(sample.BackoffUntil) {
			continue
		}
		if onDemand && r.now().Sub(sample.LastAttemptAt) < minimumRefreshGap {
			continue
		}
		r.pollAuth(ctx, auth, cfg)
	}
}

func (r *pluginRuntime) pollAuth(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig) {
	r.cache.recordAttempt(auth.ID, r.now())
	rawAuth, err := r.host.getAuth(auth.AuthIndex)
	if err != nil {
		r.recordPollFailure(auth, pollErrorAuthGet)
		return
	}
	var credential claudeCredential
	if json.Unmarshal(rawAuth, &credential) != nil || !strings.EqualFold(strings.TrimSpace(credential.Type), "claude") {
		r.recordPollFailure(auth, pollErrorInvalidCredential)
		return
	}
	token := strings.TrimSpace(credential.AccessToken)
	if token == "" {
		r.recordPollFailure(auth, pollErrorMissingToken)
		return
	}
	result, category := r.fetch(ctx, token, cfg.RequestTimeout)
	if category == pollErrorRateLimited {
		wait := r.cache.recordRateLimit(auth.ID, result.RetryAfter, cfg.PollInterval, r.now())
		r.cache.recordFailure(auth.ID, category)
		r.log("warn", "quota balancer usage read rate-limited; backing off", map[string]any{
			"auth_id": auth.ID,
			"backoff": wait.String(),
		})
		return
	}
	if category != "" {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			category = pollErrorTimeout
		}
		if category != pollErrorCancelled || ctx.Err() == nil {
			r.recordPollFailure(auth, category)
		}
		return
	}
	r.cache.recordPoll(auth.ID, result, r.now())
	r.log("debug", "quota balancer usage read", map[string]any{
		"auth_id":   auth.ID,
		"five_hour": result.FiveHour.Used,
		"weekly":    result.Weekly.Used,
	})
}

func (r *pluginRuntime) recordPollFailure(auth physicalClaudeAuth, category string) {
	r.cache.recordFailure(auth.ID, category)
	r.log("warn", "quota balancer usage read failed", map[string]any{
		"auth_id":  auth.ID,
		"category": category,
	})
}

func (r *pluginRuntime) log(level, message string, fields map[string]any) {
	if r != nil && r.host != nil {
		r.host.log(level, message, fields)
	}
}
