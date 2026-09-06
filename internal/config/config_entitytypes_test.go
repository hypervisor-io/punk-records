package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEntityTypesDefaultOff(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.Memory.EntityTypes {
		t.Error("Memory.EntityTypes = true by default, want false (deterministic-first)")
	}
}

func TestEntityTypesParsesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte("memory:\n  entities: true\n  entity_types: true\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Memory.Entities || !c.Memory.EntityTypes {
		t.Fatalf("Entities=%v EntityTypes=%v, want both true", c.Memory.Entities, c.Memory.EntityTypes)
	}
}
