package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CodexConfig is the slice of ~/.codex/config.toml AgentChat cares
// about: the declared model providers plus the top-level defaults. The
// file is READ-ONLY input — AgentChat never writes client config files.
type CodexConfig struct {
	// Providers are the [model_providers.*] tables as Defs
	// (Source "codex"; Name = table key, Label = its name field,
	// BaseURL/EnvKey from base_url/env_key), sorted by Name.
	Providers []Def
	// DefaultProvider is the top-level model_provider value ("" when
	// unset — codex then uses its built-in OpenAI provider).
	DefaultProvider string
	// DefaultModel is the top-level model value.
	DefaultModel string
}

// CodexConfigPath returns the codex config location: $CODEX_HOME/config.toml
// when CODEX_HOME is set, else ~/.codex/config.toml.
func CodexConfigPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// ReadCodexConfig parses the codex config at path. A missing file (or
// empty path) yields an empty config and no error — codex simply has no
// declared providers. The parser is a deliberately minimal TOML subset
// (line-based: [table] headers, key = "string" pairs; see parseTomlish):
// it extracts what AgentChat needs and skips everything it does not
// understand rather than failing on exotic syntax — codex itself is the
// validator of its own config.
func ReadCodexConfig(path string) (*CodexConfig, error) {
	cfg := &CodexConfig{}
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provider: reading codex config: %w", err)
	}

	tables := parseTomlish(string(b))

	top := tables[""]
	cfg.DefaultProvider = top["model_provider"]
	cfg.DefaultModel = top["model"]

	var names []string
	for table := range tables {
		if name, ok := strings.CutPrefix(table, "model_providers."); ok && name != "" && !strings.Contains(name, ".") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t := tables["model_providers."+name]
		label := t["name"]
		if label == "" {
			label = name
		}
		cfg.Providers = append(cfg.Providers, Def{
			Name:    name,
			Label:   label,
			Source:  "codex",
			BaseURL: t["base_url"],
			EnvKey:  t["env_key"],
		})
	}
	return cfg, nil
}

// parseTomlish extracts string key/values per table from a TOML document.
// Supported subset: comments, [table.path] headers (bare or basic-quoted
// segments), and `key = value` lines where the value is a basic string
// ("..." with \\ \" \n \t \r \uXXXX escapes), a literal string ('...'),
// or a bare scalar (kept verbatim: true, 42, ...). Multi-line strings
// (""" / ”') are consumed and dropped; arrays and inline tables are
// skipped. Unknown or malformed lines are ignored — this reader only
// needs to be right for the keys it extracts, never authoritative.
func parseTomlish(doc string) map[string]map[string]string {
	tables := map[string]map[string]string{"": {}}
	current := ""
	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// Table header ([[array-of-tables]] treated the same way).
			h := strings.TrimPrefix(strings.TrimPrefix(line, "["), "[")
			if end := strings.Index(h, "]"); end >= 0 {
				current = parseTableName(h[:end])
				if _, ok := tables[current]; !ok {
					tables[current] = map[string]string{}
				}
			}
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
		raw := strings.TrimSpace(line[eq+1:])
		if key == "" || raw == "" {
			continue
		}
		// Multi-line strings: consume until the closing delimiter, drop.
		for _, delim := range []string{`"""`, "'''"} {
			if strings.HasPrefix(raw, delim) {
				rest := raw[len(delim):]
				for !strings.Contains(rest, delim) && i+1 < len(lines) {
					i++
					rest = lines[i]
				}
				raw = ""
				break
			}
		}
		if raw == "" {
			continue
		}
		if v, ok := parseTomlValue(raw); ok {
			tables[current][key] = v
		}
	}
	return tables
}

// parseTableName normalizes a header's dotted path, unquoting basic- or
// literal-quoted segments (naive split: quoted segments containing dots
// are out of scope for the keys this reader extracts).
func parseTableName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// parseTomlValue decodes a single-line TOML value into a string. Arrays
// and inline tables report !ok; bare scalars pass through verbatim with
// any trailing comment stripped.
func parseTomlValue(raw string) (string, bool) {
	switch raw[0] {
	case '"':
		var b strings.Builder
		escaped := false
		for _, r := range raw[1:] {
			if escaped {
				switch r {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\':
					b.WriteRune(r)
				default:
					// \uXXXX etc.: keep the escape verbatim; the keys we
					// extract (URLs, env var names) never use them.
					b.WriteByte('\\')
					b.WriteRune(r)
				}
				escaped = false
				continue
			}
			switch r {
			case '\\':
				escaped = true
			case '"':
				return b.String(), true
			default:
				b.WriteRune(r)
			}
		}
		return "", false // unterminated
	case '\'':
		if end := strings.Index(raw[1:], "'"); end >= 0 {
			return raw[1 : 1+end], true
		}
		return "", false
	case '[', '{':
		return "", false // arrays / inline tables: not needed
	default:
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		return raw, raw != ""
	}
}
