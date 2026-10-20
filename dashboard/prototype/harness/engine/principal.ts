/**
 * Who the browser is, as the engine's request guard resolves it, and the
 * three answers that follow from it: `GET /auth/session`
 * (`internal/api/authapi/session.go`), the `viewer` question
 * (`internal/api/queries/viewer.go`) and how a change the browser makes is
 * written down (`iam.ActorFor`).
 *
 * ONE PRINCIPAL, THREE READINGS. The company holds the principal and nothing
 * derived from it, so a scenario that signs the browser in as somebody else
 * changes one value and every surface that names the reader follows: the
 * account block, the viewer's grants and acts, and the author of the next
 * comment.
 */

import { ACTIONS } from "~/contract/actions.ts";
import type { ActionTool } from "~/protocol/act.ts";
import type { SessionAnswer, Viewer } from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { derive, projectOfSeat } from "./derive.ts";
import { uuidV5 } from "./ids.ts";

/** `iam.kind` as a principal carries it: what `GET /auth/session` names as `kind`. */
export type PrincipalKind = "person" | "machine";

/**
 * The principal a request resolves to (`iam.Principal`), with the credential
 * it came through.
 */
export interface DemoPrincipal {
  /** The principal's id (`person` on the session answer). */
  person: string;
  /** Its login: a person's dotted name, a machine's `class:id`. */
  login: string;
  /**
   * The name on its own directory row, or "" where it has none: a Tier A
   * token has no row, and a machine's row usually holds no name.
   */
  name: string;
  /**
   * A person, or a machine. A Tier A token or a machine token BOUND to a seat
   * is re-kinded a person by the guard (`Guard.principalFor`), since it acts
   * as the seat's holder.
   */
  kind: PrincipalKind;
  /** The seat the identity directory binds it to, or "". */
  seat: string;
  /** Its grants after this node's admin posture: admin expanded into every app. */
  grants: string[];
  /**
   * The credential the browser presents. A session (a person's sign-in, or a
   * Tier A token exchanged at `POST /auth/token`) is a cookie with a lineage
   * and an absolute deadline; a machine token is a bearer with neither.
   */
  credential: { session: { lineage: string; expires_at: string } } | { token: string };
}

/** `auth.TokenNamespace`: the namespace a Tier A token's principal id is derived in. */
const TOKEN_NAMESPACE = "6f1c2d5e-9a34-5b7c-8e10-4d2f6a8b3c91";

/** `iam.AllGrants`, in the engine's order: admin, then every app. */
const ALL_GRANTS = ["admin", "tracker", "knowledge"] as const;
const APPS = ["tracker", "knowledge"];

/** `api.auth.session.step_up`'s default (`config.DefaultSessionStepUp`). */
const STEP_UP_MS = 60 * 60_000;

/**
 * The grants a principal carries on a serving node (`AdminPosture.Effective`):
 * admin expanded into every app, an unknown name dropped, in the engine's order.
 */
function effectiveGrants(held: readonly string[]): string[] {
  const expanded = new Set(held.includes("admin") ? ALL_GRANTS : held);
  return ALL_GRANTS.filter((g) => expanded.has(g));
}

/** The principal id the guard derives for a Tier A token (`uuid.NewSHA1(TokenNamespace, id)`). */
function tokenPrincipalId(tokenId: string): string {
  return uuidV5(TOKEN_NAMESPACE, tokenId);
}

/**
 * A browser session exchanged from Tier A token `tokenId` (`POST /auth/token`).
 *
 * THE TOKEN IS THE SUBJECT: its login is `token:<id>`, its id the one the
 * guard derives, and it has no directory row, so no name of its own. Bound to
 * `seat` in the identity directory it is re-kinded a person acting as that
 * seat; bound to none it stays a machine. Its grants are the entry's
 * (`api.auth.tokens[].grants`), and the session lasts one hour from the
 * exchange (`tokenLifetime`).
 */
export function tokenSession(o: {
  tokenId: string;
  seat: string;
  grants: readonly string[];
  lineage: string;
  expiresAt: string;
}): DemoPrincipal {
  return {
    person: tokenPrincipalId(o.tokenId),
    login: `token:${o.tokenId}`,
    name: "",
    kind: o.seat ? "person" : "machine",
    seat: o.seat,
    grants: effectiveGrants(o.grants),
    credential: { session: { lineage: o.lineage, expires_at: o.expiresAt } },
  };
}

