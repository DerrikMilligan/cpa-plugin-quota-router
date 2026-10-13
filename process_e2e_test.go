//go:build !race

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	e2eAPIKey        = "e2e-api-key"
	e2eManagementKey = "e2e-management-key"
)

var e2eUsageHits atomic.Int64

// TestCLIProxyAPIProcessEndToEnd loads the built plugin into a real CLIProxyAPI.
// Set CLIPROXYAPI_SRC to a host checkout (e.g. v8) to test against that host
// instead of the module this plugin builds against.
func TestCLIProxyAPIProcessEndToEnd(t *testing.T) {
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e2eUsageHits.Add(1)
		if r.Header.Get("anthropic-beta") != anthropicOAuthBeta {
			http.Error(w, "missing beta header", http.StatusBadRequest)
			return
		}
		weekly := 0
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "e2e-token-a":
			weekly = 80
		case "e2e-token-b":
			weekly = 20
		default:
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"five_hour":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":%d,"resets_at":"2099-01-01T00:00:00Z"}}`, weekly)
	}))
	defer usageServer.Close()

	proxyHits := make(chan string, 16)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case proxyHits <- r.Method + " " + r.Host:
		default:
		}
		http.Error(w, "fixture proxy rejects upstream", http.StatusBadGateway)
	}))
	defer proxyServer.Close()

	dir := t.TempDir()
	root := os.Getenv("CLIPROXYAPI_SRC")
	if root == "" {
		root = cliProxyAPIModuleDir(t)
	}
	pluginDir, authDir := filepath.Join(dir, "plugins"), filepath.Join(dir, "auth")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}

	extension, serverName := ".so", "cliproxyapi"
	switch runtime.GOOS {
	case "darwin":
		extension = ".dylib"
	case "windows":
		extension, serverName = ".dll", "cliproxyapi.exe"
	}
	pluginPath := filepath.Join(pluginDir, pluginName+extension)
	buildPlugin := exec.Command("go", "build", "-buildmode=c-shared", "-ldflags",
		"-X=main.usageEndpoint="+usageServer.URL+" -X=main.e2eShortIntervals=true", "-o", pluginPath, ".")
	if output, errBuild := buildPlugin.CombinedOutput(); errBuild != nil {
		t.Fatalf("build plugin: %v\n%s", errBuild, output)
	}

	serverPath := filepath.Join(dir, serverName)
	buildServer := exec.Command("go", "build", "-buildvcs=false", "-o", serverPath, "./cmd/server")
	buildServer.Dir = root
	if output, errBuild := buildServer.CombinedOutput(); errBuild != nil {
		t.Fatalf("build CLIProxyAPI: %v\n%s", errBuild, output)
	}

	for i, fixture := range []struct {
		name  string
		token string
	}{
		{name: "claude-a.json", token: "e2e-token-a"},
		{name: "claude-b.json", token: "e2e-token-b"},
	} {
		body := fmt.Sprintf(`{"type":"claude","email":"e2e-%d@example.com","access_token":%q,"refresh_token":"fixture-refresh","expired":"2099-01-01T00:00:00Z"}`, i, fixture.token)
		if err := os.WriteFile(filepath.Join(authDir, fixture.name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	port := unusedTCPPort(t)
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := fmt.Sprintf(`host: "127.0.0.1"
port: %d
proxy-url: %q
auth-dir: %q
api-keys: [%q]
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
debug: false
disable-cooling: true
plugins:
  enabled: true
  dir: %q
  configs:
    quota-balancer:
      enabled: true
      priority: 100
      five-hour-ceiling: 90
      weekly-ceiling: 90
      poll-interval: 100ms
      request-timeout: 1s
`, port, proxyServer.URL, authDir, e2eAPIKey, e2eManagementKey, pluginDir)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverPath, "-config", configPath, "-local-model")
	server.Dir = root
	server.Stdout, server.Stderr = logFile, logFile
	server.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
		"NO_PROXY=127.0.0.1,localhost",
	)
	if err := server.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = server.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		select {
		case <-processDone:
		default:
			_ = server.Process.Kill()
			<-processDone
		}
		_ = logFile.Close()
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	accountByEmail := func(status statusResponse, email string) accountView {
		for _, account := range status.Accounts {
			if account.Email == email {
				return account
			}
		}
		return accountView{}
	}
	waitForProcessStatus(t, client, baseURL, processDone, &processErr, logPath, func(status statusResponse) bool {
		return len(status.Accounts) == 2 &&
			accountByEmail(status, "e2e-0@example.com").Eligible && accountByEmail(status, "e2e-0@example.com").Weekly == 80 &&
			accountByEmail(status, "e2e-1@example.com").Eligible && accountByEmail(status, "e2e-1@example.com").Weekly == 20
	})
	model := waitForProcessClaudeModel(t, client, baseURL, processDone, &processErr, logPath)

	// A new conversation lands on the account with the most headroom (b: 70 vs a: 10).
	routed := postClaudeMessage(t, client, baseURL, model, "e2e-session")
	if bytes.Contains(routed, []byte(exhaustedErrorCode)) {
		t.Fatalf("eligible request was refused: %s\nserver log:\n%s", routed, readLog(logPath))
	}
	select {
	case hit := <-proxyHits:
		if !strings.Contains(hit, "api.anthropic.com:443") {
			t.Fatalf("unexpected upstream proxy target: %s", hit)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("request never reached upstream proxy: %s\nserver log:\n%s", routed, readLog(logPath))
	}
	// The fixture proxy fails every upstream call, so the host retries on the
	// other account; the first attempt shows which account the plugin chose.
	// Older hosts do not log the attempt's auth file; v8 does.
	if first := firstUpstreamAuthFile(readLog(logPath)); first == "" {
		t.Log("host does not log upstream attempts; skipping the chosen-account check")
	} else if first != "claude-b.json" {
		t.Fatalf("first upstream attempt used %q, want claude-b.json (most headroom)\nserver log:\n%s", first, readLog(logPath))
	}
drainProxyHits:
	for {
		select {
		case <-proxyHits:
			continue
		default:
			break drainProxyHits
		}
	}

	// Lower the weekly ceiling under both accounts: nothing may be routed.
	patchPluginConfig(t, client, baseURL, `{"weekly-ceiling":15}`)
	waitForProcessStatus(t, client, baseURL, processDone, &processErr, logPath, func(status statusResponse) bool {
		return status.Default.Weekly == 15 && len(status.Accounts) == 2 &&
			!status.Accounts[0].Eligible && !status.Accounts[1].Eligible
	})
	refused := postClaudeMessage(t, client, baseURL, model, "e2e-session")
	if !bytes.Contains(refused, []byte("no Claude account is under its usage ceiling")) {
		t.Fatalf("request over every ceiling was not refused: %s\nserver log:\n%s", refused, readLog(logPath))
	}
	select {
	case hit := <-proxyHits:
		t.Fatalf("refused request reached upstream proxy: %s", hit)
	case <-time.After(300 * time.Millisecond):
	}
	if e2eUsageHits.Load() == 0 {
		t.Fatalf("usage endpoint was never read")
	}
}

func firstUpstreamAuthFile(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if _, rest, ok := strings.Cut(line, "upstream execution failed:"); ok {
			if _, file, ok := strings.Cut(rest, "auth_file="); ok {
				return strings.Fields(file)[0]
			}
		}
	}
	return ""
}

