# ADR-0051: An assistant write executes only from its owner's approval of a server-rendered preview

- **Status:** accepted
- **Authority:** `internal/api/assistantapi`
- **Enforced-by:** `internal/api/opkey.TestARequestDigestBindsEveryPartOfTheRequest`, `internal/api/opkey.TestAnOperationsDigestIsTheCanonicalEncoding`, `internal/assistant/proposal.TestAProposalIsDrawnFromItsPreviewAlone`, `internal/assistant/proposal.TestAProposalWhoseDigestMovedIsRefused`, `internal/assistant/proposal.TestAVisibilityChangeNamesItsAudience`, `internal/assistant/proposal.TestEveryProposalMoveIsOneTheStateMachineAllows`, `internal/assistant/proposal/approval.TestAProposalExecutesOnce`, `internal/assistant/proposal/approval.TestARetriedProposalIsTheSameWrite`, `internal/assistant/proposal/approval.TestAnApprovalIsNeverAnotherPersons`, `internal/assistant/proposal/approval.TestAnApprovalOfAChangedProposalIsRefused`, `internal/assistant/proposal/approval.TestAStaleRevisionIsRefused`, `internal/assistant/proposal/approval.TestAStepUpRowAsksTheApprover`, `internal/assistant/proposal/approval.TestACompanyWideShareAsksAStepUp`, `internal/assistant/proposal/approval.TestAPageActionIsClaimedOnceAcrossTabs`, `internal/assistant/proposal/approval.TestAPageProposalIsStaleOnceThePageMoves`, `internal/api/assistantapi.TestTheAssistantCannotApproveItsOwnProposal`, `internal/api/assistantapi.TestEveryDecisionRefusesInTheEnvelope`, `internal/api.TestAReadingTurnCannotWrite`, `internal/api.TestAnAssistedWriteTakesOnlyTheVerbItWasApprovedFor`, `internal/api.TestAnAssistedWriteIsAuditedAsTheAssistant`, `internal/api.TestAnAssistGrantNeverReachesADecisionRoute`, `internal/api.TestNoWriteWithoutApproval`, `internal/api.TestInjectedTextNeverBecomesAWrite`, `internal/assistant/proposal.TestACharterWriteIsSentUnderTheRevisionItWasShown`, `internal/api.TestAPrinciplesChangeAsksAStepUp`, `internal/assistant/catalog.TestAGenericChangeIsPreviewedFromTheResource`, `internal/api.TestATurnProposesTheSeatAndChangesNothing`, `internal/api.TestAnAgentSeatAsksTheStepUp`, `internal/assistant/proposal.TestAWritesPreconditionIsItsRevisions`, `internal/assistant/catalog.TestAGenericChangeToNothingIsACreateOrNoCard`, `internal/assistant/proposal/approval.TestARacedCreateIsRefusedAsStale`, `internal/api.TestACreatableEntityIsCreatedAndARacedCreateIsRefused`, `internal/assistant.TestAChangeThatCannotBePreparedIsNeverACard`, `internal/api.TestTheAskedForRoleIsRefusedAtPreviewAndItsUnitWriteApplies`, `internal/api.TestEveryPUTAPreviewCanFindEmptyDeclaresWhatItDoes`, `internal/authz.TestARouteWithNoPolicyIsRefusedAtTheMount`, `internal/api.TestEveryPOSTSaysWhatItDoes`, `internal/assistant/catalog.TestAPostIsNamedForWhatItsRouteDeclares`, `internal/assistant/catalog.TestACallOnTheActRouteIsTitledByItsTool`, `internal/assistant/catalog.TestAChatDestructionSaysWhatGoesAndFromWhichRoom`, `internal/api.TestEveryGenericCardSpeaksThePersonsWords`
- **Tag-status:** unreleased

## The decision

**Nothing write-shaped executes inside an assistant turn.** When the model
calls a write tool (sharing and unsharing an artifact included, decision D09)
or a page action, the call is not sent: it becomes a **proposal**, the turn's
loop suspends with the call unanswered, and the parked turn holds no grant.

