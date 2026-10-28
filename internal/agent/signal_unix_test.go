//go:build unix

package agent

import (
	"os"
	"syscall"
)

func syscallZero() os.Signal { return syscall.Signal(0) }
