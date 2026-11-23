package harnessprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/envfile"
)

// FileRule is how one credential file travels between the shared login and a
// home, a seat's or a box's, both ways: which of it is the login
// ([FileRule.Login]), what a refresh carries back ([FileRule.Carry]), and
// whether a copy is the seeded login's account ([FileRule.SameAccount]).
//
// A box is seeded with a file whole, a MIXED one's settings with its login, as
// a person's own home holds it; a seat home on the engine host, which nothing
// sandboxes, with its login alone, a MIXED file's LoginKeys' fields. A refresh
// carries back its token fields alone (RefreshKeys); every other byte, an
// endpoint, a provider choice or a setting, stays as the shared copy holds it.
// A zero FileRule's login is the whole file, and it carries nothing back.
type FileRule struct {
	// Name is the file's name in the shared directory, which says its format
	// (TOML for a `.toml` name, a `.env` file's variables for a `.env` one, else
	// JSON) and whether it is a [CredentialStore]'s document
	// ([CredentialStoreDocument]), which a refresh holds to the logins it was
	// handed ([KeepHandedCredentials]).
	Name string `json:"name"`

	// CommentLines is whether the harness writes lines opening with `//` into
	// this JSON file, which are then read set aside ([Profile.CommentLines]);
	// in any other JSON file such a line does not read.
	CommentLines bool `json:"comment_lines,omitempty"`

	// LoginKeys are the key paths that are the login, for a mixed file
	// ([Profile.LoginKeys]); none for a login file.
	LoginKeys []string `json:"login_keys,omitempty"`

	// RefreshKeys are the key paths a refresh carries back from a home's copy
	// ([Profile.RefreshKeys]): its tokens and their expiry.
	RefreshKeys []string `json:"refresh_keys,omitempty"`

	// IdentityKeys are the key paths saying which login the file is: whose
	// account, at which provider ([Profile.IdentityKeys]).
	IdentityKeys []string `json:"identity_keys,omitempty"`
}

// FileRule is the rule of the credential file named name.
func (p *Profile) FileRule(name string) FileRule {
	return FileRule{
		Name:         name,
		LoginKeys:    slices.Clone(p.LoginKeys[name]),
		RefreshKeys:  slices.Clone(p.RefreshKeys[name]),
		IdentityKeys: slices.Clone(p.IdentityKeys[name]),
		CommentLines: slices.Contains(p.CommentLines, name),
	}
}

// Mixed reports whether the file keeps settings beside its login.
func (r FileRule) Mixed() bool { return len(r.LoginKeys) > 0 }

// Login is the login content holds, a copy of the file: a login file whole, a
// mixed one only its login's fields (content as it stands where it holds
// nothing else). A seat home on the engine host is seeded with it; a box with
// the copy whole, held to its login alone, so two copies whose login is one are
// one seed whatever settings beside it differ. A mixed file that does not read
// in its format is refused: its login cannot be told apart.
func (r FileRule) Login(content []byte) ([]byte, error) {
	if !r.Mixed() {
		return content, nil
	}
	doc, err := r.decode(content)
	if err != nil {
		return nil, fmt.Errorf("reading %s for its login: %w", r.Name, err)
	}
	login := map[string]any{}
	for _, key := range r.LoginKeys {
		path := strings.Split(key, ".")
		if v, held := lookupKey(doc, path); held {
			setKey(login, path, v)
		}
	}
	if reflect.DeepEqual(login, doc) {
		// THE LOGIN ALONE, byte for byte, so its digest is the whole copy's.
		return content, nil
	}
	return r.encode(login, nil)
}

// Carry is what a refresh writes of home, a home's copy, over shared, the
// shared copy: shared with each of its refresh keys as home holds it, a key home
// lacks left as shared holds it, and a store's document held to the logins
// shared holds ([KeepHandedCredentials]). Where the result means what shared
// does it is shared, byte for byte; else it is written in shared's layout,
// never as the home's bytes. A copy either side cannot read carries nothing:
// shared, and the reason.
func (r FileRule) Carry(home, shared []byte) ([]byte, error) {
	if r.Name == CredentialStoreDocument {
		return KeepHandedCredentials(home, shared), nil
	}
	if len(r.RefreshKeys) == 0 {
		return shared, nil
	}
	h, err := r.decode(home)
	if err != nil {
		return shared, fmt.Errorf("reading the home's %s: %w", r.Name, err)
	}
	s, err := r.decode(shared)
	if err != nil {
		return shared, fmt.Errorf("reading the shared %s: %w", r.Name, err)
	}
	was, _ := r.decode(shared)
	for _, key := range r.RefreshKeys {
		carryKey(s, h, strings.Split(key, "."))
	}
	if reflect.DeepEqual(s, was) {
		return shared, nil
	}
	out, err := r.encode(s, shared)
	if err != nil {
		return shared, err
	}
	return out, nil
}

