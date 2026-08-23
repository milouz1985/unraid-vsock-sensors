package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig, nil
	}
	if err != nil {
		return runtimeConfig{}, err
	}
	defer file.Close()

	var config runtimeConfig
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return runtimeConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return runtimeConfig{}, fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := validateConfig(config); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid %s: %w", path, err)
	}
	return config, nil
}

func validateConfig(config runtimeConfig) error {
	if config.CID < 3 {
		return errors.New("cid must be at least 3")
	}
	if config.Port == 0 {
		return errors.New("port must be greater than zero")
	}
	return nil
}
