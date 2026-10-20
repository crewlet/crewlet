/**
 * ACCESS, as the dashboard reads it: admin and one permission per app.
 *
 * The engine's whole vocabulary is admin and the apps (`contract/identity.ts`'s
 * GRANTS, held to `iam.AllGrants`): `admin`, which runs the company and the
 * engine and INCLUDES EVERY APP, and one permission per app: `worklet` for
 * Worklet, `doclet` for Doclet, `artifacts` for authoring artifacts and `chatlet`
 * for the company's own chat (APPS, held to `iam.Apps`, the one place an app
 * is declared). Everything a signed-in
 * principal may do beyond its own account and the read-only overview comes
 * from these, so this module is the one place a screen asks "does the reader
 * hold this?" and the one place the dashboard says a permission in words.
 *
 * # Admin includes every app, through [expand]
 *
 * The engine answers a principal's grants already expanded — a row of
 * `[admin]` is reported as admin plus every app — so a screen reading the
 * viewer needs nothing more. A STORED row is another matter: a person's
 * directory row, an invitation or a token may hold `[admin]` alone (a Tier A
 * file, a CLI create), and reading it raw would draw an administrator as
 * holding no app. Every comparison here therefore goes through [expand],
 * the engine's own `iam.Expand`, so a lock, a picker and a People row read
 * one answer however the set was stored.
 *
 * # A name this build does not know is kept and ignored
 *
 * A newer engine may report a permission this dashboard has no word for. It
 * is never a lock that opens (`holds` matches only a known name) and never a
 * crash: it is said as its raw name where a set is listed, so an operator
 * still sees it is there.
 */

import { APPS, GRANTS } from "~/contract/identity.ts";
import { KIND_LIST } from "~/lib/artifacts/words.ts";

/**
 * A permission a principal carries — the engine's `iam.Grant` vocabulary, as
 * a closed union so a lock naming a permission the engine does not have is a
 * compile error rather than a lock nobody can open.
 */
export type Grant = (typeof GRANTS)[number];

/** One app's permission — the engine's `iam.Apps`. */
export type App = (typeof APPS)[number];

/** The permission that runs the company and the engine, and includes every app. */
export const ADMIN: Grant = "admin";

/** Worklet's permission: projects, tasks, the inbox and My work. */
export const WORKLET: App = "worklet";

/** Doclet's permission: spaces and pages. */
export const DOCLET: App = "doclet";

/** The company's own chat's permission. */
export const CHATLET: App = "chatlet";

/** Whether `name` is an app this build knows. */
export function isApp(name: string): name is App {
  return (APPS as readonly string[]).includes(name);
}

/** Whether `name` is a permission this build knows. */
export function isGrant(name: string): name is Grant {
  return (GRANTS as readonly string[]).includes(name);
}

/**
 * The set, plus every app when it holds `admin` — the engine's `iam.Expand`.
 *
 * A FRESH ARRAY that never aliases `set`: its members in their order, unknown
 * names kept, then every app it did not already hold, in APPS order, when
 * `admin` is among them. Nothing is added otherwise, and `expand(expand(s))`
 * equals `expand(s)`.
 */
export function expand(set: readonly string[]): string[] {
  const out = [...set];
  if (!set.includes(ADMIN)) return out;
  for (const app of APPS) {
    if (!out.includes(app)) out.push(app);
  }
  return out;
}

/**
 * Whether a reader holding `held` holds `grant` — a known name only, counting
 * every app an admin carries.
 */
export function holds(held: readonly string[], grant: Grant): boolean {
  return isGrant(grant) && expand(held).includes(grant);
}

/** Whether a reader holding `held` holds ANY ONE of `needs` — true when it names none. */
export function holdsAny(held: readonly string[], needs: readonly Grant[] | undefined): boolean {
  if (!needs || needs.length === 0) return true;
  return needs.some((grant) => holds(held, grant));
}

/**
 * The words for each permission — the label every surface names it by, and
 * the sentence a picker draws under it. KEYED ON GRANTS, so a permission the
 * engine grew is a compile error here until it has words.
 *
 * Admin's sentence says it is HOST ACCESS, because it is: the configuration
 * an admin writes runs commands on every engine host (an MCP server, a coding
 * CLI, a sandbox on the host). Ticking it is giving somebody a shell there,
 * and the picker is the last place to say so before it is given.
 */
export const ACCESS_WORDS: Record<Grant, { label: string; description: string }> = {
  admin: {
    label: "Admin",
    description:
      "Runs the company: settings, secrets and people. Includes every app, and host access to the engine.",
  },
  worklet: {
    label: "Worklet",
    description: "Tasks, projects, the inbox and My work.",
  },
  doclet: {
    label: "Doclet",
    description: "Read, search and write pages.",
  },
  artifacts: {
    label: "Artifacts",
    description: `Create and publish ${KIND_LIST}. Anything shared with them opens without it.`,
  },
  chatlet: {
    label: "Chatlet",
    description:
      "Use the company's own chat: read, search and talk in rooms, open conversations, and keep your read state. Agents talk to people through it.",
  },
};

