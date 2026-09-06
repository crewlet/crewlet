package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// Writing a submission: the secret first, then the pointer, then the epoch.
//
// # The order is the whole of it
//
// A secret and the config slot that points at it are two writes, and every
// order but one has a window that breaks something:
//
//   - POINTER FIRST leaves the config naming a secret with nothing behind
//     it. The route it guards answers 503 to every delivery for as long as
//     the window lasts, which looks exactly like an attack.
//   - VALUE ONLY, with no activation, does nothing at all in a running
//     process. A ${VAR} is resolved from a snapshot taken when the epoch
//     was applied, so a value written into the store after that is invisible
//     until something activates. That is the failure the secret-store sink
//     already warns about at the end of every provisioning run.
//
// So: value, then pointer, then publish. And when the pointer needed no
// change (a rotation, where the config already names the right variable) the
// publish still happens, as a reload of the unchanged document, because
// otherwise the new value never reaches the seats.

// Secrets is the sealed store this package writes credentials into.
//
// Declared here rather than imported, because this is all of it this package
// uses: the fleet store's own type has a rekey, a lister and a reader, and a
// setup route must never be able to read a value back.
type Secrets interface {
	Set(ctx context.Context, name, value, by, source string, now time.Time) error
}

// Config is the company document this package patches.
//
// One method, and it is the exported core of PATCH /config, so a setup write
// merges, validates, seals and activates through exactly the code an operator
// driving that route by hand goes through.
type Config interface {
	Apply(ctx context.Context, patch []byte, summary, operator, expect string) (revisionID string, epoch int64, err error)
	Reload(ctx context.Context, summary, operator string) (revisionID string, epoch int64, err error)
	// Current is the revision a conditional submission is checked against
	// BEFORE anything is sealed. Apply performs the same check, and has
	// to, because the document can move between here and there. But by
	// the time Apply refuses, the credential is already in the store
	// under a name nothing points at.
	Current(ctx context.Context) (string, error)

	// Seat reads one seat's whole entity, as JSON, and SetSeat writes it
	// back under the same handle.
	//
	// A SEPARATE PATH FROM Apply, and it has to be: a merge patch replaces
	// an array wholesale, so patching `roles` to change one seat would
	// delete every other one. The entity route addresses a seat by its
	// handle, which is its identity rather than its position.
	Seat(ctx context.Context, handle string) ([]byte, error)
	SetSeat(ctx context.Context, handle string, body []byte, summary, operator, expect string) (revisionID string, epoch int64, err error)
}

// Source is the value the secret store records for a row this package wrote,
// so `crewlet secrets list` says which surface put it there.
const Source = "setup"

// Submission is one set of values for one integration.
type Submission struct {
	// Kind is the vendor these values belong to.
	Kind integration.Kind

	// Values are keyed by [Requirement.Field]. A field the requirement
	// list does not name is refused rather than ignored: it is a typo or
	// a stale client, and writing it nowhere while answering 201 is the
	// worst of both.
	Values map[string]string

	// Seat scopes a per-seat submission. Empty is company-wide.
	Seat string

	// Summary is the audit sentence. The caller supplies the vendor's
	// name and the verb; this package never puts a submitted VALUE in it,
	// because the summary is stored on the revision and rendered on a
	// screen.
	Summary string

	// Operator is who the revision and the secret rows record.
	Operator string

	// Expect is the revision the requirement list was computed against.
	Expect string
}

// Result is what a submission produced.
type Result struct {
	RevisionID string   `json:"revision_id"`
	Epoch      int64    `json:"epoch"`
	Secrets    []string `json:"wrote_secrets"`
	// Reloaded reports that the document did not change and the epoch was
	// advanced anyway, so a rotated value reached the running seats.
	Reloaded bool `json:"reloaded"`
}

// Writer performs a submission against one company.
type Writer struct {
	Secrets Secrets
	Config  Config
	Now     func() time.Time
}

