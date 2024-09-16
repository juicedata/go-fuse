// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	// Linux v4.20+ caps requests at 1 MiB. Older kernels at 128 kiB.
	MAX_KERNEL_WRITE = 1024 * 1024

	// defaultMaxWrite is the default value for MountOptions.MaxWrite
	defaultMaxWrite = 128 * 1024 // 128 kiB

	minMaxReaders = 1
	maxMaxReaders = 4
)

// Server contains the logic for reading from the FUSE device and
// translating it to RawFileSystem interface calls.
type Server struct {
	// Empty if unmounted.
	mountPoint string
	fileSystem RawFileSystem

	// writeMu serializes close and notify writes
	writeMu sync.RWMutex

	// I/O with kernel and daemon.
	mountFd int

	latencies LatencyMap

	opts *MountOptions

	// maxReaders is the maximum number of goroutines reading requests
	maxReaders int
	maxUnique  uint64

	// Pools for []byte
	buffers bufferPool

	// Pool for request structs.
	reqPool sync.Pool

	// Pool for raw requests data
	readPool       sync.Pool
	reqMu          sync.Mutex
	reqReaders     int
	reqInflight    []*request
	recentUnique   []uint64
	kernelSettings InitIn

	// in-flight notify-retrieve queries
	retrieveMu   sync.Mutex
	retrieveNext uint64
	retrieveTab  map[uint64]*retrieveCacheRequest // notifyUnique -> retrieve request

	singleReader bool
	canSplice    bool
	loops        sync.WaitGroup
	writes       int64
	shutdown     bool

	ready chan error

	// for implementing single threaded processing.
	requestProcessingMu sync.Mutex
}

// SetDebug is deprecated. Use MountOptions.Debug instead.
func (ms *Server) SetDebug(dbg bool) {
	// This will typically trigger the race detector.
	ms.opts.Debug = dbg
}

// KernelSettings returns the Init message from the kernel, so
// filesystems can adapt to availability of features of the kernel
// driver. The message should not be altered.
func (ms *Server) KernelSettings() *InitIn {
	ms.reqMu.Lock()
	s := ms.kernelSettings
	ms.reqMu.Unlock()

	return &s
}

const _MAX_NAME_LEN = 20

// This type may be provided for recording latencies of each FUSE
// operation.
type LatencyMap interface {
	Add(name string, dt time.Duration)
}

// RecordLatencies switches on collection of timing for each request
// coming from the kernel.P assing a nil argument switches off the
func (ms *Server) RecordLatencies(l LatencyMap) {
	ms.latencies = l
}

// Unmount calls fusermount -u on the mount. This has the effect of
// shutting down the filesystem. After the Server is unmounted, it
// should be discarded.
func (ms *Server) Unmount() (err error) {
	if ms.mountPoint == "" {
		return nil
	}
	if parseFuseFd(ms.mountPoint) >= 0 {
		return fmt.Errorf("Cannot unmount magic mountpoint %q. Please use `fusermount -u REALMOUNTPOINT` instead.", ms.mountPoint)
	}
	delay := time.Duration(0)
	for try := 0; try < 5; try++ {
		err = unmount(ms.mountPoint, ms.opts)
		if err == nil {
			break
		}

		// Sleep for a bit. This is not pretty, but there is
		// no way we can be certain that the kernel thinks all
		// open files have already been closed.
		delay = 2*delay + 5*time.Millisecond
		time.Sleep(delay)
	}
	if err != nil {
		return
	}
	// Wait for event loops to exit.
	ms.loops.Wait()
	ms.mountPoint = ""
	return err
}

