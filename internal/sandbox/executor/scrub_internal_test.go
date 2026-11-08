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
	"time"
)

// shapedKey is a credential only its shape gives away.
const shapedKey = "ghp_" + "Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7d"

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
// named; a key's lines inside one string, which only the decoded string has as
// lines; a string or a number under a credential's own key; and a cloud
// session a run obtained by itself, as a tool's result holds it — its key id
// by its shape, its secret and its token by their keys, whether the result
// names them as members or a command printed them as text, and a Google
// access token by its shape — all gone, the line still one JSON value. A line
// that is not JSON — a torn last line — is scrubbed and redacted as text, the
// engine's value taken out in the form a JSON string escapes it in, which is
// the only form a torn line holds it in.
//
// Mutation: redact the raw line alone, the strings alone, write a key as it
// came, keep a number under a credential's key, take out only a value's raw
// form, rewrite a line of two values as one, drop a session token's words
// from a credential's, or a session's keys from redact's, and a case here goes
// red.
//
// AND A KEY IS A STRING LIKE ANY OTHER: a tool's structured result can name a
// member by anything, a secret included.
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
		"a key the engine named": {
			line: `{"toolUseResult":{"opaque-engine-value":1}}`, gone: []string{"opaque-engine-value"}},
		"a key only its shape gives away": {
			line: `{"env":{"` + shapedKey + `":"set"}}`, gone: []string{shapedKey}},
		"a number under a credential's key": {
			line: `{"input":{"db_password":73519046284}}`, gone: []string{"73519046284"}},
		"an AWS session under its keys": {
			line: `{"toolUseResult":{"Credentials":{"AccessKeyId":"ASIAEXAMPLESESSION01",` +
				`"SecretAccessKey":"secret-access-key-filler-0123456789",` +
				`"SessionToken":"session-token-filler-0123456789abcdef","Expiration":"2026-11-04T12:00:00Z"}}}`,
			gone: []string{"ASIAEXAMPLESESSION01", "secret-access-key-filler", "session-token-filler"}},
		"a session's token in a credentials file's spelling": {
			line: `{"input":{"aws_session_token":"session-token-filler-0123456789abcdef"}}`,
			gone: []string{"session-token-filler"}},
		"a session's token as a request's header": {
			line: `{"headers":{"X-Amz-Security-Token":"session-token-filler-0123456789abcdef"}}`,
			gone: []string{"session-token-filler"}},
		"a Google access token in a tool's output": {
			line: `{"stdout":"ya29.c.c0ASRK0GaBcDeFgHiJkLmNoPqRsTuVwXyZ-_12\n"}`,
			gone: []string{"ASRK0GaBcDeFgHiJkLmNoPqRsTuVwXyZ"}},
		"an AWS session a tool printed as text": {
			line: `{"stdout":"export AWS_ACCESS_KEY_ID=ASIAEXAMPLESESSION01\n` +
				`export AWS_SECRET_ACCESS_KEY=Qw3/Qw3/Qw3/Qw3/Qw3/Qw3/Qw3/Qw3/Qw3/Qw3/\n` +
				`export AWS_SESSION_TOKEN=Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+Ab3+==\n"}`,
			gone: []string{"ASIAEXAMPLESESSION01", "Qw3/Qw3/", "Ab3+Ab3+"}},
	} {
		got := rewriteLine(tc.line, s, testBounds)
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: the line is no longer JSON: %q", name, got)
		}
		for _, secret := range tc.gone {
			if strings.Contains(got, secret) {
				t.Errorf("%s: %q survived: %q", name, secret, got)
			}
		}
	}
	if got := rewriteLine(`{"text":"cut off mid opaque-engine-value`, s, testBounds); strings.Contains(got,
		"opaque-engine-value") {
		t.Errorf("a line that is not JSON kept its secret: %q", got)
	}
	// A TORN LINE HOLDS A VALUE AS ITS STRING ESCAPED IT, and nothing decodes
	// the string before the scrub reads it: only the escaped form is there.
	if got := rewriteLine(`{"text":"cut off see with\"quote-value`, s, testBounds); strings.Contains(got,
		"quote-value") {
		t.Errorf("a torn line kept the escaped form of a value: %q", got)
	}
	// TWO VALUES ON ONE LINE are not the one JSON value a transcript's line
	// is: the line is redacted as the text it is, rather than split in two.
	if got := rewriteLine(`{"a":"opaque-engine-value"} {"b":1}`, s, testBounds); strings.Contains(got,
		"opaque-engine-value") || strings.Contains(got, "\n") || !strings.HasSuffix(got, `} {"b":1}`) {
		t.Errorf("a line of two values was kept as %q", got)
	}
}

