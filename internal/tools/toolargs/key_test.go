package toolargs_test

import (
	"crypto/sha256"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// AN ARGUMENT KEY IS THE TEXT THE LOG KEEPS: Key(raw) is the SHA-256 of
// tools.RecordArgs over Decode(raw), the text a run's log writes, so two
// records of one call key equal however each spelt its arguments (key order,
// whitespace, a string's escapes; nothing, `""` and null as `{}`), a number
// keys as the digits it was written in (`1.0` is not `1`, nor 2^53+1 its
// float), a key written twice keys by its last value, and what does not decode
// has no key.
//
// Mutation: hash the raw bytes, or decode numbers as float64, and a case keys
// apart from the log's text or two different texts key equal.
func TestAnArgumentKeyIsTheTextTheLogKeeps(t *testing.T) {
	t.Parallel()
	logged := func(raw string) [32]byte {
		t.Helper()
		args, err := toolargs.Decode([]byte(raw))
		if err != nil {
			t.Fatalf("%s does not decode: %v", raw, err)
		}
		text, err := tools.RecordArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256([]byte(text))
	}
	key := func(raw string) [32]byte {
		t.Helper()
		k, err := toolargs.Key([]byte(raw))
		if err != nil {
			t.Fatalf("%s has no key: %v", raw, err)
		}
		if k != logged(raw) {
			t.Errorf("%s keys apart from the text the log keeps of it", raw)
		}
		return k
	}
	for _, same := range [][]string{
		{`{"b":1,"a":"x"}`, `{ "a" : "x" , "b" : 1 }`, "{\n\t\"a\": \"\\u0078\",\n\t\"b\": 1\n}"},
		{``, `null`, ` `, `{}`},
		{`{"a":"<&>"}`, `{"a":"\u003c\u0026\u003e"}`},
		{`{"a":1,"a":2}`, `{"a":2}`},
		{`{"n":{"z":[1,{"y":true,"x":null}]}}`, `{"n":{"z":[1,{"x":null,"y":true}]}}`},
	} {
		for _, raw := range same[1:] {
			if key(raw) != key(same[0]) {
				t.Errorf("%s and %s are one call's arguments and key apart", raw, same[0])
			}
		}
	}
	for _, apart := range [][2]string{
		{`{"a":1.0}`, `{"a":1}`},
		{`{"a":1e3}`, `{"a":1000}`},
		{`{"a":9007199254740993}`, `{"a":9007199254740992}`},
		{`{"a":-0.0}`, `{"a":0}`},
		{`{"a":"1"}`, `{"a":1}`},
		{`{"a":1,"a":2}`, `{"a":1}`},
	} {
		if key(apart[0]) == key(apart[1]) {
			t.Errorf("%s and %s are two texts the log keeps and key equal", apart[0], apart[1])
		}
	}
	for _, unreadable := range []string{`[1]`, `{"a":1} {"a":2}`, `{"a":`, `"text"`} {
		if _, err := toolargs.Key([]byte(unreadable)); err == nil {
			t.Errorf("%s does not decode and has a key", unreadable)
		}
	}
}