// NewServer creates a FUSE server and attaches ("mounts") it to the
// `mountPoint` directory.
//
// See the "Mount styles" section in the package documentation if you want to
// know about the inner workings of the mount process. Usually you do not.
func NewServer(fs RawFileSystem, mountPoint string, opts *MountOptions) (*Server, error) {
	if opts == nil {
		opts = &MountOptions{
			MaxBackground: _DEFAULT_BACKGROUND_TASKS,
		}
	}
	o := *opts

	if o.MaxWrite < 0 {
		o.MaxWrite = 0
	}
	if o.MaxWrite == 0 {
		o.MaxWrite = defaultMaxWrite
	}
	if o.MaxWrite > MAX_KERNEL_WRITE {
		o.MaxWrite = MAX_KERNEL_WRITE
	}
	if o.Name == "" {
		name := fs.String()
		l := len(name)
		if l > _MAX_NAME_LEN {
			l = _MAX_NAME_LEN
		}
		o.Name = strings.Replace(name[:l], ",", ";", -1)
	}

	for _, s := range o.optionsStrings() {
		if strings.Contains(s, ",") {
			return nil, fmt.Errorf("found ',' in option string %q", s)
		}
	}

	maxReaders := runtime.GOMAXPROCS(0)
	if maxReaders < minMaxReaders {
		maxReaders = minMaxReaders
	} else if maxReaders > maxMaxReaders {
		maxReaders = maxMaxReaders
	}

	ms := &Server{
		fileSystem:  fs,
		opts:        &o,
		maxReaders:  maxReaders,
		retrieveTab: make(map[uint64]*retrieveCacheRequest),
		// OSX has races when multiple routines read from the
		// FUSE device: on unmount, sometime some reads do not
		// error-out, meaning that unmount will hang.
		singleReader: runtime.GOOS == "darwin",
		ready:        make(chan error, 1),
	}
	ms.reqPool.New = func() interface{} {
		return &request{
			cancel: make(chan struct{}),
		}
	}
	ms.readPool.New = func() interface{} {
		buf := make([]byte, o.MaxWrite+int(maxInputSize)+logicalBlockSize)
		buf = alignSlice(buf, unsafe.Sizeof(WriteIn{}), logicalBlockSize, uintptr(o.MaxWrite)+maxInputSize)
		return buf
	}
	mountPoint = filepath.Clean(mountPoint)
	if !filepath.IsAbs(mountPoint) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		mountPoint = filepath.Clean(filepath.Join(cwd, mountPoint))
	}
	ms.mountPoint = mountPoint

	err := ms.mount(&o)
	if err != nil {
		log.Printf("mount: %s", err)
		return nil, err
	}
	// This prepares for Serve being called somewhere, either
	// synchronously or asynchronously.
	ms.loops.Add(1)
	return ms, nil
}

func (ms *Server) mount(opt *MountOptions) error {
	path := os.Getenv("_FUSE_FD_COMM")
	if path != "" {
		c, err := net.Dial("unix", path)
		if err != nil {
			return fmt.Errorf("dial %s: %s", path, err)
		}
		defer c.Close()

		msg, fds, err := getFd(c.(*net.UnixConn), 2)
		if err != nil {
			return fmt.Errorf("get fd: %s", err)
		}

		if len(fds) > 0 {
			// the first sone is not needed
			syscall.Close(fds[0])
		}
		if len(fds) == 2 {
			if len(msg) < int(unsafe.Sizeof(InitIn{})) {
				return fmt.Errorf("short setting: %d < %d", len(msg), unsafe.Sizeof(InitIn{}))
			}
			close(ms.ready)
			ms.kernelSettings = *(*InitIn)(unsafe.Pointer(&msg[0]))
			if ms.kernelSettings.Minor >= 13 {
				ms.setSplice()
			}
			// log.Println("setting %+v", ms.kernelSettings)
			syscall.CloseOnExec(fds[1])
			ms.mountFd = fds[1]
			ms.fileSystem.Init(ms)
			ms.recentUnique = make([]uint64, 0)
			go ms.sendFd(path)
			go ms.checkLostRequests()
			return nil
		}
	}

	fd, err := mount(ms.mountPoint, opt, ms.ready)
	if err != nil {
		return err
	}
	ms.mountFd = fd

	if code := ms.handleInit(); !code.Ok() {
		syscall.Close(fd)
		// TODO - unmount as well?
		return fmt.Errorf("init: %s", code)
	}

	if path != "" {
		go ms.sendFd(path)
	}
	return nil
}

func (ms *Server) sendFd(path string) {
	buf := make([]byte, unsafe.Sizeof(InitIn{}))
	*(*InitIn)(unsafe.Pointer(&buf[0])) = ms.kernelSettings

	for {
		err := sendFuseFd(path, buf, ms.mountFd)
		if err == nil {
			break
		}
		log.Println("send FUSE", err)
		time.Sleep(time.Millisecond * 100)
	}
}

func (o *MountOptions) optionsStrings() []string {
	var r []string
	r = append(r, o.Options...)

	if o.AllowOther {
		r = append(r, "allow_other")
	}
	if o.FsName != "" {
		r = append(r, "fsname="+o.FsName)
	}
	if o.Name != "" {
		r = append(r, "subtype="+o.Name)
	}
	r = append(r, fmt.Sprintf("max_read=%d", o.MaxWrite))

	return r
}

