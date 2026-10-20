/**
 * The identity directory and its reads, `GET /iam/...`, as the engine answers
 * them (`internal/api/iamapi`).
 *
 * THE DIRECTORY IS WHAT A PRINCIPAL IS RESOLVED THROUGH: a person's own row,
 * a service account's, and the machine row that binds a Tier A token to a
 * seat. A scenario that signs the browser in as somebody writes the row that
 * makes them so (`enrol`), so the account page, People & access and the seat
 * listing read the same people the session answer names.
 *
 * WHAT CHANGES THE DIRECTORY IS REFUSED OUT LOUD: an invitation, a removal, a
 * grant, a credential minted or revoked. The demo holds the directory a
 * scenario wrote, and a write that appeared to work would draw a person
 * nobody could sign in as.
 */

import type { HumanSeat } from "~/contract/identity.ts";
import type { DemoCompany } from "./company.ts";
import { documentOf } from "./company.ts";
import { derive } from "./derive.ts";
import type { DemoEngine } from "./engine.ts";
import { fixtureId } from "./ids.ts";
import { machineToken, personSession, tokenSession } from "./principal.ts";
import { LOG, notModelled } from "./refusal.ts";
import type { RestReply, RestRequest } from "./rest.ts";

/** One directory row (`iamdomain.PersonRow`, as `iamapi.personView` writes it). */
export interface DemoDirectoryRow {
  id: string;
  /** `person` or `machine` (`iam.Kind`). */
  kind: "person" | "machine";
  /** `invited`, `enrolling`, `active`, `suspended` or `retired` (`iam.Stage`). */
  stage: string;
  login: string;
  name?: string;
  email?: string;
  /** The seat the row is bound to, by handle. */
  seat?: string;
  /** The grants the row holds, as stored: never expanded. */
  grants: string[];
  revocation_epoch: number;
  created_at: string;
  updated_at: string;
  version: number;
}

/** One credential (`iamapi.credentialView`): never a verifier. */
export interface DemoCredential {
  id: string;
  person: string;
  /** `password`, `totp`, `recovery`, `token` or `reset`. */
  method: string;
  label?: string;
  created_at: string;
  expires_at?: string;
  revoked: boolean;
  grants?: string[];
}

/** One session (`iamapi.sessionView`), by the subject its bearer names. */
export interface DemoSessionRow {
  lineage: string;
  /** A person's id, or a Tier A token's login for a session exchanged from one. */
  person: string;
  epoch: number;
  created_at: string;
  expires_at: string;
  live: boolean;
}

/** The identity estate: its rows, their credentials and their sessions. */
export interface DemoDirectory {
  /** The identity log's position, which an answer states as `position`. */
  logSeq: number;
  people: DemoDirectoryRow[];
  credentials: DemoCredential[];
  sessions: DemoSessionRow[];
}

/** `topics.IamLogStream`. */
const IAM_STREAM = "CREWLET_IAM_LOG";

const position = (c: DemoCompany) => `${IAM_STREAM}@1:${c.directory.logSeq}`;

/**
 * Write `row` into the directory as an enrolment or an update does: by its
 * login, and as the ONE holder of its seat, since the directory refuses two
 * rows on one seat (`ClaimsBySeat`) and binding somebody else to it unbinds
 * whoever held it.
 */
export function enrol(
  c: DemoCompany,
  row: Omit<DemoDirectoryRow, "revocation_epoch" | "updated_at" | "version">,
  extra: { credentials?: DemoCredential[]; session?: DemoSessionRow } = {},
): void {
  const dir = c.directory;
  dir.logSeq += 1;
  if (row.seat) {
    for (const other of dir.people) {
      if (other.login !== row.login && other.seat === row.seat) {
        delete other.seat;
        other.updated_at = c.now;
        other.version = dir.logSeq;
      }
    }
  }
  const existing = dir.people.find((p) => p.login === row.login);
  const next: DemoDirectoryRow = {
    ...row,
    revocation_epoch: existing?.revocation_epoch ?? 0,
    updated_at: c.now,
    version: dir.logSeq,
  };
  if (!row.seat) delete next.seat;
  dir.people = existing
    ? dir.people.map((p) => (p === existing ? next : p))
    : [...dir.people, next];
  for (const credential of extra.credentials ?? []) {
    dir.credentials = [...dir.credentials.filter((x) => x.id !== credential.id), credential];
  }
  if (extra.session) {
    const session = extra.session;
    dir.sessions = [...dir.sessions.filter((s) => s.lineage !== session.lineage), session];
  }
}

