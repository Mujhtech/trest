//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import "os/exec"

func configureBuildCancellation(_ *exec.Cmd) {}
