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
		// A PEM file written by hand with MIME's base64 — Java's MIME
		// encoder, Python's base64.encodebytes — wraps at seventy-six.
		"RSA, PKCS#1, at MIME's width": wrappedAt(76, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey)),
		// A secret-key packet's header (tag 5, a two-byte length) and its
		// body, which under a passphrase is what random bytes are.
		"OpenPGP": openPGP(append([]byte{0x95, 0x03, 0xc6}, random(0x3c6)...)),
	}
})

// wrappedAt is der as a PEM block of label whose body is wrapped at width.
func wrappedAt(width int, label string, der []byte) []string {
	body := base64.StdEncoding.EncodeToString(der)
	out := []string{"-----BEGIN " + label + "-----"}
	for len(body) > width {
		out, body = append(out, body[:width]), body[width:]
	}
	return append(out, body, "-----END "+label+"-----")
}

// openPGP is packets as GnuPG armours a secret key (RFC 4880 §6.2): no armour
// headers, the blank line that ends them anyway, the body at sixty-four, and
// the CRC-24 of the packets after it.
func openPGP(packets []byte) []string {
	crc := uint32(0xB704CE)
	for _, b := range packets {
		crc ^= uint32(b) << 16
		for range 8 {
			if crc <<= 1; crc&0x1000000 != 0 {
				crc ^= 0x1864CFB
			}
		}
	}
	sum := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 16), byte(crc >> 8), byte(crc)})
	block := wrappedAt(64, "PGP PRIVATE KEY BLOCK", packets)
	block = slices.Insert(block, 1, "")
	return slices.Insert(block, len(block)-1, "="+sum)
}

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
// two places, broken by two lines just before its last line or just before its
// END. Two lines in a row end a block, so the part before the break is read
// forward from its BEGIN, and an END reads back across them to a line of the
// key at its encoder's width — from inside, the BEGIN not in the text, the
// whole key.
func brokenVariants() map[string]func(lines []string) []string {
	return map[string]func(lines []string) []string{
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
		"broken by two lines just before its last line": func(l []string) []string {
			return inserted(l, len(l)-2, foreignLine, "[worker 4] connected")
		},
		"broken by two lines just before its END": func(l []string) []string {
			return inserted(l, len(l)-1, foreignLine, "[worker 4] connected")
		},
	}
}

// NO PIECE OF A REAL KEY SURVIVES, in any wrapping, whole or double-spaced,
// broken by the lines of another process, or read from inside it: not one
// window of 24 base64 characters — eighteen bytes of the key — of any line of
// its body. The fixtures above are shapes the reader is written against; these
// are the encoders' own output, with their own widths, headers and last lines,
// and the windows are what a person could copy off a screen.
//
// WHAT NO READ REACHES IS NAMED HERE, NOT SKIPPED. A key read from inside —
// its BEGIN not in the text — is checked whole, double-spaced, broken by one
// line and broken by two, wherever they fall, and never broken by three: the
// lines above those have no armour at all, and a read crosses two lines and no
// more. Read back, a blank line under a wrapping that marks every line — a
// reader's line number, a grep's context mark — is a line interrupting the key,
// so such a key double-spaced and broken by one line as well is broken by
// three, and is not checked from inside.
//
// Mutation: end a block at a blank line, at the first line that is not the
// key's, or read nothing back from an END, and pieces of these keys survive;
// read back across no two lines, and every key read from inside and broken by
// two loses the lines above them; read OpenPGP's checksum as a short line
// alone, and its key loses them broken by two on either side of it, and
// double-spaced under a wrapping that marks every line; leave MIME's width
// out, and the key wrapped at it loses them wherever two break it.
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
		fromInside := func(name, whole string) {
			if i := strings.IndexByte(whole, '\n'); i >= 0 {
				texts["read from inside it, "+name] = whole[i+1:]
			}
		}
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
				lines := broken(key)
				texts[vname+", "+wname] = w.wrap(lines).text
				if foreignInARow(lines) < 3 {
					fromInside(vname+", "+wname, texts[vname+", "+wname])
				}
			}
			for vname, v := range variants() {
				fromInside(vname+", "+wname, w.wrap(v.lines(key)).text)
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

// A PASSWORD AFTER A REAL KEY WITH NO END IS REDACTED AS IT IS WITH NONE: in
// every wrapping, under every encoder's last line, a label and its value on the
// next line (or after a blank one) redact the value wherever the same lines
// from the label on do, since the key's block never takes the word the
// password's rule keys on, and nothing of the key survives. Where the label's
// lines alone keep their value (an echo's quote stands where a value would
// be), so do these: that is the password's rule, not the key's read.
//
// Mutation: drop the label from bodyLine, and the value is published under
// most of these keys in most wrappings.
func TestAPasswordAfterARealKeyWithNoEndIsRedactedAsWithNone(t *testing.T) {
	t.Parallel()
	const value = "hunter2x9"
	for kname, key := range realKeys() {
		var body []string
		for _, line := range key[1 : len(key)-1] {
			if line != "" && !strings.Contains(line, ":") {
				body = append(body, line)
			}
		}
		for wname, w := range wrappings() {
			if w.oneLine {
				continue
			}
			for _, label := range []string{"Password:", "  pwd :"} {
				for _, gap := range []bool{false, true} {
					lines := append(slices.Clone(key[:len(key)-1]), label)
					if gap {
						lines = append(lines, "")
					}
					text := w.wrap(append(lines, value)).text
					word := strings.TrimSpace(strings.TrimRight(label, ": "))
					alone := text[strings.LastIndexByte(text[:strings.LastIndex(text, word)], '\n')+1:]
					name := kname + ", " + wname + ", " + label
					got := redact.Secrets(text)
					if leak := survivor(got, body); leak != "" {
						t.Errorf("%s: %q survived in\n%s", name, leak, got)
					}
					if strings.Contains(got, value) && !strings.Contains(redact.Secrets(alone), value) {
						t.Errorf("%s (blank line between: %v): the value was published under the key "+
							"and redacted without it:\n%s", name, gap, got)
					}
					if again := redact.Secrets(got); again != got {
						t.Errorf("%s: a second pass changed the text", name)
					}
					if shown, _ := settle(text, []int{5, 400}); shown != got {
						t.Errorf("%s in pieces: shown as it settled\n%q\nwant\n%q", name, shown, got)
					}
				}
			}
		}
	}
}

// foreignInARow is the most lines in a row of lines that are none of the key's:
// a body of one line broken by two lines and then by one is broken by three.
func foreignInARow(lines []string) int {
	most, run := 0, 0
	for _, line := range lines {
		run++
		if !strings.Contains(line, "] connected") {
			run = 0
		}
		most = max(most, run)
	}
	return most
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
