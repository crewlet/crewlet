/**
 * What a refusal on authority says when NO access would have admitted the
 * reader — the sentence for each rule the engine's authority table names.
 *
 * The KEYS are the engine's: `authz.Reasons`, every reason the deciding rule
 * reports, and `internal/authz`'s gate holds them here in both directions — a
 * reason the engine grew with no sentence is drawn as a claim about a rule
 * that did not decide, and a sentence for a reason nothing reports is what a
 * rename leaves behind.
 *
 * ONLY FOR AN EMPTY `grants`. A refusal some access would clear names it
 * (`needsWords`), and every one of these is a refusal that names none — a verb
 * only an agent's seat makes, one made as a seat by a credential bound to
 * none, a token on a gesture that needs the person present, somebody else's
 * record or a relation the org chart does not hold — and each is a different
 * thing to do about it, which one sentence for all of them could not say: it
 * told a credential bound to no seat that what it lacked was leading somebody.
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
};
