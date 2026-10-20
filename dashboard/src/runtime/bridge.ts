/**
 * The page bridge's wire, written once for both ends: the runtime a page runs
 * (`runtime/runtime.ts`, the third build target, served on the artifact
 * origin) and the host that embedded it (the dashboard's artifact viewer).
 *
 * BELOW THIS, THE HANDSHAKE IS THE DESIGN SYSTEM'S (`@crewlethq/ui`'s
 * `FrameProtocol`): the page says hello carrying the nonce its frame was
 * minted with, and the host transfers one `MessagePort` to the window it
 * checked. Everything here travels on that port, which no other window can
 * address, and the host binds every call to the one artifact and version it
 * embedded: a page never names its own artifact, and never holds a
 * credential.
 *
 * EVERYTHING ARRIVING FROM THE OTHER END IS UNTRUSTED. Each reader returns
 * `null` for anything that is not exactly the shape it names; nothing is
 * cast. A value inside a document is preserved as the JSON it is (ADR-0006);
 * only the envelope is held to a shape.
 *
 * No React and no DOM at module scope: the runtime build imports this, and it
 * is small enough to sit in a page before any of the page's own scripts.
 */

// RELATIVE, like every contract import outside the app: this module is also
// built into the runtime alone, where the `~` alias does not exist.
import { ARTIFACT_CAPABILITIES, ARTIFACT_ROLES } from "../contract/artifacts.ts";
import { PRESENCE_ACTIVITIES } from "../contract/presence.ts";

/** The bridge's message names on the port, beside the handshake's. */
export const BRIDGE_MESSAGE = {
  /** Host to page, first on the port: what the page may do and who is viewing. */
  init: "bridge.init",
  /** Page to host: one call. */
  call: "bridge.call",
  /** Host to page: one call's answer. */
  result: "bridge.result",
  /** Host to page: something the page subscribed to moved. */
  event: "bridge.event",
} as const;

/** The capabilities a version declares, as the runtime names them (`artifacts.Capability`). */
export type RuntimeCapability = (typeof ARTIFACT_CAPABILITIES)[number];

/** Every method a page may call, and the capability each needs (none for `openLink` and `print`). */
export const BRIDGE_METHODS = {
  "identity.me": "identity",
  "identity.people": "identity",
  "data.schema": "db",
  "data.get": "db",
  "data.list": "db",
  "data.query": "db",
  "data.subscribe": "db",
  "data.unsubscribe": "db",
  "data.write": "db",
  "data.undo": "db",
  "data.redo": "db",
  "state.get": "state",
  "state.set": "state",
  "state.delete": "state",
  "state.list": "state",
  "presence.set": "presence",
  "assets.list": "assets",
  openLink: null,
  print: null,
} as const satisfies Record<string, RuntimeCapability | null>;

export type BridgeMethod = keyof typeof BRIDGE_METHODS;

/** The refusal codes a call rejects with. A page treats an unknown code as a generic failure. */
export type BridgeErrorCode =
  | "capability_not_declared"
  | "forbidden"
  | "not_found"
  | "invalid"
  | "rate_limited"
  | "unavailable";

/** An agent's face, as the chart draws it: one of the character family and its node hue. */
export interface BridgeAvatar {
  character: string;
  hue: string;
}

/**
 * Somebody a page may name, as the chart the viewer's tab holds says them:
 * display facts only, never a token, a login or an address.
 */
export interface BridgePerson {
  /** The seat handle: the value a person column stores. */
  seat: string;
  kind: "human" | "agent";
  displayName: string;
  /** An agent's face; a person has none. */
  avatar?: BridgeAvatar;
}

/** Who is viewing, as a page is told. */
export interface BridgeViewer extends BridgePerson {
  role: (typeof ARTIFACT_ROLES)[number];
}

/** How the host's link to the engine stands. */
export type BridgeConnection = "live" | "reconnecting" | "offline";

/** Host to page, first: the contract, what the version declared, who is viewing and how the link stands. */
export interface BridgeInit {
  type: typeof BRIDGE_MESSAGE.init;
  contract: string;
  capabilities: RuntimeCapability[];
  viewer: BridgeViewer | null;
  connection: BridgeConnection;
}

/** One document as a page reads it. */
export interface BridgeDoc {
  id: string;
  data: Record<string, unknown>;
  version: number;
  createdBy: string;
  createdAt: string;
  updatedBy: string;
  updatedAt: string;
  /** A write this page made on it is not decided yet. */
  pending: boolean;
}

/** Page to host: one call. */
export interface BridgeCall {
  type: typeof BRIDGE_MESSAGE.call;
  id: number;
  method: BridgeMethod;
  params: Record<string, unknown>;
}

