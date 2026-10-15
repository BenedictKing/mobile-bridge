package main

import "testing"

func TestParseArgs(t *testing.T) {
	socket, command, err := parseArgs([]string{"--socket", "/tmp/agent.sock", "--", "kimi", "acp"})
	if err != nil || socket != "/tmp/agent.sock" || len(command) != 2 || command[1] != "acp" {
		t.Fatalf("socket=%q command=%v err=%v", socket, command, err)
	}
	if _, _, err := parseArgs([]string{"--socket", "/tmp/agent.sock"}); err == nil {
		t.Fatal("accepted incomplete host arguments")
	}
}
