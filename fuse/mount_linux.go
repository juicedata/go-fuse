// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func unixgramSocketpair() (l, r *os.File, err error) {
	fd, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil, os.NewSyscallError("socketpair",
			err.(syscall.Errno))
	}
	l = os.NewFile(uintptr(fd[0]), "socketpair-half1")
	r = os.NewFile(uintptr(fd[1]), "socketpair-half2")
	return
}

func mtabNeedUpdate(mnt string) bool {
	mtabPath := "/etc/mtab"
	if strings.HasPrefix(mtabPath, mnt) {
		return false
	}
	st, err := os.Lstat(mtabPath)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if unix.Access(mtabPath, unix.W_OK) != nil {
		return false
	}
	return true
}

func updateMtab(source, mnt, _type, options string) {
	cmd := exec.Cmd{
		Path: "/bin/mount",
		Args: []string{
			"/bin/mount", "--no-canonicalize", "-i", "-f", "-t", "fuse." + _type, "-o", options, source, mnt,
		},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		println("update /etc/mtab: ", string(out))
	}
}

// Create a FUSE FS on the specified mount point without using
// fusermount.
func mountDirect(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	fd, err = syscall.Open("/dev/fuse", os.O_RDWR, 0) // use syscall.Open since we want an int fd
	if err != nil {
		return
	}

	// managed to open dev/fuse, attempt to mount
	source := opts.FsName
	if source == "" {
		source = opts.Name
	}

	var flags uintptr = syscall.MS_NOSUID | syscall.MS_NODEV
	if opts.DirectMountFlags != 0 {
		flags = opts.DirectMountFlags
	}

	// some values we need to pass to mount, but override possible since opts.Options comes after
	var r = []string{
		fmt.Sprintf("fd=%d", fd),
		"rootmode=40000",
		"user_id=0",
		"group_id=0",
	}
	for _, o := range opts.Options {
		if o != "nonempty" && o != "allow_root" {
			r = append(r, o)
		}
	}

	if opts.AllowOther {
		r = append(r, "allow_other")
	}

	if opts.Debug {
		log.Printf("mountDirect: calling syscall.Mount(%q, %q, %q, %#x, %q)",
			source, mountPoint, "fuse."+opts.Name, flags, strings.Join(r, ","))
	}
	err = syscall.Mount(source, mountPoint, "fuse."+opts.Name, flags, strings.Join(r, ","))
	if err != nil {
		syscall.Close(fd)
		return
	}

	if os.Geteuid() == 0 {
		realmnt, _ := filepath.Abs(mountPoint)
		if mtabNeedUpdate(realmnt) {
			updateMtab(source, realmnt, opts.Name, strings.Join(r, ","))
		}
	}

	// success
	close(ready)
	return
}

// callFusermount calls the `fusermount` suid helper with the right options so
// that it:
// * opens `/dev/fuse`
// * mount()s this file descriptor to `mountPoint`
// * passes this file descriptor back to use via a unix domain socket
func callFusermount(mountPoint string, opts *MountOptions) (fd int, err error) {
	local, remote, err := unixgramSocketpair()
	if err != nil {
		return
	}

	defer local.Close()
	defer remote.Close()

	bin, err := fusermountBinary()
	if err != nil {
		return 0, err
	}

	cmd := []string{bin, mountPoint}
	if s := opts.optionsStrings(); len(s) > 0 {
		cmd = append(cmd, "-o", strings.Join(s, ","))
	}
	proc, err := os.StartProcess(bin,
		cmd,
		&os.ProcAttr{
			Env:   []string{"_FUSE_COMMFD=3"},
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, remote}})

	if err != nil {
		return
	}

	w, err := proc.Wait()
	if err != nil {
		return
	}
	if !w.Success() {
		err = fmt.Errorf("fusermount exited with code %v\n", w.Sys())
		return
	}

	fd, err = getConnection(local)
	if err != nil {
		return -1, err
	}

	return
}

// parseFuseFd checks if `mountPoint` is the special form /dev/fd/N (with N >= 0),
// and returns N in this case. Returns -1 otherwise.
func parseFuseFd(mountPoint string) (fd int) {
	dir, file := path.Split(mountPoint)
	if dir != "/dev/fd/" {
		return -1
	}
	fd, err := strconv.Atoi(file)
	if err != nil || fd <= 0 {
		return -1
	}
	return fd
}

// Create a FUSE FS on the specified mount point.  The returned
// mount point is always absolute.
func mount(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	if opts.DirectMount {
		fd, err := mountDirect(mountPoint, opts, ready)
		if err == nil {
			return fd, nil
		} else if opts.Debug {
			log.Printf("mount: failed to do direct mount: %s", err)
		}
	}

	// Magic `/dev/fd/N` mountpoint. See the docs for NewServer() for how this
	// works.
	fd = parseFuseFd(mountPoint)
	if fd >= 0 {
		if opts.Debug {
			log.Printf("mount: magic mountpoint %q, using fd %d", mountPoint, fd)
		}
	} else {
		// Usual case: mount via the `fusermount` suid helper
		fd, err = callFusermount(mountPoint, opts)
		if err != nil {
			return
		}
	}
	// golang sets CLOEXEC on file descriptors when they are
	// acquired through normal operations (e.g. open).
	// Buf for fd, we have to set CLOEXEC manually
	syscall.CloseOnExec(fd)
	close(ready)
	return fd, err
}

func unmount(mountPoint string, opts *MountOptions) (err error) {
	if opts.DirectMount {
		// Attempt to directly unmount, if fails fallback to fusermount method
		err := syscall.Unmount(mountPoint, 0)
		if err == nil {
			return nil
		}
	}

	bin, err := fusermountBinary()
	if err != nil {
		return err
	}
	errBuf := bytes.Buffer{}
	cmd := exec.Command(bin, "-u", mountPoint)
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if errBuf.Len() > 0 {
		return fmt.Errorf("%s (code %v)\n",
			errBuf.String(), err)
	}
	return err
}

func getConnection(local *os.File) (int, error) {
	var data [4]byte
	control := make([]byte, 4*256)

	// n, oobn, recvflags, from, errno  - todo: error checking.
	_, oobn, _, _,
		err := syscall.Recvmsg(
		int(local.Fd()), data[:], control[:], 0)
	if err != nil {
		return 0, err
	}

	message := *(*syscall.Cmsghdr)(unsafe.Pointer(&control[0]))
	fd := *(*int32)(unsafe.Pointer(uintptr(unsafe.Pointer(&control[0])) + syscall.SizeofCmsghdr))

	if message.Type != 1 {
		return 0, fmt.Errorf("getConnection: recvmsg returned wrong control type: %d", message.Type)
	}
	if oobn <= syscall.SizeofCmsghdr {
		return 0, fmt.Errorf("getConnection: too short control message. Length: %d", oobn)
	}
	if fd < 0 {
		return 0, fmt.Errorf("getConnection: fd < 0: %d", fd)
	}
	return int(fd), nil
}

// lookPathFallback - search binary in PATH and, if that fails,
// in fallbackDir. This is useful if PATH is possible empty.
func lookPathFallback(file string, fallbackDir string) (string, error) {
	binPath, err := exec.LookPath(file)
	if err == nil {
		return binPath, nil
	}

	abs := path.Join(fallbackDir, file)
	return exec.LookPath(abs)
}

func fusermountBinary() (string, error) {
	return lookPathFallback("fusermount", "/bin")
}

func umountBinary() (string, error) {
	return lookPathFallback("umount", "/bin")
}
