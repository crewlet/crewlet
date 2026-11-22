package harnessprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// KeepLogin is what a refresh carries back of home, the credential file named
// name that a home's harness left, over shared, the shared login's copy
// ([FileRule.Carry]).
func (p *Profile) KeepLogin(name string, home, shared []byte) ([]byte, error) {
	return p.FileRule(name).Carry(home, shared)
}

// subjectPart is the last part of an identity key path that reads the value
// there as a JWT and holds its subject claims ([subjectOf]) rather than the
// token: the account a token was issued to, which a refresh keeps.
const subjectPart = "@subject"

// identityAt is the value at path in doc: a `*` part stands for every key of
// the object there, the values beneath it by key, a key holding none of the
// rest of path as [absentIdentity], so an entry losing it is told apart; a
// last [subjectPart] reads a JWT's subject claims, a value that is no JWT
// holding none.
func identityAt(doc any, path []string) (any, bool) {
	if len(path) == 0 {
		return doc, true
	}
	if path[0] == subjectPart {
		claims, ok := jwtClaims(doc)
		if !ok {
			return nil, false
		}
		return subjectOf(claims), true
	}
	object, ok := doc.(map[string]any)
	if !ok {
		return nil, false
	}
	if path[0] != "*" {
		next, held := object[path[0]]
		if !held {
			return nil, false
		}
		return identityAt(next, path[1:])
	}
	each := make(map[string]any, len(object))
	for key, next := range object {
		if v, held := identityAt(next, path[1:]); held {
			each[key] = v
		} else {
			each[key] = absentIdentity{}
		}
	}
	return each, true
}

// absentIdentity stands, under a `*` part, for an entry holding no value at
// the rest of the path ([identityAt]).
type absentIdentity struct{}

// carryKey sets the value at path in dst, the shared copy, to src's, a home's:
// a `*` part walks every key of an object, or index of an array, that both
// hold, so an entry the home added is never carried; a value the home lacks
// leaves dst's, and a parent dst lacks takes nothing.
func carryKey(dst, src any, path []string) {
	if path[0] == "*" {
		switch d := dst.(type) {
		case map[string]any:
			s, _ := src.(map[string]any)
			for key, next := range d {
				if from, held := s[key]; held {
					carryKey(next, from, path[1:])
				}
			}
		case []any:
			s, _ := src.([]any)
			for i := range min(len(d), len(s)) {
				carryKey(d[i], s[i], path[1:])
			}
		}
		return
	}
	d, ok := dst.(map[string]any)
	if !ok {
		return
	}
	s, _ := src.(map[string]any)
	from, held := s[path[0]]
	if !held {
		return
	}
	if len(path) == 1 {
		d[path[0]] = from
		return
	}
	if next, present := d[path[0]]; present {
		carryKey(next, from, path[1:])
	}
}

// withoutCommentLines is content with every line opening with `//` set aside:
// Copilot CLI's plaintext login file opens with two.
func withoutCommentLines(content []byte) []byte {
	var out []byte
	for line := range bytes.Lines(content) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			out = append(out, line...)
		}
	}
	return out
}

// jsonObject is content read as one JSON object, its numbers as written. A
// key twice in one object, or anything after the object, is refused: two
// readers could take a different one of two values.
func jsonObject(content []byte) (map[string]any, error) {
	if err := noDuplicateKeys(json.NewDecoder(bytes.NewReader(content))); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more follows the JSON object")
	}
	return doc, nil
}

// noDuplicateKeys reads one JSON value from dec, refusing an object that names
// a key twice.
func noDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, keyErr := dec.Token()
			if keyErr != nil {
				return keyErr
			}
			name, _ := key.(string)
			if seen[name] {
				return fmt.Errorf("the key %q is named twice", name)
			}
			seen[name] = true
			if valueErr := noDuplicateKeys(dec); valueErr != nil {
				return valueErr
			}
		}
	case '[':
		for dec.More() {
			if valueErr := noDuplicateKeys(dec); valueErr != nil {
				return valueErr
			}
		}
	}
	_, err = dec.Token()
	return err
}

// lookupKey is the value at path in doc.
func lookupKey(doc map[string]any, path []string) (any, bool) {
	for _, key := range path[:len(path)-1] {
		next, ok := doc[key].(map[string]any)
		if !ok {
			return nil, false
		}
		doc = next
	}
	v, ok := doc[path[len(path)-1]]
	return v, ok
}

// setKey puts v at path in doc, making each object on the way.
func setKey(doc map[string]any, path []string, v any) {
	for _, key := range path[:len(path)-1] {
		next, ok := doc[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			doc[key] = next
		}
		doc = next
	}
	doc[path[len(path)-1]] = v
}
