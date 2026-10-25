package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// WHO HOLDS A KEY IS DECIDED IN LOG ORDER, so every record that can change it
// names the key.
//
// The key directory (`tracker_task_keys`) is FIRST-WRITER-WINS: a claim on a
// key another task already holds is left as it is, because taking it would
// re-point every link, chat message and comment anybody ever wrote — and a
// purge HANDS a claimant's keys on to the lowest id among the tasks still
// holding them as their current key ([handOnKey]), re-deriving every holder's
// `key_collision` as it goes. Both answers depend on which claims, moves and
// purges of that key a node has applied, and in what order. Records about
// DIFFERENT tasks are assumed to commute wherever their scopes do not meet,
// and two claims on one key do not commute: on a node that retained one
// task's create — a newer peer's record, or one signed under a key it was
// not restarted with — a second task's create of the same key, or the
// claimant's purge, applied first and handed the key to whoever was there,
// while every other node handed it to the task whose create it had applied.
// Nothing re-derives the directory afterwards, so the split was permanent.
//
// So beside the alias claim a move takes first — a record on the key's own
// subject, filed there already ([subjectPath]) — the directory is written by
// exactly three kinds of task record, and each states a [TermKey] term for
// every key whose claim it can create, release or hand on, filed under the
// key's own project:
//
//   - a task's CREATE, for every key the task carries;
//   - a write that RE-KEYS a task — a move's step, carrying the value it
//     minted — for the keys it held before and the key it takes: the old
//     current key becomes a former one, which changes who a later purge hands
//     that key to;
//   - a PURGE, for the purged task's current and former keys, which it
//     releases and hands on.
//
// A record deferred on a key therefore holds back every later record that
// claims, frees or hands on that key, on every node, and the claim lands
// where the log order puts it. Every other task record leaves the directory
// alone ([Applier.maintainKeys] runs only when the record changes the task's
// keys), which is what lets an ordinary edit state no key at all.
//
// No co-holder of the key is named as an object: whatever a claim or a
// hand-on writes on another task's row is `key_collision`, a cache of the
// directory derived by these same records, and every record that can move it
// is filed under the key.

// keyTerm is a key's address in the scope alphabet: the key under its OWN
// project, which is the prefix before the dash — never the project of the
// task that holds it, because a moved task keeps its former keys under the
// project that minted them, and a record about that key is filed there by
// everybody else who names it.
func keyTerm(key string) ScopeTerm {
	project, _, _ := strings.Cut(key, "-")
	return ScopeTerm{Kind: TermKey, Container: project, ID: key}
}

// mintedKey is the key a counter value takes in a project — "<PROJECT>-<n>".
//
// ONE SPELLING for the applier, which derives a moved task's key from the mint
// its record carries ([applyPatch]), and the writer, which has to name that
// key in the record's scope before the applier has derived it.
func mintedKey(project string, n uint64) string {
	return fmt.Sprintf("%s-%d", project, n)
}

