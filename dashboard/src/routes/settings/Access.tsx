/**
 * Settings › People & access: who can reach this company, and as whom.
 *
 * # Read from the identity directory, never written here
 *
 * A person, a service account and every credential they prove themselves
 * with live in the identity estate (`internal/iamdomain`), read through
 * `/iam`. This screen draws five of its answers and changes none of them:
 *
 *  - `GET /iam/people` — every principal the directory holds: its kind, its
 *    stage, its login, the grants its row declares and the seat it is bound
 *    to. Walked to its last page, because a directory drawn from its first
 *    two hundred rows is a company that looks smaller than it is.
 *  - `GET /iam/credentials?person=` and `GET /iam/people/{id}/sessions` — the
 *    credentials one principal holds (the machine tokens it minted among
 *    them) and the sessions it is signed in with, read for the row a reader
 *    opens rather than for everybody at once.
 *  - `GET /iam/node-tokens` — THIS NODE's Tier A tokens by label, each joined
 *    to the directory row its `token:<id>` login names, because a label
 *    mistyped on either side leaves a token acting as itself while its
 *    operator believes it acts as a seat, and no directory read alone can see
 *    a label.
 *  - `GET /iam/check` — what the directory reports wrong: nobody left who can
 *    administer it, somebody active with no credential, a binding whose seat
 *    is gone, a grant this node's ceiling withholds, a claim two people hold,
 *    a reservation nobody completed.
 *  - `GET /chart/check` — the two org-chart findings about people: a human
 *    seat nobody in the directory holds, and one no agent can reach.
 *
 * Inviting, granting, binding, suspending and revoking are `crewlet iam`'s:
 * a write about who may do what is a step-up gesture this page does not make.
 *
 * # Labels and verifiers, never values
 *
 * No answer here has a member a credential's value could travel in — the
 * engine stores verifiers, and a Tier A token is named by its label — and
 * this screen holds none either.
 *
 * # Decided by the engine, drawn as it decided
 *
 * Every read is `people:manage` or `audit:read` (`authz.ActionDirectoryRead`)
 * but the chart's report, which is `audit:read` alone. A reader refused sees
 * the refusal and the grants that would have admitted them — never an empty
 * company — and a reader holding `people:manage` alone sees every card but
 * that one, which says so.
 */

