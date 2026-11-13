package codescan

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// THE DEPENDENCY FILES. Only a package's NAME is read, and only to look it up
// in the SDK table: a version, a URL, a path or an index address beside it is
// where a token is written (a git URL with a password, a private index), so
// no parser declares a field for one.

// unread is a value the decoder is told to skip: it satisfies both decoders'
// interfaces and keeps nothing.
type unread struct{}

// UnmarshalJSON reads nothing.
func (*unread) UnmarshalJSON([]byte) error { return nil }

// UnmarshalTOML reads nothing.
func (*unread) UnmarshalTOML(any) error { return nil }

// packageFile is the part of a package.json the scan reads: the names of what
// the package runs with. Development dependencies are tooling, never read.
type packageFile struct {
	Dependencies         map[string]unread `json:"dependencies"`
	OptionalDependencies map[string]unread `json:"optionalDependencies"`
}

func (f *finder) packageJSON(raw []byte) error {
	var doc packageFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return errUnparsed
	}
	for _, set := range []map[string]unread{doc.Dependencies, doc.OptionalDependencies} {
		for _, name := range sortedKeys(set) {
			f.dependency(npm, name)
		}
	}
	return nil
}

// goMod reads a go.mod's required module paths: the first word of each line
// of a require block, or the word after `require` on a line of its own.
func (f *finder) goMod(raw []byte) {
	block := false
	for line := range strings.Lines(string(raw)) {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case block && fields[0] == ")":
			block = false
		case block:
			f.dependency(golang, fields[0])
		case fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			block = true
		case fields[0] == "require" && len(fields) >= 3:
			f.dependency(golang, fields[1])
		}
	}
}

// requirements reads a requirements file's package names. A line that starts
// with an option (an index address, an include, an editable install) or names
// an address is skipped whole: those are where a credential is written.
func (f *finder) requirements(raw []byte) {
	for line := range strings.Lines(string(raw)) {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") || strings.Contains(line, "://") {
			continue
		}
		f.dependency(pypi, requirementName(line))
	}
}

// requirementName is the package a requirement names: what stands before its
// extras, its version, its marker or its address.
func requirementName(req string) string {
	if i := strings.IndexAny(req, " \t[<>=!~;@("); i >= 0 {
		req = req[:i]
	}
	return req
}

// pyprojectFile is the part of a pyproject.toml the scan reads: the project's
// requirements, and the names of a Poetry project's dependencies.
type pyprojectFile struct {
	Project struct {
		Dependencies []string `toml:"dependencies"`
	} `toml:"project"`
	Tool struct {
		Poetry struct {
			Dependencies map[string]unread `toml:"dependencies"`
		} `toml:"poetry"`
	} `toml:"tool"`
}

func (f *finder) pyproject(raw []byte) error {
	var doc pyprojectFile
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return errUnparsed
	}
	for _, req := range doc.Project.Dependencies {
		if !strings.Contains(req, "://") {
			f.dependency(pypi, requirementName(strings.TrimSpace(req)))
		}
	}
	for _, name := range sortedKeys(doc.Tool.Poetry.Dependencies) {
		f.dependency(pypi, name)
	}
	return nil
}

// gemfile reads a Gemfile's gem names: the first quoted word after `gem`.
// Nothing after it (a version, a git address, a source) is read.
func (f *finder) gemfile(raw []byte) {
	for line := range strings.Lines(string(raw)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "gem")
		if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t' && rest[0] != '(') {
			continue
		}
		rest = strings.TrimLeft(rest, " \t(")
		if rest == "" || (rest[0] != '\'' && rest[0] != '"') {
			continue
		}
		if end := strings.IndexByte(rest[1:], rest[0]); end > 0 {
			f.dependency(rubygems, rest[1:1+end])
		}
	}
}

// cargoFile is the part of a Cargo.toml the scan reads: the names of the
// crates it depends on.
type cargoFile struct {
	Dependencies map[string]unread `toml:"dependencies"`
}

func (f *finder) cargo(raw []byte) error {
	var doc cargoFile
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return errUnparsed
	}
	for _, name := range sortedKeys(doc.Dependencies) {
		f.dependency(crates, name)
	}
	return nil
}

// sortedKeys is m's keys in order, so a file always declares the same claims
// in the same order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
