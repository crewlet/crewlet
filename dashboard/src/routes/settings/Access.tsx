/**
 * Settings › Access: who can reach this company, and as whom, on three pages
 * that split the identity directory between them so no row is drawn twice.
 *
 *  - PEOPLE ([People]): every principal but a service account, the
 *    invitations still open, and the human seats people come from.
 *  - SERVICE ACCOUNTS ([ServiceAccounts]): the machine principals, what each
 *    holds and the seat it acts as.
 *  - API TOKENS ([ApiTokens]): this node's Tier A tokens, each joined to the
 *    directory row its login names.
 *
 * A directory finding is drawn on the page of the principal it names
 * ([findingOn]).
 *
 * # One join, drawn from both ends
 *
 * A person acts in this company as themself once the identity directory holds
 * them and they prove who they are; they act AS A SEAT when their row binds
 * them to one. A Tier A API token is a credential of the node it is declared
 * on, and acts as a seat only when the directory row its `token:<id>` login
 * names is bound to one. Each half alone hides the half that goes wrong — a
 * seat nobody holds looks staffed on the chart, and a token label mistyped on
 * either side leaves the token acting as itself while its operator believes it
 * acts as a seat, so these pages draw the directory from both ends: each
 * principal with the seat it holds, each human seat with whoever holds it, and
 * each of this node's tokens with the row its login names.
 *
 * # Read from the identity directory
 *
 * Six answers of `/iam`:
 *
 *  - `GET /iam/people` — every principal, walked to its last page, because a
 *    directory drawn from its first page is a company that looks smaller than
 *    it is. A row opens that principal in the frame's peek ([PersonPeek]),
 *    which reads their row (`GET /iam/people/{id}`), credentials
 *    (`GET /iam/credentials?person=`) and sessions
 *    (`GET /iam/people/{id}/sessions`) for itself.
 *  - `GET /iam/seats` — every human seat of the running company and what
 *    holds it: a person, a service account, or an open invitation.
 *  - `GET /iam/node-tokens` — THIS NODE's Tier A tokens by label, each joined
 *    to the directory row its login names, and the tokens a MANAGED company
 *    document names as its only writers (`api.auth.company_writers`,
 *    ADR-0030) — drawn on that card only when the list is not empty, since
 *    "every credential may" is the default nobody configured.
 *  - `GET /iam/check` — what the directory reports wrong, worded per kind
 *    with the engine's own detail beside it, so a kind this build does not
 *    know still reads.
 *  - `GET /iam/invitations` — the invitations nobody has redeemed and that
 *    are still good, and with `?all=true` the expired and redeemed ones the
 *    estate still holds. Never a link: it was shown once.
 *
 * # And written, by an admin
 *
 * Creating a service account and minting its token, cancelling an invitation,
 * and — in an opened person's peek — changing a login, a seat or access,
 * suspending and reactivating, resetting a second factor, issuing a password
 * reset link, ending every session, revoking one credential and removing
 * somebody. Every write is announced ([directoryMoved]), so the list and the
 * peek beside it, two trees, each read again what it moved.
 *
 * A PERSON COMES FROM A SEAT. Every person holds exactly one human seat for as
 * long as they exist, so a person comes from a vacant human seat: its card on
 * the org chart, its peek and its page offer Invite and Add person
 * (`components/people.tsx`), and this screen's Invite person picks an open
 * seat first and then opens that same invitation. A person's seat is
 * changed here, among the vacant ones, and never cleared — removing them is
 * what frees it; only a service account is unbound.
 *
 * Each is one `/iam` write through `lib/iamWrite.ts`: a
 * step-up the engine asks for is confirmed and the same request replayed, an
 * unknown answer is retried under the same operation key, and a refusal is the
 * engine's sentence with the access that would admit. After each write the
 * lists it moved are read again. The controls are drawn for an admin only,
 * who is the only reader the section opens for (`nav.ts`).
 *
 * # Labels and verifiers, never values
 *
 * No answer here has a member a credential's value could travel in — the
 * engine stores verifiers, and a Tier A token is named by its label — and this
 * screen holds none either. Where a seat is reached on a chat surface, the
 * identity is drawn as the company document writes it: a `${VAR}` is its
 * name, never the variable's value.
 *
 * # Decided by the engine, drawn as it decided
 *
 * Every read of somebody else's row is Admin's (`authz.ClassDirectory`). A
 * reader refused sees the refusal and the access that would have admitted
 * them — never an empty company. The contact identities are the company
 * document's, which takes Admin too, and a reader without it is told so
 * rather than shown seats nobody can reach.
 *
 * # Access is a line, not a list
 *
 * A row's ACCESS column says what it amounts to: "Admin" (which includes
 * every app), the apps it holds ("Worklet · Doclet"), or "Overview only",
 * through `lib/access.ts`'s one reading, so a row stored as `[admin]` and one
 * stored as `[admin, worklet, doclet, artifacts, chatlet]` read the same, as the
 * engine treats them.
 */

import { ORG_EDIT_PATH } from "~/app/routes.ts";
import { useCallback, useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Card,
  Checkbox,
  EMPTY_VALUE,
  EmptyState,
  EmptyValue,
  InlineCode,
  Menu,
  Modal,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useToast,
  Inline,
  Link,
  Text,
  Stack,
  TableKeyCell,
  TableDateCell,
  TableEntityCell,
  TableTextCell,
  PageNote,
  peekRowActivation,
} from "@crewlethq/ui";
import {
  EllipsisGlyph,
  KeyGlyph,
  SendGlyph,
  PlusGlyph,
  BotGlyph,
  ShieldGlyph,
  TriangleAlertGlyph,
  UserGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  CancelInvitation,
  canManagePeople,
  ConfirmDialog,
  DIRECTORY_READ,
  directoryMoved,
  endedWord,
  holderName,
  ExpiresCell,
  AccessTags,
  IamOutcome,
  listsNoCompany,
  MintTokenDialog,
  ShownOnce,
  useDirectoryMoved,
  useHumanSeats,
} from "~/components/people.tsx";
import { FINDING_KINDS, type HumanSeat } from "~/contract/identity.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { linkedSeat } from "~/app/frame/cells.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { asSentence, fmtDate, tsKey } from "~/lib/format.ts";
import { ADMIN, accessLine, holds, needsWords } from "~/lib/access.ts";
import { documentUnits, handleLabel, indexOrg, unitByKey } from "~/lib/seats.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { useViewer } from "~/lib/viewer.ts";
import { rest } from "~/protocol/index.ts";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";
import {
  EditPersonDialog,
  InvitePersonDialog,
  OnYourself,
  ServiceAccountDialog,
} from "./AccessDialogs.tsx";

// ---------------------------------------------------------------------------
// The wire, as internal/api/iamapi writes it
// ---------------------------------------------------------------------------

/** One directory row (`iamapi.personView`): the sealed values opened, the verifiers absent. */
export interface DirectoryRow {
  id: string;
  /** `person`, `machine`, `seat` or `engine` (`iam.Kind`). */
  kind: string;
  /** `invited`, `enrolling`, `active`, `suspended` or `retired` (`iam.Stage`). */
  stage: string;
  login?: string;
  name?: string;
  email?: string;
  /** Ciphertext this node's keyring cannot open — a STATE, never an empty name. */
  sealed?: boolean;
  /** The seat the row is bound to, by its handle. */
  seat?: string;
  grants?: string[] | null;
  created_at?: string;
}

interface DirectoryPage {
  people: DirectoryRow[] | null;
  /** The cursor of the next page, "" on the last. */
  next: string;
}

/** One credential (`iamapi.credentialView`). No verifier, ever. */
export interface CredentialRow {
  id: string;
  person: string;
  /** `password`, `totp`, `recovery`, `token` or `reset`. */
  method: string;
  label?: string;
  created_at?: string;
  expires_at?: string;
  revoked_at?: string;
  /**
   * Revoked, expired, or ended by a counter — a machine token its owner's
   * revocation epoch or a restore moved past — as the engine judged it at
   * the read.
   */
  revoked: boolean;
  /** A reset link its person used, which is revoked too. */
  spent?: boolean;
  /** What a machine token was minted carrying. */
  grants?: string[] | null;
}

/** One session (`iamapi.sessionView`): its lineage, nothing a session resumes from. */
export interface SessionRow {
  lineage: string;
  person: string;
  created_at?: string;
  expires_at?: string;
  ended_at?: string;
  ended_reason?: string;
  live: boolean;
  enrolment_only?: boolean;
}

/**
 * `GET /iam/node-tokens`: this node's Tier A tokens, and the ids of those a
 * managed company document names as its writers — empty where the document
 * is not managed.
 */
export interface NodeTokens {
  tokens: NodeToken[];
  /**
   * `api.auth.company_writers`: the Tier A token ids that alone may change
   * the company document while it is managed (ADR-0030). Every other
   * credential — a person's session, a machine token, this node's other
   * tokens — reads it, and can still rotate a credential it names.
   */
  companyWriters: string[];
}

/** One of this node's Tier A tokens, by label, joined to its directory row. */
export interface NodeToken {
  id: string;
  /** `token:<id>`, the login the token acts under. */
  login: string;
  /** `none` or `held`. */
  row: string;
  person?: string;
  stage?: string;
  /**
   * The seat the row binds the token to, by handle — what the row holds, never
   * a verdict: whether the company still holds it is `/iam/check`'s finding.
   */
  seat?: string;
}

/** One invitation (`iamapi.invitationView`): never its link, its secret or what the estate keeps of it. */
export interface InvitationRow {
  id: string;
  email?: string;
  /** The address this node's keyring cannot open — a state, never a blank. */
  sealed?: boolean;
  /** The human seat redeeming it binds, by handle. */
  seat?: string;
  grants: string[] | null;
  invited_by?: string;
  created_at?: string;
  expires_at?: string;
  redeemed_at?: string;
  /** The person a redemption created. */
  person?: string;
  /** `open`, `expired` or `redeemed`. */
  state: string;
}

interface InvitationPage {
  invitations: InvitationRow[] | null;
  next: string;
}

/** One row of `GET /iam/check`. */
export interface DirectoryFinding {
  kind: string;
  person?: string;
  login?: string;
  seat?: string;
  grant?: string;
  detail: string;
}

