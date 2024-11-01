package splice

import (
	"fmt"
	"syscall"
)

// copy & paste from syscall.
func fcntl(fd uintptr, cmd int, arg int) (val int, errno syscall.Errno) {
	r0, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(cmd), uintptr(arg))
	val = int(r0)
	errno = syscall.Errno(e1)
	return
}

func osPipe() (int, int, error) {
	return 0, 0, fmt.Errorf("not implemented")
}
