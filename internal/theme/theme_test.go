package theme

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTheme(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinsCompleteAndListed(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	list := s.List()
	want := []Info{
		{Name: "agentchat-dark", Source: "built-in"},
		{Name: "agentchat-light", Source: "built-in"},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("List = %+v", list)
	}

	// Every built-in resolves and defines every variable the frontend
	// styles with — and nothing else.
	for _, info := range list {
		vars, err := s.Resolve(info.Name)
		if err != nil {
			t.Fatalf("%s: %v", info.Name, err)
		}
		if len(vars) != len(RequiredVars) {
			t.Errorf("%s defines %d vars, want %d", info.Name, len(vars), len(RequiredVars))
		}
		for _, k := range RequiredVars {
			if vars[k] == "" {
				t.Errorf("%s missing %q", info.Name, k)
			}
		}
	}

	// The two built-ins actually differ (light is not dark).
	dark, _ := s.Resolve("agentchat-dark")
	light, _ := s.Resolve("agentchat-light")
	if dark["ink"] == light["ink"] || dark["text"] == light["text"] {
		t.Error("light and dark themes share base colors")
	}
}

func TestUserOverrideAndNewTheme(t *testing.T) {
	dir := t.TempDir()
	// Same-named file overrides one variable of the built-in.
	writeTheme(t, dir, "agentchat-dark.json",
		`{"vars": {"ink": "#000000"}, "comment": "unknown keys are fine"}`)
	// A new theme extends light via base.
	writeTheme(t, dir, "sepia.json",
		`{"base": "agentchat-light", "vars": {"panel": "#f3ead9"}}`)
	// A new theme without base falls back to the dark built-in.
	writeTheme(t, dir, "midnight.json", `{"vars": {"focus": "#ff00ff"}}`)

	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	list := s.List()
	wantList := []Info{
		{Name: "agentchat-dark", Source: "user"},
		{Name: "agentchat-light", Source: "built-in"},
		{Name: "midnight", Source: "user"},
		{Name: "sepia", Source: "user"},
	}
	if !reflect.DeepEqual(list, wantList) {
		t.Fatalf("List = %+v", list)
	}

	dark, err := s.Resolve("agentchat-dark")
	if err != nil {
		t.Fatal(err)
	}
	if dark["ink"] != "#000000" {
		t.Errorf("override ignored: ink = %s", dark["ink"])
	}
	if dark["text"] != "#d7dee7" {
		t.Errorf("non-overridden var lost: text = %s", dark["text"])
	}

	sepia, err := s.Resolve("sepia")
	if err != nil {
		t.Fatal(err)
	}
	light, _ := s.Resolve("agentchat-light")
	if sepia["panel"] != "#f3ead9" || sepia["text"] != light["text"] {
		t.Errorf("sepia = panel %s text %s", sepia["panel"], sepia["text"])
	}

	midnight, err := s.Resolve("midnight")
	if err != nil {
		t.Fatal(err)
	}
	if midnight["focus"] != "#ff00ff" || midnight["ink"] != "#101418" {
		t.Errorf("midnight = focus %s ink %s", midnight["focus"], midnight["ink"])
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"non-color.json", `{"vars": {"ink": "url(javascript:x)"}}`, "not a color"},
		{"injection.json", `{"vars": {"ink": "#fff; background:red"}}`, "not a color"},
		{"badvar.json", `{"vars": {"Ink Color!": "#fff"}}`, "invalid variable name"},
		{"badname.json", `{"name": "../escape", "vars": {"ink": "#fff"}}`, "invalid theme name"},
		{"empty.json", `{"name": "empty"}`, `no "vars"`},
		{"malformed.json", `{not json`, "invalid character"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeTheme(t, dir, c.name, c.body)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
		}
	}

	// Valid color forms are accepted.
	dir := t.TempDir()
	writeTheme(t, dir, "forms.json",
		`{"vars": {"ink": "rgb(1, 2, 3)", "text": "hsla(200, 50%, 40%, 0.9)", "muted": "rebeccapurple", "line": "#abc"}}`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("valid color forms rejected: %v", err)
	}

	// Unknown base is an error at resolve time.
	dir2 := t.TempDir()
	writeTheme(t, dir2, "orphan.json", `{"base": "no-such-base", "vars": {"ink": "#fff"}}`)
	s, err := Load(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("orphan"); err == nil || !strings.Contains(err.Error(), "no-such-base") {
		t.Errorf("orphan base err = %v", err)
	}

	// Unknown theme names error.
	if _, err := s.Resolve("nope"); err == nil {
		t.Error("unknown theme accepted")
	}
}
