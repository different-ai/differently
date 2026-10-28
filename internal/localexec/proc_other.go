//go:build !unix

package localexec

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}