- **The card is rendered from a server-side preview, never from model prose.**
  The preview reads the object's current values through the turn's read grant
  and stores the exact request the approval would send (method, path, query and
  body in `internal/api/opkey`'s one canonical encoding), the object's source
  revision, and the before and after rows the card shows (decision D10). A
  model cannot describe one change and have another sent. A generic `POST`,
  whose method says nothing of what it does, is titled and rated by what its
  route declares (`authz.Policy.Effect`, mounted with the route: it creates,
  rewrites, destroys or only reads what its path names, and the act route calls
  a tool), so a purge or an erase reaches the person as a destructive change
  saying what goes, never as a create; a `POST` that declares nothing is never
  a card, and a route walk refuses one. A change decided by a verb nothing
  undoes (`authz.IrreversibleOf`: a purge, erasing or pruning a chat room, a
  room's horizon) is destructive whatever its method, its card saying what goes
  from what the change sends, and no card names a record by an opaque id.
- **The approval is bound to what was shown.** The proposal's digest, a full
  SHA-256 over its owner, conversation, turn, scope, tool, bound request, source
  revision and expiry, is shown on the card and echoed by the approval; the
  stored fields are re-hashed against it; the source revision is read again
  and a moved object is `stale`. Where the source revision is an HTTP entity
  tag (the company's principles, and any resource a generic API change was
  previewed against whose read answered one), the approved write is also sent
  under it as
  its `If-Match`, derived from the digest-bound revision rather than stored
  beside it, so the route itself refuses a change made between the re-read
  and the write. A generic `PUT` whose read found nothing is a create only
  where its route declares that it creates by address (`authz.Policy.Absent`,
  mounted with the route from the table its handler decides by), bound to
  that absence and sent under `If-None-Match: *` the same way, so the route
  refuses it once something holds the path; on a route that adds members
  elsewhere (a seat or a unit, which has a place in the chart its path cannot
  name) it is refused before any card, with the route's own sentence about
  where one is added, rather than approved and then refused every time; a `PATCH` or a `DELETE` of
  nothing is refused before any card is drawn, in the read's own words, and a
  `412` to the proposal's own precondition is shown as the change it lost to.
- **The approval is the person's, from their own browser.** It is a request
  carrying their session cookie and a matching Origin, to a proposal they own
  (anybody else's is not found), still proposed and unexpired (fifteen
  minutes). The assistant cannot make it: the guard refuses `/assistant/` to an
  assistant grant. An approver who no longer holds an app the proposing turn
  read through sends nothing: the proposal settles `failed`
  (`access_withdrawn`, the state machine's `withdrawn` event), and the parked
  loop, which keeps those reads verbatim, is never handed back to the model
  for them; the turn ends instead.
- **It executes once.** The decision is an arbitrated state-log record on the
  proposal's own subject, so two approvals on two nodes produce one; the
  approving request derives a single-use exec grant, sixty seconds long, bound
  to the request digest of the one request it may carry; and the write is sent
  under the proposal's own operation key, so a retry after an unknown outcome
  is the same write and the tracker holds one record.
- **A step-up the write's verb asks for is asked of the approver.** The write's
  `403 step_up_required` is answered on the approval request, the dashboard's
  one step-up dialog confirms, and the approval is replayed. The proposal is
  bound to the person and never to the session lineage, because a step-up
  rotates the lineage.

## Why the obvious alternatives are wrong

**Executing a write the model asked for, with a confirmation afterwards.** A
prompt injection in a page the person asked about would then act before anybody
read what it did; the person's click is the only defence that does not depend
on the model behaving.

**Rendering the card from the model's tool arguments, or its description of
them.** The card would then be the model's claim about the change rather than
the engine's reading of it, and the field the person approves would not be the
field that is sent.

**A compare-and-swap in the coordination store for the decision.** The
proposal, its card and its transcript have to be readable from any node, which
is the replicated estate's job; the state log's arbitration is already the
engine's exactly-once decision across nodes, and a second mechanism beside it
would be a second answer to "was this approved".

## What this does not decide

The proposal states and their legal moves (`internal/assistant/proposal`), the
routes and their refusal codes (`internal/api/assistantapi`), and which verbs
are approval-gated at all (ADR-0052).
