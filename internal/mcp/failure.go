package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/httpx"
)

// WHY A SERVER FAILED, in the classes a person tells apart by what fixes them.
//
// A start that failed used to be one line of text, and the text is whatever
// the layer that gave up wrote: an SDK's "failed to connect", a dial error, a
// child's exit status. A person reading it has to know which of a dozen
// remedies it calls for, and a screen cannot word a remedy for text it can
// only print. So every failure the engine records about a server carries a
// CLASS beside its sentence: the engine decides it once, where the facts are
// (the HTTP status, the process's exit, the dial error), and the heartbeat,
// a server's page and the alarms all read the class rather than parse the
// sentence.
//
// THE CLASS IS DECIDED WHERE THE FACTS ARE, never read back out of a sentence:
// [connect] knows the status the server answered and whether its process
// exited, and wraps what it returns with the class ([classedFailure]); a session
// that ends is classed by the bridge from the same facts ([client.deathClass]).
// [ClassOf] reads that, and falls back on the error's own types (a DNS
// answer, a certificate, a refused dial, a missing executable) for an error
// that reached it some other way.

// FailureClass is why talking to an MCP server failed. EMPTY IS A REAL
// ANSWER: a failure that fits none of the classes, whose sentence says what
// happened. A class is never guessed to fill the gap, since a wrong remedy
// sends a person to fix the wrong thing.
type FailureClass string

// The failure classes.
const (
	// FailBlockedAddress is an address a remote party named that the engine
	// does not dial: a private, loopback or cloud metadata address, refused by
	// the public-address guard (internal/httpx, ADR-0072).
	FailBlockedAddress FailureClass = "blocked_address"
	// FailDNS is a host the engine's resolver could not resolve.
	FailDNS FailureClass = "dns"
	// FailUnreachable is a server that did not answer: a refused or timed out
	// connection, a gateway answering for a server behind it that is down
	// (5xx), or a handshake that did not finish within the startup timeout.
	FailUnreachable FailureClass = "unreachable"
	// FailTLS is a certificate the engine does not accept: expired,
	// self-signed, or naming another host.
	FailTLS FailureClass = "tls"
	// FailNeedsSignIn is a server that asks for a sign-in the engine did not
	// send: a 401 carrying a Bearer challenge to a request that carried no
	// Authorization header.
	FailNeedsSignIn FailureClass = "needs_sign_in"
	// FailCredentialRejected is a credential the server refused: any other
	// 401.
	FailCredentialRejected FailureClass = "credential_rejected"
	// FailMissingPermission is a credential the server accepts and that may
	// not do what was asked: a 403.
	FailMissingPermission FailureClass = "missing_permission"
	// FailNoEndpoint is an address with no MCP endpoint behind it: a 404 or
	// a 405, the answer a retired SSE address gives.
	FailNoEndpoint FailureClass = "no_endpoint"
	// FailNotMCP is an address that answered as something other than an MCP
	// server: a web page (a login page, a proxy, a captive portal), or a 400
	// that is not about the protocol's version.
	FailNotMCP FailureClass = "not_mcp"
	// FailOldProtocol is a server that speaks none of the protocol versions
	// the engine does.
	FailOldProtocol FailureClass = "old_protocol"
	// FailRateLimited is a server that asked the engine to slow down: a 429.
	FailRateLimited FailureClass = "rate_limited"
	// FailEmptyTools is a server that answered and listed no tool. Never
	// what [ClassOf] answers: a running server that lists nothing serves an
	// empty catalogue, which is no error; the class exists for a reading
	// that judges an answer rather than an error.
	FailEmptyTools FailureClass = "empty_tools"
	// FailCommandNotFound is a local server whose command is not on the
	// host's PATH, or is a path to nothing.
	FailCommandNotFound FailureClass = "command_not_found"
	// FailExited is a local server whose process exited: before it answered
	// the handshake, or after it had been serving.
	FailExited FailureClass = "exited"
)

// FailureClasses is every class, for the gate holding the dashboard's copy.
var FailureClasses = []FailureClass{
	FailBlockedAddress, FailDNS, FailUnreachable, FailTLS, FailNeedsSignIn,
	FailCredentialRejected, FailMissingPermission, FailNoEndpoint, FailNotMCP,
	FailOldProtocol, FailRateLimited, FailEmptyTools, FailCommandNotFound, FailExited,
}

