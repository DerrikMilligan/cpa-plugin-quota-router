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
	defaultMaxSessions         = 65536
	minimumPollInterval        = 30 * time.Second
	defaultUsageEndpoint       = "https://api.anthropic.com/api/oauth/usage"
	anthropicOAuthBeta         = "oauth-2025-04-20"
	maxUsageResponseBytes      = 64 << 10
	exhaustedErrorCode         = "quota_balancer_exhausted"
	managementStatusRoute      = "/plugins/quota-balancer/status"
	managementStatusFullPath   = "/v0/management" + managementStatusRoute
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

var pluginVersion = "1.1.0"

// Variable only so the C-shared integration test can inject a local fixture.
var usageEndpoint = defaultUsageEndpoint

var activeRuntime = newPluginRuntime(
	cgoHostClient{},
	newHTTPUsageFetcher(usageEndpoint, nil).fetch,
	time.Now,
)

func main() {}
