//go:build linux
// +build linux

package web

import (
	"fmt"
	"syscall"
)

func init() {
	rlimit := syscall.Rlimit{
		Cur: 1000000,
		Max: 1000000,
	}
	syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		fmt.Println("warning! error getting rlimit. make sure it is big if you want to test concurrency using this tool")
	} else {
		if rlimit.Max < 1000000 {
			fmt.Println("warning! open file limit is below 1 mil, run this app as root or set the limit")
			fmt.Println("via ulimit")
		}
	}
}
