package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rawCeilings struct {
	FiveHour *float64 `yaml:"five-hour-ceiling"`
	Weekly   *float64 `yaml:"weekly-ceiling"`
}

type rawPluginConfig struct {
	Enabled         *bool                  `yaml:"enabled"`
	Models          *[]string              `yaml:"models"`
	FiveHour        *float64               `yaml:"five-hour-ceiling"`
	Weekly          *float64               `yaml:"weekly-ceiling"`
	Accounts        map[string]rawCeilings `yaml:"accounts"`
	SpreadBand      *float64               `yaml:"spread-band"`
	RebalanceMargin *float64               `yaml:"rebalance-margin"`
	AffinityTTL     string                 `yaml:"affinity-ttl"`
	PollInterval    string                 `yaml:"poll-interval"`
	MaxStaleness    string                 `yaml:"max-staleness"`
	RequestTimeout  string                 `yaml:"request-timeout"`
}

// ceilings are percent-used limits; an account at or above either one takes
// no request until a reading shows it back under.
type ceilings struct {
	FiveHour float64 `json:"five_hour"`
	Weekly   float64 `json:"weekly"`
}

type pluginConfig struct {
	Enabled bool
	// Models is the lowercased set of model IDs routed; empty routes every Claude model.
	Models   []string
	Default  ceilings
	Accounts map[string]ceilings
	// SpreadBand treats accounts within this many headroom points of the best
	// as equal and spreads new conversations across them by live load.
	SpreadBand float64
	// RebalanceMargin moves a bound conversation once another account has this
	// many more headroom points than its own.
	RebalanceMargin float64
	AffinityTTL     time.Duration
	PollInterval    time.Duration
	MaxStaleness    time.Duration
	RequestTimeout  time.Duration
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:         true,
		Default:         ceilings{FiveHour: defaultFiveHourCeiling, Weekly: defaultWeeklyCeiling},
		Accounts:        map[string]ceilings{},
		SpreadBand:      defaultSpreadBand,
		RebalanceMargin: defaultRebalanceMargin,
		AffinityTTL:     defaultAffinityTTL,
		PollInterval:    defaultPollInterval,
		MaxStaleness:    defaultMaxStaleness,
		RequestTimeout:  defaultRequestTimeout,
	}
}

// ceilingsFor resolves an account's ceilings: an override keyed by email wins
// over one keyed by auth ID or file name, and unset fields keep the default.
func (c pluginConfig) ceilingsFor(email, authID, name string) ceilings {
	for _, key := range []string{email, authID, name} {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if override, ok := c.Accounts[key]; ok {
			return override
		}
	}
	return c.Default
}

func (c pluginConfig) routesModel(model string) bool {
	if len(c.Models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, routed := range c.Models {
		if routed == model {
			return true
		}
	}
	return false
}

func decodeLifecycleConfig(raw []byte) (pluginConfig, error) {
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return pluginConfig{}, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg := defaultPluginConfig()
	if len(request.ConfigYAML) == 0 {
		return cfg, nil
	}
	var decoded rawPluginConfig
	if err := yaml.Unmarshal(request.ConfigYAML, &decoded); err != nil {
		return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
	}
	if decoded.Enabled != nil {
		cfg.Enabled = *decoded.Enabled
	}
	if decoded.Models != nil {
		cfg.Models = normalizeModels(*decoded.Models)
	}
	if decoded.FiveHour != nil {
		cfg.Default.FiveHour = *decoded.FiveHour
	}
	if decoded.Weekly != nil {
		cfg.Default.Weekly = *decoded.Weekly
	}
	if err := validCeilings("default", cfg.Default); err != nil {
		return pluginConfig{}, err
	}
	for key, raw := range decoded.Accounts {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return pluginConfig{}, fmt.Errorf("accounts must not contain an empty key")
		}
		account := cfg.Default
		if raw.FiveHour != nil {
			account.FiveHour = *raw.FiveHour
		}
		if raw.Weekly != nil {
			account.Weekly = *raw.Weekly
		}
		if err := validCeilings(key, account); err != nil {
			return pluginConfig{}, err
		}
		cfg.Accounts[key] = account
	}
	if decoded.SpreadBand != nil {
		cfg.SpreadBand = *decoded.SpreadBand
	}
	if decoded.RebalanceMargin != nil {
		cfg.RebalanceMargin = *decoded.RebalanceMargin
	}
	if !finite(cfg.SpreadBand) || cfg.SpreadBand < 0 || cfg.SpreadBand > 100 {
		return pluginConfig{}, fmt.Errorf("spread-band must be between 0 and 100")
	}
	if !finite(cfg.RebalanceMargin) || cfg.RebalanceMargin < 0 || cfg.RebalanceMargin > 100 {
		return pluginConfig{}, fmt.Errorf("rebalance-margin must be between 0 and 100")
	}
	for _, field := range []struct {
		name  string
		value string
		into  *time.Duration
	}{
		{"affinity-ttl", decoded.AffinityTTL, &cfg.AffinityTTL},
		{"poll-interval", decoded.PollInterval, &cfg.PollInterval},
		{"max-staleness", decoded.MaxStaleness, &cfg.MaxStaleness},
		{"request-timeout", decoded.RequestTimeout, &cfg.RequestTimeout},
	} {
		value := strings.TrimSpace(field.value)
		if value == "" {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return pluginConfig{}, fmt.Errorf("%s must be a positive Go duration", field.name)
		}
		*field.into = parsed
	}
	if cfg.PollInterval < minimumPollInterval && e2eShortIntervals != "true" {
		cfg.PollInterval = minimumPollInterval
	}
	if cfg.MaxStaleness < 2*cfg.PollInterval {
		cfg.MaxStaleness = 2 * cfg.PollInterval
	}
	return cfg, nil
}

