# d-704 — Confluence page subscriptions are the engine's, not the vendor's

Status: **decided and implemented.**
Implementation: `internal/confluence/parse.go`,
`internal/engine/pagewatchers.go` · Docs:
`docs/integrations/confluence.md#routing-strategy`

## The decision

A seat is woken by later activity on a Confluence page when it has **touched
that page before** — edited it, or been @mentioned on it. That subscription
list is kept by the engine, on the coordination store, and Confluence's own
watcher API is not read at all.

Subscribers and mentions are **one routing tier**, above the space-lead
fallback. An **edit** subscribes its author; a **comment** does not.

## Why not read Confluence's watchers

Confluence keeps watchers, auto-adds page editors to them, and exposes them
over REST. Reading that list is the obvious design, and the Python engine did
exactly that. Three properties make it the wrong one here, and they compound:

**It is mostly people.** A wiki's watcher list is dominated by humans, whom
Confluence has already notified natively and who resolve to nothing the engine
can wake. Worse than useless: a human found in the list *counts as a
recipient*, which suppresses the space-lead fallback in favour of a
notification the service then skips — so the page routes to nobody.

**It costs a call per event**, on the routing path, which has to stay cheap:
every page change in every watched space would make a round trip before the
engine could decide who cares.

**A per-role token often cannot read it.** Reading another user's watch state
is permission-gated, and per-role tokens are exactly the configuration this
feature is documented for. The answer on those deployments would be "nobody is
watching" — silently, on every event.

The engine's own list has none of those problems, because it holds only the
parties the engine can route to. Membership is asked as ONE question per event
— "which of my seats is subscribed to this page?" — rather than "who watches
this page?", so the cost does not grow with a page's history and no identifier
comes back that the registry then has to resolve.

## Why `coord.Ledger` rather than a bucket of its own

The question is set membership over a scope, tested against a caller-known
candidate list. That is [`coord.Ledger`](../internal/coord/fleet.go) exactly,
down to the failure polarity. A second primitive with the same shape would
mean two retention rules, two backends' worth of conformance cases, and
eventually a disagreement between them.

Retention is therefore the **bucket's age**, not a per-page expiry — the same
rule everything on the coordination store follows. A page nobody has touched
inside that window drops its subscribers, which is the right forgetting: a
seat that edited a page a year ago is not waiting on it.

## An edit subscribes you; a comment does not

This asymmetry looks like a detail and is the entire delegation loop.

The lead-fallback prompt tells a lead to hand a page over by @mentioning the
right teammate in a comment. If commenting subscribed its author, the lead
would subscribe *itself* in the act of delegating, and every later event on
that page would come straight back — the delegation would achieve nothing, and
nothing would report it. So editing a page (a claim on it) subscribes;
commenting on one (often the opposite — handing it over) does not.

It is also what Confluence does for people, which keeps agents and humans
behaving alike on a shared page.

## Failure polarities

- **An unreadable list routes to the space lead**, which is where the event
  went before subscriptions existed. The other direction — treating "cannot
  tell" as "everybody" — would turn a store blip into a company-wide
  interrupt.
- **A subscription that does not land is a warning, not a failed delivery.**
  It costs one seat one missed follow-up; failing the event would cost the
  event.
- **No coordination store means no list**, and mentions plus space leads carry
  the routing. That is a supported shape for a single embedded node, not a
  degradation to work around.

## What the rewrite got wrong, and how

The Go port dropped watcher routing without recording it, and three sources
then disagreed for as long as that lasted: `docs/integrations/confluence.md`
described watcher routing in full, with worked examples; `decisions/703` listed
Confluence as fully served and never mentioned watchers; and
`internal/confluence/prompt.go` told every lead that "Confluence has no
per-agent page watchers the engine can read" — which is false about the vendor
and was the only one of the three a *reader of the code* would find.

The operator cost was real and silent: every page event that was not an
explicit mention fell through to the space lead, including the follow-up to a
page a seat had edited minutes earlier, and including the second event on a
page a lead had just delegated. The prompt told leads to delegate by mention;
the mechanism that made delegation work was gone.
