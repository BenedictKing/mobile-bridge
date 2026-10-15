// Command agent-host keeps one ACP child alive while gateway processes restart.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/BenedictKing/mobile-bridge/internal/acphost"
)

func main() {
	socket, command, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := acphost.New(socket, command).Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (string, []string, error) {
	if len(args) < 3 || args[0] != "--socket" || args[2] != "--" {
		return "", nil, fmt.Errorf("用法：mobile-bridge-agent-host --socket PATH -- AGENT_COMMAND [ARGS...]")
	}
	if strings.TrimSpace(args[1]) == "" || len(args[3:]) == 0 {
		return "", nil, fmt.Errorf("Agent Host 需要 socket 路径和 Agent 命令")
	}
	return args[1], args[3:], nil
}
