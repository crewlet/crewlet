# Manager 1:1

## What a 1:1 is

A short, **private** working session between you and the person you report
to (or, if you are the manager, your direct report). You review recent work
together, exchange feedback, surface blockers, and leave with a few concrete
action items. It is not a status broadcast and not a performance tribunal —
it is how the two of you stay aligned.

## Channel — use A2A, keep it private

Open the 1:1 on a **private A2A channel** with `a2a_ask`. A 1:1 is private,
so it does **not** go in Mattermost or Plane — those are for things the whole team
should see. (This is the one review-type conversation that belongs on A2A
*because* it is private.) If your manager is a **human teammate**, they are
not on A2A: reach them in a Mattermost DM / thread instead, leave the full
context in your message, and end your turn — they reply asynchronously.

**One channel is one exchange.** Your ask is delivered, the other side's
answer comes back, and the channel closes on its own — you never close it and
there is no tool to send a second message on it. So put the whole of your
half in the one message, and if you genuinely need another round, open a new
one with `a2a_ask`. Keep it tight: **two exchanges each way, at most**. Past
that the engine's delegation cap stops the conversation for you, which is a
worse ending than agreeing on the action items and stopping.

## If you are the report (the employee)

1. **Prepare.** Use `query_episodes` to recall what you actually did since
   the last 1:1 — what you shipped, what stalled, what you learned. Pull the
   relevant Plane work items if you need specifics.
2. **Walk your manager through it in one message.** Be concrete and be
   candid about blockers — a 1:1 is the place to raise what isn't working,
   not to hide it. Your manager answers the message you actually sent, so
   what you leave out does not get discussed.
3. **Ask for feedback** on the work and on how you're operating — name the
   specific feedback you want in the same message.
4. **Capture the outcome.** Note the action items you agree on. You do not
   need to "save" the conversation — the engine persists durable facts from
   the turn automatically — but do record concrete commitments (e.g. open or
   update the Plane work items for the action items).

## If you are the manager

1. **Review before you respond.** Look at the report's recent work (their
   Plane board, the team channel, the MRs) so your feedback is specific, not
   generic.
2. **Give actionable feedback** — concrete, tied to real work, balanced
   between what went well and what to change.
3. **Draw out blockers** and commit to unblocking the ones that are yours.
4. **Converge in your reply.** Your response *is* what gets delivered back —
   put the feedback, the answers to their blockers, and a small number of
   agreed action items in it. If something genuinely needs another round, ask
   for that one specific thing with `a2a_ask`; do not plan on a long
   back-and-forth.
5. **Route standing rules to the team, not just the 1:1.** If the feedback is
   a *standing rule* the whole team should follow ("always get a review before
   merging"), it belongs in the **team's Plane pages**, not buried in one
   person's 1:1 — update the relevant page (or hand off to whoever owns it) so
   every report picks it up. A 1:1 changes one person; team policy changes the
   team.

## What happens to the conversation afterward

You don't manage this — the engine does it for you:

- **Facts** you surface ("my manager wants EOD updates on Fridays") are
  written to your private memory automatically.
- **Standing directives** are recognised as team policy and nudged toward the
  team docs rather than your personal memory.
- The conversation itself is private and ephemeral — nobody can browse it
  later; only the **outcomes** — your memory, the Plane action items, any
  team page you updated — persist and are visible.
