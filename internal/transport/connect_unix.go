//go:build !windows

package transport

import (
	"net"
	"time"
)

// ConnectStream mirrors connect_stream on POSIX: a Unix socket dial with a
// connect timeout and no read deadline afterwards.
func ConnectStream(path string, timeout time.Duration) (net.Conn, error) {
	connection, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	return connection, nil
}
