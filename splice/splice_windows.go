package splice

import (
	"syscall"
)

func newPipe() *Pair {
	return nil
}

func destroyPipe(p *Pair) {
	panic("not implemented")
}

func fcntl(fd uintptr, cmd int, arg int) (val int, errno syscall.Errno) {
	return 0, syscall.Errno(1)
}
