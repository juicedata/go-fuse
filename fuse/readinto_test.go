package fuse

import (
	"testing"
	"unsafe"
)

type intoTestFS struct {
	RawFileSystem
	count  uint32
	status Status
}

func (f *intoTestFS) ReadInto(_ <-chan struct{}, _ *ReadIn, b []byte) (uint32, Status) {
	for i := range b {
		b[i] = byte(i * 37)
	}
	return f.count, f.status
}
func TestReadIntoBuffer(t *testing.T) {
	for _, tc := range []struct {
		size, count uint32
		status      Status
		want        Status
	}{
		{4096, 4096, OK, OK}, {4096, 17, OK, OK}, {4096, 0, OK, OK},
		{4096, 4097, OK, EIO}, {4096, 0, EINTR, EINTR}, {8192, 0, OK, EIO},
	} {
		data := make([]byte, 4096)
		in := ReadIn{Size: tc.size}
		req := &request{inData: unsafe.Pointer(&in), uringOutput: data}
		srv := &Server{fileSystem: &intoTestFS{count: tc.count, status: tc.status}}
		doRead(srv, req)
		if req.status != tc.want {
			t.Fatalf("%+v: %v", tc, req.status)
		}
		if req.status.Ok() {
			if len(req.flatData) != int(tc.count) {
				t.Fatal("wrong length")
			}
			if tc.count > 0 && &req.flatData[0] != &data[0] {
				t.Fatal("reply does not alias transport buffer")
			}
		}
		req.clear()
		if req.uringOutput != nil {
			t.Fatal("retained mmap buffer in pool")
		}
	}
}
