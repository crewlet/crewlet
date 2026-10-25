# ADR-0052: An assistant conversation belongs to its owner

- **Status:** accepted
- **Authority:** `internal/assistantdomain`
- **Enforced-by:** `internal/assistantdomain.TestAnotherPersonsConversationIsNotFound`, `internal/assistantdomain.TestEverySealedValueOpensOnlyUnderItsOwnRecord`, `internal/assistantdomain.TestTheApplierNeverOpensASealedValue`, `internal/assistantdomain.TestAPurgedConversationIsNeverRecreated`, `internal/assistantdomain.TestAConversationPastItsRetentionIsReaped`, `internal/assistantdomain.TestAnErasedPersonsConversationsGoInOnePass`, `internal/assistantdomain.TestAComplianceReadIsRecordedBeforeItOpens`, `internal/assistantdomain.TestTheAssistantDomainReadsNoOtherDomain`
- **Tag-status:** unreleased

## The decision

**A Dashboard Assistant conversation is its owner's, replicated, sealed and
gone thirty days after it was last used.**

- **Replicated.** Conversations, turns, the messages, steps and cards the person
  saw, and the proposals they decided are a state-log domain of their own
  (`CREWLET_ASSISTANT_LOG`), applied into every data node's replicated estate,
  because a person opens the drawer on any node and a reconnect may land on
  another. A proposal's decision is arbitrated on its own subject (ADR-0050).
  A turn's live frames are not stored at all: they are told, as presence is
  (ADR-0038).
- **Its owner's.** Every read is keyed on the owner, and another person's
  conversation, card, export or proposal is not found, never forbidden. The one
  exception is decision D06: an administrator may read a conversation for
  compliance through a verb of its own (`assistant.conversation.audit_read`:
  admin, a recent proof, a person present, never through the assistant) with a
  reason they must give. Each such read is recorded as its own audit event
  naming the administrator and the reason, and beside the conversation so its
  owner sees who read it and why.
- **Sealed.** Every message, card, preview and parked turn is sealed under the
  fleet keyring with associated data naming the conversation and the record,
  because the estate is not encrypted at rest; the applier never opens a value,
  so every node writes identical rows. What is stored is what the person saw:
  never a raw tool result outside the bounded parked turn, a grant, a request
  header or a rendered prompt.
- **Bounded in time.** A conversation untouched for `assistant.retention_days`
  (thirty by default, one to three hundred and sixty-five) is purged through its
  own purge record by the trash reaper; the Clear gesture purges at once; a
  purge installs a gate so nothing later recreates the conversation; and a
  person's erasure takes their conversations in the same pass. A stored card
  whose app the reader no longer holds is replaced by a notice on every read,
  and its export is not found.

## Why the obvious alternatives are wrong

**The node's own event log, which already holds turn detail.** Turn detail
there is readable by whoever may read events, and a person's assistant
conversation holds whatever that person was able to read; it would become a
second way to read it.

**Administrators reading every conversation as a matter of course.** A
conversation is a copy of what one person was shown; an unaudited reader of it
is a second disclosure path. The compliance read exists, and it costs a reason,
a step-up and a record the owner sees.

**Keeping conversations until deleted.** Each one is a copy of data shown to a
person, on every data node; thirty days matches the turn detail and trash
horizons and bounds what a breach of one node exposes.

## What this does not decide

The record shapes, subjects, tables and sealing format, which are
`internal/assistantdomain`'s own doc, and the HTTP routes, which are
`internal/api/assistantapi`'s.
