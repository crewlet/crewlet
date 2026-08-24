package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/envfile"
	"github.com/crewlet/crewlet/internal/store"
)

// ---- the secret store ------------------------------------------------- //

// SecretStoreSink writes minted credentials into the encrypted secret store.
//
// THE ONE WITH NO FILE TO SOURCE. The engine reads these back directly, so
// there is nothing for an operator to copy anywhere and nothing to keep in
// sync — which also means a rotation takes effect on the next config
// activation rather than on the next deploy of a file.
type SecretStoreSink struct {
	values *store.SecretValues
	by     string

	mu      sync.Mutex
	written []string
}

// NewSecretStoreSink builds the sink.
func NewSecretStoreSink(values *store.SecretValues, by string) *SecretStoreSink {
	return &SecretStoreSink{values: values, by: by}
}

// Record implements [TokenSink], write-through.
func (s *SecretStoreSink) Record(ctx context.Context, name, value string) error {
	if err := s.values.Set(ctx, name, value, s.by, "provision", time.Now().UTC()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, name)
	return nil
}

// Discard implements [TokenSink].
//
// BEST EFFORT AND IT REPORTS WHAT IT MISSED. A sink that cannot be reached
// to remove a value is a worse problem than the one being cleaned up, so it
// keeps going — but an operator finishing by hand needs the list, and
// swallowing it would leave dead credentials in the store reading exactly
// like live ones.
func (s *SecretStoreSink) Discard(ctx context.Context) error {
	s.mu.Lock()
	names := append([]string(nil), s.written...)
	s.written = nil
	s.mu.Unlock()

	var stuck []string
	for _, name := range names {
		if _, err := s.values.Unset(ctx, name); err != nil {
			stuck = append(stuck, name)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("provision: these secrets could not be removed and "+
			"now hold revoked credentials — remove them by hand: %s",
			strings.Join(stuck, ", "))
	}
	return nil
}

// Flush implements [TokenSink]. Nothing to do: every Record was durable.
func (s *SecretStoreSink) Flush(context.Context) error { return nil }

// Describe implements [TokenSink].
func (s *SecretStoreSink) Describe() string {
	return "the encrypted secret store (the engine reads it directly; no file to source)"
}

// ---- an env file ------------------------------------------------------ //

// EnvFileSink writes minted credentials into a `.env`.
//
// # It is created 0600 at OPEN, not at write
//
// A file that appears with default permissions and is tightened later has a
// window, however short, in which every process on the host can read a live
// credential. Creating it up front means the window never exists — and it
// happens at open so a run that mints nothing still leaves a correctly-moded
// file rather than one an operator has to remember to fix.
//
// # Write-through, for the reason the interface states
//
// Each Record rewrites the file. That is more IO than batching, and it is
// the difference between a crash mid-run leaving a recorded credential and
// leaving a live one nobody knows about.
type EnvFileSink struct {
	path string

	mu      sync.Mutex
	values  map[string]string
	written []string
}

// NewEnvFileSink opens (or creates) the file and reads what it holds.
//
// It READS FIRST, because the file usually exists and usually has other
// things in it: a provisioning run rotates one seat's token, and a sink that
// truncated would take every unrelated credential with it.
func NewEnvFileSink(path string) (*EnvFileSink, error) {
	s := &EnvFileSink{path: path, values: map[string]string{}}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		for _, line := range strings.Split(string(body), "\n") {
			if name, value, ok := envfile.ParseAssignment(line); ok {
				s.values[name] = value
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// Created NOW, at 0600, so the permissions are right before
		// anything is in it.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("provision: %s: %w", path, err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("provision: %s: %w", path, err)
		}
		_ = f.Close()
	default:
		return nil, fmt.Errorf("provision: %s: %w", path, err)
	}
	return s, nil
}

// Record implements [TokenSink], rewriting the file each time.
func (s *EnvFileSink) Record(_ context.Context, name, value string) error {
	// FORMATTED FIRST, so a value this file cannot carry is refused before
	// anything is written — the caller then revokes the credential rather
	// than leaving one recorded in a form a reader will mangle.
	if _, err := envfile.FormatAssignment(name, value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// TRACKED ONCE per name: a run that rotates the same variable twice
	// must not queue it for two removals, and a name that was already in
	// the file before this run is still ours to roll back — we overwrote
	// what was there, and leaving our value behind on a rollback would be
	// worse than removing it.
	if !contains(s.written, name) {
		s.written = append(s.written, name)
	}
	s.values[name] = value
	return s.rewrite()
}

// Discard implements [TokenSink].
func (s *EnvFileSink) Discard(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.written {
		delete(s.values, name)
	}
	s.written = nil
	return s.rewrite()
}

// Flush implements [TokenSink]. Nothing to do: every Record rewrote.
func (s *EnvFileSink) Flush(context.Context) error { return nil }

// Describe implements [TokenSink].
func (s *EnvFileSink) Describe() string {
	return s.path + " (source it, or point the engine's env at it)"
}

// rewrite writes the whole file atomically. The caller holds the lock.
//
// ATOMIC, because this file is the only record of a live credential: a
// partial write from an interrupted run would lose the ones below the cut,
// and they would still exist at the vendor.
func (s *EnvFileSink) rewrite() error {
	names := make([]string, 0, len(s.values))
	for name := range s.values {
		names = append(names, name)
	}
	// SORTED, so a rotation produces a diff an operator can read rather
	// than a reshuffle of the whole file.
	sort.Strings(names)

	var body strings.Builder
	body.WriteString("# Written by `crewlet ... provision`. Values are single-quoted so\n" +
		"# `source` and the engine's dotenv reader agree on them.\n")
	for _, name := range names {
		line, err := envfile.FormatAssignment(name, s.values[name])
		if err != nil {
			return err
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".env.*")
	if err != nil {
		return fmt.Errorf("provision: %s: %w", s.path, err)
	}
	defer os.Remove(tmp.Name())
	// NO CHMOD, and that is load-bearing rather than an omission:
	// os.CreateTemp documents 0600, and the rename below carries the temp
	// file's mode onto the destination. A chmod after the rename would be
	// too late — the window it was meant to close has already been open —
	// and one before it would be a second statement of a guarantee the
	// standard library already makes.
	if _, err := io.WriteString(tmp, body.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("provision: %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("provision: %s: %w", s.path, err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("provision: %s: %w", s.path, err)
	}
	return nil
}

// ---- printing --------------------------------------------------------- //

// PrintSink writes minted credentials to a stream.
//
// THE ESCAPE HATCH, and it says what it is: an operator who wants to paste
// the values into a password manager, or into a deployment system this
// binary knows nothing about. It is the one sink that leaves a credential in
// a terminal, so it is never a default.
type PrintSink struct {
	w io.Writer

	mu      sync.Mutex
	written []string
}

// NewPrintSink builds the sink.
func NewPrintSink(w io.Writer) *PrintSink { return &PrintSink{w: w} }

// Record implements [TokenSink].
func (s *PrintSink) Record(_ context.Context, name, value string) error {
	line, err := envfile.FormatAssignment(name, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, name)
	_, err = fmt.Fprintln(s.w, line)
	return err
}

// Discard implements [TokenSink].
//
// It cannot unprint. What it can do is SAY so, by name, which is the whole
// of what an operator needs: these credentials have been revoked, so the
// lines above are dead and must not be pasted anywhere.
func (s *PrintSink) Discard(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) == 0 {
		return nil
	}
	fmt.Fprintf(s.w, "\n# THE VALUES ABOVE ARE REVOKED and must not be used: %s\n",
		strings.Join(s.written, ", "))
	s.written = nil
	return nil
}

// Flush implements [TokenSink].
func (s *PrintSink) Flush(context.Context) error { return nil }

// Describe implements [TokenSink].
func (s *PrintSink) Describe() string {
	return "standard output (nothing was persisted; copy them somewhere)"
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