// Write applies a submission: secrets first, then the config, then publish.
//
// The requirements carry what the document holds today, in [Requirement.Stored],
// which is how a secret requirement decides between minting a new pointer,
// writing through an existing one, and refusing a literal.
func (w Writer) Write(ctx context.Context, reqs []Requirement, in Submission) (Result, error) {
	if w.Config == nil {
		return Result{}, fmt.Errorf("setup: no config surface on this process")
	}
	// THE STALE-BASE CHECK COMES FIRST, before a single value is sealed.
	// A caller working from a page it read a minute ago would otherwise
	// end the request refused AND with a credential in the store that
	// nothing points at, which somebody then has to find and remove.
	if in.Expect != "" {
		active, err := w.Config.Current(ctx)
		if err != nil {
			return Result{}, err
		}
		if active != in.Expect {
			return Result{}, &ErrStaleBase{Base: in.Expect, Current: active}
		}
	}

	byField := map[string]Requirement{}
	for _, r := range reqs {
		byField[r.Field] = r
	}

	// A PER-SEAT SUBMISSION IS A DIFFERENT WRITE, and it is decided here
	// rather than per field: every requirement in one submission belongs
	// to the same seat or to none, because the screen collects one seat's
	// form at a time.
	if in.Seat != "" {
		return w.writeSeat(ctx, reqs, in)
	}

	// The patch is built BEFORE anything is written, so a submission naming
	// a field this vendor does not have is refused having changed nothing.
	patch := map[string]any{}
	type pending struct {
		name  string
		value string
	}
	var secretsToWrite []pending
	for field, value := range in.Values {
		r, ok := byField[field]
		if !ok {
			return Result{}, fmt.Errorf(
				"setup: %s has no field %q; the requirement list names %s",
				in.Kind, field, strings.Join(fields(reqs), ", "))
		}
		if r.Kind != KindSecret {
			if err := setPath(patch, r.ConfigPath, typed(r.Kind, value)); err != nil {
				return Result{}, err
			}
			continue
		}
		if w.Secrets == nil {
			return Result{}, fmt.Errorf(
				"setup: %s is a credential and this process has no secret store to seal it in",
				r.ConfigPath)
		}
		name, writePointer, err := PointerFor(in.Kind, r, r.Stored)
		if err != nil {
			return Result{}, err
		}
		secretsToWrite = append(secretsToWrite, pending{name: name, value: value})
		if writePointer {
			if err := setPath(patch, r.ConfigPath, "${"+name+"}"); err != nil {
				return Result{}, err
			}
		}
	}

	// VALUE FIRST. See the note at the top of this file: the other order
	// leaves the config naming a secret with nothing behind it, and the
	// route it guards refusing every delivery for the length of the window.
	now := w.now()
	written := []string{}
	for _, p := range secretsToWrite {
		if err := w.Secrets.Set(ctx, p.name, p.value, in.Operator, Source, now); err != nil {
			// Named, never valued. The name is a fact an operator needs;
			// the value is the one thing that must not reach a log or a
			// response body.
			return Result{}, fmt.Errorf("setup: seal %s: %w", p.name, err)
		}
		written = append(written, p.name)
	}

	if len(patch) == 0 {
		// NOTHING TO PATCH, and still something to do: the pointer was
		// already correct and only the value changed, which a running
		// process cannot see until an activation refreshes its snapshot.
		if len(written) == 0 {
			return Result{}, fmt.Errorf("setup: the submission carried no values")
		}
		id, epoch, err := w.Config.Reload(ctx, in.Summary, in.Operator)
		if err != nil {
			return Result{}, err
		}
		return Result{RevisionID: id, Epoch: epoch, Secrets: written, Reloaded: true}, nil
	}

	body, err := json.Marshal(patch)
	if err != nil {
		return Result{}, fmt.Errorf("setup: encode the patch: %w", err)
	}
	id, epoch, err := w.Config.Apply(ctx, body, in.Summary, in.Operator, in.Expect)
	if err != nil {
		// THE SECRETS STAY. A value sealed under a name nothing points at
		// yet is inert and harmless, and deleting it here would be a
		// second write on the path where one has already failed. The
		// caller re-submits, the write is idempotent by name, and
		// `crewlet secrets` lists anything genuinely orphaned.
		return Result{Secrets: written}, err
	}
	return Result{RevisionID: id, Epoch: epoch, Secrets: written}, nil
}