/** A permission's label, or its raw name for one this build has no word for. */
export function accessLabel(name: string): string {
  return isGrant(name) ? ACCESS_WORDS[name].label : name;
}

/**
 * What a set of permissions IS, as one line — the People list's Access
 * column, the Account's "Your access", the user block:
 *
 *   - "Admin" for anybody holding admin (every app is included, so naming
 *     them beside it would read as if they were separate);
 *   - otherwise the apps they hold, "Worklet · Doclet";
 *   - "Overview only" for nobody's apps — the org chart, agents' status,
 *     spend and their own account, which every signed-in principal reads.
 *
 * A name this build does not know is appended raw, so it is never silently
 * dropped from a row that holds it.
 */
export function accessLine(grants: readonly string[]): string {
  const unknown = grants.filter((g) => !isGrant(g));
  const known = grants.includes(ADMIN)
    ? [ACCESS_WORDS.admin.label]
    : APPS.filter((app) => grants.includes(app)).map((app) => ACCESS_WORDS[app].label);
  const parts = [...known, ...unknown];
  return parts.length > 0 ? parts.join(" · ") : "Overview only";
}

/**
 * What a seat's withheld BODY is drawn as — a prompt, an answer, a tool's
 * arguments, a coding run's task — where the engine sent only its status
 * (`body_withheld`). Said, never drawn as an empty value, because "the prompt
 * was empty" and "you may not read the prompt" are opposite facts.
 */
export const WITHHELD_WORDS = "Hidden";

/** Why, in one sentence: whose a seat's trail is to read. */
export const WITHHELD_WHY = "Only this seat’s lead and admins can see it.";

/** What a coding run's question is drawn as where the engine withheld it. */
export const QUESTION_WITHHELD = "Its question is hidden";

/**
 * Whether a coding run's QUESTION was withheld from this reader: the engine
 * sent no question and said it withheld the run's body (`body_withheld`). The
 * people a question is put to are always sent it, so this is a reader looking
 * at somebody else's run — told so, never shown a run that asked nothing.
 */
export function questionWithheld(run: { question?: string; body_withheld?: boolean }): boolean {
  return !run.question && run.body_withheld === true;
}

/**
 * A refused change's one sentence, to the reader: "You need Worklet access for
 * this." ONE SENTENCE where the refusal and its remedy were two that said the
 * same thing ("You do not hold what this change needs. This needs Worklet
 * access.").
 */
export function youNeedWords(needs: readonly string[]): string {
  return `You need ${accessNames(needs)} access for this.`;
}

/**
 * The permissions a lock names, as words: "Worklet", "Worklet or Doclet",
 * "Worklet, Doclet or Artifacts": commas, and "or" before the last, the
 * way the engine names them in a refusal's own sentence (httpjson's
 * grantWords), so a screen and the message beside it never list one set two
 * ways. Joined by "or" alone, three permissions read as "Worklet or Doclet
 * or Artifacts" on the screen while the engine wrote the comma form.
 */
export function accessNames(needs: readonly string[]): string {
  const words = needs.map(accessLabel);
  if (words.length < 3) return words.join(" or ");
  return `${words.slice(0, -1).join(", ")} or ${words[words.length - 1]}`;
}

/**
 * What a lock says, as the kit's `LockedLabel` takes it: "Needs Admin
 * access", or nothing where nothing is missing. One sentence for the
 * sidebar's rows, the Settings column's and the section tabs'.
 */
export function lockReason(needs: readonly string[]): string | undefined {
  return needs.length > 0 ? `Needs ${accessNames(needs)} access` : undefined;
}

/**
 * A group's lock: the one reason every row of it shares, so the key is drawn
 * once on the group's label rather than down every row (the kit's
 * `SidebarNav.Group` `locked`). Nothing where any row is open, where the rows
 * want different access, or where the group is empty: there the key stays on
 * each locked row, which is what tells them apart.
 */
export function groupLock(reasons: readonly (string | undefined)[]): string | undefined {
  const [first] = reasons;
  return first !== undefined && reasons.every((reason) => reason === first) ? first : undefined;
}

/**
 * The one sentence a lock or a refusal says about `what` (a capitalised
 * subject: "Search", "Taking a backup") and the permissions that would open it:
 * "Search needs Doclet access. An admin can give it to you."
 *
 * FROM THE ANSWER WHERE THERE IS ONE: a refusal's `grants` are the engine's
 * own (`refusedGrants`), and a lock's are the navigation table's. With none,
 * nothing the engine accepted was presented — a 401 — and the repair is to
 * sign in rather than to ask an admin.
 */
export function needsWords(what: string, needs: readonly string[]): string {
  if (needs.length === 0) return "Sign in to see this.";
  return `${what} needs ${accessNames(needs)} access. An admin can give it to you.`;
}
