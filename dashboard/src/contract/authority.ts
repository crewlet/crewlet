/**
 * What a refusal on authority says where NO access is the way past it: the
 * sentence for each rule the engine's authority table names.
 *
 * The KEYS are the engine's: `authz.Reasons`, every reason the deciding rule
 * reports, and `internal/authz`'s gate holds them here in both directions — a
 * reason the engine grew with no sentence is drawn as a claim about a rule
 * that did not decide, and a sentence for a reason nothing reports is what a
 * rename leaves behind.
 *
 * ONLY WHERE NO ACCESS IS THE WAY PAST, which the rule decides
 * (`lib/authorityRemedy.ts`), never the grants alone. A refusal some access
 * would clear names it (`needsWords`). Every one of these is a refusal none
 * would: a verb only an agent's seat makes, one made as a seat by a credential
 * bound to none, a token on a gesture that needs the person present, a proof
 * gone stale (whose grants are the ones that admitted the reader), somebody
 * else's record or a relation the org chart does not hold, an artifact's role
 * (which its owner's share clears). Each is a different thing to do about it,
 * which one sentence for all of them could not say: it told a credential bound
 * to no seat that what it lacked was leading somebody.
 *
 * THE ARTIFACT RULES. `not_shared` — no relation at all to an artifact — never
 * reaches a screen as `unauthorized`: the engine answers it `404 not_found`
 * with no reason, byte for byte an id nothing holds, so a screen draws an
 * absent artifact, and its row is the engine's own sentence for the reason,
 * kept because the keys are the engine's whole set. `role` is a relation below
 * the role the verb asks, which the artifact's owner clears by sharing it at a
 * higher one. `ceiling` is an agent acting for somebody who could not take the
 * verb themselves, which no access the agent holds would change.
 *
 * THE DASHBOARD ASSISTANT'S RULES. `assist_refused` is a verb the assistant
 * never takes, whatever its person approves, so the gesture stays with them;
 * `assist_scope` is one its grant's scope does not reach, which it makes only
 * as a change its person approves. A screen never meets either — its own
 * requests are its reader's, never the assistant's, and the assistant's panel
 * shows the engine's own words for a refusal it met — and their rows are kept,
 * like `not_shared`'s, because the keys are the engine's whole set.
 *
 * NULL FOR AN ADMISSION, which no refusal carries: the rule admitted the
 * caller, and there is nothing to say.
 *
 * `stage` is an account whose stage may not act — invited, enrolling,
 * suspended or retired — which an admin activates. It said "set up your second
 * factor first", which is a different refusal with a code of its own
 * (`second_factor_enrolment_required`): a person who has yet to enrol a factor
 * is active, holding a session that may only enrol one.
 */
export const REFUSAL_SENTENCES: Readonly<Record<string, string | null>> = {
  grant: null,
  self: null,
  author: null,
  lead: null,
  no_grant: "No access you can be given covers this.",
  not_self: "This is someone else’s, and no access would change that.",
  not_lead: "Only its lead in the org chart can see this.",
  not_author: "Only whoever wrote this can do that.",
  seat_refused: "Only a person can do this, not an agent.",
  token_refused: "Access tokens can’t do this. Sign in as yourself.",
  stage: "Your account isn’t active. Ask an admin to activate it.",
  unnamed: "The request didn’t say what it’s about. That’s a bug in this screen.",
  unknown_action:
    "This engine has no rule for that request. It may be running a different version. Reload the page.",
  step_up: "Confirm it’s you, then try again.",
  everybody: null,
  asked: null,
  seat_only: "Only an agent can do this.",
  unseated:
    "This is done as a seat, and this account has none. Ask an admin to bind it to a human seat (crewlet iam bind).",
  shared: null,
  not_shared: "This isn’t shared with you.",
  role: "Your role on this doesn’t allow that. Its owner can share it with you at a higher one.",
  ceiling: "The person this agent is working for can’t do this, so the agent can’t either.",
  assist_refused: "Ask Crewlet never does this, so it stays with you.",
  assist_scope: "Ask Crewlet can do this only as a change you approve.",
};
