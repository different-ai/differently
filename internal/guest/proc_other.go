//go:build !unix

package guest

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}
