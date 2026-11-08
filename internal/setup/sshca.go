package setup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// An SSH certificate authority: an Ed25519 key the engine generates, seals and
// never shows, whose public line servers trust (TrustedUserCAKeys). The engine
// signs a short-lived user certificate with it for each use, so the key is the
// one secret.

// sshCAPEMType is the armour of an OpenSSH private key.
const sshCAPEMType = "OPENSSH PRIVATE KEY"

// SSHCertificateAuthority is a held SSH certificate authority: its signer and
// its public line, never its private key.
type SSHCertificateAuthority struct {
	Signer ssh.Signer
}

// GenerateSSHCertificateAuthority mints a certificate authority and returns
// the value to seal: its private key in OpenSSH format, unencrypted.
func GenerateSSHCertificateAuthority() (string, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(key, SSHCertificateAuthorityComment(sshPub))
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(block)), nil
}

// ParseSSHCertificateAuthority reads a held authority back: exactly one
// unencrypted OpenSSH private key block holding an Ed25519 key.
func ParseSSHCertificateAuthority(value string) (*SSHCertificateAuthority, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(value)))
	switch {
	case block == nil || block.Type != sshCAPEMType:
		return nil, errors.New("setup: an SSH certificate authority is one OpenSSH private key block")
	case len(strings.TrimSpace(string(rest))) > 0:
		return nil, errors.New("setup: an SSH certificate authority holds one key and nothing after it")
	}
	raw, err := ssh.ParseRawPrivateKey(pem.EncodeToMemory(block))
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, errors.New("setup: the SSH certificate authority's key is encrypted, and the engine holds it unencrypted")
	case err != nil:
		return nil, errors.New("setup: the SSH certificate authority's key does not parse")
	}
	key, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("setup: the SSH certificate authority's key is not an Ed25519 key")
	}
	signer, err := ssh.NewSignerFromKey(*key)
	if err != nil {
		return nil, errors.New("setup: the SSH certificate authority's key cannot sign")
	}
	return &SSHCertificateAuthority{Signer: signer}, nil
}

// SSHCertificateAuthorityComment names an authority by its public key, so the
// line a server holds says which authority it is without the engine storing a
// name: crewlet-ssh-ca- and the first 16 hex digits of the key's SHA-256.
func SSHCertificateAuthorityComment(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return "crewlet-ssh-ca-" + hex.EncodeToString(sum[:8])
}

// PublicLine is the line a server trusts, as TrustedUserCAKeys reads it.
func (ca *SSHCertificateAuthority) PublicLine() string {
	pub := ca.Signer.PublicKey()
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + SSHCertificateAuthorityComment(pub)
}

// Fingerprint is the authority's SHA-256 fingerprint as ssh-keygen -l prints it.
func (ca *SSHCertificateAuthority) Fingerprint() string {
	return ssh.FingerprintSHA256(ca.Signer.PublicKey())
}
