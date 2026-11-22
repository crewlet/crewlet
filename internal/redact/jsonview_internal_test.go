package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// jsonValue is the password's value every shape of [loggedShapes] holds.
const jsonValue = "hunter2x9"

// loggedShapes is every way a password's value a plain line hides comes to
// stand in a logged JSON string escaped in layers: a shell writing a key and
// its value a line at a time (`>>`, `>`, `| tee`), a quote's `>` before the
// value, a key closed by an escaped quote; each as json.Marshal writes it
// (`\\n`, `\"`, `\u003e`), alone, after a timestamp, a level, a logfmt field or
// a prefix holding braces, after another field, and in an array; between two
// plain lines, so a cut can fall on either side. plain is the line it carries.
func loggedShapes() []struct{ name, plain, text string } {
	plains := []struct{ name, text string }{
		{"echo, append", "echo \"Password:\\n\" >> k\necho \"" + jsonValue + "\" >> k\necho \"done\" >> k"},
		{"echo, file opened", "echo \"Password:\\n\" > k\necho " + jsonValue + " >> k\necho done >> k"},
		{"echo, tee", "echo \"Password:\\n\" | tee -a k\necho \"" + jsonValue + "\" | tee -a k\necho done | tee -a k"},
		{"a quote's mark below", "Password:\n> " + jsonValue + "\ndone"},
		{"a key closed by a quote", "echo \"Password:\\n\" >> k\necho '" + jsonValue + "' >> k\necho done >> k"},
	}
	var shapes []struct{ name, plain, text string }
	for _, p := range plains {
		q, err := json.Marshal(p.text)
		if err != nil {
			panic(err)
		}
		for _, w := range []struct{ name, open, close string }{
			{"", `{"log": `, `}`},
			{"after a timestamp", `2026-11-22T10:00:00Z {"log": `, `}`},
			{"after a level", `INFO {"log": `, `}`},
			{"after a logfmt field", `ts=2026-11-22 level=info {"log": `, `}`},
			{"after braces", `[worker{1}] {"log": `, `}`},
			{"after another field", `{"ts": "10:00:00", "log": `, `, "n": 1}`},
			{"in an array", `["run", `, `]`},
		} {
			shapes = append(shapes, struct{ name, plain, text string }{
				strings.TrimSuffix(p.name+", "+w.name, ", "), p.text,
				"starting\n" + w.open + string(q) + w.close + "\nnext\n",
			})
		}
	}
	return shapes
}

// A VALUE IN A LOGGED STRING ESCAPED IN LAYERS IS HIDDEN AS IN THE LINE IT
// CARRIES: over [loggedShapes], whose plain lines Secrets hides, Secrets shows
// no value, a second pass changes nothing, a stream at its own hold and at
// 256 KiB, written whole and in pieces of 7 bytes and of 1, writes Secrets,
// and Settled keeps the split promise at every line's end c: with
// s = Settled(text[:c]), Secrets(text[:s]) + Secrets(text[s:]) is Secrets.
//
// Mutation: read no decoded string ([decodedReading]), decode no `\u`
// ([jsonEscapeAt]), or read a JSON value only at its line's start
// ([jsonPartBefore]), and a value is shown.
func TestAValueInALoggedStringIsHiddenAsInTheLineItCarries(t *testing.T) {
	t.Parallel()
	for _, c := range loggedShapes() {
		if plain := Secrets(c.plain); strings.Contains(plain, jsonValue) {
			t.Fatalf("%s: the plain line shows its value, so this shape tests nothing:\n %q", c.name, plain)
		}
		once := Secrets(c.text)
		if strings.Contains(once, jsonValue) {
			t.Errorf("%s: the value is shown:\n text %q\n once %q", c.name, c.text, once)
		}
		if twice := Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", c.name, once, twice)
		}
		for _, h := range []int{holdBytes(), 256 << 10} {
			for _, n := range []int{len(c.text), 7, 1} {
				var out bytes.Buffer
				s := newStreamHolding(&out, h)
				for at := 0; at < len(c.text); at += n {
					_, _ = s.Write([]byte(c.text[at:min(len(c.text), at+n)]))
				}
				if err := s.Close(); err != nil || out.String() != once {
					t.Errorf("%s: hold %d, pieces of %d: %v, %s", c.name, h, n, err, parted(out.String(), once))
				}
			}
		}
		for at := 0; at < len(c.text); at++ {
			if c.text[at] != '\n' {
				continue
			}
			s := Settled(c.text[:at+1])
			if split := Secrets(c.text[:s]) + Secrets(c.text[s:]); split != once {
				t.Errorf("%s: split at %d of the line ending at %d: %s", c.name, s, at, parted(split, once))
			}
		}
	}
}

