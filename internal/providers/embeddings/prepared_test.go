package embeddings_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// strayBytes is text that is not valid UTF-8 in every way a string can fail
// to be: lone continuation bytes, a lead byte with nothing after it, a
// sequence cut short, an encoded surrogate and an overlong encoding — beside a
// literal U+FFFD, which IS valid and must survive as itself.
var strayBytes = map[string]string{
	"continuation run":  "\x80\x80\x80",
	"lone lead":         "a\xe2b",
	"cut-short":         "deploy \xe2\x82 failed",
	"encoded surrogate": "\xed\xa0\x80",
	"overlong slash":    "\xc0\xaf",
	"literal U+FFFD":    "keep � as it is \x80",
	"mixed with spaces": "  \x80\n\t\x81  word\xff  ",
}

// THE PREPARED TEXT IS THE TEXT THE REQUEST CARRIES, to the byte.
//
// The SDK encodes a request with encoding/json, which replaces each byte that
// does not begin a valid encoding with U+FFFD. A bound, a digest or a cut
// measured on anything else is measured on a text no server ever decoded —
// so Prepare must already be what that encoding round-trips to: valid UTF-8,
// one U+FFFD per stray byte (never one per run), and unchanged by a second
// preparation.
func TestThePreparedTextIsTheTextTheRequestCarries(t *testing.T) {
	t.Parallel()
	for name, text := range strayBytes {
		t.Run(name, func(t *testing.T) {
			got := embeddings.Prepare(text)
			if !utf8.ValidString(got) {
				t.Fatalf("Prepare(%q) = %q is not valid UTF-8", text, got)
			}
			wire, err := json.Marshal(strings.Join(strings.Fields(text), " "))
			if err != nil {
				t.Fatalf("encoding the collapsed text: %v", err)
			}
			var decoded string
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatalf("decoding the request's string: %v", err)
			}
			if got != decoded {
				t.Errorf("Prepare(%q) = %q, but a server decodes %q from the request",
					text, got, decoded)
			}
			if again := embeddings.Prepare(got); again != got {
				t.Errorf("Prepare is not idempotent on %q: %q", got, again)
			}
		})
	}
	// ONE PER BYTE: three stray bytes are three replacements.
	if got, want := embeddings.Prepare("\x80\x80\x80"), strings.Repeat("�", 3); got != want {
		t.Errorf("Prepare of three stray bytes = %q, want %q", got, want)
	}
}

// A STRAY BYTE IS MEASURED AS THE SERVER RECEIVES IT — three bytes of U+FFFD
// — so an input that fits the bound only as the bytes handed to the encoder is
// refused here, before any request, rather than sent at three times its
// measured size.
func TestAStrayByteIsMeasuredAsTheServerReceivesIt(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	p := e.provider(t, small)

	// Ten stray bytes: ten as handed in, thirty as sent, past sixteen.
	_, err := p.Embed(t.Context(), strings.Repeat("\x80", 10))
	var long *embeddings.TooLongError
	if !errors.As(err, &long) || long.Bytes != 30 || long.Limit != 16 {
		t.Fatalf("Embed of ten stray bytes = %v, want a TooLongError of 30 bytes against 16", err)
	}
	if len(e.sent()) != 0 {
		t.Fatal("an input past the bound as sent reached the network")
	}
	if _, err := p.EmbedBatch(t.Context(), []string{"ok", strings.Repeat("\x80", 10)}); !errors.As(err, &long) || long.Index != 1 {
		t.Fatalf("EmbedBatch with ten stray bytes = %v, want input 1 refused", err)
	}
	if len(e.sent()) != 0 {
		t.Fatal("a batch with an input past the bound as sent reached the network")
	}

	// Five: fifteen bytes as sent, inside the bound — and what the server
	// decoded is the prepared text, byte for byte.
	text := strings.Repeat("\x80", 5)
	if _, err := p.Embed(t.Context(), text); err != nil {
		t.Fatalf("Embed of five stray bytes: %v", err)
	}
	sent := e.sent()
	if len(sent) != 1 || sent[0][0] != embeddings.Prepare(text) || len(sent[0][0]) != 15 {
		t.Fatalf("the server decoded %q, want the prepared %q (15 bytes)", sent, embeddings.Prepare(text))
	}
}

// A RUN OF STRAY BYTES LONGER THAN THE BOUND IS CUT, AND THE CUT ENDS.
//
// The case a narrow local model meets from a search: a query of 300 raw 0x80
// bytes (what `?q=%80%80…` decodes to) against a 240-byte bound. No prefix of
// a run of continuation bytes ends on a character, so a cut over the raw run
// took nothing on every pass, and Chunks appended empty pieces until the node
// ran out of memory. Over the prepared text every piece holds whole
// characters, and every one is non-empty, inside the bound and valid.
func TestARunOfStrayBytesLongerThanTheBoundIsCutAndTheCutEnds(t *testing.T) {
	t.Parallel()
	const bound = 240
	text := strings.Repeat("\x80", 300)
	// THE PRECONDITION FIRST: Chunks' cut ends only on valid UTF-8, so a
	// preparation that let the stray bytes through would hang the calls
	// below rather than fail them. Checked here, it fails at once.
	prepared := embeddings.Prepare(text)
	if !utf8.ValidString(prepared) {
		t.Fatalf("Prepare left %d bytes of invalid UTF-8; Chunks would never end on it", len(prepared))
	}

	chunks := embeddings.Chunks(text, bound)
	if len(chunks) < 2 {
		t.Fatalf("Chunks = %d pieces for %d prepared bytes at a %d-byte bound",
			len(chunks), len(prepared), bound)
	}
	for i, chunk := range chunks {
		switch {
		case chunk == "":
			t.Errorf("piece %d is empty", i)
		case len(chunk) > bound:
			t.Errorf("piece %d is %d bytes, past %d", i, len(chunk), bound)
		case !utf8.ValidString(chunk):
			t.Errorf("piece %d is not valid UTF-8", i)
		}
	}
	if strings.Join(chunks, "") != prepared {
		t.Error("the pieces do not carry the prepared text, in order")
	}

	opening := embeddings.Opening(text, bound)
	switch {
	case opening == "":
		t.Error("Opening of the run is empty")
	case len(opening) > bound:
		t.Errorf("Opening is %d bytes, past %d", len(opening), bound)
	case !utf8.ValidString(opening):
		t.Error("Opening is not valid UTF-8")
	case !strings.HasPrefix(prepared, opening):
		t.Error("Opening is not the start of the prepared text")
	}

	fake := embeddings.NewFake(8)
	fake.SetLimits(embeddings.Limits{InputBytes: bound, BatchInputs: 16, BatchBytes: 4096})
	vector, err := embeddings.EmbedWhole(t.Context(), fake, text)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	if len(vector) != 8 {
		t.Fatalf("EmbedWhole = a %d-wide vector, want 8", len(vector))
	}
	for _, request := range fake.Requests() {
		for _, input := range request {
			if input == "" || len(input) > bound || !utf8.ValidString(input) {
				t.Errorf("EmbedWhole sent an input of %d bytes (valid %v) at a %d-byte bound",
					len(input), utf8.ValidString(input), bound)
			}
		}
	}
}