// DebugData returns internal status information for debugging
// purposes.
func (ms *Server) DebugData() string {
	var r int
	ms.reqMu.Lock()
	r = ms.reqReaders
	ms.reqMu.Unlock()

	return fmt.Sprintf("readers: %d", r)
}

// handleEINTR retries the given function until it doesn't return syscall.EINTR.
// This is similar to the HANDLE_EINTR() macro from Chromium ( see
// https://code.google.com/p/chromium/codesearch#chromium/src/base/posix/eintr_wrapper.h
// ) and the TEMP_FAILURE_RETRY() from glibc (see
// https://www.gnu.org/software/libc/manual/html_node/Interrupted-Primitives.html
// ).
//
// Don't use handleEINTR() with syscall.Close(); see
// https://code.google.com/p/chromium/issues/detail?id=269623 .
func handleEINTR(fn func() error) (err error) {
	for {
		err = fn()
		if err != syscall.EINTR {
			break
		}
	}
	return
}

// Returns a new request, or error. In case exitIdle is given, returns
// nil, OK if we have too many readers already.
func (ms *Server) readRequest(exitIdle bool) (req *request, code Status) {
	ms.reqMu.Lock()
	if exitIdle {
		if ms.shutdown || ms.reqReaders >= ms.maxReaders {
			ms.reqMu.Unlock()
			return nil, OK
		}
	} else {
		// main thread, don't exit for restart
		for ms.shutdown {
			ms.reqMu.Unlock()
			time.Sleep(time.Millisecond)
			ms.reqMu.Lock()
		}
	}
	ms.reqReaders++
	ms.reqMu.Unlock()

	dest := ms.readPool.Get().([]byte)

	var n int
	err := handleEINTR(func() error {
		var err error
		n, err = syscall.Read(ms.mountFd, dest)
		return err
	})
	if err != nil {
		code = ToStatus(err)
		ms.readPool.Put(dest)
		ms.reqMu.Lock()
		ms.reqReaders--
		ms.reqMu.Unlock()
		return nil, code
	}

	req = ms.reqPool.Get().(*request)
	req.startTime = time.Now()
	gobbled := req.setInput(dest[:n])
	if !gobbled {
		ms.readPool.Put(dest)
	}

	ms.reqMu.Lock()
	defer ms.reqMu.Unlock()
	ms.reqReaders--
	// Must parse request.Unique under lock
	if status := req.parseHeader(); !status.Ok() {
		return nil, status
	}
	if ms.recentUnique != nil {
		ms.recentUnique = append(ms.recentUnique, req.inHeader.Unique)
	}
	if req.inHeader.Unique > ms.maxUnique {
		ms.maxUnique = req.inHeader.Unique
	}
	req.inflightIndex = len(ms.reqInflight)
	ms.reqInflight = append(ms.reqInflight, req)

	if !ms.singleReader && ms.reqReaders < 2 && ms.reqReaders < ms.maxReaders && !ms.shutdown {
		ms.loops.Add(1)
		go ms.loop(true)
	}

	return req, OK
}

func (ms *Server) checkLostRequests() {
	go func() {
		// issue a few requests to interrupt lost ones
		for i := 0; i < 30; i++ {
			ms.wakeupReader()
			time.Sleep(time.Millisecond * 100)
		}
	}()
	var recentUnique []uint64
	time.Sleep(time.Second * 3)
	for {
		ms.reqMu.Lock()
		if len(ms.recentUnique) >= 30 {
			recentUnique = ms.recentUnique
			ms.recentUnique = nil
			ms.reqMu.Unlock()
			break
		}
		ms.reqMu.Unlock()
		time.Sleep(time.Second)
	}

	sort.Slice(recentUnique, func(i, j int) bool { return recentUnique[i] < recentUnique[j] })
	var last = recentUnique[0]
	for _, u := range recentUnique[:len(recentUnique)/2] {
		for u > last+1 {
			last++
			// interrupt lost one
			ms.returnInterrupted(last)
		}
	}
	// interrupt historic ones
	last = recentUnique[0] - 1
	var c int
	for last > 0 && c < 3e6 {
		ms.returnInterrupted(last)
		last--
		c++
	}
}

func (ms *Server) returnInterrupted(unique uint64) {
	header := make([]byte, sizeOfOutHeader)
	o := (*OutHeader)(unsafe.Pointer(&header[0]))
	o.Unique = unique
	o.Status = -int32(syscall.EINTR)
	o.Length = uint32(sizeOfOutHeader)
	err := handleEINTR(func() error {
		_, err := syscall.Write(ms.mountFd, header)
		return err
	})
	if err == nil {
		log.Printf("FUSE: interrupt request %d", unique)
	}
}