/**
 * `GET /iam/check`, as every screen that reads it takes it: this page's
 * Needs attention card and Home's People & access widget read ONE type, so a
 * field one of them reads is a field the other cannot lose.
 */
export interface DirectoryCheck {
  findings: DirectoryFinding[] | null;
  /**
   * Active, enrolled PERSONS holding Admin — never a service account, whose
   * token can write no administrative record — which the last-admin guard
   * reads.
   */
  people_with_admin: number;
  /**
   * Bindings this node could not check against its own chart just now, which
   * is "could not say" and never "no dangling binding".
   */
  bindings_unchecked: number;
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

type Tone = "success" | "neutral" | "warning" | "danger";

interface Words {
  label: string;
  tone: Tone;
  hint: string;
}

/**
 * The five stages, in the engine's words (`internal/iam/principal.go`). A
 * value this build has no word for is drawn as written — the engine adds one
 * additively, and naming it beats dropping it. So for every table below.
 */
export const STAGE_WORDS: Record<string, Words> = {
  invited: {
    label: "Invited",
    tone: "neutral",
    hint: "Hasn’t set a password yet.",
  },
  enrolling: {
    label: "Enrolling",
    tone: "neutral",
    hint: "Setting up sign-in.",
  },
  active: { label: "Active", tone: "success", hint: "Can sign in." },
  suspended: {
    label: "Suspended",
    tone: "warning",
    hint: "Can’t sign in.",
  },
  retired: {
    label: "Retired",
    tone: "neutral",
    hint: "Has left.",
  },
};

/** The principal kinds, as a reader says them. */
const KIND_WORDS: Record<string, string> = {
  person: "Person",
  machine: "Service account",
  seat: "Seat",
  engine: "Engine",
};

/** What a Tier A token's login names in the directory. */
export const TOKEN_ROW_WORDS: Record<string, Words> = {
  none: {
    label: "Not bound",
    tone: "neutral",
    hint: "Not bound to a seat. Bind it with crewlet iam create -kind machine -login token:<id> -seat <seat> -grants none.",
  },
  held: { label: "Yes", tone: "success", hint: "A seat entry holds this login." },
};

/**
 * The directory report's kinds in words — KEYED ON THE ENGINE'S OWN LIST
 * (`FINDING_KINDS`, held against `iamapi.FindingKinds`), so a kind the engine
 * grew is a compile error here rather than a raw code on the screen. The
 * report arrives sorted in that list's order.
 */
export const FINDING_WORDS: Record<(typeof FINDING_KINDS)[number], { label: string; tone: Tone }> =
  {
    no_admin_holder: { label: "No admin", tone: "danger" },
    person_without_credential: { label: "Can’t sign in", tone: "warning" },
    binding_dangling: { label: "Seat removed", tone: "warning" },
    admin_clamped_by_node: { label: "Admin off on this node", tone: "neutral" },
  };

/** A finding's words, or none for a kind this build does not know. */
function findingWords(kind: string): { label: string; tone: Tone } | undefined {
  return (FINDING_KINDS as readonly string[]).includes(kind)
    ? FINDING_WORDS[kind as (typeof FINDING_KINDS)[number]]
    : undefined;
}

/** What a credential method is called. */
const METHOD_WORDS: Record<string, string> = {
  password: "Password",
  totp: "Authenticator app",
  recovery: "Recovery codes",
  token: "Token",
  reset: "Password reset link",
};

/** What an invitation's state is called. */
export const INVITATION_WORDS: Record<string, Words> = {
  open: { label: "Open", tone: "success", hint: "Not used yet." },
  expired: { label: "Expired", tone: "neutral", hint: "Link expired." },
  redeemed: { label: "Used", tone: "neutral", hint: "Used to join." },
};

/**
 * The surface each contact field reaches a person on, in the chart's own
 * field order. A key this build has no word for is drawn as the key itself.
 */
const CONTACT_SURFACES: Record<string, string> = {
  slack_user_id: "Slack",
  mattermost_user_id: "Mattermost",
  atlassian_account_id: "Atlassian",
  github_login: "GitHub",
  gitlab_username: "GitLab",
};

/**
 * A state in a list's column: QUIET WORDS while it is the resting one, a pill
 * only when it is news. "Active" on every person and every service account,
 * and a binding state on every token, were pills down whole columns that a
 * reader learned to skip, so the row that differs was the one they missed.
 */
function StateCell({
  words,
  fallback,
  resting,
}: {
  words: Words | undefined;
  fallback: string;
  resting: boolean;
}) {
  if (resting) {
    return (
      <span title={words?.hint}>
        <TableTextCell>{words?.label ?? fallback}</TableTextCell>
      </span>
    );
  }
  return <WordTag words={words} fallback={fallback} />;
}

function WordTag({ words, fallback }: { words: Words | undefined; fallback: string }) {
  return (
    <Tag size="sm" variant={words?.tone ?? "neutral"} title={words?.hint}>
      {words?.label ?? fallback}
    </Tag>
  );
}

/**
 * What comes next in a company nobody has joined: put yourself on your seat —
 * every person holds a human seat, so the org chart is where a person comes
 * from — or, once an invitation is open, open its link, which is where its
 * person chooses a login and password.
 *
 * THE SEAT LISTING DECIDES THE STEP, so it is read whole rather than as its
 * rows. A node running NO COMPANY answers it `409 no_active_revision`, and
 * the step is creating one that declares a human seat: read as its rows, the
 * refusal was an empty answer taken for a company holding seats, and the
 * callout sent the operator to "choose your human seat" on a chart with no
 * company behind it. A company with NO HUMAN SEAT can admit nobody, and is
 * told so with the way to add one. A listing refused for any other reason
 * says nothing about the seats, so the callout claims nothing about them.
 */
/** The one next step of a company nobody has joined: what to say, and its control. */
interface FirstStep {
  body: React.ReactNode;
  action?: React.ReactNode;
}

function firstPersonStep(waiting: InvitationRow[], seats: RestResult<HumanSeat[]>): FirstStep {
  if (waiting.length > 0) {
    return { body: "Lost the invitation link? Cancel it and invite again." };
  }
  if (listsNoCompany(seats)) {
    return {
      body: "No company yet. Create one in Edit org.",
      action: (
        <ButtonLink size="small" variant="primary" href={href(ORG_EDIT_PATH)}>
          Create the company
        </ButtonLink>
      ),
    };
  }
  if (seats.data && seats.data.length === 0) {
    return {
      body: "No people in the chart yet. Add one in Edit org.",
      action: (
        <ButtonLink size="small" variant="primary" href={href(ORG_EDIT_PATH, { add: "human" })}>
          Add a human seat
        </ButtonLink>
      ),
    };
  }
  const chart = (
    <ButtonLink size="small" variant="primary" href={href(["company", "org"])}>
      Open the org chart
    </ButtonLink>
  );
  if (!seats.data) {
    return {
      body: (
        <>
          This company has no person in it, so nobody can sign in with a password yet. Every person
          is invited or created on a human seat in the org chart; this node could not list the seats
          just now, so Human seats below says why. The API token you are using stays the way back
          in.
        </>
      ),
      action: chart,
    };
  }
  return { body: "Invite yourself from your seat in the org chart.", action: chart };
}

/** The first-run step as a callout, over a directory that already lists somebody. */
function FirstPerson({ step }: { step: FirstStep }) {
  return (
    <Callout variant="neutral" title={FIRST_RUN_TITLE} action={step.action}>
      {step.body}
    </Callout>
  );
}

/** What a company nobody has joined is told, wherever the step is drawn. */
const FIRST_RUN_TITLE = "Nobody has joined yet";

/**
 * Whether a read has said something — answered or been refused — rather than
 * being on its way for the first time. A step decided before then is decided
 * on nothing, and says the wrong one first.
 */
function settled(read: RestResult<unknown>): boolean {
  return !(read.loading && read.data === null && read.error === null);
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

/** A page as large as the directory serves (`iamdomain.MaxPageSize`). */
const PAGE = 200;

/** The access the company document — and so every contact identity — is read under. */
const CONFIG_READ = ADMIN;

/** Every row of the directory, walked to its last page. */
async function readDirectory(signal: AbortSignal): Promise<DirectoryRow[]> {
  return walk(
    (query) => rest.get(`/iam/people?${query}`, signal) as Promise<DirectoryPage | null>,
    "",
    (page) => page.people,
  );
}

/** Every invitation the listing holds — the open ones, or with `all` every state. */
function readInvitations(all: boolean) {
  return (signal: AbortSignal) =>
    walk(
      (query) => rest.get(`/iam/invitations?${query}`, signal) as Promise<InvitationPage | null>,
      all ? "all=true" : "",
      (page) => page.invitations,
    );
}

/**
 * A paged `/iam` listing, walked to its last page: `read` asks one page with
 * the query it is handed — its own path a literal at its call, which is what
 * the dev proxy's gate reads.
 */
async function walk<T, P extends { next: string }>(
  read: (query: string) => Promise<P | null>,
  extra: string,
  rowsOf: (page: P) => T[] | null,
): Promise<T[]> {
  const rows: T[] = [];
  let after = "";
  for (;;) {
    const params = new URLSearchParams(extra);
    params.set("limit", String(PAGE));
    if (after) params.set("after", after);
    const page = await read(params.toString());
    rows.push(...((page && rowsOf(page)) ?? []));
    // THE CURSOR MUST MOVE: a page that names its own cursor again would
    // otherwise be asked for ever.
    if (!page?.next || page.next === after) return rows;
    after = page.next;
  }
}

/** The list an `/iam` answer carries under `field`, or none. */
function listOf<T>(answer: unknown, field: string): T[] {
  const list = (answer as Record<string, unknown> | null)?.[field];
  return Array.isArray(list) ? (list as T[]) : [];
}

/** `GET /iam/node-tokens`, read whole: the tokens and the managed document's writers. */
async function readNodeTokens(signal: AbortSignal): Promise<NodeTokens> {
  const answer = await rest.get("/iam/node-tokens", signal);
  return {
    tokens: listOf<NodeToken>(answer, "tokens"),
    companyWriters: listOf<string>(answer, "company_writers"),
  };
}

/** How every `/iam` read here is kept current: as every directory read is. */
const READ = DIRECTORY_READ;

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/**
 * WHOSE A DIRECTORY ROW IS, as the pages split the directory: a service
 * account (`machine`) is Settings › Service accounts', and every other
 * principal (a person, a seat's own principal, the engine's) is Settings ›
 * People's. One rule, so no row is drawn on both pages and none on neither.
 */
function isServiceAccount(row: Pick<DirectoryRow, "kind">): boolean {
  return row.kind === "machine";
}

/**
 * Which page a directory finding belongs on: the page of the principal it
 * names, or People where it names none (nobody holding Admin is a fact about
 * the people who could).
 */
function findingOn(
  f: DirectoryFinding,
  byId: ReadonlyMap<string, DirectoryRow>,
): "people" | "service-accounts" {
  const row = f.person ? byId.get(f.person) : undefined;
  return row && isServiceAccount(row) ? "service-accounts" : "people";
}

/**
 * The directory and its report, as People and Service accounts both read
 * them, each read again after any write that may have moved it.
 *
 * A WRITE IS ANNOUNCED ([directoryMoved]) rather than re-read where it is
 * made, because the person peek beside a list is another tree: an edit or a
 * removal made there left the list naming what it had replaced until the next
 * poll.
 */
function useDirectory() {
  const directory = useRest("/iam/people", readDirectory, READ);
  const check = useRest(
    "/iam/check",
    async (signal) => (await rest.get("/iam/check", signal)) as DirectoryCheck | null,
    READ,
  );
  const { reload: reloadDirectory } = directory;
  const { reload: reloadCheck } = check;
  useDirectoryMoved(
    useCallback(() => {
      for (const reload of [reloadDirectory, reloadCheck]) void reload({ quiet: true });
    }, [reloadDirectory, reloadCheck]),
  );
  return { directory, check };
}

/**
 * A list's rows open their principal in the frame's peek, beside the list on
 * a wide window and as a drawer on a phone, and `[` and `]` step through them
 * in the order the grid draws them.
 */
function usePrincipalPeek(rows: readonly DirectoryRow[]) {
  const { open } = usePeekControls();
  usePeekNeighbours(useMemo(() => rows.map((p) => personRef(p.id)), [rows]));
  return (row: DirectoryRow) => open(personRef(row.id));
}

/** A refused or loading directory, drawn alike on both pages that read it. */
function DirectoryState({ directory }: { directory: RestResult<DirectoryRow[]> }) {
  return (
    <>
      {/* A REFUSED DIRECTORY IS NOT AN EMPTY COMPANY: every read here is
          decided on the same grant, so the first refusal is the whole page's,
          drawn with the grants that would have admitted the reader. */}
      {directory.code && !directory.data && (
        <QueryState
          error={directory.code}
          refusal={directory.refusal}
          detail={directory.error?.detail || undefined}
          loading={false}
        />
      )}
      {directory.loading && !directory.data && (
        <Skeleton variant="text" rows={6} label="Loading the directory" />
      )}
    </>
  );
}

/** The directory's report, narrowed to the findings that belong on this page ([findingOn]). */
function checkFor(
  check: RestResult<DirectoryCheck | null>,
  page: "people" | "service-accounts",
  byId: ReadonlyMap<string, DirectoryRow>,
): RestResult<DirectoryCheck | null> {
  if (!check.data) return check;
  const findings = (check.data.findings ?? []).filter((f) => findingOn(f, byId) === page);
  return { ...check, data: { ...check.data, findings } };
}

/**
 * Settings › People: who is in the directory, the invitations still open,
 * and the human seats they come from.
 */
export function People() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const health = useEngineHealth();
  const manages = canManagePeople(viewer.grants);
  const nav = useNavigator();
  const [inviting, setInviting] = useState(false);
  const [allInvitations, setAllInvitations] = useState(false);

  const { directory, check } = useDirectory();
  // THE ONE READING OF THE SEATS every seat surface takes, so a gesture made
  // on a seat elsewhere on the page, or this page's own, is heard here too.
  const seats = useHumanSeats(true);
  const invitations = useRest(
    `/iam/invitations?all=${allInvitations}`,
    readInvitations(allInvitations),
    READ,
  );
  const { reload: reloadInvitations } = invitations;
  useDirectoryMoved(
    useCallback(() => void reloadInvitations({ quiet: true }), [reloadInvitations]),
  );
  // THE CONTACT IDENTITIES ARE THE COMPANY DOCUMENT'S, which takes Admin. A
  // reader without it is never asked: the refusal is known before the
  // question, and each seat says what it would take instead.
  const readsConfig = holds(viewer.grants, CONFIG_READ);
  const config = useQuery("config", undefined, { enabled: readsConfig });
  const contacts = useMemo(() => contactsByHandle(config.data), [config.data]);

  const everyone = useMemo(() => directory.data ?? [], [directory.data]);
  const byId = useMemo(() => new Map(everyone.map((p) => [p.id, p])), [everyone]);
  const people = useMemo(() => everyone.filter((p) => !isServiceAccount(p)), [everyone]);
  const openPeek = usePrincipalPeek(people);
  const active = people.filter((p) => p.stage === "active").length;
  const seatRows = useMemo(() => seats.data ?? [], [seats.data]);
  // VACANT IS HELD BY NOTHING: an open invitation holds its seat as surely as
  // a person does, and counted as vacant it read as a seat an administrator
  // could still invite somebody onto.
  const vacant = seatRows.filter((s) => !s.holder && !s.invitation).length;
  const invited = seatRows.filter((s) => !s.holder && s.invitation).length;
  // FIRST RUN: nobody has joined, and the first-person step names the one
  // next step. See [firstPersonStep].
  const firstRun = health?.identity === "unclaimed" && settled(invitations) && settled(seats);
  const firstStep = firstRun
    ? firstPersonStep(
        (invitations.data ?? []).filter((i) => i.state === "open"),
        seats,
      )
    : null;
  // NOBODY LISTED: the People card's empty state IS the step. Over an empty
  // directory the callout, two stat tiles of zero and the card's own "Nobody's
  // here yet" said one thing four times before any data.
  const nobody = directory.data !== null && people.length === 0;

  return (
    <>
      <PageActions>
        {/* INVITING IS THE PAGE'S MAIN TASK, so it leads: it picks an open
            seat and opens that seat's invitation, since a person still comes
            from a seat (see the file's doc). */}
        {/* ONE PRIMARY ACTION FOR ONE STEP: while the first-person callout
            is drawn its button is the step, and this one steps back. */}
        {manages && (
          <Button
            variant={firstRun ? "secondary" : "primary"}
            size="small"
            leadingIcon={<SendGlyph size="sm" />}
            onClick={() => setInviting(true)}
          >
            Invite person
          </Button>
        )}
        {/* WHERE THIS PAGE LEADS, folded: the trail of identity changes, and
            the chart, where a seat and its contacts are written. */}
        <span className="page-action-folds">
          <Menu
            label="More on People"
            icon={<EllipsisGlyph size="sm" />}
            triggerVariant="ghost"
            align="end"
            items={[
              {
                key: "trail",
                label: "Identity trail",
                onSelect: () => nav.to(["settings", "audit"], { kind: "identity" }),
              },
              { key: "edit", label: "Edit org", onSelect: () => nav.to(ORG_EDIT_PATH) },
            ]}
          />
        </span>
      </PageActions>
      <PageNote>
        {manages
          ? "Invite people from their seat in the org chart."
          : needsWords("Changing any of this", [ADMIN])}
      </PageNote>
      {inviting && (
        <InvitePersonDialog
          held={viewer.grants}
          onClose={() => setInviting(false)}
          onDone={directoryMoved}
        />
      )}

      {/* FIRST RUN: nobody has JOINED (`/health`'s `unclaimed` says no person
          is enrolled, and an invitation creates its person only when it is
          redeemed) and somebody signed in with an API token is reading this.
          Which step is next turns on the invitations and the seats: with an
          invitation open, its link; with none, the seat to put yourself on,
          or, with no human seat at all, adding one. Drawn once the
          invitations AND the seats have answered or been refused, so it never
          says the wrong one first. */}
      {firstStep && !nobody && <FirstPerson step={firstStep} />}

      <DirectoryState directory={directory} />

      {directory.data && (
        <>
          {!nobody && (
            <Card padding="none">
              <StatGroup columns={2}>
                <StatCard
                  icon={<UsersGlyph size="xs" />}
                  label="People"
                  value={people.length}
                  sub={`${active} active`}
                />
                <StatCard
                  icon={<TriangleAlertGlyph size="xs" />}
                  label="Open seats"
                  value={seats.data ? vacant : EMPTY_VALUE}
                  sub={
                    seats.data
                      ? `${invited > 0 ? `${invited} invited · ` : ""}${seatRows.length} total`
                      : listsNoCompany(seats)
                        ? "no company yet"
                        : "not read"
                  }
                />
              </StatGroup>
            </Card>
          )}

          <Findings
            check={checkFor(check, "people", byId)}
            people={everyone}
            manages={manages}
            held={viewer.grants}
          />

          <Card padding="none">
            <Card.Header icon={<UsersGlyph size="sm" />} count={people.length}>
              <Card.Title>People</Card.Title>
            </Card.Header>
            <DataGrid<DirectoryRow>
              rows={people}
              rowKey={(p) => p.id}
              defaultSort="person"
              // A ROW IS A REAL LINK AND A PLAIN CLICK PEEKS (the frame's one
              // rule), and the grid marks the row the peek is open on.
              rowHref={(p) => peekHref(personRef(p.id))}
              onRowActivate={peekRowActivation(openPeek)}
              // THE HINT ADDS WHAT THE PAGE NOTE DOES NOT: it repeated the note
              // word for word 530px below it.
              empty={
                firstStep
                  ? { title: FIRST_RUN_TITLE, hint: firstStep.body, action: firstStep.action }
                  : {
                      title: "Nobody’s here yet",
                      hint: "A person is listed once they accept their invitation, or are created on their seat.",
                    }
              }
              columns={[
                {
                  key: "person",
                  header: "Name",
                  floor: "12rem",
                  sortValue: (p) => p.name || p.login || p.id,
                  cell: (p) => <PersonName row={p} />,
                },
                {
                  key: "login",
                  header: "Login",
                  shrink: true,
                  // LAST TO GO, and it does go: the name column draws the
                  // login of anybody with no name, and the peek beside the
                  // list leads with it.
                  drop: 3,
                  sortValue: (p) => p.login ?? "",
                  cell: (p) =>
                    p.login ? <TableKeyCell value={p.login} /> : <EmptyValue label="No login" />,
                },
                {
                  key: "kind",
                  header: "Kind",
                  shrink: true,
                  drop: 2,
                  sortValue: (p) => p.kind,
                  cell: (p) => <TableTextCell>{KIND_WORDS[p.kind] ?? p.kind}</TableTextCell>,
                },
                {
                  key: "stage",
                  header: "Stage",
                  shrink: true,
                  sortValue: (p) => p.stage,
                  cell: (p) => (
                    <StateCell
                      words={STAGE_WORDS[p.stage]}
                      fallback={p.stage}
                      resting={p.stage === "active"}
                    />
                  ),
                },
                {
                  key: "seat",
                  header: "Seat",
                  // A FLOOR, AS EVERY COLUMN THIS LIST KEEPS HAS ONE: beside
                  // the person peek the list is a column of the sheet, and a
                  // flexible track at zero gave way to every content-sized
                  // one. With floors the tracks overrun instead, and the grid
                  // drops by `drop`.
                  floor: "8rem",
                  sortValue: (p) => p.seat ?? "",
                  cell: (p) => <SeatHandle seat={p.seat} index={index} />,
                },
                {
                  key: "grants",
                  header: "Access",
                  floor: "8rem",
                  drop: 1,
                  sortValue: (p) => accessLine(p.grants ?? []),
                  cell: (p) => <TableTextCell>{accessLine(p.grants ?? [])}</TableTextCell>,
                },
              ]}
            />
          </Card>

          <Invitations
            invitations={invitations}
            all={allInvitations}
            onAll={setAllInvitations}
            manages={manages}
            index={index}
            onChanged={directoryMoved}
          />

          <Card padding="none">
            <Card.Header icon={<UserGlyph size="sm" />} count={seatRows.length}>
              <Card.Title>Human seats</Card.Title>
            </Card.Header>
            {/* NO COMPANY IS NOT A FAILURE: the listing's 409 is the node
                saying it has no seat to list yet, which the generic refusal
                read as "the engine failed to answer". */}
            {listsNoCompany(seats) ? (
              <Callout variant="neutral">
                This node runs no company yet, so it has no human seat to list. Seats appear here
                once a company that declares them is created or imported.
              </Callout>
            ) : (
              <QueryState
                error={seats.code}
                refusal={seats.refusal}
                detail={seats.error?.detail || undefined}
                loading={seats.loading && !seats.data}
              >
                <DataGrid<HumanSeat>
                  rows={seatRows}
                  rowKey={(s) => s.handle}
                  defaultSort="seat"
                  empty={{ title: "The running company has no human seats" }}
                  columns={[
                    {
                      key: "seat",
                      header: "Seat",
                      floor: "12rem",
                      sortValue: (s) => s.name || s.handle,
                      cell: (s) => (
                        <TableEntityCell
                          {...linkedSeat({ handle: s.handle, name: s.name, kind: "human" })}
                        />
                      ),
                    },
                    {
                      key: "unit",
                      header: "Team",
                      shrink: true,
                      drop: 2,
                      sortValue: (s) => (s.unit ? (unitByKey(index, s.unit)?.name ?? s.unit) : ""),
                      cell: (s) => {
                        if (!s.unit) return <EmptyValue label="No team" />;
                        return (
                          <TableTextCell>{unitByKey(index, s.unit)?.name ?? s.unit}</TableTextCell>
                        );
                      },
                    },
                    {
                      key: "holder",
                      header: "Held by",
                      sortValue: (s) =>
                        s.holder?.login ?? s.holder?.person ?? s.invitation?.email ?? "",
                      cell: (s) => <Holder seat={s} />,
                    },
                    {
                      key: "reach",
                      header: "Reached on",
                      drop: 1,
                      cell: (s) =>
                        readsConfig ? (
                          <Contacts contact={contacts.get(s.handle)} />
                        ) : (
                          <EmptyValue
                            label={needsWords("Reading where a seat is reached", [CONFIG_READ])}
                          />
                        ),
                    },
                  ]}
                />
              </QueryState>
            )}
          </Card>
        </>
      )}
    </>
  );
}

/**
 * Settings › Service accounts: the machine principals, what each holds and the
 * seat it acts as. A service account signs in with a token it was minted, and
 * is created here rather than from a seat, since it may hold none.
 */
export function ServiceAccounts() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const manages = canManagePeople(viewer.grants);
  const [creating, setCreating] = useState(false);
  const { directory, check } = useDirectory();
  const everyone = useMemo(() => directory.data ?? [], [directory.data]);
  const byId = useMemo(() => new Map(everyone.map((p) => [p.id, p])), [everyone]);
  const accounts = useMemo(() => everyone.filter(isServiceAccount), [everyone]);
  const openPeek = usePrincipalPeek(accounts);
  const bound = accounts.filter((a) => a.seat).length;

