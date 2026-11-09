# ADR-0113: An agent's words wake only the seats they address

- **Status:** accepted
- **Authority:** `internal/routing`
- **Enforced-by:** `internal/chatlet.TestAnAgentsAcknowledgementWakesNoAgent`, `internal/chatlet.TestAMessageAddressedToAPersonWakesNoUnnamedAgent`, `internal/chatlet.TestAnAnswerWakesTheAgentThatAsked`, `internal/chatlet.TestAnAgentsAskingMessageRoutesAsBefore`, `internal/worklet.TestAnAgentsReplyInAThreadWakesOnlyWhoStillOwesAnAnswer`, `internal/worklet.TestAPersonsCommentNamingSomebodyWakesOnlyWhomItNames`, `internal/chatlet.TestTwoAgentsThankingEachOtherStopAtTheAnswer`
- **Measured:** on a company running this engine, two agents in one thread answered each other's acknowledgements six times in five minutes, each saying it would stop replying, and agents took turns for status notes addressed to a person only to decide to say nothing. Over the same five hours, 200 turns had a median of 20 seconds.
- **Tag-status:** unreleased

## The decision

**A message wakes a seat for a standing arrangement only when it asks something and is addressed to nobody in particular.** Following a room, following a thread, having spoken in a thread, being the other side of a direct conversation or the author of the thread are standing arrangements. They wake a seat for a person's message that names nobody, and for an agent's message that says it asks something, and never otherwise.

**An agent's message asks something only where the agent says so.** The chat tools take `asks`; a message without it wakes only the seats it names, under a reason that obliges no answer (`named`), the seats whose open question it answers (`answer`), and the seats asked something in its thread that have not answered yet (`owing`). An asking message gives each seat it asked a receipt, so its answer wakes the asker. A tracker comment needs no flag: an ask is already a structured record there, and an agent's reply in a thread wakes only the participants an open ask in that thread is addressed to.

**A message that names somebody is addressed to them.** Whoever wrote it, a message that @-mentions a seat, asks one, or answers one wakes the seats it names, the asker it answers and, in a direct conversation, the other side when it asks, and no seat for a standing arrangement. People are never woken and still see everything, in the room and in their inbox.

Every decision is made from structured facts the record carries (the author's kind, the mentions, the `asks` statement, the ask and answer records and the open receipts), never from the prose. The seats a rule held back ride the chat record as `held`, which the parser logs as `chat_wakes_held`.

## Why the obvious alternative is wrong

**The obvious alternative is to tell agents not to acknowledge**, in the prompt. The prompt already said an unaddressed wake may end in silence, and the measured loop ran anyway: a direct message and a mention oblige an answer through the delivery gate, so an agent acknowledged into a direct conversation or named in an acknowledgement had to reply, and its reply obliged the other side. Text cannot release an obligation the routing imposes.

**Reading the prose for a question mark or a thank-you** is the other obvious answer. A model's "Done, thanks" and "Done, can you check?" differ in a way only the model knows, and a heuristic that guesses wrong either drops a question or keeps the loop. The agent knows whether it is asking, so it says so, and the engine records it.

## What this does not decide

It does not change who is told: every candidate a Worklet change concerns still gets its inbox notice, and every chat message stays in its room. It does not change a person's routing when the person names nobody. It does not change the vendor chat surfaces (Mattermost and Slack): an agent posts there through the vendor's own message, which carries no `asks` statement for the engine to read, so their routing stays the vendor targeting it was.
