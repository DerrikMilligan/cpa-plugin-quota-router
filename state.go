package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const stateFileVersion = 1

// persistedSample is the part of a reading worth keeping across restarts.
// It carries no credentials.
type persistedSample struct {
	Identity string        `json:"identity"`
	Email    string        `json:"email,omitempty"`
	FiveHour windowReading `json:"five_hour"`
	Weekly   windowReading `json:"weekly"`
	PolledAt time.Time     `json:"polled_at,omitzero"`
	HeaderAt time.Time     `json:"header_at,omitzero"`
}

type stateDocument struct {
	Version int                        `json:"version"`
	Samples map[string]persistedSample `json:"samples"`
}

func defaultStateFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "cpa-"+pluginName, "state.json")
}

// loadState seeds the cache from the state file once per runtime. Saved
// readings keep their timestamps, so stale ones are still re-read and are
// ineligible past max-staleness; reconcile drops accounts that changed.
func (r *pluginRuntime) loadState() {
	if r.stateFile == "" || r.stateLoaded {
		return
	}
	r.stateLoaded = true
	raw, err := os.ReadFile(r.stateFile)
	if err != nil {
		return
	}
	var doc stateDocument
	if json.Unmarshal(raw, &doc) != nil || doc.Version != stateFileVersion {
		return
	}
	restored := 0
	for authID, saved := range doc.Samples {
		if saved.Identity == "" {
			continue
		}
		r.cache.update(authID, func(s *quotaSample) {
			if s.HasSample {
				return
			}
			s.Identity, s.Email = saved.Identity, saved.Email
			s.FiveHour, s.Weekly = saved.FiveHour, saved.Weekly
			s.PolledAt, s.HeaderAt = saved.PolledAt, saved.HeaderAt
			s.HasSample = true
			restored++
		})
	}
	r.savedVersion = r.cache.currentVersion()
	r.log("info", "quota balancer restored cached readings", map[string]any{"accounts": restored})
}

// saveState writes the cache when it changed since the last write.
func (r *pluginRuntime) saveState() {
	if r.stateFile == "" {
		return
	}
	version := r.cache.currentVersion()
	if version == r.savedVersion {
		return
	}
	doc := stateDocument{Version: stateFileVersion, Samples: map[string]persistedSample{}}
	for authID, s := range r.cache.all() {
		if !s.HasSample || s.Identity == "" {
			continue
		}
		doc.Samples[authID] = persistedSample{
			Identity: s.Identity, Email: s.Email,
			FiveHour: s.FiveHour, Weekly: s.Weekly,
			PolledAt: s.PolledAt, HeaderAt: s.HeaderAt,
		}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.stateFile), 0o700); err != nil {
		return
	}
	tmp := r.stateFile + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil && os.Rename(tmp, r.stateFile) == nil {
		r.savedVersion = version
	}
}
