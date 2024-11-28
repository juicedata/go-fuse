// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package splice

import (
	"fmt"
	"runtime"
	"sync"
)

var splicePool = sync.Pool{
	New: newPoolPipe,
}

func newPoolPipe() interface{} {
	// Discard the error which occurred during the creation of pipe buffer,
	// redirecting the data transmission to the conventional way utilizing read() + write() as a fallback.
	p := newPipe()
	if p == nil {
		return nil
	}
	runtime.SetFinalizer(p, destroyPipe)
	return p
}

type pairPool struct {
	sync.Mutex
	unused    []*Pair
	usedCount int
}

func Get() (*Pair, error) {
	p := splicePool.Get()
	if p == nil {
		return nil, fmt.Errorf("create pipe failed")
	}
	return p.(*Pair), nil
}

// Done returns the pipe pair to pool.
func Done(p *Pair) {
	p.discard()
	splicePool.Put(p)
}

// Closes and discards pipe pair.
func Drop(p *Pair) {
	runtime.SetFinalizer(p, nil)
	destroyPipe(p)
}
