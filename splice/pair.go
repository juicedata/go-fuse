// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package splice

import (
	"fmt"
)

type Pair struct {
	r, w int
	size int

	// We want to use a finalizer, so ensure that the size is
	// large enough to not use the tiny allocator.
	_ [12]byte
}

func (p *Pair) MaxGrow() {
	for p.Grow(2*p.size) == nil {
	}
}

func (p *Pair) Grow(n int) error {
	if n <= p.size {
		return nil
	}
	if !resizable {
		return fmt.Errorf("splice: want %d bytes, but not resizable", n)
	}
	if n > maxPipeSize {
		return fmt.Errorf("splice: want %d bytes, max pipe size %d", n, maxPipeSize)
	}

	newsize, errNo := fcntl(uintptr(p.r), F_SETPIPE_SZ, n)
	if errNo != 0 {
		return fmt.Errorf("splice: fcntl returned %v", errNo)
	}
	p.size = newsize
	return nil
}

func (p *Pair) Cap() int {
	return p.size
}

func (p *Pair) ReadFd() uintptr {
	return uintptr(p.r)
}

func (p *Pair) WriteFd() uintptr {
	return uintptr(p.w)
}