import { useCallback, useMemo, useState } from "react";
import {
  Callout,
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
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useRestRead, type RestRead } from "~/lib/restRead.ts";
import { indexOrg, seatByAddress, type Seat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { rest } from "~/protocol/index.ts";

// ---------------------------------------------------------------------------
// The wire, as internal/api/iamapi and internal/api/chartapi write it
// ---------------------------------------------------------------------------

/** One directory row (`personView`): the sealed values opened, the verifiers absent. */
export interface DirectoryRow {
  id: string;
  /** `person`, `machine`, `seat` or `engine` (iam.Kind). */
  kind: string;
  /** `invited`, `enrolling`, `active`, `suspended` or `retired` (iam.Stage). */
  stage: string;
  login?: string;
  name?: string;
  email?: string;
  /** Ciphertext this node's keyring cannot open — a STATE, never an empty name. */
  sealed?: boolean;
  /** An enrolment whose claims landed and whose content record has not. */
  reserved?: boolean;
  /** The bound seat's IDENTITY — the handle it was created under (ADR-0027). */
  seat?: string;
  grants?: string[] | null;
  /** `none`, `read` or `write` (iam.Colleague). */
  colleague?: string;
  created_at?: string;
}

interface DirectoryPage {
  people: DirectoryRow[] | null;
  next: string;
  position: string;
}

/** One credential (`credentialView`). No verifier, ever. */
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

/** One session (`sessionView`): its lineage, nothing a session resumes from. */
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

/** One of this node's Tier A tokens, by label, joined to its directory row. */
export interface NodeToken {
  id: string;
  /** `token:<id>`, the login the token acts under. */
  login: string;
  /** `none`, `reserved` or `held`. */
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
  claim?: string;
  people?: string[] | null;
  detail: string;
}

interface DirectoryCheck {
  findings: DirectoryFinding[] | null;
  people_with_people_manage: number;
  bindings_unchecked: number;
}

/** One row of the chart's report (`chartapi.Finding`). */
export interface ChartFinding {
  kind: string;
  severity: string;
  object: string;
  detail: string;
  remedy?: string;
}

interface ChartCheck {
  report: {
    findings: ChartFinding[] | null;
    seats: number;
    unchecked?: number;
    evaluated: boolean;
  };
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
 * The five stages, in the engine's words (`internal/iam/principal.go`). A stage
 * this build has no word for is drawn as written — the engine adds a value
 * additively, and naming it beats dropping it.
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

/** The four principal kinds, as a reader says them. */
const KIND_WORDS: Record<string, string> = {
  person: "Person",
  machine: "Service account",
  seat: "Seat",
  engine: "Engine",
};

/** How far into the company's own work a principal reaches (iam.Colleague). */
const COLLEAGUE_WORDS: Record<string, string> = {
  none: "No reach",
  read: "Reads the work",
  write: "Works here",
};

/** What a Tier A token's login names in the directory. */
export const TOKEN_ROW_WORDS: Record<string, Words> = {
  none: {
    label: "No row",
    tone: "neutral",
    hint: "Nobody in the directory holds this login, so the token acts as itself and is bound to no seat. Bind it with crewlet iam if it should act as one.",
  },
  reserved: {
    label: "Reservation",
    tone: "warning",
    hint: "An enrolment claimed this login and stopped before its record landed. Remove the reservation's id to release it.",
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

/** The directory report's six kinds, worst first (`iamapi.FindingKinds`). */
export const DIRECTORY_FINDING_WORDS: Record<string, { label: string; tone: Tone }> = {
  no_people_manage_holder: { label: "Nobody can administer", tone: "danger" },
  person_without_credential: { label: "Active, no credential", tone: "warning" },
  binding_dangling: { label: "Seat gone", tone: "warning" },
  grant_clamped_by_ceiling: { label: "Grant withheld here", tone: "neutral" },
  claim_duplicated: { label: "Held twice", tone: "danger" },
  claim_orphaned: { label: "Reservation left behind", tone: "warning" },
};

/** The two chart findings this screen draws, and the words for each. */
export const CHART_FINDING_WORDS: Record<string, { label: string; tone: Tone }> = {
  seat_unheld: { label: "Nobody holds it", tone: "warning" },
  seat_unreachable: { label: "No contact", tone: "warning" },
};

/** What a credential method is called. */
const METHOD_WORDS: Record<string, string> = {
  password: "Password",
  totp: "Authenticator app",
  recovery: "Recovery codes",
  token: "Machine token",
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
const cadence = () => POLL_MS;

/** A page as large as the directory serves (`iamdomain.MaxPageSize`). */
const PAGE = 200;

/** Every row of the directory, walked to its last page. */
async function readDirectory(signal: AbortSignal): Promise<DirectoryRow[]> {
  const rows: DirectoryRow[] = [];
  let after = "";
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE) });
    if (after) params.set("after", after);
    const page = (await rest.get(`/iam/people?${params}`, signal)) as DirectoryPage | null;
    rows.push(...(page?.people ?? []));
    // A CURSOR THAT DOES NOT MOVE ends the walk rather than looping on it.
    if (!page?.next || page.next === after) return rows;
    after = page.next;
  }
}

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/** A principal as a reader names it: their name, else their login, else their id. */
function whoOf(row: DirectoryRow | undefined, id: string): string {
  return row?.name || row?.login || id;
}

export function PeopleAndAccess() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatOf = useCallback((identity: string) => seatByAddress(index, identity), [index]);

  const directory = useRestRead("/iam/people", readDirectory, { cadence, refetchOnFocus: true });
  // EACH READ NAMES ITS OWN PATH at the call, so `protocol/proxy.test.ts` can
  // hold the dev server's proxy to every one of them.
  const tokens = useRestRead(
    "/iam/node-tokens",
    async (signal) =>
      (await rest.get("/iam/node-tokens", signal)) as { tokens: NodeToken[] | null } | null,
    { cadence, refetchOnFocus: true },
  );
  const check = useRestRead(
    "/iam/check",
    async (signal) => (await rest.get("/iam/check", signal)) as DirectoryCheck | null,
    { cadence, refetchOnFocus: true },
  );
  const chart = useRestRead(
    "/chart/check",
    async (signal) => (await rest.get("/chart/check", signal)) as ChartCheck | null,
    { cadence, refetchOnFocus: true },
  );

  const [opened, setOpened] = useState<string | null>(null);

  const people = useMemo(() => directory.data ?? [], [directory.data]);
  const byID = useMemo(() => new Map(people.map((p) => [p.id, p])), [people]);
  const nodeTokens = useMemo(() => tokens.data?.tokens ?? [], [tokens.data]);
  const openedRow = opened ? byID.get(opened) : undefined;

  const peopleColumns = useMemo(() => directoryColumns(seatOf), [seatOf]);
  const tokenColumns = useMemo(() => nodeTokenColumns(seatOf, byID), [seatOf, byID]);

  const activate = useCallback(
    (row: DirectoryRow) => setOpened((was) => (was === row.id ? null : row.id)),
    [],
  );
  const isOpened = useCallback((row: DirectoryRow) => row.id === opened, [opened]);

  // REFUSED OUTRIGHT: the directory is the screen, so a reader it was refused
  // to sees the refusal alone — no tiles over nothing, no empty lists.
  if (directory.failure?.error === "unauthorized" && directory.data === null) {
    return (
      <>
        <AccessNote />
        <QueryState
          error={directory.failure.error}
          refusal={directory.failure.refusal}
          loading={false}
        />
      </>
    );
  }

  const persons = people.filter((p) => p.kind === "person");
  const machines = people.filter((p) => p.kind === "machine");
  const activeOf = (rows: DirectoryRow[]) => rows.filter((p) => p.stage === "active").length;
  const boundTokens = nodeTokens.filter((t) => t.binding === "bound").length;
  const findings = findingsTile(check, chart);

  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["settings", "audit"])}>
          Identity trail →
        </a>
      </PageActions>
      <AccessNote />

      {directory.data !== null && (
        <Card padding="none">
          <StatGroup columns={4}>
            <StatCard
              icon={<UsersGlyph size="xs" />}
              label="People"
              value={persons.length}
              sub={`${activeOf(persons)} active`}
            />
            <StatCard
              icon={<KeyGlyph size="xs" />}
              label="Service accounts"
              value={machines.length}
              sub={`${activeOf(machines)} active`}
            />
            <StatCard
              icon={<KeyGlyph size="xs" />}
              label="API tokens here"
              value={tokens.data ? nodeTokens.length : EMPTY_VALUE}
              sub={tokens.data ? `${boundTokens} act as a seat` : "not read yet"}
            />
            <StatCard
              icon={<TriangleAlertGlyph size="xs" />}
              label="Findings"
              value={findings.value}
              sub={findings.sub}
            />
          </StatGroup>
        </Card>
      )}

      <Card padding="none">
        <Card.Header
          icon={<UsersGlyph size="sm" />}
          count={directory.data ? people.length : undefined}
        >
          People
        </Card.Header>
        {directory.loading && directory.data === null ? (
          <Skeleton variant="text" rows={6} label="Loading the directory" />
        ) : (
          <>
            <QueryState
              error={directory.failure?.error ?? null}
              refusal={directory.failure?.refusal ?? null}
              loading={false}
            />
            {directory.data !== null && (
              <DataGrid<DirectoryRow>
                rows={people}
                rowKey={(p) => p.id}
                defaultSort="who"
                columns={peopleColumns}
                onRowActivate={activate}
                isSelected={isOpened}
                empty={{
                  title: "Nobody is enrolled",
                  hint: "Invite the first person with crewlet iam invite, under a Tier A token.",
                  icon: "users",
                }}
              />
            )}
          </>
        )}
      </Card>

      {openedRow && (
        <PrincipalDetail key={openedRow.id} row={openedRow} onClose={() => setOpened(null)} />
      )}

      <Card padding="none">
        <Card.Header
          icon={<KeyGlyph size="sm" />}
          count={tokens.data ? nodeTokens.length : undefined}
        >
          API tokens on this node
        </Card.Header>
        <p className="t-caption muted" style={{ padding: "var(--spacing-3) var(--spacing-4) 0" }}>
          Declared under <InlineCode>api.auth.tokens</InlineCode> in this node&rsquo;s bootstrap
          file, and shown by label. A token acts under the login{" "}
          <InlineCode>token:&lt;id&gt;</InlineCode>, and the directory row holding that login is
          what binds it to a seat.
        </p>
        {tokens.loading && tokens.data === null ? (
          <Skeleton variant="text" rows={3} label="Loading this node's tokens" />
        ) : (
          <>
            <QueryState
              error={tokens.failure?.error ?? null}
              refusal={tokens.failure?.refusal ?? null}
              loading={false}
            />
            {tokens.data !== null && (
              <DataGrid<NodeToken>
                name="node-tokens"
                rows={nodeTokens}
                rowKey={(t) => t.id}
                defaultSort="id"
                columns={tokenColumns}
                empty={{
                  title: "No Tier A token on this node",
                  hint: "A node serving the API refuses to start without one, so this node serves none.",
                  icon: "key",
                }}
              />
            )}
          </>
        )}
      </Card>

      <DirectoryReport check={check} byID={byID} seatOf={seatOf} />
      <ChartReport chart={chart} seatOf={seatOf} />
    </>
  );
}

