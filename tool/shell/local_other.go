//go:build !unix

package shell

import "os/exec"

// killProcessGroup does nothing on non-Unix platforms: canceling only ends the command's own process.
func killProcessGroup(*exec.Cmd) {}
