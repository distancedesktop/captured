package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// cpuTime is user+kernel time from GetProcessTimes, or -1.
func cpuTime(pid int) time.Duration {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return -1
	}
	ticks := func(f windows.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration(ticks(k)+ticks(u)) * 100
}
