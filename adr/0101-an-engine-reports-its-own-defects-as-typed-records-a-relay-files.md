# ADR-0101: An engine reports its own defects as typed records a relay files

- **Status:** accepted
- **Authority:** `internal/issuereport`
- **Enforced-by:** `internal/issuereport.TestEveryWiredSignalBuildsAReportTheExportAccepts`, `internal/issuereport.TestAPublishedClassShapeIsImmutable`, `internal/issuereport.TestEveryCatalogueSiteMapsToAFunctionInTheSource`, `internal/issuereport.TestAnIdentityIsSmallAndClosed`, `internal/issuereport.TestAReportHoldsNoPlainString`, `internal/issuereport.TestNoAdversarialValueReachesAReport`, `internal/issuereport.TestAFingerprintIsSentOncePerWindowAcrossTheFleet`, `internal/issuereport.TestTurningItOffStopsEverything`, `internal/issuereport.TestAnInertBuildSendsNothingWhateverTheSetting`, `internal/issuereport.TestAnExposedPanicSendsItsSiteAlone`, `internal/panics.TestTheSiteIsTheFunctionThatPanickedThroughARaise`, `internal/api.TestEveryUnauthenticatedRouteIsServedByAnExposedPackage`, `internal/api.TestTheAPIReportsAPanicByWhetherItsRouteIsGuarded`, `internal/issuereport.TestTheExportCarriesWhatTheRelayEnforces`, `internal/api/configapi.TestOnlyAnAdministratorMayTurnIssueReportsOn`, `cmd/crewlet.TestTheCheckedInIssueReportCatalogueIsWhatTheGeneratorEmits`
- **Tag-status:** unreleased

## The decision

An engine whose company opted in (`diagnostics.issue_reports.enabled`, off by
default, an administrator's to change) reports the defects it detects in itself
to the project's public issue tracker. It posts a typed record to a relay at a
constant address, and the relay files the issue as a GitHub App of its own. The
engine holds no credential, and none may ever ship in the binary: it is open
source and extractable.

A report is an allowlist, and the type is what holds it: `issuereport.Report`
has no plain string. Each is a named type that only a validating constructor
makes: an enum of the catalogue, a version matched by a strict pattern, or a
function of the engine's own public source. No redactor is relied on, because
nothing is left to redact.

One Go table, `issuereport.Catalogue`, names every class: its fixed title and
summary, its component and function, the facts it may carry and its threshold. A
generated export (`schema/issue-report-catalogue.json`) is what the relay
vendors, so both ends refuse the same unknowns, and a fact the class does not mark
optional is required on both. A fingerprint is a digest of the class, the component
and the function: no version and no company's value, so one defect is one issue on
every engine. A class whose function varies reports the package of the function
found as its component, and the file that declares it, so the relay can verify it.
A class may declare identity facts, closed-enum or boolean and never a number, a
version or a pattern, that feed the fingerprint too, when one class is several
defects. Each class has at most 16 combinations of them.

A panic is reported wherever the engine contains one: every such `recover()` hands
its value to `panics.Recovered`, the one reader, which names the function under the
original panic. Its report carries up to five of the engine's own frames under it,
each a function, its file as the package and base name and a line, so a stack is
no longer only the node's: a frame names public source and nothing of the input,
and it never feeds the fingerprint. A panic in a package that parses input from
outside the company's principals before authenticating it
(`issuereport.BeforeAuthentication`), or one served on a route the guard exempts,
is reported with its site alone, since frames there would show anybody how to
reach the defect.

A class's shape is immutable after a reporting tag, which is any `v*` tag: its
id, component, site, facts, identity and harness flag, at the report schema that
tag published. A later change is a new class id or a schema bump. A tag freezes the
export it publishes in `schema/issue-report-history/`, and the relay keeps every
frozen catalogue and validates a report against the one its `schema` names, so an
older release's reports stay valid. A site is a stable token, which a test maps to
today's function, so renaming a function does not orphan its issue.

A schema change after the first reporting tag keeps the classes it changes off
until an administrator accepts it again, in Settings, Diagnostics, where the switch
is and with one action, so that an administrator's consent always covers what is
sent. No tag has shipped yet, so this binds from the first reporting tag: the first
consent covers the format as that tag publishes it.

Only a released or clean module build sends: `issuereport.Origin` holds the
binary to a release tag or a module pseudo-version, an unmodified tree and a
recorded commit, and any other binary stays inert whatever the setting says. The
exported pattern refuses the same versions, so the relay does too.

The record that a report was sent answers "who has to agree" with the whole
company, now: one compare-and-set record per fingerprint in the coordination
store (`coord.IssueReports`). Of any number of nodes that detect one defect, one
sends, at most once in seven days. A store that cannot be read sends nothing.

## Why the obvious alternative is wrong

The obvious alternative is an error message with a redactor over it: the message
says what went wrong, and a scrubber takes out what looks private. A scrubber
fails open. It knows the shapes of the secrets it was told about and none of the
words a company writes, and the engine's own errors quote seat names, item keys,
paths, hosts and what a model said. Once one such word is in a public issue it
cannot be taken back.

The second alternative is a token in the binary, so the engine files issues
itself. It is one `strings` away from anybody, and it would identify the company
to GitHub on every report.

Each node's own record of what it sent would send once per node, and the relay's
search would have to absorb what the promise says never leaves.

Reporting from every build would file a development build's defect, which is
often its developer's own unmerged change, as a public issue about code nobody
else has.

## What this does not decide

It does not collect anything about usage: a report is a defect, and there is no
telemetry here to widen. An exploitable defect never enters the catalogue, and
goes through `SECURITY.md`. It does not decide how a defect is fixed: detecting a
loop leaves the loop running. It does not decide how the relay filters, caps or
deduplicates, which are the relay's, beyond that it recomputes the fingerprint
and refuses what the catalogue does not name.