  return (
    <>
      <PageActions>
        {manages && (
          <Button
            variant="primary"
            size="small"
            leadingIcon={<PlusGlyph size="sm" />}
            onClick={() => setCreating(true)}
          >
            New service account
          </Button>
        )}
      </PageActions>
      <PageNote>
        {manages
          ? "A service account signs in with its own token, and acts as the seat it is bound to."
          : needsWords("Changing any of this", [ADMIN])}
      </PageNote>
      {creating && (
        <ServiceAccountDialog
          held={viewer.grants}
          onClose={() => setCreating(false)}
          onDone={directoryMoved}
        />
      )}

      <DirectoryState directory={directory} />

      {directory.data && (
        <>
          <Findings
            check={checkFor(check, "service-accounts", byId)}
            people={everyone}
            manages={manages}
            held={viewer.grants}
          />
          <Card padding="none">
            {/* HOW MANY ACT AS A SEAT is the card's fact, said in its head
                rather than as a footnote floating inside the card under the
                table. */}
            <Card.Header
              icon={<BotGlyph size="sm" />}
              count={accounts.length}
              subtitle={
                accounts.length > 0 ? `${bound} of ${accounts.length} act as a seat` : undefined
              }
            >
              <Card.Title>Service accounts</Card.Title>
            </Card.Header>
            <DataGrid<DirectoryRow>
              rows={accounts}
              rowKey={(p) => p.id}
              defaultSort="account"
              rowHref={(p) => peekHref(personRef(p.id))}
              onRowActivate={peekRowActivation(openPeek)}
              empty={{
                title: "No service accounts yet",
                hint: manages
                  ? "Create one for an automation that signs in on its own."
                  : "An admin creates one for an automation that signs in on its own.",
              }}
              columns={[
                {
                  key: "account",
                  header: "Name",
                  floor: "12rem",
                  sortValue: (p) => p.name || p.login || p.id,
                  cell: (p) => <PersonName row={p} />,
                },
                {
                  key: "login",
                  header: "Login",
                  shrink: true,
                  drop: 2,
                  sortValue: (p) => p.login ?? "",
                  cell: (p) =>
                    p.login ? <TableKeyCell value={p.login} /> : <EmptyValue label="No login" />,
                },
                {
                  key: "stage",
                  header: "Stage",
                  shrink: true,
                  sortValue: (p) => p.stage,
                  cell: (p) => (
                    <StateCell
                      words={STAGE_WORDS[p.stage]}
                      fallback={p.stage}
                      resting={p.stage === "active"}
                    />
                  ),
                },
                // SIZED TO WHAT THEY HOLD, so the name takes the spare width:
                // as two more flexible tracks, Acts as held a dash in 226px
                // while the name beside it was cut at 174.
                {
                  key: "seat",
                  header: "Acts as",
                  shrink: true,
                  sortValue: (p) => p.seat ?? "",
                  cell: (p) => <SeatHandle seat={p.seat} index={index} />,
                },
                {
                  key: "grants",
                  header: "Access",
                  shrink: true,
                  drop: 1,
                  sortValue: (p) => accessLine(p.grants ?? []),
                  cell: (p) => <TableTextCell>{accessLine(p.grants ?? [])}</TableTextCell>,
                },
              ]}
            />
          </Card>
        </>
      )}
    </>
  );
}

