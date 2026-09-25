package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMessagingStdioConfigAndEnvironment(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "punk")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build %v %s", err, out)
	}
	for _, tc := range []struct {
		name     string
		config   bool
		env, set string
		want     bool
	}{
		{"default lean", false, "", "agent", false},
		{"file enabled", true, "", "agent", true},
		{"env enabled", false, "1", "agent", true},
		{"env disabled overrides file", true, "0", "agent", false},
		{"default full member preserved", false, "", "full", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "test.db")
			db, err := store.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.MigrateUp(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			config := filepath.Join(dir, "config.yaml")
			// JSON strings are valid YAML scalars; quote platform-specific paths.
			dsn, _ := json.Marshal(dbPath)
			specs, _ := json.Marshal(dir)
			body := fmt.Sprintf("db:\n  driver: sqlite\n  dsn: %s\nspecs:\n  dir: %s\nmessaging:\n  enabled: %t\n", dsn, specs, tc.config)
			if err := os.WriteFile(config, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "mcp", "--config", config, "--toolset", tc.set)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "USERPROFILE=" + dir, "SystemRoot=" + os.Getenv("SystemRoot"), "PUNK_MESSAGING=" + tc.env}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
			cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			res, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			present := map[string]bool{}
			for _, tool := range res.Tools {
				present[tool.Name] = true
			}
			if present["send_message"] != tc.want || present["read_messages"] != tc.want {
				t.Fatalf("gate want=%v have=%v", tc.want, present)
			}
			if present["list_region_members"] != (tc.want || tc.set == "full") || !present["register"] {
				t.Fatalf("region not wired: %v", present)
			}
			if tc.want {
				for _, a := range []string{"sender", "receiver"} {
					r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "register", Arguments: map[string]any{"namespace": "team", "agent": a}})
					if e != nil || r.IsError {
						t.Fatalf("register %v %+v", e, r)
					}
				}
				r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"namespace": "team", "sender": "sender", "recipient": "receiver", "body": "stdio works"}})
				if e != nil || r.IsError {
					t.Fatalf("send %v %+v", e, r)
				}
				r, e = cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_messages", Arguments: map[string]any{"namespace": "team", "agent": "receiver", "timeout_seconds": 1}})
				if e != nil || r.IsError {
					t.Fatalf("await %v %+v", e, r)
				}
			}
		})
	}
}