function AccessNote() {
  return (
    <PageNote>
      Who can reach this company and as whom: the people and service accounts the identity directory
      holds, the credentials each proves themselves with, this node&rsquo;s API tokens, and what the
      directory and the org chart report wrong. Read only — invite, grant, bind and revoke with{" "}
      <InlineCode>crewlet iam</InlineCode>. No credential&rsquo;s value reaches this page.
    </PageNote>
  );
}

/** The bound seat, by the chart's CURRENT handle, or the identity as written. */
function BoundSeat({ identity, seat }: { identity?: string; seat: Seat | null }) {
  if (!identity) return <EmptyValue label="No seat" />;
  if (!seat) return <KeyCell value={identity} />;
  return <SeatCell handle={seat.handle} name={seat.name} kind={seat.kind} />;
}

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

function directoryColumns(seatOf: (identity: string) => Seat | null): GridColumn<DirectoryRow>[] {
  return [
    {
      key: "who",
      header: "Who",
      floor: "12rem",
      sortValue: (p) => whoOf(p, p.id),
      cell: (p) => (
        <span className="col" style={{ minWidth: 0 }}>
          <span className="row gap-2">
            <span className="truncate">{whoOf(p, p.id)}</span>
            {p.sealed && (
              <Tag
                size="sm"
                variant="warning"
                title="This node's keyring cannot open this row's name or address: a key dropped before the values were moved off it, or a restore under another keyring."
              >
                sealed
              </Tag>
            )}
            {p.reserved && (
              <Tag
                size="sm"
                variant="warning"
                title="An enrolment claimed these and stopped before its record landed."
              >
                reservation
              </Tag>
            )}
          </span>
          {p.email && <span className="t-caption muted truncate">{p.email}</span>}
        </span>
      ),
    },
    {
      key: "kind",
      header: "Kind",
      shrink: true,
      drop: 3,
      sortValue: (p) => p.kind,
      cell: (p) => <TextCell>{KIND_WORDS[p.kind] ?? p.kind}</TextCell>,
    },
    {
      key: "login",
      header: "Login",
      shrink: true,
      drop: 2,
      sortValue: (p) => p.login ?? "",
      cell: (p) => (p.login ? <KeyCell value={p.login} /> : <EmptyValue label="No login" />),
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
      floor: "9rem",
      sortValue: (p) => p.seat ?? "",
      cell: (p) => <BoundSeat identity={p.seat} seat={p.seat ? seatOf(p.seat) : null} />,
    },
    {
      key: "grants",
      header: "Grants",
      drop: 1,
      sortValue: (p) => p.grants?.length ?? 0,
      cell: (p) => <Grants grants={p.grants} />,
    },
    {
      key: "colleague",
      header: "Reach",
      shrink: true,
      drop: 4,
      sortValue: (p) => p.colleague ?? "",
      cell: (p) =>
        p.colleague ? (
          <TextCell>{COLLEAGUE_WORDS[p.colleague] ?? p.colleague}</TextCell>
        ) : (
          <EmptyValue label="Not recorded" />
        ),
    },
    {
      key: "joined",
      header: "Since",
      shrink: true,
      drop: 5,
      sortValue: (p) => p.created_at ?? "",
      cell: (p) => <DateCell at={p.created_at} />,
    },
  ];
}

