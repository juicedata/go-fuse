package main

import (
	"fmt"
	"os"
	"time"

	"github.com/hanwen/go-fuse/v2/splice"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: fastcopy <src> <dst>")
		return
	}
	src, arg := os.Args[1], os.Args[2]
	start := time.Now()
	st, err := os.Stat(src)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	err = splice.CopyFile(arg, src, 0644)
	used := time.Since(start)
	speed := float64(st.Size()) / used.Seconds() / 1024 / 1024
	fmt.Printf("Copied %s to %s in %v, speed: %.1f MiB/s\n", src, arg, used, speed)
}
