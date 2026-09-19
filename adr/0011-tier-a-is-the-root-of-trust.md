# ADR-0011 — Tier A resolves from the environment and nothing else

- **Status:** accepted
- **Authority:** `internal/config`
- **Enforced-by:** `internal/config.TestTierAIsNeverResolvedFromTheSecretStore`
- **Cost-when-tried:** not tried here, and the gate was written before it could be. The near miss is in the tree's own history: `internal/whsec` exists because one rule about a secret's shape was written twice and a `${VAR}` holding a 16-byte key was refused as a literal and silently accepted as a reference — the same class of asymmetry, in the same value, one layer up.
- **Tag-status:** unreleased

## The decision

The two configuration tiers resolve their `${VAR}` references from different
sources, and the split is a trust boundary rather than a convenience.

**Tier A** — the operator's on-disk `bootstrap.yaml`: the node id, the broker,
the store paths, the retention estate, and the address and credentials of the
sealed secret store. It resolves with `config.EnvOnly()`, which reads the
process environment and nothing else. Every function that produces a Tier A
document takes the resolver as an argument, and passing the store-backed one is
the violation this record forbids.

**Tier B** — the founder's company document, versioned in the store and edited
live. Its secrets are `${VAR}` POINTERS, stored verbatim, resolved with
`config.WithStore(...)` only at the moment a provider or transport is
constructed. So a revision exported, diffed, or rendered in the dashboard
carries no credential, and the resolution happens at the one place that needs
the value.

The asymmetry is the whole of it: Tier A holds the key to the store, so it can
never read a value out of the store.

## Why the obvious alternative is wrong

The obvious alternative is one resolver for both tiers, and it is obvious for a
good reason — an operator who has just put `ANTHROPIC_API_KEY` in the sealed
store reasonably expects `${ANTHROPIC_API_KEY}` to work everywhere, and making
it work is one argument at one call site. It compiles, and it appears to work.

What it actually creates is a resolution order nobody declared. The store is
behind `store.path` and `secrets.*`, which are Tier A fields, so a Tier A
document resolving from the store asks the store for the address of itself.
That is not a loop — the resolver simply answers from whichever source has the
name, and the environment is still in the chain. So it *works*, until the day
one value exists in both places, and then the node boots with a different
identity, a different broker or a different database than the file says. There
is no error, because nothing is malformed.

The second cost is smaller and certain rather than conditional: it puts a
secret-store read on the boot path of every process that parses Tier A,
including `crewlet validate` and `crewlet config`, which are the two commands an
operator runs when the store is what is broken.

## What this does not decide

It does not say where a Tier B secret is resolved — only that it is resolved
late, at construction, and not at load. It does not say which store backs
`config.Source`; `internal/fleetsecrets` owns that, and this record holds only
for the direction, which is that Tier A cannot see it.

It also does not forbid an ENVIRONMENT variable from being populated by
something else before the process starts. A container runtime, a secrets
operator or an `envfile` may put whatever it likes in the environment; that
happens outside the engine and above this boundary, which is exactly why the
boundary is drawn at the resolver rather than at the value.
