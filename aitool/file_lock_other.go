//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package aitool

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("durable local store locking is unsupported on this platform; supply a transactional Store")
}