// returnRequest returns a request to the pool of unused requests.
func (ms *Server) returnRequest(req *request) {
	ms.reqMu.Lock()
	this := req.inflightIndex
	last := len(ms.reqInflight) - 1

	if last != this {
		ms.reqInflight[this] = ms.reqInflight[last]
		ms.reqInflight[this].inflightIndex = this
	}
	ms.reqInflight = ms.reqInflight[:last]
	interrupted := req.interrupted
	ms.reqMu.Unlock()

	ms.recordStats(req)
	if interrupted {
		// Don't reposses data, because someone might still
		// be looking at it
		return
	}

	if req.bufferPoolOutputBuf != nil {
		ms.buffers.FreeBuffer(req.bufferPoolOutputBuf)
		req.bufferPoolOutputBuf = nil
	}

	req.clear()

	if p := req.bufferPoolInputBuf; p != nil {
		req.bufferPoolInputBuf = nil
		ms.readPool.Put(p)
	}

	select {
	case <-req.cancel:
		// canceled
		log.Printf("request is canceled")
	default:
		ms.reqPool.Put(req)
	}
}

func (ms *Server) recordStats(req *request) {
	if ms.latencies != nil {
		dt := time.Now().Sub(req.startTime)
		opname := operationName(req.inHeader.Opcode)
		ms.latencies.Add(opname, dt)
	}
}

// Serve initiates the FUSE loop. Normally, callers should run Serve()
// and wait for it to exit, but tests will want to run this in a
// goroutine.
//
// Each filesystem operation executes in a separate goroutine.
func (ms *Server) Serve() {
	ms.loop(false)
	if ms.opts.Timeout > 0 {
		go ms.checkHangRequests(ms.opts.Timeout)
	}
	ms.loops.Wait()

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

	_ = closeFuseFd()

	ms.writeMu.Lock()
	syscall.Close(ms.mountFd)
	ms.writeMu.Unlock()
}

// Wait waits for the serve loop to exit. This should only be called
// after Serve has been called, or it will hang indefinitely.
func (ms *Server) Wait() {
	ms.loops.Wait()
}

func (ms *Server) wakeupReader() {
	cmd := exec.Command("df", ms.mountPoint)
	_ = cmd.Run()
}

func (ms *Server) checkHangRequests(timeout time.Duration) {
	for {
		time.Sleep(timeout / 100)
		var batch = 100
		for i := 0; ; i += batch {
			now := time.Now()
			ms.reqMu.Lock()
			if ms.shutdown {
				ms.reqMu.Unlock()
				break
			}
			for j := 0; j < batch && i+j < len(ms.reqInflight); j++ {
				req := ms.reqInflight[i+j]
				if req.interrupted {
					unique := req.inHeader.Unique
					ms.reqMu.Unlock()
					ms.returnInterrupted(unique)
					ms.reqMu.Lock()
				} else if used := now.Sub(req.startTime); used > timeout || req.inHeader.Unique+2e6 > ms.maxUnique {
					log.Printf("interrupt request %d after %s: %+v", req.inHeader.Unique, used, req.inHeader)
					req.interrupted = true
					close(req.cancel)
				}
			}
			ms.reqMu.Unlock()
		}
	}
}

