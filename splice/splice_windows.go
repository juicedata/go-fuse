package splice

import (
	"fmt"
	"syscall"
)

func osPipe() (int, int, error) {
	return 0, 0, fmt.Errorf("not implemented")
}

func fcntl(fd uintptr, cmd int, arg int) (val int, errno syscall.Errno) {
	return 0, syscall.Errno(1)
}
