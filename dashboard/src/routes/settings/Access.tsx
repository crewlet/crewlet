/**
 * Settings › People & access: who can reach this company, and as whom.
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
 * acts as a seat — so this screen draws the directory from both ends: each
 * principal with the seat it holds, each human seat with whoever holds it, and
 * each of this node's tokens with the row its login names.
 *
 * # Read from the identity directory
 *
 * Six answers of `/iam`:
 *
 *  - `GET /iam/people` — every principal, walked to its last page, because a
 *    directory drawn from its first page is a company that looks smaller than
 *    it is. Opening a row reads that one principal's credentials
 *    (`GET /iam/credentials?person=`) and sessions
 *    (`GET /iam/people/{id}/sessions`).
 *  - `GET /iam/seats` — every human seat of the running company and who
 *    holds it.
 *  - `GET /iam/node-tokens` — THIS NODE's Tier A tokens by label, each joined
 *    to the directory row its login names.
 *  - `GET /iam/check` — what the directory reports wrong, worded per kind
 *    with the engine's own detail beside it, so a kind this build does not
 *    know still reads.
 *  - `GET /iam/invitations` — the invitations nobody has redeemed and that
 *    are still good, and with `?all=true` the expired and redeemed ones the
 *    estate still holds. Never a link: it was shown once.
 *
 * # And written, by a reader holding `people:manage`
 *
 * Inviting somebody, creating a service account and minting its token,
 * cancelling an invitation, and — on an opened row — changing a login, a seat
 * or grants, suspending and reactivating, resetting a second factor, issuing a
 * password reset link, ending every session, revoking one credential and
 * removing somebody. Each is one `/iam` write through `lib/iamWrite.ts`: a
 * step-up the engine asks for is confirmed and the same request replayed, an
 * unknown answer is retried under the same operation key, and a refusal is the
 * engine's sentence with the grants that would admit. After each write the
 * lists it moved are read again. The controls are drawn for a reader holding
 * `people:manage` only — see `components/people.tsx` for why an auditor's
 * read view stays as it was.
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
 * Every read is `people:manage` or `audit:read` (`authz.ActionDirectoryRead`).
 * A reader refused sees the refusal and the grants that would have admitted
 * them — never an empty company. The contact identities are the company
 * document's, which takes `config:read`, and a reader without it is told so
 * rather than shown seats nobody can reach.
 */

import { useCallback, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Card,
  Checkbox,
  EMPTY_VALUE,
  EmptyValue,
  Modal,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  KeyGlyph,
  SendGlyph,
  PlusGlyph,
  TriangleAlertGlyph,
  UserPlusGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  canManagePeople,
  ConfirmDialog,
  endedWord,
  ExpiresCell,
  GrantTags,
  IamOutcome,
  InviteDialog,
  MintTokenDialog,
  ShownOnce,
} from "~/components/people.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href, useParam } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, tsKey } from "~/lib/format.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { documentUnits, indexOrg, unitByKey } from "~/lib/seats.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { useViewer } from "~/lib/viewer.ts";
import { auth, rest } from "~/protocol/index.ts";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";
import { EditPersonDialog, ServiceAccountDialog } from "./AccessDialogs.tsx";

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

/** One human seat of the running company and who holds it (`iamapi.SeatRow`). */
export interface SeatRow {
  handle: string;
  name: string;
  /** The key of the unit the seat sits in, "" at the root. */
  unit?: string;
  /** Whoever the directory binds to it, absent for a seat nobody holds. */
  holder?: { person: string; login?: string; stage?: string };
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

interface DirectoryCheck {
  findings: DirectoryFinding[] | null;
  people_with_people_manage: number;
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
    hint: "Created, and has proved nothing yet. May not act.",
  },
  enrolling: {
    label: "Enrolling",
    tone: "neutral",
    hint: "Part-way through proving who they are. May not act yet.",
  },
  active: { label: "Active", tone: "success", hint: "Enrolled, and the only stage that may act." },
  suspended: {
    label: "Suspended",
    tone: "warning",
    hint: "Enrolled and may not act; the record is kept, and reinstating does not re-enrol.",
  },
  retired: {
    label: "Retired",
    tone: "neutral",
    hint: "Has left. Kept so every audit row they wrote still resolves to a name.",
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
    label: "No row",
    tone: "neutral",
    hint: "Nobody in the directory holds this login, so the token acts as itself and is bound to no seat. Bind it with crewlet iam if it should act as one.",
  },
  held: { label: "Row", tone: "success", hint: "A directory row holds this login." },
};

