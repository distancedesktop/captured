package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// cpuTime is user+system time for pid from /proc, or -1.
func cpuTime(pid int) time.Duration {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	var u, k int64
	fmt.Sscan(f[11], &u)
	fmt.Sscan(f[12], &k)
	return time.Duration(u+k) * time.Second / 100
}
