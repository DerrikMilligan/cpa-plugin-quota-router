package main

import (
	"context"
	"encoding/json"
	"errors"
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
	r.refreshAll(ctx, false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshAll(ctx, false)
		case <-wake:
			r.refreshAll(ctx, true)
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
		if onDemand {
			sample, _ := r.cache.snapshot(auth.ID)
			if r.now().Sub(sample.LastAttemptAt) < minimumRefreshGap {
				continue
			}
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
