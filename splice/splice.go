// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package splice

// Routines for efficient file to file copying.

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"syscall"
)

var maxPipeSize int
var resizable bool

func Resizable() bool {
	return resizable
}

func MaxPipeSize() int {
	return maxPipeSize
}

// From manpage on ubuntu Lucid:
//
// Since Linux 2.6.11, the pipe capacity is 65536 bytes.
const DefaultPipeSize = 16 * 4096

// We empty pipes by splicing to /dev/null.
var devNullFD uintptr

func init() {
	if runtime.GOOS != "linux" {
		return
	}
	content, err := ioutil.ReadFile("/proc/sys/fs/pipe-max-size")
	if err != nil {
		maxPipeSize = DefaultPipeSize
	} else {
		fmt.Sscan(string(content), &maxPipeSize)
	}

	r, w, err := os.Pipe()
	if err != nil {
		log.Panicf("cannot create pipe: %v", err)
	}
	sz, errNo := fcntl(r.Fd(), F_GETPIPE_SZ, 0)
	resizable = (errNo == 0)
	_, errNo = fcntl(r.Fd(), F_SETPIPE_SZ, 2*sz)
	resizable = resizable && (errNo == 0)
	r.Close()
	w.Close()

	fd, err := syscall.Open("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		log.Panicf("splice: %v", err)
	}

	devNullFD = uintptr(fd)
}

const F_SETPIPE_SZ = 1031
const F_GETPIPE_SZ = 1032
