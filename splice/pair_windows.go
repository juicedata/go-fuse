// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package splice

func (p *Pair) LoadFromAt(fd uintptr, sz int, off int64) (int, error) {
	panic("not implemented")
}

func (p *Pair) LoadFrom(fd uintptr, sz int) (int, error) {
	panic("not implemented")
}

func (p *Pair) WriteTo(fd uintptr, n int) (int, error) {
	panic("not implemented")
}

func (p *Pair) discard() {
	panic("not implemented")
}

func (p *Pair) Close() error {
	panic("not implemented")
}

func (p *Pair) Read(d []byte) (n int, err error) {
	panic("not implemented")
}

func (p *Pair) Write(d []byte) (n int, err error) {
	panic("not implemented")
}
