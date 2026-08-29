# d-203 — Secrets follow the config path

**Status:** decided and implemented. This file previously argued the opposite
and was wrong — see *The argument that did not hold* below, kept because a
decision record that quietly reverses itself teaches nobody why.

**Applies to:** `internal/fleetsecrets`, `internal/coord/fleet.go`,
`internal/api/secretsapi`, `internal/store/secretvalues.go`,
`cmd/crewlet/secrets.go`.

## The question

Six kinds of fleet-shared state moved onto the coordination KV under d-201.
`secret_values` did not, so `crewlet secrets set` reaches exactly the node
whose Tier A file it was pointed at. On three nodes an operator runs it three
times or the rotation half-lands, and the failure is silent until a seat lands
on a node that never got the value.

## The argument that did not hold

The first version of this file said moving it would force the CLI to write
through the engine, over "a route whose body is a plaintext credential", and
that this widens the surface an auth bypass would expose. Both halves are
false, and checking either would have shown it:

- **`PUT /config` already accepts credentials in a body.** Inlining literals
  in the company document is a supported shape — discouraged on this page's
  own advice for rotation and blast-radius reasons, not refused — so the API
  already carries them. Adding a secrets route adds no transport class that
  is not already there.
- **The KV already carries Tier-A-sealed bytes, fleet-wide.** `Plane.Activate`
  writes `secrets.Seal(cipher, document)` into the `_config` bucket and every
  node reads it there, opening it with its own keyring. That is the SAME
  cipher and the same keyring `secret_values` uses. Secrets on the KV would be
  the same ciphertext class, in the same bucket family, over the same
  connection.

So the config document — which may itself contain credentials — already
travels the exact path the secret store was being kept off. The asymmetry was
the anomaly, not the safeguard.

The one constraint that IS real got buried under the wrong one: the KV is
unreachable from a second process on the solo embedded topology, so the CLI
cannot write it directly there. That is an argument about the CLI's route in,
not about where the rows live — and config already answers it.

## What was decided

**Secrets replicate the way config does.** One sealed copy on the coordination
KV, written through the authenticated API, read by every node.

There is no leader and none is needed: the KV is the shared truth, any node's
API can write it, and every node already polls the activation pointer. That is
the shape the config plane has today, and secrets adopting it removes a
special case rather than adding a mechanism.

The engine-down path stays, because config has it too — `crewlet config
import` writes the local store with no engine running, and bootstrap needs the
equivalent. Which route the CLI takes is no longer a guess: the store's file
lock (`internal/store/lock.go`) makes "engine up" a refusal with a name on it,
so the CLI can try the API and fall back to the file exactly when the engine
is not holding it.

## Shape, as built

- **`coord.Secrets`** beside the other buckets — get, set, delete, list over an
  opaque sealed value, so coordination never sees plaintext and the KV is
  storage rather than a party to the encryption. Certified by the same
  `coordtest` suite as every other bucket, on both backends.
- **A `_secrets` bucket with no age.** Unlike the dedupe and the valve, a
  credential is not short-horizon state and a bucket TTL would expire a
  company's authentication. A delete is a `Purge` rather than a delete, so a
  removed secret leaves no tombstone carrying its envelope.
- **`internal/fleetsecrets`** owns the KEY; coordination owns the BYTES. It is
  where a value is sealed on the way in and opened on the way out, with the
  name bound in as associated data — the same binding `store.SecretValues`
  uses, which is what lets either read a row the other wrote during the
  migration.
- **`internal/api/secretsapi`** serves `/secrets`, added to `auth`'s
  always-guarded prefixes beside `/config`. It went there rather than onto
  `configapi` because the honest URL is `/secrets`, and the guard change is
  one list entry with a test that walks it.
- **`crewlet secrets` is a client of it**, and the routing is a fact rather
  than a probe: the store's file lock means "the engine is up, write through
  its API" with a pid attached, and an unlocked store means it is stopped and
  the local table is the only place a value can go. `-secret-store` on every
  provisioner follows the same path, and both take `-api URL`.
- **The local table is the bootstrap path and clears itself.** The engine
  migrates its rows onto the fleet at boot and removes them: copy before
  delete, never overwrite a name the fleet already holds (the fleet's copy is
  the newer write), never delete what did not copy. Without the delete a stale
  local row would undo a later `unset` at every boot, forever.
- **One record type.** `store.SecretRecord` and a second on `coord` would have
  made the CLI, the API and the provisioning sinks each pick a side, so both
  stores answer in `secrets.Record` and the sentinels moved to
  `secrets.ErrNoKeyring` / `secrets.ErrNotFound` with them.
- **The env fallback is untouched.** It is the bootstrap path — a node with no
  keyring resolves from the environment and runs normally — and it stays the
  answer for operators who want credentials to come from their platform.

## One thing that changed on the way

`fleetsecrets.All` **fails closed**, like the local store's, rather than
skipping a record this node's keyring cannot open. The first draft skipped,
reasoning that a fleet mid-rotation legitimately holds mixed keys. It does not:
the config plane seals its payload with the same keyring, so a node that cannot
open a secret cannot apply a config either. A skipped record resolves from the
environment instead — the stale-`.env` shadowing the store exists to prevent,
arriving silently. `Rekey` aborts for the same reason: a pass that moved 12 of
13 rows is the state an operator retires the old key on the strength of.

## Transport

Orthogonal to this decision, and worth stating so nobody reads it as load
bearing: the values are sealed with the Tier A keyring before they reach the
wire either way, so the transport protects metadata and access rather than the
secret. A peer that can read the bucket learns which names exist and when they
changed, not what they are.

It was built anyway, because the question exposed a real gap. The coordination
slot took a NATS `credentials` file (NKey/JWT) and a `token`, but nothing for
the TCP layer underneath — so a broker configured `tls { verify: true }`, the
hardened default every NATS operator guide recommends, was unreachable, as was
any estate behind a private CA. `stream.tls` and `coordination.nats.tls` now
carry `ca`, `cert` and `key`. Neither can express "do not verify".
