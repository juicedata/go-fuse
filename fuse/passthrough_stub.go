//go:build !linux

// FUSE passthrough is a Linux-only feature (kernel >= 6.9). On other platforms
// the API is present but inert, so cross-platform callers compile unchanged.

package fuse

import "syscall"

// RegisterBackingFd is unsupported on non-Linux platforms.
func (ms *Server) RegisterBackingFd(m *BackingMap) (int32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

// UnregisterBackingFd is unsupported on non-Linux platforms.
func (ms *Server) UnregisterBackingFd(id int32) syscall.Errno {
	return syscall.ENOSYS
}

// SupportsPassthrough always reports false on non-Linux platforms.
func (ms *Server) SupportsPassthrough() bool { return false }

func negotiatePassthrough(server *Server, input *InitIn, out *InitOut) {}
