//go:build linux

package proxy

import (
	"net"

	"golang.org/x/sys/unix"
)

// trySplice attempts zero-copy transfer using Linux splice(2) syscall.
// Returns (true, bytesMoved, err) if splice was used, (false, 0, nil) if caller
// should fall back to io.Copy (e.g. non-TCP connections).
func trySplice(dst, src net.Conn) (bool, int64, error) {
	// Both connections must be raw TCP to get file descriptors.
	srcTCP, ok := src.(*net.TCPConn)
	if !ok {
		return false, 0, nil
	}
	dstTCP, ok := dst.(*net.TCPConn)
	if !ok {
		return false, 0, nil
	}

	// Get raw file descriptors via SyscallConn (no dup, keeps non-blocking mode).
	srcRC, err := srcTCP.SyscallConn()
	if err != nil {
		return false, 0, nil
	}
	dstRC, err := dstTCP.SyscallConn()
	if err != nil {
		return false, 0, nil
	}

	// Create a kernel pipe as the intermediate buffer for splice.
	pipeFDs := make([]int, 2)
	if err := unix.Pipe2(pipeFDs, unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return false, 0, nil
	}
	pipeR := pipeFDs[0]
	pipeW := pipeFDs[1]
	defer unix.Close(pipeR)
	defer unix.Close(pipeW)

	// Increase pipe buffer to 64KB for better throughput.
	// Use raw IoctlSetInt instead of Fcntl which may not be available on all archs.
	unix.IoctlSetInt(pipeR, unix.F_SETPIPE_SZ, 65536) //nolint:errcheck

	var spliceErr error
	var moved int64

	// The outer Read call gives us the src fd.
	srcRC.Read(func(srcFD uintptr) bool {
		// The inner Write call gives us the dst fd.
		dstRC.Write(func(dstFD uintptr) bool {
			moved, spliceErr = splicePump(int(srcFD), int(dstFD), pipeR, pipeW)
			return true
		})
		return true
	})

	if spliceErr != nil {
		return true, moved, spliceErr
	}
	return true, moved, nil
}

// splicePump moves data: src → pipeW → pipeR → dst using splice(2), returning
// the total bytes delivered to dst. Runs until src returns EOF (n==0) or an
// error occurs; the byte count is returned either way, so a tunnel that dies
// mid-transfer still reports what it moved.
func splicePump(srcFD, dstFD, pipeR, pipeW int) (int64, error) {
	const spliceFlags = unix.SPLICE_F_MOVE | unix.SPLICE_F_NONBLOCK

	var total int64
	for {
		// Move data from src socket into the pipe write end.
		n, err := unix.Splice(srcFD, nil, pipeW, nil, 65536, spliceFlags)
		if err != nil {
			if err == unix.EAGAIN {
				if pollErr := pollFD(srcFD, false); pollErr != nil {
					return total, pollErr
				}
				continue
			}
			if n == 0 {
				return total, nil // EOF
			}
			return total, err
		}
		if n == 0 {
			return total, nil // EOF — src closed
		}

		// Drain the pipe into the dst socket.
		for written := int64(0); written < int64(n); {
			w, err := unix.Splice(pipeR, nil, dstFD, nil, int(int64(n)-written), spliceFlags)
			if err != nil {
				if err == unix.EAGAIN {
					if pollErr := pollFD(dstFD, true); pollErr != nil {
						return total, pollErr
					}
					continue
				}
				return total, err
			}
			written += int64(w)
			total += int64(w)
		}
	}
}

// pollFD waits for a file descriptor to become ready for reading or writing.
func pollFD(fd int, write bool) error {
	events := int16(unix.POLLIN)
	if write {
		events = unix.POLLOUT
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
	for {
		n, err := unix.Poll(fds, 60000)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			// Nothing ready within the poll window. An idle tunnel is not a dead
			// one — websockets and long-polling can sit quiet for far longer than
			// this — so renew the wait rather than tearing it down. The io.Copy
			// fallback imposes no idle limit either, so this keeps the two paths
			// behaving alike. POLLERR/POLLHUP/POLLNVAL are reported regardless of
			// Events, so a genuinely closed fd still wakes the poll and the next
			// splice observes the error.
			continue
		}
		return nil
	}
}