func (ms *Server) Shutdown() bool {
	log.Printf("try to restart gracefully")
	start := time.Now()
	ms.reqMu.Lock()
	ms.shutdown = true
	readers := ms.reqReaders
	reqs := len(ms.reqInflight)
	ms.reqMu.Unlock()

	for readers > 0 || reqs > 0 || atomic.LoadInt64(&ms.writes) > 0 {
		if readers > 0 {
			go ms.wakeupReader()
		} else if atomic.LoadInt64(&ms.writes) > 0 {
			// the write could be blocked by FUSE requests, let's process them
			time.Sleep(time.Millisecond * 100)
			// double check
			if n := atomic.LoadInt64(&ms.writes); n > 0 {
				log.Printf("restore process for %d writes", n)
				ms.reqMu.Lock()
				ms.shutdown = false
				ms.reqMu.Unlock()
				time.Sleep(time.Millisecond * 100)
				ms.reqMu.Lock()
				ms.shutdown = true
				ms.reqMu.Unlock()
			}
		}
		if time.Since(start) > time.Second*3 {
			ms.reqMu.Lock()
			log.Printf("interrupt %d inflight requests", len(ms.reqInflight))
			for _, req := range ms.reqInflight {
				if !req.interrupted {
					close(req.cancel)
					req.interrupted = true
				}
			}
			ms.reqMu.Unlock()
		}
		if time.Since(start) > time.Second*10 {
			log.Printf("FUSE session is still busy (%d readers, %d requests, %d writers) after 10 seconds, give up",
				readers, reqs, atomic.LoadInt64(&ms.writes))
			ms.reqMu.Lock()
			ms.shutdown = false
			ms.reqMu.Unlock()
			return false
		}
		time.Sleep(time.Millisecond * 10)
		ms.reqMu.Lock()
		readers = ms.reqReaders
		reqs = len(ms.reqInflight)
		ms.reqMu.Unlock()
	}

	// double check
	ms.reqMu.Lock()
	if len(ms.reqInflight) > 0 {
		log.Printf("there are %d requests in flight, interrupt them", len(ms.reqInflight))
		for _, req := range ms.reqInflight {
			ms.returnInterrupted(req.inHeader.Unique)
		}
	}
	ms.reqMu.Unlock()
	return true
}

func (ms *Server) handleInit() Status {
	// The first request should be INIT; read it synchronously,
	// and don't spawn new readers.
	orig := ms.singleReader
	ms.singleReader = true
	req, errNo := ms.readRequest(false)
	ms.singleReader = orig

	if errNo != OK || req == nil {
		return errNo
	}
	if code := ms.handleRequest(req); !code.Ok() {
		return code
	}

	// INIT is handled. Init the file system, but don't accept
	// incoming requests, so the file system can setup itself.
	ms.fileSystem.Init(ms)
	return OK
}

func (ms *Server) loop(exitIdle bool) {
	defer ms.loops.Done()
exit:
	for {
		req, errNo := ms.readRequest(exitIdle)
		switch errNo {
		case OK:
			if req == nil {
				break exit
			}
		case ENOENT:
			continue
		case ENODEV:
			// unmount
			if ms.opts.Debug {
				log.Printf("received ENODEV (unmount request), thread exiting")
			}
			break exit
		default: // some other error?
			log.Printf("Failed to read from fuse conn: %v", errNo)
			break exit
		}

		if ms.singleReader {
			go ms.handleRequest(req)
		} else {
			ms.handleRequest(req)
		}
	}
}

func (ms *Server) handleRequest(req *request) Status {
	if ms.opts.SingleThreaded {
		ms.requestProcessingMu.Lock()
		defer ms.requestProcessingMu.Unlock()
	}

	req.parse()
	if req.handler == nil {
		req.status = ENOSYS
	}

	if req.status.Ok() && ms.opts.Debug {
		log.Println(req.InputDebug())
	}

	if req.inHeader.NodeId == pollHackInode ||
		req.inHeader.NodeId == FUSE_ROOT_ID && len(req.filenames) > 0 && req.filenames[0] == pollHackName {
		doPollHackLookup(ms, req)
	} else if req.status.Ok() && req.handler.Func == nil {
		log.Printf("Unimplemented opcode %v", operationName(req.inHeader.Opcode))
		req.status = ENOSYS
	} else if req.status.Ok() {
		req.handler.Func(ms, req)
	}

	errNo := ms.write(req)
	if errNo != 0 {
		// Unless debugging is enabled, ignore ENOENT for INTERRUPT responses
		// which indicates that the referred request is no longer known by the
		// kernel. This is a normal if the referred request already has
		// completed.
		if ms.opts.Debug || !(req.inHeader.Opcode == _OP_INTERRUPT && errNo == ENOENT) {
			log.Printf("writer: Write/Writev failed, err: %v. opcode: %v",
				errNo, operationName(req.inHeader.Opcode))
		}

	}
	ms.returnRequest(req)
	return Status(errNo)
}

// alignSlice ensures that the byte at alignedByte is aligned with the
// given logical block size.  The input slice should be at least (size
// + blockSize)
func alignSlice(buf []byte, alignedByte, blockSize, size uintptr) []byte {
	misaligned := uintptr(unsafe.Pointer(&buf[alignedByte])) & (blockSize - 1)
	buf = buf[blockSize-misaligned:]
	return buf[:size]
}