/**
 * Settings › API tokens: THIS NODE's Tier A tokens, by label, each joined to
 * the directory row its `token:<id>` login names, and, where the company
 * document is managed (ADR-0030), the tokens that alone may write it.
 */
export function ApiTokens() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const tokens = useRest("/iam/node-tokens", readNodeTokens, READ);
  const { reload } = tokens;
  useDirectoryMoved(useCallback(() => void reload({ quiet: true }), [reload]));
  const tokenRows = useMemo(() => tokens.data?.tokens ?? [], [tokens.data]);
  const companyWriters = tokens.data?.companyWriters ?? [];
  return (
    <>
      <PageNote>
        Declared under <InlineCode>api.auth.tokens</InlineCode> in this node&rsquo;s bootstrap file,
        and changed with a restart. A person&rsquo;s own tokens are on their Account.
      </PageNote>
      <Card padding="none">
        <Card.Header
          icon={<KeyGlyph size="sm" />}
          count={tokens.data ? tokenRows.length : undefined}
        >
          <Card.Title>API tokens on this node</Card.Title>
        </Card.Header>
        {companyWriters.length > 0 && (
          <Card.Body padding="md">
            <CompanyWriters writers={companyWriters} />
          </Card.Body>
        )}
        <QueryState
          error={tokens.code}
          refusal={tokens.refusal}
          detail={tokens.error?.detail || undefined}
          loading={tokens.loading && !tokens.data}
        >
          <DataGrid<NodeToken>
            rows={tokenRows}
            rowKey={(t) => t.id}
            defaultSort="id"
            empty={{
              title: "This node declares no API token",
              hint: "Tokens are set in api.auth.tokens in the bootstrap file and change with a restart.",
            }}
            columns={[
              // IDENTIFIERS ARE NEVER CUT: a token's label and the login it
              // signs in as are what an operator matches against the
              // bootstrap file, and they read "token:fo…" beside a Seat column
              // holding a dash in most of the grid. Their tracks are their
              // own width; the seat takes what is left.
              {
                key: "id",
                header: "Label",
                width: "max-content",
                sortValue: (t) => t.id,
                cell: (t) => <TableKeyCell value={t.id} />,
              },
              // THE LOGIN TAKES THE SPARE WIDTH, never cut (its floor is its
              // content): a Seat column holding a dash took 530px of it.
              {
                key: "login",
                header: "Signs in as",
                floor: "max-content",
                sortValue: (t) => t.login,
                cell: (t) => <TableKeyCell value={t.login} />,
              },
              {
                key: "row",
                header: "Seat entry",
                shrink: true,
                sortValue: (t) => t.row,
                cell: (t) => (
                  <StateCell
                    words={TOKEN_ROW_WORDS[t.row]}
                    fallback={t.row}
                    resting={t.row === "none"}
                  />
                ),
              },
              // A SEAT COLUMN ONLY WHERE A TOKEN IS BOUND TO ONE.
              ...(tokenRows.some((t) => t.seat)
                ? [
                    {
                      key: "seat",
                      header: "Seat",
                      shrink: true,
                      sortValue: (t: NodeToken) => t.seat ?? "",
                      cell: (t: NodeToken) => <BoundSeat seat={t.seat} index={index} />,
                    },
                  ]
                : []),
            ]}
          />
        </QueryState>
      </Card>
    </>
  );
}

