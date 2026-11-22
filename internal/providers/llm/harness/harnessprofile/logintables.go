package harnessprofile

import (
	"bytes"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/BurntSushi/toml"
)

// KeepLoginTOML is content, the TOML credential file named name, with only the
// top-level keys it holds its login under ([Profile.LoginKeys]): what a seat
// home is seeded with and a box's run is handed of it, so a hook, a plugin, a
// permission rule or any other setting written into the file beside the login
// reaches no call and no run. A file that is no TOML file, or names no login
// keys, is content as it stands.
func (p *Profile) KeepLoginTOML(name string, content []byte) ([]byte, error) {
	keys := p.LoginKeys[name]
	if len(keys) == 0 || filepath.Ext(name) != ".toml" {
		return content, nil
	}
	return KeepLoginTables(content, keys)
}

// KeepLoginTables is content, a TOML document, with only its top-level keys
// among tables. Where it holds no other key it is content, byte for byte, so a
// login that holds only itself is seeded as written and its digest is the
// shared one's. Empty content is empty; content that is not TOML is refused,
// since nothing in it can be told apart.
func KeepLoginTables(content []byte, tables []string) ([]byte, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return content, nil
	}
	var doc map[string]any
	if _, err := toml.Decode(string(content), &doc); err != nil {
		return nil, fmt.Errorf("the file is not TOML: %w", err)
	}
	kept := maps.Clone(doc)
	maps.DeleteFunc(kept, func(key string, _ any) bool { return !slices.Contains(tables, key) })
	if len(kept) == len(doc) {
		return content, nil
	}
	if len(kept) == 0 {
		return []byte{}, nil
	}
	var out bytes.Buffer
	if err := toml.NewEncoder(&out).Encode(kept); err != nil {
		return nil, fmt.Errorf("writing the login's tables: %w", err)
	}
	return out.Bytes(), nil
}
