// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

const outputHeaderSize = 160

const (
	_FUSE_KERNEL_VERSION   = 7
	_MINIMUM_MINOR_VERSION = 12
	_OUR_MINOR_VERSION     = 28
	// _MINOR_VERSION_INIT_EXT is the first minor version whose INIT request
	// wire size covers InitIn.Flags2 (see request.go's parse(), which clamps
	// a legacy-sized INIT read to the shorter struct). Reading Flags2 for an
	// older client isn't just "wrong protocol data" — the request buffer is
	// pooled and reused across requests without zeroing (see reqPool), so
	// bytes past what a short INIT actually wrote are leftover content from
	// an unrelated prior request, not zero. Anything that reads InitIn.Flags2
	// (directly, or via Server.kernelSettings) must gate on this first.
	_MINOR_VERSION_INIT_EXT = 36
)
