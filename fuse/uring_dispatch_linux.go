// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const uringWakeToken = ^uint64(0)

type uringEntry struct {
	qid     uint16
	header  *fuseUringReqHeader
	payload []byte
	iov     []syscall.Iovec
	busy    bool // only accessed by the ring goroutine
}

type uringQueue struct {
	ms          *Server
	qid         uint16
	ring        *uringRing
	entries     []uringEntry
	memory      []byte
	started     bool
	eventFD     int
	replies     chan int
	stop        chan struct{}
	workers     sync.WaitGroup
	cancelled   bool
	completed   atomic.Uint64
	wakePending atomic.Bool
}

func newUringQueue(ms *Server, qid uint16, depth, payload int) (*uringQueue, error) {
	// One extra SQE for the eventfd poll. The kernel rounds up to a power of two.
	r, err := newUringRing(uint32(depth+1), _IORING_SETUP_SQE128)
	if err != nil {
		return nil, err
	}
	q := &uringQueue{ms: ms, qid: qid, ring: r, eventFD: -1, entries: make([]uringEntry, depth), replies: make(chan int, depth), stop: make(chan struct{})}
	q.eventFD, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		q.close()
		return nil, err
	}
	// Keep each payload page aligned: a 4 KiB read should pin/copy one page,
	// not two pages because the request header preceded it in the mapping.
	pageSize := syscall.Getpagesize()
	payloadOffset := (int(unsafe.Sizeof(fuseUringReqHeader{})) + 2*int(unsafe.Sizeof(syscall.Iovec{})) + pageSize - 1) &^ (pageSize - 1)
	stride := payloadOffset + (payload+pageSize-1)&^(pageSize-1)
	q.memory, err = unix.Mmap(-1, 0, stride*depth, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		q.close()
		return nil, err
	}
	for i := range q.entries {
		e := &q.entries[i]
		e.qid = qid
		b := q.memory[i*stride : (i+1)*stride]
		e.header = (*fuseUringReqHeader)(unsafe.Pointer(&b[0]))
		off := int(unsafe.Sizeof(*e.header))
		e.iov = unsafe.Slice((*syscall.Iovec)(unsafe.Pointer(&b[off])), 2)
		e.payload = b[payloadOffset : payloadOffset+payload]
		e.iov[0].Base = (*byte)(unsafe.Pointer(e.header))
		e.iov[0].SetLen(int(unsafe.Sizeof(*e.header)))
		e.iov[1].Base = &e.payload[0]
		e.iov[1].SetLen(payload)
	}
	return q, nil
}

func (q *uringQueue) wake() {
	value := uint64(1)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&value)), 8)
	_ = handleEINTR(func() error { _, err := unix.Write(q.eventFD, b); return err })
}

func (q *uringQueue) pollWake() {
	s := q.ring.nextSqe()
	s.Opcode = 6 // IORING_OP_POLL_ADD
	s.Fd = int32(q.eventFD)
	s.OpFlags = unix.POLLIN
	s.UserData = uringWakeToken
	q.ring.commitSqe()
}

func (q *uringQueue) submit(i int, op uint32) {
	e := &q.entries[i]
	s := q.ring.nextSqe()
	s.Opcode = _IORING_OP_URING_CMD
	s.Fd = int32(q.ms.mountFd)
	s.Off = uint64(op)
	s.Addr = uint64(uintptr(unsafe.Pointer(&e.iov[0])))
	s.Len = 2
	s.UserData = uint64(i)
	*(*fuseUringCmdReq)(unsafe.Pointer(&s.CmdPayload()[0])) = fuseUringCmdReq{CommitID: e.header.RingEntInOut.CommitID, Qid: e.qid}
	q.ring.commitSqe()
}

// enter submits all queued SQEs, including after partial submission or EINTR.
// SQ head is the authoritative count; reusing a stale toSubmit loses replies.
func (q *uringQueue) enter(wait bool) error {
	for {
		pending := atomic.LoadUint32(q.ring.sqTail) - atomic.LoadUint32(q.ring.sqHead)
		var minimum, flags uint32
		if wait {
			minimum, flags = 1, _IORING_ENTER_GETEVENTS
		}
		_, err := ioUringEnter(q.ring.fd, pending, minimum, flags)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if atomic.LoadUint32(q.ring.sqTail) == atomic.LoadUint32(q.ring.sqHead) {
			return nil
		}
	}
}

func (q *uringQueue) run(ready chan<- error) error {
	q.started = true
	for i := range q.entries {
		q.submit(i, _FUSE_IO_URING_CMD_REGISTER)
	}
	q.pollWake()
	err := q.enter(false)
	// Registration failures are delivered in CQEs, not the enter return value.
	if err == nil {
		if c := q.ring.peekCqe(); c != nil && c.Res < 0 {
			err = syscall.Errno(-c.Res)
		}
	}
	ready <- err
	if err != nil {
		return err
	}
	for {
		select {
		case <-q.stop:
			return nil
		default:
		}
		if err := q.enter(true); err != nil {
			return err
		}
		for c := q.ring.peekCqe(); c != nil; c = q.ring.peekCqe() {
			token, res := c.UserData, c.Res
			q.ring.advanceCq()
			if res < 0 {
				return syscall.Errno(-res)
			}
			if token == uringWakeToken {
				var b [8]byte
				_ = handleEINTR(func() error { _, err := unix.Read(q.eventFD, b[:]); return err })
				q.pollWake()
				continue
			}
			if token >= uint64(len(q.entries)) || q.entries[token].busy {
				return fmt.Errorf("invalid FUSE CQE token %d", token)
			}
			q.completed.Add(1)
			i := int(token)
			q.entries[i].busy = true
			q.workers.Add(1)
			go func() {
				defer q.workers.Done()
				q.handle(i)
				// Capacity equals the maximum number of busy entries. Shutdown never
				// waits on a reply consumer that has already exited.
				q.replies <- i
				if q.wakePending.CompareAndSwap(false, true) {
					q.wake()
				}
			}()
		}
		q.wakePending.Store(false)
	drain:
		for {
			select {
			case i := <-q.replies:
				q.entries[i].busy = false
				q.submit(i, _FUSE_IO_URING_CMD_COMMIT_AND_FETCH)
			default:
				break drain
			}
		}
	}
}

