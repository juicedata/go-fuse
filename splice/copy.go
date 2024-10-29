// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package splice

import (
	"fmt"
	"io"
	"os"
)

// NOTE: DEPRECIATED
const (
	FADVISE_NORMAL     = 0x0
	FADVISE_RANDOM     = 0x1
	FADVISE_SEQUENTIAL = 0x2
	FADVISE_WILLNEED   = 0x3
	FADVISE_DONTNEED   = 0x4
	FADVISE_NOREUSE    = 0x5
)

func SpliceCopy(dst *os.File, src *os.File, p *Pair) (int64, error) {
	total := int64(0)
	st, _ := src.Stat()
	err := Fadvise64(int(src.Fd()), 0, st.Size(), FADVISE_SEQUENTIAL)
	if err != nil {
		fmt.Println("Fadvise64 error:", err)
	}
	for {
		n, err := p.LoadFrom(src.Fd(), p.size)
		if err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		m, err := p.WriteTo(dst.Fd(), n)
		total += int64(m)
		if err != nil {
			return total, err
		}
		if m < n {
			return total, err
		}
		if int(n) < p.size {
			break
		}
	}

	return total, nil
}

// Argument ordering follows io.Copy.
func CopyFile(dstName string, srcName string, mode int) error {
	src, err := os.Open(srcName)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return err
	}
	defer dst.Close()

	return CopyFds(dst, src)
}

func CopyFds(dst *os.File, src *os.File) (err error) {
	p, err := splicePool.get()
	if p != nil {
		p.Grow(256 * 1024)
		_, err := SpliceCopy(dst, src, p)
		splicePool.done(p)
		return err
	} else {
		_, err = io.Copy(dst, src)
	}
	if err == io.EOF {
		err = nil
	}
	return err
}
