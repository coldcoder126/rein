package skill

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/coldcoder126/rein/tool"
)

const (
	// LoadToolName is the name of the tool that loads skill instructions; ReadResourceToolName is the name of the tool that reads skill resource files.
	LoadToolName         = "load_skill"
	ReadResourceToolName = "read_skill_resource"
)

// NewToolset wraps a skill source as a toolset; add it to Agent.Toolsets to use it.
//
// It appends the skill catalog to the system prompt and provides two tools: load_skill loads a skill's full instructions and resource list,
// and read_skill_resource reads one of the resource files. When the source has no visible skills, it provides no tools and no instructions.
func NewToolset(src Source) tool.Toolset {
	return toolset{src: src}
}

type toolset struct {
	src Source
}

type loadParams struct {
	Name string `json:"name" jsonschema:"description=Skill name from available_skills."`
}

type resourceParams struct {
	Name string `json:"name" jsonschema:"description=Skill name from available_skills."`
	Path string `json:"path" jsonschema:"description=Resource path exactly as listed when the skill was loaded."`
}

func (ts toolset) Tools(ctx context.Context, _ tool.RunInfo) ([]tool.Tool, error) {
	infos, err := ts.src.List(ctx)
	if err != nil || len(infos) == 0 {
		return nil, err
	}
	load := tool.New(LoadToolName, "Load the full instructions of a skill listed in available_skills before using that skill.",
		func(ctx *tool.Context, in loadParams) (*tool.Result, error) {
			s, err := ts.src.Load(ctx, in.Name)
			if err != nil {
				return nil, ts.lookupError(ctx, in.Name, err)
			}
			result := tool.Text(format(s))
			result.Details = map[string]any{"skill": s.Name}
			return result, nil
		})
	read := tool.New(ReadResourceToolName, "Read a resource file that belongs to a loaded skill.",
		func(ctx *tool.Context, in resourceParams) (*tool.Result, error) {
			data, err := ts.src.ReadResource(ctx, in.Name, in.Path)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("resource %q not found in skill %q; load the skill to see its resources", in.Path, in.Name)
				}
				return nil, err
			}
			if !utf8.Valid(data) {
				return nil, fmt.Errorf("resource %q is a binary file and cannot be shown as text", in.Path)
			}
			return tool.Text(string(data)), nil
		})
	return []tool.Tool{load, read}, nil
}

// Instructions builds the skill catalog for the system prompt, without the skills' full instructions.
func (ts toolset) Instructions(ctx context.Context, _ tool.RunInfo) (string, error) {
	infos, err := ts.src.List(ctx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, s := range infos {
		if strings.TrimSpace(s.Name) == "" {
			continue
		}
		fmt.Fprintf(&sb, "  <skill>\n    <name>%s</name>\n    <description>%s</description>\n  </skill>\n", escape(s.Name), escape(s.Description))
	}
	if sb.Len() == 0 {
		return "", nil
	}
	return "The following skills provide specialized instructions for specific tasks.\n" +
		"When a user request matches a skill, call the " + LoadToolName + " tool to load its full instructions before answering.\n\n" +
		"<available_skills>\n" + sb.String() + "</available_skills>", nil
}

// lookupError builds the error text for the model, listing the available skill names so it can correct itself.
func (ts toolset) lookupError(ctx context.Context, name string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	message := fmt.Sprintf("skill %q is unavailable", name)
	if errors.Is(err, ErrNotFound) {
		message = fmt.Sprintf("unknown skill %q", name)
	}
	if infos, listErr := ts.src.List(ctx); listErr == nil && len(infos) > 0 {
		names := make([]string, 0, len(infos))
		for _, info := range infos {
			names = append(names, info.Name)
		}
		message += ". available skills: " + strings.Join(names, ", ")
	}
	return errors.New(message)
}

func format(s Skill) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill name=\"%s\">\n", escape(s.Name))
	if strings.TrimSpace(s.Description) != "" {
		fmt.Fprintf(&sb, "<description>%s</description>\n\n", escape(s.Description))
	}
	sb.WriteString(strings.TrimRight(s.Instructions, "\n"))
	sb.WriteString("\n")
	if len(s.Resources) > 0 {
		sb.WriteString("\n<resources hint=\"read with " + ReadResourceToolName + "\">\n")
		for _, r := range s.Resources {
			fmt.Fprintf(&sb, "%s\n", escape(r))
		}
		sb.WriteString("</resources>\n")
	}
	sb.WriteString("</skill>")
	return sb.String()
}

func escape(value string) string {
	var sb strings.Builder
	_ = xml.EscapeText(&sb, []byte(value))
	return sb.String()
}