// testBounds is the scrub of an archive at a session's bound.
var testBounds = boundsFor(32 << 20)

// A VALUE TOO SHORT TO BE A CREDENTIAL IS NEVER TAKEN OUT BY VALUE, and a long
// one that begins with another is taken out whole.
//
// Mutation: drop the floor, and a transcript loses every occurrence of a
// three-letter word; drop the longest-first order, and a value that begins with
// another — the one place a replacer's order decides which it takes — leaves
// its tail behind.
func TestTheScrubTakesOutCredentialsNotWords(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{"yes", "inner-secret", "inner-secret-value-tail"})
	got := s.apply("yes: inner-secret-value-tail and inner-secret")
	if want := "yes: " + scrubMarker + " and " + scrubMarker; got != want {
		t.Errorf("scrubbed to %q, want %q", got, want)
	}
}

// A CREDENTIAL'S KEY IS READ HOWEVER ITS WORDS ARE JOINED, AND A TRANSCRIPT'S
// OWN KEYS ARE NO CREDENTIAL'S. A cloud session's token sits under a key of its
// own wherever it is written — `SessionToken` in what STS answers,
// `sessionToken` in what Roles Anywhere does, `aws_session_token` in AWS's
// credentials file, and `X-Amz-Security-Token`, AWS's own name for it, as a
// request's header — and each spelling is a credential's key, as an API key's
// and a token's are under a header's hyphens: `x-api-key`, `X-Auth-Token`,
// GitLab's `PRIVATE-TOKEN`. So are the other names a credential carries: the
// token that refreshes a login, the authorization a request carries, a
// client's credentials, an API token, a bearer token, and an OpenID Connect ID
// token under its own words, however they are joined; and what a Docker config
// holds for a registry, its `auth` (the user and password, encoded), its
// identity token and its registry token.
//
// A harness counts its tokens and names its sessions under keys of its own,
// an access key's id names no secret, a cache's key no credential, and a
// session's expiry is no secret: none of those is one. Nor is a count whose
// name spells `idtoken` by chance (`paid_tokens`), since an ID token's `id` is
// a word of its own, nor a commit's author or a server's OAuth settings, since
// only the whole key is read as `auth`.
//
// Mutation: drop any one name from the list — the session's, the refresh
// token's, the authorization's, the credential's, the API token's, the auth
// token's, the private token's, the bearer token's — and its rows are no
// credential's; read the words in the key with its separators kept, and the
// headers' and the credentials file's are not; read `token` or `key` anywhere
// in a key, and every transcript's usage counts, an access key's id and a
// cache's key are; read an ID token's `id` anywhere, and the paid tokens are,
// and as no word at all, the ID tokens are not; drop `auth`, or Docker's
// identity or registry token, and a Docker config's rows are no credential's,
// and read `auth` anywhere in a key, and the author and the OAuth settings are.
func TestACredentialsKeyIsReadHoweverItsWordsAreJoined(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"SessionToken":            true,
		"sessionToken":            true,
		"aws_session_token":       true,
		"AWS_SESSION_TOKEN":       true,
		"X-Amz-Security-Token":    true,
		"AWS_SECURITY_TOKEN":      true,
		"SecretAccessKey":         true,
		"access_token":            true,
		"refresh_token":           true,
		"Proxy-Authorization":     true,
		"client_credentials":      true,
		"x-api-key":               true,
		"api_token":               true,
		"X-Auth-Token":            true,
		"PRIVATE-TOKEN":           true,
		"bearer_token":            true,
		"id_token":                true,
		"idToken":                 true,
		"OIDC_ID_TOKEN":           true,
		"Private-Key":             true,
		"token":                   true,
		"auth":                    true,
		"Auth":                    true,
		"identitytoken":           true,
		"IdentityToken":           true,
		"registrytoken":           true,
		"RegistryToken":           true,
		"sessionId":               false,
		"session_id":              false,
		"input_tokens":            false,
		"cache_read_input_tokens": false,
		"tokens":                  false,
		"Expiration":              false,
		"AccessKeyId":             false,
		"keyId":                   false,
		"cache_key":               false,
		"paid_tokens":             false,
		"invalid_tokens":          false,
		"author":                  false,
		"authors":                 false,
		"oauth_provider":          false,
		"auth_method":             false,
	} {
		if got := credentialKey(key); got != want {
			t.Errorf("credentialKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// A KEY THE ENGINE HANDED A BOX IS TAKEN OUT A LINE AT A TIME, AND ITS ARMOUR
// STAYS, so every other key of its kind in the session is still one the shape
// pass reads. A deploy key a setup step wrote from a reference is handed whole;
// its BEGIN and END lines are every OPENSSH key's, and taken out by value they
// were taken off a second key the run printed, whose body the shape pass then
// could not place and kept.
//
// Mutation: take the armour lines out by value, and the printed key's body
// survives below.
func TestAHandedKeysArmourStaysForTheShapePass(t *testing.T) {
	t.Parallel()
	armoured := func(body string) string {
		return "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat(body+"\n", 6) +
			"-----END OPENSSH PRIVATE KEY-----"
	}
	handed := armoured("b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz")
	printed := armoured("QyNTUxOQAAACDhRunsOwnPrintedKeyBodyLineOfBase64Chars0123456789ab")
	s := newScrubber([]string{handed})
	for name, text := range map[string]string{
		"the handed key":        handed,
		"a key the run printed": printed,
	} {
		got := rewriteLine(`{"type":"tool_result","content":`+jsonString(t, "cat k\n"+text)+`}`, s, testBounds)
		if strings.Contains(got, "QyNTUxOQAAACDhRunsOwnPrintedKeyBodyLineOfBase64Chars0123456789ab") ||
			strings.Contains(got, "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz") {
			t.Errorf("%s: a line of a key's body survived: %q", name, got)
		}
	}
}

// jsonString is s as a JSON string.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
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
	err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s, testBounds)
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
	err = scrubTar(bytes.NewReader(archive), reopenOf(changed), io.Discard, s, testBounds)
	if err == nil {
		t.Error("an archive that changed between its two readings was written")
	}
}

// AN ARCHIVE THAT CHANGED NAMES ITS MEMBER SHORT, whichever way it changed.
// The name is the archive's, which the box wrote, at up to a MiB a member, and
// the failure becomes the reason the run's session was not kept, which the
// run's row, the launch's record and a box host's reply to the node that asked
// for the read carry: so it quotes the name as a transcript line quotes a path
// — its first line, redacted and held to [transcriptDetailLimit]
// ([memberNamed]). The two changes the second reading can meet are each named
// where they are met, a member's size and its kind, and each is a case.
//
// Mutation: quote the member's name whole where either change is met, or hold
// it to more than a transcript line quotes of a path, and a case goes red.
func TestAChangedArchiveNamesItsMemberShort(t *testing.T) {
	t.Parallel()
	name := "projects/p/" + strings.Repeat("n", 64<<10) + ".txt"
	s := newScrubber([]string{"opaque-engine-value"})
	for change, readings := range map[string][2][]byte{
		// SHORTER the second time, so the rewrite ends before the size the
		// first reading counted.
		"its size": {
			tarOf(t, map[string]string{name: "out opaque-engine-value and the rest of it\n"}),
			tarOf(t, map[string]string{name: "out\n"}),
		},
		// A FILE the first time and a directory under the same name the
		// second.
		"its kind": {
			tarOfMembers(t, []tarMember{{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: "out\n"}}),
			tarOfMembers(t, []tarMember{{h: tar.Header{Name: name, Mode: 0o700, Typeflag: tar.TypeDir}}}),
		},
	} {
		err := scrubTar(bytes.NewReader(readings[0]), reopenOf(readings[1]), io.Discard, s, testBounds)
		if err == nil {
			t.Errorf("%s: an archive that changed between its two readings was written", change)
			continue
		}
		_, quoted, _ := strings.Cut(err.Error(), "changed its member ")
		quoted, _, found := strings.Cut(quoted, " while it was read")
		if want := firstLine(name, transcriptDetailLimit); !found || quoted != want ||
			len(quoted) > transcriptDetailLimit {
			t.Errorf("%s: the failure names the member as %d bytes, %.60q…; want the %d bytes a transcript "+
				"line quotes of it, %.60q…", change, len(quoted), quoted, len(want), want)
		}
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

// tarMember is one member of a tar a case writes, header and all.
type tarMember struct {
	h    tar.Header
	body string
}

// tarOfMembers is a gzipped tar of members, in order, each header as given and
// its size its body's.
func tarOfMembers(t *testing.T, members []tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		h := m.h
		h.Size = int64(len(m.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
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

// gzipOf is text gzipped.
func gzipOf(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// reopenOf opens archive again from its start, as a box's file is reopened.
func reopenOf(archive []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(archive)), nil }
}

// THE SCRUB HOLDS NOTHING PAST ITS BOUNDS, whatever the box's own code wrote: a
// gzip that expands past the plaintext one reading admits — a tar's or an
// export's — a line, a string or a run of blanks the decoder would hold past
// the piece, a stretch a redaction holds unsettled past it (a key with no END),
// and a value nested past the rewrite's depth are each refused; the same
// archive within its bounds, and a value at the depth, are not.
//
// Mutation: drop the plaintext's bound, the line's, the token's, the blanks',
// the held stretch's or the depth's, and something below is read to its end.
func TestTheScrubHoldsNothingPastItsBounds(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	small := scrubBounds{plain: 1 << 20, piece: 64 << 10}
	bomb := tarOf(t, map[string]string{
		"projects/p/s.jsonl": strings.Repeat(`{"text":"ordinary"}`+"\n", (2<<20)/20),
	})
	if err := scrubTar(bytes.NewReader(bomb), reopenOf(bomb), io.Discard, s, small); !errors.Is(err, errScrubBound) {
		t.Errorf("a tar expanding past its plaintext's bound: %v, want errScrubBound", err)
	}
	if err := scrubTar(bytes.NewReader(bomb), reopenOf(bomb), io.Discard, s, testBounds); err != nil {
		t.Errorf("the same tar within its bounds: %v", err)
	}
	export := gzipOf(t, `{"messages":[`+strings.Repeat(`"ordinary",`, (2<<20)/11)+`"end"]}`)
	if err := scrubDocument(bytes.NewReader(export), io.Discard, s, small); !errors.Is(err, errScrubBound) {
		t.Errorf("an export expanding past its plaintext's bound: %v, want errScrubBound", err)
	}
	long := strings.Repeat("a", 128<<10)
	for name, files := range map[string]map[string]string{
		"a transcript line past the piece":   {"projects/p/s.jsonl": `{"text":"` + long + `"}` + "\n"},
		"a document's string past the piece": {"tasks/s/1.json": `{"text":"` + long + `"}`},
		"a key with no END held past the piece": {"projects/p/s/tool-results/out.txt": "-----BEGIN RSA PRIVATE KEY-----\n" +
			strings.Repeat(keyLine+"\n", (128<<10)/len(keyLine))},
	} {
		archive := tarOf(t, files)
		if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), io.Discard, s,
			small); !errors.Is(err, errScrubBound) {
			t.Errorf("%s: %v, want errScrubBound", name, err)
		}
	}
	// BLANKS ARE HELD AS A TOKEN IS: the decoder skips them only once it
	// sees the token after them.
	blanks := `{"text":` + strings.Repeat(" \n", 64<<10) + `"a"}`
	if err := rewriteDocument(io.Discard, strings.NewReader(blanks), s, small); !errors.Is(err, errScrubBound) {
		t.Errorf("a run of blanks past the piece: %v, want errScrubBound", err)
	}
	deep := func(n int) string { return strings.Repeat("[", n) + `"x"` + strings.Repeat("]", n) }
	if err := rewriteDocument(io.Discard, strings.NewReader(deep(scrubDepthMax+1)), s,
		testBounds); !errors.Is(err, errTooDeep) {
		t.Errorf("a value nested past the depth: %v, want errTooDeep", err)
	}
	if err := rewriteDocument(io.Discard, strings.NewReader(deep(scrubDepthMax)), s, testBounds); err != nil {
		t.Errorf("a value nested at the depth: %v", err)
	}
}

// A TAR LEAVES THE BOX WITH NOTHING OF THE BOX'S BUT ITS SESSION'S FILES: a
// member's name scrubbed as the transcript's paths to it are; a header carrying
// only its kind, name, mode, size and time — no owner, no extended record; a
// link the box's own code made left out; and a `.json` file that is not one
// JSON value kept as the text it is, scrubbed, rather than costing the session.
//
// Mutation: copy the box's header through, keep a link, write a member's name
// as it came, or refuse the session over a torn task file, and a check here
// goes red.
func TestATarLeavesTheBoxWithOnlyItsSessionsFiles(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0)
	archive := tarOfMembers(t, []tarMember{
		{h: tar.Header{Name: "projects/p/s.jsonl", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: at,
			Uname: "box-user", Gname: "box-group", Uid: 4242, Gid: 4242, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"SCHILY.xattr.user.note": "opaque-engine-value"}},
			body: `{"type":"user"}` + "\n"},
		{h: tar.Header{Name: "projects/p/s/tool-results/opaque-engine-value.txt", Typeflag: tar.TypeReg,
			Mode: 0o600, ModTime: at}, body: "spilled\n"},
		{h: tar.Header{Name: "projects/p/s/escape", Typeflag: tar.TypeSymlink, ModTime: at,
			Linkname: "/etc/opaque-engine-value"}},
		{h: tar.Header{Name: "tasks/s/1.json", Typeflag: tar.TypeReg, Mode: 0o600, ModTime: at},
			body: `{"subject":"opaque-engine-value","status":"in_prog`},
	})
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out,
		newScrubber([]string{"opaque-engine-value"}), testBounds); err != nil {
		t.Fatalf("scrubTar: %v", err)
	}
	zr, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	kept := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the scrubbed tar does not read: %v", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			t.Errorf("kept a member of kind %q: %s", h.Typeflag, h.Name)
		}
		if h.Uname != "" || h.Gname != "" || h.Uid != 0 || h.Gid != 0 || len(h.PAXRecords) != 0 ||
			h.Linkname != "" {
			t.Errorf("%s keeps the box's header: %+v", h.Name, h)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		kept[h.Name] = string(body)
	}
	for name, body := range kept {
		if strings.Contains(name, "opaque-engine-value") || strings.Contains(body, "opaque-engine-value") {
			t.Errorf("%s still holds the engine's value: %q", name, body)
		}
	}
	if _, ok := kept["projects/p/s/escape"]; ok || len(kept) != 3 {
		t.Errorf("kept %v, want the transcript, the tool result and the task, and no link", slices.Sorted(maps.Keys(kept)))
	}
	if task, ok := kept["tasks/s/1.json"]; !ok || !strings.HasPrefix(task, `{"subject":"`) {
		t.Errorf("the torn task file was kept as %q, want its text", task)
	}
}

