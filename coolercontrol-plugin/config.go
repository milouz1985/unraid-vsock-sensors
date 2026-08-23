package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type runtimeConfig struct {
	CID  uint32 `json:"cid"`
	Port uint32 `json:"port"`
}

var defaultConfig = runtimeConfig{CID: 42, Port: 19090}

func configPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate plugin executable: %w", err)
	}
	return filepath.Join(filepath.Dir(executable), "config.json"), nil
}

func loadConfig(path string) (runtimeConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return defaultConfig, nil
	}
	if err != nil {
		return runtimeConfig{}, err
	}

	var config runtimeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return runtimeConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if config.CID < 3 {
		return runtimeConfig{}, fmt.Errorf("invalid %s: cid must be at least 3", path)
	}
	if config.Port == 0 {
		return runtimeConfig{}, fmt.Errorf("invalid %s: port must be greater than zero", path)
	}
	return config, nil
}
