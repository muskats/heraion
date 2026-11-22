package mcp

import "time"

// SetConnectTimeout shortens connectTimeout until the returned func runs.
func SetConnectTimeout(d time.Duration) (restore func()) {
	old := connectTimeout
	connectTimeout = d
	return func() { connectTimeout = old }
}
