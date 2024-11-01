package splice

import "syscall"

// copy & paste from syscall.
func fcntl(fd uintptr, cmd int, arg int) (val int, errno syscall.Errno) {
	r0, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(cmd), uintptr(arg))
	val = int(r0)
	errno = syscall.Errno(e1)
	return
}

func osPipe() (int, int, error) {
	var fds [2]int
	err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK)
	return fds[0], fds[1], err
}