// SameAccount reports whether home and shared, two copies of the file, are
// one login at each identity key: each key's value alike in both, or absent
// from both. A `*` part stands for every key of the object there, so
// `*.user_id` holds each entry's user_id and the set of entries alike. A copy
// that does not read in its format names no login it can be held to, so it is
// the same only where the rule names no identity key.
func (r FileRule) SameAccount(home, shared []byte) bool {
	if len(r.IdentityKeys) == 0 {
		return true
	}
	h, err := r.decode(home)
	if err != nil {
		return false
	}
	s, err := r.decode(shared)
	if err != nil {
		return false
	}
	for _, key := range r.IdentityKeys {
		path := strings.Split(key, ".")
		hv, inHome := identityAt(h, path)
		sv, inShared := identityAt(s, path)
		if inHome != inShared || !reflect.DeepEqual(hv, sv) {
			return false
		}
	}
	return true
}

// toml reports whether the file is TOML.
func (r FileRule) toml() bool { return strings.HasSuffix(r.Name, ".toml") }

// dotenv reports whether the file is a `.env` file ([envfile.ReadFile]).
func (r FileRule) dotenv() bool { return strings.HasSuffix(r.Name, ".env") }

// decode reads content as the file's format into an object; empty content is
// an empty object.
func (r FileRule) decode(content []byte) (map[string]any, error) {
	if r.dotenv() {
		vars, err := envfile.ReadFile(content)
		if err != nil {
			return nil, err
		}
		doc := map[string]any{}
		for key, value := range vars {
			doc[key] = value
		}
		return doc, nil
	}
	if r.toml() {
		doc := map[string]any{}
		if _, err := toml.Decode(string(content), &doc); err != nil {
			return nil, fmt.Errorf("not TOML: %w", err)
		}
		return doc, nil
	}
	body := content
	if r.CommentLines {
		body = withoutCommentLines(content)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	return jsonObject(body)
}

// encode writes doc in the file's format and in like's layout, like being the
// copy it replaces (nil for none): a JSON file's `//` header where its rule has
// one, indented unless like is one line, and a last newline only where like
// ends in one.
func (r FileRule) encode(doc map[string]any, like []byte) ([]byte, error) {
	var out []byte
	switch {
	case r.dotenv():
		var err error
		if out, err = r.encodeEnv(doc); err != nil {
			return nil, err
		}
	case r.toml():
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
			return nil, fmt.Errorf("writing %s: %w", r.Name, err)
		}
		out = buf.Bytes()
	default:
		var header []byte
		if r.CommentLines {
			header = commentHeader(like)
		}
		return encodeJSON(doc, like, header)
	}
	return lastNewlineLike(out, like), nil
}

// encodeJSON writes v as JSON in like's layout, like being the copy it
// replaces (nil for none): header first, indented unless like is one line, a
// last newline only where like ends in one, and HTML left unescaped.
func encodeJSON(v any, like, header []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if like == nil || bytes.Contains(bytes.TrimSpace(like[len(header):]), []byte("\n")) {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("writing JSON: %w", err)
	}
	return lastNewlineLike(append(slices.Clone(header), buf.Bytes()...), like), nil
}

// lastNewlineLike is out ending in a newline only where like, the copy it
// replaces, does (always, where there is none).
func lastNewlineLike(out, like []byte) []byte {
	if like != nil && !bytes.HasSuffix(like, []byte("\n")) {
		return bytes.TrimRight(out, "\n")
	}
	return out
}

// encodeEnv writes doc a variable a line, sorted, each as [envfile.FormatAssignment]
// writes it; a value no line can carry is refused.
func (r FileRule) encodeEnv(doc map[string]any) ([]byte, error) {
	var out bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(doc)) {
		line, err := envfile.FormatAssignment(name, fmt.Sprint(doc[name]))
		if err != nil {
			return nil, fmt.Errorf("writing %s: %w", r.Name, err)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// commentHeader is content's leading lines that open with `//`.
func commentHeader(content []byte) []byte {
	var out []byte
	for line := range bytes.Lines(content) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			break
		}
		out = append(out, line...)
	}
	return out
}

// HoldsLogin reports whether the credential file named name, holding content,
// is a login: a login file always, a mixed one ([Profile.LoginKeys]) once any
// of its login's fields holds something other than null or an empty string,
// object or array. A mixed file that does not read in its format IS taken for
// a login: guessing harder means parsing a vendor's format, and the opposite
// wrong guess is a login the engine would not carry, export or keep.
func (p *Profile) HoldsLogin(name string, content []byte) bool {
	r := p.FileRule(name)
	if !r.Mixed() {
		return true
	}
	doc, err := r.decode(content)
	if err != nil {
		return true
	}
	for _, key := range r.LoginKeys {
		if v, held := lookupKey(doc, strings.Split(key, ".")); held && heldValue(v) {
			return true
		}
	}
	return false
}

// heldValue reports whether value, one value of a login file, holds
// something: not null, or an empty string, object or array.
func heldValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case []map[string]any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}
