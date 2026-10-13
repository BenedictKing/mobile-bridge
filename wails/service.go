package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func runAction(configured, action string, payload any) (map[string]any, error) {
	executable := resolveGateway(configured)
	dataDir := resolveDataDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, action, "--data-dir", dataDir)
	if payload != nil {
		body, _ := json.Marshal(payload)
		command.Stdin = bytes.NewReader(body)
	}
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, err
	}
	if !envelope.OK {
		if envelope.Error == "" {
			envelope.Error = "网关运行时操作失败"
		}
		return nil, errors.New(envelope.Error)
	}
	return envelope.Result, nil
}

func resolveGateway(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if configured = os.Getenv("MOBILE_BRIDGE_GATEWAY"); strings.TrimSpace(configured) != "" {
		return configured
	}
	if executable, err := os.Executable(); err == nil {
		base := filepath.Dir(executable)
		candidates := []string{
			filepath.Join(base, "gateway", "codex-mobile-gateway"),
			filepath.Join(base, "..", "Resources", "gateway", "codex-mobile-gateway"),
			filepath.Join(base, "..", "gateway", "codex-mobile-gateway"),
		}
		for _, candidate := range candidates {
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return filepath.Join("dist", "gateway", "codex-mobile-gateway")
}

func resolveDataDir() string {
	if configured := strings.TrimSpace(os.Getenv("MOBILE_BRIDGE_DATA_DIR")); configured != "" {
		return configured
	}
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		return filepath.Join(configDir, "mobile-bridge")
	}
	return ".local"
}
