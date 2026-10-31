//go:build !linux && !windows

package main

import "time"

func cpuTime(int) time.Duration { return -1 }
