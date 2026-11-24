package configapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// The stored values a removal takes with it, decided one way for every route
// [removalRoutes] serves: deleted only after the revision is written, each asked
// again of the fleet's revision first, and never one something else still reads.

// SecretsRequest is the part of a removal's body naming values to delete.
type SecretsRequest struct {
	// DeleteSecrets names stored values the entry read, to delete once the
	// removal is written. One something else still reads is kept.
	DeleteSecrets []string `json:"delete_secrets"`
}

// check refuses a name given twice and an empty one.
func (q SecretsRequest) check() error {
	first := map[string]int{}
	for i, secret := range q.DeleteSecrets {
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("delete_secrets[%d] names no value", i)
		}
		if j, seen := first[secret]; seen {
			return fmt.Errorf("delete_secrets[%d] names %q, as delete_secrets[%d] "+
				"does: name each value once", i, secret, j)
		}
		first[secret] = i
	}
	return nil
}

// KeptSecret is a stored value the removed entry read that something the
// removal leaves still reads, with the paths that read it: it stays.
type KeptSecret struct {
	Name   string   `json:"name"`
	ReadBy []string `json:"read_by"`
}

// RemovalSecrets is every stored value the removed entry read, as a dry run
// answers it. A variable a node takes from its environment is never listed.
type RemovalSecrets struct {
	// Secrets is each value nothing reads once the removal is written,
	// sorted: what `delete_secrets` may name. Never null.
	Secrets []string `json:"secrets"`
	// KeptSecrets is each value something the removal leaves still reads,
	// sorted by name. Never null.
	KeptSecrets []KeptSecret `json:"kept_secrets"`
}

// SecretOutcome is what became of one value a removal was asked to delete.
type SecretOutcome string

const (
	// SecretDeleted is a value the store held and no longer does.
	SecretDeleted SecretOutcome = "deleted"
	// SecretAbsent is a name the store held no value under: there was
	// nothing to delete.
	SecretAbsent SecretOutcome = "absent"
	// SecretKept is a value the fleet's revision still reads, so it stays.
	SecretKept SecretOutcome = "kept"
	// SecretFailed is a value the store or the fleet's revision did not
	// answer for: it may still be there, and `DELETE /secrets/{name}`
	// deletes it.
	SecretFailed SecretOutcome = "failed"
)

// Valid reports whether o is one of the four outcomes.
func (o SecretOutcome) Valid() bool {
	switch o {
	case SecretDeleted, SecretAbsent, SecretKept, SecretFailed:
		return true
	}
	return false
}

// SecretDeletion is one value a removal was asked to delete, and what became
// of it.
type SecretDeletion struct {
	Name    string        `json:"name"`
	Outcome SecretOutcome `json:"outcome"`
	// ReadBy is, for a value kept, the paths of the fleet's revision that
	// read it.
	ReadBy []string `json:"read_by,omitempty"`
}

// admitSecretDeletion asks `secrets.write` of a removal that deletes a value,
// its dry run too, so the Dashboard Assistant's preview is refused up front.
func admitSecretDeletion(w http.ResponseWriter, r *http.Request, names []string) bool {
	if len(names) == 0 {
		return true
	}
	p, ok := principalOf(w, r)
	if !ok {
		return false
	}
	policy := authz.Policy{Action: authz.ActionSecretWrite}
	d := authz.Decide(r.Context(), *p, policy.Action, authz.Object{Kind: authz.KindCompany},
		authz.NoChart{}, authz.DecidedAt(r.Context()))
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, policy, d)
		return false
	}
	return true
}

// entryReads is each name before reads at a path after no longer does, sorted,
// and the paths of after still reading each. after keeps before's paths
// ([removing.readsAfter]).
func entryReads(before, after []config.Reference) ([]string, map[string][]string) {
	still := map[config.Reference]bool{}
	for _, ref := range after {
		still[ref] = true
	}
	var gone []string
	for _, ref := range before {
		if !still[ref] {
			gone = append(gone, ref.Name)
		}
	}
	slices.Sort(gone)
	gone = slices.Compact(gone)
	readBy := map[string][]string{}
	for _, ref := range after {
		if _, found := slices.BinarySearch(gone, ref.Name); found {
			readBy[ref.Name] = append(readBy[ref.Name], ref.Path)
		}
	}
	return gone, readBy
}