function nodeTokenColumns(
  seatOf: (identity: string) => Seat | null,
  byID: ReadonlyMap<string, DirectoryRow>,
): GridColumn<NodeToken>[] {
  return [
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
      drop: 2,
      sortValue: (t) => t.login,
      cell: (t) => <KeyCell value={t.login} />,
    },
    {
      key: "row",
      header: "Directory",
      shrink: true,
      sortValue: (t) => t.row,
      cell: (t) =>
        t.row === "held" && t.stage ? (
          <span className="row gap-1">
            <WordTag words={TOKEN_ROW_WORDS.held} fallback={t.row} />
            <WordTag words={STAGE_WORDS[t.stage]} fallback={t.stage} />
          </span>
        ) : (
          <WordTag words={TOKEN_ROW_WORDS[t.row]} fallback={t.row} />
        ),
    },
    {
      key: "principal",
      header: "Row",
      drop: 3,
      sortValue: (t) => (t.person ? whoOf(byID.get(t.person), t.person) : ""),
      cell: (t) =>
        t.person ? (
          <TextCell>{whoOf(byID.get(t.person), t.person)}</TextCell>
        ) : (
          <EmptyValue label="Nobody" />
        ),
    },
    {
      key: "seat",
      header: "Seat",
      floor: "9rem",
      sortValue: (t) => t.seat ?? "",
      cell: (t) => <BoundSeat identity={t.seat} seat={t.seat ? seatOf(t.seat) : null} />,
    },
    {
      key: "binding",
      header: "Acts as",
      shrink: true,
      sortValue: (t) => t.binding,
      cell: (t) => {
        const words = BINDING_WORDS[t.binding];
        return (
          <Tag
            size="sm"
            variant={words?.tone ?? "neutral"}
            title={t.detail ? sentence(t.detail) : words?.hint}
          >
            {words?.label ?? t.binding}
          </Tag>
        );
      },
    },
  ];
}