/** The directory report's kinds, in the engine's order (`iamapi.FindingKinds`). */
export const FINDING_WORDS: Record<string, { label: string; tone: Tone }> = {
  no_people_manage_holder: { label: "Nobody can administer", tone: "danger" },
  person_without_credential: { label: "Active, no credential", tone: "warning" },
  binding_dangling: { label: "Seat gone", tone: "warning" },
  grant_clamped_by_ceiling: { label: "Grant withheld here", tone: "neutral" },
};

/** What a credential method is called. */
const METHOD_WORDS: Record<string, string> = {
  password: "Password",
  totp: "Authenticator app",
  recovery: "Recovery codes",
  token: "Machine token",
  reset: "Password reset link",
};

/** What an invitation's state is called. */
export const INVITATION_WORDS: Record<string, Words> = {
  open: { label: "Open", tone: "success", hint: "Not redeemed, and still good." },
  expired: {
    label: "Expired",
    tone: "neutral",
    hint: "Past its deadline; the link answers that it is no longer valid.",
  },
  redeemed: { label: "Redeemed", tone: "neutral", hint: "Spent: it created the person it names." },
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

/** The engine writes a detail in its own lower case; drawn, it is a sentence. */
function sentence(text: string): string {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  const opened = trimmed[0]!.toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(opened) ? opened : `${opened}.`;
}

function WordTag({ words, fallback }: { words: Words | undefined; fallback: string }) {
  return (
    <Tag size="sm" variant={words?.tone ?? "neutral"} title={words?.hint}>
      {words?.label ?? fallback}
    </Tag>
  );
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

/**
 * How often each answer is asked again, in ms.
 *
 * The directory moves on an administrator's gesture and a node's Tier A at a
 * restart: a minute is soon enough to see a `crewlet iam` run in another
 * terminal land, and the page is read, not watched. The tab coming back asks
 * at once, which is where somebody who just ran the command is looking.
 */
const POLL_MS = 60_000;

/** A page as large as the directory serves (`iamdomain.MaxPageSize`). */
const PAGE = 200;

/** The grant the company document — and so every contact identity — is read under. */
const CONFIG_READ = "config:read";

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

/** How every `/iam` read here is kept current. */
const READ = { pollMs: POLL_MS, refetchOnFocus: true };

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/** The dialog a page action has open. */
type Opening = "invite" | "service" | null;

export function PeopleAndAccess() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const health = useEngineHealth();
  const manages = canManagePeople(viewer.grants);
  const [opened, setOpened] = useParam("person", "");
  const [opening, setOpening] = useState<Opening>(null);
  const [allInvitations, setAllInvitations] = useState(false);

  const directory = useRest("/iam/people", readDirectory, READ);
  const seats = useRest(
    "/iam/seats",
    async (signal) => listOf<SeatRow>(await rest.get("/iam/seats", signal), "seats"),
    READ,
  );
  const tokens = useRest(
    "/iam/node-tokens",
    async (signal) => listOf<NodeToken>(await rest.get("/iam/node-tokens", signal), "tokens"),
    READ,
  );
  const check = useRest(
    "/iam/check",
    async (signal) => (await rest.get("/iam/check", signal)) as DirectoryCheck | null,
    READ,
  );
  const invitations = useRest(
    `/iam/invitations?all=${allInvitations}`,
    readInvitations(allInvitations),
    READ,
  );
  // AFTER A WRITE, EVERY LIST IT MAY HAVE MOVED is read again, quietly: an
  // invitation moves the invitations and the vacant seats, a create the
  // directory, an edit the directory, the seats and the report.
  const { reload: reloadDirectory } = directory;
  const { reload: reloadSeats } = seats;
  const { reload: reloadTokens } = tokens;
  const { reload: reloadCheck } = check;
  const { reload: reloadInvitations } = invitations;
  const refresh = useCallback(() => {
    for (const reload of [
      reloadDirectory,
      reloadSeats,
      reloadTokens,
      reloadCheck,
      reloadInvitations,
    ]) {
      void reload({ quiet: true });
    }
  }, [reloadDirectory, reloadSeats, reloadTokens, reloadCheck, reloadInvitations]);
  // THE CONTACT IDENTITIES ARE THE COMPANY DOCUMENT'S, which takes
  // `config:read`. A reader without it is never asked: the refusal is known
  // before the question, and each seat says what it would take instead.
  const readsConfig = viewer.grants.includes(CONFIG_READ);
  const config = useQuery("config", undefined, { enabled: readsConfig });
  const contacts = useMemo(() => contactsByHandle(config.data), [config.data]);

  const people = useMemo(() => directory.data ?? [], [directory.data]);
  const active = people.filter((p) => p.stage === "active").length;
  const seatRows = useMemo(() => seats.data ?? [], [seats.data]);
  const unheld = seatRows.filter((s) => !s.holder).length;
  const tokenRows = useMemo(() => tokens.data ?? [], [tokens.data]);
  const boundTokens = tokenRows.filter((t) => t.seat).length;
  const openedRow = people.find((p) => p.id === opened) ?? null;

  return (
    <>
      <PageActions>
        {manages && (
          <>
            <Button
              variant="primary"
              leadingIcon={<UserPlusGlyph size="sm" />}
              onClick={() => setOpening("invite")}
            >
              Invite person
            </Button>
            <Button
              variant="secondary"
              leadingIcon={<PlusGlyph size="sm" />}
              onClick={() => setOpening("service")}
            >
              New service account
            </Button>
          </>
        )}
        <a className="t-link" href={href(["settings", "audit"], { kind: "identity" })}>
          Identity trail →
        </a>
        {/* THE CHART IS WHERE A SEAT AND ITS CONTACTS ARE WRITTEN, so the
            action here leaves for it rather than editing a copy. */}
        <a className="t-link" href={href(["agents", "edit"])}>
          Edit org →
        </a>
      </PageActions>
      <PageNote>
        Who can reach this company and as whom: the identity directory, its open invitations, the
        human seats and who holds them, and this node&rsquo;s API tokens.{" "}
        {manages
          ? "Invite, change and remove people here; an invitation's link, a reset link and a token are shown once."
          : "Changing any of it takes people:manage."}{" "}
        This screen never reads a credential&rsquo;s value back.
      </PageNote>
      {opening === "invite" && (
        <InviteDialog held={viewer.grants} onClose={() => setOpening(null)} onDone={refresh} />
      )}
      {opening === "service" && (
        <ServiceAccountDialog
          held={viewer.grants}
          onClose={() => setOpening(null)}
          onDone={refresh}
        />
      )}

      {/* FIRST RUN: nobody has been invited, and somebody signed in with an
          API token is reading this — the one moment the next step is
          inviting yourself. */}
      {health?.identity === "unclaimed" && (
        <Callout
          variant="neutral"
          title="Nobody has been invited yet"
          action={
            manages ? (
              <Button size="small" variant="primary" onClick={() => setOpening("invite")}>
                Invite person
              </Button>
            ) : undefined
          }
        >
          This company has no person in it, so nobody can sign in with a password yet. Invite
          yourself — your address, your seat and the grants you need — and open the link it shows:
          that is where you choose your login and password. The API token you are using stays the
          way back in.
        </Callout>
      )}

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

      {directory.data && (
        <>
          <Card padding="none">
            <StatGroup columns={3}>
              <StatCard
                icon={<UsersGlyph size="xs" />}
                label="People"
                value={people.length}
                sub={`${active} active`}
              />
              <StatCard
                icon={<TriangleAlertGlyph size="xs" />}
                label="Human seats nobody holds"
                value={seats.data ? unheld : EMPTY_VALUE}
                sub={
                  seats.data
                    ? `of ${seatRows.length} human seat${seatRows.length === 1 ? "" : "s"}`
                    : "not read"
                }
              />
              <StatCard
                icon={<KeyGlyph size="xs" />}
                label="API tokens on this node"
                value={tokens.data ? tokenRows.length : EMPTY_VALUE}
                sub={tokens.data ? `${boundTokens} bound to a seat` : "not read"}
              />
            </StatGroup>
          </Card>

          <Findings check={check} />

          <Card padding="none">
            <Card.Header icon={<UsersGlyph size="sm" />} count={people.length}>
              People
            </Card.Header>
            <DataGrid<DirectoryRow>
              rows={people}
              rowKey={(p) => p.id}
              defaultSort="person"
              isSelected={(p) => p.id === opened}
              onRowActivate={(p) => setOpened(p.id === opened ? "" : p.id)}
              empty={{
                title: "Nobody is in the directory yet",
                hint: "Invite the first person with Invite person above (or crewlet iam invite), signed in with this node's API token.",
              }}
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
                  sortValue: (p) => p.login ?? "",
                  cell: (p) =>
                    p.login ? <KeyCell value={p.login} /> : <EmptyValue label="No login" />,
                },
                {
                  key: "kind",
                  header: "Kind",
                  shrink: true,
                  drop: 2,
                  sortValue: (p) => p.kind,
                  cell: (p) => <TextCell>{KIND_WORDS[p.kind] ?? p.kind}</TextCell>,
                },
                {
                  key: "stage",
                  header: "Stage",
                  shrink: true,
                  sortValue: (p) => p.stage,
                  cell: (p) => <WordTag words={STAGE_WORDS[p.stage]} fallback={p.stage} />,
                },
                {
                  key: "seat",
                  header: "Seat",
                  sortValue: (p) => p.seat ?? "",
                  cell: (p) => <BoundSeat seat={p.seat} index={index} />,
                },
                {
                  key: "grants",
                  header: "Grants",
                  drop: 1,
                  sortValue: (p) => (p.grants ?? []).length,
                  cell: (p) => <GrantTags grants={p.grants} />,
                },
              ]}
            />
          </Card>

          {openedRow && (
            <Principal row={openedRow} manages={manages} held={viewer.grants} onChanged={refresh} />
          )}

          <Invitations
            invitations={invitations}
            all={allInvitations}
            onAll={setAllInvitations}
            manages={manages}
            index={index}
            onChanged={refresh}
          />

          <Card padding="none">
            <Card.Header icon={<UsersGlyph size="sm" />} count={seatRows.length}>
              Human seats
            </Card.Header>
            <QueryState
              error={seats.code}
              refusal={seats.refusal}
              detail={seats.error?.detail || undefined}
              loading={seats.loading && !seats.data}
            >
              <DataGrid<SeatRow>
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
                    cell: (s) => <SeatCell handle={s.handle} name={s.name} kind="human" />,
                  },
                  {
                    key: "unit",
                    header: "Team",
                    shrink: true,
                    drop: 2,
                    sortValue: (s) => (s.unit ? (unitByKey(index, s.unit)?.name ?? s.unit) : ""),
                    cell: (s) => {
                      if (!s.unit) return <EmptyValue label="No team" />;
                      return <TextCell>{unitByKey(index, s.unit)?.name ?? s.unit}</TextCell>;
                    },
                  },
                  {
                    key: "holder",
                    header: "Held by",
                    sortValue: (s) => s.holder?.login ?? s.holder?.person ?? "",
                    cell: (s) => <Holder holder={s.holder} />,
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
                          label={needsSentence("Reading where a seat is reached", [CONFIG_READ])}
                        />
                      ),
                  },
                ]}
              />
            </QueryState>
          </Card>

          <Card padding="none">
            <Card.Header icon={<KeyGlyph size="sm" />} count={tokenRows.length}>
              API tokens on this node
            </Card.Header>
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
                  hint: "Tokens are declared in Tier A under api.auth.tokens and change with a restart.",
                }}
                columns={[
                  {
                    key: "id",
                    header: "Label",
                    shrink: true,
                    sortValue: (t) => t.id,
                    cell: (t) => <KeyCell value={t.id} />,
                  },
                  {
                    key: "login",
                    header: "Acts under",
                    shrink: true,
                    drop: 1,
                    sortValue: (t) => t.login,
                    cell: (t) => <KeyCell value={t.login} />,
                  },
                  {
                    key: "row",
                    header: "Directory",
                    shrink: true,
                    sortValue: (t) => t.row,
                    cell: (t) => <WordTag words={TOKEN_ROW_WORDS[t.row]} fallback={t.row} />,
                  },
                  {
                    key: "seat",
                    header: "Seat",
                    floor: "10rem",
                    sortValue: (t) => t.seat ?? "",
                    cell: (t) => <BoundSeat seat={t.seat} index={index} />,
                  },
                ]}
              />
            </QueryState>
          </Card>
        </>
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Parts
// ---------------------------------------------------------------------------

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
  return <TextCell>{row.name || row.login || row.id}</TextCell>;
}

