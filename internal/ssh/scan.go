package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
)

// The host scan: what a server presents as its host keys, read for a person
// who then compares each fingerprint with the server's own console before
// pinning it. One connection per algorithm, each closed the moment the key
// arrives: no authentication is ever attempted.

// ScannedKey is one host key a server presented.
type ScannedKey struct {
	Line              string `json:"line"`
	Type              string `json:"type"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	// Problem is why the key cannot be pinned, "" for one that can.
	Problem string `json:"problem,omitempty"`
}

// scanAlgorithms are the host key algorithms the scan asks for, one connection
// each; rsa-sha2-512 reads a server's RSA key.
var scanAlgorithms = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512,
}

// errScanned ends a scan's handshake once the key is read.
var errScanned = errors.New("ssh: host key read")

// ErrNoHostKey is a server that answered no algorithm the engine pins.
var ErrNoHostKey = errors.New("ssh: the server presented no host key the engine can pin")

// Scan reads the host keys the server at address and port presents, through
// the same address rule a registered host is held to.
func Scan(ctx context.Context, address string, port int) ([]ScannedKey, error) {
	return scan(ctx, guardedDial, address, port)
}

func scan(ctx context.Context, dial dialer, address string, port int) ([]ScannedKey, error) {
	if sentence := config.SSHAddress(address); sentence != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnreachableAddress, sentence)
	}
	if port == 0 {
		port = config.SSHDefaultPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("ssh: %d is not a port", port)
	}
	addr := net.JoinHostPort(address, strconv.Itoa(port))
	var out []ScannedKey
	seen := map[string]bool{}
	var lastErr error
	for _, algo := range scanAlgorithms {
		key, err := scanOne(ctx, dial, addr, algo)
		switch {
		case errors.Is(err, ErrUnreachableAddress):
			return nil, err
		case err != nil:
			lastErr = err
			continue
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if seen[line] {
			continue
		}
		seen[line] = true
		k := ScannedKey{Line: line, Type: key.Type(), FingerprintSHA256: ssh.FingerprintSHA256(key)}
		if _, err := config.ParseSSHHostKey(line); err != nil {
			k.Problem = err.Error()
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = ErrNoHostKey
		}
		return nil, fmt.Errorf("%w: %w", ErrNoHostKey, lastErr)
	}
	return out, nil
}

// scanOne is the key presented under one algorithm, read before any
// authentication and the connection closed.
func scanOne(ctx context.Context, dial dialer, addr, algo string) (ssh.PublicKey, error) {
	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	conn, err := dial(dialCtx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := dialCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "crewlet-scan", HostKeyAlgorithms: []string{algo}, Timeout: DialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = key
			return errScanned
		},
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if got != nil {
		return got, nil
	}
	if err == nil {
		err = ErrNoHostKey
	}
	return nil, err
}