// ---------------------------------------------------------------------------
// One principal's credentials and sessions
// ---------------------------------------------------------------------------

const CREDENTIAL_COLUMNS: GridColumn<CredentialRow>[] = [
  {
    key: "method",
    header: "Method",
    shrink: true,
    sortValue: (c) => c.method,
    cell: (c) => <TextCell>{METHOD_WORDS[c.method] ?? c.method}</TextCell>,
  },
  {
    key: "label",
    header: "Label",
    sortValue: (c) => c.label ?? "",
    cell: (c) => (c.label ? <TextCell>{c.label}</TextCell> : <EmptyValue label="No label" />),
  },
  {
    key: "grants",
    header: "Carries",
    drop: 1,
    sortValue: (c) => c.grants?.length ?? 0,
    cell: (c) =>
      c.method === "token" ? <Grants grants={c.grants} /> : <EmptyValue label="Its owner's" />,
  },
  {
    key: "state",
    header: "State",
    shrink: true,
    sortValue: (c) => (c.revoked ? 1 : 0),
    cell: (c) =>
      !c.revoked ? (
        <Tag size="sm" variant="success">
          Live
        </Tag>
      ) : c.revoked_at ? (
        <Tag size="sm" variant="neutral">
          Revoked
        </Tag>
      ) : (
        <Tag size="sm" variant="neutral">
          Expired
        </Tag>
      ),
  },
  {
    key: "created",
    header: "Made",
    shrink: true,
    drop: 2,
    sortValue: (c) => c.created_at ?? "",
    cell: (c) => <DateCell at={c.created_at} />,
  },
  {
    key: "expires",
    header: "Expires",
    shrink: true,
    drop: 3,
    sortValue: (c) => c.expires_at ?? "",
    cell: (c) => (c.expires_at ? <DateCell at={c.expires_at} /> : <EmptyValue label="Never" />),
  },
];

