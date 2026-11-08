package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// The engine's client: it reaches only a host the company registered, accepts
// only a host key byte-equal to one pinned for it, authenticates only by the
// certificate it was handed, and runs one command per session with no PTY,
// no agent, no environment and no forwarding. Output is capped per stream and
// hashed whole; nothing of it is stored.

// DialTimeout bounds reaching a host and finishing the SSH handshake: a
// server on the other side of the world answers in well under a second, and a
// filtered port is a server that is not there.
const DialTimeout = 10 * time.Second

// ErrUnreachableAddress is an address the engine never connects a server
// session to (httpx.ServerAddress), however a name resolved.
var ErrUnreachableAddress = errors.New("ssh: the address is the engine host or link-local")

// ErrHostKey is a server that presented no pinned key.
var ErrHostKey = errors.New("ssh: the server presented a host key that is not pinned for it")

// dialer opens a TCP connection; tests hand one reaching their own server.
type dialer func(ctx context.Context, network, address string) (net.Conn, error)

// guardedDial connects only where httpx.ServerAddress allows, judged on the
// socket's own address, so a name that resolves twice cannot slip a second,
// unchecked answer through.
func guardedDial(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: DialTimeout, ControlContext: func(_ context.Context, _, addr string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil || !httpx.ServerAddress(ap.Addr()) {
			return fmt.Errorf("%w: %s", ErrUnreachableAddress, addr)
		}
		return nil
	}}
	return d.DialContext(ctx, network, address)
}

// Client runs commands on the company's hosts.
type Client struct {
	timeout   time.Duration
	maxOutput int
	dial      dialer
	// step bounds each Test connection step; zero is [StepTimeout].
	step time.Duration
}

// StepTimeout is how long one Test connection step may take before the engine
// closes its connection.
const StepTimeout = 30 * time.Second

func (c *Client) stepLimit() time.Duration {
	if c.step > 0 {
		return c.step
	}
	return StepTimeout
}

// NewClient is a client bound by the company's command limits.
func NewClient(limits *config.SSHCommands) *Client {
	return &Client{timeout: limits.Timeout(), maxOutput: limits.MaxOutput(), dial: guardedDial}
}

// Command is one command to run and, for an approved operate command alone,
// the bytes its stdin reads.
type Command struct {
	Line  string
	Stdin []byte
}

// Result is what a command did. Its output is the agent's to read and never
// stored; the audit keeps its sizes and digest.
type Result struct {
	ExitCode       int
	Stdout, Stderr []byte
	StdoutBytes    int64
	StderrBytes    int64
	Truncated      bool
	TimedOut       bool
	Duration       time.Duration
	// OutputSHA256 is the SHA-256, in hex, of the two streams' own SHA-256
	// digests, stdout's then stderr's, each over the whole stream.
	OutputSHA256 string
	// Started is a command the server began: the rest describes what it did.
	Started bool
}

// RunError is a command that did not run or did not finish, with the closed
// word the access log records it by.
type RunError struct {
	Failure accessdomain.CommandFailure
	Err     error
}

func (e *RunError) Error() string { return e.Err.Error() }
func (e *RunError) Unwrap() error { return e.Err }

// FailureOf is the access log's word for err: engine_error for an error the
// client did not classify.
func FailureOf(err error) accessdomain.CommandFailure {
	var run *RunError
	if errors.As(err, &run) {
		return run.Failure
	}
	return accessdomain.FailureEngineError
}

func failed(word accessdomain.CommandFailure, err error) error {
	return &RunError{Failure: word, Err: err}
}

// hostKeys is the host's pinned keys and the algorithms that negotiate them.
func hostKeys(h config.SSHHost) ([]ssh.PublicKey, []string, error) {
	var keys []ssh.PublicKey
	var algos []string
	for _, line := range h.HostKeys {
		pk, err := config.ParseSSHHostKey(line)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh: host %s's pinned key: %w", h.ID, err)
		}
		keys = append(keys, pk)
		switch pk.Type() {
		case ssh.KeyAlgoRSA:
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			algos = append(algos, pk.Type())
		}
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("ssh: host %s pins no host key", h.ID)
	}
	return keys, algos, nil
}

