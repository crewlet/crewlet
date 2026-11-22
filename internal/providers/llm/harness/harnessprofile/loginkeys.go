package harnessprofile

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// KeepLogin is home, the credential file named name that a home's harness
// left, with the keys the harness writes during a run ([Profile.RunKeys]) as
// shared, the shared login's copy, holds them ([KeepLoginKeys]), and, for a
// [CredentialStore]'s document, no login but the ones shared holds
// ([KeepHandedCredentials]).
func (p *Profile) KeepLogin(name string, home, shared []byte) []byte {
	kept := KeepLoginKeys(home, shared, p.RunKeys[name])
	if p.CredentialStore != nil && name == CredentialStoreDocument {
		kept = KeepHandedCredentials(kept, shared)
	}
	return kept
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
