// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	errShutdownInProgress    = errors.New("fuse: shutdown already in progress")
	errShutdownNotConfigured = errors.New("fuse: shutdown requires MountOptions.FdCommSocket")
)

// Shutdown phases
const (
	// phaseInactive: normal processing, no shutdown in progress.
	phaseInactive = iota
	// phaseDrainNotify: new notifies fail with EINTR while readers keep
	// consuming the fd, so in-flight retrieves can drain their
	// _OP_NOTIFY_REPLY out of the kernel queue.
	phaseDrainNotify
	// phaseStopReaders: readers stop entering the read; the main loop
	// parks on cond, the idle ones exit.
	phaseStopReaders
)

type shutdownState struct {
	mu   *sync.Mutex
	cond *sync.Cond

	phase int
}

func newShutdownState(mu *sync.Mutex) *shutdownState {
	state := &shutdownState{mu: mu}
	state.cond = sync.NewCond(mu)
	return state
}

// Shutdown stops the server at a clean request boundary. It does not close the
// FUSE file descriptor or stop the process. On success, the caller must either
// save its business state and exit, or call Resume on the Server.
func (ms *Server) Shutdown(ctx context.Context) error {
	shutdown := ms.shutdown
	if shutdown == nil {
		return errShutdownNotConfigured
	}
	shutdown.mu.Lock()
	if shutdown.phase != phaseInactive {
		shutdown.mu.Unlock()
		return errShutdownInProgress
	}
	shutdown.phase = phaseDrainNotify
	shutdown.mu.Unlock()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return ms.abortShutdown(err)
		}
		ms.retrieveMu.Lock()
		retrieves := len(ms.retrieveTab)
		ms.retrieveMu.Unlock()
		if retrieves == 0 {
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	shutdown.mu.Lock()
	shutdown.phase = phaseStopReaders
	shutdown.mu.Unlock()

	for {
		shutdown.mu.Lock()
		if err := ctx.Err(); err != nil {
			shutdown.mu.Unlock()
			return ms.abortShutdown(err)
		}
		ms.interruptMu.Lock()
		requests := len(ms.reqInflight)
		ms.interruptMu.Unlock()
		readers := ms.fuseFD.reqReaders
		if readers == 0 && requests == 0 {
			shutdown.mu.Unlock()
			return nil
		}
		shutdown.mu.Unlock()

		// Readers stay blocked in syscall.Read when no FUSE requests are pending.
		// Generate enough requests for them to return and stop at the shutdown check.
		for i := 0; i < readers; i++ {
			ms.wakeupRequest()
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (ms *Server) abortShutdown(err error) error {
	shutdown := ms.shutdown
	shutdown.mu.Lock()
	shutdown.phase = phaseInactive
	shutdown.cond.Broadcast()
	shutdown.mu.Unlock()
	return err
}

// Resume restores request processing after a successful Shutdown.
func (ms *Server) Resume() {
	shutdown := ms.shutdown
	if shutdown == nil {
		return
	}
	shutdown.mu.Lock()
	shutdown.phase = phaseInactive
	shutdown.cond.Broadcast()
	shutdown.mu.Unlock()
}

func (ms *Server) wakeupRequest() {
	if ms.mountPoint == "" {
		return
	}
	go func() {
		var st syscall.Statfs_t
		_ = syscall.Statfs(ms.mountPoint, &st)
	}()
}

func (ms *Server) adoptRestartSession() (bool, error) {
	path := ms.opts.FdCommSocket
	if path == "" {
		return false, nil
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false, fmt.Errorf("receive FUSE fd: %w", err)
	}
	defer conn.Close()

	setting, fds, err := getFd(conn.(*net.UnixConn), 2)
	if err != nil {
		return false, fmt.Errorf("receive FUSE fd: %w", err)
	}
	if len(fds) > 0 {
		_ = syscall.Close(fds[0])
	}
	if len(fds) != 2 {
		return false, nil
	}
	fd := fds[1]
	if len(setting) < int(unsafe.Sizeof(InitIn{})) {
		syscall.Close(fd)
		return false, fmt.Errorf("short FUSE setting: %d", len(setting))
	}

	syscall.CloseOnExec(fd)
	fuseFD, err := ms.newFuseFD(fd)
	if err != nil {
		return false, err
	}
	ms.fuseFD = fuseFD
	ms.protocolServer.writev = fuseFD.writev
	ms.shutdown = newShutdownState(&fuseFD.reqMu)
	if !ms.opts.CleanRestart {
		fuseFD.recentUnique = make([]uint64, 0, 30)
	}
	settings := unsafe.Slice((*byte)(unsafe.Pointer(&ms.kernelSettings)), int(unsafe.Sizeof(InitIn{})))
	copy(settings, setting)
	if err := ms.publishRestartSession(); err != nil {
		fuseFD.close()
		return false, err
	}
	if ms.kernelSettings.Minor >= 13 {
		ms.setSplice()
	}
	ms.fileSystem.Init(ms)
	close(ms.ready)
	return true, nil
}

// checkLostRequests interrupts requests that the old process read from the
// shared FUSE session but exited before replying to. It samples new request
// Unique values, interrupts gaps in the stable half of the sample, then probes
// older values for a request that precedes the entire sample. It is only
// started after taking over a dirty session.
func (ms *Server) checkLostRequests() {
	go func() {
		// A recovered session may have no user traffic. Generate requests so we
		// can sample recent Unique values and find requests lost by the old process.
		for i := 0; i < 30; i++ {
			ms.wakeupRequest()
			time.Sleep(100 * time.Millisecond)
		}
	}()

	start := time.Now()
	time.Sleep(3 * time.Second)
	var recentUnique []uint64
	for {
		ms.fuseFD.reqMu.Lock()
		if ms.shutdown.phase != phaseInactive {
			ms.fuseFD.recentUnique = nil
			ms.fuseFD.reqMu.Unlock()
			return
		}
		used := time.Since(start)
		if len(ms.fuseFD.recentUnique) >= 30 ||
			len(ms.fuseFD.recentUnique) > 1 && used > 10*time.Second {
			recentUnique = ms.fuseFD.recentUnique
			ms.fuseFD.recentUnique = nil
			ms.fuseFD.reqMu.Unlock()
			break
		}
		if used > 30*time.Second {
			ms.fuseFD.recentUnique = nil
			ms.fuseFD.reqMu.Unlock()
			ms.opts.Logger.Printf("FUSE: no requests observed during recovery after %s", used)
			return
		}
		ms.fuseFD.reqMu.Unlock()
		time.Sleep(time.Second)
	}

	sort.Slice(recentUnique, func(i, j int) bool {
		return recentUnique[i] < recentUnique[j]
	})
	last := recentUnique[0]
	for _, unique := range recentUnique[:len(recentUnique)/2] {
		for unique > last+1 {
			last++
			ms.returnInterrupted(last)
		}
		last = unique
	}

	last = recentUnique[0] - 1
	for checked := 0; last > 0 && checked < 6e6; checked++ {
		ms.returnInterrupted(last)
		last--
	}
}

func (ms *Server) returnInterrupted(unique uint64) {
	header := make([]byte, sizeOfOutHeader)
	out := (*OutHeader)(unsafe.Pointer(&header[0]))
	out.Unique = unique
	out.Status = -int32(syscall.EINTR)
	out.Length = uint32(sizeOfOutHeader)
	err := handleEINTR(func() error {
		_, err := ms.fuseFD.writevFD([][]byte{header})
		return err
	})
	if err == nil {
		ms.opts.Logger.Printf("FUSE: interrupt request %d", unique)
	}
}

func (ms *Server) publishRestartSession() error {
	if ms.opts.FdCommSocket == "" {
		return nil
	}
	var sendErr error
	if err := ms.fuseFD.withFD(func(fd int) {
		sendErr = sendFuseFd(ms.opts.FdCommSocket, ms.kernelSettingsBytes(), fd)
	}); err != nil {
		return fmt.Errorf("access FUSE fd: %w", err)
	}
	if sendErr != nil {
		return fmt.Errorf("publish FUSE fd: %w", sendErr)
	}
	return nil
}

func (ms *Server) kernelSettingsBytes() []byte {
	settings := unsafe.Slice((*byte)(unsafe.Pointer(&ms.kernelSettings)), int(unsafe.Sizeof(InitIn{})))
	return append([]byte(nil), settings...)
}
