# ADR-0069: A model's sign-in state belongs to its sign-in

- **Status:** accepted
- **Authority:** `internal/providers/llm/cliagent`
- **Enforced-by:** `internal/providers/llm/cliagent.TestASitOutFollowsTheSignInAcrossAnApply`, `internal/providers/llm/cliagent.TestANewCredentialUnderTheSameNameStartsClean`, `internal/providers/llm/cliagent.TestEntriesOnOneSignInSitOutTogether`, `internal/providers/llm/cliagent.TestAModelScopedLimitHoldsOnlyThatModel`, `internal/providers/llm/cliagent.TestARefreshWriteBackKeepsTheSignIn`, `internal/providers/llm/cliagent.TestASignInIDIsComparableOnlyUnderOneKey`, `internal/providers/llm/cliagent.TestASweepKeepsAWindowInForce`, `internal/providers/llm/cliagent.TestAClearEndsOnlyWhatWasReportedBeforeIt`, `internal/providers/llm/cliagent.TestARefusedSignInIsTheSignInsLastFailure`, `internal/providers/llm/cliagent.TestALimitIsDatedAndNamedByTheCLIsEvent`, `internal/providers/llm/cliagent.TestARefusedSignInIsReadFromTheCLIsFields`, `internal/providers/llm/cliagent.TestEveryChildIsHandedTheEnginesZone`, `internal/engine.TestAnApplyKeepsASpentPlanSpent`, `internal/engine.TestARunningNodesApplyKeepsTheSignInsSitOut`, `internal/providers/llm/cliagent.TestAnEnvSignInIsOneSignInPerPlaceItReaches`, `internal/secrets.TestADerivedKeyIsTheFleetsOnePerPurpose`, `internal/engine.TestEachEntrysRowIsItsSignInsStateOnThisNode`, `internal/engine.TestARetryClearsEveryNodesSitOut`, `internal/engine.TestARunRefusedAfterARotationIsRelaunchedOnce`, `internal/engine.TestAModelsConditionIsTheFleetsHeartbeats`, `internal/statelog.TestModelAlarmsFireAtTheOwnersThresholds`
- **Measured:** on Claude Code 2.1.312 (2026-10-28), every `-p --output-format stream-json` run writes a `rate_limit_event` whose `rate_limit_info` carries `status` (`allowed`, `allowed_warning`, `rejected`), `resetsAt` in epoch seconds and `rateLimitType` (`five_hour`, `seven_day`); a refused key or token ends in a result event with `api_error_status: 401`, and a refused token is first retried in `system` `api_retry` events naming `error: authentication_failed`. On Codex 0.150.1, `exec --json` writes no rate-limit event (its `rate_limits` are in the session rollout only, which a plain call's `--ephemeral` never writes), and its limit sentence names the reset as a local time with no zone (`%-I:%M %p`, or `%b %-d<st|nd|rd|th>, %Y %-I:%M %p` on another day).
- **Cost-when-tried:** the sit-out was a field of the provider, and every apply rebuilds every provider, so the first fix copied it to a rebuilt entry whose config spec was `reflect.DeepEqual` to its predecessor's (`carrySitOuts`). A spec holds `${VAR}` names, not values, so it read the wrong thing both ways: a new token stored under the same name stayed benched for up to an hour (`maxLimitBench`) on a credential nobody had refused, while an edit to the entry's timeout dropped a sit-out the vendor still held and the node's seats each spent a process relearning it. Entries on one token learned one plan-wide limit separately, one launch each. Separately, a claude-code entry signed in through its `env` (Bedrock, Vertex, Foundry, a gateway's `ANTHROPIC_AUTH_TOKEN`) read as having no sign-in, which held the assistant off it, and under the default mode it was still handed the plan token, into its box too, where the cloud switch outranks it and it was never used.
- **Tag-status:** unreleased

## The decision

