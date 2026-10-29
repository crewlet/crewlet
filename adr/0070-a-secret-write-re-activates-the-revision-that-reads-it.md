# ADR-0070: A secret write re-activates the revision that reads it

- **Status:** accepted
- **Authority:** `internal/api/secretsapi`
- **Enforced-by:** `internal/api/secretsapi.TestAWriteOfAReadSecretReactivates`, `internal/api/secretsapi.TestABatchIsOneActivation`, `internal/api/secretsapi.TestAReloadFailureStillAnswers200WithTheSealedValue`, `internal/api/secretsapi.TestABatchIsRefusedWholeNamingEveryFault`, `internal/api/configapi.TestReadsAnswersThePathsReadingEachName`, `cmd/crewlet.TestSettingSeveralSecretsIsOneActivation`, `internal/api/secretsapi.TestADeferredBatchSealsAndReactivatesNothing`, `cmd/crewlet.TestAProvisioningRunThroughANodeReactivatesOnce`
- **Cost-when-tried:** `PUT /secrets/{name}` sealed the value and stopped. A `${VAR}` is resolved when a provider or a transport is built, from a snapshot an apply takes, so every running model and integration went on signing in with the old credential until something re-activated the company, and nothing a person used did: the dashboard's secret dialog told the operator "Running seats pick it up after a re-activation or restart" and no screen ever re-activated. A rotation of four credentials one model reads, written one request each, would have moved the fleet through four epochs, three of them running a mix of the old and the new values.
- **Tag-status:** unreleased

## The decision

**Every write through `/secrets` re-activates the active revision when that
revision reads a name it wrote**, synchronously, in the same request:

1. `PUT /secrets/{name}`, `DELETE /secrets/{name}` and the batch
   `PUT /secrets` ask the config surface which paths of the active revision
   read each name (`config.Company.VariablesRead`, the index
   `GET /config/references` answers). When any does, they ask `config.write`
   of the guard on top of their own verb **before anything is sealed**, seal,
   and then call `configapi.Service.Reload` once.
2. **The status stays 200 whatever the reload did.** The value is sealed
   first, so a reload that lost a race, found the company unrunnable or went
   unanswered is said in the answer's `reload.error` with its sentence, beside
   a write that happened. crewlet-operator reads exactly 200 from these routes.
3. The answer says what the write did to the running company: `read_by` (the
   paths) and `reload` (`{revision_id, epoch}`, `{error, detail}`, or `null`
   when nothing reads the name). One shape, `configapi.Reloaded`, for every
   surface that seals and then reloads, the model credential route included.
4. **A multi-secret rotation is one activation.** `PUT /secrets` takes up to
   64 values, refuses the whole body naming every bad name or value, seals
   them all and reloads once. `crewlet secrets set NAME NAME...` and
   crewlet-operator's secret sync write through it.
5. **A writer that seals one value at a time re-activates once, at its
   end.** `"defer_reload": true` seals and re-activates nothing; a provisioning run through a node seals each
   minted credential that way, the moment it exists (a token live at the vendor
   and recorded nowhere cannot be revoked), and its flush re-activates once for
   every value it sealed.

## Why the obvious alternatives are wrong

**The engine's coalescing republisher** exists to bound a provisioning loop
that may never converge. A person's write is bounded by the person and by
step-up, and the page that made it has to show the epoch it produced, which a
background republish cannot answer.

**Leaving the reload to the client** is what this replaces: every client
(the dashboard, the CLI, the operator) had to know to send
`POST /config/reload` after a write, and the one people use most did not.

**Re-reading the store on every call** would make every model and transport
call a coordination read, and would still leave a running provider's
already-built environment unchanged.
