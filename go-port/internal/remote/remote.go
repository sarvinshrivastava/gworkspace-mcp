// Package remote: the client address of a request, as the TS read it from
// req.socket.remoteAddress — the host only, never the port.
package remote

import "net"

// Host strips the port from an http.Request.RemoteAddr ("1.2.3.4:5",
// "[::1]:5"). Audit dedupe, rate-limit buckets and the admin allowlist all key
// on it: with the port left in, every new connection is a new key.
func Host(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
