// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"fmt"
	"os"

	"github.com/hanwen/go-fuse/v2/splice"
)

func (s *Server) setSplice() {
	s.canSplice = splice.Resizable()
}

// trySplice:  Zero-copy read from fdData.Fd into /dev/fuse
//
// This is a three-step process:
//
//  1. Write header into the pipe buffer               --> pair2: [header]
//  2. Splice data from "fdData" into pipe buffer                   --> pair2: [header][payload]
//  3. Splice the data from pipe buffer into /dev/fuse
//
// This dance is neccessary because header and payload cannot be split across
// two splices and we cannot seek in a pipe buffer.
func (ms *Server) trySplice(header []byte, req *request) error {
	// Get a pipe of connected pipes
	pipe, err := splice.Get()
	if err != nil {
		return err
	}
	defer splice.Done(pipe)

	size := req.flatDataSize()
	total := len(header) + size
	// Grow buffer pipe to requested size + one extra page
	// Without the extra page the kernel will block once the pipe is almost full
	pair1Sz := total + os.Getpagesize()
	if err := pipe.Grow(pair1Sz); err != nil {
		return err
	}

	if req.fdData == nil {
		n, err := pipe.Write(header)
		if err != nil {
			return err
		}
		if n != len(header) {
			return fmt.Errorf("Short write into splice: wrote %d, want %d", n, len(header))
		}
		// Read data from file
		fdData := req.fdData
		n, err = pipe.LoadFromAt(fdData.Fd, size, fdData.Off)
		if err != nil {
			// TODO - extract the data from splice.
			return err
		}
		if n != size {
			return fmt.Errorf("short read from file: %d < %d", n, size)
		}
	} else {
		bufs := [][]byte{header}
		if req.slices != nil {
			bufs = append(bufs, req.slices...)
		} else {
			bufs = append(bufs, req.flatData)
		}
		_, err := writev(int(pipe.WriteFd()), bufs)
		if err != nil {
			return err
		}
	}

	// Write header + data to /dev/fuse
	_, err = pipe.WriteTo(uintptr(ms.mountFd), total)
	return err
}
