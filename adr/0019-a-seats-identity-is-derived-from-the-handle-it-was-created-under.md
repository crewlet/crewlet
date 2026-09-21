# ADR-0019 — A seat's identity is derived from the handle it was created under

- **Status:** accepted
- **Authority:** `internal/org`
- **Enforced-by:** `internal/org.TestARenameKeepsTheIDTheLeaseTheMailboxAndTheDiary`
- **Supersedes:** ADR-0013
- **Cost-when-tried:** renaming a seat under ADR-0013 orphaned its mailbox, its seat lease, its diary and episodes, its onboarding markers and its schedule ledger in one gesture, and left the old ones addressed by a handle nothing named.
- **Tag-status:** unreleased

## The decision

An agent seat's runtime id stays a UUIDv5 computed by every node from
configuration alone. What changes is the second input: it is the handle the
seat was **created** under — `org.Role.Origin`, carried on the seat's chart row
as `origin_handle` and frozen there by the first rename — rather than the
handle the seat answers to now.

So a rename moves the seat's ADDRESS and nothing else. Its mailbox subject and
consumer group, its seat lease, its memory changelog subjects, its learning
rows and its schedule ledger all key on the id, and the id does not move. The
addresses a rename retires go on resolving through the row's alias list, which
is a separate field with a separate job: an alias keeps a REFERENCE somebody
wrote down working and is capped, while the origin is the seat's identity and
can never be dropped.

A second family of consumers keys on the ORIGIN HANDLE ITSELF rather than on
the id derived from it, and the difference is forced rather than chosen: an
agent's account at Mattermost, GitLab, Datadog and Atlassian is named after
its origin, because none of those apps stores a Crewlet id, none of them keeps
a field of this engine's own, and a uuid in a bot username is a name no person
can read. What that buys is the same thing the id buys — a durable address
that a rename does not move — and the failure it avoids is worse than an
orphan, because the account is live in somebody else's system: a second bot
created beside the first, the first still holding a sealed token, and
`-decommission` deleting the one the agent is working as. `internal/provision`
is the one place that can refuse a seat with no origin, and does.

## Why the obvious alternative is wrong

ADR-0013's alternative was a minted uuid in a row, and it rejected it because
the lookup had to succeed on a node that had never run the seat — the node's
own database holds only the seats it runs, and there was no other estate to
ask. That argument has been answered by the chart domain: the org tree is now
derived from replicated rows every node holds, and a node that cannot see a
seat's row cannot route to that seat at all. Reading one more field off a row
you are already holding costs nothing, and adds no wait that resolving the
seat had not already paid.

The alternative this record rejects is the one ADR-0013 accepted: keep
deriving from the live handle and treat a rename as a new seat. It is cheap
and it is wrong, because a handle is prose — a founder writes `sarah-chen`,
the person marries, and the honest edit silently retires a colleague and hires
a stranger wearing their name. Nothing reports it. The seat comes up with an
empty diary, a mailbox nobody publishes to and a lease nobody holds, while its
predecessor's rows sit under an address no document now contains.

The other alternative is to anchor on the alias list's oldest entry rather than
on a field of its own. That fails on the cap: the list is bounded at
`chart.MaxFormerKeys`, so a seat renamed once too often would silently change
identity — an identity a cap can drop is not one.

## What this does not decide

It does not decide what a handle is, which names yield one, or that a retired
handle goes on resolving — those are `internal/org`'s and `internal/chart`'s
own rules.

It does not extend to a unit, whose durable state is keyed on `org.Unit.Origin`
directly rather than on a derived uuid: a unit has no mailbox, no lease and no
memory, so there is nothing for a hash to buy it.

It does not change what happens when the COMPANY is renamed. The company name
is still an input to the derivation, so it still moves every seat's id — which
is the same trade ADR-0013 made and the reason the derivation stays namespaced.
