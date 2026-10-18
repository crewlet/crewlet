package redact_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// realKeys are private keys generated for the run, in every encoding a key
// file is written in, as their PEM lines armour to armour: what the fixtures
// above stand for, made by the encoders that make the real ones.
var realKeys = sync.OnceValue(func() map[string][]string {
	must := func(b []byte, err error) []byte {
		if err != nil {
			panic(err)
		}
		return b
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return b
	}
	lines := func(block *pem.Block) []string {
		return strings.Split(strings.TrimSuffix(string(pem.EncodeToMemory(block)), "\n"), "\n")
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	edPub, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return map[string][]string{
		"RSA, PKCS#1":       lines(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}),
		"RSA, PKCS#8":       lines(&pem.Block{Type: "PRIVATE KEY", Bytes: must(x509.MarshalPKCS8PrivateKey(rsaKey))}),
		"EC, SEC 1":         lines(&pem.Block{Type: "EC PRIVATE KEY", Bytes: must(x509.MarshalECPrivateKey(ecKey))}),
		"EC, PKCS#8":        lines(&pem.Block{Type: "PRIVATE KEY", Bytes: must(x509.MarshalPKCS8PrivateKey(ecKey))}),
		"Ed25519":           lines(&pem.Block{Type: "PRIVATE KEY", Bytes: must(x509.MarshalPKCS8PrivateKey(edKey))}),
		"Ed25519, SSH":      openSSH(edPub, edKey),
		"PKCS#8, encrypted": lines(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: random(1298)}),
		// RFC 1421's traditional encrypted form, as OpenSSL writes it: the
		// body is ciphertext, which is what random bytes are.
		"RSA, encrypted": lines(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{
			"Proc-Type": "4,ENCRYPTED",
			"DEK-Info":  "AES-128-CBC," + strings.ToUpper(hex.EncodeToString(random(16))),
		}, Bytes: random(1232)}),
	}
})

// openSSH is an Ed25519 key in OpenSSH's own format, written as ssh-keygen
// writes it (PROTOCOL.key): unencrypted, its body base64 at seventy.
func openSSH(pub ed25519.PublicKey, key ed25519.PrivateKey) []string {
	str := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	pubBlob := append(str([]byte("ssh-ed25519")), str(pub)...)
	check := make([]byte, 4)
	_, _ = rand.Read(check)
	private := append(append(slices.Clone(check), check...), str([]byte("ssh-ed25519"))...)
	private = append(append(append(private, str(pub)...), str(key)...), str([]byte("run@box"))...)
	for i := byte(1); len(private)%8 != 0; i++ {
		private = append(private, i)
	}
	blob := []byte("openssh-key-v1\x00")
	blob = append(append(append(blob, str([]byte("none"))...), str([]byte("none"))...), str(nil)...)
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = append(append(blob, str(pubBlob)...), str(private)...)
	body := base64.StdEncoding.EncodeToString(blob)
	out := []string{"-----BEGIN OPENSSH PRIVATE KEY-----"}
	for len(body) > 70 {
		out, body = append(out, body[:70]), body[70:]
	}
	return append(out, body, "-----END OPENSSH PRIVATE KEY-----")
}

// brokenVariants are the ways a key reaches a transcript that a block does not
// read whole: broken by two lines in a row, broken before its body, broken in
// two places, broken by two lines just before its last line. Two lines in a
// row end a block, so each part of the key is read from its own armour, the
// part before the break forward from its BEGIN and the part after it back from
// its END.
func brokenVariants() map[string]brokenVariant {
	return map[string]brokenVariant{
		"broken by two lines in a row": {lines: func(l []string) []string {
			at := bodyAt(l)
			return inserted(l, at+max(1, (len(l)-1-at)/2), foreignLine, "[worker 4] connected")
		}},
		"broken before its body": {lines: func(l []string) []string {
			return inserted(l, 1, foreignLine)
		}},
		"broken by two lines, then by one": {lines: func(l []string) []string {
			at := bodyAt(l)
			n := len(l) - 1 - at
			l = inserted(l, at+max(1, n/3), foreignLine, "[worker 4] connected")
			return inserted(l, len(l)-1-max(1, n/3), "[worker 5] connected")
		}},
		// The part after the break is the body's last line alone, and an
		// END reading back finds no full line of the key above it: a line
		// shorter than a key's full one, alone before an END, reads as a
		// word does.
		"broken by two lines just before its last line": {
			lines: func(l []string) []string {
				return inserted(l, len(l)-2, foreignLine, "[worker 4] connected")
			},
			unreached: func(body []string) []string {
				if last := body[len(body)-1]; len(last) < 40 {
					return []string{last}
				}
				return nil
			},
		},
	}
}

// brokenVariant is a way a key reaches a transcript broken by lines that are
// not its own.
type brokenVariant struct {
	// lines reads to the variant's lines from the key's.
	lines func(lines []string) []string
	// unreached is the lines of the body, given, that no read of the
	// variant reaches, or nil for none.
	unreached func(body []string) []string
}