/** Whoever holds a seat, by login and stage — or the plain fact that nobody does. */
function Holder({ holder }: { holder: SeatRow["holder"] }) {
  if (!holder) return <EmptyValue label="Nobody holds it" />;
  return (
    <Tag
      size="sm"
      variant={STAGE_WORDS[holder.stage ?? ""]?.tone ?? "neutral"}
      title={STAGE_WORDS[holder.stage ?? ""]?.label ?? holder.stage}
    >
      <span className="mono">{holder.login || holder.person}</span>
    </Tag>
  );
}

/** Where agents reach a seat: one chip per field, as the document writes it. */
function Contacts({ contact }: { contact: ConfigRole["contact"] }) {
  const entries = Object.entries(contact ?? {}).filter(([, value]) => value);
  if (entries.length === 0) return <EmptyValue label="No surface" />;
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {entries.map(([key, value]) => (
        <Tag key={key} size="sm" appearance="outline" title={`${key}: ${value}`}>
          {CONTACT_SURFACES[key] ?? key} <span className="mono">{value}</span>
        </Tag>
      ))}
    </span>
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
 */
function BoundSeat({ seat, index }: { seat?: string; index: ReturnType<typeof indexOrg> }) {
  if (!seat) return <EmptyValue label="Bound to no seat" />;
  const held = index.byHandle.get(seat);
  return <SeatCell handle={seat} name={held?.name ?? seat} kind={held?.kind} />;
}

/**
 * What the directory reports wrong. Each finding by its kind and, beside it,
 * the engine's own detail — so a kind this build has no word for still reads
 * as the sentence the engine wrote.
 */
function Findings({ check }: { check: RestResult<DirectoryCheck | null> }) {
  const findings = check.data?.findings ?? [];
  const unchecked = check.data?.bindings_unchecked ?? 0;
  return (
    <Card padding="none">
      <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={findings.length}>
        What the directory reports
      </Card.Header>
      <QueryState
        error={check.code}
        refusal={check.refusal}
        detail={check.error?.detail || undefined}
        loading={check.loading && !check.data}
      >
        <div className="col" style={{ gap: "var(--spacing-2)", padding: "var(--spacing-4)" }}>
          {findings.length === 0 && <p className="t-body muted">Nothing to report.</p>}
          {findings.map((f, i) => {
            const words = FINDING_WORDS[f.kind];
            return (
              <div key={`${f.kind}:${f.person ?? f.seat ?? i}`} className="row gap-2">
                <Tag size="sm" variant={words?.tone ?? "neutral"}>
                  {words?.label ?? f.kind}
                </Tag>
                <span className="t-body">{sentence(f.detail)}</span>
              </div>
            );
          })}
          {/* SAID RATHER THAN SILENT: "no dangling binding" and "this node
              could not say" are different answers. */}
          {unchecked > 0 && (
            <p className="t-caption muted">
              {unchecked} binding{unchecked === 1 ? "" : "s"} could not be checked against this
              node&rsquo;s chart just now.
            </p>
          )}
        </div>
      </QueryState>
    </Card>
  );
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
        actions={
          <Checkbox label="Show expired and redeemed" checked={all} onCheckedChange={onAll} />
        }
      >
        Invitations
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
            title: all ? "No invitations are held" : "No invitation is waiting to be redeemed",
          }}
          columns={[
            {
              key: "email",
              header: "Address",
              floor: "12rem",
              sortValue: (i) => i.email ?? "",
              cell: (i) =>
                i.sealed ? (
                  <SealedTag />
                ) : i.email ? (
                  <TextCell>{i.email}</TextCell>
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
              drop: 2,
              sortValue: (i) => i.seat ?? "",
              cell: (i) => <BoundSeat seat={i.seat} index={index} />,
            },
            {
              key: "grants",
              header: "Grants",
              drop: 1,
              sortValue: (i) => (i.grants ?? []).length,
              cell: (i) => <GrantTags grants={i.grants} />,
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
              header: "Expires",
              shrink: true,
              sortValue: (i) => i.expires_at ?? "",
              cell: (i) =>
                i.state === "open" ? (
                  <DateCell at={i.expires_at} now={now} />
                ) : (
                  <WordTag words={INVITATION_WORDS[i.state]} fallback={i.state} />
                ),
            },
            ...(manages
              ? [
                  {
                    key: "cancel",
                    header: "",
                    label: "Cancel",
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
    <SeatCell handle={author} name={seat.name || author} kind={seat.kind} />
  ) : (
    <KeyCell value={author} />
  );
}

/** Withdraw an invitation nobody redeemed: its link stops working, and the address is free. */
function CancelInvitation({
  row,
  onClose,
  onChanged,
}: {
  row: InvitationRow;
  onClose: () => void;
  onChanged: () => void;
}) {
  const write = useIamGesture();
  return (
    <ConfirmDialog
      title="Cancel this invitation?"
      confirm="Cancel invitation"
      dismiss="Keep it"
      danger
      write={write}
      onClose={onClose}
      onConfirm={async () => {
        const answer = await write.run({
          method: "DELETE",
          path: `/iam/invitations/${encodeURIComponent(row.id)}`,
        });
        if (!answer) return;
        onChanged();
        if (answer.kind === "done" && !answer.pending) onClose();
      }}
    >
      The link sent to {row.sealed ? "this address" : <strong>{row.email}</strong>} stops working at
      once, as one nobody issued, and the address is free to invite again.
    </ConfirmDialog>
  );
}

/** A name or an address this node's keyring cannot open. */
function SealedTag() {
  return (
    <Tag
      size="sm"
      variant="warning"
      title="This node's keyring cannot open this value: a key was dropped before crewlet secrets rekey moved it off, or the estate was restored under another keyring."
    >
      sealed
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

/**
 * One principal opened: the credentials they prove themselves with and the
 * sessions they are signed in with, read for this row alone — and, for a reader
 * holding `people:manage`, what may be done about them. A machine is offered
 * what applies to one: its grants, its stage, its tokens and its removal; it
 * has no password, no second factor and no session.
 */
function Principal({
  row,
  manages,
  held,
  onChanged,
}: {
  row: DirectoryRow;
  manages: boolean;
  held: readonly string[];
  onChanged: () => void;
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
  const who = row.sealed ? row.id : row.name || row.login || row.id;
  const machine = row.kind === "machine";
  const live = (credentials.data ?? []).filter((c) => !c.revoked);
  const holdsFactor = live.some((c) => c.method === "totp" || c.method === "recovery");
  const suspended = row.stage === "suspended";
  const { reload: reloadCredentials } = credentials;
  const { reload: reloadSessions } = sessions;
  const changed = () => {
    onChanged();
    void reloadCredentials({ quiet: true });
    void reloadSessions({ quiet: true });
  };
  const close = () => setGesture(null);

  return (
    <Card padding="none">
      <Card.Header icon={<KeyGlyph size="sm" />}>{who}: credentials and sessions</Card.Header>
      {manages && (
        <div
          className="row gap-2"
          style={{ flexWrap: "wrap", padding: "var(--spacing-3) var(--spacing-4)" }}
        >
          <Button size="small" variant="secondary" onClick={() => setGesture("edit")}>
            Edit login, seat and grants
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
          {machine ? (
            <Button size="small" variant="secondary" onClick={() => setGesture("mint")}>
              Mint token
            </Button>
          ) : (
            <>
              {/* NOT FOR A STOPPED PERSON: a link would hand back an account
                  somebody stopped, and the engine refuses it naming the stage. */}
              {!suspended && row.stage !== "retired" && (
                <Button size="small" variant="secondary" onClick={() => setGesture("reset")}>
                  Issue password reset link
                </Button>
              )}
              {holdsFactor && (
                <Button size="small" variant="secondary" onClick={() => setGesture("mfa")}>
                  Reset second factor
                </Button>
              )}
              <Button size="small" variant="secondary" onClick={() => setGesture("sessions")}>
                End all sessions
              </Button>
            </>
          )}
          <Button size="small" variant="danger" onClick={() => setGesture("remove")}>
            Remove
          </Button>
        </div>
      )}
      {gesture === "edit" && (
        <EditPersonDialog row={row} held={held} onClose={close} onDone={changed} />
      )}
      {gesture === "mint" && (
        <MintTokenDialog owner={row} held={held} onClose={close} onDone={changed} />
      )}
      {gesture === "reset" && <ResetLink row={row} who={who} onClose={close} onDone={changed} />}
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
            ? `Anything presenting it is refused from now on${gesture.revoke.label ? ` — "${gesture.revoke.label}"` : ""}.`
            : gesture.revoke.method === "reset"
              ? "The link stops working; issue another if they still need one."
              : `${who} can no longer prove who they are with it.`}
        </PersonWrite>
      )}
      {(gesture === "suspend" || gesture === "reactivate") && (
        <PersonWrite
          title={gesture === "reactivate" ? `Reactivate ${who}?` : `Suspend ${who}?`}
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
          {/* WHAT THIS PERSON HOLDS, said: a seat they do not hold is not
              withheld, and the sessions a suspension ends stay ended. */}
          {gesture === "reactivate"
            ? `${who} may sign in again with what they held${row.seat ? ", their seat included" : ""}; nothing has to be enrolled again. The sessions and tokens the suspension ended stay ended.`
            : `${who} may not act while suspended: every session and token they hold ends and their sign-ins are refused${row.seat ? ", and their seat is withheld" : ""}. Their record is kept, and reactivating lets them sign in again.`}
        </PersonWrite>
      )}
      {gesture === "mfa" && (
        <PersonWrite
          title={`Reset ${who}'s second factor?`}
          confirm="Reset second factor"
          danger
          request={{ method: "POST", path: `/iam/people/${id}/mfa/reset` }}
          onClose={close}
          onDone={changed}
        >
          Their authenticator app and recovery codes stop working, and every session and token they
          hold ends. <AfterFactorReset />
        </PersonWrite>
      )}
      {gesture === "sessions" && (
        <PersonWrite
          title={`End every session ${who} holds?`}
          confirm="End all sessions"
          danger
          request={{ method: "DELETE", path: `/iam/people/${id}/sessions` }}
          onClose={close}
          onDone={changed}
        >
          They are signed out everywhere, and every personal access token they minted stops working
          too.
        </PersonWrite>
      )}
      {gesture === "remove" && (
        <PersonWrite
          title={`Remove ${who}?`}
          confirm="Remove"
          danger
          typeToConfirm={row.login || row.id}
          request={{ method: "DELETE", path: `/iam/people/${id}` }}
          onClose={close}
          onDone={changed}
        >
          Their row, credentials and sessions are deleted, {row.seat ? "their seat is freed, " : ""}
          and every sealed value of theirs is erased. This cannot be undone: to bring them back,
          invite them again. The trail keeps what they did.
        </PersonWrite>
      )}
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
              sortValue: (c) => c.method,
              cell: (c) => (
                <TextCell>
                  {METHOD_WORDS[c.method] ?? c.method}
                  {c.label ? ` · ${c.label}` : ""}
                </TextCell>
              ),
            },
            {
              key: "state",
              header: "State",
              shrink: true,
              sortValue: (c) => (c.revoked ? 1 : 0),
              cell: (c) =>
                c.revoked ? (
                  <Tag size="sm" variant="neutral">
                    {endedWord(c, now)}
                  </Tag>
                ) : (
                  <Tag size="sm" variant="success">
                    In use
                  </Tag>
                ),
            },
            {
              key: "created",
              header: "Created",
              shrink: true,
              sortValue: (c) => c.created_at ?? "",
              cell: (c) => <DateCell at={c.created_at} now={now} />,
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
              drop: 2,
              cell: (c) =>
                c.method === "token" ? (
                  <GrantTags grants={c.grants} />
                ) : (
                  <EmptyValue label="Its holder's grants" />
                ),
            },
            ...(manages
              ? [
                  {
                    key: "revoke",
                    header: "",
                    label: "Revoke",
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
      {!machine && (
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
                // A TIMESTAMP, so it is the column that shrinks: it took the
                // grid's flexible width and drew it nearly empty while the
                // State beside it, capped as a shrink column, cut a session's
                // reason off at its edge with no ellipsis and no way to read
                // the rest.
                key: "started",
                header: "Session",
                shrink: true,
                sortValue: (s) => s.created_at ?? "",
                cell: (s) => <DateCell at={s.created_at} now={now} />,
              },
              {
                key: "state",
                header: "State",
                sortValue: (s) => (s.live ? 0 : 1),
                cell: (s) =>
                  s.live ? (
                    <Tag
                      size="sm"
                      variant="success"
                      title={s.enrolment_only ? "May only enrol a second factor" : undefined}
                    >
                      {s.enrolment_only ? "Enrolling a factor" : "Signed in"}
                    </Tag>
                  ) : (
                    // WHY, SAID: it rode in a tooltip, so a session a password
                    // change ended read as one that merely had. ONE LINE, cut
                    // with an ellipsis and whole on its title where the column
                    // is narrower than the sentence.
                    <span className="row gap-1" style={{ minWidth: 0 }}>
                      <Tag size="sm" variant="neutral">
                        Ended
                      </Tag>
                      {s.ended_reason && (
                        <span className="t-caption muted truncate" title={s.ended_reason}>
                          {s.ended_reason}
                        </span>
                      )}
                    </span>
                  ),
              },
              {
                key: "ends",
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
                    <DateCell at={s.ended_at ?? s.expires_at} now={now} />
                  ),
              },
            ]}
          />
        </QueryState>
      )}
    </Card>
  );
}

/**
 * What a reset second factor leaves its person to do, as this deployment
 * decides it (`api.auth.totp`): enrol a new one before anything else opens, or
 * nothing — a deployment where a factor is optional signs them in on their
 * password alone, and promising an enrolment there was a promise nothing
 * keeps. Said only once the setting is read.
 */
function AfterFactorReset() {
  const config = useRest("/auth/config", (signal) => auth.config(signal));
  switch (config.data?.second_factor) {
    case "required":
      return <>They enrol a new factor at their next sign-in, before anything else opens.</>;
    case "optional":
      return <>They sign in with their password alone until they set up a new one.</>;
  }
  return null;
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
  children,
}: {
  title: string;
  confirm: string;
  danger?: boolean;
  typeToConfirm?: string;
  request: Parameters<ReturnType<typeof useIamGesture>["run"]>[0];
  onClose: () => void;
  onDone: () => void;
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
 * their credentials, where it can be revoked.
 */
function ResetLink({
  row,
  who,
  onClose,
  onDone,
}: {
  row: DirectoryRow;
  who: string;
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
        title={`Password reset link for ${who}`}
        onClose={onClose}
        stackBody
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <ShownOnce label="Reset link" value={url}>
          This link sets a new password for {who} once, and expires {fmtDateTime(expires)}. It is
          shown only now — send it to them yourself. Setting the password ends every session and
          token they hold; they sign in afterwards, with their second factor if they hold one.
        </ShownOnce>
        <IamOutcome answer={write.answer} />
      </Modal>
    );
  }
  return (
    <ConfirmDialog
      title={`Issue a password reset link for ${who}?`}
      confirm="Issue link"
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
      The link lets {who} choose a new password once, within a day. It revokes any link issued for
      them before.
    </ConfirmDialog>
  );
}
