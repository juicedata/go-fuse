//go:build !linux

package fuse

import "fmt"

type uringQueue struct{}

func (ms *Server) prepareUring() error {
	if ms.opts.EnableIoUring {
		return fmt.Errorf("FUSE io_uring is only supported on Linux")
	}
	return nil
}
func (ms *Server) startUring() error { return nil }
func (ms *Server) stopUring()        {}
func (ms *Server) closeUring()       {}
