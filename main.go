package main

import "time"

const (
	pluginName                 = "quota-balancer"
	defaultFiveHourCeiling     = 90.0
	defaultWeeklyCeiling       = 90.0
	defaultPollInterval        = 2 * time.Minute
	defaultMaxStaleness        = 10 * time.Minute
	defaultRequestTimeout      = 10 * time.Second
	defaultAffinityTTL         = time.Hour
	defaultSpreadBand          = 10.0
	defaultRebalanceMargin     = 25.0
	defaultFiveHourWeight      = 0.25
	defaultResetWeight         = 1.0
	weeklyWindow               = 7 * 24 * time.Hour
	defaultMaxSessions         = 65536
	minimumPollInterval        = 30 * time.Second
	maxRateLimitBackoff        = 15 * time.Minute
	usageReadSpacing           = 5 * time.Second
	defaultUsageEndpoint       = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeCLIUserAgent         = "claude-cli/2.1.280 (external, cli)"
	resetClaimTimeout          = 25 * time.Second
	anthropicOAuthBeta         = "oauth-2025-04-20"
	maxUsageResponseBytes      = 64 << 10
	exhaustedErrorCode         = "quota_balancer_exhausted"
	managementStatusRoute      = "/plugins/quota-balancer/status"
	managementStatusFullPath   = "/v0/management" + managementStatusRoute
	managementResetRoute       = "/plugins/quota-balancer/reset"
	uiResourcePath             = "/v0/resource/plugins/" + pluginName + "/ui"
	pollErrorAuthList          = "auth_list"
	pollErrorAuthGet           = "auth_get"
	pollErrorInvalidCredential = "invalid local credential"
	pollErrorMissingToken      = "missing_token"
	pollErrorInvalidWindow     = "invalid_window"
	pollErrorInvalidJSON       = "invalid_json"
	pollErrorBodyTooLarge      = "body_too_large"
	pollErrorTimeout           = "timeout"
	pollErrorCancelled         = "cancelled"
	pollErrorNetwork           = "network"
	pollErrorUnauthorized      = "unauthorized"
	pollErrorForbidden         = "forbidden"
	pollErrorRateLimited       = "rate_limited"
	pollErrorServer            = "server_error"
	pollErrorHTTP              = "http_error"
	pollErrorRead              = "read_error"
)

var pluginVersion = "1.6.0"

// Variables only so tests can inject local fixtures.
var (
	usageEndpoint = defaultUsageEndpoint
	claudeAPIBase = "https://api.anthropic.com"
)

var activeRuntime = func() *pluginRuntime {
	r := newPluginRuntime(cgoHostClient{}, newHTTPUsageFetcher(usageEndpoint, nil).fetch, time.Now)
	r.instanceMarker, r.instanceToken = defaultInstanceMarker(), newInstanceToken()
	r.stateFile = defaultStateFile()
	r.readSpacing = usageReadSpacing
	return r
}()

func main() {}