/**
 * A person signed in with their password (`POST /auth/login`): their own
 * directory row's id, login and name, the human seat every person holds
 * (ADR-0032), and the grants on their row.
 */
export function personSession(o: {
  person: string;
  login: string;
  name: string;
  seat: string;
  grants: readonly string[];
  lineage: string;
  expiresAt: string;
}): DemoPrincipal {
  return {
    person: o.person,
    login: o.login,
    name: o.name,
    kind: "person",
    seat: o.seat,
    grants: effectiveGrants(o.grants),
    credential: { session: { lineage: o.lineage, expires_at: o.expiresAt } },
  };
}

/**
 * A service account presenting one of its machine tokens (`pat:<id>`) on
 * every request: no session, so no lineage and no deadline on the session
 * answer. Bound to a seat it acts as that seat's holder, as a bound Tier A
 * token does; unbound it acts under its own login.
 */
export function machineToken(o: {
  person: string;
  login: string;
  name?: string;
  seat?: string;
  grants: readonly string[];
  token: string;
}): DemoPrincipal {
  const seat = o.seat ?? "";
  return {
    person: o.person,
    login: o.login,
    name: o.name ?? "",
    kind: seat ? "person" : "machine",
    seat,
    grants: effectiveGrants(o.grants),
    credential: { token: o.token },
  };
}

/** The credential name the operator column records (`Principal.Via`, else the login). */
function operatorIdOf(p: DemoPrincipal): string {
  if ("session" in p.credential) return `session:${p.credential.session.lineage}`;
  return `pat:${p.credential.token}`;
}

/**
 * The name a principal's own record is kept under (`iam.RecordOwner`): a
 * bound person's seat, and everybody else's login.
 */
export function recordOwnerOf(p: DemoPrincipal): string {
  return p.kind === "person" && p.seat ? p.seat : p.login;
}

/** How a change the browser makes is written down (`iam.ActorFor`). */
export interface DemoActor {
  /** The author: the seat for a bound person, the login otherwise. */
  name: string;
  /**
   * `human` for a bound person, `operator` for any credential acting as
   * itself, `system` for the engine's own write.
   */
  kind: "human" | "operator" | "system";
  /** The credential it came through: `session:<lineage>` or `pat:<id>`. */
  operator_id: string;
}

export function actorOf(c: DemoCompany): DemoActor {
  const p = c.principal;
  return {
    name: recordOwnerOf(p),
    kind: p.kind === "person" && p.seat ? "human" : "operator",
    operator_id: operatorIdOf(p),
  };
}

/** What `GET /auth/session` answers the browser. */
export function sessionOf(
  c: DemoCompany,
  now: Date,
): SessionAnswer & { stage: string; reauth_at: string; step_up_due: boolean } {
  const p = c.principal;
  const out: SessionAnswer & { stage: string; reauth_at: string; step_up_due: boolean } = {
    person: p.person,
    login: p.login,
    kind: p.kind,
    grants: [...p.grants],
    stage: "active",
    // EVERY CREDENTIAL HERE IS FRESH: a token, exchanged or presented, is
    // stepped up by construction, and a person's proof is taken as just made.
    reauth_at: new Date(now.getTime() + STEP_UP_MS).toISOString(),
    step_up_due: false,
    status: "signed_in",
  };
  if (p.name) out.name = p.name;
  if (p.seat) out.seat = p.seat;
  if ("session" in p.credential) {
    out.expires_at = p.credential.session.expires_at;
    out.lineage = p.credential.session.lineage;
  }
  return out;
}

/** One write's admission rule (`internal/authz/rules.go`), for the tools the act surface serves. */
interface ActRule {
  class: "app" | "any_app" | "app_or_asked" | "own_record" | "own_or_lead" | "saved_view" | "tidy";
  app?: "tracker" | "knowledge";
  /** The tool names its object inside the tool, so the gate asks with it unresolved. */
  inTool?: boolean;
}