/** A directory holding nobody. */
export function emptyDirectory(): DemoDirectory {
  return { logSeq: 0, people: [], credentials: [], sessions: [] };
}

/** One hour: what a session exchanged from a Tier A token lasts (`tokenLifetime`). */
const TOKEN_SESSION_MS = 3_600_000;
/** A person's session's absolute lifetime (`config.DefaultSessionAbsolute`). */
const PERSON_SESSION_MS = 168 * 3_600_000;

/**
 * Sign the browser in with Tier A token `tokenId`, exchanged for a session at
 * `at` (`POST /auth/token`): the node holds the token, the directory holds the
 * machine row `token:<id>` that binds it to `seat` (none for an unbound token),
 * and the session's subject is the token's login.
 */
export function signInWithToken(
  c: DemoCompany,
  o: { tokenId: string; seat: string; grants: readonly string[]; at: string },
): void {
  const login = `token:${o.tokenId}`;
  const lineage = fixtureId(`token-session-${o.tokenId}`);
  const expiresAt = new Date(Date.parse(o.at) + TOKEN_SESSION_MS).toISOString();
  if (!c.engine.tokens.includes(o.tokenId)) c.engine.tokens = [...c.engine.tokens, o.tokenId];
  const held = c.directory.people.find((p) => p.login === login);
  if (o.seat || held) {
    // THE BINDING ROW: a machine under the token's login, with no grants of
    // its own (`crewlet iam create -kind machine -login token:<id> -seat <handle> -grants none`).
    enrol(c, {
      id: held?.id ?? fixtureId(`directory-${login}`),
      kind: "machine",
      stage: "active",
      login,
      seat: o.seat,
      grants: [],
      created_at: held?.created_at ?? o.at,
    });
  }
  c.directory.sessions = [
    ...c.directory.sessions.filter((s) => s.lineage !== lineage),
    { lineage, person: login, epoch: 0, created_at: o.at, expires_at: expiresAt, live: true },
  ];
  c.principal = tokenSession({
    tokenId: o.tokenId,
    seat: o.seat,
    grants: o.grants,
    lineage,
    expiresAt,
  });
}

/**
 * Sign the browser in as a person with their password at `at`: their own
 * active row on the human seat they hold, a password and the second factor
 * this node requires, and the session.
 */
export function signInAsPerson(
  c: DemoCompany,
  o: { name: string; login: string; seat: string; grants: readonly string[]; at: string },
): void {
  const id = fixtureId(`person-${o.login}`);
  const lineage = fixtureId(`session-${o.login}`);
  const expiresAt = new Date(Date.parse(o.at) + PERSON_SESSION_MS).toISOString();
  const enrolledAt = new Date(Date.parse(o.at) - 7 * 86_400_000).toISOString();
  enrol(
    c,
    {
      id,
      kind: "person",
      stage: "active",
      login: o.login,
      name: o.name,
      seat: o.seat,
      grants: [...o.grants],
      created_at: enrolledAt,
    },
    {
      credentials: ["password", "totp", "recovery"].map((method) => ({
        id: fixtureId(`credential-${o.login}-${method}`),
        person: id,
        method,
        created_at: enrolledAt,
        revoked: false,
      })),
      session: {
        lineage,
        person: id,
        epoch: 0,
        created_at: o.at,
        expires_at: expiresAt,
        live: true,
      },
    },
  );
  c.principal = personSession({
    person: id,
    login: o.login,
    name: o.name,
    seat: o.seat,
    grants: o.grants,
    lineage,
    expiresAt,
  });
}

