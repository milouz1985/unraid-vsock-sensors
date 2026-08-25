package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"cid":57,"port":20000}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.CID != 57 || config.Port != 20000 {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for name, config := range map[string]string{
		"CID below guest range": `{"cid":0,"port":19090}`,
		"CID any":               `{"cid":4294967295,"port":19090}`,
		"zero port":             `{"cid":42,"port":0}`,
		"port any":              `{"cid":42,"port":4294967295}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path); err == nil {
				t.Fatal("invalid configuration should be rejected")
			}
		})
	}
}

func TestLoadConfigUsesDefaultsWhenMissing(t *testing.T) {
	config, err := loadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config != defaultConfig {
		t.Fatalf("got %#v, want %#v", config, defaultConfig)
	}
}
