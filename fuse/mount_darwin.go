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
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func openFUSEDevice() (*os.File, error) {
	fs, err := filepath.Glob("/dev/osxfuse*")
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		bin := oldLoadBin
		if _, err := os.Stat(newLoadBin); err == nil {
			bin = newLoadBin
		}

		cmd := exec.Command(bin)
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		fs, err = filepath.Glob("/dev/osxfuse*")
		if err != nil {
			return nil, err
		}
	}

	for _, fn := range fs {
		f, err := os.OpenFile(fn, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		return f, nil
	}

	return nil, fmt.Errorf("all FUSE devices busy")
}

const oldLoadBin = "/Library/Filesystems/osxfusefs.fs/Support/load_osxfusefs"
const newLoadBin = "/Library/Filesystems/osxfuse.fs/Contents/Resources/load_osxfuse"

const oldMountBin = "/Library/Filesystems/osxfusefs.fs/Support/mount_osxfusefs"
const newMountBin = "/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse"

const loadBinV4 = "/Library/Filesystems/macfuse.fs/Contents/Resources/load_macfuse"
const mountBinV4 = "/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse"

func mount(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	if _, err := os.Stat(mountBinV4); err == nil {
		return mountV4(mountPoint, opts, ready)
	}

	f, err := openFUSEDevice()
	if err != nil {
		return 0, err
	}

	bin := oldMountBin
	if _, err := os.Stat(newMountBin); err == nil {
		bin = newMountBin
	}

	cmd := exec.Command(bin, "-o", strings.Join(opts.optionsStrings(), ","), "-o", fmt.Sprintf("iosize=%d", opts.MaxWrite), "3", mountPoint)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Env = append(os.Environ(), "MOUNT_FUSEFS_CALL_BY_LIB=", "MOUNT_OSXFUSE_CALL_BY_LIB=",
		"MOUNT_OSXFUSE_DAEMON_PATH="+os.Args[0],
		"MOUNT_FUSEFS_DAEMON_PATH="+os.Args[0])

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Start(); err != nil {
		f.Close()
		return 0, err
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("mount_osxfusefs failed: %v. Stderr: %s, Stdout: %s", err, errOut.String(), out.String())
		}

		ready <- err
		close(ready)
	}()

	// The finalizer for f will close its fd so we return a dup.
	defer f.Close()
	return syscall.Dup(int(f.Fd()))
}

func unixgramSocketpair() (l, r *os.File, err error) {
	fd, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, os.NewSyscallError("socketpair",
			err.(syscall.Errno))
	}
	l = os.NewFile(uintptr(fd[0]), "socketpair-half1")
	r = os.NewFile(uintptr(fd[1]), "socketpair-half2")
	return
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

func mountV4(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	local, remote, err := unixgramSocketpair()
	if err != nil {
		return
	}
	defer local.Close()
	defer remote.Close()

	bin := mountBinV4
	cmd := []string{bin, mountPoint}
	if s := opts.optionsStrings(); len(s) > 0 {
		cmd = append(cmd, "-o", strings.Join(s, ","))
	}

	proc, err := os.StartProcess(bin,
		cmd,
		&os.ProcAttr{
			Env:   []string{"_FUSE_COMMFD=3", "_FUSE_CALL_BY_LIB=1", "_FUSE_DAEMON_PATH=" + os.Args[0]},
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, remote}})

	if err != nil {
		return
	}

	fd, err = getConnection(local)
	if err != nil {
		return -1, err
	}

	go func() {
		w, err := proc.Wait()
		if err != nil {
			log.Printf("wait mount_macfuse: %s", err)
		} else if !w.Success() {
			log.Printf("mount_macfuse exited with code %v", w.Sys())
		}
	}()

	// golang sets CLOEXEC on file descriptors when they are
	// acquired through normal operations (e.g. open).
	// Buf for fd, we have to set CLOEXEC manually
	syscall.CloseOnExec(fd)

	close(ready)
	return fd, nil
}

func unmount(dir string, opts *MountOptions) error {
	return syscall.Unmount(dir, 0)
}
