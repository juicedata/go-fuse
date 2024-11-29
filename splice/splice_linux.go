package splice

import (
	"log"
	"syscall"
)

// copy & paste from syscall.
func fcntl(fd uintptr, cmd int, arg int) (val int, errno syscall.Errno) {
	r0, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(cmd), uintptr(arg))
	val = int(r0)
	errno = syscall.Errno(e1)
	return
}

func newPipe() *Pair {
	var fds [2]int
	var err error
	err = syscall.Pipe2(fds[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK)
	if err != nil {
		log.Printf("Warning: create pipe failed: %v\n", err)
		return nil
	}
	if resizable {
		_, ferr := fcntl(uintptr(fds[0]), syscall.F_SETPIPE_SZ, maxPipeSize)
		if ferr != syscall.Errno(0) {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			log.Printf("Warning: grow pipe failed: %v\n", ferr)
			return nil
		}
	}
	return &Pair{r: fds[0], w: fds[1], size: maxPipeSize}
}

func destroyPipe(p *Pair) {
	err := p.close()
	if err != nil {
		log.Printf("close pipe failed: %v\n", err)
	}
}
