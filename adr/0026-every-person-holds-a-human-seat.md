# ADR-0026 — Every person holds a human seat

- **Status:** accepted
- **Authority:** `internal/iamdomain`
- **Enforced-by:** `internal/iamdomain.TestAPersonEnrolsOnlyOntoAHumanSeat`, `internal/iamdomain.TestAPersonsSeatIsChangedButNeverCleared`, `internal/iamdomain.TestAnOpenInvitationHoldsItsSeat`, `internal/iamdomain.TestACreatedPersonGetsAFirstPasswordLink`, `internal/api/iamapi.TestAPersonIsNeverCreatedOrInvitedWithoutASeat`, `internal/api/configapi.TestAnInvitedSeatCannotBeTakenOutOfTheCompany`
- **Cost-when-tried:** the seat was optional, and the documented way in produced exactly the person it should never produce. The first-person command in the identity guide and the `iam_unclaimed` boot hint both read `crewlet iam invite <address> -grants …` with no `-seat`, and Settings › People & access offered **No seat** in its invite dialog, so a founder could join with no inbox, no day, no lead and no contact route. Separately, an administrator's create and a bind checked only that the chart held the handle, not that the seat was human, so a person could be bound to an agent's seat and then be refused `403 seat_unavailable` on every request they made.
- **Tag-status:** unreleased

## The decision

A **person** holds exactly one **human** seat from the record that creates them
until the record that removes them. Both ways in name it. An administrator's
create names the seat and hands back the person's first password link. An
invitation names the seat, and redeeming it binds the person there. A person's
seat can be **moved** to another vacant human seat but never **cleared**. A
suspended or retired person keeps theirs. A seat is freed by moving its holder
or by removing them.

An **open invitation holds its seat** the way it holds its address. Until it
is redeemed, cancelled or lapses, nobody else can be created on that seat,
bound to it or invited to it, and a company write cannot remove it.

**Every binding names a human seat** of the company the node runs. That holds
for a service account too: a machine row bound to a seat, a Tier A token's
included, acts as that seat on every request exactly as a person does, so an
agent's seat would refuse it the same way.

The writer's decides enforce this. The applier never refuses a record. If a
build before this rule recorded a person with no seat, the applier writes that
row as recorded, and `GET /iam/check` reports it as `person_without_seat`.

Because a person always comes from a seat, the place they join from is the
vacant human seat. On the dashboard that is the seat's card, its peek and its
page in Agents › Org chart, each offering **Invite** and **Create person** to
`people:manage`. With the CLI and the API, the seat is a required argument.

## Why the obvious alternative is wrong

The obvious alternative is to treat the seat as an attribute a person may or
may not have. You would invite somebody first and bind them later if they need
one. Meanwhile they act under their login with kind `operator`, the way a
token does, and
[ADR-0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)
already admits that for a credential.

For a person, that alternative has nothing for the org chart to decide. Every
rule the chart answers is asked of a seat:

- who leads somebody, and so who may read or reorder their queue;
- whose inbox a notice lands in;
- which contact identities an inbound message is attributed through;
- who an agent escalates to.

A seatless person is led by nobody, sits in no unit and is reachable by no
route. So every one of those rules needed a second answer for the case where
there was no seat, and each rule's second answer was different. Making the seat
required removes that case instead of answering it many times.

**Unbinding a person to free their seat** is the same alternative under
another name. An unbind is the gesture that produces a seatless person.
Freeing a seat is therefore a move or a removal. The removal's tombstone keeps
who held the seat, and the trail stays under the seat's handle, so a
successor's arrival loses no audit.

**An invitation that did not hold its seat** would let a create, a bind or a
second invitation take the seat while the first link was still in somebody's
mailbox. The invitee would only find out at the last step, after choosing a
password, that the seat they were invited to belonged to somebody else.

## What this does not decide

It does not make a seat required of **service accounts or Tier A tokens**.
Theirs stays optional, and a human seat when it is given, and
[ADR-0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)'s
"nobody is refused for being unbound" still holds for them. The unbound
principals are therefore a token, a service account, or a person recorded
before this rule.

It does not decide **which seats exist**. That is the company document's. A
company that declares no human seat can admit nobody, and its first person
needs one declared before they can be invited or created.

It does not make the two ends of a binding agree atomically. The directory
holds the binding, and the company document holds the seat. A `/config` write
that removes a held or invited seat is refused `409 seat_held`, but that check
is advisory, so a binding left dangling is still a legal state that
`GET /iam/check` names.

It does not decide what a stage withdraws. A suspended person keeps their
seat, and what the suspension ends is their access and their contact routing.