func (ms *Server) allocOut(req *request, size uint32) []byte {
	if cap(req.bufferPoolOutputBuf) >= int(size) {
		req.bufferPoolOutputBuf = req.bufferPoolOutputBuf[:size]
		return req.bufferPoolOutputBuf
	}
	if req.bufferPoolOutputBuf != nil {
		ms.buffers.FreeBuffer(req.bufferPoolOutputBuf)
		req.bufferPoolOutputBuf = nil
	}
	// As this allocated a multiple of the page size, very likely
	// this is aligned to logicalBlockSize too, which is smaller.
	req.bufferPoolOutputBuf = ms.buffers.AllocBuffer(size)
	return req.bufferPoolOutputBuf
}

func (ms *Server) write(req *request) Status {
	// Forget/NotifyReply do not wait for reply from filesystem server.
	switch req.inHeader.Opcode {
	case _OP_FORGET, _OP_BATCH_FORGET, _OP_NOTIFY_REPLY:
		return OK
	case _OP_INTERRUPT:
		if req.status.Ok() {
			return OK
		}
	}

	header := req.serializeHeader(req.flatDataSize())
	if ms.opts.Debug {
		log.Println(req.OutputDebug())
	}

	if header == nil {
		return OK
	}

	atomic.AddInt64(&ms.writes, 1)
	defer func() {
		atomic.AddInt64(&ms.writes, -1)
	}()
	s := ms.systemWrite(req, header)
	return s
}

func (ms *Server) isShutdown() bool {
	ms.reqMu.Lock()
	defer ms.reqMu.Unlock()
	return ms.shutdown
}

// InodeNotify invalidates the information associated with the inode
// (ie. data cache, attributes, etc.)
func (ms *Server) InodeNotify(node uint64, off int64, length int64) Status {
	if !ms.kernelSettings.SupportsNotify(NOTIFY_INVAL_INODE) {
		return ENOSYS
	}
	if ms.isShutdown() {
		return EINTR
	}

	req := request{
		inHeader: &InHeader{
			Opcode: _OP_NOTIFY_INVAL_INODE,
		},
		handler: operationHandlers[_OP_NOTIFY_INVAL_INODE],
		status:  NOTIFY_INVAL_INODE,
	}

	entry := (*NotifyInvalInodeOut)(req.outData())
	entry.Ino = node
	entry.Off = off
	entry.Length = length

	// Protect against concurrent close.
	ms.writeMu.RLock()
	result := ms.write(&req)
	ms.writeMu.RUnlock()

	if ms.opts.Debug {
		log.Println("Response: INODE_NOTIFY", result)
	}
	return result
}

// InodeNotifyStoreCache tells kernel to store data into inode's cache.
//
// This call is similar to InodeNotify, but instead of only invalidating a data
// region, it gives updated data directly to the kernel.
func (ms *Server) InodeNotifyStoreCache(node uint64, offset int64, data []byte) Status {
	if !ms.kernelSettings.SupportsNotify(NOTIFY_STORE_CACHE) {
		return ENOSYS
	}
	if ms.isShutdown() {
		return EINTR
	}

	for len(data) > 0 {
		size := len(data)
		if size > math.MaxInt32 {
			// NotifyStoreOut has only uint32 for size.
			// we check for max(int32), not max(uint32), because on 32-bit
			// platforms int has only 31-bit for positive range.
			size = math.MaxInt32
		}

		st := ms.inodeNotifyStoreCache32(node, offset, data[:size])
		if st != OK {
			return st
		}

		data = data[size:]
		offset += int64(size)
	}

	return OK
}

// inodeNotifyStoreCache32 is internal worker for InodeNotifyStoreCache which
// handles data chunks not larger than 2GB.
func (ms *Server) inodeNotifyStoreCache32(node uint64, offset int64, data []byte) Status {
	if ms.isShutdown() {
		return EINTR
	}
	req := request{
		inHeader: &InHeader{
			Opcode: _OP_NOTIFY_STORE_CACHE,
		},
		handler: operationHandlers[_OP_NOTIFY_STORE_CACHE],
		status:  NOTIFY_STORE_CACHE,
	}

	store := (*NotifyStoreOut)(req.outData())
	store.Nodeid = node
	store.Offset = uint64(offset) // NOTE not int64, as it is e.g. in NotifyInvalInodeOut
	store.Size = uint32(len(data))

	req.flatData = data

	// Protect against concurrent close.
	ms.writeMu.RLock()
	result := ms.write(&req)
	ms.writeMu.RUnlock()

	if ms.opts.Debug {
		log.Printf("Response: INODE_NOTIFY_STORE_CACHE: %v", result)
	}
	return result
}