// ---------------------------------------------------------------------------
// Parts
// ---------------------------------------------------------------------------

/**
 * WHO MAY CHANGE THE COMPANY, where a managed document says (ADR-0030).
 *
 * Part of what a Tier A token is: the deployment's `api.auth` names the tokens
 * that alone write a managed document, and this is the screen an operator
 * reads that posture on. Worded for EVERY credential rather than "every other
 * token", because a person's session and a machine token are refused
 * `config_managed` exactly as an unlisted token is — the managed check judges
 * the credential, whatever grants stand behind it.
 */
function CompanyWriters({ writers }: { writers: readonly string[] }) {
  return (
    <Callout variant="neutral" icon={<ShieldGlyph size="md" />}>
      <span>
        Only{" "}
        {writers.map((id, i) => (
          <span key={id}>
            {i > 0 && ", "}
            <InlineCode>{id}</InlineCode>
          </span>
        ))}{" "}
        can change the company document.
      </span>
    </Callout>
  );
}

/**
 * Every seat's contact identities, by handle, from the company document —
 * top-level seats and every unit's, as `seatSettings` walks them.
 */
function contactsByHandle(
  doc: CompanyDocument | null | undefined,
): Map<string, ConfigRole["contact"]> {
  const out = new Map<string, ConfigRole["contact"]>();
  const roles: ConfigRole[] = [...(doc?.roles ?? [])];
  for (const unit of documentUnits(doc)) roles.push(...(unit.roles ?? []));
  for (const role of roles) if (role.handle) out.set(role.handle, role.contact);
  return out;
}

/**
 * A principal's name. A SEALED value is a state the operator can end by
 * putting a key back, never a blank — a blank reads as somebody who never
 * gave a name.
 */
function PersonName({ row }: { row: DirectoryRow }) {
  if (row.sealed) return <SealedTag />;
  const name = row.name || row.login || row.id;
  // THE BADGE IS THE PERSON'S, beside their name: in the Seat column it
  // repeated the name a cell over ("Lee Park  LP Lee Park"). A service account
  // wears a key, since the initials of a credential's label mean nothing.
  return (
    <Inline gap={2}>
      <SeatAvatar
        name={name}
        size="xs"
        kind="human"
        icon={row.kind === "person" ? undefined : <KeyGlyph size="sm" />}
        decorative
      />
      <TableTextCell>{name}</TableTextCell>
    </Inline>
  );
}

/**
 * What holds a seat: its holder by login and stage, the open invitation that
 * names it, or the plain fact that nothing does. An INVITED seat is not a
 * vacant one — nothing else may be invited onto it or bound to it until the
 * invitation is redeemed, cancelled or lapses — so it is never drawn as
 * "Nobody holds it".
 */
function Holder({ seat }: { seat: HumanSeat }) {
  const { holder, invitation } = seat;
  if (!holder && invitation) {
    return (
      <Inline as="span" gap={1}>
        <Tag size="sm" variant="neutral" title={`Open invitation ${invitation.id}`}>
          Invited
        </Tag>
        {invitation.sealed ? (
          <SealedTag />
        ) : invitation.email ? (
          <TableTextCell>{invitation.email}</TableTextCell>
        ) : null}
      </Inline>
    );
  }
  // OPEN SEAT, IN WORDS, as the chart's cards and the stat tile say it: a
  // seat nothing holds is one an admin can invite somebody onto, which a dash
  // did not say.
  if (!holder) return <TableTextCell>Open seat</TableTextCell>;
  // THE HOLDER BY NAME, as plain text. Status colour is reserved for status, so
  // the stage is a tag of its own, and only where it is news: an active holder
  // is the ordinary case.
  const stage = holder.stage ?? "";
  return (
    <Inline gap={1} wrap>
      <TableTextCell>{holderName(holder)}</TableTextCell>
      {stage !== "active" && <WordTag words={STAGE_WORDS[stage]} fallback={stage} />}
    </Inline>
  );
}

/** Where agents reach a seat: one chip per field, as the document writes it. */
function Contacts({ contact }: { contact: ConfigRole["contact"] }) {
  const entries = Object.entries(contact ?? {}).filter(([, value]) => value);
  if (entries.length === 0) return <EmptyValue label="No surface" />;
  return (
    <Inline as="span" gap={1} wrap>
      {entries.map(([key, value]) => (
        <Tag key={key} size="sm" appearance="outline" title={`${key}: ${value}`}>
          {CONTACT_SURFACES[key] ?? key}{" "}
          <Text variant="inherit" mono>
            {value}
          </Text>
        </Tag>
      ))}
    </Inline>
  );
}

/**
 * The seat a directory row binds — a person's or a token's — or the plain fact
 * that it binds none. What the row holds, never a verdict on it: a seat the
 * company no longer holds is the report's finding, below.
 *
 * THE KIND IS THE CHART'S, never assumed: a row bound to an AGENT seat is the
 * residue the report names, and drawn with a person's badge this cell would
 * state the opposite of the chart on the row the report calls dangling. A seat
 * the chart does not hold has no kind, which the cell draws as its default.
 *
 * BOUND TO NONE is a service account's or a token's row acting as itself,
 * which is ordinary: a person always holds a human seat (ADR-0032).
 */
function BoundSeat({ seat, index }: { seat?: string; index: ReturnType<typeof indexOrg> }) {
  if (!seat) return <EmptyValue label="No seat" />;
  const held = index.byHandle.get(seat);
  return (
    <TableEntityCell
      {...linkedSeat({ handle: seat, name: held?.name ?? seat, kind: held?.kind })}
    />
  );
}

/** A finding's fix, opened from its row: the dialog the person's peek opens for it. */
type FindingFix = { gesture: "mint" | "reset"; row: DirectoryRow };

/**
 * The seat a PERSON's row binds, by its own identifier: its `@handle`, linked
 * to its page, with its name on the tooltip. In the People table the seat's
 * name and badge echoed the person's a cell over ("Lee Park  LP Lee Park"),
 * so the badge is the person's, beside their name, and the seat is its handle.
 */
function SeatHandle({ seat, index }: { seat?: string; index: ReturnType<typeof indexOrg> }) {
  if (!seat) return <EmptyValue label="No seat" />;
  return (
    <Link
      href={href(["company", "org", "seats", seat])}
      size="inherit"
      variant="subtle"
      title={index.byHandle.get(seat)?.name}
    >
      <Text variant="cell" mono>
        {handleLabel(seat)}
      </Text>
    </Link>
  );
}

/**
 * What the directory reports wrong, each kind written here in a short
 * sentence about the person it names, with the fix beside it where there is
 * one to make: a token for a service account that has none, a reset link for
 * a person who cannot sign in. The engine's own detail named an API path; it
 * is kept only for a kind this build has no words for.
 */
function Findings({
  check,
  people,
  manages,
  held,
}: {
  check: RestResult<DirectoryCheck | null>;
  /** The directory, to name each finding's person and open its fix. */
  people: readonly DirectoryRow[];
  manages: boolean;
  held: readonly string[];
}) {
  const reader = useFrameSession().answer?.person;
  const [fixing, setFixing] = useState<FindingFix | null>(null);
  const findings = check.data?.findings ?? [];
  const unchecked = check.data?.bindings_unchecked ?? 0;
  const byId = new Map(people.map((p) => [p.id, p]));
  // NOTHING TO SAY IS NO CARD: "Needs attention 0 / All good." was a whole
  // bordered section above the one list the page is about. Drawn when there
  // is a finding, a binding that could not be checked, or a check that
  // failed; a check still running draws nothing until it has said.
  if (!check.code && findings.length === 0 && unchecked === 0) return null;
  return (
    <Card padding="none">
      <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={findings.length}>
        <Card.Title>Needs attention</Card.Title>
      </Card.Header>
      <QueryState
        error={check.code}
        refusal={check.refusal}
        detail={check.error?.detail || undefined}
        loading={check.loading && !check.data}
      >
        <Card.Body padding="md">
          <Stack gap={2}>
            {findings.map((f, i) => {
              const words = findingWords(f.kind);
              const row = f.person ? byId.get(f.person) : undefined;
              const fix = manages && row ? findingFix(f, row) : null;
              return (
                <Inline key={`${f.kind}:${f.person ?? f.seat ?? i}`} gap={2} wrap>
                  <Tag size="sm" variant={words?.tone ?? "neutral"}>
                    {words?.label ?? f.kind}
                  </Tag>
                  <Text variant="body">{findingSentence(f, row)}</Text>
                  {fix && row && (
                    <Button
                      size="small"
                      variant="secondary"
                      onClick={() => setFixing({ gesture: fix.gesture, row })}
                    >
                      {fix.label}
                    </Button>
                  )}
                </Inline>
              );
            })}
            {/* SAID RATHER THAN SILENT: "no dangling binding" and "this node
              could not say" are different answers. */}
            {unchecked > 0 && (
              <Text as="p" variant="caption">
                {unchecked} binding{unchecked === 1 ? "" : "s"} could not be checked against this
                node&rsquo;s chart just now.
              </Text>
            )}
          </Stack>
        </Card.Body>
      </QueryState>
      {fixing?.gesture === "mint" && (
        <MintTokenDialog
          owner={fixing.row}
          held={held}
          onClose={() => setFixing(null)}
          onDone={directoryMoved}
        />
      )}
      {fixing?.gesture === "reset" && (
        <ResetLink
          row={fixing.row}
          who={principalName(fixing.row)}
          you={fixing.row.id === reader}
          onClose={() => setFixing(null)}
          onDone={directoryMoved}
        />
      )}
    </Card>
  );
}

