package harnessprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileRule is how one credential file travels between the shared login and a
// home, a seat's or a box's, both ways: what a home is seeded with
// ([FileRule.Seed]), what a refresh carries back ([FileRule.Carry]), and
// whether a copy is the seeded login's account at all ([FileRule.SameAccount]).
// One rule, so the engine host and a box never disagree on a file.
//
// A file is a LOGIN FILE, carried whole, or a MIXED one, where the harness also
// reads its own settings beside the login: LoginKeys names the login's fields,
// and nothing else of it is seeded or carried, so a setting written in one home
// reaches no other. A zero FileRule is a login file with no other rule.
type FileRule struct {
	// Name is the file's name in the shared directory, which says its format
	// (TOML for a `.toml` name, a `.env` file's variables for a `.env` one, else
	// JSON with every `//` line set aside) and
	// whether it is a [CredentialStore]'s document ([CredentialStoreDocument]),
	// which a refresh holds to the logins it was handed
	// ([KeepHandedCredentials]).
	Name string `json:"name"`

	// LoginKeys are the key paths that are the login, for a mixed file
	// ([Profile.LoginKeys]); none for a login file.
	LoginKeys []string `json:"login_keys,omitempty"`

	// RunKeys are the key paths the harness writes during a run, which a
	// refresh carries as the shared copy holds them ([Profile.RunKeys]).
	RunKeys []string `json:"run_keys,omitempty"`

	// IdentityKeys are the key paths saying which login the file is: whose
	// account, at which provider ([Profile.IdentityKeys]).
	IdentityKeys []string `json:"identity_keys,omitempty"`
}

// FileRule is the rule of the credential file named name.
func (p *Profile) FileRule(name string) FileRule {
	return FileRule{
		Name:         name,
		LoginKeys:    slices.Clone(p.LoginKeys[name]),
		RunKeys:      slices.Clone(p.RunKeys[name]),
		IdentityKeys: slices.Clone(p.IdentityKeys[name]),
	}
}

// Mixed reports whether the file keeps settings beside its login.
func (r FileRule) Mixed() bool { return len(r.LoginKeys) > 0 }

// Seed is what a home is handed of shared, the shared copy: a login file
// whole, a mixed one only its login's fields, so no setting in the shared copy
// rides along. A mixed file that does not read in its format is refused: none
// of it can be told apart.
func (r FileRule) Seed(shared []byte) ([]byte, error) {
	if !r.Mixed() {
		return shared, nil
	}
	doc, err := r.decode(shared)
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
	return r.encode(login, nil)
}

// Carry is what a refresh writes of home, a home's copy, over shared, the
// shared copy: a login file as home has it, a mixed one shared with only its
// login's fields taken from home (a field home lacks dropped); then every run
// key as shared holds it ([KeepLoginKeys]), and a store's document held to the
// logins shared holds ([KeepHandedCredentials]). Where the result means what
// shared does it is shared, byte for byte. A mixed copy either side cannot
// read carries nothing: shared.
func (r FileRule) Carry(home, shared []byte) []byte {
	out := home
	if r.Mixed() {
		out = r.mergeLogin(home, shared)
	}
	out = KeepLoginKeys(out, shared, r.RunKeys)
	if r.Name == CredentialStoreDocument {
		out = KeepHandedCredentials(out, shared)
	}
	return out
}

// mergeLogin is shared with each of the login's fields as home holds it.
func (r FileRule) mergeLogin(home, shared []byte) []byte {
	h, err := r.decode(home)
	if err != nil {
		return shared
	}
	s, err := r.decode(shared)
	if err != nil {
		return shared
	}
	was, _ := r.decode(shared)
	for _, key := range r.LoginKeys {
		path := strings.Split(key, ".")
		if v, held := lookupKey(h, path); held {
			setKey(s, path, v)
		} else {
			dropKey(s, path)
		}
	}
	if reflect.DeepEqual(s, was) {
		return shared
	}
	out, err := r.encode(s, commentHeader(shared))
	if err != nil {
		return shared
	}
	return out
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

// dotenv reports whether the file is a `.env` file ([parseDotenv]).
func (r FileRule) dotenv() bool { return strings.HasSuffix(r.Name, ".env") }

// decode reads content as the file's format into an object; empty content is
// an empty object.
func (r FileRule) decode(content []byte) (map[string]any, error) {
	if r.dotenv() {
		doc := map[string]any{}
		for key, value := range parseDotenv(content) {
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
	body := withoutCommentLines(content)
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	doc, ok := jsonObject(body)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return doc, nil
}

// encode writes doc in the file's format, header (a JSON file's leading `//`
// lines) first.
func (r FileRule) encode(doc map[string]any, header []byte) ([]byte, error) {
	if r.dotenv() {
		return encodeDotenv(doc), nil
	}
	if r.toml() {
		var out bytes.Buffer
		if err := toml.NewEncoder(&out).Encode(doc); err != nil {
			return nil, fmt.Errorf("writing %s: %w", r.Name, err)
		}
		return out.Bytes(), nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("writing %s: %w", r.Name, err)
	}
	return append(append(slices.Clone(header), out...), '\n'), nil
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
