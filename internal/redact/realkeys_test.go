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
// read whole but that must not publish any of it: broken by two lines in a row,
// broken before its body, broken in two places, and a stream's end read from
// inside the key — its BEGIN line dropped with the partial line the read began
// in.
func brokenVariants() map[string]func([]string) []string {
	return map[string]func([]string) []string{
		"broken by two lines in a row": func(l []string) []string {
			at := bodyAt(l)
			return inserted(l, at+max(1, (len(l)-1-at)/2), foreignLine, "[worker 4] connected")
		},
		"broken before its body": func(l []string) []string {
			return inserted(l, 1, foreignLine)
		},
		"broken by two lines, then by one": func(l []string) []string {
			at := bodyAt(l)
			n := len(l) - 1 - at
			l = inserted(l, at+max(1, n/3), foreignLine, "[worker 4] connected")
			return inserted(l, len(l)-1-max(1, n/3), "[worker 5] connected")
		},
	}
}

// NO PIECE OF A REAL KEY SURVIVES, in any wrapping, whole or double-spaced,
// broken by the lines of another process anywhere in it, or read from inside
// it: not one window of 24 base64 characters — eighteen bytes of the key — of
// any line of its body. The fixtures above are shapes the reader is written
// against; these are the encoders' own output, with their own widths, headers
// and last lines, and the windows are what a person could copy off a screen.
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
				texts[vname+", "+wname] = w.wrap(broken(key)).text
			}
			for vname, v := range variants() {
				whole := w.wrap(v.lines(key)).text
				if i := strings.IndexByte(whole, '\n'); i >= 0 {
					texts["read from inside it, "+vname+", "+wname] = whole[i+1:]
				}
			}
		}
		for name, text := range texts {
			got := redact.Secrets(text)
			if leak := survivor(got, body); leak != "" {
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
