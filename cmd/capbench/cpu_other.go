//go:build !windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cpuTime is user+system time from /proc/<pid>/stat, or -1.
func cpuTime(pid int) time.Duration {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	u, _ := strconv.ParseInt(f[11], 10, 64)
	k, _ := strconv.ParseInt(f[12], 10, 64)
	return time.Duration(u+k) * time.Second / 100
}