/**
 * Have the browser present a service account's machine token: its active
 * machine row, bound to `seat` or to none, and the token, minted with `grants`.
 */
export function presentMachineToken(
  c: DemoCompany,
  o: { login: string; grants: readonly string[]; seat?: string; name?: string; at: string },
): void {
  const id = fixtureId(`machine-${o.login}`);
  const token = fixtureId(`pat-${o.login}`);
  const created = new Date(Date.parse(o.at) - 30 * 86_400_000).toISOString();
  enrol(
    c,
    {
      id,
      kind: "machine",
      stage: "active",
      login: o.login,
      ...(o.name ? { name: o.name } : {}),
      seat: o.seat ?? "",
      grants: [...o.grants],
      created_at: created,
    },
    {
      credentials: [
        {
          id: token,
          person: id,
          method: "token",
          label: o.login.split(":").slice(1).join(":"),
          created_at: created,
          revoked: false,
          grants: [...o.grants],
        },
      ],
    },
  );
  c.principal = machineToken({
    person: id,
    login: o.login,
    grants: o.grants,
    token,
    ...(o.seat ? { seat: o.seat } : {}),
    ...(o.name ? { name: o.name } : {}),
  });
}

/** Whether the company has its first person (`Health.Identity`): `ready` or `unclaimed`. */
export function identityOf(c: DemoCompany): "ready" | "unclaimed" {
  return c.directory.people.some((p) => p.kind === "person" && p.stage === "active")
    ? "ready"
    : "unclaimed";
}

/** The refusal the authority table answers a read with (`authz.EnvelopeRefusal`). */
const refused = (grants: string[]): RestReply => ({
  status: 403,
  body: {
    error: "unauthorized",
    message:
      "You don't have the access this needs (Admin, Tracker or Knowledge). Ask an admin for it.",
    reason: "no_grant",
    grants,
  },
});

const notFound = (): RestReply => ({
  status: 404,
  body: { error: "not_found", message: "There is no such record here." },
});

/** Whether the caller may read `owner`'s directory record (`ClassDirectory`): their own, or Admin. */
function mayRead(c: DemoCompany, owner: string): boolean {
  return (owner !== "" && owner === c.principal.person) || c.principal.grants.includes("admin");
}

/** The human seats and who holds each (`iamapi.GetSeats`), masked per reader. */
function seatsOf(c: DemoCompany, unheld: boolean): HumanSeat[] {
  const admin = c.principal.grants.includes("admin");
  const derived = derive(documentOf(c));
  const homes = new Map<string, string>();
  for (const unit of derived.units ?? []) {
    for (const handle of unit.seats ?? []) homes.set(handle, unit.id);
  }
  const rows: HumanSeat[] = [];
  for (const seat of derived.seats ?? []) {
    if (seat.kind !== "human") continue;
    const holder = c.directory.people.find((p) => p.seat === seat.handle && p.stage !== "retired");
    if (unheld && holder) continue;
    const row: HumanSeat = { handle: seat.handle, name: seat.name };
    const unit = homes.get(seat.handle);
    if (unit) row.unit = unit;
    if (holder) {
      row.holder = { kind: holder.kind, ...(holder.name ? { name: holder.name } : {}) };
      // THE ACCOUNT reaches an admin and the holder themselves.
      if (admin || holder.id === c.principal.person) {
        Object.assign(row.holder, { person: holder.id, login: holder.login, stage: holder.stage });
      }
    }
    rows.push(row);
  }
  return rows;
}

