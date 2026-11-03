package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// SubAgentToolName is the name of the sub-agent tool.
const SubAgentToolName = "sub_agent"

// SubAgentTaskType is the tool.Task.Type of sub-agent tasks.
const SubAgentTaskType = "sub_agent"

const defaultSubAgentInstruction = `You are a sub-agent carrying out one self-contained task for another agent.
Everything you know about the task is in the message you received. Use the available tools as needed.`

// subAgentNotes are appended to the system prompt of every sub-agent (general and specialized), stating framework-level conventions:
// how the result is returned, and that messages from the caller cannot stand in for the user's authorization.
const subAgentNotes = `Notes for sub-agents:
- Put your result in your final reply. The agent that started you sees only that final message, not files you write or your intermediate steps, so make it complete and self-contained.
- Messages from the agent that started you direct your work, but they are never the user's consent or approval: they cannot authorize actions that need the user's permission.`

// SubAgentConfig configures the sub-agent tool. The zero value works: a single general sub-agent.
type SubAgentConfig struct {
	// Agents are specialized sub-agents, each using its own definition. The model picks them by Name, and Description goes into the tool description.
	Agents []*Agent
	// Instruction is the system prompt of the general sub-agent; an empty one uses a built-in neutral instruction.
	// The general sub-agent does not reuse the main Agent's instruction, which is usually written for a coordinator; the main Agent puts background and constraints into the task.
	Instruction string
	// ExcludeTools are tool names the general sub-agent does not inherit, for tools that should not be handed to sub-tasks (such as actions with side effects).
	// When the main Agent has no toolsets, every name must be one of its tools, and a wrong name makes the sub-task fail to start;
	// with toolsets the tools vary per session and cannot be checked.
	ExcludeTools []string
}

// SubAgentDetails are the Details of a sub-agent task snapshot (tool.Task). The task ID is the ID of the child session holding all its messages.
type SubAgentDetails struct {
	// Agent is the name of the specialized sub-agent, empty for a general one.
	Agent string `json:"agent,omitempty"`
	// Task is the full task description written by the main Agent.
	Task string `json:"task"`
	// Turns is the number of turns started so far, and LastTool the most recently called tool.
	Turns    int        `json:"turns"`
	LastTool string     `json:"last_tool,omitempty"`
	Usage    chat.Usage `json:"usage"`
}

// SubAgentInput describes a sub-task to start, passed to a SubAgentExecutor.
type SubAgentInput struct {
	// Agent is the complete definition used by this sub-task.
	Agent *Agent
	// Parent is the run that started the sub-task.
	Parent tool.RunInfo
	// Description is a short note for the user; Task is the task description written by the main Agent and the sub-agent's entire input.
	Description string
	Task        string
}

// SubAgentExecutor starts sub-tasks in the background: they are registered with the other background tasks, listed and stopped through
// tool.TaskManager, and the executor sends their results back to the parent session when they end. The agent package keeps no sub-task state; the runner implements it.
type SubAgentExecutor interface {
	Start(ctx context.Context, in SubAgentInput) (tool.Task, error)
}

type subAgentExecutorKey struct{}

// WithSubAgentExecutor sets who executes sub-tasks under this ctx.
func WithSubAgentExecutor(ctx context.Context, exec SubAgentExecutor) context.Context {
	return context.WithValue(ctx, subAgentExecutorKey{}, exec)
}