// pinned accepts only a key byte-equal to one of keys.
func pinned(keys []ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		for _, k := range keys {
			if bytes.Equal(k.Marshal(), key.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrHostKey, ssh.FingerprintSHA256(key))
	}
}

// connect is a session-capable connection to h as user, authenticated by auth.
func (c *Client) connect(ctx context.Context, h config.SSHHost, user string, auth ssh.Signer) (*ssh.Client, error) {
	keys, algos, err := hostKeys(h)
	if err != nil {
		return nil, failed(accessdomain.FailureEngineError, err)
	}
	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.PortOrDefault()))
	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	conn, err := c.dial(dialCtx, "tcp", addr)
	if err != nil {
		return nil, failed(accessdomain.FailureUnreachable, fmt.Errorf("ssh: reach %s: %w", h.ID, err))
	}
	deadline, _ := dialCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	// VERIFIED is a server that passed the pin: a handshake that fails after
	// it is the server refusing the certificate.
	verified, check := false, pinned(keys)
	cfg := &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(auth)},
		HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			if pinErr := check(host, remote, key); pinErr != nil {
				return pinErr
			}
			verified = true
			return nil
		},
		HostKeyAlgorithms: algos, Timeout: DialTimeout,
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		word := accessdomain.FailureUnreachable
		switch {
		case errors.Is(err, ErrHostKey):
			word = accessdomain.FailureHostKeyRefused
		case verified:
			word = accessdomain.FailureAuthenticationRefused
		}
		return nil, failed(word, fmt.Errorf("ssh: sign in to %s as %s: %w", h.ID, user, err))
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// Run runs cmd on h as user, authenticated by auth, and stops it at the
// company's timeout.
func (c *Client) Run(ctx context.Context, h config.SSHHost, user string, auth ssh.Signer, cmd Command) (Result, error) {
	started := time.Now()
	client, err := c.connect(ctx, h, user, auth)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return Result{}, failed(accessdomain.FailureSessionFailed, fmt.Errorf("ssh: open a session on %s: %w", h.ID, err))
	}
	defer func() { _ = session.Close() }()
	stdout, stderr := newCapped(c.maxOutput), newCapped(c.maxOutput)
	session.Stdout, session.Stderr = stdout, stderr
	if cmd.Stdin != nil {
		session.Stdin = bytes.NewReader(cmd.Stdin)
	}
	if err := session.Start(cmd.Line); err != nil {
		return Result{}, failed(accessdomain.FailureSessionFailed, fmt.Errorf("ssh: start the command on %s: %w", h.ID, err))
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}
	if timedOut {
		// KILL, then close: a server that ignores the signal loses the
		// channel, and its sshd ends the command with it.
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		_ = client.Close()
		waitErr = <-done
	}
	res := Result{
		Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(),
		StdoutBytes: stdout.total, StderrBytes: stderr.total,
		Truncated: stdout.cut || stderr.cut, TimedOut: timedOut, Duration: time.Since(started),
		OutputSHA256: outputDigest(stdout.sum, stderr.sum), Started: true,
	}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case timedOut:
		res.ExitCode = -1
	case waitErr == nil:
	case errors.As(waitErr, &exit):
		res.ExitCode = exit.ExitStatus()
	case errors.As(waitErr, &missing):
		res.ExitCode = -1
	default:
		return res, failed(accessdomain.FailureSessionFailed, fmt.Errorf("ssh: run the command on %s: %w", h.ID, waitErr))
	}
	return res, nil
}

func outputDigest(stdout, stderr hash.Hash) string {
	both := sha256.New()
	both.Write(stdout.Sum(nil))
	both.Write(stderr.Sum(nil))
	return hex.EncodeToString(both.Sum(nil))
}

// capped keeps the first max bytes of a stream and counts and hashes all of it.
type capped struct {
	mu    sync.Mutex
	max   int
	buf   bytes.Buffer
	total int64
	cut   bool
	sum   hash.Hash
}

func newCapped(max int) *capped { return &capped{max: max, sum: sha256.New()} }

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	c.sum.Write(p)
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.cut = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.cut = true
	}
	return len(p), nil
}

// text is what was kept so far.
func (c *capped) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
