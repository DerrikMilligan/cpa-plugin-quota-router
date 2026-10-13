package main

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionHeaders are the client conversation signals, strongest first. They
// mirror the host's own session-affinity extraction for header-borne IDs.
var sessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"Session-Id",
	"Session_id",
	"X-Session-Id",
	"X-Session-Affinity",
}

// sessionKey names a conversation per model: the prompt cache is per model,
// so each model a conversation uses is placed on its own.
func sessionKey(headers map[string][]string, model string) string {
	for _, name := range sessionHeaders {
		for key, values := range headers {
			if !strings.EqualFold(key, name) && http.CanonicalHeaderKey(key) != http.CanonicalHeaderKey(name) {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return value + "::" + strings.ToLower(strings.TrimSpace(model))
				}
			}
		}
	}
	return ""
}

type binding struct {
	AuthID   string
	LastSeen time.Time
}

type sessionStore struct {
	mu         sync.Mutex
	bindings   map[string]binding
	maxEntries int
}

func newSessionStore(maxEntries int) *sessionStore {
	return &sessionStore{bindings: make(map[string]binding), maxEntries: maxEntries}
}

func (s *sessionStore) get(key string, now time.Time, ttl time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[key]
	if !ok {
		return "", false
	}
	if now.Sub(b.LastSeen) > ttl {
		delete(s.bindings, key)
		return "", false
	}
	return b.AuthID, true
}

func (s *sessionStore) bind(key, authID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.bindings[key]; !exists && len(s.bindings) >= s.maxEntries {
		oldestKey, oldest := "", now
		for k, b := range s.bindings {
			if oldestKey == "" || b.LastSeen.Before(oldest) {
				oldestKey, oldest = k, b.LastSeen
			}
		}
		delete(s.bindings, oldestKey)
	}
	s.bindings[key] = binding{AuthID: authID, LastSeen: now}
}

// live counts unexpired bindings per account and drops expired ones.
func (s *sessionStore) live(now time.Time, ttl time.Duration) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int)
	for key, b := range s.bindings {
		if now.Sub(b.LastSeen) > ttl {
			delete(s.bindings, key)
			continue
		}
		counts[b.AuthID]++
	}
	return counts
}

func (s *sessionStore) reset() {
	s.mu.Lock()
	clear(s.bindings)
	s.mu.Unlock()
}
