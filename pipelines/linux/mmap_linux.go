//go:build linux

package linux

import "syscall"

func mmapRO(fd, length int) ([]byte, error) {
	return syscall.Mmap(fd, 0, length, syscall.PROT_READ, syscall.MAP_SHARED)
}

func mmapRW(fd, length int) ([]byte, error) {
	return syscall.Mmap(fd, 0, length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

func munmap(b []byte) error {
	return syscall.Munmap(b)
}

func closeFD(fd int) error {
	return syscall.Close(fd)
}
