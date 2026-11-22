package harnessprofile

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// KeepLogin is what a refresh carries back of home, the credential file named
// name that a home's harness left, over shared, the shared login's copy
// ([FileRule.Carry]).
func (p *Profile) KeepLogin(name string, home, shared []byte) []byte {
	return p.FileRule(name).Carry(home, shared)
}

// KeepLoginKeys is home with each of keys (dotted, into a JSON object) as
// shared holds it, absent where shared holds none: what a run's carry writes
// of a login file, so a key its harness minted during the run never enters
// the shared login. Where the result means what shared does it is shared,
// byte for byte, so a run that changed nothing else changed nothing; where
// either file is no JSON object it is home as it stands, since nothing in it
// can be told apart.
func KeepLoginKeys(home, shared []byte, keys []string) []byte {
	if len(keys) == 0 {
		return home
	}
	h, ok := jsonObject(home)
	if !ok {
		return home
	}
	s := map[string]any{}
	if len(bytes.TrimSpace(shared)) > 0 {
		if s, ok = jsonObject(shared); !ok {
			return home
		}
	}
	for _, key := range keys {
		path := strings.Split(key, ".")
		if v, held := lookupKey(s, path); held {
			setKey(h, path, v)
		} else {
			dropKey(h, path)
		}
	}
	if reflect.DeepEqual(h, s) {
		return shared
	}
	out, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return home
	}
	return append(out, '\n')
}

// identityAt is the value at path in doc, a `*` part standing for every key of
// the object there: the values beneath it by key, a key holding none of the
// rest of path as [absentIdentity], so an entry losing it is told apart.
func identityAt(doc any, path []string) (any, bool) {
	if len(path) == 0 {
		return doc, true
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

// jsonObject is content read as a JSON object, its numbers as written.
func jsonObject(content []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var doc map[string]any
	if dec.Decode(&doc) != nil || doc == nil {
		return nil, false
	}
	return doc, true
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

// dropKey removes the value at path in doc, where it holds one.
func dropKey(doc map[string]any, path []string) {
	for _, key := range path[:len(path)-1] {
		next, ok := doc[key].(map[string]any)
		if !ok {
			return
		}
		doc = next
	}
	delete(doc, path[len(path)-1])
}
