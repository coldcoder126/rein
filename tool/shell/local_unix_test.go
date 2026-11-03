//go:build unix

package shell_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coldcoder126/rein/tool/shell"
)

// Canceling should end the whole process group; child processes started by the command must not survive.
func TestLocalKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out strings.Builder
	start := time.Now()
	_, err := shell.Local{}.Run(ctx, `sleep 30 & echo $!; wait`, &out)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("should return the ctx error soon after canceling: %v %s", err, time.Since(start))
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(out.String()))
	if convErr != nil {
		t.Fatalf("no child process id: %q", out.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child process %d was not ended", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