// NO PIECE OF A REAL KEY SURVIVES, in any wrapping, whole or double-spaced,
// broken by the lines of another process, or read from inside it: not one
// window of 24 base64 characters — eighteen bytes of the key — of any line of
// its body. The fixtures above are shapes the reader is written against; these
// are the encoders' own output, with their own widths, headers and last lines,
// and the windows are what a person could copy off a screen.
//
// WHAT NO READ REACHES IS NAMED HERE, NOT SKIPPED. Two lines in a row end a
// block, so each part of a key they break is read from its own armour, and a
// part with no armour, or with only a short line before its END, has nothing
// to read: broken by two lines just before a last line shorter than a full
// one, that last line is published. And a key read from inside — its BEGIN
// not in the text — is checked whole, double-spaced and broken by one line,
// never broken by two: the lines above the break have no armour at all. Read
// back, a blank line under a wrapping that marks every line — a reader's line
// number, a grep's context mark — is a line interrupting the key, so such a
// key double-spaced and broken by one line as well is broken by two.
//
// Mutation: end a block at a blank line, at the first line that is not the
// key's, or read nothing back from an END, and pieces of these keys survive.
func TestNoPieceOfARealKeySurvivesAnyWrapping(t *testing.T) {
	t.Parallel()
	for kname, key := range realKeys() {
		var body []string
		for _, line := range key[1 : len(key)-1] {
			if line != "" && !strings.Contains(line, ":") {
				body = append(body, line)
			}
		}
		texts := map[string]string{}
		// unreached is the lines of the body a text leaves to no read.
		unreached := map[string][]string{}
		for wname, w := range wrappings() {
			for vname, v := range variants() {
				if !v.lineByLine || !w.oneLine {
					texts[vname+", "+wname] = w.wrap(v.lines(key)).text
				}
			}
			if w.oneLine {
				continue
			}
			for vname, broken := range brokenVariants() {
				texts[vname+", "+wname] = w.wrap(broken.lines(key)).text
				if broken.unreached != nil {
					unreached[vname+", "+wname] = broken.unreached(body)
				}
			}
			for vname, v := range variants() {
				whole := w.wrap(v.lines(key)).text
				if i := strings.IndexByte(whole, '\n'); i >= 0 {
					texts["read from inside it, "+vname+", "+wname] = whole[i+1:]
				}
			}
		}
		// TWO WHOLE KEYS FLATTENED ONTO ONE LINE, each way they get there: a
		// shell `echo $A $B` spacing their breaks, an env dump with the
		// breaks stripped. The first key's body must not leak because the
		// second's BEGIN appears on the line before the first's END is
		// found — the regression this test's package guards.
		spaced := strings.Join(key, " ")
		flat := strings.Join(key, "")
		texts["two flattened onto one line, spaced"] = "KEYS=" + spaced + " " + spaced + "\n"
		texts["two flattened onto one line, breaks removed"] = "KEYS=" + flat + flat + "\n"
		texts["two echoed on one line"] = "+ echo " + spaced + " " + spaced + "\n"
		texts["one-line key, then a bare BEGIN mention"] = "KEY=" + spaced +
			" # -----BEGIN PRIVATE KEY----- format\n"
		// A ONE-LINE KEY WHOSE END WAS LOST, followed on its line by another
		// block's armour: a certificate's, or the next key whole. The armour
		// ends the block at its body, as one on a later line does, and the
		// same holds for its own END once a word has come before it.
		noEnd := strings.Join(key[:len(key)-1], " ")
		texts["one-line key with no END, then a certificate"] = "A=" + noEnd + " -----BEGIN CERTIFICATE----- MIIB\n"
		texts["one-line key with no END, then a whole key"] = "A=" + noEnd + " " + spaced + "\n"
		texts["one-line key whose END follows two words"] = "A=" + noEnd + " two words " + key[len(key)-1] + "\n"
		for name, text := range texts {
			got := redact.Secrets(text)
			reached := slices.DeleteFunc(slices.Clone(body), func(line string) bool {
				return slices.Contains(unreached[name], line)
			})
			if leak := survivor(got, reached); leak != "" {
				t.Errorf("%s, %s: %q survived in\n%s", kname, name, leak, got)
				continue
			}
			if again := redact.Secrets(got); again != got {
				t.Errorf("%s, %s: a second pass changed the text", kname, name)
			}
			for _, sizes := range [][]int{{97}, {5, 400}} {
				if shown, _ := settle(text, sizes); shown != got {
					t.Errorf("%s, %s in pieces of %v: shown as it settled\n%q\nwant\n%q", kname, name, sizes, shown, got)
				}
			}
		}
	}
}

// survivor is a 24-character window of one of a key's body lines that text
// still holds, or "".
func survivor(text string, body []string) string {
	for _, line := range body {
		for i := 0; i+24 <= len(line); i++ {
			if w := line[i : i+24]; strings.Contains(text, w) {
				return w
			}
		}
	}
	return ""
}
