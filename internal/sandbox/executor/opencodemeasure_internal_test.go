package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// decodedExportInput is the meter's reading of an export as encoding/json's
// stream decoder made it, every value it read held whole: the oracle the
// scanner's reading is held to ([meterExport]'s figure).
func decodedExportInput(r io.Reader) (int64, error) {
	dec := json.NewDecoder(r)
	if err := decodedDelim(dec, '{'); err != nil {
		return 0, err
	}
	var last int64
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, err
		}
		if key != "messages" {
			if err = decodedSkip(dec); err != nil {
				return 0, err
			}
			continue
		}
		if err = decodedDelim(dec, '['); err != nil {
			return 0, err
		}
		for dec.More() {
			if err = decodedDelim(dec, '{'); err != nil {
				return 0, err
			}
			var w openCodeMeasuredWire
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return 0, err
				}
				switch key {
				case "type":
					err = dec.Decode(&w.Type)
				case "time":
					err = dec.Decode(&w.Time)
				case "tokens":
					err = dec.Decode(&w.Tokens)
				case "error":
					err = dec.Decode(&w.Error)
				default:
					err = decodedSkip(dec)
				}
				if err != nil {
					return 0, err
				}
			}
			if err = decodedDelim(dec, '}'); err != nil {
				return 0, err
			}
			m, _, err := w.message()
			if err != nil {
				return 0, err
			}
			if n, measured := measuredPrompt(m); measured {
				last = n
			}
		}
		if err = decodedDelim(dec, ']'); err != nil {
			return 0, err
		}
	}
	return last, nil
}

// exportFigure is the floor's figure the meter reads off an export: one walk
// of it ([meterExport]), the count beside it unread here.
func exportFigure(t *testing.T, r io.Reader) (int64, error) {
	read, err := meterExport(t.Context(), r, "2.3.2", nil)
	return read.figure, err
}

// decodedDelim reads the decoder's next token, which must be want: the
// oracle's own, apart from the walk's, which reads a jsontext decoder.
func decodedDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return fmt.Errorf("the export holds %T where %q belongs", tok, string(want))
	}
	return nil
}

// decodedSkip passes over the decoder's next value, a token at a time.
func decodedSkip(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// THE METER READS AN EXPORT AS IT DID, A STRING AT A TIME NO LONGER: over the
// real export each release this build drives printed, and exports made of its
// messages with every way a box can write one (a key written twice, a key in
// another case, a null, an error, a value of another type, a message that is
// no object, content of any length, a document cut short or followed by
// more), the scanner's reading answers what encoding/json's decoder answered,
// a figure or none.
//
// Mutation: read a name by its first bytes alone, take the first of a key
// written twice, or read past a message that does not read, and an export
// here is read otherwise.
func TestTheMeterReadsAnExportAsTheDecoderDid(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 9))
	var exports []string
	for _, version := range []string{"2.3.2"} {
		b, err := os.ReadFile("testdata/opencode/" + version + "/export.json")
		if err != nil {
			t.Fatal(err)
		}
		exports = append(exports, string(b))
	}
	settled := func(input int) string {
		return fmt.Sprintf(`{"id":"msg_%d","type":"assistant","time":{"created":1,"completed":2},`+
			`"tokens":{"input":%d,"output":5,"cache":{"read":200,"write":0}}}`, input, input)
	}
	variants := []string{
		settled(10),
		`{"type":"assistant","type":"user","time":{"completed":2},"tokens":{"input":7,"cache":{"read":1,"write":1}}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7,"INPUT":99,"cache":{"read":1,"write":1}}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"error":null}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"error":{"name":"x"}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":"lots"}`,
		`{"type":"assistant","time":{"completed":"two"},"tokens":{"input":7}}`,
		`{"type":"assistant","time":null,"tokens":{"input":7}}`,
		`{"type":["assistant"],"time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"assistant","Time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"content":[{"text":"` +
			strings.Repeat("ab", 70<<10) + `"}]}`,
		`{"typ\u0065":"assistant","time":{"completed":2},"tokens":{"input":8}}`,
		`"no object"`,
		`{"type":"user","text":"` + strings.Repeat("x", 3<<10) + `"}`,
	}
	for range 300 {
		var messages []string
		for range 1 + r.IntN(6) {
			messages = append(messages, variants[r.IntN(len(variants))])
		}
		doc := `{"info":{"id":"ses_1"},"messages":[` + strings.Join(messages, ",") + `]}`
		switch r.IntN(8) {
		case 0:
			doc = doc[:r.IntN(len(doc))]
		case 1:
			doc += ` {"more":1}`
		case 2:
			doc = `{"messages":[` + settled(3) + `],"info":{"x":[1,2]},"messages":[` + strings.Join(messages, ",") + `]}`
		}
		exports = append(exports, doc)
	}
	for _, doc := range exports {
		want, wantErr := decodedExportInput(strings.NewReader(doc))
		got, gotErr := exportFigure(t, bytes.NewReader(gzipOf(t, doc)))
		if got != want || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("the export %.200q reads %d (%v), want the decoder's %d (%v)", doc, got, gotErr, want, wantErr)
		}
	}
}

// A MESSAGE'S VALUE PAST THE METER'S BOUND IS READ FOR WHAT IT IS: an error
// past it is one, so its step measured nothing and the step before it stands;
// a type, a time or tokens past it is no message OpenCode writes, so the export
// reads as none, never as an older figure.
//
// Mutation: read an error past the bound as none, or pass over a time past it,
// and a step here is measured.
func TestAMessagesValuePastTheMetersBoundIsReadForWhatItIs(t *testing.T) {
	t.Parallel()
	older := `{"type":"assistant","time":{"completed":2},"tokens":{"input":40}}`
	long := strings.Repeat("e", exportValueMaxBytes)
	for name, tc := range map[string]struct {
		newest string
		want   int64
		failed bool
	}{
		"an error past the bound": {`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},` +
			`"error":{"message":"` + long + `"}}`, 40, false},
		"a time past the bound": {`{"type":"assistant","time":{"completed":2,"note":"` + long + `"},` +
			`"tokens":{"input":7}}`, 0, true},
		"tokens past the bound": {`{"type":"assistant","time":{"completed":2},"tokens":{"input":7,"note":"` +
			long + `"}}`, 0, true},
	} {
		doc := `{"messages":[` + older + `,` + tc.newest + `]}`
		got, err := exportFigure(t, bytes.NewReader(gzipOf(t, doc)))
		if got != tc.want || (err != nil) != tc.failed || tc.failed && !errors.Is(err, errNotExport) {
			t.Errorf("%s: read %d (%v), want %d, failed %v", name, got, err, tc.want, tc.failed)
		}
	}
}