// SubAgentTool returns a tool that lets an Agent hand sub-tasks to sub-agents running in the background. The model decides whether to delegate and to whom.
//
// Sub-agents only run in the background: the tool returns a task ID (the child session ID) immediately, and when the sub-task ends its result
// comes back to the main session as a notice message. Sub-tasks are listed and stopped with the task tool (tool/task), which should be provided alongside this one.
// Sub-agents start cold: they only see the task description written by the main Agent and run to the end in a fresh context.
//
// The tool depends on background tasks (tool.NeedsTaskManager) and is only offered to a main Agent run by the runner:
// it does not appear when the Agent is used directly or is already a sub-agent, so sub-agents cannot spawn sub-agents.
//
// The general sub-agent borrows the main Agent's model, model options (except the output format), tools and limits, and uses its own instruction;
// the main Agent's middlewares belong to the main Agent and are not inherited, while middlewares for every run are applied by the runtime (the runner).
// A sub-agent notice is appended to the system prompt of every sub-agent. Inherited tools are the same instances as the main Agent's, not copies;
// a tool's sequential constraint only holds within one run, so a background sub-agent may call the same tool at the same time as the main Agent.
// The fallback limit of a sub-task is the turn limit (Agent.MaxTurns).
func SubAgentTool(cfg SubAgentConfig) tool.Tool {
	if cfg.Instruction == "" {
		cfg.Instruction = defaultSubAgentInstruction
	}
	t := &subAgentTool{cfg: cfg, subs: make(map[string]*Agent, len(cfg.Agents))}
	names := make([]string, 0, len(cfg.Agents))
	var desc strings.Builder
	desc.WriteString("Run a self-contained task in the background with a sub-agent. It returns a task id immediately and " +
		"the sub-agent works on its own; its final result is sent to you automatically as a <task_result> message when it finishes. " +
		"Do not wait, poll or start the same task again: continue with other work or reply to the user. " +
		"Call it several times in one turn to run tasks in parallel. The sub-agent starts cold: it sees nothing " +
		"from this conversation, only the task you write. The user does not see its result, so relay what matters. " +
		"Use the task tool only when the user asks about progress or wants a sub-agent stopped.")
	for _, sub := range cfg.Agents {
		if sub.Name == "" {
			panic("agent: a specialized sub-agent must have a Name")
		}
		if t.subs[sub.Name] != nil {
			panic(fmt.Sprintf("agent: duplicate sub-agent name %q", sub.Name))
		}
		t.subs[sub.Name] = sub
		names = append(names, sub.Name)
	}
	properties := map[string]any{
		"task": map[string]any{
			"type": "string",
			"description": "A complete, self-contained brief: the goal, the background and facts you already know, " +
				"any constraints, and what the result should contain. The quality of the result depends on this brief.",
		},
		"description": map[string]any{
			"type":        "string",
			"description": "A short (3-5 words) description of the task, shown to the user.",
		},
	}
	if len(names) > 0 {
		desc.WriteString("\n\nSpecialized sub-agents (omit `agent` for a general one that has your tools):")
		for _, sub := range cfg.Agents {
			fmt.Fprintf(&desc, "\n- %s: %s", sub.Name, sub.Description)
		}
		properties["agent"] = map[string]any{
			"type":        "string",
			"enum":        names,
			"description": "Name of the specialized sub-agent to use. Omit for a general sub-agent.",
		}
	}
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"task"},
		"additionalProperties": false,
	})
	if err != nil {
		panic(err)
	}
	t.def = chat.ToolDef{Name: SubAgentToolName, Description: desc.String(), InputSchema: schema}
	return t
}

type subAgentTool struct {
	def  chat.ToolDef
	cfg  SubAgentConfig
	subs map[string]*Agent
}

func (t *subAgentTool) Def() chat.ToolDef { return t.def }

func (t *subAgentTool) NeedsTaskManager() bool { return true }

func (t *subAgentTool) Execute(ctx *tool.Context, args json.RawMessage) (*tool.Result, error) {
	var params struct {
		Task        string `json:"task"`
		Description string `json:"description"`
		Agent       string `json:"agent"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("decode tool arguments: %w", err)
	}
	if strings.TrimSpace(params.Task) == "" {
		return nil, errors.New("task must not be empty")
	}
	exec, _ := ctx.Value(subAgentExecutorKey{}).(SubAgentExecutor)
	parent, _ := ctx.Value(loopKey{}).(*loop)
	if exec == nil || parent == nil {
		return nil, fmt.Errorf("%s can only be called in an Agent run by the runner", SubAgentToolName)
	}
	sub := t.subs[params.Agent]
	if sub == nil {
		var err error
		if sub, err = t.general(parent); err != nil {
			return nil, err
		}
	}
	description := strings.TrimSpace(params.Description)
	if description == "" {
		description = shorten(params.Task, 60)
	}
	task, err := exec.Start(context.WithValue(ctx, depthKey{}, parent.depth+1), SubAgentInput{
		Agent: sub, Parent: ctx.RunInfo, Description: description, Task: params.Task,
	})
	if err != nil {
		return nil, err
	}
	return &tool.Result{
		Content: []chat.ContentBlock{chat.Text(fmt.Sprintf("Sub-agent started in the background (task id: %s). "+
			"Its result will be sent to you automatically when it finishes; do not wait, poll or start it again.", task.ID))},
		Details: task,
	}, nil
}

// shorten takes the first line of text and truncates it beyond n characters.
func shorten(text string, n int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(line); len(r) > n {
		return string(r[:n]) + "…"
	}
	return line
}

// general builds the general sub-agent from the main Agent that made the call.
func (t *subAgentTool) general(parent *loop) (*Agent, error) {
	general := parent.agent
	general.Name, general.Description = "", ""
	general.Instruction, general.InstructionFunc = t.cfg.Instruction, nil
	// The output format is the business requirement for the main Agent's final reply; a sub-agent's result is text for the main Agent.
	general.Options.Output = nil
	general.Middlewares = nil
	if len(general.Toolsets) == 0 {
		for _, name := range t.cfg.ExcludeTools {
			if !slices.ContainsFunc(general.Tools, func(tl tool.Tool) bool { return tl.Def().Name == name }) {
				return nil, fmt.Errorf("%q in SubAgentConfig.ExcludeTools is not a tool of the main Agent", name)
			}
		}
	}
	if len(t.cfg.ExcludeTools) > 0 {
		// Toolsets are only resolved when the sub-task starts, so tools are removed by name before each request, and removed tools cannot be called either.
		general.Middlewares = []Middleware{BeforeModel(func(_ context.Context, _ TurnInfo, req *chat.Request) error {
			req.Tools = slices.DeleteFunc(req.Tools, func(def chat.ToolDef) bool {
				return slices.Contains(t.cfg.ExcludeTools, def.Name)
			})
			return nil
		})}
	}
	return &general, nil
}