func patchPluginConfig(t *testing.T, client *http.Client, baseURL, patch string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPatch, baseURL+"/v0/management/plugins/"+pluginName+"/config", strings.NewReader(patch))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eManagementKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if body := readResponseBody(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("patch plugin config: status=%d body=%s", response.StatusCode, body)
	}
}

func waitForProcessClaudeModel(t *testing.T, client *http.Client, baseURL string, processDone <-chan struct{}, processErr *error, logPath string) string {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before model registration: %v\n%s", *processErr, readLog(logPath))
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
		if response, err := client.Do(request); err == nil {
			var models struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			body := readResponseBody(t, response)
			if response.StatusCode == http.StatusOK && json.Unmarshal(body, &models) == nil {
				for _, m := range models.Data {
					if strings.HasPrefix(m.ID, "claude-") {
						return m.ID
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no Claude model was registered\n%s", readLog(logPath))
	return ""
}

func cliProxyAPIModuleDir(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/router-for-me/CLIProxyAPI/v7")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve CLIProxyAPI module: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func waitForProcessStatus(t *testing.T, client *http.Client, baseURL string, processDone <-chan struct{}, processErr *error, logPath string, ready func(statusResponse) bool) statusResponse {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before readiness: %v\n%s", *processErr, readLog(logPath))
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, baseURL+managementStatusFullPath, nil)
		request.Header.Set("Authorization", "Bearer "+e2eManagementKey)
		response, err := client.Do(request)
		if err == nil {
			var status statusResponse
			body := readResponseBody(t, response)
			if response.StatusCode == http.StatusOK && json.Unmarshal(body, &status) == nil && ready(status) {
				return status
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI status did not converge\n%s", readLog(logPath))
	return statusResponse{}
}
func postClaudeMessage(t *testing.T, client *http.Client, baseURL, model, session string) []byte {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model))
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eAPIKey)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", session)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return readResponseBody(t, response)
}

func readResponseBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func readLog(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