/** Host to page: one call's answer. */
export type BridgeResult =
  | { type: typeof BRIDGE_MESSAGE.result; id: number; ok: true; value: unknown }
  | {
      type: typeof BRIDGE_MESSAGE.result;
      id: number;
      ok: false;
      error: { code: BridgeErrorCode; message: string };
    };

/** Host to page: a subscription moved. */
export type BridgeEvent =
  | {
      type: typeof BRIDGE_MESSAGE.event;
      topic: "data";
      subscription: number;
      upsert: BridgeDoc[];
      remove: string[];
    }
  | {
      type: typeof BRIDGE_MESSAGE.event;
      topic: "presence";
      roster: BridgeRoster;
    }
  | { type: typeof BRIDGE_MESSAGE.event; topic: "connection"; state: BridgeConnection }
  | { type: typeof BRIDGE_MESSAGE.event; topic: "identity"; viewer: BridgeViewer | null };

/** One member of the room, as a page reads it: the session, and the person the chart says holds it. */
export interface BridgeMember extends BridgePerson {
  session: string;
  activity: (typeof PRESENCE_ACTIVITIES)[number];
  focus: { version?: number; region?: string; collection?: string; doc?: string; field?: string };
  at: string;
}

/** Who is in the room. */
export interface BridgeRoster {
  members: BridgeMember[];
  counts?: { human: number; agent: number };
  truncated: boolean;
}

/** A write's outcome, as a page handles it: an answer, not an exception, for every verdict the engine decided. */
export interface BridgeWriteOutcome {
  verdict: "applied" | "conflict" | "invalid" | "forbidden" | "quota" | "gone" | "pending";
  seq?: number;
  reason?: string;
  conflict?: BridgeDoc[];
}

// ---- readers -------------------------------------------------------------

const record = (data: unknown): Record<string, unknown> | null =>
  typeof data === "object" && data !== null && !Array.isArray(data)
    ? (data as Record<string, unknown>)
    : null;

const isCapability = (v: unknown): v is RuntimeCapability =>
  typeof v === "string" && (ARTIFACT_CAPABILITIES as readonly string[]).includes(v);

/**
 * The capabilities the artifact origin wrote on the runtime's own script
 * element (`data-capabilities`, space separated): the frame's grant, known from
 * the page's first script, before the host's handshake says it again. Only the
 * names a version may declare are read; anything else in the attribute is
 * left out.
 */
export function readDeclaredCapabilities(attribute: string | null): RuntimeCapability[] {
  if (!attribute) return [];
  return [...new Set(attribute.split(/\s+/).filter(isCapability))];
}

const isMethod = (v: unknown): v is BridgeMethod =>
  typeof v === "string" && Object.hasOwn(BRIDGE_METHODS, v);

const CONNECTIONS: readonly BridgeConnection[] = ["live", "reconnecting", "offline"];

/** The longest call id either end accepts, so a counter is never a float. */
const MAX_CALL_ID = Number.MAX_SAFE_INTEGER;

/** Reads a call, or null for anything that is not exactly one. */
export function readCall(data: unknown): BridgeCall | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.call) return null;
  const { id, method } = m;
  const params = m["params"] === undefined ? {} : record(m["params"]);
  if (typeof id !== "number" || !Number.isInteger(id) || id < 1 || id > MAX_CALL_ID) return null;
  if (!isMethod(method) || !params) return null;
  return { type: BRIDGE_MESSAGE.call, id, method, params };
}

/** Reads a person, or null. An avatar that is not exactly one is left off rather than refusing the person. */
export function readPerson(data: unknown): BridgePerson | null {
  const v = record(data);
  if (!v) return null;
  const { seat, kind, displayName } = v;
  if (typeof seat !== "string" || typeof displayName !== "string") return null;
  if (kind !== "human" && kind !== "agent") return null;
  const person: BridgePerson = { seat, kind, displayName };
  const avatar = record(v["avatar"]);
  if (avatar && typeof avatar["character"] === "string" && typeof avatar["hue"] === "string") {
    person.avatar = { character: avatar["character"], hue: avatar["hue"] };
  }
  return person;
}

/** Reads a viewer, or null. */
export function readViewer(data: unknown): BridgeViewer | null {
  const person = readPerson(data);
  const role = record(data)?.["role"];
  if (!person) return null;
  if (typeof role !== "string" || !(ARTIFACT_ROLES as readonly string[]).includes(role))
    return null;
  return { ...person, role: role as BridgeViewer["role"] };
}

