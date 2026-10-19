# ADR-0043: A dashboard's live data is read as its viewer

- **Status:** accepted
- **Authority:** `internal/api/datasetsapi`
- **Enforced-by:** `internal/api.TestTwoViewersOfOneBoardEachSeeOnlyTheirOwnRows`, `internal/api.TestTheBoardAndItsOutlineCarryNoRow`, `internal/api/datasetsapi.TestABindingToAPerRecordQuestionIsRefused`, `internal/api/datasetsapi.TestThePrincipalKeyChangesWithEveryAuthorityFact`, `internal/api/datasetsapi.TestEveryBindingAnswersExactlyItsSourcesColumns`
- **Cost-when-tried:** a dashboard's figures over the tracker, spend and the agents would be decided by whoever answered the read: read as the board's author, sharing a board would hand every viewer the author's rows; read by a service identity, it would hand them everybody's. Either way the share of an artifact would become a grant over the data behind it.
- **Tag-status:** unreleased

## The decision

**No row leaves the engine in a dashboard that its viewer could not read
directly.** A `dashboard` artifact is one spec for every reader, and every
figure on it is each reader's own:

- An **engine** dataset (a named read from the closed catalogue in
  `internal/artifacts/dashspec`) is answered by the **same question** the
  dashboard's own screens ask, through the question registry, **on the
  viewer's own request**: the principal the guard resolved for the request is
  the principal the question decides for, so the posture, the grants and every
  per-record rule are the registry's, unchanged. A personal scope (`mine`)
  names the viewer's own record. The author is never consulted.
- The **agents** source is `GET /agents` as the viewer would be answered it:
  one function decides the route and the source, by the socket's audience
  table.
- A **store** dataset is read under the collection's read rule for the reader
  the artifact's admission names, as every datastore read is.
- A **file** dataset is the version's own, which the admission decided.
- A **calc** dataset runs over inputs read by the rules above as the same
  viewer; an input the viewer may not read refuses the calculation, never a
  partial answer.
- A source the viewer may not read is a **refusal of that dataset**, naming
  the rule and the grants that would admit them, while the rest of the board
  draws.
- The **board** (`GET .../dashboard`) and its **outline** carry no row, so they
  are cached by version and served to every reader alike. A gallery cover
  shows labels and dataset titles, never a figure (no screenshot service, which
  would freeze one viewer's data into an image others see).

## What makes it hold

A binding is a **projection** of a question's answer, the very value the
registry hands the REST route, encoded as that route encodes it, so a column
can only hold a field the viewer could have read from that route. A binding
may name only a question decided on the principal's grants alone (everybody's,
or one app's): the surface refuses at wiring a question decided per record,
because an engine answer is cached for thirty seconds under a key derived from
everything about the principal such a question can read (identity, kind, seat,
record owner, effective grants, position, stage) and the source's arguments,
and a relation outside that key would let one principal's answer reach
another. Live reads are charged to the shared cost limiter per viewer and per
artifact on each node.

## Why the obvious alternatives are wrong

- **Read as the author, like a report.** A share is a relation to the
  artifact; it would become a grant over the tracker, spend and the agents for
  whoever the author shares with, which ADR-0034 forbids.
- **Read with a service identity and filter in the dashboard.** Every row
  would leave the engine, and the filter would be a second, weaker copy of the
  rules the questions already apply.
- **Per-viewer connector calls, as Claude's dashboards do.** Crewlet's
  integrations are company-held credentials, not the viewer's, so a call would
  read with the integration's authority. Integration data reaches a board
  through a store collection an agent writes, under the store's own rules.
- **A query language over the domains.** It would let a spec ask what no
  screen asks, and validate widgets against columns discovered at run time
  rather than promised at publish.