const SESSION_COLUMNS: GridColumn<SessionRow>[] = [
  {
    key: "lineage",
    header: "Session",
    shrink: true,
    sortValue: (s) => s.lineage,
    cell: (s) => <KeyCell value={s.lineage} text={s.lineage.slice(0, 8)} />,
  },
  {
    key: "state",
    header: "State",
    shrink: true,
    sortValue: (s) => (s.live ? 0 : 1),
    cell: (s) => (
      <span className="row gap-1">
        {s.live ? (
          <Tag size="sm" variant="success">
            Signed in
          </Tag>
        ) : (
          <Tag size="sm" variant="neutral" title={s.ended_reason}>
            Ended
          </Tag>
        )}
        {s.enrolment_only && (
          <Tag
            size="sm"
            variant="warning"
            title="Opened on a password alone where a second factor is required: it may only enrol one."
          >
            enrolment only
          </Tag>
        )}
      </span>
    ),
  },
  {
    key: "started",
    header: "Started",
    shrink: true,
    sortValue: (s) => s.created_at ?? "",
    cell: (s) => <DateCell at={s.created_at} />,
  },
  {
    key: "ends",
    header: "Ends",
    shrink: true,
    drop: 1,
    sortValue: (s) => s.ended_at ?? s.expires_at ?? "",
    cell: (s) => <DateCell at={s.ended_at ?? s.expires_at} />,
  },
];

/** The empty lists a read that answered none draws: one identity each, held still. */
const NO_CREDENTIALS: CredentialRow[] = [];
const NO_SESSIONS: SessionRow[] = [];

