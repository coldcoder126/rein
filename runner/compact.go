package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

// CompactInput is the input of one compaction decision.
type CompactInput struct {
	// Messages are the session messages currently in the model context, with earlier summaries applied:
	// when there is a summary it comes first, followed by the kept messages and those added since.
	Messages []session.Message
	// Tokens is the estimated context usage, and MaxInputTokens the model's context limit.
	Tokens         int
	MaxInputTokens int
	// Reason says how this compaction was triggered.
	Reason CompactReason
	// Instructions are the caller's extra requirements for the summary in a manual compaction; may be empty.
	Instructions string
	// Client is the model client of the session, which can be used to generate the summary.
	Client chat.Client
	// Continue sends the conversation as the model last saw it, followed by extra, as a side call of the Agent (see agent.SideCall):
	// the same system prompt, tools, options and messages, through the same middlewares. Summarizing through it reuses the
	// conversation's prompt cache. The conversation covers Messages up to and including ContinueThrough, so the kept part must
	// start no later than the message after it. Continue is nil when there is no such conversation, for example when no model call
	// has succeeded since the last summary.
	Continue        func(ctx context.Context, extra []chat.Message, opts agent.SideCallOptions) (*chat.Message, error)
	ContinueThrough string
}

// CompactReason says how a compaction was triggered.
type CompactReason string

const (
	// CompactAuto is the routine check before a run and after each turn; the Compactor decides from the usage whether to compact.
	CompactAuto CompactReason = "auto"
	// CompactOverflow means the model already failed because the context was exceeded; compact as much as possible regardless of the estimate.
	CompactOverflow CompactReason = "overflow"
	// CompactManual means the caller asked for compaction through Runner.Compact; compact as much as possible regardless of the estimate.
	// MaxInputTokens is 0 when the model declares no context limit.
	CompactManual CompactReason = "manual"
)

// CompactResult describes a compaction: the messages before FirstKeptID are replaced by Summary.
type CompactResult struct {
	Summary string
	// FirstKeptID is the ID of the first kept message and must come from CompactInput.Messages.
	// It cannot be a tool result message, or its tool call would be summarized away and leave an orphan result.
	FirstKeptID string
	// Usage is the tokens consumed generating the summary, counted in the run result.
	Usage chat.Usage
}

// Compactor decides when and how to compact session history. Returning nil means no compaction this time.
//
// The Runner calls it automatically at three points: before each run, after each turn of a run, and when the model reports a context overflow;
// callers can also trigger it manually with Runner.Compact; see CompactInput.Reason.
// The original messages always stay in the store; compaction only affects what enters the model context.
type Compactor interface {
	Compact(ctx context.Context, in CompactInput) (*CompactResult, error)
}

const (
	defaultTriggerRatio     = 0.8
	defaultKeepRatio        = 0.2
	defaultSummaryMaxTokens = 4096
	// summaryItemLimit is how many bytes of each message are kept in the summary input, so huge tool results cannot blow up the summary request.
	summaryItemLimit = 4000
	mediaTokens      = 1500
)

const defaultSummaryPrompt = `Summarize the conversation so far so that the assistant can continue the work with only this summary in place of the earlier messages.

Write a concise but complete summary that preserves:
- the user's goals, requirements and preferences
- key facts, decisions and conclusions reached, including exact values, names and identifiers
- what tools were used and their important results
- what is still in progress or pending

Write the summary in the language the user has been using. Output only the summary.`

// continueNote is added to the instructions when the summary is asked for within the conversation, where tools are still offered.
const continueNote = `The most recent messages will stay visible after the summary, so focus on what comes before them.
Do not call any tools: reply with the summary text only.`

// SummaryCompactor is the default compaction strategy: when context usage exceeds a threshold, it keeps the most recent messages
// as they are and has the model summarize the earlier part. The cut prefers user messages and never separates a tool call from its result.
// The zero value works.
type SummaryCompactor struct {
	// TriggerRatio is the context usage ratio that triggers compaction, 0.8 by default.
	TriggerRatio float64
	// KeepRatio is the share of the context limit kept as recent messages after compaction, 0.2 by default.
	KeepRatio float64
	// Prompt is the instruction for generating the summary; an empty one uses the built-in prompt.
	Prompt string
	// Client is the model that generates the summary; empty uses the session's model. A cheaper model can be used.
	//
	// With the session's model the summary is asked for at the end of the conversation through CompactInput.Continue,
	// reusing its prompt cache. Otherwise, or when Continue is nil, the messages are sent as a plain text transcript
	// with Prompt as the system prompt, which no cache covers.
	Client chat.Client
	// Options are the parameters of the summary request. MaxTokens of 0 becomes 4096 and applies to both ways; the other
	// fields only apply to the transcript, as the conversation keeps its own. There an empty CacheRetention disables caching,
	// since the transcript is used only once.
	Options chat.Options
}

