//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly || solaris)

package tools

import (
	"os/exec"
)

func configureCommandCancellation(cmd *exec.Cmd) {
}
