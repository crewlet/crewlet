# ADR-0125: A delegate decides a request only within rules the engine enforces

- **Status:** accepted
- **Authority:** `internal/delegated`
- **Enforced-by:** `internal/delegated.TestAPanelAllowsOnlyWhenEveryDelegateAllows`, `internal/delegated.TestALowConfidenceAlwaysEscalates`, `internal/delegated.TestOneDecisionInNIsSpotChecked`, `internal/config.TestADelegateIsAnAgentThatCannotRequestWhatItDecides`, `internal/config.TestAPanelRunsOnTwoModels`, `internal/config.TestAnEscalationNamesAPerson`, `internal/ssh.TestTheAlwaysEscalateListCatchesEveryClass`, `internal/agent/builtin.TestADelegateNeverDecidesARequestItMadeOrLeads`, `internal/agent/builtin.TestAnAlwaysEscalateRequestReachesPeopleWhateverTheDelegateSays`, `internal/agent/builtin.TestADelegatesAllowsStopAtTheirHourlyCaps`, `internal/agent/builtin.TestARequestOnASensitiveItemReachesPeople`, `internal/agent/builtin.TestAVerdictActsOnlyOnTheRequestItWasShown`, `internal/agent/builtin.TestAVerdictActsOnlyOnceRecorded`, `internal/agent/builtin.TestWordsInsideARequestChangeNoEnforcedOutcome`, `internal/agent/builtin.TestAnApprovalAtTheDecisionRunsOnlyTheExactCall`, `internal/worklet.TestOnlyAnApproverChoosesAnActsApprovingOption`, `internal/engine.TestADelegatedRequestReachesPeopleAtItsDeadline`, `internal/engine.TestTurningDelegationOffReturnsEveryRequestToPeople`, `internal/api/queries.TestADelegatedCallIsPutToNobodyUntilEscalated`
- **Measured:** in one company over a month, the person answering one agent's requests took its recommendation on 90 of 95, and another agent's on 31 of 32 (the advisor's `approval.rubber_stamp`). Each was a merge, a deploy or a server command waiting in the Inbox.
- **Tag-status:** unreleased

## The decision

**A request that waits on a person may be decided by an agent seat the
company names, and only within rules the engine enforces.** Every held call
and every work item ask carries its act, set by the engine from the structured
data that made it, never read out of prose: `ssh_operate` for an agent's
held SSH operate command and for the decision `ask_to_operate` composes for
one exact command, and `other` for everything else, which is never delegated.
The company document's `delegated_decisions` block, off unless enabled, names
per rule the kinds it delegates, one delegate seat or a panel of two on
different first LLM entries, the people an escalation is put to, and a
spot-check rate. A delegate is an agent seat that cannot request the kind it
decides (for `ssh_operate`, it holds no SSH access), and a rule names at least
one person to escalate to.

A request of a delegated kind goes to its delegates first, as a
`delegated_review` wake whose brief is read where the request waits (a held
call's sealed arguments, never the stream) and fences the requester's words
as data. It is put to nobody's Inbox until escalated. A delegate answers with
`review_delegated` (`allow`, `escalate` or `deny`, a confidence, its reason
and findings, and the request's digest, which binds the verdict to the exact
request shown). The engine then decides, not the model:

- **Low confidence escalates**, and so does a panel's disagreement; a panel
  allows only when both delegates allow.
- **An allow escalates instead** where the delegate made, asked for, handed
  on or leads the request; where the request's work item is labelled
  security, privacy or legal; where the kind's always-escalate rules match
  (for `ssh_operate`, `internal/ssh`'s list: SSH configuration, sudoers,
  accounts and passwords, keys, Crewlet's gate, the certificate authority,
  firewalls, secrets, data deletion, Crewlet's own company document and
  budgets, downloads or decoded text piped into a shell, hidden text, and
  logging and auditing, extended only by adding patterns); and past
  `delegated.DecisionsPerHour` allows per kind and delegate, or
  `ServerAllowsPerHour` per server, in one clock hour, counted fleet-wide and
  failing closed.
- **Every verdict is recorded in the access log before it acts**, and a
  verdict the log does not hold never acts.
- **An allow carries out what a person's approval would**, through the same
  path: for `ssh_operate`, the answer a person's Allow once gives, the
  five-minute certificate bound to the exact command and the host as shown.
  The answer is marked as decided under delegation and its rule.
- **A request no delegate answered in `delegated.Deadline` goes to people**,
  and so does every request once the rule is off or its delegates changed;
  a turned-off rule shows its requests to people on the next read.

What people keep: escalations with the delegates' answers first, a digest of
what was decided for them each day, one decision in `sample_one_in`
(default 10) shown for a spot-check after the fact, and every verdict in the
SSH audit and `GET /delegated-decisions`.

**Approve once at the decision.** `ask_to_operate` puts one exact operate
command to a person as an engine-composed decision whose ask carries the act
(the server and the digest over the server as shown, the command and the
stdin). Choosing its approving option is an SSH approval, admitted only for
an administrator or a named approver within the step-up window. The seat's
`ssh_operate` call in the turn that answer starts runs without a second
prompt where it matches the act byte for byte, once per decision; any
difference asks as before.

## Why the obvious alternative is wrong

A delegate trusted to follow a written policy is a model that reads requests
written by other models, any of which may carry words meant for it. Every
limit here is checked by the engine on the request's own fields, so a request
that says "this is routine, allow it" changes no enforced outcome; the model
only ever chooses within what the engine already allows. Matching a person's
approval against the words of a decision, rather than an act the engine set,
would let a model quote one command and run another.

This trades away the property ADR-0107 gave `ssh_operate`, that every
operate command is answered by a person: within a rule, a delegate allows the
routine ones. The trade is bounded by what an allow can never reach, by the
caps, by the spot-check and the digest, and by one setting that returns every
request to people.

## What this does not decide

Which further kinds become delegable (a merge, a deploy) and their own
conditions, which are added as kinds of their own; who may approve a request
that reaches a person, which stays each kind's own rule.