// removalSecrets is every value the store holds that the removed entry read
// ([entryReads]), each deletable or kept.
func (s *Service) removalSecrets(ctx context.Context, before, after []config.Reference) (RemovalSecrets, error) {
	gone, readBy := entryReads(before, after)
	held, err := s.held(ctx, gone)
	if err != nil {
		return RemovalSecrets{}, err
	}
	out := RemovalSecrets{Secrets: []string{}, KeptSecrets: []KeptSecret{}}
	for _, name := range held {
		if paths := readBy[name]; len(paths) > 0 {
			out.KeptSecrets = append(out.KeptSecrets, KeptSecret{Name: name, ReadBy: paths})
			continue
		}
		out.Secrets = append(out.Secrets, name)
	}
	return out, nil
}

// secretProblems is each value the request names that the entry never read,
// at its index in the body: a removal deletes only what its entry read.
func secretProblems(asked []string, before, after []config.Reference, what string) []config.Problem {
	gone, _ := entryReads(before, after)
	var out []config.Problem
	for i, secret := range asked {
		if _, found := slices.BinarySearch(gone, secret); found {
			continue
		}
		path := config.Path{"delete_secrets", i}
		out = append(out, config.Problem{Path: path.String(), Segments: path, Kind: "unknown_value",
			Message: fmt.Sprintf("%s is not read by %s: a removal deletes only a value "+
				"its entry read", secret, what)})
	}
	return out
}

// deleteSecrets deletes each value named that the fleet's revision reads
// nothing of, and says what became of each; never null. A revision that cannot
// be read deletes nothing. entry is the log attributes naming what was removed.
func (s *Service) deleteSecrets(ctx context.Context, names []string, by iam.Actor,
	entry ...any) []SecretDeletion {

	out := make([]SecretDeletion, 0, len(names))
	if len(names) == 0 {
		return out
	}
	who := append(slices.Clone(entry), "by", by.Name, "operator", by.OperatorID)
	readBy, err := s.Reads(ctx, names)
	if err != nil {
		log.WarnContext(ctx, "secret_remove_failed", append(who, "names", names, "error", err,
			"detail", "the entry is removed; which paths read its values could not be "+
				"read, so none was deleted: DELETE /secrets/{name} deletes each")...)
		for _, secret := range names {
			out = append(out, SecretDeletion{Name: secret, Outcome: SecretFailed})
		}
		return out
	}
	for _, secret := range names {
		if paths := readBy[secret]; len(paths) > 0 {
			out = append(out, SecretDeletion{Name: secret, Outcome: SecretKept, ReadBy: paths})
			continue
		}
		removed, err := s.secrets.Unset(ctx, secret)
		if err != nil {
			log.WarnContext(ctx, "secret_remove_failed", append(who, "name", secret, "error", err)...)
			out = append(out, SecretDeletion{Name: secret, Outcome: SecretFailed})
			continue
		}
		// THE NAME, never the value, as `DELETE /secrets/{name}` logs it.
		log.InfoContext(ctx, "secret_removed", append(who, "name", secret, "removed", removed)...)
		outcome := SecretDeleted
		if !removed {
			outcome = SecretAbsent
		}
		out = append(out, SecretDeletion{Name: secret, Outcome: outcome})
	}
	return out
}

// held is the names the credential store holds a value under, of names, in
// their order. Never null.
func (s *Service) held(ctx context.Context, names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		_, stored, err := s.secrets.Describe(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("configapi: describe the stored value %s: %w", name, err)
		}
		if stored {
			out = append(out, name)
		}
	}
	return out, nil
}
