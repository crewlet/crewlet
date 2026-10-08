/**
 * The identity directory's closed sets that People & access offers back to the
 * engine — the grants a person, an invitation or a token may carry, the login
 * grammars, the kinds of finding the directory reports — and the shapes of the
 * answers the seat surfaces and the reset screen read.
 *
 * EXACTLY THE ENGINE'S — each held by the gate its doc names. A grant the
 * engine grew that this lacks is one no administrator can confer from the
 * dashboard; one this keeps that the engine dropped is a checkbox every write
 * refuses; and a seat listing's member renamed on one side alone draws every
 * invited seat as vacant.
 */

/** Every grant, in the engine's own order (`iam.AllGrants`). */
export const GRANTS = [
  "state:read",
  "audit:read",
  "config:read",
  "secrets:read",
  "work:write",
  "knowledge:write",
  "config:write",
  "secrets:write",
  "fleet:operate",
  "people:manage",
  "sandbox:run",
] as const;

/**
 * The grants a machine token never carries, whatever its owner holds
 * (`iam.PersonPresentGrants`): revealing a credential's value, and deciding who
 * may do anything at all, need a person present. A token mint naming one is
 * refused, so the dialog says so before it is sent.
 */
export const TOKEN_WITHHELD_GRANTS = ["secrets:read", "people:manage"] as const;

/**
 * The two login grammars, as the engine holds them (`iam.ValidLoginFor`):
 * lowercase words of letters and digits with inner hyphens, a person's joined
 * by DOTS and a machine's by a COLON, at most [MAX_LOGIN] characters — and a
 * machine's never in a credential's class ([CREDENTIAL_CLASSES]).
 *
 * HELD BY BEHAVIOUR, not by spelling:
 * `internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar` reads
 * these four, compiles the two patterns and runs a corpus of logins through
 * them and through the engine's own check, failing on any login the two
 * answer differently. Before the forms held a login to them, `Frank` was
 * posted and came back as a 400 carrying the domain's own sentence.
 *
 * STRINGS rather than regular expressions, because a contract module is data
 * and `lib/login.ts` is where they are compiled.
 */
export const PERSON_LOGIN = "^[a-z0-9]+(?:-[a-z0-9]+)*(?:\\.[a-z0-9]+(?:-[a-z0-9]+)*)+$";

/** A machine's login: see [PERSON_LOGIN]. */
export const MACHINE_LOGIN = "^[a-z0-9]+(?:-[a-z0-9]+)*(?::[a-z0-9]+(?:-[a-z0-9]+)*)+$";

/** The longest login either grammar admits (`iam.MaxLogin`): a seat handle's own width. */
export const MAX_LOGIN = 64;

/**
 * The prefixes that name a CREDENTIAL in the operator column (`pat:` a
 * machine token, `session:` a browser session), which no machine's login may
 * begin with.
 */
export const CREDENTIAL_CLASSES = ["pat:", "session:"] as const;

/**
 * How long a machine token lives when its mint names no lifetime, in days
 * (`credential.DefaultTokenLifetime`), and the most it may be minted for
 * (`credential.MaxTokenLifetime`) — held by
 * `internal/api/iamapi.TestTheDashboardMintsWithinTheEnginesTokenLifetimes`.
 * The token dialog refuses a lifetime past the second before it posts.
 */
export const TOKEN_DEFAULT_DAYS = 90;

/** See [TOKEN_DEFAULT_DAYS]. */
export const TOKEN_MAX_DAYS = 365;

/**
 * The kinds of finding `GET /iam/check` reports, in the engine's order
 * (`iamapi.FindingKinds`) — which is the order the report is sorted in, so a
 * company's worst problem (nobody can administer it) comes first.
 *
 * HELD BY `internal/api/iamapi.TestTheDashboardWordsEveryFindingKindInTheEnginesOrder`
 * in both directions and in order. People & access words each one
 * (`FINDING_WORDS`, keyed on this list so a kind left unworded does not
 * compile); a kind the engine grew that this lacks is drawn as its raw code,
 * which is what `person_without_seat` would have been.
 */
export const FINDING_KINDS = [
  "no_people_manage_holder",
  "person_without_credential",
  "person_without_seat",
  "binding_dangling",
  "grant_clamped_by_ceiling",
] as const;

/**
 * `GET /iam/seats`: every human seat of the running company and what holds it
 * (`?unheld=true`: only the seats nothing holds — no person, no open
 * invitation — which are exactly the ones a bind may name).
 *
 * THE SEAT LISTING'S SHAPES are held by
 * `internal/api/iamapi.TestTheSeatSurfacesReadWhatTheSeatListingSends`, which
 * reads these four interfaces by name and holds the answer to each in both
 * directions. The org chart's card, the seat peek, a person's seat page and
 * People & access all read it, and a key renamed on the engine's side alone
 * would draw an invited seat as vacant on every one of them.
 */
export interface HumanSeatsAnswer {
  seats: HumanSeat[];
}

/** One human seat and what holds it (`iamapi.SeatRow`). */
export interface HumanSeat {
  handle: string;
  name: string;
  /** The key of the unit the seat sits in, absent at the root. */
  unit?: string;
  /**
   * Whoever the directory binds to the seat, at any stage short of removal —
   * a suspended person still holds theirs — and absent for a seat nobody
   * holds.
   */
  holder?: SeatHolder;
  /**
   * The open invitation that names the seat, absent where none does. It holds
   * the seat as surely as a person would: nothing else may be bound to it or
   * invited onto it until it is redeemed, cancelled or lapses.
   */
  invitation?: SeatInvitation;
}

/**
 * Who holds a human seat (`iamapi.SeatHolding`) — declared once and composed
 * into [HumanSeat], which is the name a screen reads.
 */
interface SeatHolder {
  /** Their id, which every `/iam` write about them names. */
  person: string;
  /**
   * `person` or `machine` (`iam.Kind`). A person holds exactly one human seat
   * for as long as they exist; a service account — a Tier A token's row
   * among them — may hold one, and is the only holder a seat is unbound from.
   */
  kind: string;
  login?: string;
  /** How far through enrolment they are (`iam.Stage`). */
  stage?: string;
}

/**
 * An open invitation as the seat it names reports it — never its link, which
 * was shown once.
 */
export interface SeatInvitation {
  id: string;
  /** The address it was issued to, opened on this node's keyring. */
  email?: string;
  /** An address this node's keyring cannot open — a state, never a blank. */
  sealed?: boolean;
  /** Who issued it: the seat they write as, or their login. */
  invited_by?: string;
  expires_at: string;
}

/**
 * What a password link's screen renders from, before anything is spent
 * (`GET /auth/reset/{id}`).
 *
 * HELD BY `internal/api/authapi.TestTheResetScreenReadsWhatTheResetViewSends`.
 * One screen serves two links — a reset an administrator issued to somebody
 * who has a password, and the FIRST password link a person created on a seat
 * is handed — and `first` is the only thing that tells it which to say: a
 * reset ends every session its person holds, and a first password ends none.
 */
export interface ResetView {
  /** Whose password the link sets. */
  login: string;
  /** When the link stops opening. */
  expires_at: string;
  /** The floor every password is held to. */
  min_password_length: number;
  /** The person holds no password yet: this link sets their first one. */
  first?: boolean;
}