function PrincipalDetail({ row, onClose }: { row: DirectoryRow; onClose: () => void }) {
  const id = encodeURIComponent(row.id);
  const credentials = useRestRead(
    `/iam/credentials?person=${id}`,
    async (signal) =>
      (await rest.get(`/iam/credentials?person=${id}`, signal)) as {
        credentials: CredentialRow[] | null;
      } | null,
    { cadence },
  );
  const sessions = useRestRead(
    `/iam/people/${id}/sessions`,
    async (signal) =>
      (await rest.get(`/iam/people/${id}/sessions`, signal)) as {
        sessions: SessionRow[] | null;
      } | null,
    { cadence },
  );
  const who = whoOf(row, row.id);
  return (
    <Card padding="none">
      <Card.Header icon={<KeyGlyph size="sm" />}>
        <span className="row gap-2">
          <span>{who}&rsquo;s credentials and sessions</span>
          <button type="button" className="t-link" onClick={onClose}>
            Close
          </button>
        </span>
      </Card.Header>
      <div className="col gap-3" style={{ padding: "var(--spacing-3) var(--spacing-4)" }}>
        <QueryState
          error={credentials.failure?.error ?? null}
          refusal={credentials.failure?.refusal ?? null}
          loading={credentials.loading}
        />
        {credentials.loading && credentials.data === null && (
          <Skeleton variant="text" rows={2} label="Loading credentials" />
        )}
        {credentials.data !== null && (
          <DataGrid<CredentialRow>
            name="credentials"
            rows={credentials.data.credentials ?? NO_CREDENTIALS}
            rowKey={(c) => c.id}
            defaultSort="method"
            columns={CREDENTIAL_COLUMNS}
            flush
            empty={{
              title: "No credential",
              hint:
                row.kind === "machine"
                  ? "A service account proves itself with a machine token an administrator mints for it."
                  : "They have not redeemed an invitation into a password yet.",
              icon: "key",
            }}
          />
        )}
        <QueryState
          error={sessions.failure?.error ?? null}
          refusal={sessions.failure?.refusal ?? null}
          loading={sessions.loading}
        />
        {sessions.loading && sessions.data === null && (
          <Skeleton variant="text" rows={2} label="Loading sessions" />
        )}
        {sessions.data !== null && (
          <DataGrid<SessionRow>
            name="sessions"
            rows={sessions.data.sessions ?? NO_SESSIONS}
            rowKey={(s) => s.lineage}
            defaultSort="started"
            columns={SESSION_COLUMNS}
            flush
            empty={{
              title: "No session",
              hint: "A session is what a browser holds once somebody signs in; a machine token opens none.",
              icon: "key",
            }}
          />
        )}
      </div>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// The two reports
// ---------------------------------------------------------------------------

function DirectoryReport({
  check,
  byID,
  seatOf,
}: {
  check: RestRead<DirectoryCheck | null>;
  byID: ReadonlyMap<string, DirectoryRow>;
  seatOf: (identity: string) => Seat | null;
}) {
  const findings = check.data?.findings ?? [];
  const unchecked = check.data?.bindings_unchecked ?? 0;
  const nameOf = (id: string) => whoOf(byID.get(id), id);
  return (
    <Card padding="none">
      <Card.Header
        icon={<TriangleAlertGlyph size="sm" />}
        count={check.data ? findings.length : undefined}
      >
        What the directory reports
      </Card.Header>
      <div className="col gap-2" style={{ padding: "var(--spacing-3) var(--spacing-4)" }}>
        {check.loading && check.data === null && (
          <Skeleton variant="text" rows={2} label="Loading the directory's report" />
        )}
        <QueryState
          error={check.failure?.error ?? null}
          refusal={check.failure?.refusal ?? null}
          loading={check.loading}
        />
        {check.data && unchecked > 0 && (
          // SAID RATHER THAN SILENT: "no dangling binding" and "this node's
          // chart could not say" are different answers.
          <Callout variant="warning">
            {plural(unchecked, "seat binding")} could not be checked on this node just now, so a
            binding whose seat is gone may be missing below.
          </Callout>
        )}
        {check.data && findings.length === 0 && unchecked === 0 && (
          <p className="t-body muted">The directory reports nothing wrong.</p>
        )}
        {findings.map((f, i) => (
          <DirectoryFindingRow key={`${f.kind}:${i}`} finding={f} nameOf={nameOf} seatOf={seatOf} />
        ))}
      </div>
    </Card>
  );
}

function DirectoryFindingRow({
  finding: f,
  nameOf,
  seatOf,
}: {
  finding: DirectoryFinding;
  nameOf: (id: string) => string;
  seatOf: (identity: string) => Seat | null;
}) {
  const words = DIRECTORY_FINDING_WORDS[f.kind];
  const holders = f.people ?? [];
  return (
    <div className="col gap-1" data-finding={f.kind}>
      <span className="row gap-2" style={{ flexWrap: "wrap" }}>
        <Tag size="sm" variant={words?.tone ?? "neutral"}>
          {words?.label ?? f.kind}
        </Tag>
        {f.person && <span className="t-body">{nameOf(f.person)}</span>}
        {!f.person && f.login && <span className="mono">{f.login}</span>}
        {f.claim && <span className="t-caption muted">{f.claim} claim</span>}
        {f.grant && <InlineCode>{f.grant}</InlineCode>}
        {f.seat && <BoundSeat identity={f.seat} seat={seatOf(f.seat)} />}
      </span>
      {holders.length > 0 && (
        <span className="t-caption">Held by {holders.map(nameOf).join(", ")}</span>
      )}
      <span className="t-caption muted">{sentence(f.detail)}</span>
    </div>
  );
}

/** The chart findings about people, in the report's own order. */
function chartPeopleFindings(chart: ChartCheck): ChartFinding[] {
  return (chart.report.findings ?? []).filter((f) => f.kind in CHART_FINDING_WORDS);
}

/**
 * The Findings tile: a COUNT only once every report this reader may read has
 * answered, and "nothing reported" only where each of them also evaluated
 * everything it was asked about.
 *
 * ABSENT EVIDENCE IS NOT A CLEAN BILL, and the tile is the first thing on the
 * page: it summed whatever had answered, so a directory report that failed
 * beside a clean chart, or a chart this node never evaluated, read "0 —
 * nothing reported" above the two cards saying otherwise. A chart report
 * REFUSED to this reader is the one absence that is settled rather than
 * pending — it takes `audit:read`, which `people:manage` alone does not carry
 * — so the count stands without it and the line says what it covers.
 */
function findingsTile(
  check: Pick<RestRead<DirectoryCheck | null>, "data" | "loading">,
  chart: Pick<RestRead<ChartCheck | null>, "data" | "loading" | "failure">,
): { value: number | string; sub: string } {
  const chartRefused = chart.data === null && chart.failure?.error === "unauthorized";
  if (check.data === null || (chart.data === null && !chartRefused)) {
    return {
      value: EMPTY_VALUE,
      sub: check.loading || chart.loading ? "not read yet" : "a report could not be read",
    };
  }
  const report = chart.data?.report;
  const count =
    (check.data.findings?.length ?? 0) + (chart.data ? chartPeopleFindings(chart.data).length : 0);
  const partial =
    check.data.bindings_unchecked > 0 ||
    (report !== undefined && (!report.evaluated || (report.unchecked ?? 0) > 0));
  return {
    value: count,
    sub:
      count > 0
        ? "listed below"
        : partial
          ? "not everything could be checked"
          : chartRefused
            ? "in the directory's report — the chart's needs audit:read"
            : "nothing reported",
  };
}

function ChartReport({
  chart,
  seatOf,
}: {
  chart: RestRead<ChartCheck | null>;
  seatOf: (identity: string) => Seat | null;
}) {
  const report = chart.data?.report;
  const findings = chart.data ? chartPeopleFindings(chart.data) : [];
  const unchecked = report?.unchecked ?? 0;
  return (
    <Card padding="none">
      <Card.Header icon={<UsersGlyph size="sm" />} count={chart.data ? findings.length : undefined}>
        Human seats nobody holds or reaches
      </Card.Header>
      <div className="col gap-2" style={{ padding: "var(--spacing-3) var(--spacing-4)" }}>
        {chart.loading && chart.data === null && (
          <Skeleton variant="text" rows={2} label="Loading the org chart's report" />
        )}
        <QueryState
          error={chart.failure?.error ?? null}
          refusal={chart.failure?.refusal ?? null}
          loading={chart.loading}
        />
        {report && !report.evaluated && (
          // ABSENT EVIDENCE IS NOT A CLEAN BILL.
          <Callout variant="warning">
            This node has not evaluated the org chart yet, so it cannot say which seats are held.
          </Callout>
        )}
        {report?.evaluated && unchecked > 0 && (
          <Callout variant="warning">
            {plural(unchecked, "human seat")} could not be asked about on this node just now, so a
            seat nobody holds may be missing below.
          </Callout>
        )}
        {report?.evaluated && findings.length === 0 && unchecked === 0 && (
          <p className="t-body muted">
            Every human seat is held by somebody in the directory and has a way to be reached.
          </p>
        )}
        {findings.map((f) => {
          const words = CHART_FINDING_WORDS[f.kind];
          const seat = seatOf(f.object);
          return (
            <div key={`${f.kind}:${f.object}`} className="col gap-1" data-finding={f.kind}>
              <span className="row gap-2" style={{ flexWrap: "wrap" }}>
                <Tag size="sm" variant={words?.tone ?? "neutral"}>
                  {words?.label ?? f.kind}
                </Tag>
                <BoundSeat identity={f.object} seat={seat} />
              </span>
              <span className="t-caption muted">{sentence(f.detail)}</span>
              {f.remedy && <span className="t-caption">{sentence(f.remedy)}</span>}
            </div>
          );
        })}
      </div>
    </Card>
  );
}