/** What a panel or a sentence calls a principal: a sealed name is never drawn. */
function principalName(row: DirectoryRow): string {
  return row.sealed ? row.login || row.id : row.name || row.login || row.id;
}

/**
 * A finding as a person reads it, about the principal it names. A kind this
 * build has no words for keeps the engine's own sentence.
 */
function findingSentence(f: DirectoryFinding, row: DirectoryRow | undefined): string {
  const name = row ? principalName(row) : f.login || "Someone";
  switch (f.kind) {
    case "no_admin_holder":
      return "Nobody active can manage people. Give someone Admin.";
    case "person_without_credential":
      return row?.kind === "machine"
        ? `${name} has no token, so it can’t sign in.`
        : `${name} has no password, so they can’t sign in.`;
    case "binding_dangling":
      return `${name} is on a seat that was removed. Move them to another seat.`;
    case "admin_clamped_by_node":
      return `${name} has Admin, but this node turns it off.`;
  }
  return asSentence(f.detail);
}

/** The one-click repair a finding offers, where it has one. */
function findingFix(
  f: DirectoryFinding,
  row: DirectoryRow,
): { gesture: FindingFix["gesture"]; label: string } | null {
  if (f.kind !== "person_without_credential") return null;
  return row.kind === "machine"
    ? { gesture: "mint", label: "Create token" }
    : { gesture: "reset", label: "Get reset link" };
}

/**
 * The invitations nobody has redeemed and that are still good — and, asked, the
 * expired and redeemed ones the estate still holds until the sweep collects
 * them. Never a link: it was shown once, and an inviter who lost it cancels and
 * issues another.
 */
function Invitations({
  invitations,
  all,
  onAll,
  manages,
  index,
  onChanged,
}: {
  invitations: RestResult<InvitationRow[]>;
  all: boolean;
  onAll: (all: boolean) => void;
  manages: boolean;
  index: ReturnType<typeof indexOrg>;
  onChanged: () => void;
}) {
  const now = useNow();
  const [cancelling, setCancelling] = useState<InvitationRow | null>(null);
  const rows = invitations.data ?? [];
  return (
    <Card padding="none">
      <Card.Header
        icon={<SendGlyph size="sm" />}
        count={rows.length}
        actions={<Checkbox label="Show used and expired" checked={all} onCheckedChange={onAll} />}
      >
        <Card.Title>Invitations</Card.Title>
      </Card.Header>
      <QueryState
        error={invitations.code}
        refusal={invitations.refusal}
        detail={invitations.error?.detail || undefined}
        loading={invitations.loading && !invitations.data}
      >
        <DataGrid<InvitationRow>
          rows={rows}
          rowKey={(i) => i.id}
          defaultSort="expires"
          empty={{
            title: all ? "No invitations" : "No open invitations",
          }}
          columns={[
            // THE ADDRESS IS THE ONE FLEXIBLE TRACK, and is never cut (its
            // floor is its content): it is what a reader matches against the
            // mail they sent, and it ended in an ellipsis at 1440 while Seat
            // and Access beside it held 203px each of air. They size to what
            // they hold.
            {
              key: "email",
              header: "Address",
              floor: "max-content",
              sortValue: (i) => i.email ?? "",
              cell: (i) =>
                i.sealed ? (
                  <SealedTag />
                ) : i.email ? (
                  <TableTextCell>{i.email}</TableTextCell>
                ) : (
                  // NO ADDRESS AND NONE SEALED: a removal erases every value
                  // of its person's that other rows hold, the invitation
                  // they redeemed included — the cell was simply empty.
                  <EmptyValue label="Erased with the person it invited" />
                ),
            },
            {
              key: "seat",
              header: "Seat",
              shrink: true,
              drop: 2,
              sortValue: (i) => i.seat ?? "",
              cell: (i) => <BoundSeat seat={i.seat} index={index} />,
            },
            {
              key: "grants",
              header: "Access",
              shrink: true,
              drop: 1,
              sortValue: (i) => accessLine(i.grants ?? []),
              cell: (i) => <TableTextCell>{accessLine(i.grants ?? [])}</TableTextCell>,
            },
            {
              key: "by",
              header: "Invited by",
              shrink: true,
              drop: 2,
              sortValue: (i) => i.invited_by ?? "",
              cell: (i) => <Inviter author={i.invited_by} index={index} />,
            },
            {
              key: "expires",
              align: "right",
              header: "Expires",
              shrink: true,
              sortValue: (i) => i.expires_at ?? "",
              cell: (i) =>
                i.state === "open" ? (
                  <TableDateCell value={i.expires_at} now={now} />
                ) : (
                  <WordTag words={INVITATION_WORDS[i.state]} fallback={i.state} />
                ),
            },
            ...(manages
              ? [
                  {
                    key: "cancel",
                    header: "",
                    action: true,
                    shrink: true,
                    cell: (i: InvitationRow) =>
                      i.state === "redeemed" ? null : (
                        <Button
                          size="small"
                          variant="ghost"
                          aria-label={`Cancel the invitation to ${i.sealed ? i.id : i.email}`}
                          onClick={() => setCancelling(i)}
                        >
                          Cancel
                        </Button>
                      ),
                  },
                ]
              : []),
          ]}
        />
      </QueryState>
      {cancelling && (
        <CancelInvitation
          row={cancelling}
          seatName={cancelling.seat ? index.byHandle.get(cancelling.seat)?.name : undefined}
          onClose={() => setCancelling(null)}
          onChanged={onChanged}
        />
      )}
    </Card>
  );
}

/**
 * Who issued an invitation, as the chart names them: a person bound to a seat
 * writes as the seat, so the record holds its handle — "jane-founder" here
 * beside "Jane Founder" on the invitation's own page. Anybody else, a machine
 * included, by the login the record holds.
 */
function Inviter({ author, index }: { author?: string; index: ReturnType<typeof indexOrg> }) {
  if (!author) return <EmptyValue label="Unknown" />;
  const seat = index.byHandle.get(author);
  return seat ? (
    <TableEntityCell
      {...linkedSeat({ handle: author, name: seat.name || author, kind: seat.kind })}
    />
  ) : (
    <TableKeyCell value={author} />
  );
}

/** A name or an address this node's keyring cannot open. */
function SealedTag() {
  return (
    <Tag size="sm" variant="warning" title="This node can’t decrypt it.">
      Locked
    </Tag>
  );
}

/**
 * A gesture on one opened principal.
 *
 * A STAGE GESTURE NAMES ITS DIRECTION when it opens, never from the live row:
 * an unknown answer re-reads the directory, and a suspension that did land
 * would turn the dialog's "Try again" — the same key, which promises the same
 * operation — into a reactivation the engine takes as a new one.
 */
type PersonGesture =
  | "edit"
  | "suspend"
  | "reactivate"
  | "mfa"
  | "reset"
  | "sessions"
  | "remove"
  | "mint"
  | { revoke: CredentialRow };

/** A directory row's peek reference: by its id, never a login, which a rename moves. */
function personRef(id: string): ObjectRef {
  return { kind: "person", id };
}

/**
 * One principal, beside the directory it was found in — the frame's peek on
 * `person:{id}`, opened from a row of People, and their only page
 * (`KindSpec.peekIsPage`).
 *
 * IN THE PEEK, NOT UNDER THE LIST. It was a card drawn after the stats, the
 * report and the whole People grid, so pressing a row — your own row
 * included — opened it below the fold and the page did not move: it looked
 * as if nothing had happened. The peek opens in view beside the list on a
 * wide window and as a drawer on a phone, steps through the rows with `[` and
 * `]`, and closes on Back.
 *
 * IT ASKS FOR ITS OWN ROW (`GET /iam/people/{id}`), because a `peek=` arrives
 * pasted as often as clicked, and an id the directory does not hold — a
 * person removed since the link was made, or a mistyped one — is said rather
 * than drawn as an empty rail.
 *
 * THE READER'S OWN ROW LEADS WITH THEIR ACCOUNT. Every gesture here is an
 * administrator's, made about somebody — a reset link, a reset factor, every
 * session ended — and the reader who opened their own row was offered those
 * and nothing that said their password, their second factor and their
 * sessions are theirs to manage, on their Account. It says so first; the
 * administrator's gestures follow, each warning what it does to the reader.
 */
