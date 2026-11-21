package redact

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// credentialValue is what a string or number under a password's key becomes
// where that key names a container ([argValue]): the marker the engine's
// other readers of a key write (internal/sandbox/executor's scrub).
const credentialValue = Marker + "credential]"

// Args is a call's arguments as a person is shown them, on a record or
// a screen, REDACTED AS VALUES, a copy, never as their JSON text, whose
// redaction takes a quoted password's quotes with it and leaves no JSON: every
// key and string redacted as the text it decodes to, and every string or
// number read beside its key too ([argScalar]), so a value only its key
// names a credential (a password's, an AWS secret's) goes whole. Each value is
// read with its own key alone: one member's words never redact another's.
func Args(args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	out, _ := argValue(args, "", false).(map[string]any)
	return out
}

// argValue is v, under key, with every key and string redacted, a copy;
// marked says a key above v names a password ([NamesPassword]), under
// which every string and number is taken out whole. Two keys a redaction makes
// one keep both values, the later named apart.
func argValue(v any, key string, marked bool) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			name := Secrets(k)
			for n := 2; ; n++ {
				if _, taken := out[name]; !taken {
					break
				}
				name = fmt.Sprintf("%s (%d)", Secrets(k), n)
			}
			// A CONTAINER under a password's key holds it: its members are
			// read under their own keys, which need not say so.
			out[name] = argValue(t[k], k, marked || (holdsValues(t[k]) && NamesPassword(k)))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = argValue(e, key, marked)
		}
		return out
	case nil, bool:
		return t
	}
	if marked {
		return credentialValue
	}
	return argScalar(v, key)
}

// holdsValues reports whether v is an object or an array.
func holdsValues(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// argScalar is a string or a number under key as a person is shown it: a
// string redacted as its own text, and then read again as the one member it
// is, `{"key":value}`, as the rules read a document. Where that reading takes
// out more, the key names the value a credential, and it is the markers the
// member's redaction wrote past the key: the value whole. A value under a
// password's key goes whole whatever the reading.
func argScalar(v any, key string) any {
	own := v
	if s, ok := v.(string); ok {
		own = Secrets(s)
	}
	if key == "" || Secrets(key) != key {
		return own
	}
	if taken := takenByKey(own, key); taken != "" {
		return taken
	}
	if NamesPassword(key) {
		return credentialValue
	}
	return own
}

// takenByKey is what the redaction of own, a value already redacted as its
// own text, writes past key as the member under it, where it takes anything
// out: its markers, or [credentialValue] where it wrote none there; "" where
// it takes nothing, the key naming no credential.
func takenByKey(own any, key string) string {
	member, err := plainjson.Marshal(map[string]any{key: own}, "")
	if err != nil {
		return ""
	}
	read := Secrets(string(member))
	if read == string(member) {
		return ""
	}
	name, err := plainjson.Marshal(key, "")
	if err == nil {
		if taken := markersIn(strings.TrimPrefix(read, "{"+string(name)+":")); taken != "" {
			return taken
		}
	}
	return credentialValue
}

// markersIn is the markers the redaction wrote in s, in order, with none of
// s's own words between them ([OwnWords]).
func markersIn(s string) string {
	var b strings.Builder
	at := 0
	for from, words := range OwnWords(s) {
		b.WriteString(s[at:from])
		at = from + len(words)
	}
	b.WriteString(s[at:])
	return b.String()
}
