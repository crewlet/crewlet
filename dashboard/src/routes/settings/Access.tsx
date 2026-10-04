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
 * # Read from the identity directory, never written here
 *
 * Five answers of `/iam`, none of them changed from this page:
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
 *
 * Inviting, granting, binding, suspending and revoking are `crewlet iam`'s: a
 * write about who may do what is a step-up gesture this page does not make.
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

import { useMemo } from "react";
import {
  Card,
  EMPTY_VALUE,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { KeyGlyph, TriangleAlertGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href, useParam } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { documentUnits, indexOrg, unitByKey } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { useViewer } from "~/lib/viewer.ts";
import { rest } from "~/protocol/index.ts";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";

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
  /** `password`, `totp`, `recovery` or `token`. */
  method: string;
  label?: string;
  created_at?: string;
  expires_at?: string;
  revoked_at?: string;
  /** Revoked OR expired, as the engine judged it at the read. */
  revoked: boolean;
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
  seat?: string;
  /** `bound`, `unbound`, `dangling` or `unknown`. */
  binding: string;
  detail?: string;
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

/** Whether a token's row names a seat the chart still holds. */
export const BINDING_WORDS: Record<string, Words> = {
  bound: {
    label: "Acts as its seat",
    tone: "success",
    hint: "The row names a human seat the chart holds, so what this token writes is recorded as that seat.",
  },
  unbound: {
    label: "Acts as itself",
    tone: "neutral",
    hint: "The row names no seat, so the token writes under its own login.",
  },
  dangling: {
    label: "Seat gone",
    tone: "warning",
    hint: "The row names a seat this node's chart no longer holds as a human seat, so the token is refused acting as it.",
  },
  unknown: {
    label: "Could not tell",
    tone: "neutral",
    hint: "This node could not read the chart just now to say whether the seat is still there.",
  },
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
  const rows: DirectoryRow[] = [];
  let after = "";
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE) });
    if (after) params.set("after", after);
    const page = (await rest.get(`/iam/people?${params}`, signal)) as DirectoryPage | null;
    rows.push(...(page?.people ?? []));
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

export function PeopleAndAccess() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const [opened, setOpened] = useParam("person", "");

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
  const boundTokens = tokenRows.filter((t) => t.binding === "bound").length;
  const openedRow = people.find((p) => p.id === opened) ?? null;

  return (
    <>
      <PageActions>
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
        Who can reach this company and as whom: the identity directory, the human seats and who
        holds them, and this node&rsquo;s API tokens. Read-only — inviting, granting, binding and
        revoking are <InlineCode>crewlet iam</InlineCode>&rsquo;s. This screen never holds a
        credential&rsquo;s value.
      </PageNote>

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
                sub={tokens.data ? `${boundTokens} acting as a seat` : "not read"}
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
                hint: "Invite the first person with crewlet iam invite, under this node's API token.",
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
                  cell: (p) => {
                    if (!p.seat) return <EmptyValue label="Bound to no seat" />;
                    const seat = index.byHandle.get(p.seat);
                    return <SeatCell handle={p.seat} name={seat?.name ?? p.seat} kind="human" />;
                  },
                },
                {
                  key: "grants",
                  header: "Grants",
                  drop: 1,
                  sortValue: (p) => (p.grants ?? []).length,
                  cell: (p) => <Grants grants={p.grants} />,
                },
              ]}
            />
          </Card>

          {openedRow && <Principal row={openedRow} />}

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
                    key: "binding",
                    header: "Acts as",
                    floor: "10rem",
                    sortValue: (t) => t.binding,
                    cell: (t) => <TokenBinding token={t} index={index} />,
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
  if (row.sealed) {
    return (
      <Tag
        size="sm"
        variant="warning"
        title="This node's keyring cannot open this row's name and address: a key was dropped before crewlet secrets rekey moved them off it, or the estate was restored under another keyring."
      >
        sealed
      </Tag>
    );
  }
  return <TextCell>{row.name || row.login || row.id}</TextCell>;
}

/** A row's declared grants, as written. */
function Grants({ grants }: { grants?: string[] | null }) {
  if (!grants || grants.length === 0) return <EmptyValue label="No grants" />;
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {grants.map((g) => (
        <Tag key={g} size="sm" appearance="outline">
          <span className="mono">{g}</span>
        </Tag>
      ))}
    </span>
  );
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

/** What a token acts as: the seat it is bound to, or why it is not. */
function TokenBinding({ token, index }: { token: NodeToken; index: ReturnType<typeof indexOrg> }) {
  const words = BINDING_WORDS[token.binding];
  const tag = (
    <Tag
      size="sm"
      variant={words?.tone ?? "neutral"}
      title={token.detail ? sentence(token.detail) : words?.hint}
    >
      {words?.label ?? token.binding}
    </Tag>
  );
  if (token.binding !== "bound" || !token.seat) return tag;
  const seat = index.byHandle.get(token.seat);
  return (
    <span className="row gap-2">
      <SeatCell handle={token.seat} name={seat?.name ?? token.seat} kind="human" />
      {tag}
    </span>
  );
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
 * One principal opened: the credentials they prove themselves with and the
 * sessions they are signed in with, read for this row alone.
 */
function Principal({ row }: { row: DirectoryRow }) {
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
  const who = row.sealed ? row.id : row.name || row.login || row.id;
  return (
    <Card padding="none">
      <Card.Header icon={<KeyGlyph size="sm" />}>{who}: credentials and sessions</Card.Header>
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
                    {c.revoked_at ? "Revoked" : "Expired"}
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
              cell: (c) =>
                c.expires_at ? (
                  <DateCell at={c.expires_at} now={now} />
                ) : (
                  <EmptyValue label="Does not expire" />
                ),
            },
            {
              key: "grants",
              header: "Carries",
              drop: 2,
              cell: (c) =>
                c.method === "token" ? (
                  <Grants grants={c.grants} />
                ) : (
                  <EmptyValue label="Its holder's grants" />
                ),
            },
          ]}
        />
      </QueryState>
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
              key: "started",
              header: "Session",
              sortValue: (s) => s.created_at ?? "",
              cell: (s) => <DateCell at={s.created_at} now={now} />,
            },
            {
              key: "state",
              header: "State",
              shrink: true,
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
                  <Tag size="sm" variant="neutral" title={s.ended_reason}>
                    Ended
                  </Tag>
                ),
            },
            {
              key: "ends",
              header: "Ends",
              shrink: true,
              drop: 1,
              sortValue: (s) => s.ended_at ?? s.expires_at ?? "",
              cell: (s) => <DateCell at={s.ended_at ?? s.expires_at} now={now} />,
            },
          ]}
        />
      </QueryState>
    </Card>
  );
}
