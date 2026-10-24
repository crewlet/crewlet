package stream_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// A FRAME ESCAPES MARKUP AT ITS ONE WRITER, as an HTTP answer does
// (internal/api/httpjson, "Escaped at the one writer, read by decoders"): a
// payload's own string and the bytes it embeds come out spelled alike, and a
// client decodes them back as written. Mutation: EncodeFrame through
// internal/plainjson leaves the embedded `<b>&` raw and is red.
func TestAFrameEscapesMarkupAtItsOneWriter(t *testing.T) {
	t.Parallel()
	frame, err := stream.EncodeFrame(stream.Envelope{Kind: stream.Kind("event"),
		Data: map[string]any{
			"text":     "a < b & c > d",
			"embedded": json.RawMessage(`"<b>&</b>"`),
		}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(frame.Raw()), "<>&") {
		t.Errorf("the frame carried markup unescaped: %s", frame.Raw())
	}
	var back struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(frame.Raw(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Data["text"] != "a < b & c > d" || back.Data["embedded"] != "<b>&</b>" {
		t.Errorf("a client read back %q, want the text as written", back.Data)
	}
}
