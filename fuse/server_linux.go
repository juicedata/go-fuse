// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"log"
	"syscall"
)

func (ms *Server) systemWrite(req *request, header []byte) Status {
	size := req.flatDataSize()
	if size == 0 {
		err := handleEINTR(func() error {
			_, err := syscall.Write(ms.mountFd, header)
			return err
		})
		return ToStatus(err)
	}

	if ms.canSplice && ms.opts.MinSpliceSize > 0 && size > ms.opts.MinSpliceSize {
		err := ms.trySplice(header, req)
		if err == nil {
			if req.readResult != nil {
				req.readResult.Done()
			}
			return OK
		}
		log.Println("trySplice:", err)
	}

	if req.fdData != nil {
		buf := ms.allocOut(req, uint32(size))
		var st int
		req.flatData, st = req.fdData.Bytes(buf)
		req.status = Status(st)
		header = req.serializeHeader(len(req.flatData))
	}

	bufs := [][]byte{header}
	if req.slices != nil {
		bufs = append(bufs, req.slices...)
	} else {
		bufs = append(bufs, req.flatData)
	}
	_, err := writev(ms.mountFd, bufs)
	if req.readResult != nil {
		req.readResult.Done()
	}
	return ToStatus(err)
}