// e2eShortIntervals, set to "true" by the process test's -ldflags, lets it poll
// faster than the production floor.
var e2eShortIntervals = ""

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validCeilings(name string, c ceilings) error {
	if !finite(c.FiveHour) || c.FiveHour <= 0 || c.FiveHour > 100 {
		return fmt.Errorf("%s five-hour-ceiling must be above 0 and at most 100", name)
	}
	if !finite(c.Weekly) || c.Weekly <= 0 || c.Weekly > 100 {
		return fmt.Errorf("%s weekly-ceiling must be above 0 and at most 100", name)
	}
	return nil
}

func normalizeModels(models []string) []string {
	normalized := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" || model == "*" {
			// "*" or an empty entry means every Claude model.
			return nil
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	sort.Strings(normalized)
	return normalized
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{Routes: []managementRoute{{
		Method: http.MethodGet,
		Path:   managementStatusRoute,
	}}}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "Derrik Milligan (fork of Smarty Pants Inc quota-router)",
			GitHubRepository: "https://github.com/DerrikMilligan/cpa-plugin-quota-router",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Claude model IDs to balance, one per item. A single * or an empty list balances every Claude model. Default: all."},
				{Name: "five-hour-ceiling", Type: pluginapi.ConfigFieldTypeNumber, Description: "Hard limit in percent used on the 5-hour session window. An account at or above it takes no requests until a reading shows it back under. Default: 90."},
				{Name: "weekly-ceiling", Type: pluginapi.ConfigFieldTypeNumber, Description: "Hard limit in percent used on the weekly window. An account at or above it takes no requests until a reading shows it back under. Default: 90."},
				{Name: "accounts", Type: pluginapi.ConfigFieldTypeObject, Description: "Per-account ceilings that override the two defaults above, as a JSON object keyed by account email or auth file name. Each value can set five-hour-ceiling and weekly-ceiling in percent; a field left out uses the default. The example@example.com entry matches no account: copy it, change the email and set the numbers. Delete an entry to return that account to the defaults. A lower ceiling also means less traffic, since the account has less headroom."},
				{Name: "spread-band", Type: pluginapi.ConfigFieldTypeNumber, Description: "Accounts within this many headroom points of the best count as tied, and new conversations spread across them by load. Default: 10."},
				{Name: "rebalance-margin", Type: pluginapi.ConfigFieldTypeNumber, Description: "A running conversation moves to another account once that account has this many more headroom points than its own. Each move costs one cold prompt cache. Default: 25."},
				{Name: "affinity-ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long a conversation can sit idle before its account binding expires, as a duration such as 1h or 30m. Default: 1h."},
				{Name: "poll-interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often each account usage is read from Anthropic, as a duration (minimum 30s). Default: 2m."},
				{Name: "max-staleness", Type: pluginapi.ConfigFieldTypeString, Description: "A reading older than this makes the account ineligible; at least twice poll-interval. Default: 10m."},
				{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Timeout for one usage read, as a duration. Default: 10s."},
			},
		},
		Capabilities: registrationCapabilities{Scheduler: true, UsagePlugin: true, ManagementAPI: true},
	}
}
