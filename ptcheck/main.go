// Standalone check for the ported passthrough primitives.
// Run: go run ./ptcheck <emptydir-on-tmpfs> <backing-file-on-tmpfs>
package main

import (
	"fmt"
	"os"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type root struct{ fs.Inode }

func main() {
	mnt := os.Args[1]
	server, err := fs.Mount(mnt, &root{}, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:             true,
			EnablePassthrough: true,
			MaxStackDepth:     2,
		},
	})
	if err != nil {
		fmt.Println("MOUNT ERR:", err)
		os.Exit(1)
	}
	fmt.Println(">>> SupportsPassthrough():", server.SupportsPassthrough())

	f, err := os.OpenFile(os.Args[2], os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		fmt.Println("OPEN backing ERR:", err)
		server.Unmount()
		os.Exit(1)
	}
	id, errno := server.RegisterBackingFd(&fuse.BackingMap{Fd: int32(f.Fd())})
	fmt.Printf(">>> RegisterBackingFd: id=%d errno=%v\n", id, errno)
	if errno == 0 {
		fmt.Printf(">>> UnregisterBackingFd errno=%v\n", server.UnregisterBackingFd(id))
	}
	f.Close()
	server.Unmount()
}
