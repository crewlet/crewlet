package harnessprofile

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/BurntSushi/toml"
)

// KeepLoginTables is content, a TOML credential file, with only its top-level
// keys among tables ([Profile.LoginTables]): what a box is handed of the
// shared copy, so a hook, a permission rule or any other setting a run once
// wrote into the file reaches no later run. Empty content is empty; content
// that is not TOML is refused, since nothing in it can be told apart.
func KeepLoginTables(content []byte, tables []string) ([]byte, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	var doc map[string]any
	if _, err := toml.Decode(string(content), &doc); err != nil {
		return nil, fmt.Errorf("the file is not TOML: %w", err)
	}
	kept := map[string]any{}
	for key, value := range doc {
		if slices.Contains(tables, key) {
			kept[key] = value
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	if err := toml.NewEncoder(&out).Encode(kept); err != nil {
		return nil, fmt.Errorf("writing the login's tables: %w", err)
	}
	return out.Bytes(), nil
}
