//go:build !linux

package proxy

import "net"

// trySplice is a no-op on non-Linux platforms.
// Returns (false, 0, nil) so the caller falls back to io.Copy.
func trySplice(dst, src net.Conn) (bool, int64, error) {
	return false, 0, nil
}
