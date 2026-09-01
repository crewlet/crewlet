package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/envfile"
	"github.com/crewlet/crewlet/internal/secrets"
)

// ---- the secret store ------------------------------------------------- //

// SecretStore is the encrypted store a minted credential is recorded in.
//
// AN INTERFACE, because there are two: the fleet's, on the coordination KV,
// which every node reads; and this node's own table, which is what a
// provisioning run can reach while the engine is stopped. Naming either
// concretely here would have made a provisioner that works on a stopped node
// and a provisioner that works on a running fleet two different sinks.
type SecretStore interface {
	Set(ctx context.Context, name, value, by, source string, now time.Time) error
	Get(ctx context.Context, name string) (string, error)
	Unset(ctx context.Context, name string) (bool, error)
}

// SecretStoreSink writes minted credentials into the encrypted secret store.
//
// THE ONE WITH NO FILE TO SOURCE. The engine reads these back directly, so
// there is nothing for an operator to copy anywhere and nothing to keep in
// sync — which also means a rotation takes effect on the next config
// activation rather than on the next deploy of a file.
type SecretStoreSink struct {
	values SecretStore
	by     string

	mu      sync.Mutex
	written []string
}

// NewSecretStoreSink builds the sink.
func NewSecretStoreSink(values SecretStore, by string) *SecretStoreSink {
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

// Value implements [TokenSink].
//
// A row this run wrote counts too, and deliberately: a value recorded a
// moment ago is held, and a caller asking twice must get the same answer
// whether or not the row existed before the run started.
func (s *SecretStoreSink) Value(ctx context.Context, name string) (string, bool, error) {
	value, err := s.values.Get(ctx, name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return "", false, nil
	case err != nil:
		// UNREADABLE IS NOT ABSENT. A store that cannot be read would
		// otherwise make every credential look missing, and the caller
		// would rotate the lot — which is the outage this exists to
		// prevent, arriving through the failure path instead.
		return "", false, err
	}
	value = strings.TrimSpace(value)
	return value, value != "", nil
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

// NextStep implements [TokenSink].
//
// NOT "nothing", which is the answer the store's convenience invites. The
// engine resolves ${VAR} through a SNAPSHOT of the store, rebuilt on every
// config apply — so a value written here reaches a running process at the
// next apply and not before, and re-activating the current revision is the
// documented gesture for exactly this.
func (s *SecretStoreSink) NextStep() string {
	return "re-activate the current revision (`crewlet config activate <uuid>`) " +
		"so the running engine rebuilds its secret snapshot; a fresh start " +
		"picks it up on its own"
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
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("provision: %s: %w", path, err)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
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

// Value implements [TokenSink], from what the file carries.
func (s *EnvFileSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := strings.TrimSpace(s.values[name])
	return value, value != "", nil
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

// NextStep implements [TokenSink].
func (s *EnvFileSink) NextStep() string {
	return "source " + s.path + " into the engine's environment and restart it"
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
	slices.Sort(names)

	var body strings.Builder
	body.WriteString("# Written by `crewlet ... provision`. Values are single-quoted\n" +
		"# for `source`; the engine reads its ${VAR}s from the process\n" +
		"# environment, so this file has to be sourced before it starts.\n")
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
	defer func() { _ = os.Remove(tmp.Name()) }()
	// NO CHMOD, and that is load-bearing rather than an omission:
	// os.CreateTemp documents 0600, and the rename below carries the temp
	// file's mode onto the destination. A chmod after the rename would be
	// too late — the window it was meant to close has already been open —
	// and one before it would be a second statement of a guarantee the
	// standard library already makes.
	if _, err := io.WriteString(tmp, body.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("provision: %s: %w", s.path, err)
	}
	// DURABILITY BEFORE VISIBILITY, the rule hostbox.CopyFileAtomic states
	// and these two did not: os.Rename is atomic against a PROCESS crash,
	// not a machine one. A rename can be observed before the data behind it
	// reaches disk, so a power loss between the two leaves a file that
	// exists and is empty — and this file's own doc calls its contents the
	// only record of a live credential.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

// NewPrintSink builds the sink, refusing a nil stream.
//
// Refused rather than defaulted, because of WHEN a nil one fails: nothing
// notices until Record writes the first credential, which is after the run
// has already minted it against the vendor. The operator is then handed a
// panic in place of the token that now exists in GitLab and nowhere else.
// A sink with nowhere to write is not a sink, so it is refused at the point
// the caller can still do something about it — the same shape
// [NewEnvFileSink] takes for the same reason.
func NewPrintSink(w io.Writer) (*PrintSink, error) {
	if w == nil {
		return nil, errors.New("provision: the print sink has no stream to " +
			"write to, so a minted credential would be destroyed on the way out")
	}
	return &PrintSink{w: w}, nil
}

// Value implements [TokenSink]: nothing is ever held.
//
// This sink persists NOTHING — the value went to a terminal and this
// process has no idea what became of it. Not-held is the correct answer
// rather than a degradation: an operator who chose -print is asking to be
// handed the credentials, and one that was not minted cannot be pasted
// anywhere.
func (s *PrintSink) Value(context.Context, string) (string, bool, error) {
	return "", false, nil
}

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
// # It emits `unset`, not a comment
//
// It cannot unprint, but this stream is meant to be SOURCED — that is the
// whole reason it emits `export` lines — and a comment is a no-op to a shell.
// An operator who piped the output into `source` and then hit a rollback
// would keep a revoked token exported in their session, with the only warning
// being a line the shell threw away. So the rollback is itself a statement:
// sourcing the stream to its end leaves the environment as it started.
//
// The comment stays too, after the statements, because a person READING the
// output needs the sentence and `unset` alone does not say why.
func (s *PrintSink) Discard(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) == 0 {
		return nil
	}
	for _, name := range s.written {
		fmt.Fprintf(s.w, "unset %s\n", name)
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

// NextStep implements [TokenSink].
//
// The one sink that has already lost the values if the operator does not
// act, which is why this says "before you close this terminal".
func (s *PrintSink) NextStep() string {
	return "put these into the engine's environment before you close this " +
		"terminal — nothing here was persisted — and restart it"
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