// InodeRetrieveCache retrieves data from kernel's inode cache.
//
// InodeRetrieveCache asks kernel to return data from its cache for inode at
// [offset:offset+len(dest)) and waits for corresponding reply. If kernel cache
// has fewer consecutive data starting at offset, that fewer amount is returned.
// In particular if inode data at offset is not cached (0, OK) is returned.
//
// The kernel returns ENOENT if it does not currently have entry for this inode
// in its dentry cache.
func (ms *Server) InodeRetrieveCache(node uint64, offset int64, dest []byte) (n int, st Status) {
	// the kernel won't send us in one go more then what we negotiated as MaxWrite.
	// retrieve the data in chunks.
	// TODO spawn some number of readahead retrievers in parallel.
	ntotal := 0
	for {
		chunkSize := len(dest)
		if chunkSize > ms.opts.MaxWrite {
			chunkSize = ms.opts.MaxWrite
		}
		n, st = ms.inodeRetrieveCache1(node, offset, dest[:chunkSize])
		if st != OK || n == 0 {
			break
		}

		ntotal += n
		offset += int64(n)
		dest = dest[n:]
	}

	// if we could retrieve at least something - it is ok.
	// if ntotal=0 - st will be st returned from first inodeRetrieveCache1.
	if ntotal > 0 {
		st = OK
	}
	return ntotal, st
}

// inodeRetrieveCache1 is internal worker for InodeRetrieveCache which
// actually talks to kernel and retrieves chunks not larger than ms.opts.MaxWrite.
func (ms *Server) inodeRetrieveCache1(node uint64, offset int64, dest []byte) (n int, st Status) {
	if !ms.kernelSettings.SupportsNotify(NOTIFY_RETRIEVE_CACHE) {
		return 0, ENOSYS
	}
	if ms.isShutdown() {
		return 0, EINTR
	}

	req := request{
		inHeader: &InHeader{
			Opcode: _OP_NOTIFY_RETRIEVE_CACHE,
		},
		handler: operationHandlers[_OP_NOTIFY_RETRIEVE_CACHE],
		status:  NOTIFY_RETRIEVE_CACHE,
	}

	// retrieve up to 2GB not to overflow uint32 size in NotifyRetrieveOut.
	// see InodeNotifyStoreCache in similar place for why it is only 2GB, not 4GB.
	//
	// ( InodeRetrieveCache calls us with chunks not larger than
	//   ms.opts.MaxWrite, but MaxWrite is int, so let's be extra cautious )
	size := len(dest)
	if size > math.MaxInt32 {
		size = math.MaxInt32
	}
	dest = dest[:size]

	q := (*NotifyRetrieveOut)(req.outData())
	q.Nodeid = node
	q.Offset = uint64(offset) // not int64, as it is e.g. in NotifyInvalInodeOut
	q.Size = uint32(len(dest))

	reading := &retrieveCacheRequest{
		nodeid: q.Nodeid,
		offset: q.Offset,
		dest:   dest,
		ready:  make(chan struct{}),
	}

	ms.retrieveMu.Lock()
	q.NotifyUnique = ms.retrieveNext
	ms.retrieveNext++
	ms.retrieveTab[q.NotifyUnique] = reading
	ms.retrieveMu.Unlock()

	// Protect against concurrent close.
	ms.writeMu.RLock()
	result := ms.write(&req)
	ms.writeMu.RUnlock()

	if ms.opts.Debug {
		log.Printf("Response: NOTIFY_RETRIEVE_CACHE: %v", result)
	}
	if result != OK {
		ms.retrieveMu.Lock()
		r := ms.retrieveTab[q.NotifyUnique]
		if r == reading {
			delete(ms.retrieveTab, q.NotifyUnique)
		} else if r == nil {
			// ok - might be dequeued by umount
		} else {
			// although very unlikely, it is possible that kernel sends
			// unexpected NotifyReply with our notifyUnique, then
			// retrieveNext wraps, makes full cycle, and another
			// retrieve request is made with the same notifyUnique.
			log.Printf("W: INODE_RETRIEVE_CACHE: request with notifyUnique=%d mutated", q.NotifyUnique)
		}
		ms.retrieveMu.Unlock()
		return 0, result
	}

	// NotifyRetrieveOut sent to the kernel successfully. Now the kernel
	// have to return data in a separate write-style NotifyReply request.
	// Wait for the result.
	<-reading.ready
	return reading.n, reading.st
}