export function PersonPeek({ id }: { id: string }) {
  const viewer = useViewer();
  const toast = useToast();
  const { close } = usePeekControls();
  const at = encodeURIComponent(id);
  const read = useRest(
    id ? `/iam/people/${at}` : null,
    // THE PATH A LITERAL AT ITS CALL, which is what the dev proxy's gate reads.
    async (signal) => (await rest.get(`/iam/people/${at}`, signal)) as DirectoryRow | null,
    READ,
  );
  const row = read.data;
  // WHO IS READING, by the id the session names — never the login, which a
  // rename moves to somebody else: a gesture on the reader's own row signs
  // them out, and the panel names that row in the second person.
  const reader = useFrameSession().answer?.person;
  const you = row !== null && reader !== undefined && row.id === reader;
  // THE ADMINISTRATORS' COUNT, asked only about the reader's own row — the one
  // place it changes a sentence (`alone`).
  const check = useRest(
    you ? "/iam/check" : null,
    async (signal) => (await rest.get("/iam/check", signal)) as DirectoryCheck | null,
    READ,
  );
  const { reload } = read;
  const { reload: reloadCheck } = check;
  useDirectoryMoved(
    useCallback(() => {
      void reload({ quiet: true });
      void reloadCheck({ quiet: true });
    }, [reload, reloadCheck]),
  );
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  if (read.loading && !row) return <Skeleton variant="text" rows={6} label="Loading" />;
  // NOT AN EMPTY RAIL: an id nobody holds is said — the rail's own Close is
  // the way back to the list — and every other refusal is the read's own.
  if (read.code === "not_found") {
    return (
      <EmptyState
        size="compact"
        icon={<UsersGlyph size="xl" />}
        title="Not found"
        description="They may have been removed."
      />
    );
  }
  if (!row) {
    return (
      <QueryState
        error={read.code}
        refusal={read.refusal}
        detail={read.error?.detail || undefined}
        loading={false}
      />
    );
  }
  const who = row.sealed ? row.id : principalName(row);
  return (
    <>
      <ObjectHeader
        size="peek"
        kind={KIND_WORDS[row.kind] ?? row.kind}
        icon="user"
        identifier={row.sealed ? undefined : row.login}
        title={row.sealed ? <SealedTag /> : who}
        status={<WordTag words={STAGE_WORDS[row.stage]} fallback={row.stage} />}
        facts={[
          { label: "Seat", value: <BoundSeat seat={row.seat} index={index} /> },
          { label: "Access", value: <AccessTags grants={row.grants} /> },
        ]}
      />
      <Stack gap={3}>
        {you && (
          <Callout
            variant="neutral"
            title="This is you"
            action={
              <ButtonLink size="small" variant="secondary" href={href(["account"])}>
                Open Account
              </ButtonLink>
            }
          >
            Manage your sign-in on your Account.
          </Callout>
        )}
        <Principal
          row={row}
          who={who}
          you={you}
          administrators={check.data?.people_with_admin}
          manages={canManagePeople(viewer.grants)}
          held={viewer.grants}
          onRemoved={() => {
            // THE PEEK CLOSES ON ITS OWN REMOVAL, with what happened said:
            // left open, the re-read row answered 404 and the rail turned into
            // the note for a link naming nobody, a raw id included, right after
            // the reader's own action.
            toast.ok(`${who} was removed`);
            close();
          }}
        />
      </Stack>
    </>
  );
}

/**
 * One principal's credentials and sessions, read for this row alone — and, for
 * an admin, what may be done about them. A machine is offered what applies to
 * one: its access, its stage, its tokens and its removal; it has no password,
 * no second factor and no session.
 *
 * SECTIONS, NOT CARDS: the rail is the panel already, so they run on as plain
 * blocks rather than growing a second frame inside the first.
 */
function Principal({
  row,
  who,
  you,
  administrators,
  manages,
  held,
  onRemoved,
}: {
  row: DirectoryRow;
  /** What the panel calls them. */
  who: string;
  /** Whether this row is the person reading it. */
  you: boolean;
  /** How many active, enrolled PERSONS `GET /iam/check` counts holding Admin. */
  administrators: number | undefined;
  manages: boolean;
  held: readonly string[];
  /** The row's removal landed. */
  onRemoved: () => void;
}) {
  const now = useNow();
  const id = encodeURIComponent(row.id);
  const credentials = useRest(
    `/iam/credentials?person=${id}`,
    async (signal) =>
      listOf<CredentialRow>(await rest.get(`/iam/credentials?person=${id}`, signal), "credentials"),
    READ,
  );
  const sessions = useRest(
    `/iam/people/${id}/sessions`,
    async (signal) =>
      listOf<SessionRow>(await rest.get(`/iam/people/${id}/sessions`, signal), "sessions"),
    READ,
  );
  const [gesture, setGesture] = useState<PersonGesture | null>(null);
  const machine = row.kind === "machine";
  const live = (credentials.data ?? []).filter((c) => !c.revoked);
  const holdsFactor = live.some((c) => c.method === "totp" || c.method === "recovery");
  const suspended = row.stage === "suspended";
  // THE ONE ADMIN LEFT, by the engine's own count — active, enrolled persons
  // holding Admin, never a service account — which the reader, signed in, is
  // among.
  const alone = you && canManagePeople(row.grants ?? []) && administrators === 1;
  // EVERY WRITE IS ANNOUNCED ([directoryMoved]), and these reads hear it
  // with the list this peek is beside.
  const { reload: reloadCredentials } = credentials;
  const { reload: reloadSessions } = sessions;
  useDirectoryMoved(
    useCallback(() => {
      void reloadCredentials({ quiet: true });
      void reloadSessions({ quiet: true });
    }, [reloadCredentials, reloadSessions]),
  );
  const changed = directoryMoved;
  const close = () => setGesture(null);

  return (
    <>
      {/* EDIT AND SUSPEND IN VIEW, the rest under More with Remove last and
          set apart: six equal buttons wrapped over two rows and put the one
          that cannot be undone in the middle of the second. */}
      {manages && (
        <Inline gap={2} wrap>
          <Button size="small" variant="secondary" onClick={() => setGesture("edit")}>
            Edit
          </Button>
          {(row.stage === "active" || suspended) && (
            <Button
              size="small"
              variant="secondary"
              onClick={() => setGesture(suspended ? "reactivate" : "suspend")}
            >
              {suspended ? "Reactivate" : "Suspend"}
            </Button>
          )}
          <Menu
            label={`More for ${who}`}
            trigger="More"
            triggerVariant="secondary"
            items={[
              ...(machine
                ? [{ key: "mint", label: "Create token", onSelect: () => setGesture("mint") }]
                : [
                    // NOT FOR A STOPPED PERSON: a link would hand back an
                    // account somebody stopped, and the engine refuses it
                    // naming the stage.
                    ...(!suspended && row.stage !== "retired"
                      ? [
                          {
                            key: "reset",
                            label: "Get reset link",
                            onSelect: () => setGesture("reset"),
                          },
                        ]
                      : []),
                    ...(holdsFactor
                      ? [
                          {
                            key: "mfa",
                            label: "Reset two-step verification",
                            onSelect: () => setGesture("mfa"),
                          },
                        ]
                      : []),
                    {
                      key: "sessions",
                      label: "Sign out everywhere",
                      onSelect: () => setGesture("sessions"),
                    },
                  ]),
              { kind: "separator" as const, key: "before-remove" },
              {
                key: "remove",
                label: "Remove",
                danger: true,
                onSelect: () => setGesture("remove"),
              },
            ]}
          />
        </Inline>
      )}
      {gesture === "edit" && (
        <EditPersonDialog
          row={row}
          held={held}
          you={you}
          alone={alone}
          onClose={close}
          onDone={changed}
        />
      )}
      {gesture === "mint" && (
        <MintTokenDialog owner={row} held={held} onClose={close} onDone={changed} />
      )}
      {gesture === "reset" && (
        <ResetLink row={row} who={who} you={you} onClose={close} onDone={changed} />
      )}
      {gesture !== null && typeof gesture === "object" && (
        <PersonWrite
          title={`Revoke this ${METHOD_WORDS[gesture.revoke.method]?.toLowerCase() ?? "credential"}?`}
          confirm="Revoke"
          danger
          request={{
            method: "DELETE",
            path: `/iam/credentials/${encodeURIComponent(gesture.revoke.id)}`,
            query: { person: row.id },
          }}
          onClose={close}
          onDone={changed}
        >
          {gesture.revoke.method === "token"
            ? "Anything using it stops working."
            : gesture.revoke.method === "reset"
              ? `The link stops working. Get another if ${you ? "you" : "they"} still need one.`
              : you
                ? "You can no longer prove who you are with it."
                : `${who} can no longer prove who they are with it.`}
        </PersonWrite>
      )}
      {(gesture === "suspend" || gesture === "reactivate") && (
        <PersonWrite
          title={
            gesture === "reactivate" ? `Reactivate ${who}?` : `Suspend ${you ? "yourself" : who}?`
          }
          confirm={gesture === "reactivate" ? "Reactivate" : "Suspend"}
          danger={gesture === "suspend"}
          request={{
            method: "PATCH",
            path: `/iam/people/${id}`,
            body: { stage: gesture === "reactivate" ? "active" : "suspended" },
          }}
          onClose={close}
          onDone={changed}
        >
          {gesture === "suspend" && you && <OnYourself alone={alone} />}
          {/* WHAT THIS PERSON HOLDS, said: a seat they do not hold is not
              withheld, and the sessions a suspension ends stay ended — a
              password reset link among them, which a reactivation does not
              bring back either. A SUSPENDED PERSON KEEPS THEIR SEAT: it is
              withheld, never freed, so nobody else is put on it meanwhile. */}
          {gesture === "reactivate"
            ? `${who} can sign in again.`
            : you
              ? "You can’t sign in until another admin reactivates you."
              : "They can’t sign in until reactivated."}
        </PersonWrite>
      )}
      {gesture === "mfa" && (
        <PersonWrite
          title={
            you ? "Reset your two-step verification?" : `Reset ${who}’s two-step verification?`
          }
          confirm="Reset two-step verification"
          danger
          request={{ method: "POST", path: `/iam/people/${id}/mfa/reset` }}
          onClose={close}
          onDone={changed}
        >
          {you && <OnYourself />}
          {you
            ? "Your app and recovery codes stop working."
            : "Their app and recovery codes stop working."}
        </PersonWrite>
      )}
      {gesture === "sessions" && (
        <PersonWrite
          title={you ? "Sign out everywhere?" : `Sign ${who} out everywhere?`}
          confirm="Sign out everywhere"
          danger
          request={{ method: "DELETE", path: `/iam/people/${id}/sessions` }}
          onClose={close}
          onDone={changed}
        >
          {you && <OnYourself />}
          {you
            ? "Your sessions, tokens and reset link stop working."
            : "Their sessions, tokens and reset link stop working."}
        </PersonWrite>
      )}
      {gesture === "remove" && (
        <PersonWrite
          title={`Remove ${you ? "yourself" : who}?`}
          confirm="Remove"
          danger
          typeToConfirm={row.login || row.id}
          request={{ method: "DELETE", path: `/iam/people/${id}` }}
          onClose={close}
          onDone={changed}
          onLanded={onRemoved}
        >
          {you && <OnYourself alone={alone} what="" />}
          {you
            ? "Can’t be undone."
            : machine
              ? "Deletes the service account and its tokens. Can’t be undone."
              : "Their account is deleted and their seat opens up. Can’t be undone."}
        </PersonWrite>
      )}
      <Stack as="section" gap={2}>
        <Text as="div" variant="label">
          Credentials
        </Text>
        <QueryState
          error={credentials.code}
          refusal={credentials.refusal}
          detail={credentials.error?.detail || undefined}
          loading={credentials.loading && !credentials.data}
        >
          <DataGrid<CredentialRow>
            rows={credentials.data ?? []}
            rowKey={(c) => c.id}
            defaultSort="method"
            empty={{ title: "Holds no credential" }}
            columns={[
              {
                key: "method",
                header: "Credential",
                // THE POINT OF THE LIST HAS A FLOOR, and so does Carries: in
                // the peek's column every flexible track at zero gave way to
                // the content-sized ones, so "Password" read "P…" and the
                // header "C…" while Expires and Carries — which go first —
                // stayed at a pixel each.
                floor: "8rem",
                sortValue: (c) => c.method,
                cell: (c) => (
                  <TableTextCell>
                    {METHOD_WORDS[c.method] ?? c.method}
                    {c.label ? ` · ${c.label}` : ""}
                  </TableTextCell>
                ),
              },
              {
                key: "state",
                header: "State",
                // ITS OWN WIDTH, EXACTLY: one of four short words in a pill,
                // which a content-sized column's cap — a fifth of a 385px peek
                // — cut to "In u…".
                width: "max-content",
                sortValue: (c) => (c.revoked ? 1 : 0),
                cell: (c) =>
                  c.revoked ? (
                    <Tag size="sm" variant="neutral">
                      {endedWord(c, now)}
                    </Tag>
                  ) : (
                    // A RESET LINK NOBODY OPENED is not "in use": it is waiting.
                    <Tag size="sm" variant="success">
                      {c.method === "reset" ? "Not used yet" : "In use"}
                    </Tag>
                  ),
              },
              {
                key: "created",
                align: "right",
                header: "Created",
                shrink: true,
                drop: 3,
                sortValue: (c) => c.created_at ?? "",
                cell: (c) => <TableDateCell value={c.created_at} now={now} />,
              },
              {
                key: "expires",
                header: "Expires",
                shrink: true,
                drop: 1,
                sortValue: (c) => c.expires_at ?? "",
                cell: (c) => <ExpiresCell c={c} now={now} />,
              },
              {
                key: "grants",
                header: "Carries",
                floor: "8rem",
                drop: 2,
                // A RESET LINK CARRIES NO ACCESS: it sets a password once and
                // signs nobody in, and "Its holder's access" beside it read as
                // a link that opened the account.
                cell: (c) =>
                  c.method === "token" ? (
                    <AccessTags grants={c.grants} />
                  ) : (
                    <EmptyValue label={c.method === "reset" ? "One-time" : "Same as owner"} />
                  ),
              },
              ...(manages
                ? [
                    {
                      key: "revoke",
                      header: "",
                      action: true,
                      shrink: true,
                      cell: (c: CredentialRow) =>
                        c.revoked ? null : (
                          <Button
                            size="small"
                            variant="ghost"
                            aria-label={`Revoke ${METHOD_WORDS[c.method] ?? c.method}${c.label ? ` ${c.label}` : ""}`}
                            onClick={() => setGesture({ revoke: c })}
                          >
                            Revoke
                          </Button>
                        ),
                    },
                  ]
                : []),
            ]}
          />
        </QueryState>
      </Stack>
      {!machine && (
        <Stack as="section" gap={2}>
          <Text as="div" variant="label">
            Sessions
          </Text>
          <QueryState
            error={sessions.code}
            refusal={sessions.refusal}
            detail={sessions.error?.detail || undefined}
            loading={sessions.loading && !sessions.data}
          >
            <DataGrid<SessionRow>
              rows={sessions.data ?? []}
              rowKey={(s) => s.lineage}
              defaultSort="started"
              empty={{ title: "Signed in nowhere" }}
              columns={[
                {
                  // A TIMESTAMP, so it is not the flexible column: it took the
                  // grid's flexible width and drew it nearly empty while the
                  // State beside it, capped as a shrink column, cut a session's
                  // reason off at its edge with no ellipsis and no way to read
                  // the rest. ITS OWN WIDTH, EXACTLY, rather than capped: a
                  // relative time is short, and the cap — a fifth of the
                  // peek's column — cut its own header to "SESS…".
                  key: "started",
                  header: "Session",
                  // A TIME: right-aligned, as the same session start is on the
                  // account page's own list.
                  align: "right",
                  width: "max-content",
                  sortValue: (s) => s.created_at ?? "",
                  cell: (s) => <TableDateCell value={s.created_at} now={now} />,
                },
                {
                  key: "state",
                  header: "State",
                  // THE POINT OF THE LIST — signed in, or ended and why — so it
                  // keeps a floor, and Ends goes before it narrows to nothing.
                  floor: "8rem",
                  sortValue: (s) => (s.live ? 0 : 1),
                  cell: (s) =>
                    s.live ? (
                      <Tag
                        size="sm"
                        variant="success"
                        title={
                          s.enrolment_only ? "Can only set up two-step verification" : undefined
                        }
                      >
                        {s.enrolment_only ? "Setting up" : "Signed in"}
                      </Tag>
                    ) : (
                      // WHY, SAID: it rode in a tooltip, so a session a password
                      // change ended read as one that merely had. ONE LINE, cut
                      // with an ellipsis and whole on its title where the column
                      // is narrower than the sentence.
                      <Inline as="span" gap={1}>
                        <Tag size="sm" variant="neutral">
                          Ended
                        </Tag>
                        {s.ended_reason && (
                          <Text variant="caption" truncate title={s.ended_reason}>
                            {s.ended_reason}
                          </Text>
                        )}
                      </Inline>
                    ),
                },
                {
                  key: "ends",
                  align: "right",
                  header: "Ends",
                  shrink: true,
                  drop: 1,
                  sortValue: (s) => s.ended_at ?? s.expires_at ?? "",
                  cell: (s) =>
                    // A DEADLINE IS NOT AN END: a session a counter ended — a
                    // password change, signing out everywhere — has no ended_at,
                    // and its expiry read "in 6d" beside "Ended".
                    !s.live && !s.ended_at && s.expires_at && tsKey(s.expires_at) > now ? (
                      <EmptyValue label="Not recorded" />
                    ) : (
                      <TableDateCell value={s.ended_at ?? s.expires_at} now={now} />
                    ),
                },
              ]}
            />
          </QueryState>
        </Stack>
      )}
    </>
  );
}

