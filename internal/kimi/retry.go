package kimi

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const maxRetryWireBytes = 128 << 10

var retryWireFields = []string{
	"turnId", "step", "failedAttempt", "nextAttempt", "maxAttempts", "delayMs",
	"errorName", "errorMessage", "statusCode", "time",
}

// RetrySource reads live retry state from Kimi's main-agent event log.
type RetrySource struct {
	home string
	mu   sync.Mutex
	path map[string]string
}

func NewRetrySource(home string) *RetrySource {
	return &RetrySource{home: home, path: map[string]string{}}
}

func (s *RetrySource) Status(sessionID string) (*jsoncompat.Object, error) {
	path, err := s.wirePath(sessionID)
	if err != nil {
		return nil, err
	}
	raw, err := readWireTail(path)
	if err != nil {
		return nil, err
	}
	var retrying *jsoncompat.Object
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), maxRetryWireBytes+1)
	for scanner.Scan() {
		value, decodeErr := jsoncompat.Unmarshal(scanner.Bytes())
		event, _ := value.(*jsoncompat.Object)
		if decodeErr != nil || event == nil {
			continue
		}
		kind, _ := event.MustGet("type").(string)
		switch kind {
		case "turn.step.retrying":
			retrying = jsoncompat.NewObject()
			for _, field := range retryWireFields {
				if fieldValue, exists := event.Get(field); exists {
					retrying.Set(field, fieldValue)
				}
			}
		case "llm.request", "turn.prompt", "turn.ended", "turn.cancel":
			retrying = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return retrying, nil
}

func (s *RetrySource) wirePath(sessionID string) (string, error) {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, `/\\`) {
		return "", fmt.Errorf("invalid Kimi session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if path := s.path[sessionID]; path != "" {
		return path, nil
	}
	workspaces, err := os.ReadDir(filepath.Join(s.home, "sessions"))
	if err != nil {
		return "", err
	}
	for _, workspace := range workspaces {
		if !workspace.IsDir() || strings.HasPrefix(workspace.Name(), ".") {
			continue
		}
		path := filepath.Join(s.home, "sessions", workspace.Name(), sessionID, "agents", "main", "wire.jsonl")
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().IsRegular() {
			s.path[sessionID] = path
			return path, nil
		}
	}
	return "", os.ErrNotExist
}

func readWireTail(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := max(int64(0), info.Size()-maxRetryWireBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRetryWireBytes))
	if err != nil || start == 0 {
		return raw, err
	}
	newline := bytes.IndexByte(raw, '\n')
	if newline < 0 {
		return nil, nil
	}
	return raw[newline+1:], nil
}
