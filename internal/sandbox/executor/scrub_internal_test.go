package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// THE DIGEST IS POSIX CKSUM'S, byte for byte as the box's `cksum` prints it for
// its standard input — the digest a box's restore compares a carried archive
// by, so one the engine computes is one the box recognises. Over nothing, a
// byte, the CRC check string, and lengths whose count needs one, two and three
// bytes to fold in.
//
// Mutation: fold the length in most significant byte first, leave the count
// out, skip the complement, or use hash/crc32's reflected table, and every
// digest below differs from cksum's.
func TestTheDigestIsPOSIXCksums(t *testing.T) {
	t.Parallel()
	for _, input := range [][]byte{
		nil, []byte("a"), []byte("123456789"),
		bytes.Repeat([]byte{0xff}, 255), bytes.Repeat([]byte("x"), 256), bytes.Repeat([]byte("yz"), 40000),
	} {
		sum := &cksum{}
		_, _ = sum.Write(input)
		cmd := exec.Command("cksum")
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cksum: %v", err)
		}
		if want := strings.TrimSpace(string(out)); sum.String() != want {
			t.Errorf("the digest of %d bytes is %q, cksum says %q", len(input), sum.String(), want)
		}
	}
}

// A TRANSCRIPT LINE STAYS JSON, AND LOSES EVERY SECRET IN IT. A value the engine
// named, raw and JSON-escaped; a key's lines inside one string, which only the
// decoded string has as lines; a password's key and its value, two strings
// apart, which only the whole line has together; and a value under a
// credential's own key — all gone, the line still one JSON value, and a line
// that is not JSON redacted as text.
//
// Mutation: redact the raw line alone, the strings alone, or keep a whole-line
// redaction that broke the JSON, and a case here goes red.
func TestATranscriptLineStaysJSONAndLosesEverySecret(t *testing.T) {
	t.Parallel()
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEowIBAAKCAQEAu7Jx9f3Lm2Pq8Rt5Vw1Yz4Bc6De9Fg0Hi3Jk5Lm7No9Pq2Rs4\n", 6) +
		"-----END RSA PRIVATE KEY-----"
	encodedPEM, _ := json.Marshal(pem)
	s := newScrubber([]string{"opaque-engine-value", `with"quote-value`})
	for name, tc := range map[string]struct {
		line string
		gone []string
	}{
		"a value the engine named": {
			line: `{"text":"the token opaque-engine-value here"}`, gone: []string{"opaque-engine-value"}},
		"its JSON-escaped form": {
			line: `{"text":"see with\"quote-value now"}`, gone: []string{`with\"quote-value`}},
		"a key inside one string": {
			line: `{"content":` + string(encodedPEM) + `}`, gone: []string{"MIIEowIBAAKCAQEAu7Jx9f3Lm2"}},
		"a value under a credential's key": {
			line: `{"input":{"api_key":"looks-like-nothing-at-all"}}`, gone: []string{"looks-like-nothing-at-all"}},
	} {
		got := rewriteLine(tc.line, s)
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: the line is no longer JSON: %q", name, got)
		}
		for _, secret := range tc.gone {
			if strings.Contains(got, secret) {
				t.Errorf("%s: %q survived: %q", name, secret, got)
			}
		}
	}
	if got := rewriteLine(`{"text":"cut off mid opaque-engine-value`, s); strings.Contains(got,
		"opaque-engine-value") {
		t.Errorf("a line that is not JSON kept its secret: %q", got)
	}
}

// A VALUE TOO SHORT TO BE A CREDENTIAL IS NEVER TAKEN OUT BY VALUE, and a long
// one whose line holds another is taken out whole.
//
// Mutation: drop the floor, and a transcript loses every occurrence of a
// three-letter word; drop the longest-first order, and a value holding another
// leaves its tail behind.
func TestTheScrubTakesOutCredentialsNotWords(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{"yes", "inner-secret", "outer-inner-secret-value"})
	got := s.apply("yes: outer-inner-secret-value and inner-secret")
	if want := "yes: " + scrubMarker + " and " + scrubMarker; got != want {
		t.Errorf("scrubbed to %q, want %q", got, want)
	}
}

// A TAR IS WRITTEN WITH EVERY MEMBER'S SIZE AS REWRITTEN, read twice: the second
// reading writes exactly what the first counted, and an archive that changed
// between them is refused rather than written with headers that lie.
//
// Mutation: write the first reading's header sizes from the members' own, or
// skip the check that the second reading wrote what the first counted, and the
// tar is unreadable or the changed archive passes.
func TestATarIsWrittenWithEachMembersSizeAsRewritten(t *testing.T) {
	t.Parallel()
	archive := tarOf(t, map[string]string{
		"projects/p/s.jsonl":        `{"text":"opaque-engine-value"}` + "\n",
		"projects/p/s/tool/out.txt": "out opaque-engine-value\n",
	})
	s := newScrubber([]string{"opaque-engine-value"})
	var out bytes.Buffer
	err := scrubTar(bytes.NewReader(archive), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	}, &out, s)
	if err != nil {
		t.Fatalf("scrubTar: %v", err)
	}
	zr, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the scrubbed tar does not read: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil || int64(len(body)) != h.Size || strings.Contains(string(body), "opaque-engine-value") {
			t.Errorf("%s: %q (header says %d bytes), %v", h.Name, body, h.Size, err)
		}
	}
	changed := tarOf(t, map[string]string{
		"projects/p/s.jsonl":        `{"text":"a different and longer transcript"}` + "\n",
		"projects/p/s/tool/out.txt": "out opaque-engine-value\n",
	})
	err = scrubTar(bytes.NewReader(archive), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(changed)), nil
	}, io.Discard, s)
	if err == nil {
		t.Error("an archive that changed between its two readings was written")
	}
}

// tarOf is a gzipped tar of files, in name order.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
