// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kernel requires a queue for every possible CPU, including offline CPUs.
// It indexes them by CPU ID, so reject sparse masks rather than misrouting IO.
func parsePossibleCPUs(s string) (int, error) {
	next := 0
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi, rangeOK := strings.Cut(part, "-")
		if !rangeOK {
			hi = lo
		}
		l, e1 := strconv.Atoi(lo)
		h, e2 := strconv.Atoi(hi)
		if e1 != nil || e2 != nil || l != next || h < l || h >= 65536 {
			return 0, fmt.Errorf("unsupported possible CPU mask %q", s)
		}
		next = h + 1
	}
	return next, nil
}

func uringRingCount(requested, cpus int) (int, error) {
	if requested < 0 || requested > cpus {
		return 0, fmt.Errorf("FUSE io_uring rings must be in [0, %d]", cpus)
	}
	if requested == 0 {
		return min(4, cpus), nil
	}
	return requested, nil
}

func (ms *Server) prepareUring() (err error) {
	if !ms.opts.EnableIoUring {
		return nil
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("FUSE io_uring supports Linux amd64 and arm64")
	}
	if parseFuseFd(ms.mountPoint) >= 0 {
		return fmt.Errorf("FUSE io_uring cannot attach to an inherited FUSE fd")
	}
	depth := ms.opts.IoUringQueueDepth
	if depth == 0 {
		depth = 4
	}
	if depth < 1 || depth > 256 || depth&(depth-1) != 0 {
		return fmt.Errorf("FUSE io_uring queue depth must be a power of two in [1, 256]")
	}
	b, err := os.ReadFile("/sys/module/fuse/parameters/enable_uring")
	if err != nil || (strings.TrimSpace(string(b)) != "Y" && strings.TrimSpace(string(b)) != "1") {
		return fmt.Errorf("FUSE io_uring requires Linux 6.14+ with CONFIG_FUSE_IO_URING and /sys/module/fuse/parameters/enable_uring enabled")
	}
	b, err = os.ReadFile("/sys/devices/system/cpu/possible")
	if err != nil {
		return err
	}
	cpus, err := parsePossibleCPUs(string(b))
	if err != nil {
		return err
	}
	// Allocate before negotiating INIT: allocation failure must not leave the
	// kernel waiting for queues that can never be registered.
	defer func() {
		if err != nil {
			ms.closeUring()
		}
	}()
	payload := max(_FUSE_MIN_READ_BUFFER, (ms.opts.MaxWrite+syscall.Getpagesize()-1)/syscall.Getpagesize()*syscall.Getpagesize())
	// Several per-CPU FUSE queues may share a ring to batch small replies.
	// Larger replies can need more issuing threads for parallel kernel copies.
	rings, err := uringRingCount(ms.opts.IoUringRings, cpus)
	if err != nil {
		return err
	}
	for id := 0; id < rings; id++ {
		count := (cpus-1-id)/rings + 1
		q, e := newUringQueue(ms, uint16(id), depth*count, payload)
		if e != nil {
			return fmt.Errorf("FUSE io_uring ring %d: %w", id, e)
		}
		for i := range q.entries {
			q.entries[i].qid = uint16(id + (i/depth)*rings)
		}
		ms.uringQueues = append(ms.uringQueues, q)
	}
	return nil
}

func (ms *Server) startUring() error {
	if !ms.opts.EnableIoUring {
		return nil
	}
	ready := make(chan error, len(ms.uringQueues))
	for _, q := range ms.uringQueues {
		ms.loops.Add(1)
		go func() {
			defer ms.loops.Done()
			// Completions use task_work; keep the issuing OS thread alive until all
			// commands have been synchronously cancelled. Filesystem handlers run
			// on ordinary goroutines because they may wait for backend IO.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			err := q.run(ready)
			ms.stopUring()
			// A lost queue cannot silently fall back: its pending requests would hang.
			if err != nil && err != syscall.ENODEV && err != syscall.ENOTCONN && err != syscall.ECANCELED {
				ms.opts.Logger.Printf("FUSE io_uring queue %d failed: %v; aborting mount", q.qid, err)
				if e := unix.Unmount(ms.mountPoint, unix.MNT_FORCE|unix.MNT_DETACH); e != nil {
					ms.opts.Logger.Printf("abort FUSE io_uring mount: %v", e)
				}
			}
			q.workers.Wait()
			q.cancel()
		}()
	}
	for range ms.uringQueues {
		if err := <-ready; err != nil {
			return err
		}
	}
	ms.opts.Logger.Print("FUSE io_uring is experimental: blocking file locks and fd handover are unsupported")
	ms.opts.Logger.Printf("FUSE io_uring enabled: %d shared rings", len(ms.uringQueues))
	return nil
}

func (ms *Server) stopUring() {
	if !ms.opts.EnableIoUring {
		return
	}
	ms.uringStopOnce.Do(func() {
		ms.reqMu.Lock()
		for _, r := range ms.reqInflight {
			if !r.interrupted {
				r.interrupted = true
				close(r.cancel)
			}
		}
		ms.reqMu.Unlock()
		for _, q := range ms.uringQueues {
			close(q.stop)
			q.wake()
		}
	})
}

func (ms *Server) closeUring() {
	var completed uint64
	for _, q := range ms.uringQueues {
		completed += q.completed.Load()
		q.close()
	}
	if completed > 0 {
		ms.opts.Logger.Printf("FUSE io_uring completed requests: %d", completed)
	}
	ms.uringQueues = nil
}