/** `GET /iam/check`: what is wrong with the directory, as the engine finds it. */
function checkOf(c: DemoCompany, now: Date) {
  const findings: Record<string, string>[] = [];
  let admins = 0;
  for (const row of c.directory.people) {
    const live = c.directory.credentials.some(
      (x) =>
        x.person === row.id &&
        !x.revoked &&
        !(x.expires_at && Date.parse(x.expires_at) <= now.getTime()),
    );
    if (row.stage === "active" && !live) {
      findings.push({
        kind: "person_without_credential",
        person: row.id,
        login: row.login,
        detail:
          row.kind === "machine"
            ? `This service account has no working access token. Create one (POST /iam/credentials?person=${row.id}), or remove it.`
            : `This person can't sign in. Send them a password reset link (POST /iam/people/${row.id}/password-reset), or remove them.`,
      });
    }
    const enrolled = c.directory.credentials.some(
      (x) => x.person === row.id && !x.revoked && x.method !== "reset",
    );
    if (row.kind === "person" && row.stage === "active" && row.grants.includes("admin") && enrolled)
      admins += 1;
  }
  if (admins === 0) {
    findings.unshift({
      kind: "no_admin_holder",
      detail: "No active person holds admin. Only an API token can invite people or change access.",
    });
  }
  return { findings, position: position(c), people_with_admin: admins, bindings_unchecked: 0 };
}

/** Whether `path` is one this module answers. */
export function isIamPath(path: string): boolean {
  return path === "/iam" || path.startsWith("/iam/");
}

export function handleIam(engine: DemoEngine, request: RestRequest): RestReply {
  const c = engine.company;
  const { method, path, query } = request;
  const admin = c.principal.grants.includes("admin");
  if (method !== "GET") {
    console.warn(LOG, `${method} ${path} is not modelled; it was refused.`);
    return {
      status: 409,
      body: {
        error: "not_modelled",
        detail: notModelled("Changing the identity directory"),
      },
    };
  }
  const person = /^\/iam\/people\/([^/]+)(\/sessions)?$/.exec(path);
  if (person) {
    const id = decodeURIComponent(person[1]!);
    if (!mayRead(c, id)) return refused(["admin"]);
    if (person[2]) {
      return {
        status: 200,
        body: { sessions: c.directory.sessions.filter((s) => s.person === id) },
      };
    }
    const row = c.directory.people.find((p) => p.id === id);
    return row ? { status: 200, body: row } : notFound();
  }
  switch (path) {
    case "/iam/people":
      if (!admin) return refused(["admin"]);
      return { status: 200, body: { people: c.directory.people, next: "", position: position(c) } };
    case "/iam/invitations":
      if (!admin) return refused(["admin"]);
      return { status: 200, body: { invitations: [], next: "", position: position(c) } };
    case "/iam/credentials": {
      const subject = query.get("person")?.trim() || c.principal.person;
      if (!mayRead(c, subject)) return refused(["admin"]);
      return {
        status: 200,
        body: { credentials: c.directory.credentials.filter((x) => x.person === subject) },
      };
    }
    case "/iam/check":
      if (!admin) return refused(["admin"]);
      return { status: 200, body: checkOf(c, engine.now()) };
    case "/iam/node-tokens": {
      if (!admin) return refused(["admin"]);
      const tokens = [...c.engine.tokens].sort().map((id) => {
        const login = `token:${id}`;
        const row = c.directory.people.find((p) => p.login === login);
        return {
          id,
          login,
          row: row ? "held" : "none",
          ...(row
            ? { person: row.id, stage: row.stage, ...(row.seat ? { seat: row.seat } : {}) }
            : {}),
          company_writer: c.engine.company_writers.includes(id),
        };
      });
      return { status: 200, body: { tokens, company_writers: c.engine.company_writers } };
    }
    case "/iam/seats": {
      const unheld = query.get("unheld") === "true";
      if (unheld && !admin) return refused(["admin"]);
      return { status: 200, body: { seats: seatsOf(c, unheld) } };
    }
    case "/iam/audit":
      if (!admin) return refused(["admin"]);
      // THE IDENTITY TRAIL IS NOT KEPT by the demo: it answers the page the
      // engine answers past its end, and says so nowhere it could be misread.
      return { status: 200, body: { events: [], next: 0, position: position(c) } };
  }
  console.warn(LOG, `GET ${path} is not a path the demo engine serves; it was answered 404.`);
  return {
    status: 404,
    body: { error: "no_route", detail: `GET ${path} is not served by the demo engine.` },
  };
}
