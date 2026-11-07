# ADR-0102: An integration puts its agent's own credential in that agent's box, and its own wins

- **Status:** accepted
- **Authority:** `internal/seataccess`
- **Enforced-by:** `internal/seataccess.TestAnOnSeatsBoxHoldsEveryCredentialItsToolReads`, `internal/seataccess.TestAnOnSeatsBoxIsItsToolWithNoLine`, `internal/seataccess.TestAVendorGainsBoxDefaultsOnlyByAReviewedChange`, `internal/expo.TestAnOnExpoAgentsBoxHoldsItsLiveTokenWithNoLine`, `internal/engine.TestAGitHubAgentsBoxHoldsItsAppTokenAndItsFileWithNoLine`, `internal/engine.TestALineAnIntegrationsCredentialReplacesIsSaid`, `internal/engine.TestAToolTokenUnderASignInNameIsWithheldAndSaid`, `internal/engine.TestAVendorsBoxFileIsWrittenAtLaunchAndKeptByThePoll`
- **Cost-when-tried:** an agent on at Expo, its robot's token pasted and sealed, had no `EXPO_TOKEN` in its box until a person wrote `EXPO_TOKEN: "${EXPO_TOKEN_<HANDLE>}"` into its `sandbox.env`, a line whose only correct value the engine already knew. The agent concluded it could not use Expo. GitHub, GitLab, DigitalOcean and Scaleway each asked for the same hand-written lines.
- **Tag-status:** unreleased

## The decision

**An integration that gives an agent a credential puts it in that agent's
box itself, for as long as the agent is on at it.** A lane whose tool is a CLI
in the box declares the variables it reads (`seataccess.LaneSpec.BoxEnv`), and
a vendor whose box needs more declares it as a `seataccess.BoxDefaulter`: a
credential a CLI beside the tool reads (GitHub's and GitLab's token for git), a
setting, or a file. The launch resolves each credential through the seat's own
resolution, scrubs it from the run's output, never counts it as the harness's
sign-in, and withholds it under a name a member of the seat's executor chain
signs in by. A sealed credential is put there where the engine provisions the
integration and so seals it; an issued one (GitHub's app token) wherever the
engine issues it.

**A credential is the integration's, a setting the person's.** A line of the
document under a credential's name, in the seat's `sandbox.env` or any setup
step's `env`, and a setup file at a credential file's path, are ignored for an
agent on at the integration, and said as an advisory at the line. A setting
(a default project, a CLI profile of literal text) stays beneath every layer a
person writes.

## Why the obvious alternative is wrong

The obvious shape is the one the box contract started with: the engine names
no credential of its own, and a person declares each one in `sandbox.env`. For
an operator's own secret that is right, since only the operator knows which
variable holds it. For a credential the engine seals under a name it derives
for the seat, the line has exactly one correct value, which the engine knows
and the person has to copy, and a seat without it holds an account at the
vendor and nothing to use it with.

The second obvious shape lets a person's line win, as it wins for a setting.
It would let the line hand the box a broader credential than the agent's own
(Expo's provisioner token holds Admin on the account, beyond every level an
agent may hold) or one that outlives turning the agent off, while the Agents
tab reports the level the agent's own credential holds. The integration's
state is the one authority on which identity the box acts as.

## What this does not decide

How each credential is created, sealed, rotated or issued, which is each
vendor's and [ADR-0058](0058-a-seats-github-app-token-is-issued-at-use-and-read-only-by-that-seat.md)'s.
Whether a CLI is installed in the box, which is the box image's or a setup
step's. And the credentials of an integration whose agents' accounts are made
outside Crewlet, which the seat's own settings still name.