// keysOf is every key a task holds — its current key and its former ones —
// once each, the empty ones dropped.
func keysOf(task Task) []string {
	out := make([]string, 0, 1+len(task.FormerKeys))
	for _, key := range append([]string{task.Key}, task.FormerKeys...) {
		if key != "" && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}

// keysMoved reports whether after holds a different set of keys than before:
// a new current key, or a former-key list that changed.
//
// IT IS THE APPLIER'S QUESTION AND THE WRITER'S, asked of the same two
// documents — the stored task and the task the record leaves — so the record
// the applier lets write the directory is exactly the record whose writer had
// to name its keys.
func keysMoved(before, after Task) bool {
	return before.Key != after.Key || !slices.Equal(before.FormerKeys, after.FormerKeys)
}

// mintsKey is the key a minting patch files its task under: the mint's value
// in the project the patch moves the task to, or the one it is in.
func mintsKey(project string, patch TaskPatch) string {
	if patch.Project != nil {
		project = *patch.Project
	}
	return mintedKey(project, patch.Mint.N)
}

// formerKeysAfterRekey is the former-key list a task holds once a mint has
// re-keyed it: the ones it already held, and the key it is leaving, LAST —
// the key a move left is the newest former key, which is how a reader names
// it.
//
// FORMED FROM THE TASK THE DECIDE READ, never carried in by a caller: the list
// travels whole on the record, and one formed from an earlier read replaced
// the former keys a task had gained since — a task moved away and back
// between a walk's read and its step lost two of its keys, which then opened
// it with nothing on it saying so and were never handed on by its purge.
func formerKeysAfterRekey(current Task) []string {
	out := slices.DeleteFunc(slices.Clone(current.FormerKeys), func(key string) bool {
		return key == "" || key == current.Key
	})
	if current.Key != "" {
		out = append(out, current.Key)
	}
	return out
}

// withKeys turns a sentinel scope into the enumeration that also names these
// keys, keeping whatever the scope already stated.
//
// THE SUBJECT IS ALWAYS AMONG THEM, for [ScopeSet.withObjects]' reason: a
// scope built from terms carries no sentinel, so dropping the subject's own
// term would claim the keys and not the task being written.
func (s ScopeSet) withKeys(subject string, keys []string) ScopeSet {
	named := make([]ScopeTerm, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		term := keyTerm(key)
		if !slices.Contains(named, term) && !slices.Contains(s.Terms, term) {
			named = append(named, term)
		}
	}
	if len(named) == 0 {
		return s
	}
	terms := make([]ScopeTerm, 0, len(s.Terms)+len(named)+1)
	if s.Subject {
		terms = append(terms, ScopeTerm{
			Kind: TermObject, Container: s.Container, ID: subject,
		})
	}
	terms = append(terms, s.Terms...)
	return ScopeSet{Terms: append(terms, named...)}
}

// coversKeys reports whether this scope names every one of these keys at its
// own project's path: its key term, its project's container, or the domain.
//
// CHECKED INSIDE THE DECIDE, against the keys the rows it decides on hold,
// for [ScopeSet.covers]' reason: the scope was formed before the snapshot,
// from a read or from the caller's view of the task, and a task re-keyed in
// between holds a key the request never probed.
func (s ScopeSet) coversKeys(keys []string) error {
	for _, key := range keys {
		if key == "" {
			continue
		}
		want := keyTerm(key)
		covered := slices.ContainsFunc(s.Terms, func(t ScopeTerm) bool {
			switch t.Kind {
			case TermDomain:
				return true
			case TermContainer:
				return t.named() == want.Container
			case TermKey:
				return t.ID == want.ID && t.container() == want.container()
			}
			return false
		})
		if covered {
			continue
		}
		// NAMED, AND NOT SILENTLY WIDENED, for [ScopeSet.covers]' reason:
		// the request's scope is what the publisher probed with.
		return fmt.Errorf("worklet: key %s changes hands under this write and "+
			"the write's scope does not name it, so a record deferred under "+
			"that key would not hold this one back — re-run it, and the "+
			"second attempt names the keys the task holds now: %w",
			key, statelog.ErrConflict)
	}
	return nil
}

// scopeForRekey widens a minting write's scope to every key its task holds on
// this node and the key the mint takes, read before the request for
// [Writer.scopeForDependents]' reason: the publisher probes the deferral index
// with the REQUEST's scope, so the keys have to be named before the decide
// runs, and the decide checks them again against the rows it decides on
// ([ScopeSet.coversKeys]). A task this node does not hold names only the key
// it takes here, and the decide refuses it as missing.
//
// A WRITER WITH NO REPLICATED ESTATE states the DOMAIN, for
// [Writer.scopeForPurge]'s reason: it cannot read which keys the task holds,
// and every task holds one, so any narrower scope is one the decide refuses
// on every attempt.
func (w *Writer) scopeForRekey(ctx context.Context, id, project string,
	patch TaskPatch, scope ScopeSet) (ScopeSet, error) {

	if w.db.IsZero() {
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}, nil
	}
	var keys []string
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		if err != nil || !held {
			return err
		}
		keys = keysOf(current)
		return nil
	}); err != nil {
		return ScopeSet{}, fmt.Errorf("worklet: read task %s's keys to scope its "+
			"re-key: %w", id, err)
	}
	return scope.withKeys(id, append(keys, mintsKey(project, patch))), nil
}
