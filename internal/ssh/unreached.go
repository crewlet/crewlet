package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// Why a server could not be reached, in a person's words with the remedy for
// each cause, shared by Scan and Test connection's reach step. The dial's own
// error stays in the chain for the operator's log.

// ScanError is a host key read that found no key, with the reason in a
// person's words beside the chain ([ErrNoHostKey] still matches through it).
type ScanError struct {
	// Said is which way it failed and what to check, with no Go error in it.
	Said string
	err  error
}

func (e *ScanError) Error() string { return e.err.Error() }

// Unwrap exposes the chain, so a sentinel inside it still matches.
func (e *ScanError) Unwrap() error { return e.err }

// scanFailed is err with the person's words for it.
func scanFailed(said string, err error) *ScanError { return &ScanError{Said: said, err: err} }

// unreachedWords says why a dial to addr ("host:port") failed, told apart by
// the error's kind, never its text.
func unreachedWords(addr string, err error) string {
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host, port = addr, strconv.Itoa(22)
	}
	if errors.Is(err, ErrUnreachableAddress) {
		return host + " resolves to the engine host or a link-local address, which no server should be reached at."
	}
	if dns, ok := errors.AsType[*net.DNSError](err); ok {
		if dns.IsNotFound {
			return host + " does not resolve to an address. Check the host name."
		}
		return host + " could not be looked up just now. Try again in a moment."
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("%s refused the connection on port %s: nothing listens there, "+
			"or a firewall turns the engine away. Check the port.", host, port)
	}
	if ne, ok := errors.AsType[net.Error](err); (ok && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("Nothing answered at %s on port %s within %d seconds. Check the address and port, "+
			"and let the engine's address through the server's firewall.", host, port, int(DialTimeout.Seconds()))
	}
	return fmt.Sprintf("%s could not be reached on port %s. Check the address and port, "+
		"and let the engine's address through the server's firewall.", host, port)
}
