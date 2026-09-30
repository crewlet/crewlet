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
consumer group, its seat lease, its memory changelog subjects, its diary and
onboarding markers and its schedule ledger all key on the id, and the id does
not move. The
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

The rest of a seat's memory is in that second family too: its episodes, its
synthesized skills and their versions, its counterparty profiles (and a
colleague's, as their subject) and its conversation ledger name the seat by
the origin handle, in columns that have always held a handle. Keying them on
the id instead would have meant re-keying every row already written, which no
statement can do — the id is a hash the database cannot compute — while the
origin IS the handle every seat never renamed already answers to, so every
existing row was already keyed correctly. What that asks of the code is that
every writer and reader pass the origin and never the live handle, and that a
surface showing a row resolve it back to the seat's current address;
`internal/learning`'s package doc states the rule and
`internal/engine.TestARenamedSeatKeepsItsEpisodesSkillsProfilesAndLedger` holds
it end to end. The completion ledger is in that family for the same reason
with a sharper cost: a trigger worked under the old handle is redelivered
after a rename under the new one, and keyed on the address it found nothing
and ran the turn again. So are its chat thread follows: keyed on the address,
a rename left the seat deaf to every thread it had been following until
somebody named it again — and its Confluence page subscriptions, which it
lost the same way for every page it had touched. And so is every person
column of the work tracker —
an item's assignee, reporter, collaborators and watchers, a checklist line's
owner, a comment's author and whom it asks, whose inbox, priority list and
pins a person's record is, a view's owner, a project's default assignee:
keyed on the address, a renamed seat opened an empty queue, inbox and pin
strip, and its own comments were no longer its own to edit. There the origin
rides the RECORD, resolved once by the writer, because an applier may not
read a chart; `internal/tracker`'s people.go states the rule (a
`person:"seat"` tag on every such field, which its tests hold every name to)
and `TestARenamedSeatKeepsItsWorkInboxQueueAndPins` holds it end to end.

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
