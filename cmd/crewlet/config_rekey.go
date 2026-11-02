package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet config rekey` — the config document's half of a keyring rotation.
//
// # Why the secret store's rekey is not enough
//
// A company's credentials live in two places sealed with the same Tier A
// keyring: the secret store, and the config document itself, which a founder
// may legitimately carry literals in. `crewlet secrets rekey` moves the
// first. Without this, the second stays sealed under whatever key sealed it
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
//
// # The revision this node BOOTS on
//
// That is the row the next start must open, so it is the one that has to move
// off a retired key — on a node that refused the fleet's revision, not the
// fleet's. The re-sealed copy is offered over what the boot row was built on: a
// rekeyed copy of a revision the fleet has moved past is this node's alone, and
// is never published over the company that replaced it.

// rekeyConfig re-seals the revision this node boots on under the keyring's
// active key.
//
// IDEMPOTENT, and the check is on the DENORMALISED key id rather than on a
// decrypt: a document already under the active key is left alone, so this is
// safe in a deploy script and a dry run costs no decryption at all.
func rekeyConfig(ctx context.Context, cs *configStore, dryRun bool, stdout io.Writer) error {
	active := activeKeyID(cs)
	rev, found, err := cs.configs.Boot(ctx)
	if err != nil {
		return fmt.Errorf("read the revision this node boots on: %w", err)
	}
	if !found {
		return errors.New("this node boots on no revision, so there is nothing to " +
			"re-seal; run `crewlet config import`")
	}
	// WHY IT IS NOT A SEAL THIS BUILD READS, before the key id: a plaintext
	// revision and one sealed in a form this build does not read both have
	// no key id, and they are refused for opposite reasons.
	if err = secrets.CheckSealed(rev.Payload); err != nil {
		return storedRefusal(rev.ID, err)
	}
	sealedUnder, _ := secrets.EnvelopeKeyIDOf(rev.Payload)
	if sealedUnder == active {
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
			"keyring cannot open. Restore that key to secrets.keys and "+
			"re-run: %w", rev.ID, sealedUnder, err)
	}
	id, err := reseal(ctx, cs, rev, document, fmt.Sprintf(
		"rekeyed from %s to %s", sealedUnder, active))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "re-sealed revision %s as %s under %s (was %s)\n",
		rev.ID, id, active, sealedUnder)
	fmt.Fprintln(stdout, rekeyPublishNote(offerOn(rev)))
	return nil
}

// reseal writes the document back as a new revision this node boots on,
// offered over what the revision it re-seals was built on.
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
	id, err := cs.configs.Offer(ctx, offerOn(parent), store.Revision{
		ParentID: parent.ID, Source: "rekey", CreatedBy: hostActor().Name,
		CreatedByKind: hostActor().Kind, Summary: summary, Payload: payload,
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