const ACT_RULES: Record<ActionTool, ActRule> = {
  create_work_item: { class: "app", app: "tracker" },
  answer_knowledge: { class: "any_app" },
  update_work_item: { class: "app", app: "tracker" },
  restore_work_item: { class: "tidy", app: "tracker", inTool: true },
  write_project: { class: "app", app: "tracker" },
  set_pins: { class: "own_record", app: "tracker" },
  set_priorities: { class: "own_or_lead", app: "tracker", inTool: true },
  comment_on_work_item: { class: "app_or_asked", app: "tracker", inTool: true },
  answer_run: { class: "own_or_lead", inTool: true },
  pause_seat: { class: "own_or_lead", inTool: true },
  resume_seat: { class: "own_or_lead", inTool: true },
  steer_turn: { class: "own_or_lead", inTool: true },
  write_page: { class: "app", app: "knowledge" },
  save_page: { class: "app", app: "knowledge" },
  comment_on_page: { class: "app", app: "knowledge" },
  place_work_item: { class: "app", app: "tracker" },
  save_work_view: { class: "saved_view", app: "tracker" },
  mark_inbox: { class: "own_record", app: "tracker" },
};

/** The rules that read a relation, which an app's absence closes outright (`relationClasses`). */
const RELATION: ActRule["class"][] = ["own_record", "own_or_lead", "saved_view", "tidy"];

/** Whether `actor` leads anybody on the chart (`orgchart.leadsAnyone`). */
function leadsAnyone(c: DemoCompany, actor: string): boolean {
  if (!actor) return false;
  const seat = (derive(documentOf(c)).seats ?? []).find((s) => s.handle === actor);
  return (seat?.reports?.length ?? 0) > 0;
}

/**
 * Whether the gate would offer `tool` before any object is named
 * (`builtin.Admits` over `authz.Decide`): admitted, or refused only for want
 * of the object (`ErrUnresolved`).
 */
function admits(c: DemoCompany, tool: ActionTool): boolean {
  const p = c.principal;
  const rule = ACT_RULES[tool];
  const can = (g: string) => p.grants.includes(g);
  const owner = rule.inTool ? "(unresolved)" : recordOwnerOf(p);
  const self = owner === p.login || (p.seat !== "" && owner === p.seat);
  let admitted: boolean;
  switch (rule.class) {
    case "app":
      admitted = can(rule.app!);
      break;
    case "any_app":
      admitted = APPS.some(can);
      break;
    case "app_or_asked":
      // Not held, the object decides: unresolved here, which is admitted.
      admitted = true;
      break;
    case "own_record":
      admitted = (owner !== "" && self) || can("admin");
      break;
    case "own_or_lead":
      admitted = self || can("admin") || leadsAnyone(c, actorOf(c).name);
      break;
    case "saved_view":
      // Asked with no view named, only the admin path admits.
      admitted = can("admin");
      break;
    case "tidy":
      // Not held, the task's own row decides: unresolved here, which is admitted.
      admitted = true;
      break;
  }
  if (rule.app && RELATION.includes(rule.class) && !can(rule.app)) return false;
  if (rule.class === "app_or_asked" && !can(rule.app!)) return true;
  return admitted;
}

/** What the `viewer` question answers the browser. */
export function viewerOf(c: DemoCompany): Viewer {
  const p = c.principal;
  const doc = documentOf(c);
  const seat = p.seat
    ? (derive(doc).seats ?? []).find((s) => s.handle === p.seat && p.kind === "person")
    : undefined;
  const writers = c.engine.company_writers;
  return {
    login: p.login,
    grants: [...p.grants],
    handle: seat?.handle ?? "",
    name: seat?.name ?? "",
    kind: seat?.kind ?? "",
    owner: recordOwnerOf(p),
    acts: (Object.keys(ACTIONS) as ActionTool[]).filter((tool) => admits(c, tool)),
    project: seat ? projectOfSeat(doc, seat.handle) : "",
    // `APIAuth.MayWriteCompany`: anybody while the document is unmanaged,
    // and once it is, only a listed Tier A token PRESENTED as itself, never
    // through a session exchanged from it.
    config_writer: writers.length === 0 || writers.some((id) => operatorIdOf(p) === `token:${id}`),
    config_managed_by: [...writers],
  };
}
