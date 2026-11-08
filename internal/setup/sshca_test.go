package setup_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/setup"
)

// A MINTED SSH AUTHORITY IS ONE THE ENGINE SIGNS WITH: an unencrypted OpenSSH
// Ed25519 key whose public line a server reads, named by its own fingerprint,
// and two mints are two authorities.
func TestAMintedSSHAuthoritySignsAndShowsOnlyItsPublicLine(t *testing.T) {
	t.Parallel()
	shape := setup.ShapeSSHCertificateAuthority
	if !shape.Multiline() {
		t.Error("an SSH authority spans lines, and its shape does not say so")
	}
	first, err := shape.Mint(nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := shape.Mint(nil)
	if err != nil || first == second {
		t.Fatalf("two mints: equal %v, %v", first == second, err)
	}
	if problem := shape.Problem("certificate authority", first); problem != "" {
		t.Errorf("a minted authority is refused: %s", problem)
	}
	ca, err := setup.ParseSSHCertificateAuthority(first)
	if err != nil {
		t.Fatal(err)
	}
	line := ca.PublicLine()
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("public line %q: %v", line, err)
	}
	if comment != setup.SSHCertificateAuthorityComment(pub) || !strings.HasPrefix(comment, "crewlet-ssh-ca-") ||
		len(comment) != len("crewlet-ssh-ca-")+16 {
		t.Errorf("the line is named %q", comment)
	}
	if strings.Contains(line, "PRIVATE") || strings.Contains(first, line) {
		t.Errorf("the public line carries the key: %q", line)
	}
	if _, err := setup.PublicOf(first, line, "text"); err != nil {
		t.Errorf("the public line is refused as a public view: %v", err)
	}
	if ca.Fingerprint() != ssh.FingerprintSHA256(pub) {
		t.Errorf("fingerprint %s, want %s", ca.Fingerprint(), ssh.FingerprintSHA256(pub))
	}
}

// ONLY AN UNENCRYPTED OPENSSH ED25519 KEY IS AN SSH AUTHORITY: an encrypted
// one, another key type, a PKCS#8 key, a public line and trailing text are
// refused, each with the sentence to leave the field to the engine.
func TestAnSSHAuthorityThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(edKey, "c", []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaBlock, err := ssh.MarshalPrivateKey(ecKey, "c")
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := setup.GenerateSSHCertificateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := setup.ParseSSHCertificateAuthority(minted)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"encrypted":   string(pem.EncodeToMemory(encrypted)),
		"ECDSA":       string(pem.EncodeToMemory(ecdsaBlock)),
		"PKCS#8":      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"public line": ca.PublicLine(),
		"two keys":    minted + minted,
		"empty":       "",
	} {
		if _, err := setup.ParseSSHCertificateAuthority(value); err == nil {
			t.Errorf("%s: accepted", name)
		}
		problem := setup.ShapeSSHCertificateAuthority.Problem("certificate authority", value)
		if !strings.Contains(problem, "Leave the field to the engine") {
			t.Errorf("%s: problem %q", name, problem)
		}
	}
}
