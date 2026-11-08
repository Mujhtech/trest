//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package aitool

import (
	"os"
	"syscall"
)

func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
