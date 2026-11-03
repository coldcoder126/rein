package skill

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

const skillFile = "SKILL.md"

// Dir returns a source that reads skills from a local directory; see FS.
func Dir(dir string) Source {
	return FS(os.DirFS(dir), ".")
}

// FS returns a source that reads skills from an fs.FS, compatible with go:embed.
//
// Every directory under root that contains a SKILL.md is a skill, and directories may be nested into groups. The frontmatter at
// the top of SKILL.md gives name and description (the directory name when there is no name), the rest is the skill's instructions,
// and the other files in the directory are its resources. Files are read again on every call, so changes take effect on the next run.
func FS(fsys fs.FS, root string) Source {
	return fsSource{fsys: fsys, root: root}
}

type fsSource struct {
	fsys fs.FS
	root string
}

// entry is one skill found by the scan.
type entry struct {
	skill Skill
	dir   string
}

// scan walks root and returns all skills indexed by name.
func (s fsSource) scan() (map[string]entry, error) {
	entries := make(map[string]entry)
	err := fs.WalkDir(s.fsys, s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != skillFile {
			return nil
		}
		data, err := fs.ReadFile(s.fsys, p)
		if err != nil {
			return fmt.Errorf("skill: read %s: %w", p, err)
		}
		dir := path.Dir(p)
		name, description, body := parseFrontmatter(string(data))
		if name == "" {
			name = path.Base(dir)
		}
		entries[name] = entry{dir: dir, skill: Skill{Name: name, Description: description, Instructions: strings.TrimSpace(body)}}
		return nil
	})
	return entries, err
}

func (s fsSource) List(context.Context) ([]Info, error) {
	entries, err := s.scan()
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, Info{Name: e.skill.Name, Description: e.skill.Description})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (s fsSource) Load(_ context.Context, name string) (Skill, error) {
	entries, err := s.scan()
	if err != nil {
		return Skill{}, err
	}
	e, ok := entries[strings.TrimSpace(name)]
	if !ok {
		return Skill{}, ErrNotFound
	}
	// Resources are the files in the skill directory other than SKILL.md, excluding other skills nested inside it.
	err = fs.WalkDir(s.fsys, e.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != e.dir {
				if _, statErr := fs.Stat(s.fsys, path.Join(p, skillFile)); statErr == nil {
					return fs.SkipDir
				}
			}
			return nil
		}
		if rel := strings.TrimPrefix(strings.TrimPrefix(p, e.dir), "/"); rel != skillFile {
			e.skill.Resources = append(e.skill.Resources, rel)
		}
		return nil
	})
	return e.skill, err
}

func (s fsSource) ReadResource(_ context.Context, name, resource string) ([]byte, error) {
	entries, err := s.scan()
	if err != nil {
		return nil, err
	}
	e, ok := entries[strings.TrimSpace(name)]
	// fs.ValidPath rejects absolute paths and "..", so reads cannot leave the skill directory.
	if !ok || !fs.ValidPath(resource) || resource == "." {
		return nil, ErrNotFound
	}
	data, err := fs.ReadFile(s.fsys, path.Join(e.dir, resource))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

// parseFrontmatter parses the YAML frontmatter (--- ... ---) at the top of SKILL.md, taking only name and description.
// Without frontmatter the whole file is the body.
func parseFrontmatter(content string) (name, description, body string) {
	if !strings.HasPrefix(content, "---") {
		return "", "", content
	}
	nl := strings.IndexByte(content, '\n')
	if nl < 0 {
		return "", "", content
	}
	rest := content[nl+1:]
	const closing = "\n---"
	end := strings.Index(rest, closing)
	if end < 0 {
		return "", "", content
	}
	body = strings.TrimPrefix(rest[end+len(closing):], "\n")
	scanner := bufio.NewScanner(strings.NewReader(rest[:end]))
	for scanner.Scan() {
		k, v, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			name = strings.TrimSpace(v)
		case "description":
			description = strings.TrimSpace(v)
		}
	}
	return name, description, body
}
