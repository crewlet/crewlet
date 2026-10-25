package cliagent_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// streamAll feeds reply to a fresh stream cut at the given byte offsets and
// returns everything it revealed.
func streamAll(reply string, cuts []int) string {
	s := cliagent.NewMessageStream()
	var b strings.Builder
	from := 0
	for _, at := range cuts {
		b.WriteString(s.Write(reply[from:at]))
		from = at
	}
	b.WriteString(s.Write(reply[from:]))
	return b.String()
}

// randomCuts picks sorted byte offsets inside reply.
func randomCuts(r *rand.Rand, reply string) []int {
	if len(reply) < 2 {
		return nil
	}
	cuts := make([]int, r.IntN(8))
	for i := range cuts {
		cuts[i] = 1 + r.IntN(len(reply)-1)
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}

// everyByte cuts reply after every byte, the finest split there is.
func everyByte(reply string) []int {
	cuts := make([]int, 0, len(reply))
	for i := 1; i < len(reply); i++ {
		cuts = append(cuts, i)
	}
	return cuts
}

// runePool is what generated messages are made of: every class of character
// JSON escapes differently, and the characters that look like envelope syntax.
var runePool = []rune{
	'a', 'Z', '7', ' ', '"', '\\', '/', '\n', '\r', '\t', '\b', '\f', 0x01, 0x1f,
	'<', '>', '&', '{', '}', '[', ']', ':', ',', '`',
	'\u00e9', '\u4e2d', '\u2028', '\u2029', '\ufffd', '\U0001f600', '\U0001f389', '\U00010000',
}

func randomMessage(r *rand.Rand) string {
	n := r.IntN(40)
	var b strings.Builder
	for range n {
		b.WriteRune(runePool[r.IntN(len(runePool))])
	}
	return b.String()
}

// encodeString writes s as a JSON string, choosing for every character among
// the spellings JSON allows (literal, short escape, \u escape with either hex
// case, a surrogate pair above the BMP), so the stream meets every spelling
// the model might produce and not only encoding/json's own.
func encodeString(r *rand.Rand, s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		literalOK := c >= 0x20 && c != '"' && c != '\\'
		short := map[rune]string{'"': `\"`, '\\': `\\`, '/': `\/`, '\b': `\b`, '\f': `\f`,
			'\n': `\n`, '\r': `\r`, '\t': `\t`}[c]
		switch pick := r.IntN(3); {
		case pick == 0 && literalOK:
			b.WriteRune(c)
		case pick == 1 && short != "":
			b.WriteString(short)
		default:
			hex := "%04x"
			if r.IntN(2) == 0 {
				hex = "%04X"
			}
			if c > 0xffff {
				hi, lo := utf16.EncodeRune(c)
				fmt.Fprintf(&b, `\u`+hex+`\u`+hex, hi, lo)
			} else {
				fmt.Fprintf(&b, `\u`+hex, c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// randomEnvelope builds one valid envelope around message: tool calls whose
// arguments carry `message` keys and envelope-looking strings, the members in
// either order, whitespace anywhere JSON allows it, sometimes fenced.
func randomEnvelope(r *rand.Rand, message string) string {
	ws := func() string { return []string{"", " ", "\n", "\t ", "\r\n  "}[r.IntN(5)] }
	args := `{` + ws() + `"message":` + ws() + encodeString(r, randomMessage(r)) + `,"n":[1,{"a":"}"},true,null,-2.5e3]` + ws() + `}`
	calls := `[` + ws() + `{"name":"search","arguments":` + args + `}` + ws() + `]`
	if r.IntN(3) == 0 {
		calls = `[]`
	}
	members := []string{
		`"message"` + ws() + `:` + ws() + encodeString(r, message),
		`"tool_calls"` + ws() + `:` + ws() + calls,
	}
	if r.IntN(2) == 0 {
		members[0], members[1] = members[1], members[0]
	}
	body := `{` + ws() + members[0] + ws() + `,` + ws() + members[1] + ws() + `}`
	switch r.IntN(3) {
	case 0:
		return ws() + body + ws()
	case 1:
		return ws() + "```json\n" + body + "\n```" + ws()
	default:
		return "```\n" + ws() + body + "\n```\n"
	}
}

// ANY CUT OF A VALID ENVELOPE STREAMS EXACTLY ITS MESSAGE. The property this
// whole type exists for: however the CLI splits the reply into fragments
// (inside an escape, between the halves of a surrogate pair, through the
// middle of a multi-byte character), the prose revealed is exactly what the
// authoritative parser reads as the message once the call is over, so the
// stream never has to be retracted on a well-formed answer.
func TestEveryCutOfAValidEnvelopeStreamsExactlyItsMessage(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(15, 2026))
	for i := range 400 {
		reply := randomEnvelope(r, randomMessage(r))
		env := cliagent.ParseEnvelope(reply, nil)
		if !env.Parsed {
			t.Fatalf("case %d: the generator built a reply the parser refuses: %q", i, reply)
		}
		for _, cuts := range [][]int{nil, everyByte(reply), randomCuts(r, reply), randomCuts(r, reply)} {
			if got := streamAll(reply, cuts); got != env.Message {
				t.Fatalf("case %d, cuts %v:\nreply    %q\nstreamed %q\nmessage  %q", i, cuts, reply, got, env.Message)
			}
		}
	}
}

// THE DECODING IS encoding/json's, CASE BY CASE. Unpaired surrogates, a pair
// whose second half is not one, an invalid UTF-8 byte and a truncated
// character before the closing quote are each read as encoding/json reads
// them, at every cut, so the view and the parser cannot disagree on them.
func TestEscapesDecodeAsEncodingJSONDecodesThem(t *testing.T) {
	t.Parallel()
	messages := []string{
		`plain`,
		`\"quoted\" \\ back\/slash \b\f\n\r\t`,
		`\u00e9\u4E2D\u2028`,
		`\ud83d\ude00 pair`,
		`lone high \ud800 then text`,
		`lone low \udc00 alone`,
		`high then high \ud800\ud800\udc00`,
		`high then BMP \ud800\u0041`,
		`high then short \ud800\n`,
		`high at the end \ud800`,
		"raw invalid \xff byte",
		"truncated \xe2\x82",
		"raw \xf0\x9f\x98\x80 emoji",
	}
	for _, m := range messages {
		reply := `{"message":"` + m + `","tool_calls":[]}`
		var want struct{ Message string }
		if err := json.Unmarshal([]byte(reply), &want); err != nil {
			t.Fatalf("%q does not decode: %v", reply, err)
		}
		if env := cliagent.ParseEnvelope(reply, nil); env.Message != want.Message {
			t.Fatalf("%q: the parser reads %q and encoding/json %q", reply, env.Message, want.Message)
		}
		for _, cuts := range [][]int{nil, everyByte(reply)} {
			if got := streamAll(reply, cuts); got != want.Message {
				t.Errorf("%q (cuts %d):\nstreamed %q\nwant     %q", m, len(cuts), got, want.Message)
			}
		}
	}
}

// NOTHING OUTSIDE THE MESSAGE STRING IS EVER SHOWN. A tool call's arguments
// carry a `message` key of their own and strings that look like envelope
// syntax; prose before the object means the reply is not one this view
// follows; a reply with no message shows nothing.
func TestNothingOutsideTheMessageStringIsRevealed(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, reply, want string }{
		{"nested message key", `{"tool_calls":[{"name":"post","arguments":{"message":"NOT THIS","x":"\"}"}}],"message":"this"}`, "this"},
		{"message first", `{"message":"first","tool_calls":[{"name":"post","arguments":{"message":"NOT THIS"}}]}`, "first"},
		{"fenced", "```json\n{\"message\": \"in the fence\", \"tool_calls\": []}\n```", "in the fence"},
		{"a synonym", `{"content":"through a synonym","tool_calls":[]}`, "through a synonym"},
		{"escaped key", `{"mess\u0061ge":"escaped key","tool_calls":[]}`, "escaped key"},
		{"non-string synonym passed over", `{"message":null,"text":"the next synonym"}`, "the next synonym"},
		{"number then message", `{"n":12,"message":"after a number"}`, "after a number"},
		{"long key", `{"message_for_someone_else":"NOT THIS","message":"this"}`, "this"},
		{"prose first", `Sure, here it is: {"message":"NOT THIS","tool_calls":[]}`, ""},
		{"no message", `{"tool_calls":[{"name":"post","arguments":{"message":"NOT THIS"}}]}`, ""},
		{"not an object", `["message","NOT THIS"]`, ""},
		{"invalid escape stops", `{"message":"good \q NOT THIS"}`, "good "},
		{"control character stops", "{\"message\":\"good \x01 NOT THIS\"}", "good "},
		{"bad hex stops", `{"message":"good \u12G4 NOT THIS"}`, "good "},
		{"truncated", `{"message":"half an ans`, "half an ans"},
	}
	for _, c := range cases {
		for _, cuts := range [][]int{nil, everyByte(c.reply)} {
			if got := streamAll(c.reply, cuts); got != c.want {
				t.Errorf("%s (cuts %d): streamed %q, want %q", c.name, len(cuts), got, c.want)
			}
		}
	}
}

// firstMessage is an oracle independent of the stream: the first top-level
// member under a message synonym whose value is a string, read with
// encoding/json's tokenizer from a reply that is exactly one JSON object.
func firstMessage(t *testing.T, reply string) (string, bool) {
	t.Helper()
	trimmed := strings.TrimLeft(reply, " \t\r\n")
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if _, err := dec.Token(); err != nil {
		return "", false
	}
	synonyms := []string{"message", "content", "text", "response"}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return "", false
		}
		key, _ := tok.(string)
		if !slices.Contains(synonyms, key) || !bytes.HasPrefix(value, []byte(`"`)) {
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return "", false
		}
		return s, true
	}
	return "", true
}

// FuzzMessageStream holds the stream to three facts on ANY input: the cut
// never changes what is revealed; what is revealed is valid UTF-8; and on a
// reply that is one valid JSON object, it is exactly the first string under a
// message synonym, as encoding/json's own tokenizer reads it.
func FuzzMessageStream(f *testing.F) {
	f.Add(`{"message":"hi","tool_calls":[]}`, uint64(1))
	f.Add("```json\n{\"message\":\"\\ud83d\\ude00\",\"tool_calls\":[{\"name\":\"a\",\"arguments\":{\"message\":\"x\"}}]}\n```", uint64(2))
	f.Add(`{"text":1,"content":"\u00e9\ud800x"}`, uint64(3))
	f.Add("{\"message\":\"a\xffb\"}", uint64(4))
	f.Add(`prose {"message":"no"}`, uint64(5))
	f.Fuzz(func(t *testing.T, reply string, seed uint64) {
		r := rand.New(rand.NewPCG(seed, 0))
		whole := streamAll(reply, nil)
		if !utf8.ValidString(whole) {
			t.Fatalf("revealed invalid UTF-8 %q from %q", whole, reply)
		}
		for _, cuts := range [][]int{everyByte(reply), randomCuts(r, reply)} {
			if got := streamAll(reply, cuts); got != whole {
				t.Fatalf("cuts %v changed what %q reveals: %q, whole %q", cuts, reply, got, whole)
			}
		}
		if want, ok := firstMessage(t, reply); ok && whole != want {
			t.Fatalf("%q revealed %q, want %q", reply, whole, want)
		}
	})
}
