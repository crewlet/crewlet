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
// names the key. The key directory (`tracker_task_keys`) is first-writer-wins,
// a purge hands a key on to the lowest id still holding it as current
// ([handOnKey]), and nothing re-derives it, so two claims on one key applied in
// another order would split nodes for good. Beside a move's alias claim on the
// key's own subject ([subjectPath]), the three task records that write the
// directory state a [TermKey] term, under the key's own project, for every key
// they claim, free or hand on (a CREATE for its keys, a RE-KEY for the keys it
// held and the one it takes, a PURGE for its current and former keys), so a
// record deferred on a key holds back every later one. [Applier.maintainKeys]
// runs only when a record changes its task's keys, so an edit states none, and
// a co-holder's `key_collision` is derived by these records, never named.

// keyTerm is a key's address in the scope alphabet: the key under its OWN
// project, the prefix before the dash, never the holding task's project: a
// moved task keeps its former keys under the project that minted them, where
// every other record naming the key files it.
func keyTerm(key string) ScopeTerm {
	project, _, _ := strings.Cut(key, "-")
	return ScopeTerm{Kind: TermKey, Container: project, ID: key}
}

// mintedKey is the key a counter value takes in a project — "<PROJECT>-<n>": one
// spelling for the applier ([applyPatch]) and the writer, which names the key in
// the record's scope before the applier derives it.
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

// keysMoved reports whether after holds a different set of keys than before: a
// new current key, or a changed former-key list. The applier and the writer ask
// it of the same two documents, so the record allowed to write the directory is
// exactly the one whose writer had to name its keys.
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
// re-keyed it: the ones it held, then the key it is leaving, LAST (the newest
// former key is how a reader names it). It is formed from the task the decide
// read, never passed in: the list travels whole on the record, so one formed
// from an earlier read would drop keys the task gained since, leaving them
// opening it unrecorded and never handed on by its purge.
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
// keys, keeping whatever the scope already stated. The SUBJECT stays among them
// ([ScopeSet.withObjects]): a term-built scope has no sentinel, so without its
// own term the write would claim the keys and not its task.
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
// own project's path: its key term, its project's container, or the domain. It
// runs inside the decide, against the keys the decided rows hold, for
// [ScopeSet.covers]' reason: a task re-keyed since the scope was formed holds a
// key the request never probed.
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
		// NAMED, not silently widened ([ScopeSet.covers]): the request's scope is what
		// the publisher probed with.
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
// with the REQUEST's scope, and the decide re-checks the keys against its rows
// ([ScopeSet.coversKeys]). A task this node does not hold names only the key it
// takes, and the decide refuses it as missing. A writer with no replicated
// estate states the DOMAIN, for [Writer.scopeForPurge]'s reason: it cannot read
// the task's keys, so any narrower scope is refused on every attempt.
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