func (c SummaryCompactor) Compact(ctx context.Context, in CompactInput) (*CompactResult, error) {
	trigger, keep := c.TriggerRatio, c.KeepRatio
	if trigger <= 0 {
		trigger = defaultTriggerRatio
	}
	if keep <= 0 {
		keep = defaultKeepRatio
	}
	force := in.Reason != CompactAuto
	if !force && float64(in.Tokens) < trigger*float64(in.MaxInputTokens) {
		return nil, nil
	}
	cut := cutIndex(in.Messages, int(keep*float64(in.MaxInputTokens)), force)
	conversation := in.Continue != nil && c.Client == nil
	if conversation {
		// The summary covers the conversation only up to ContinueThrough; a later cut would drop the messages in between.
		if i := slices.IndexFunc(in.Messages, func(m session.Message) bool { return m.ID == in.ContinueThrough }); i >= 0 {
			cut = min(cut, i+1)
		}
		for cut > 0 && cut < len(in.Messages) && in.Messages[cut].Type == session.TypeTool {
			cut--
		}
	}
	if cut <= 0 {
		return nil, nil
	}

	prompt := c.Prompt
	if prompt == "" {
		prompt = defaultSummaryPrompt
	}
	if text := strings.TrimSpace(in.Instructions); text != "" {
		prompt += "\n\nAdditional instructions from the user for this summary:\n" + text
	}
	options := c.Options
	if options.MaxTokens <= 0 {
		options.MaxTokens = defaultSummaryMaxTokens
	}
	var msg *chat.Message
	var err error
	if conversation {
		msg, err = in.Continue(ctx, []chat.Message{chat.NewUserTextMessage(prompt + "\n\n" + continueNote)}, agent.SideCallOptions{MaxTokens: options.MaxTokens})
	} else {
		client := c.Client
		if client == nil {
			client = in.Client
		}
		if options.CacheRetention == "" {
			options.CacheRetention = chat.CacheRetentionNone
		}
		msg, err = chat.Complete(ctx, client, chat.Request{
			SystemPrompt: prompt,
			Options:      options,
			Messages:     []chat.Message{chat.NewUserTextMessage(transcript(in.Messages[:cut]))},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("runner: generate summary: %w", err)
	}
	summary := strings.TrimSpace(msg.TextContent())
	if summary == "" {
		return nil, fmt.Errorf("runner: the model returned no summary text")
	}
	return &CompactResult{Summary: summary, FirstKeptID: in.Messages[cut].ID, Usage: msg.Usage}, nil
}

// cutIndex returns where the kept part starts, or 0 when there is nothing to summarize.
//
// It walks back from the end until keepTokens to find a rough position, then prefers to align backward to the nearest user message
// so the kept part starts with a complete exchange; if that would keep more than twice the budget (for example when one question
// led to a long chain of tool calls), it aligns forward to the nearest message that is not a tool result instead.
func cutIndex(messages []session.Message, keepTokens int, force bool) int {
	kept := 0
	start := len(messages)
	for start > 0 && kept < keepTokens {
		start--
		kept += estimateTokens(messages[start])
	}
	if start == 0 || start == len(messages) {
		// By the estimate every message fits within the keep budget, or there is no budget (the model declares no limit).
		// Only when asked to compact as much as possible is it cut in half; otherwise summarizing one or two messages is pointless.
		if !force {
			return 0
		}
		start = len(messages) / 2
	}
	extra := 0
	for i := start; i > 0 && extra <= keepTokens; i-- {
		if messages[i].Type == session.TypeUser {
			return i
		}
		extra += estimateTokens(messages[i-1])
	}
	for ; start < len(messages); start++ {
		if start > 0 && messages[start].Type != session.TypeTool {
			return start
		}
	}
	return 0
}

// transcript writes the messages to summarize as a plain text transcript, so the model does not treat them as a conversation to continue.
func transcript(messages []session.Message) string {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation.\n\n<conversation>\n")
	for _, msg := range messages {
		var parts []string
		for _, block := range msg.Content() {
			switch block.Type {
			case chat.ContentTypeText:
				parts = append(parts, block.Text)
			case chat.ContentTypeMedia:
				parts = append(parts, "[media]")
			case chat.ContentTypeToolCall:
				if block.ToolCall != nil {
					parts = append(parts, fmt.Sprintf("[tool call %s %s]", block.ToolCall.Name, block.ToolCall.Arguments))
				}
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			continue
		}
		if len(text) > summaryItemLimit {
			text = strings.ToValidUTF8(text[:summaryItemLimit], "") + "\n[truncated]"
		}
		label := string(msg.Type)
		if msg.Type == session.TypeSummary {
			label = "earlier summary"
		}
		fmt.Fprintf(&sb, "[%s]\n%s\n\n", label, text)
	}
	sb.WriteString("</conversation>")
	return sb.String()
}

// estimateTokens roughly estimates the tokens of a message at 3 bytes per token: close for Chinese, conservative for English.
func estimateTokens(msg session.Message) int {
	total := 0
	for _, block := range msg.Content() {
		switch block.Type {
		case chat.ContentTypeMedia:
			total += mediaTokens
		case chat.ContentTypeToolCall:
			if block.ToolCall != nil {
				total += (len(block.ToolCall.Name) + len(block.ToolCall.Arguments)) / 3
			}
		default:
			total += len(block.Text) / 3
		}
	}
	return total + 4
}

// effectiveMessages applies the latest summary, returning "summary + the messages it keeps + messages added since"
// and where the added part starts. Without a summary the messages are returned unchanged.
func effectiveMessages(all []session.Message) (out []session.Message, fresh int) {
	last := -1
	for i, msg := range all {
		if msg.Type == session.TypeSummary {
			last = i
		}
	}
	if last < 0 {
		return all, 0
	}
	out = append(out, all[last])
	keeping := false
	for _, msg := range all[:last] {
		keeping = keeping || msg.ID == all[last].FirstKeptID
		if keeping && msg.Type != session.TypeSummary {
			out = append(out, msg)
		}
	}
	fresh = len(out)
	return append(out, all[last+1:]...), fresh
}

// estimateContext estimates context usage. Assistant messages produced after the summary carry real usage; the latest one is the baseline
// and only the messages after it are estimated roughly. Usage from before the summary reflects the size before compaction and cannot be used.
func estimateContext(messages []session.Message, fresh int, systemPrompt string) int {
	for i := len(messages) - 1; i >= fresh; i-- {
		cm := messages[i].ChatMessage
		if messages[i].Type != session.TypeAssistant || cm.Usage.TotalTokens == 0 || cm.StopReason == chat.StopReasonError {
			continue
		}
		total := cm.Usage.TotalTokens
		for _, msg := range messages[i+1:] {
			total += estimateTokens(msg)
		}
		return total
	}
	total := len(systemPrompt) / 3
	for _, msg := range messages {
		total += estimateTokens(msg)
	}
	return total
}

// CompactOptions adjusts a manual compaction.
type CompactOptions struct {
	// Instructions are extra requirements for the summary and may be empty; see CompactInput.Instructions.
	Instructions string
	// Model generates the summary instead of Agent.Model, typically the model the session currently runs on.
	Model chat.Client
}

// Compact immediately compacts the history of an idle session, for when the user asks for it, and returns the stored summary message and
// the tokens consumed generating it; the summary is nil when there is nothing to compact. On error the tokens already consumed are still returned.
//
// Compaction is a run of its own: Current returns it, Events gives run_start, the stored summary and run_end, and Abort cancels it.
// Meanwhile Start returns ErrBusy, and background task results are written to history after it ends.
// It returns ErrBusy while the session is running, and an error when compaction is disabled.
func (r *Runner) Compact(ctx context.Context, sessionID string, opts CompactOptions) (*session.Message, chat.Usage, error) {
	if r.compactor == nil {
		return nil, chat.Usage{}, errors.New("runner: compaction is disabled")
	}
	if r.model(opts.Model) == nil {
		return nil, chat.Usage{}, errNoModel
	}
	ctx, cancel := context.WithCancel(ctx)
	run, err := r.acquire(sessionID, cancel, true)
	if err != nil {
		cancel()
		return nil, chat.Usage{}, err
	}
	var summary *session.Message
	result, err := r.occupation(ctx, run, r.model(opts.Model), func(st *runState) (err error) {
		summary, err = st.compact(ctx, CompactManual, opts.Instructions, st.sideCall, false)
		return err
	})
	return summary, result.Usage, err
}

// sideCaller makes a side call of the Agent with the given messages: agent.SideCall within a run, runState.sideCall outside one.
type sideCaller func(ctx context.Context, messages []chat.Message, opts agent.SideCallOptions) (*chat.Message, error)

// sideCall makes a side call outside a run, as a run of its own set up like process sets up the Agent, so the system prompt and
// tools resolve to what the session's next run would see.
func (st *runState) sideCall(ctx context.Context, messages []chat.Message, opts agent.SideCallOptions) (*chat.Message, error) {
	a := *st.r.agent
	a.Middlewares = slices.Concat(st.r.middlewares, a.Middlewares)
	return a.SideCall(st.services(ctx), agent.Input{RunInfo: st.info, Model: st.model, Messages: messages}, opts)
}

// summaryReserve is the room left for the summary instructions and output when the summary is asked for at the end of the
// conversation, at most a tenth of the context.
const summaryReserve = 8192

// compact compacts the session history when needed and stores the summary, returning the stored summary message, or nil when it did not compact.
// When the model declares no context limit, only manual compaction happens.
//
// The Compactor gets the conversation to summarize through as a side call made with call. latest asks for the conversation up to the
// newest message, as the coming turn would send it; otherwise, or when that would not fit, it ends at the last assistant message
// whose model call succeeded, whose input and output fit within the context and whose input is cached.
func (st *runState) compact(ctx context.Context, reason CompactReason, instructions string, call sideCaller, latest bool) (*session.Message, error) {
	r := st.r
	limit := st.model.Info().MaxInputTokens
	if r.compactor == nil || (limit <= 0 && reason != CompactManual) {
		return nil, nil
	}
	all, err := r.store.ListMessages(ctx, st.info.SessionID)
	if err != nil {
		return nil, err
	}
	messages, fresh := effectiveMessages(all)
	in := CompactInput{
		Messages:       session.CloneMessages(messages),
		Tokens:         estimateContext(messages, fresh, r.agent.Instruction),
		MaxInputTokens: max(limit, 0),
		Reason:         reason,
		Instructions:   instructions,
		Client:         st.model,
	}
	through := len(messages) - 1
	if !latest || (limit > 0 && in.Tokens+min(summaryReserve, limit/10) > limit) {
		for through >= 0 && !succeeded(messages[through]) {
			through--
		}
	}
	if through >= 0 {
		covered := messages[:through+1]
		withContext := latest || covered[len(covered)-1].RunID == st.info.RunID
		in.ContinueThrough = covered[len(covered)-1].ID
		in.Continue = func(ctx context.Context, extra []chat.Message, opts agent.SideCallOptions) (*chat.Message, error) {
			conversation, err := st.chatMessages(ctx, covered, withContext)
			if err != nil {
				return nil, err
			}
			return call(ctx, append(conversation, extra...), opts)
		}
	}
	result, err := r.compactor.Compact(ctx, in)
	if err != nil || result == nil {
		return nil, err
	}
	st.result.Usage = st.result.Usage.Add(result.Usage)
	valid := false
	for _, msg := range messages {
		valid = valid || (msg.ID == result.FirstKeptID && msg.Type != session.TypeTool)
	}
	if !valid {
		return nil, fmt.Errorf("runner: Compactor returned an invalid FirstKeptID %q", result.FirstKeptID)
	}
	summary := session.SummaryMessage(result.Summary, result.FirstKeptID)
	summary.ID = newID("msg")
	if err := st.save(ctx, summary); err != nil {
		return nil, err
	}
	saved := st.result.Messages[len(st.result.Messages)-1]
	return &saved, nil
}

// succeeded reports whether msg is an assistant message whose model call succeeded.
func succeeded(msg session.Message) bool {
	stop := msg.ChatMessage.StopReason
	return msg.Type == session.TypeAssistant && stop != chat.StopReasonError && stop != chat.StopReasonAborted
}

// wrapTurn is the per-turn middleware hooked into the Agent, inside the Agent's own middlewares.
// Before each turn it decides whether to compact, and replaces the history the turn uses when it does; the first turn asks for the
// conversation up to the previous run, as cached by it, and later turns for the conversation so far.
// When a turn fails because the context was exceeded it forces a compaction and lets the loop call the model again with the
// compacted history, recovering at most once per run.
func (st *runState) wrapTurn(ctx context.Context, info agent.TurnInfo, next agent.TurnNext) (*agent.TurnResult, error) {
	summary, err := st.compact(ctx, CompactAuto, "", agent.SideCall, info.TurnIndex > 0)
	if err != nil {
		return nil, err
	}
	if summary != nil {
		messages, err := st.buildMessages(ctx)
		if err != nil {
			return nil, err
		}
		if err := agent.SetMessages(ctx, messages); err != nil {
			return nil, err
		}
	}
	result, err := next(ctx)
	if err != nil || result.Status != agent.StatusFailed || result.Turn.Message.ErrorKind != chat.KindContextLength || st.recovered {
		return result, err
	}
	// The failed assistant message is already stored and is excluded on replay.
	st.recovered = true
	if summary, err = st.compact(ctx, CompactOverflow, "", agent.SideCall, false); err != nil || summary == nil {
		return result, err
	}
	if result.Messages, err = st.buildMessages(ctx); err != nil {
		return nil, err
	}
	result.Status = ""
	return result, nil
}
