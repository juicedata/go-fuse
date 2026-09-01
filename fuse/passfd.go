// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"net"
	"syscall"
)

// Get receives file descriptors from a Unix domain socket.
//
// Num specifies the expected number of file descriptors in one message.
// Internal files' names to be assigned are specified via optional filenames
// argument.
//
// You need to close all files in the returned slice. The slice can be
// non-empty even if this function returns an error.
func getFd(via *net.UnixConn, num int) ([]byte, []int, error) {
	if num < 1 {
		return nil, nil, nil
	}

	viaf, err := via.File()
	if err != nil {
		return nil, nil, err
	}
	socket := int(viaf.Fd())
	defer viaf.Close()

	msg := make([]byte, syscall.CmsgSpace(100))
	buf := make([]byte, syscall.CmsgSpace(num*4))
	n, oobn, _, _, err := syscall.Recvmsg(socket, msg, buf, MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, nil, err
	}

	msgs, err := syscall.ParseSocketControlMessage(buf[:oobn])
	if err != nil {
		return nil, nil, err
	}
	fds := make([]int, 0, len(msgs))
	for _, msg := range msgs {
		rights, err := syscall.ParseUnixRights(&msg)
		fds = append(fds, rights...)
		if err != nil {
			for _, fd := range fds {
				_ = syscall.Close(fd)
			}
			return nil, nil, err
		}
	}
	return msg[:n], fds, nil
}

// putFd sends file descriptors to Unix domain socket.
//
// Please note that the number of descriptors in one message is limited
// and is rather small.
func putFd(via *net.UnixConn, msg []byte, fds ...int) error {
	if len(fds) == 0 {
		return nil
	}
	viaf, err := via.File()
	if err != nil {
		return err
	}
	socket := int(viaf.Fd())
	defer viaf.Close()

	rights := syscall.UnixRights(fds...)
	return syscall.Sendmsg(socket, msg, rights, nil, 0)
}

func sendFuseFd(path string, msg []byte, fd int) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, fds, err := getFd(conn.(*net.UnixConn), 2)
	if err != nil {
		return err
	}
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
	return putFd(conn.(*net.UnixConn), msg, fd)
}

func closeFuseFd(path string) error {
	if path == "" {
		return nil
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, fds, err := getFd(conn.(*net.UnixConn), 2)
	if err != nil {
		return err
	}
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
	return nil
}
