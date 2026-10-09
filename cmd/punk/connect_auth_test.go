package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func authVerifyMCPServer(t *testing.T, wantToken string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "test"}, nil)
	type out struct {
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "who"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, out, error) {
			return nil, out{Namespace: "agent-test", Source: "test"}, nil
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	var accepted atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &accepted
}

func isolatedOpenCodeConnect(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(root, "missing-credentials.json"))
	t.Chdir(root)
	return root
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/CONFIG.md plan=client-auth test=TestConnectOpenCodeVerifyUsesConfiguredAPIKeyEnv
func TestConnectOpenCodeVerifyUsesConfiguredAPIKeyEnv(t *testing.T) {
	isolatedOpenCodeConnect(t)
	const selected = "dummy-selected-connect-token"
	t.Setenv("PUNK_API_KEY", "dummy-different-cli-token")
	t.Setenv("PUNK_SELECTED_TEST_KEY", selected)
	ts, accepted := authVerifyMCPServer(t, selected)

	_, err := captureStdout(t, func() error {
		return cmdConnectOpenCode([]string{
			"--project", "--no-skill", "--verify", "--url", ts.URL,
			"--api-key-env", "PUNK_SELECTED_TEST_KEY",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Load() == 0 {
		t.Fatal("verify never reached the MCP server with the selected environment credential")
	}
}

func TestConnectOpenCodeVerifyMissingConfiguredAPIKeyEnvFailsClosed(t *testing.T) {
	root := isolatedOpenCodeConnect(t)
	t.Setenv("PUNK_API_KEY", "dummy-cli-token-must-not-be-used")
	t.Setenv("PUNK_MISSING_TEST_KEY", "")
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(ts.Close)

	_, err := captureStdout(t, func() error {
		return cmdConnectOpenCode([]string{
			"--project", "--no-skill", "--verify", "--url", ts.URL,
			"--api-key-env", "PUNK_MISSING_TEST_KEY",
		})
	})
	if err == nil {
		t.Fatal("missing selected credential must fail verification")
	}
	if !strings.Contains(err.Error(), "PUNK_MISSING_TEST_KEY") {
		t.Fatalf("error must identify only the missing variable name: %v", err)
	}
	if strings.Contains(err.Error(), "dummy-cli-token") {
		t.Fatalf("error leaked a credential value: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("missing selected credential must fail before network access; requests=%d", requests.Load())
	}
	if _, statErr := os.Stat(filepath.Join(root, "opencode.json")); statErr != nil {
		t.Fatalf("connect should still write the isolated config before reporting verify failure: %v", statErr)
	}
}
