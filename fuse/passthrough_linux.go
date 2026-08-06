// FUSE passthrough support (Linux, kernel >= 6.9).
//
// Lets the daemon register a real backing file descriptor with the kernel so
// that, for files opened with FOPEN_PASSTHROUGH + a valid OpenOut.BackingID,
// the kernel serves read() and write() directly from the backing file without
// an upcall to the daemon.

package fuse

import (
	"log"
	"syscall"
	"unsafe"
)

const (
	_DEV_IOC_BACKING_OPEN  = 0x4010e501
	_DEV_IOC_BACKING_CLOSE = 0x4004e502
)

// negotiatePassthrough advertises CAP_PASSTHROUGH at INIT when the daemon
// opted in and the kernel supports it. The kernel only reads Flags2 when
// CAP_INIT_EXT is set in Flags.
func negotiatePassthrough(server *Server, input *InitIn, out *InitOut) {
	// input.Flags2 (added in minor 36) is only valid to read if this INIT
	// request's minor version actually carries it — an older kernel's
	// shorter INIT never wrote those bytes, so input.Flags2 would otherwise
	// read leftover content from an unrelated prior request sharing the
	// same pooled, never-zeroed buffer (see _MINOR_VERSION_INIT_EXT).
	if !server.opts.EnablePassthrough || input.Minor < _MINOR_VERSION_INIT_EXT ||
		input.Flags2&uint32(CAP_PASSTHROUGH>>32) == 0 {
		return
	}
	// Kernel cap is FUSE_MAX_MAX_STACK_DEPTH (2); clamp locally rather than
	// relying solely on the kernel to reject an out-of-range caller value.
	msd := server.opts.MaxStackDepth
	if msd <= 0 || msd > 2 {
		msd = 2
	}
	out.Flags |= CAP_INIT_EXT
	out.Flags2 |= uint32(CAP_PASSTHROUGH >> 32)
	out.MaxStackDepth = uint32(msd)
}

// RegisterBackingFd registers a backing file descriptor with the kernel via
// FUSE_DEV_IOC_BACKING_OPEN. On success it returns a backing ID (>0) to set in
// OpenOut.BackingID alongside FOPEN_PASSTHROUGH. Release it with
// UnregisterBackingFd once no open file references it. The backing file must
// live on a non-stacked filesystem (tmpfs/ext4/xfs), not overlayfs or fuse.
func (ms *Server) RegisterBackingFd(m *BackingMap) (int32, syscall.Errno) {
	id, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(ms.mountFd), uintptr(_DEV_IOC_BACKING_OPEN), uintptr(unsafe.Pointer(m)))
	if ms.opts.Debug {
		log.Printf("ioctl: BACKING_OPEN {fd %d, flags %#x}: id %d (%v)", m.Fd, m.Flags, int32(id), errno)
	}
	return int32(id), errno
}

// UnregisterBackingFd releases a backing ID via FUSE_DEV_IOC_BACKING_CLOSE.
func (ms *Server) UnregisterBackingFd(id int32) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(ms.mountFd), uintptr(_DEV_IOC_BACKING_CLOSE), uintptr(unsafe.Pointer(&id)))
	if ms.opts.Debug {
		log.Printf("ioctl: BACKING_CLOSE id %d: %v", id, errno)
	}
	return errno
}

// SupportsPassthrough reports whether passthrough was both requested
// (MountOptions.EnablePassthrough) and advertised by the kernel at INIT.
// Valid only after the FUSE INIT handshake has completed.
func (ms *Server) SupportsPassthrough() bool {
	ms.reqMu.Lock()
	defer ms.reqMu.Unlock()
	return ms.opts.EnablePassthrough &&
		ms.kernelSettings.Flags2&uint32(CAP_PASSTHROUGH>>32) != 0
}
