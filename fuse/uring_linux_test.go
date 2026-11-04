package fuse

import (
	"bytes"
	"log"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type uringTestFS struct {
	RawFileSystem
	data      []byte
	status    Status
	entered   chan struct{}
	cancelled chan struct{}
}

func (f *uringTestFS) Read(cancel <-chan struct{}, in *ReadIn, buf []byte) (ReadResult, Status) {
	if f.entered != nil {
		close(f.entered)
		<-cancel
		close(f.cancelled)
		return nil, EINTR
	}
	if f.status != OK {
		return nil, f.status
	}
	if in.Offset >= uint64(len(f.data)) {
		return ReadResultData(nil), OK
	}
	return ReadResultData(f.data[in.Offset:min(uint64(len(f.data)), in.Offset+uint64(in.Size))]), OK
}
func testUringServer(f RawFileSystem) *Server {
	ms := &Server{fileSystem: f, opts: &MountOptions{EnableIoUring: true, NoAllocForRead: true, Logger: log.Default(), MaxWrite: 8192}, kernelSettings: InitIn{Minor: 42}}
	ms.reqPool.New = func() interface{} { return &request{cancel: make(chan struct{})} }
	return ms
}
func testUringRead(ms *Server, offset uint64, size uint32) *uringQueue {
	e := uringEntry{header: new(fuseUringReqHeader), payload: make([]byte, 8192)}
	in := ReadIn{InHeader: InHeader{Opcode: _OP_READ, Unique: 123, NodeId: 2, Length: uint32(unsafe.Sizeof(ReadIn{}))}, Offset: offset, Size: size}
	b := unsafe.Slice((*byte)(unsafe.Pointer(&in)), int(unsafe.Sizeof(in)))
	copy(e.header.InOut[:], b[:unsafe.Sizeof(InHeader{})])
	copy(e.header.OpIn[:], b[unsafe.Sizeof(InHeader{}):])
	return &uringQueue{ms: ms, entries: []uringEntry{e}, stop: make(chan struct{}), eventFD: -1}
}
func TestUringRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		off    uint64
		size   uint32
		status Status
		want   string
	}{
		{"data", 0, 3, OK, "abc"}, {"short", 4, 8, OK, "ef"}, {"eof", 6, 8, OK, ""}, {"zero", 0, 0, OK, ""}, {"error", 0, 8, EIO, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := testUringServer(&uringTestFS{RawFileSystem: NewDefaultRawFileSystem(), data: []byte("abcdef"), status: tc.status})
			q := testUringRead(ms, tc.off, tc.size)
			q.handle(0)
			e := &q.entries[0]
			h := (*OutHeader)(unsafe.Pointer(&e.header.InOut[0]))
			if h.Unique != 123 || h.Status != -int32(tc.status) || h.Length != uint32(16+len(tc.want)) || string(e.payload[:e.header.RingEntInOut.PayloadSz]) != tc.want {
				t.Fatalf("reply %+v payload %q", h, e.payload[:e.header.RingEntInOut.PayloadSz])
			}
			if len(ms.reqInflight) != 0 {
				t.Fatal("request leaked")
			}
		})
	}
}
func TestUringCancellation(t *testing.T) {
	f := &uringTestFS{RawFileSystem: NewDefaultRawFileSystem(), entered: make(chan struct{}), cancelled: make(chan struct{})}
	ms := testUringServer(f)
	q := testUringRead(ms, 0, 1)
	ms.uringQueues = []*uringQueue{q}
	done := make(chan struct{})
	go func() { q.handle(0); close(done) }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("handler not dispatched")
	}
	ms.stopUring()
	ms.stopUring()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock handler")
	}
	if (*OutHeader)(unsafe.Pointer(&q.entries[0].header.InOut[0])).Status != -int32(EINTR) {
		t.Fatal("missing EINTR")
	}
}
func TestUringMalformedInput(t *testing.T) {
	ms := testUringServer(NewDefaultRawFileSystem())
	q := testUringRead(ms, 0, 1)
	q.entries[0].header.RingEntInOut.PayloadSz = 9000
	q.handle(0)
	if (*OutHeader)(unsafe.Pointer(&q.entries[0].header.InOut[0])).Status != -int32(EIO) {
		t.Fatal("malformed payload accepted")
	}
}
func TestUringPossibleCPUs(t *testing.T) {
	for _, s := range []string{"", "1-3", "0-3,5", "0-3,3-4", "0--1", "0-65536", "0-1,x"} {
		if _, err := parsePossibleCPUs(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	for s, want := range map[string]int{"0": 1, "0-43\n": 44, "0-3,4-7": 8} {
		got, err := parsePossibleCPUs(s)
		if err != nil || got != want {
			t.Fatalf("%q: %d %v", s, got, err)
		}
	}
}
func TestUringInit(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, offered := range []bool{false, true} {
			ms := testUringServer(NewDefaultRawFileSystem())
			ms.opts.EnableIoUring = enabled
			in := InitIn{InHeader: InHeader{Opcode: _OP_INIT}, Major: 7, Minor: 42, Flags: 1 << 30}
			if offered {
				in.Flags2 = 1 << 9
			}
			r := &request{inData: unsafe.Pointer(&in)}
			doInit(ms, r)
			if enabled && !offered {
				if r.status != Status(syscall.EOPNOTSUPP) {
					t.Fatal("unsupported capability accepted")
				}
				continue
			}
			out := (*InitOut)(r.outData())
			if (out.Flags2&(1<<9) != 0) != enabled || (enabled && (out.Flags&(1<<30) == 0 || out.Minor != 42)) {
				t.Fatalf("bad negotiation %+v", out)
			}
		}
	}
}
func TestUringReply(t *testing.T) {
	ms := testUringServer(NewDefaultRawFileSystem())
	q := testUringRead(ms, 0, 1)
	e := &q.entries[0]
	r := &request{inHeader: &InHeader{Opcode: _OP_READ, Unique: 7}, slices: [][]byte{[]byte("ab"), []byte("cd")}}
	q.reply(e, r)
	if !bytes.Equal(e.payload[:4], []byte("abcd")) || e.header.RingEntInOut.PayloadSz != 4 {
		t.Fatal("slice reply corrupted")
	}
	f, err := os.CreateTemp(t.TempDir(), "read")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("abcdef")
	r.slices = nil
	r.fdData = ReadResultFd(f.Fd(), 4, 8).(*readResultFd)
	r.readResult = r.fdData
	q.reply(e, r)
	if string(e.payload[:2]) != "ef" || e.header.RingEntInOut.PayloadSz != 2 {
		t.Fatal("fd short read corrupted")
	}
	r.fdData = nil
	r.readResult = nil
	r.flatData = make([]byte, 9000)
	q.reply(e, r)
	if (*OutHeader)(unsafe.Pointer(&e.header.InOut[0])).Status != -int32(EIO) {
		t.Fatal("oversized response accepted")
	}
}
func TestUringRefusesHandover(t *testing.T) {
	if testUringServer(NewDefaultRawFileSystem()).Shutdown() {
		t.Fatal("allowed unsafe handover")
	}
}

func TestUringPrepareRejectsInvalidOptions(t *testing.T) {
	for _, depth := range []int{-1, 3, 257} {
		ms := testUringServer(NewDefaultRawFileSystem())
		ms.opts.IoUringQueueDepth = depth
		if err := ms.prepareUring(); err == nil {
			t.Fatalf("accepted depth %d", depth)
		}
		if len(ms.uringQueues) != 0 {
			t.Fatal("allocated invalid queue")
		}
	}
	ms := testUringServer(NewDefaultRawFileSystem())
	ms.mountPoint = "/dev/fd/123"
	if err := ms.prepareUring(); err == nil {
		t.Fatal("accepted inherited fd")
	}
}

type uringDoneResult struct{ done int }

func (*uringDoneResult) Size() int                     { return 0 }
func (*uringDoneResult) Bytes([]byte) ([]byte, Status) { return nil, OK }
func (r *uringDoneResult) Done()                       { r.done++ }

func TestUringReplyReleasesResult(t *testing.T) {
	for _, status := range []Status{OK, EIO} {
		ms := testUringServer(NewDefaultRawFileSystem())
		q := testUringRead(ms, 0, 0)
		result := &uringDoneResult{}
		r := &request{inHeader: &InHeader{Opcode: _OP_READ, Unique: 7}, readResult: result, status: status}
		q.reply(&q.entries[0], r)
		if result.done != 1 {
			t.Fatalf("Done called %d times", result.done)
		}
	}
}

func TestUringStructuredAndFlatReply(t *testing.T) {
	ms := testUringServer(NewDefaultRawFileSystem())
	q := testUringRead(ms, 0, 0)
	e := &q.entries[0]
	r := &request{inHeader: &InHeader{Opcode: _OP_IOCTL, Unique: 7}, handler: getHandler(_OP_IOCTL), flatData: []byte("data")}
	out := (*IoctlOut)(r.outData())
	out.Result = 17
	q.reply(e, r)
	if e.header.RingEntInOut.PayloadSz != uint32(unsafe.Sizeof(IoctlOut{}))+4 {
		t.Fatal("missing reply structure or data")
	}
	if (*IoctlOut)(unsafe.Pointer(&e.payload[0])).Result != 17 || string(e.payload[unsafe.Sizeof(IoctlOut{}):e.header.RingEntInOut.PayloadSz]) != "data" {
		t.Fatal("overlapping reply structure and data")
	}
}

func TestUringRejectsBlockingLock(t *testing.T) {
	ms := testUringServer(NewDefaultRawFileSystem())
	q := testUringRead(ms, 0, 1)
	h := (*InHeader)(unsafe.Pointer(&q.entries[0].header.InOut[0]))
	h.Opcode = _OP_SETLKW
	q.handle(0)
	out := (*OutHeader)(unsafe.Pointer(&q.entries[0].header.InOut[0]))
	if out.Status != -int32(syscall.EOPNOTSUPP) || out.Unique != 123 {
		t.Fatalf("blocking lock response: %+v", out)
	}
}

func TestUringRingCount(t *testing.T) {
	for _, tc := range []struct{ requested, cpus, want int }{
		{0, 44, 4}, {0, 2, 2}, {1, 44, 1}, {16, 44, 16}, {44, 44, 44}, {-1, 44, 0}, {45, 44, 0},
	} {
		got, err := uringRingCount(tc.requested, tc.cpus)
		if (err != nil) != (tc.want == 0) || got != tc.want {
			t.Fatalf("rings(%d,%d) = %d, %v", tc.requested, tc.cpus, got, err)
		}
	}
}
