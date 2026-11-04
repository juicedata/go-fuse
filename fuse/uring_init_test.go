package fuse

import (
	"testing"
	"unsafe"
)

func TestUringInitWithIDMap(t *testing.T) {
	const idmap = CAP_ALLOW_IDMAP >> 32
	for _, tc := range []struct {
		name         string
		uring, idmap bool
		want         uint32
	}{
		{"neither", false, false, 0},
		{"uring", true, false, _CAP_OVER_IO_URING_FLAGS2},
		{"idmap", false, true, idmap},
		{"both", true, true, _CAP_OVER_IO_URING_FLAGS2 | idmap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &Server{opts: &MountOptions{EnableIoUring: tc.uring, IDMappedMount: tc.idmap, MaxWrite: 4096}}
			in := InitIn{Major: 7, Minor: 42, Flags: CAP_INIT_EXT, Flags2: _CAP_OVER_IO_URING_FLAGS2 | idmap}
			r := &request{handler: getHandler(_OP_INIT), inData: unsafe.Pointer(&in)}
			doInit(ms, r)
			out := (*InitOut)(r.outData())
			if r.status != OK || out.Flags2 != tc.want || (out.Flags&CAP_INIT_EXT != 0) != (tc.want != 0) {
				t.Fatalf("INIT status %v, flags %#x, flags2 %#x (want %#x)", r.status, out.Flags, out.Flags2, tc.want)
			}
		})
	}
}
