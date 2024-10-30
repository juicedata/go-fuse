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
// This is a four-step process:
//
//  1. Splice data form fdData.Fd into the "pair1" pipe buffer --> pair1: [payload]
//     Now we know the actual payload length and can
//     construct the reply header
//  2. Write header into the "pair2" pipe buffer               --> pair2: [header]
//  4. Splice data from "pair1" into "pair2"                   --> pair2: [header][payload]
//  3. Splice the data from "pair2" into /dev/fuse
//
// This dance is neccessary because header and payload cannot be split across
// two splices and we cannot seek in a pipe buffer.
func (ms *Server) trySplice(header []byte, req *request, fdData *readResultFd) error {
	// Get a pair of connected pipes
	pair, err := splice.Get()
	if err != nil {
		return err
	}
	defer splice.Done(pair)

	// Grow pipe to header + actually read size + one extra page
	// Without the extra page the kernel will block once the pipe is almost full
	payloadLen := fdData.Size()
	header = req.serializeHeader(payloadLen)
	total := len(header) + payloadLen
	// FIXME: use pipes big enough to skip the Grow()?
	if err := pair.Grow(total + os.Getpagesize()); err != nil {
		return err
	}

	// Write header into pair2
	n, err := pair.Write(header)
	if err != nil {
		return err
	}
	if n != len(header) {
		return fmt.Errorf("Short write into splice: wrote %d, want %d", n, len(header))
	}

	// Write data into pair2
	n, err = pair.LoadFrom(fdData.Fd, payloadLen)
	if err != nil {
		return err
	}
	if n != payloadLen {
		return fmt.Errorf("Short splice: wrote %d, want %d", n, payloadLen)
	}

	// Write header + data to /dev/fuse
	_, err = pair.WriteTo(uintptr(ms.mountFd), total)
	return err
}