// retrieveCacheRequest represents in-flight cache retrieve request.
type retrieveCacheRequest struct {
	nodeid uint64
	offset uint64
	dest   []byte

	// reply status
	n     int
	st    Status
	ready chan struct{}
}

// DeleteNotify notifies the kernel that an entry is removed from a
// directory.  In many cases, this is equivalent to EntryNotify,
// except when the directory is in use, eg. as working directory of
// some process. You should not hold any FUSE filesystem locks, as that
// can lead to deadlock.
func (ms *Server) DeleteNotify(parent uint64, child uint64, name string) Status {
	if ms.kernelSettings.Minor < 18 {
		return ms.EntryNotify(parent, name)
	}
	if ms.isShutdown() {
		return EINTR
	}

	req := request{
		inHeader: &InHeader{
			Opcode: _OP_NOTIFY_DELETE,
		},
		handler: operationHandlers[_OP_NOTIFY_DELETE],
		status:  NOTIFY_DELETE,
	}

	entry := (*NotifyInvalDeleteOut)(req.outData())
	entry.Parent = parent
	entry.Child = child
	entry.NameLen = uint32(len(name))

	// Many versions of FUSE generate stacktraces if the
	// terminating null byte is missing.
	nameBytes := make([]byte, len(name)+1)
	copy(nameBytes, name)
	nameBytes[len(nameBytes)-1] = '\000'
	req.flatData = nameBytes

	// Protect against concurrent close.
	ms.writeMu.RLock()
	result := ms.write(&req)
	ms.writeMu.RUnlock()

	if ms.opts.Debug {
		log.Printf("Response: DELETE_NOTIFY: %v", result)
	}
	return result
}

// EntryNotify should be used if the existence status of an entry
// within a directory changes. You should not hold any FUSE filesystem
// locks, as that can lead to deadlock.
func (ms *Server) EntryNotify(parent uint64, name string) Status {
	if !ms.kernelSettings.SupportsNotify(NOTIFY_INVAL_ENTRY) {
		return ENOSYS
	}
	if ms.isShutdown() {
		return EINTR
	}
	req := request{
		inHeader: &InHeader{
			Opcode: _OP_NOTIFY_INVAL_ENTRY,
		},
		handler: operationHandlers[_OP_NOTIFY_INVAL_ENTRY],
		status:  NOTIFY_INVAL_ENTRY,
	}
	entry := (*NotifyInvalEntryOut)(req.outData())
	entry.Parent = parent
	entry.NameLen = uint32(len(name))

	// Many versions of FUSE generate stacktraces if the
	// terminating null byte is missing.
	nameBytes := make([]byte, len(name)+1)
	copy(nameBytes, name)
	nameBytes[len(nameBytes)-1] = '\000'
	req.flatData = nameBytes

	// Protect against concurrent close.
	ms.writeMu.RLock()
	result := ms.write(&req)
	ms.writeMu.RUnlock()

	if ms.opts.Debug {
		log.Printf("Response: ENTRY_NOTIFY: %v", result)
	}
	return result
}

// SupportsVersion returns true if the kernel supports the given
// protocol version or newer.
func (in *InitIn) SupportsVersion(maj, min uint32) bool {
	return in.Major > maj || (in.Major == maj && in.Minor >= min)
}

// SupportsNotify returns whether a certain notification type is
// supported. Pass any of the NOTIFY_* types as argument.
func (in *InitIn) SupportsNotify(notifyType int) bool {
	switch notifyType {
	case NOTIFY_INVAL_ENTRY:
		return in.SupportsVersion(7, 12)
	case NOTIFY_INVAL_INODE:
		return in.SupportsVersion(7, 12)
	case NOTIFY_STORE_CACHE, NOTIFY_RETRIEVE_CACHE:
		return in.SupportsVersion(7, 15)
	case NOTIFY_DELETE:
		return in.SupportsVersion(7, 18)
	}
	return false
}

// WaitMount waits for the first request to be served. Use this to
// avoid racing between accessing the (empty or not yet mounted)
// mountpoint, and the OS trying to setup the user-space mount.
func (ms *Server) WaitMount() error {
	err := <-ms.ready
	if err != nil {
		return err
	}
	if parseFuseFd(ms.mountPoint) >= 0 {
		// Magic `/dev/fd/N` mountpoint. We don't know the real mountpoint, so
		// we cannot run the poll hack.
		return nil
	}
	return pollHack(ms.mountPoint)
}