// writeSeat is the per-seat half: the same ordering, through the entity route.
//
// The order is the same and for the same reasons: the credential is sealed
// first, then the seat's own document is given a `${VAR}` pointing at it, then
// the epoch advances. What differs is only the address, because a seat is
// addressed by its handle and a merge patch cannot reach one.
func (w Writer) writeSeat(ctx context.Context, reqs []Requirement, in Submission) (Result, error) {
	byField := map[string]Requirement{}
	for _, r := range reqs {
		byField[r.Field] = r
	}
	entity, err := w.Config.Seat(ctx, in.Seat)
	if err != nil {
		return Result{}, err
	}
	var seat map[string]any
	if err := json.Unmarshal(entity, &seat); err != nil {
		return Result{}, fmt.Errorf("setup: read seat %s: %w", in.Seat, err)
	}

	changed := false
	type pending struct{ name, value string }
	var secretsToWrite []pending
	for field, value := range in.Values {
		r, ok := byField[field]
		if !ok {
			return Result{}, fmt.Errorf(
				"setup: %s has no field %q; the requirement list names %s",
				in.Kind, field, strings.Join(fields(reqs), ", "))
		}
		if r.Kind != KindSecret {
			if err := setPath(seat, r.ConfigPath, typed(r.Kind, value)); err != nil {
				return Result{}, err
			}
			changed = true
			continue
		}
		if w.Secrets == nil {
			return Result{}, fmt.Errorf(
				"setup: %s is a credential and this process has no secret store to seal it in",
				r.ConfigPath)
		}
		name, writePointer, err := PointerFor(in.Kind, r, r.Stored)
		if err != nil {
			return Result{}, err
		}
		secretsToWrite = append(secretsToWrite, pending{name: name, value: value})
		if writePointer {
			if err := setPath(seat, r.ConfigPath, "${"+name+"}"); err != nil {
				return Result{}, err
			}
			changed = true
		}
	}

	now := w.now()
	written := []string{}
	for _, p := range secretsToWrite {
		if err := w.Secrets.Set(ctx, p.name, p.value, in.Operator, Source, now); err != nil {
			return Result{}, fmt.Errorf("setup: seal %s: %w", p.name, err)
		}
		written = append(written, p.name)
	}

	if !changed {
		// The pointer was already right and only the value rotated, which
		// a running process cannot see until an activation refreshes its
		// snapshot.
		if len(written) == 0 {
			return Result{}, fmt.Errorf("setup: the submission carried no values")
		}
		id, epoch, err := w.Config.Reload(ctx, in.Summary, in.Operator)
		if err != nil {
			return Result{}, err
		}
		return Result{RevisionID: id, Epoch: epoch, Secrets: written, Reloaded: true}, nil
	}

	body, err := json.Marshal(seat)
	if err != nil {
		return Result{}, fmt.Errorf("setup: encode seat %s: %w", in.Seat, err)
	}
	id, epoch, err := w.Config.SetSeat(ctx, in.Seat, body, in.Summary, in.Operator, in.Expect)
	if err != nil {
		return Result{Secrets: written}, err
	}
	return Result{RevisionID: id, Epoch: epoch, Secrets: written}, nil
}

func (w Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// fields names every field a requirement list declares, for a refusal that
// tells the caller what it could have sent.
func fields(reqs []Requirement) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Field)
	}
	return out
}

// setPath writes a value into a nested map from a dotted config path.
//
// Merge-patch shaped: `integrations.datadog.route_to` becomes
// {"integrations":{"datadog":{"route_to":"..."}}}, so two fields of one
// vendor merge into one object rather than the second replacing the first.
//
// A list position is REFUSED rather than guessed at. The grammar has one
// (`roles[2].llm`), and a merge patch cannot address a list element at all:
// merging an array replaces it wholesale, so a patch built this way would
// silently delete every other seat. A per-seat write addresses its seat
// through the entity routes instead.
// typed turns a submitted string into the JSON value its kind is.
//
// Every kind but one is a string in the document. A toggle is a boolean, and
// the strict reader refuses "true" for a bool field, so a patch carrying the
// string would be rejected on every submission.
func typed(kind Kind, value string) any {
	if kind != KindToggle {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

func setPath(into map[string]any, path string, value any) error {
	if path == "" {
		return fmt.Errorf("setup: a requirement declares no config path")
	}
	if strings.ContainsAny(path, "[]") {
		return fmt.Errorf(
			"setup: %s addresses a list position, which a merge patch cannot "+
				"reach without replacing the whole list", path)
	}
	segments := strings.Split(path, ".")
	node := into
	for _, segment := range segments[:len(segments)-1] {
		next, ok := node[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[segment] = next
		}
		node = next
	}
	node[segments[len(segments)-1]] = value
	return nil
}