/** One confirmed write about a principal: the dialog, the request, the answer. */
function PersonWrite({
  title,
  confirm,
  danger,
  typeToConfirm,
  request,
  onClose,
  onDone,
  onLanded,
  children,
}: {
  title: string;
  confirm: string;
  danger?: boolean;
  typeToConfirm?: string;
  request: Parameters<ReturnType<typeof useIamGesture>["run"]>[0];
  onClose: () => void;
  onDone: () => void;
  /** The write landed — applied here, or durable and not yet applied. */
  onLanded?: () => void;
  children: React.ReactNode;
}) {
  const write = useIamGesture();
  return (
    <ConfirmDialog
      title={title}
      confirm={confirm}
      danger={danger}
      typeToConfirm={typeToConfirm}
      write={write}
      onClose={onClose}
      onConfirm={async () => {
        const answer = await write.run(request);
        if (!answer) return;
        onDone();
        if (answer.kind === "done") onLanded?.();
        if (answer.kind === "done" && !answer.pending) onClose();
      }}
    >
      {children}
    </ConfirmDialog>
  );
}

/**
 * Issue a one-time password reset link, and show it ONCE: it sets a new
 * password, ends every session and token the person holds, and signs nobody
 * in. Issuing another revokes this one; the outstanding link is listed among
 * their credentials, where it can be revoked. Whatever ends their sessions —
 * a second factor's reset, ending them, a suspension — ends it too, so the
 * dialog says to issue it after those rather than leave a dead link in flight.
 *
 * ON THE READER'S OWN ROW IT SPEAKS TO THEM, as every other gesture there
 * does, and says what they would more likely want: a password they know is
 * changed on their Account with no link.
 */
function ResetLink({
  row,
  who,
  you,
  onClose,
  onDone,
}: {
  row: DirectoryRow;
  who: string;
  /** The row is the reader's own. */
  you: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const write = useIamGesture();
  const issued = write.answer?.kind === "done" ? write.answer.body : null;
  if (issued) {
    const url = typeof issued.url === "string" ? issued.url : "";
    const expires = typeof issued.expires_at === "string" ? issued.expires_at : "";
    // DONE ALONE, as an invitation's link is: the link exists, and a Cancel
    // beside it read as a way to take it back, which closing does not do.
    return (
      <Modal
        open
        size="sm"
        title={you ? "Your reset link" : `Reset link for ${who}`}
        onClose={onClose}
        stackBody
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <ShownOnce label="Reset link" value={url}>
          Copy it now. You won’t see it again. Works until {fmtDate(expires)}.
        </ShownOnce>
        <IamOutcome answer={write.answer} />
      </Modal>
    );
  }
  return (
    <ConfirmDialog
      title={you ? "Get a reset link for yourself?" : `Get a reset link for ${who}?`}
      confirm="Get link"
      write={write}
      onClose={onClose}
      onConfirm={async () => {
        // A LINK READS NO KEY: a replay could not hand back a secret its first
        // attempt never showed, so a retry is a new link — which revokes the one
        // that may have landed.
        const answer = await write.run(
          { method: "POST", path: `/iam/people/${encodeURIComponent(row.id)}/password-reset` },
          false,
        );
        if (answer) onDone();
      }}
    >
      {you
        ? "Works once, within a day. To change a known password, use Account › Security."
        : "Works once, for a day."}
    </ConfirmDialog>
  );
}