func (q *uringQueue) handle(i int) {
	e := &q.entries[i]
	r := q.ms.reqPool.Get().(*request)
	r.startTime = time.Now()
	// Copy the input out of registered memory: cancelled handlers may retain
	// it, whereas the kernel will reuse the entry after COMMIT_AND_FETCH.
	h := (*InHeader)(unsafe.Pointer(&e.header.InOut[0]))
	// Blocking locks can fill every entry on a CPU, preventing the unlock
	// request from ever being fetched. Until the transport can replenish
	// entries independently, reject them explicitly; nonblocking SETLK is safe.
	if h.Opcode == _OP_SETLKW {
		q.errorReply(e, h.Unique, Status(syscall.EOPNOTSUPP))
		r.clear()
		q.ms.reqPool.Put(r)
		return
	}
	handler := getHandler(h.Opcode)
	inSize := int(unsafe.Sizeof(InHeader{}))
	if handler != nil {
		inSize = max(inSize, int(handler.InputSize))
	}
	payload := int(e.header.RingEntInOut.PayloadSz)
	if inSize > int(unsafe.Sizeof(InHeader{}))+len(e.header.OpIn) || payload > len(e.payload) || uint64(h.Length) != uint64(inSize)+uint64(payload) {
		q.errorReply(e, h.Unique, EIO)
		q.ms.reqPool.Put(r)
		return
	}
	var buf []byte
	if inSize+payload <= len(r.smallInputBuf) {
		buf = r.smallInputBuf[:inSize+payload]
	} else {
		buf = make([]byte, inSize+payload)
	}
	copy(buf, e.header.InOut[:unsafe.Sizeof(InHeader{})])
	copy(buf[unsafe.Sizeof(InHeader{}):inSize], e.header.OpIn[:])
	copy(buf[inSize:], e.payload[:payload])
	r.inputBuf = buf
	r.parseHeader()
	r.uringReply = func(r *request) Status { return q.reply(e, r) }
	r.uringOutput = e.payload
	q.ms.reqMu.Lock()
	select {
	case <-q.stop:
		q.ms.reqMu.Unlock()
		r.clear()
		q.ms.reqPool.Put(r)
		return
	default:
	}
	r.inflightIndex = len(q.ms.reqInflight)
	q.ms.reqInflight = append(q.ms.reqInflight, r)
	q.ms.reqMu.Unlock()
	q.ms.handleRequest(r)
}

func (q *uringQueue) errorReply(e *uringEntry, unique uint64, status Status) {
	*(*OutHeader)(unsafe.Pointer(&e.header.InOut[0])) = OutHeader{Length: uint32(sizeOfOutHeader), Unique: unique, Status: -int32(status)}
	e.header.RingEntInOut.PayloadSz = 0
}

func (q *uringQueue) reply(e *uringEntry, r *request) Status {
	if r.readResult != nil {
		defer r.readResult.Done()
	}
	if r.status.Ok() && r.fdData != nil {
		if r.fdData.Size() > len(e.payload) {
			r.status = EIO
		} else {
			r.flatData, r.status = r.fdData.Bytes(e.payload)
			r.fdData = nil
		}
	}
	if !r.status.Ok() {
		r.flatData = nil
		r.fdData = nil
		r.slices = nil
	}
	hdr := r.serializeHeader(r.flatDataSize())
	size := len(hdr) - int(sizeOfOutHeader) + r.flatDataSize()
	if size > len(e.payload) {
		q.errorReply(e, r.inHeader.Unique, EIO)
		return OK
	}
	n := copy(e.payload, hdr[sizeOfOutHeader:])
	if r.slices != nil {
		for _, b := range r.slices {
			n += copy(e.payload[n:], b)
		}
	} else {
		n += copy(e.payload[n:], r.flatData)
	}
	copy(e.header.InOut[:], hdr[:sizeOfOutHeader])
	e.header.RingEntInOut.PayloadSz = uint32(n)
	return OK
}

// SYNC_CANCEL waits until the kernel no longer references the registered
// buffers. Closing the ring alone can schedule asynchronous cancellation.
func (q *uringQueue) cancel() {
	if q.cancelled {
		return
	}
	type cancelReg struct {
		Addr  uint64
		Fd    int32
		Flags uint32
		Sec   int64
		Nsec  int64
		Pad   [4]uint64
	}
	arg := cancelReg{Fd: -1, Flags: 1 | 4, Sec: -1} // ALL | ANY, infinite timeout
	_, err := ioUringRegister(q.ring.fd, 24, unsafe.Pointer(&arg), 1)
	if err != nil && err != syscall.ENOENT {
		// Retain the mmap on failure: the kernel may still reference it.
		q.ms.opts.Logger.Printf("FUSE io_uring cancellation failed: %v", err)
		return
	}
	q.cancelled = true
}

func (q *uringQueue) close() {
	if q.ring != nil {
		q.ring.Close()
		q.ring = nil
	}
	if q.eventFD >= 0 {
		unix.Close(q.eventFD)
		q.eventFD = -1
	}
	// No commands exist before run; afterwards cancel must have succeeded.
	if q.memory != nil && (q.cancelled || !q.started) {
		unix.Munmap(q.memory)
		q.memory = nil
	}
}
