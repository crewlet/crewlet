# ADR-0072: A domain's fault takes only its own copy out of service

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/engine.TestEveryDomainsCopyIsJudged`, `internal/engine.TestAWrongUsageCopyAnswersNoSpendRead`, `internal/engine.TestAWrongCopyOfOneDomainLeavesTheOthersInService`, `internal/engine.TestAFaultInADomainThatGatesNoSeatShedsNone`, `internal/engine.TestADutyDeclinesWhileACopyItDecidesFromIsOutOfService`, `internal/engine.TestADeclinedDutysLeaseIsGivenBack`, `internal/estate.TestAFaultInOneDomainRoutesOnlyThatDomainsOperations`, `internal/estate.TestAPagesBacklinksRunOnlyWhereBothCopiesServe`, `internal/search.TestAnOutOfServiceSliceIsCountedMissing`, `internal/search.TestACoordinatorGivenNoRangeStandsInForWhatItsPeersLeftUncovered`, `internal/statelog/statelogtest.TestTheSuiteCatchesADomainThatMisdeclaresItself`
- **Cost-when-tried:** one declaration (whether a domain's health was a readiness input) answered two questions: does a seat depend on this domain, and is its copy judged at all. The compacted domains answered no, and so escaped the judgement: a vectors or usage copy whose applier stopped, whose rows fell below the log or that held an undecodable record past the grace was served for ever, and nothing said so. The two that answered yes took the WHOLE copy out of service for one log's fault — a knowledge-base applier halted on one page sent every tracker call the node's seats made to a peer, and on a single node refused them all — and a knowledge base that merely lagged its log made every tracker read a last resort. The native code host's design had to choose between gating every seat claim on a busy git log's lag and never judging that log at all.
- **Tag-status:** unreleased

## The decision

There are two questions about a domain's copy on a node, and they get two
answers. WHETHER A SEAT DEPENDS ON IT is declared by the domain
(`statelog.Domain.GatesSeatAdmission`): admission waits for exactly those
domains to be level, and the copy a seat is served by — the SEAT COPY,
`estate.SeatCopy` — is every such domain in service at once. WHETHER THE COPY
IS WRONG is never declared: the engine judges every registered domain's copy
on every reading, by the one predicate that judges any
(`statelog.Health.Healthy`), and holds one verdict per domain.

A domain's copy that is wrong goes out of service for that domain's operations
alone. Every estate operation names the domain whose copy it reads, and the
router and the serving node ask that domain's verdict before running it here
(`estate.LocalBackends.For`): a node whose tracker copy is wrong sends its
seats' tracker calls to the other data nodes, tells a peer asking for one
`out_of_service`, and goes on answering every other domain's operations from
its own copy. Whether a copy answers requests now — its distance from its log —
is per domain the same way. No fault sheds a seat by itself (ADR-0025's
amendment stands): a fault in a domain that gates seats takes the seat copy out
with it, so admission is answered by a peer and the node's seats need its view
of the fleet to be routed; a fault in any other domain never does either.

A search fan-out participant asks the same verdict: a range over a copy that
is out of service is counted missing, and a vectors copy out of service skips
the semantic half. A node that answers either — the coordinator itself
included — is given no range for that kind of search for a while, since every
node holds the whole corpus: a copy out of service costs one search a slice,
never every search. A coordinator given no range still scans whatever range
its peers then leave uncovered, so a broker that fails never empties a search
it could answer.

A fleet-singleton duty that decides from a domain's rows asks the same verdict
before it claims, and a node whose copy of any domain the duty decides from is
out of service declines it, so a node whose copies serve takes it: the trim
(the evictions the identity-claiming logs' rows hold, and the anchors the logs
that write them hold — ADR-0073), the embedding duty (the
vectors, and the tracker's and the knowledge base's rows it embeds and
withdraws for) and the object collector (the rows that name objects).
Declining is not claiming — the conservative direction for each: nothing
withdrawn, purged or deleted — and a node that declines a duty it holds gives
its lease back, so a node whose copies serve takes it at its own next tick
rather than once the lease lapses: the trim's lease is 45 minutes, and the
fault that made its holder decline does not heal on its own.

## Why the obvious alternative is wrong

The obvious alternative is the one this replaces: one boolean per domain and one
verdict per node. It costs what the field above records — a wrong copy of an
undeclared domain served for ever, and one log's fault charged to every other
log the node holds. It also contradicts a rule the estate already applies to
floors: an operation answers from its own domain's rows, which is why a
request's floor is on its own domain's log and never on another's. A verdict for
the whole copy is the one place that rule was broken. The reads that cross into
a second domain's rows ask its verdict too, and wait on no floor of it, since
each reads an index no log position describes: an operation that reads another
domain's rows declares it and runs only where every copy it reads serves (a
page's backlinks, which name the tasks linking to it), and a search's semantic
half, over the vectors, is best effort and degrades rather than routes.

The second obvious alternative is to keep a per-domain declaration and ask a
second question of it: does this domain want its copy judged? That is the same
mistake under a new name. No domain has a reason to want a wrong copy of itself
served, and a domain allowed to opt out of the judgement is a domain whose wrong
copy is served for ever — which is exactly what the vectors and usage domains
did, by answering a question about seats.

## What this does not decide

WHAT makes a copy wrong: `statelog.Health.Healthy` and the engine's judgement
own that list, and a lag of any size is still not on it. How a seat's tools
behave when no data node serves a domain: ADR-0025. Which node a declined duty
lands on: whichever node whose copies serve claims the given-back lease first.