/** Reads the init message, or null. */
export function readInit(data: unknown): BridgeInit | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.init) return null;
  const { contract, capabilities, connection } = m;
  if (typeof contract !== "string" || !Array.isArray(capabilities)) return null;
  if (!capabilities.every(isCapability)) return null;
  if (typeof connection !== "string" || !(CONNECTIONS as readonly string[]).includes(connection))
    return null;
  const viewer = m["viewer"] === null ? null : readViewer(m["viewer"]);
  if (m["viewer"] !== null && viewer === null) return null;
  return {
    type: BRIDGE_MESSAGE.init,
    contract,
    capabilities: [...capabilities],
    viewer,
    connection: connection as BridgeConnection,
  };
}

/** Reads a call's answer, or null. */
export function readResult(data: unknown): BridgeResult | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.result) return null;
  const { id, ok } = m;
  if (typeof id !== "number" || !Number.isInteger(id)) return null;
  if (ok === true) return { type: BRIDGE_MESSAGE.result, id, ok: true, value: m["value"] };
  const error = record(m["error"]);
  if (ok !== false || !error) return null;
  const { code, message } = error;
  if (typeof code !== "string" || typeof message !== "string") return null;
  return {
    type: BRIDGE_MESSAGE.result,
    id,
    ok: false,
    error: { code: code as BridgeErrorCode, message },
  };
}

/** Reads one document a page is handed, or null. */
export function readDoc(data: unknown): BridgeDoc | null {
  const d = record(data);
  if (!d) return null;
  const fields = record(d["data"]);
  const { id, version, createdBy, createdAt, updatedBy, updatedAt, pending } = d;
  if (typeof id !== "string" || !fields || typeof version !== "number") return null;
  if (typeof createdBy !== "string" || typeof createdAt !== "string") return null;
  if (typeof updatedBy !== "string" || typeof updatedAt !== "string") return null;
  return {
    id,
    data: fields,
    version,
    createdBy,
    createdAt,
    updatedBy,
    updatedAt,
    pending: pending === true,
  };
}

/** Reads a member of the room, or null. */
function readMember(data: unknown): BridgeMember | null {
  const m = record(data);
  const person = readPerson(data);
  if (!m || !person) return null;
  const { session, activity, at } = m;
  const focus = record(m["focus"]) ?? {};
  if (typeof session !== "string" || typeof at !== "string") return null;
  if (
    typeof activity !== "string" ||
    !(PRESENCE_ACTIVITIES as readonly string[]).includes(activity)
  ) {
    return null;
  }
  const out: BridgeMember["focus"] = {};
  if (typeof focus["version"] === "number") out.version = focus["version"];
  for (const key of ["region", "collection", "doc", "field"] as const) {
    if (typeof focus[key] === "string") out[key] = focus[key];
  }
  return { ...person, session, activity: activity as BridgeMember["activity"], focus: out, at };
}

/** Reads an event, or null. */
export function readEvent(data: unknown): BridgeEvent | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.event) return null;
  switch (m["topic"]) {
    case "data": {
      const { subscription, upsert, remove } = m;
      if (typeof subscription !== "number" || !Array.isArray(upsert) || !Array.isArray(remove))
        return null;
      const docs = upsert.map(readDoc);
      if (docs.some((d) => d === null) || !remove.every((r) => typeof r === "string")) return null;
      return {
        type: BRIDGE_MESSAGE.event,
        topic: "data",
        subscription,
        upsert: docs as BridgeDoc[],
        remove: [...(remove as string[])],
      };
    }
    case "presence": {
      const roster = record(m["roster"]);
      if (!roster || !Array.isArray(roster["members"])) return null;
      const members = roster["members"].map(readMember);
      if (members.some((x) => x === null)) return null;
      const counts = record(roster["counts"]);
      const out: BridgeRoster = {
        members: members as BridgeMember[],
        truncated: roster["truncated"] === true,
      };
      if (counts && typeof counts["human"] === "number" && typeof counts["agent"] === "number") {
        out.counts = { human: counts["human"], agent: counts["agent"] };
      }
      return { type: BRIDGE_MESSAGE.event, topic: "presence", roster: out };
    }
    case "connection": {
      const state = m["state"];
      if (typeof state !== "string" || !(CONNECTIONS as readonly string[]).includes(state))
        return null;
      return { type: BRIDGE_MESSAGE.event, topic: "connection", state: state as BridgeConnection };
    }
    case "identity": {
      const viewer = m["viewer"] === null ? null : readViewer(m["viewer"]);
      if (m["viewer"] !== null && viewer === null) return null;
      return { type: BRIDGE_MESSAGE.event, topic: "identity", viewer };
    }
  }
  return null;
}

/** The capability a method needs, or null for one every page may call. */
export function capabilityOf(method: BridgeMethod): RuntimeCapability | null {
  return BRIDGE_METHODS[method];
}
