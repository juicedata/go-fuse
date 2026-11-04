//go:build !linux

package fuse

import (
	"strings"
	"testing"
)

func TestUringUnsupportedPlatform(t *testing.T) {
	_, err := NewServer(NewDefaultRawFileSystem(), t.TempDir(), &MountOptions{EnableIoUring: true})
	if err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("expected platform rejection before mounting, got %v", err)
	}
}
