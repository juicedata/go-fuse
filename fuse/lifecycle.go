// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// FuseFile returns a duplicate of the FUSE connection file
// descriptor, so the connection can be handed to another server, see
// NewServerWithFD. The connection stays alive while the returned file
// is open, also after this server has shut down.
func (ms *Server) FuseFile() (*os.File, error) {
	fd, err := dupFD(ms.fuseFD.file)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/fuse"), nil
}

func dupFD(f *os.File) (int, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var dupErr error
	if err := conn.Control(func(orig uintptr) {
		fd, dupErr = unix.FcntlInt(orig, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return -1, err
	}
	return fd, dupErr
}

// Start serves requests in the background. It resumes a stopped server,
// and does nothing on a running one. Use Wait to wait for the server to
// finish.
func (ms *Server) Start() error {
	ms.lifecycleMu.Lock()
	defer ms.lifecycleMu.Unlock()
	r := ms.fuseFD
	r.reqMu.Lock()
	defer r.reqMu.Unlock()
	if r.state == fuseFDClosing {
		return os.ErrClosed
	}
	ms.stopping.Store(false)
	if r.state == fuseFDStopped {
		r.state = fuseFDRunning
		r.loops.Add(1)
		go ms.loop()
	}
	return nil
}

// Stop stops reading requests, and waits until the requests read so
// far are answered. The connection stays open: the kernel queues new
// requests until Start is called on this server, or on a server that
// took over the connection. While stopping and stopped, notifications
// fail with EINTR.
//
// If ctx expires first, Stop returns ctx.Err() with the connection
// still open. Call Start to resume serving.
func (ms *Server) Stop(ctx context.Context) error {
	ms.lifecycleMu.Lock()
	defer ms.lifecycleMu.Unlock()
	return ms.stop(ctx)
}

func (ms *Server) stop(ctx context.Context) error {
	r := ms.fuseFD
	r.reqMu.Lock()
	closing := r.state == fuseFDClosing
	r.reqMu.Unlock()
	if closing {
		return os.ErrClosed
	}
	if parseFuseFd(ms.mountPoint) >= 0 {
		return fmt.Errorf("fuse: cannot stop a server on magic mountpoint %q", ms.mountPoint)
	}

	ms.stopping.Store(true)
	// Keep reading until pending retrieves got their NOTIFY_REPLY.
	if err := waitUntil(ctx, func() bool {
		ms.retrieveMu.Lock()
		defer ms.retrieveMu.Unlock()
		return len(ms.retrieveTab) == 0
	}); err != nil {
		return err
	}

	r.reqMu.Lock()
	if r.state != fuseFDClosing {
		r.state = fuseFDStopped
	}
	readers := r.reqReaders
	r.reqMu.Unlock()
	// Idle readers are blocked in read(2); a STATFS lets each of them return.
	mnt := ms.mountPoint
	for i := 0; i < readers; i++ {
		go func() {
			var st syscall.Statfs_t
			syscall.Statfs(mnt, &st)
		}()
	}
	return waitUntil(ctx, func() bool {
		r.reqMu.Lock()
		defer r.reqMu.Unlock()
		// Held from before read(2) until the reply is written.
		return r.inflightRequestBytes == 0
	})
}

func waitUntil(ctx context.Context, cond func() bool) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// Shutdown stops the server like Stop, closes its file descriptor
// and calls RawFileSystem.OnUnmount. It does not unmount: the mount
// keeps working if another server took over the connection (see
// FuseFile), and is disconnected otherwise. Use Unmount to stop
// serving for good.
//
// If stopping is canceled, the connection remains open. Once resource
// cleanup starts, canceling ctx only stops waiting for it to finish.
func (ms *Server) Shutdown(ctx context.Context) error {
	ms.lifecycleMu.Lock()
	err := ms.stop(ctx)
	if err == nil {
		ms.teardown()
	}
	ms.lifecycleMu.Unlock()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	select {
	case <-ms.done:
		return ms.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// teardown releases the connection once all readers have exited. It
// runs once, on Shutdown, Unmount or a fatal read error.
func (ms *Server) teardown() {
	r := ms.fuseFD
	r.reqMu.Lock()
	if r.state == fuseFDClosing {
		r.reqMu.Unlock()
		return
	}
	r.state = fuseFDClosing
	r.reqMu.Unlock()

	go func() {
		r.loops.Wait()
		ms.closeErr = r.close()

		// shutdown in-flight cache retrieves.
		//
		// It is possible that umount comes in the middle - after retrieve
		// request was sent to kernel, but corresponding kernel reply has not
		// yet been read. We unblock all such readers and wake them up with ENODEV.
		ms.retrieveMu.Lock()
		rtab := ms.retrieveTab
		// retrieve attempts might be erroneously tried even after close
		// we have to keep retrieveTab !nil not to panic.
		ms.retrieveTab = make(map[uint64]*retrieveCacheRequest)
		ms.retrieveMu.Unlock()
		for _, reading := range rtab {
			reading.n = 0
			reading.st = ENODEV
			close(reading.ready)
		}

		ms.fileSystem.OnUnmount()
		close(ms.done)
	}()
}
