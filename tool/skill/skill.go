// Package skill provides skills: focused instructions for the model, optionally with resource files.
//
// To save context, the system prompt only lists the skill catalog (names and summaries); when the model needs a skill it loads
// its full instructions with a tool, then reads its resource files as needed. A Source decides where skills come from: this package
// provides a local directory, an fs.FS and memory, and a database or remote service can implement the same interface. NewToolset turns a source into a toolset for an Agent.
package skill

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// ErrNotFound means the requested skill or resource does not exist or is not visible now.
var ErrNotFound = errors.New("skill: not found")

// Info is a skill catalog entry visible to the model.
type Info struct {
	Name        string
	Description string
}

// Skill is the complete content of a skill.
type Skill struct {
	Name        string
	Description string
	// Instructions are the skill's full instructions, which only enter the context after the model loads the skill.
	Instructions string
	// Resources are the paths of the skill's resource files (relative to the skill root, separated by /); read their content with ReadResource.
	Resources []string
}

// Source is where skills come from. The business layer can implement it to apply permissions, publishing state or scenario filters,
// or to read from a database or object storage. Its methods may be called concurrently.
type Source interface {
	// List returns the skill catalog the model may currently see. It is called once at the start of each run.
	List(ctx context.Context) ([]Info, error)
	// Load returns the complete content of a skill, or ErrNotFound when it does not exist or is not visible.
	Load(ctx context.Context, name string) (Skill, error)
	// ReadResource reads one resource file of a skill, or returns ErrNotFound when it does not exist.
	ReadResource(ctx context.Context, name, path string) ([]byte, error)
}

// Static returns a source made of skills in memory; on duplicate names the later one wins. These skills have no resource files.
func Static(skills ...Skill) Source {
	s := staticSource{}
	for _, sk := range skills {
		sk.Resources = nil
		s[sk.Name] = sk
	}
	return s
}

type staticSource map[string]Skill

func (s staticSource) List(context.Context) ([]Info, error) {
	infos := make([]Info, 0, len(s))
	for _, sk := range s {
		infos = append(infos, Info{Name: sk.Name, Description: sk.Description})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (s staticSource) Load(_ context.Context, name string) (Skill, error) {
	sk, ok := s[strings.TrimSpace(name)]
	if !ok {
		return Skill{}, ErrNotFound
	}
	return sk, nil
}

func (staticSource) ReadResource(context.Context, string, string) ([]byte, error) {
	return nil, ErrNotFound
}
