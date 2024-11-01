package splice

import "syscall"

func osPipe() (int, int, error) {
	var fds [2]int
	err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK)
	return fds[0], fds[1], err
}
