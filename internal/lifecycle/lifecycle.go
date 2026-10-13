// Package lifecycle is the Go port of bridge/lifecycle.py's control-file
// protocol: the cooperative stop record, its ASCII JSON serialization and the
// pairing response files. Byte parity with Python is enforced against
// conformance/lifecycle goldens.
package lifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type Controller struct {
	DataDir string
	Record  *jsoncompat.Object
	Path    string
	Request string
}

func NewController(dataDir string, pid int, instanceID string) (*Controller, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	record := ControlRecord(pid, randomToken(), ".pairing-go", instanceID, false, false, false, false)
	controller := &Controller{DataDir: dataDir, Record: record, Path: filepath.Join(dataDir, "gateway-control.json"), Request: filepath.Join(dataDir, "gateway.stop")}
	if err := os.WriteFile(controller.Path, DumpASCII(record), 0o600); err != nil {
		return nil, err
	}
	return controller, nil
}

func (c *Controller) StopRequested() bool {
	raw, err := os.ReadFile(c.Request)
	if err != nil {
		return false
	}
	value := ParseRecord(string(raw))
	return string(jsoncompat.EncodeCompact(value)) == string(jsoncompat.EncodeCompact(c.Record))
}

func (c *Controller) Close() {
	for _, path := range []string{c.Request, c.Path} {
		if raw, err := os.ReadFile(path); err == nil && string(jsoncompat.EncodeCompact(ParseRecord(string(raw)))) == string(jsoncompat.EncodeCompact(c.Record)) {
			_ = os.Remove(path)
		}
	}
}

func randomToken() string { return strconv.FormatInt(time.Now().UnixNano(), 16) }

// ErrNoControlRecord mirrors request_stop's refusal message; it never kills a
// PID that may have been reused.
var ErrNoControlRecord = errors.New(
	"No gateway control record; use Ctrl+C in its terminal if it is running.")

// ControlRecord builds the gateway-control.json payload with Python's key
// order: pid, token, then the start() update sequence.
func ControlRecord(pid int, token, pairingDir string, instanceID any,
	deviceManagement, accountsManagement, accountManagement, notificationManagement bool) *jsoncompat.Object {
	record := jsoncompat.NewObject()
	record.Set("pid", jsoncompat.Num(pid))
	record.Set("token", token)
	record.Set("pairingDir", pairingDir)
	record.Set("instanceId", instanceID)
	record.Set("deviceManagement", deviceManagement)
	record.Set("accountsManagement", accountsManagement)
	record.Set("accountManagement", accountManagement)
	record.Set("notificationManagement", notificationManagement)
	return record
}

// DumpASCII serializes control and pairing files: json.dumps defaults with
// ensure_ascii=True.
func DumpASCII(value any) []byte { return jsoncompat.EncodePythonDefaultASCII(value) }

// StopRequestValid mirrors request_stop's record guard. Like Python, a bool
// pid counts as an int (isinstance(True, int)).
func StopRequestValid(record any) error {
	object, ok := record.(*jsoncompat.Object)
	if !ok {
		return ErrNoControlRecord
	}
	pidValid := false
	switch pid := object.MustGet("pid").(type) {
	case json.Number:
		_, err := strconv.Atoi(pid.String())
		pidValid = err == nil
	case bool:
		pidValid = true
	}
	if !pidValid || !jsoncompat.Truthy(object.MustGet("token")) {
		return ErrNoControlRecord
	}
	return nil
}

// ParseRecord mirrors read_record: any parse failure yields nil.
func ParseRecord(text string) any {
	value, err := jsoncompat.Unmarshal([]byte(text))
	if err != nil {
		return nil
	}
	return value
}
