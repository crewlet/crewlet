package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet config seal` and `crewlet config rekey` — the config document's
// half of a keyring rotation.
//
// # Why the secret store's rekey is not enough
//
// A company's credentials live in two places sealed with the same Tier A
// keyring: the secret store, and the config document itself, which a founder
// may legitimately carry literals in. `crewlet secrets rekey` moves the
// first. Without these, the second stays sealed under whatever key sealed it
// — so an operator who follows the documented rotation, sees `secrets rekey`
// report success and drops the retired key has just made their company
// configuration unreadable on every node, at the next apply, with no way back
// short of restoring the key from a backup.
//
// # A NEW REVISION, never an edit
//
// Revisions are immutable and the activation pointer is append-only, which is
// what makes the history a record rather than a claim. Re-sealing writes a new
// revision carrying the same document, exactly as `import` and `revert` do.

// sealConfig re-stores a plaintext active revision as a sealed one.
//
// The one-time migration off plaintext-at-rest: a deployment that ran before
// a keyring existed has a plaintext revision, and nothing re-seals it on its
// own — `import` seals what it writes, but only when something is imported.
func sealConfig(ctx context.Context, cs *configStore, bootstrapPath string, stdout io.Writer) error {
	if cs.cipher == nil {
		return fmt.Errorf(
			"%s declares no secrets.keys, so there is no key to seal with; "+
				"run `crewlet secrets keygen` and add one", bootstrapPath)
	}
	rev, err := revisionOrActive(ctx, cs, "")
	if err != nil {
		return err
	}
	if _, sealed := secrets.EnvelopeKeyIDOf(rev.Payload); sealed {
		fmt.Fprintf(stdout, "revision %s is already sealed; nothing to do\n", rev.ID)
		return nil
	}
	// OPENED THROUGH THE SAME PATH every other reader uses, so a payload
	// this build cannot make sense of is refused here rather than stored
	// again in a shape no node can apply.
	document, err := secrets.Open(cs.cipher, rev.Payload)
	if err != nil {
		return fmt.Errorf("open revision %s: %w", rev.ID, err)
	}
	id, err := reseal(ctx, cs, rev, document, "sealed under "+activeKeyID(cs))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sealed revision %s as %s under %s\n", rev.ID, id, activeKeyID(cs))
	fmt.Fprintln(stdout, publishNote)
	return nil
}

// rekeyConfig re-seals the active revision under the keyring's active key.
//
// IDEMPOTENT, and the check is on the DENORMALISED key id rather than on a
// decrypt: a document already under the active key is left alone, so this is
// safe in a deploy script and a dry run costs no decryption at all.
func rekeyConfig(ctx context.Context, cs *configStore, bootstrapPath string,
	dryRun bool, stdout io.Writer,
) error {
	if cs.cipher == nil {
		return fmt.Errorf(
			"%s declares no secrets.keys, so there is no active key to "+
				"re-seal under; run `crewlet secrets keygen` and add one",
			bootstrapPath)
	}
	active := activeKeyID(cs)
	rev, err := revisionOrActive(ctx, cs, "")
	if err != nil {
		return err
	}
	sealedUnder, sealed := secrets.EnvelopeKeyIDOf(rev.Payload)
	switch {
	case !sealed:
		// SAID RATHER THAN SILENTLY SEALED. "Rotate the key this is under"
		// and "start encrypting this at all" are different decisions, and
		// an operator running a rotation script has not asked for the
		// second — see [sealConfig].
		return fmt.Errorf(
			"revision %s is stored in plaintext, so there is no key to "+
				"rotate; `crewlet config seal` encrypts it under %s first",
			rev.ID, active)
	case sealedUnder == active:
		fmt.Fprintf(stdout, "revision %s is already sealed under %s\n", rev.ID, active)
		return nil
	}
	if dryRun {
		fmt.Fprintf(stdout, "revision %s is sealed under %s and would be "+
			"re-sealed under %s\n", rev.ID, sealedUnder, active)
		return nil
	}
	document, err := secrets.Open(cs.cipher, rev.Payload)
	if err != nil {
		// THE KEY THAT SEALED IT IS NAMED, because the fix is to put that
		// key back in secrets.keys and the error is otherwise a decrypt
		// failure with nothing to act on.
		return fmt.Errorf("revision %s is sealed under %s, which this "+
			"keyring cannot open — restore that key to secrets.keys and "+
			"re-run: %w", rev.ID, sealedUnder, err)
	}
	id, err := reseal(ctx, cs, rev, document, fmt.Sprintf(
		"rekeyed from %s to %s", sealedUnder, active))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "re-sealed revision %s as %s under %s (was %s)\n",
		rev.ID, id, active, sealedUnder)
	fmt.Fprintln(stdout, publishNote)
	return nil
}

// reseal writes the document back as a new active revision.
func reseal(ctx context.Context, cs *configStore, parent store.Revision,
	document []byte, summary string,
) (string, error) {
	payload, err := secrets.Seal(cs.cipher, document)
	if err != nil {
		return "", err
	}
	if _, ok := secrets.EnvelopeKeyIDOf(payload); !ok {
		// UNREACHABLE with a non-nil cipher, and checked anyway: storing
		// an unsealed payload from a command whose whole job is to seal
		// one would report success over a document still in the clear.
		return "", errors.New("config: the keyring produced no sealed payload")
	}
	id, err := cs.configs.InsertActive(ctx, store.Revision{
		ParentID: parent.ID, Source: "rekey", CreatedBy: currentOperator(),
		Summary: summary, Payload: payload,
	})
	if err != nil {
		return "", fmt.Errorf("store the re-sealed revision: %w", err)
	}
	return id, nil
}

// activeKeyID is the key a fresh seal uses, read from Tier A.
//
// Held on the store rather than re-read, because the cipher was built from
// the same document and the two disagreeing is how a rekey reports moving a
// row onto a key it did not use.
func activeKeyID(cs *configStore) string { return cs.activeKeyID }
