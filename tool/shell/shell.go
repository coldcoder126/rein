// Package shell provides the built-in command tool shell: the model can run a command in the foreground and get its output,
// or run it in the background and receive the result as a notice in the session when it ends.
//
// Where commands run is decided by an Executor: this package only provides Local, which runs on this machine; containers, remote sandboxes and so on are implemented by the business layer.
// Running commands hands the execution environment to the model, so production should run them in an isolated environment; use a WrapTool middleware when approval is needed.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

const (
	// ToolName is the name of the command tool.
	ToolName = "shell"
	// TaskType is the tool.Task.Type of background commands.
	TaskType = "shell"
)

// Executor decides where commands run.
type Executor interface {
	// Run runs a command, writes stdout and stderr together to out, and returns the exit code.
	// When ctx is canceled it must end the command and every child process it started, and return the ctx error.
	Run(ctx context.Context, command string, out io.Writer) (exitCode int, err error)
}

// Config configures the command tool.
type Config struct {
	// Executor is the execution environment of commands and is required.
	Executor Executor
	// Timeout is the default timeout of foreground commands, 2 minutes by default.
	Timeout time.Duration
	// MaxTimeout is the largest timeout the model may set for a foreground command, 10 minutes by default. Background commands have no timeout.
	MaxTimeout time.Duration
}

// Details are the Details of a command result: in the tool result for foreground commands, in the task snapshot for background ones.
type Details struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Duration time.Duration `json:"duration"`
}

type params struct {
	Command         string `json:"command" jsonschema:"description=The command to run."`
	Description     string `json:"description,omitempty" jsonschema:"description=A short description of what the command does, shown to the user."`
	Timeout         int    `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for a foreground command."`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"description=Run the command in the background and get notified when it finishes. Use it for long-running commands such as builds or servers."`
}

// Only the start and end of the output are kept, so a command that floods output cannot fill memory and the context.
const (
	headBytes = 8 << 10
	tailBytes = 24 << 10
)

// Tool returns the command tool. A nil Executor is a programming error and panics.
func Tool(cfg Config) tool.Tool {
	if cfg.Executor == nil {
		panic("shell: Config.Executor must not be nil")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = 10 * time.Minute
	}
	cfg.Timeout = min(cfg.Timeout, cfg.MaxTimeout)
	s := shell{cfg: cfg}
	description := fmt.Sprintf(`Run a shell command and get its combined stdout and stderr with the exit code.
Each call starts in the same working directory; a cd does not carry over to the next call.
Foreground commands time out after %s by default (at most %s). Long output keeps only its beginning and end.
Set run_in_background for long-running commands: it returns a task id at once, and the result is sent to you
as a <task_result> message when the command finishes. Use the task tool to check its output or stop it.`,
		cfg.Timeout, cfg.MaxTimeout)
	// The tool controls its own timeout; the Agent's generic timeout leaves headroom so it does not give up first.
	return tool.New(ToolName, description, s.execute, tool.WithTimeout(cfg.MaxTimeout+30*time.Second))
}

type shell struct{ cfg Config }

func (s shell) execute(ctx *tool.Context, in params) (*tool.Result, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, errors.New("command is empty")
	}
	if in.RunInBackground {
		return s.background(ctx, in)
	}
	timeout := s.cfg.Timeout
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Second, s.cfg.MaxTimeout)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &output{report: func(p string) { _ = ctx.Update(tool.Update{Content: []chat.ContentBlock{chat.Text(p)}}) }}
	start := time.Now()
	code, err := s.cfg.Executor.Run(runCtx, in.Command, out)
	details := Details{Command: in.Command, ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		if ctx.Err() != nil || !errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, err
		}
		details.TimedOut = true
	}
	result := tool.Text(out.render(details, timeout))
	result.Details = details
	result.IsError = details.TimedOut || code != 0
	return result, nil
}

func (s shell) background(ctx *tool.Context, in params) (*tool.Result, error) {
	tasks := tool.TaskManagerFrom(ctx)
	if tasks == nil {
		return nil, errors.New("background execution is not available here; run the command in the foreground")
	}
	desc := in.Description
	if desc == "" {
		desc = in.Command
	}
	task, err := tasks.Start(ctx, tool.TaskSpec{
		Type:        TaskType,
		Description: desc,
		Run: func(ctx context.Context, report func(tool.TaskUpdate)) (tool.TaskResult, error) {
			out := &output{report: func(p string) { report(tool.TaskUpdate{Output: p}) }}
			start := time.Now()
			code, err := s.cfg.Executor.Run(ctx, in.Command, out)
			details := Details{Command: in.Command, ExitCode: code, Duration: time.Since(start)}
			result := tool.TaskResult{Text: out.render(details, 0), Details: details}
			if err == nil && code != 0 {
				err = fmt.Errorf("exit code %d", code)
			}
			return result, err
		},
	})
	if err != nil {
		return nil, err
	}
	result := tool.Text(fmt.Sprintf("Command started in the background (task id: %s). Its result will be sent to you "+
		"as a <task_result> message when it finishes; use the task tool to check its output or stop it.", task.ID))
	result.Details = task
	return result, nil
}

// output collects command output: it keeps the start and the end, and passes every new chunk to report. It may be written concurrently.
type output struct {
	mu      sync.Mutex
	report  func(string)
	head    []byte
	tail    []byte
	omitted int
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	rest := p
	if room := headBytes - len(o.head); room > 0 {
		k := min(room, len(rest))
		o.head = append(o.head, rest[:k]...)
		rest = rest[k:]
	}
	o.tail = append(o.tail, rest...)
	if over := len(o.tail) - tailBytes; over > 0 {
		o.omitted += over
		o.tail = append(o.tail[:0:0], o.tail[over:]...)
	}
	o.mu.Unlock()
	if o.report != nil {
		o.report(strings.ToValidUTF8(string(p), "�"))
	}
	return len(p), nil
}

// render builds the result for the model: the output plus the exit code or a timeout note.
func (o *output) render(d Details, timeout time.Duration) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var sb strings.Builder
	sb.Write(o.head)
	if o.omitted > 0 {
		fmt.Fprintf(&sb, "\n... [%d bytes omitted] ...\n", o.omitted)
	}
	sb.Write(o.tail)
	text := strings.ToValidUTF8(strings.TrimRight(sb.String(), "\n"), "�")
	if text == "" {
		text = "(no output)"
	}
	if d.TimedOut {
		return text + fmt.Sprintf("\n[timed out after %s; the command was killed]", timeout)
	}
	return text + fmt.Sprintf("\n[exit code: %d]", d.ExitCode)
}
