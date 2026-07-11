// Package theme loads UI color themes: the frontend's CSS custom
// properties as data. Two themes ship built in (agentchat-dark,
// agentchat-light); users drop JSON files into <data>/themes/ to override
// a built-in by name or to add new themes that extend one via "base".
//
// A theme file:
//
//	{
//	  "name": "my-light",              // optional; defaults to the filename
//	  "base": "agentchat-light",       // optional; see resolution below
//	  "vars": { "ink": "#f2efe9", "accent... }
//	}
//
// Resolution: a theme's vars overlay its base's. "base" defaults to the
// same-named built-in when one exists (override use case), else to
// agentchat-dark — so user themes are always complete. Values must look
// like CSS colors (hex, rgb[a](), hsl[a](), or a bare color name);
// anything else is rejected loudly at load time. Unknown JSON keys are
// tolerated. Fonts and layout are not themeable.
package theme

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed themes/*.json
var builtinFS embed.FS

// BuiltinDefault is the theme applied when nothing is configured.
const BuiltinDefault = "agentchat-dark"

// builtinOrder fixes the listing order of built-in themes.
var builtinOrder = []string{"agentchat-dark", "agentchat-light"}

// RequiredVars are the CSS custom properties (without the leading "--")
// the frontend styles with. Every resolved theme defines all of them;
// the built-ins are tested for completeness against this list.
var RequiredVars = []string{
	"ink", "panel", "panel-2", "line", "text", "muted", "focus", "danger",
	"agent-claude", "agent-codex", "agent-aider", "agent-swival", "agent-echo",
}

// Theme is one parsed theme file.
type Theme struct {
	Name string            `json:"name,omitempty"`
	Base string            `json:"base,omitempty"`
	Vars map[string]string `json:"vars"`
}

// Info describes a listed theme.
type Info struct {
	Name   string `json:"name"`
	Source string `json:"source"` // "built-in" or "user"
}

// Set holds the built-in themes plus the user's.
type Set struct {
	builtin map[string]Theme
	user    map[string]Theme
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	varRe   = regexp.MustCompile(`^[a-z0-9-]+$`)
	colorRe = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]+|(rgb|rgba|hsl|hsla)\([0-9deg,.%\s/]+\))$`)
)

// Load parses the built-in themes and any user themes in userDir
// (typically <data>/themes; a missing directory is fine, a malformed or
// invalid theme file is a loud error naming the file).
func Load(userDir string) (*Set, error) {
	s := &Set{builtin: map[string]Theme{}, user: map[string]Theme{}}

	entries, err := fs.Glob(builtinFS, "themes/*.json")
	if err != nil {
		return nil, err
	}
	for _, path := range entries {
		b, err := builtinFS.ReadFile(path)
		if err != nil {
			return nil, err
		}
		t, err := parse(b, strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			return nil, fmt.Errorf("theme: built-in %s: %w", path, err)
		}
		s.builtin[t.Name] = t
	}
	for _, name := range builtinOrder {
		if _, ok := s.builtin[name]; !ok {
			return nil, fmt.Errorf("theme: built-in %q missing from embed", name)
		}
	}

	if userDir != "" {
		files, err := os.ReadDir(userDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(userDir, f.Name()))
			if err != nil {
				return nil, err
			}
			t, err := parse(b, strings.TrimSuffix(f.Name(), ".json"))
			if err != nil {
				return nil, fmt.Errorf("theme: %s: %w", filepath.Join(userDir, f.Name()), err)
			}
			s.user[t.Name] = t
		}
	}
	return s, nil
}

func parse(b []byte, fallbackName string) (Theme, error) {
	var t Theme
	if err := json.Unmarshal(b, &t); err != nil {
		return t, err
	}
	if t.Name == "" {
		t.Name = fallbackName
	}
	if !nameRe.MatchString(t.Name) {
		return t, fmt.Errorf("invalid theme name %q", t.Name)
	}
	if len(t.Vars) == 0 {
		return t, errors.New(`no "vars" defined`)
	}
	for k, v := range t.Vars {
		if !varRe.MatchString(k) {
			return t, fmt.Errorf("invalid variable name %q", k)
		}
		if !colorRe.MatchString(v) {
			return t, fmt.Errorf("variable %q: %q is not a color (hex, rgb[a](), hsl[a](), or a color name)", k, v)
		}
	}
	return t, nil
}

// List returns the available themes: built-ins in fixed order, then
// user-only themes sorted by name. A user file that overrides a built-in
// keeps the built-in's position but reports source "user".
func (s *Set) List() []Info {
	var out []Info
	for _, name := range builtinOrder {
		src := "built-in"
		if _, ok := s.user[name]; ok {
			src = "user"
		}
		out = append(out, Info{Name: name, Source: src})
	}
	var extra []string
	for name := range s.user {
		if _, ok := s.builtin[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, Info{Name: name, Source: "user"})
	}
	return out
}

// Resolve returns a theme's full variable map: base vars overlaid with
// the theme's own. Unknown names error; a resolved theme missing any
// RequiredVars errors (cannot happen for pure overrides of a built-in).
func (s *Set) Resolve(name string) (map[string]string, error) {
	if t, ok := s.builtin[name]; ok {
		if _, userOverride := s.user[name]; !userOverride {
			return s.complete(t.Name, cloneVars(t.Vars))
		}
	}
	t, ok := s.user[name]
	if !ok {
		if bt, ok := s.builtin[name]; ok {
			return s.complete(bt.Name, cloneVars(bt.Vars))
		}
		return nil, fmt.Errorf("theme: unknown theme %q", name)
	}

	base := t.Base
	if base == "" {
		if _, ok := s.builtin[t.Name]; ok {
			base = t.Name // override use case: extend the same-named built-in
		} else {
			base = BuiltinDefault
		}
	}
	bt, ok := s.builtin[base]
	if !ok {
		return nil, fmt.Errorf("theme: %q: base %q is not a built-in theme", name, base)
	}
	vars := cloneVars(bt.Vars)
	for k, v := range t.Vars {
		vars[k] = v
	}
	return s.complete(name, vars)
}

func (s *Set) complete(name string, vars map[string]string) (map[string]string, error) {
	for _, k := range RequiredVars {
		if _, ok := vars[k]; !ok {
			return nil, fmt.Errorf("theme: %q is incomplete: missing %q", name, k)
		}
	}
	return vars, nil
}

func cloneVars(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
