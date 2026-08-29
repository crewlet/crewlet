# d-203 — Secrets follow the config path

**Status:** decided; implementation outstanding. This file previously argued
the opposite and was wrong — see *The argument that did not hold* below, kept
because a decision record that quietly reverses itself teaches nobody why.

**Applies to:** `internal/store/secretvalues.go`, `internal/coord/fleet.go`,
`internal/api/configapi`, `cmd/crewlet/secrets.go`.

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

## Shape

- `coord.Secrets` beside the other buckets — get, set, delete, list over an
  opaque sealed value, so coordination never sees plaintext and the KV is
  storage rather than a party to the encryption.
- A `_secrets` bucket with **no age**: unlike the dedupe and the valve, a
  credential is not short-horizon state and a bucket TTL would expire a
  company's authentication.
- An authenticated route on `configapi`, which already holds the cipher and
  already seals a body onto the KV.
- `crewlet secrets` becomes a client of it, with the local-store path kept for
  a stopped engine.
- The env fallback is untouched. It is the bootstrap path — a node with no
  keyring resolves from the environment and runs normally — and it stays the
  answer for operators who want credentials to come from their platform.

## Transport

The coordination slot already takes a NATS `credentials` file (NKey/JWT) and a
`token`, and a `tls://` URL. Client-certificate mTLS is a small addition to
that block if a deployment wants it, and is orthogonal to this decision: the
values are sealed with the Tier A keyring before they reach the wire either
way, so the transport protects metadata and access rather than the secret.
