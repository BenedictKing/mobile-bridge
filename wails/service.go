package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func runAction(configured, action string, payload any) (map[string]any, error) {
	executable := configured
	if executable == "" { executable = os.Getenv("MOBILE_BRIDGE_GATEWAY") }
	if executable == "" { executable = filepath.Join("dist", "gateway", "codex-mobile-gateway") }
	dataDir := os.Getenv("MOBILE_BRIDGE_DATA_DIR"); if dataDir == "" { dataDir = filepath.Join(".local") }
	command := exec.Command(executable, action, "--data-dir", dataDir)
	if payload != nil { body, _ := json.Marshal(payload); command.Stdin = bytes.NewReader(body) }
	output, err := command.Output(); if err != nil { return nil, err }
	var envelope struct { OK bool `json:"ok"`; Result map[string]any `json:"result"`; Error string `json:"error"` }
	if err := json.Unmarshal(output, &envelope); err != nil { return nil, err }
	if !envelope.OK { if envelope.Error == "" { envelope.Error = "网关运行时操作失败" }; return nil, errors.New(envelope.Error) }
	return envelope.Result, nil
}
