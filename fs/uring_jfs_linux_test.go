package fs

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// This requires an io_uring-capable Linux CI runner. Never silently skip a
// requested run because the kernel is old or enable_uring is disabled.
func TestJFSUringMount(t *testing.T) {
	if os.Getenv("JFS_TEST_FUSE_IO_URING") != "1" {
		t.Skip("set JFS_TEST_FUSE_IO_URING=1 on a capable CI runner")
	}
	for _, depth := range []int{1, 4, 32} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			backing, mp := t.TempDir(), t.TempDir()
			data := make([]byte, (1<<20)+1)
			for i := range data {
				data[i] = byte(i * 37)
			}
			if err := os.WriteFile(filepath.Join(backing, "data"), data, 0600); err != nil {
				t.Fatal(err)
			}
			root, err := NewLoopbackRoot(backing)
			if err != nil {
				t.Fatal(err)
			}
			var transportLog bytes.Buffer
			opts := &Options{MountOptions: fuse.MountOptions{Logger: log.New(&transportLog, "", 0), EnableIoUring: true, IoUringQueueDepth: depth, MaxWrite: 1 << 20, EnableLocks: true}}
			if depth == 1 {
				opts.IoUringRings = 1
			}
			raw := NewNodeFS(root, opts)
			// Force reads through FUSE even after warmup, as in the random-read benchmark.
			srv, err := fuse.NewServer(&uringDirectFS{RawFileSystem: raw}, mp, &opts.MountOptions)
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan struct{})
			go func() { srv.Serve(); close(serveDone) }()
			defer func() {
				if err := srv.Unmount(); err != nil {
					t.Error(err)
				}
				<-serveDone
				if strings.Contains(transportLog.String(), "failed:") {
					t.Errorf("transport teardown: %s", transportLog.String())
				}
				match := regexp.MustCompile(`FUSE io_uring completed requests: (\d+)`).FindStringSubmatch(transportLog.String())
				if len(match) != 2 {
					t.Fatalf("no io_uring completions: %s", transportLog.String())
				}
				completed, _ := strconv.Atoi(match[1])
				if completed < 32*50 {
					t.Errorf("only %d io_uring requests; expected at least the concurrent reads", completed)
				}
			}()
			if err := srv.WaitMount(); err != nil {
				t.Fatal(err)
			}
			if srv.KernelSettings().Flags2&(1<<9) == 0 {
				t.Fatal("io_uring was not negotiated")
			}
			file, err := os.OpenFile(filepath.Join(mp, "data"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var wg sync.WaitGroup
			for job := 0; job < 32; job++ {
				wg.Add(1)
				go func(job int) {
					defer wg.Done()
					b := make([]byte, 300<<10)
					for i := 0; i < 50; i++ {
						size := 4096
						if i%3 == 0 {
							size = 300 << 10
						}
						b = b[:size]
						off := (job*127 + i*4096) % (len(data) - size)
						n, err := file.ReadAt(b, int64(off))
						if err != nil || n != len(b) || !bytes.Equal(b, data[off:off+len(b)]) {
							t.Errorf("read at %d: %d %v", off, n, err)
							return
						}
					}
				}(job)
			}
			wg.Wait()
			lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Len: 1}
			if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
				t.Fatalf("nonblocking lock: %v", err)
			}
			lock.Type = syscall.F_UNLCK
			if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
				t.Fatalf("unlock: %v", err)
			}
			lock.Type = syscall.F_WRLCK
			if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLKW, &lock); err != syscall.EOPNOTSUPP {
				t.Fatalf("blocking lock: %v, want EOPNOTSUPP", err)
			}
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != syscall.EOPNOTSUPP {
				t.Fatalf("blocking flock: %v, want EOPNOTSUPP", err)
			}
			b := make([]byte, 4096)
			n, err := file.ReadAt(b, int64(len(data)-1))
			if n != 1 || err != io.EOF || b[0] != data[len(data)-1] {
				t.Fatalf("short read: %d %v", n, err)
			}
			n, err = file.ReadAt(b, int64(len(data)))
			if n != 0 || err != io.EOF {
				t.Fatalf("EOF: %d %v", n, err)
			}
			if _, err = file.WriteAt([]byte("written"), 17); err != nil {
				t.Fatal(err)
			}
			if err = file.Sync(); err != nil {
				t.Fatal(err)
			}
			if _, err = file.ReadAt(b[:7], 17); err != nil || string(b[:7]) != "written" {
				t.Fatalf("write/read: %q %v", b[:7], err)
			}
			if err = os.Rename(filepath.Join(mp, "data"), filepath.Join(mp, "renamed")); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink("renamed", filepath.Join(mp, "link")); err != nil {
				t.Fatal(err)
			}
			if target, err := os.Readlink(filepath.Join(mp, "link")); err != nil || target != "renamed" {
				t.Fatalf("readlink: %s %v", target, err)
			}
			if err = syscall.Setxattr(filepath.Join(mp, "renamed"), "user.test", []byte("value"), 0); err != nil {
				t.Fatal(err)
			}
			n, err = syscall.Getxattr(filepath.Join(mp, "renamed"), "user.test", b)
			if err != nil || string(b[:n]) != "value" {
				t.Fatalf("xattr: %d %v", n, err)
			}
			if err = os.Remove(filepath.Join(mp, "renamed")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type uringDirectFS struct{ fuse.RawFileSystem }

func (f *uringDirectFS) Open(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	status := f.RawFileSystem.Open(cancel, in, out)
	out.OpenFlags |= fuse.FOPEN_DIRECT_IO
	return status
}

func (f *uringDirectFS) ReadInto(cancel <-chan struct{}, in *fuse.ReadIn, buf []byte) (uint32, fuse.Status) {
	result, status := f.RawFileSystem.Read(cancel, in, buf)
	if result != nil {
		defer result.Done()
	}
	if !status.Ok() {
		return 0, status
	}
	data, status := result.Bytes(buf)
	if status.Ok() {
		copy(buf, data)
	}
	return uint32(len(data)), status
}
