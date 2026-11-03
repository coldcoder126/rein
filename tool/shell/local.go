package shell

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"
)

// Local runs commands on this machine. It is only suited to development or trusted environments: commands have all the permissions of the current process.
// Each command runs in its own process group, so canceling or timing out also ends the child processes it started.
type Local struct {
	// Dir is the working directory every command starts in; empty means the working directory of the current process.
	Dir string
	// Env is appended to the environment of the current process, in KEY=VALUE form.
	Env []string
	// Shell is the shell that runs commands, passed with -c; bash by default.
	Shell string
}

// Run implements Executor.
func (l Local) Run(ctx context.Context, command string, out io.Writer) (int, error) {
	sh := l.Shell
	if sh == "" {
		sh = "bash"
	}
	cmd := exec.CommandContext(ctx, sh, "-c", command)
	cmd.Dir = l.Dir
	if len(l.Env) > 0 {
		cmd.Env = append(os.Environ(), l.Env...)
	}
	// stdout and stderr share one Writer; exec guarantees only one goroutine writes at a time.
	cmd.Stdout, cmd.Stderr = out, out
	// After the command exits, background children may still hold the output pipe; stop reading after waiting at most this long.
	cmd.WaitDelay = 2 * time.Second
	killProcessGroup(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return cmd.ProcessState.ExitCode(), nil
	}
	return -1, err
}
