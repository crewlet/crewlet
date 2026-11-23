package harnessprofile

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// KeepHandedCredentials is home, a [CredentialStore]'s document as a run's
// harness exported it, holding no login but those of shared, the document the
// run was handed: each handed credential takes the run's tokens only where it
// is still the same login ([sameLogin]), one the run gained is dropped, and one
// it lost is kept as handed. Where the result means what shared does it is
// shared, byte for byte; else it is written in shared's layout, never as
// home's bytes. A document either side cannot read as a store's carries
// nothing: shared.
func KeepHandedCredentials(home, shared []byte) []byte {
	handed, ok := credentialList(shared)
	if !ok {
		return shared
	}
	held, ok := credentialList(home)
	if !ok {
		return shared
	}
	byID := make(map[string]map[string]any, len(held))
	for _, c := range held {
		id := c["id"].(string)
		if _, twice := byID[id]; twice {
			return shared
		}
		byID[id] = c
	}
	out := make([]map[string]any, 0, len(handed))
	for _, c := range handed {
		run, present := byID[c["id"].(string)]
		if !present || !sameLogin(c, run) {
			out = append(out, c)
			continue
		}
		out = append(out, refreshed(c, run))
	}
	if reflect.DeepEqual(out, handed) {
		return shared
	}
	doc, err := encodeJSON(out, shared, nil)
	if err != nil {
		return shared
	}
	return doc
}

// credentialList is content read as a store's document: a JSON array of
// objects, each with a string id. Its numbers stay as written. A key twice in
// one object, or anything after the array, is refused, as [jsonObject] refuses
// them.
func credentialList(content []byte) ([]map[string]any, bool) {
	if noDuplicateKeys(json.NewDecoder(bytes.NewReader(content))) != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var list []map[string]any
	if dec.Decode(&list) != nil || list == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	for _, c := range list {
		if id, ok := c["id"].(string); !ok || id == "" {
			return nil, false
		}
	}
	return list, true
}

// refreshTokenKeys are the keys of a credential's value a refresh rewrites:
// the tokens and their expiry. Nothing else of it is carried, metadata a
// refresh fills in included ([refreshed]).
var refreshTokenKeys = []string{"access", "refresh", "expires"}

// sameLogin reports whether run is handed after a refresh: only a credential
// whose value names its type (OpenCode's shape, [sameTypedLogin]), held to
// that type's refresh. One of any other shape is no login whose tokens can be
// told apart, so it is never one.
func sameLogin(handed, run map[string]any) bool {
	if _, typed := typedValue(handed); !typed {
		return false
	}
	return sameTypedLogin(handed, run)
}

// typedValue is c's value where it is an object naming its type.
func typedValue(c map[string]any) (map[string]any, bool) {
	v, ok := c["value"].(map[string]any)
	if !ok {
		return nil, false
	}
	_, ok = v["type"].(string)
	return v, ok
}

// sameTypedLogin is [sameLogin] for a credential of OpenCode's shape: the same
// credential (every field but its value and active the same), an OAuth login
// of the same method, every value field a refresh leaves alone the same, and
// its access token naming the same account ([sameSubject]). A key never
// refreshes, so a changed one is another login.
func sameTypedLogin(handed, run map[string]any) bool {
	for _, key := range unionKeys(handed, run) {
		if key != "value" && key != "active" && !reflect.DeepEqual(handed[key], run[key]) {
			return false
		}
	}
	hv, _ := typedValue(handed)
	rv, ok := typedValue(run)
	if !ok || hv["type"] != "oauth" {
		return false
	}
	for _, key := range unionKeys(hv, rv) {
		switch {
		case key == "metadata":
			if had, held := hv[key]; held && !reflect.DeepEqual(had, rv[key]) {
				return false
			}
		case !slices.Contains(refreshTokenKeys, key) && !reflect.DeepEqual(hv[key], rv[key]):
			return false
		}
	}
	return sameSubject(hv["access"], rv["access"])
}

// refreshed is the credential carried for handed, which run refreshed: of
// OpenCode's shape, handed with each token and expiry handed holds taken from
// run where it is of the same kind ([sameScalar]); of any other, handed as it
// is ([sameLogin] never takes one).
func refreshed(handed, run map[string]any) map[string]any {
	hv, typed := typedValue(handed)
	if !typed {
		return maps.Clone(handed)
	}
	out := maps.Clone(handed)
	value := maps.Clone(hv)
	rv, _ := typedValue(run)
	for _, key := range refreshTokenKeys {
		if v, ok := rv[key]; ok && sameScalar(value[key], v) {
			value[key] = v
		}
	}
	out["value"] = value
	return out
}

// sameSubject reports whether two access tokens name one account. A JWT names
// it in its subject claims ([subjectOf]), which a refresh keeps; an opaque
// token names nothing that can be told apart, so it holds the same login as
// any, where the fields [sameLogin] compares agree. A JWT login never turns
// into an opaque one.
func sameSubject(handed, run any) bool {
	hc, ok := jwtClaims(handed)
	if !ok {
		return true
	}
	subject := subjectOf(hc)
	if len(subject) == 0 {
		return true
	}
	rc, ok := jwtClaims(run)
	return ok && maps.Equal(subject, subjectOf(rc))
}

// subjectOf is the claims naming whom a JWT was issued to: `sub`, and each
// string claim, top-level or one object down (a vendor's namespaced claim),
// whose name ends in user_id or account_id.
func subjectOf(claims map[string]any) map[string]string {
	out := map[string]string{}
	for name, v := range claims {
		switch v := v.(type) {
		case string:
			if isSubjectClaim(name) {
				out[name] = v
			}
		case map[string]any:
			for inner, iv := range v {
				if s, ok := iv.(string); ok && isSubjectClaim(inner) {
					out[name+"."+inner] = s
				}
			}
		}
	}
	return out
}

func isSubjectClaim(name string) bool {
	return name == "sub" || strings.HasSuffix(name, "user_id") || strings.HasSuffix(name, "account_id")
}

// jwtClaims is token's payload, where it is a JWT whose payload is a JSON
// object. Its signature is not checked: both tokens came from one box.
func jwtClaims(token any) (map[string]any, bool) {
	s, ok := token.(string)
	if !ok {
		return nil, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return nil, false
	}
	return claims, true
}

func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	return keys
}
