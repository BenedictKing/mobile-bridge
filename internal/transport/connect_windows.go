//go:build windows

package transport

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// ConnectStream mirrors connect_stream on Windows: the local named pipe
// (overlapped I/O and cancellation are provided by go-winio's DialPipe).
func ConnectStream(path string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(path, &timeout)
}
