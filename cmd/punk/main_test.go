package main

import "testing"

func TestOpenCodePathsGlobalUsesOpencodeDir(t *testing.T) {
	plugin, cfg := openCodePaths(false, "", "/home/u")
	if cfg != "/home/u/.config/opencode/opencode.json" {
		t.Fatalf("config path = %s", cfg)
	}
	if plugin != "/home/u/.config/opencode/plugins/punk-memory.js" {
		t.Fatalf("plugin path = %s", plugin)
	}
	_, cfg = openCodePaths(false, "/xdg", "/home/u")
	if cfg != "/xdg/opencode/opencode.json" {
		t.Fatalf("xdg config path = %s", cfg)
	}
	plugin, cfg = openCodePaths(true, "/xdg", "/home/u")
	if plugin != ".opencode/plugins/punk-memory.js" || cfg != "opencode.json" {
		t.Fatalf("project paths = %s %s", plugin, cfg)
	}
}
