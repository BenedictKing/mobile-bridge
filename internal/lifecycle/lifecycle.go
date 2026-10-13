// Package lifecycle is the Go port of bridge/lifecycle.py's control-file
// protocol: the cooperative stop record, its ASCII JSON serialization and the
// pairing response files. Byte parity with Python is enforced against
// conformance/lifecycle goldens.
package lifecycle

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

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