**What the vendor said about an account is kept per account, not per entry.**
Each coding-CLI entry computes a sign-in identity, `SignInID`: an HMAC of its
CLI, its auth mode and every credential its CLI is handed (read off the very
environment a call is given) with where they reach (the profile's
`identity_env`: a cloud region, a project, a gateway's address), or, for a login kept in files, of that login's
install record, which moves only when a login is installed and never when the
CLI refreshes its token. A node keeps one registry, `cliagent.Plans`, created
at boot and handed to every revision's build; an entry reads and writes the
`Plan` of its sign-in. A rebuilt entry on the same account finds the window its
predecessor was sitting out; a new credential is a new id and a clean plan.
`carrySitOuts` is gone.

**Entries on one sign-in pause together on a plan-wide limit; a model's own
limit holds that model only.** A report whose window is `model` ("Opus limit",
`seven_day_opus`) is kept under the model argument the CLI was given, and an
entry on another model on the same plan still answers. The readiness verdict
and the last answer and refusal are the sign-in's too.

**The id is keyed by the fleet, not the process.** The key is derived from the
keyring's active key with HKDF (`secrets.Keyring.Derive`, info
`crewlet/llm-sign-in-id/v1`), so every node and every restart computes one id
for one credential, which lets a run's row name the sign-in it launched with,
and nobody without the keyring can test a guessed credential against an id. A
keyring rotation changes every id, so every sit-out starts clean once: bounded,
and accepted.

**A limit is named and dated from the CLI's fields first.** A refused sign-in
reported in fields (`auth_events`) is read before anything else, in a run the
CLI reported failed; a limit, once recognised, takes its reset and window from
the last matching `limit_reset_events` entry, then from the sentence's own
reset and wording (`limit_windows`), then `unknown`. A time with no zone is
read in the zone the engine handed the CLI: every child and every box is
handed an explicit `TZ` (the engine's own, else UTC).

**`auth.mode: env` signs a CLI in with the entry's env alone.** Neither the
token nor the key variable reaches the CLI, no login is seeded, and the profile
names which env variables sign it in (`sign_in_env`).

**What a node knows of its models rides its heartbeat.** Each node publishes
one row per entry on its presence lease (`coord.NodeStatus.Models`): the first
twelve hex characters of the sign-in id (never the whole), the route it signs
in by, the window it sits out, its last answer and last refusal, the seats it
holds parked on a chain naming the entry, and what the CLI says of a login kept
on its disk. A reader joins every node's rows, as `mcp_servers_status` does; a
node that did not report is a column of its own, never a row of zeros.

**Retry and clear are a record every node applies by its stamp.** A person's
"retry now" or "clear" is one `llm_retry` record per entry in the coordination
store; every node watches the class and clears the entry's sign-in of the
windows reported before the stamp, then fires every provider-park probe whose
chain names it. A report after the stamp stands, so a late, repeated or
post-restart delivery clears exactly what an on-time one would. "Retry now"
writes the record only after one probe call answered.

**A run refused on a replaced sign-in is carried on once.** A launch stamps the
sign-in id its CLI was handed on the run's row (`LaunchRecord.SignInID`). A run
that ends refused on an id that is no longer its entry's is relaunched for the
same round, in its box, continuing its session, with the new credential every
launch takes; the relaunch is marked (`FollowUpSignInReplaced`), and a marked
run refused again is an ordinary failure.

**The model alarms are the alarm table's, at the owner's fixed thresholds.**
A credential's computed expiry at thirty days and seven, a refused sign-in, and
a limit that turns into `llm_limit_waiting` once a seat has waited fifteen
minutes, each per entry, evaluated by every data node from the same fleet-wide
inputs.

## Rejected

- **A per-process fingerprint.** Cheaper, and meaningless the moment a run's
  row is compared on another node or after a restart.
- **A plain hash of the credential.** Comparable, and a dictionary test away
  from the credential for anybody who reads a heartbeat.
- **Keying the sit-out by the entry's spec**, which is what was tried; see
  Cost-when-tried.
- **Reading the reset from Codex's rollout file.** It is written only by a run
  with tools, never by a plain call, and its sentence carries the reset
  already; the engine pins the zone instead.
- **Claude Code's `apiKeyHelper` to reach a running process with a rotated
  key.** It carries no OAuth token, and the engine runs Claude with
  `--setting-sources ""`: one mode of one CLI, where the relaunch covers every
  mode of every runner.
- **Asking each node for its models' state (a fan-out).** Every answer would
  be partial on a slow node, and every node re-sends its status on its
  heartbeat anyway.