// AN EXPORT IS ONE DOCUMENT, KEPT AS ONE AND SCRUBBED: every string and every
// key rewritten as a transcript's are, a value under a credential's key taken
// out whole, the document still JSON — and an export holding a second document
// after the first is refused, never kept as something its harness would not
// import.
//
// Mutation: skip the document's rewrite, write its keys as they came, or let a
// second value through, and a check here goes red.
func TestAnExportIsOneDocumentKeptScrubbed(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{"opaque-engine-value"})
	doc := `{"info":{"id":"ses_1"},"messages":[{"text":"the token opaque-engine-value and ` + shapedKey +
		`"},{"opaque-engine-value":"named","input":{"password":"opaque-but-keyed"}}]}`
	var out bytes.Buffer
	if err := scrubDocument(bytes.NewReader(gzipOf(t, doc)), &out, s, testBounds); err != nil {
		t.Fatalf("scrubDocument: %v", err)
	}
	zr, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(kept) || !strings.Contains(string(kept), `"id":"ses_1"`) {
		t.Fatalf("the export was kept as %q, want the same document", kept)
	}
	for _, secret := range []string{"opaque-engine-value", shapedKey, "opaque-but-keyed"} {
		if strings.Contains(string(kept), secret) {
			t.Errorf("the export kept %q: %s", secret, kept)
		}
	}
	if err := scrubDocument(bytes.NewReader(gzipOf(t, doc+"\n"+doc)), io.Discard, s,
		testBounds); !errors.Is(err, errNotOneValue) {
		t.Errorf("an export of two documents: %v, want errNotOneValue", err)
	}
}