// A VALUE WRITTEN WITH AN ESCAPE IS HIDDEN WHOLE: a value whose last letters a
// logger wrote as `\u0032\u0078\u0039` is replaced with them, never
// leaving an escape's tail, and a second pass changes nothing.
//
// Mutation: map a view offset past an escape as though the escape were one
// byte ([jsonString.raw]), and the escapes are shown.
func TestAValueWrittenWithAnEscapeIsHiddenWhole(t *testing.T) {
	t.Parallel()
	text := `{"log": "echo \"Password:\\n\" >> k\necho hunter\u0032\u0078\u0039 >> k\necho done >> k"}` + "\n"
	once := Secrets(text)
	for _, piece := range []string{"hunter", `2`, `x`, `9`} {
		if strings.Contains(once, piece) {
			t.Errorf("%q of the value is shown:\n text %q\n once %q", piece, text, once)
		}
	}
	if twice := Secrets(once); twice != once {
		t.Errorf("a second pass changed\n%q\nto\n%q", once, twice)
	}
}

// ONLY A WELL-FORMED STRING ESCAPED IN LAYERS IS DECODED: a string that holds
// a layer (`\\`, `\"`, `\uXXXX`) and closes at its first unescaped quote on
// its line is read decoded, a lone surrogate as U+FFFD; one with a `\` that
// begins no escape, one unterminated on its line, one whose lines were joined
// into it raw (a quote that closes nothing), one escaping only its breaks, and
// one on a line with no JSON value, are not, and the text's reading alone
// decides them.
//
// Mutation: decode past a `\` that begins no escape or past a raw quote
// ([loggedString]), or take a break's escape for a layer, and one is decoded.
func TestOnlyAWellFormedStringEscapedInLayersIsDecoded(t *testing.T) {
	t.Parallel()
	const key = "Password:"
	for _, c := range []struct {
		name, before, after string
		decoded             string
	}{
		{"a quoted value", `{"log": "`, ` \"x\"\n"}`, key + " \"x\"\n"},
		{"an escaped backslash", `{"log": "`, ` x\\ny"}`, key + " x\\ny"},
		{"a unicode escape", `{"log": "`, ` x \u003e k"}`, key + " x > k"},
		{"a lone surrogate", `{"log": "x \ud83d `, ` \"y\""}`, "x \uFFFD " + key + ` "y"`},
		{"a bad escape", `{"log": "`, ` \"x\" \x41"}`, ""},
		{"unterminated on its line", `{"log": "`, ` \"x\"` + "\n" + `"}`, ""},
		{"lines joined raw", `{"log": "`, ` \"a\"\necho "x" >> k"}`, ""},
		{"only its breaks escaped", `{"log": "`, `\nx\n"}`, ""},
		{"no JSON value", `msg="`, ` \"x\""`, ""},
	} {
		text := c.before + key + c.after
		j, ok := loggedString(text, len(c.before), len(c.before)+len(key))
		switch {
		case c.decoded == "" && ok:
			t.Errorf("%s: decoded %q; want it read only as written", c.name, j.text)
		case c.decoded != "" && (!ok || j.text != c.decoded):
			t.Errorf("%s: decoded %v, %q; want %q", c.name, ok, j.text, c.decoded)
		}
	}
}

// A RANGE MAPPED BACK HOLDS EVERY ESCAPE IT TOUCHES WHOLE: a view offset
// inside an escape's decoded rune maps to the escape's start where a range
// begins and to its end where one ends, and a raw offset inside an escape to
// the rune's start, so no marker splits an escape and changes how a second
// pass decodes the string.
//
// Mutation: map a range's end inside an escape to the escape's start
// ([jsonString.raw]), or a raw offset inside one past it ([jsonString.view]).
func TestARangeMappedBackHoldsEveryEscapeItTouchesWhole(t *testing.T) {
	t.Parallel()
	const before = `{"log": "`
	text := before + `Password: a\u00e9b \"x\""}`
	j, ok := loggedString(text, len(before), len(before)+len("Password:"))
	if !ok || j.text != "Password: a\u00e9b \"x\"" {
		t.Fatalf("decoded %v, %q", ok, j.text)
	}
	esc := len(before) + len("Password: a")
	mid := len("Password: a") + 1
	if from, to := j.raw(mid, false), j.raw(mid, true); from != esc || to != esc+len(`\u00e9`) {
		t.Errorf("inside the rune: a range begins at %d and ends at %d; want %d and %d", from, to, esc, esc+6)
	}
	if v := j.view(esc + 3); v != mid-1 {
		t.Errorf("inside the escape: view %d; want the rune's start, %d", v, mid-1)
	}
}
