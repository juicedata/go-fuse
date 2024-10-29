package main

import (
	"fmt"
	"os"
	"time"

	"github.com/hanwen/go-fuse/v2/splice"
)

func copy(p *splice.Pair, f *os.File) (int64, error) {
	total := int64(0)
	for {
		m, err := p.WriteTo(f.Fd(), 128<<10)
		total += int64(m)
		if err != nil {
			return total, err
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: fastwrite <dst>")
		return
	}
	dst := os.Args[1]
	f, err := os.Create(dst)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer f.Close()

	pair, _ := splice.Get()
	pair.Grow(256 * 1024)
	defer pair.Close()

	go func() {
		buf := make([]byte, 128<<10)
		for i := 0; i < 1000; i++ {
			pair.Write(buf)
		}
		pair.Close()
	}()
	start := time.Now()
	copy(pair, f)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	used := time.Since(start)
	speed := float64(128<<10*1000) / used.Seconds() / 1024 / 1024
	fmt.Printf("Copied to %s in %v, speed: %.1f MiB/s\n", dst, used, speed)
}