// Valid reports whether c is a class this build names. Empty is not one of
// them, though it is a value a failure may carry ("fits none").
func (c FailureClass) Valid() bool { return slices.Contains(FailureClasses, c) }

// Failure is one failure of a server, as its bridge keeps it: the class, the
// sentence, and when. The sentence is the engine's, carrying what the server
// or its process said, which is somebody else's text: a reader bounds it and
// shows it as text, never as anything to act on.
type Failure struct {
	Class FailureClass
	Said  string
	At    time.Time
}

// classedFailure is an error whose class was decided where the facts were.
// Its text is the error's own, so wrapping it changes no sentence anybody
// reads.
type classedFailure struct {
	class FailureClass
	err   error
}

func (c *classedFailure) Error() string { return c.err.Error() }
func (c *classedFailure) Unwrap() error { return c.err }

// classify wraps err with class, unless class is empty, which leaves err to
// [ClassOf]'s fallback.
func classify(class FailureClass, err error) error {
	if err == nil || class == "" {
		return err
	}
	return &classedFailure{class: class, err: err}
}

// ClassOf is why err says talking to a server failed: the class [connect] or
// the bridge decided where the facts were, else what the error's own types
// say, else empty.
func ClassOf(err error) FailureClass {
	if err == nil {
		return ""
	}
	if c, ok := errors.AsType[*classedFailure](err); ok {
		return c.class
	}
	return classOfCause(err)
}

// classOfCause reads a class off the types an error carries, for an error no
// layer classified: the ones the standard library and the guard return.
func classOfCause(err error) FailureClass {
	var (
		dns       *net.DNSError
		unknownCA x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		verify    *tls.CertificateVerificationError
		header    tls.RecordHeaderError
		alert     tls.AlertError
	)
	switch {
	case errors.Is(err, httpx.ErrNotPublic):
		return FailBlockedAddress
	case errors.Is(err, exec.ErrNotFound), isMissingCommand(err):
		return FailCommandNotFound
	case errors.As(err, &unknownCA), errors.As(err, &hostname), errors.As(err, &invalid),
		errors.As(err, &verify), errors.As(err, &header), errors.As(err, &alert):
		return FailTLS
	case errors.As(err, &dns):
		return FailDNS
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded):
		return FailUnreachable
	case unsupportedProtocol(err):
		return FailOldProtocol
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return FailUnreachable
	}
	return ""
}

// isMissingCommand reports whether err is a start of a command that names a
// path to nothing: the PathError exec returns for an absolute or relative
// command that does not exist, which is not exec.ErrNotFound (that one is a
// bare name not found on PATH).
func isMissingCommand(err error) bool {
	var path *fs.PathError
	return errors.As(err, &path) && errors.Is(path.Err, fs.ErrNotExist)
}

// unsupportedProtocol reports whether err is the SDK refusing every version the
// server offered. The SDK keeps that error's type to itself, so its JSON-RPC
// code is read where it is a wire error, and its one fixed sentence otherwise.
func unsupportedProtocol(err error) bool {
	if wire, ok := errors.AsType[*jsonrpc.Error](err); ok && wire.Code == sdk.CodeUnsupportedProtocolVersion {
		return true
	}
	return strings.Contains(err.Error(), "unsupported protocol version")
}

// classOfStatus is the class of an HTTP answer a server gave, nil for one
// that is no failure. challenged is whether a 401 carried a Bearer challenge,
// authorized whether the request carried an Authorization header, and
// protocol whether a 400's body spoke of the protocol's version.
func classOfStatus(status int, challenged, authorized, protocol bool) FailureClass {
	switch {
	case status == 401 && challenged && !authorized:
		return FailNeedsSignIn
	case status == 401:
		return FailCredentialRejected
	case status == 403:
		return FailMissingPermission
	case status == 404 || status == 405:
		return FailNoEndpoint
	case status == 429:
		return FailRateLimited
	case status == 400 && protocol:
		return FailOldProtocol
	case status >= 500:
		return FailUnreachable
	case status >= 400:
		return FailNotMCP
	}
	return ""
}
