/**
 * The artifact runtime, contract 1.0: what installs `window.crewlet` in an
 * artifact page and speaks to the page's host over the bridge.
 *
 * A BUILD TARGET OF ITS OWN (`vite.runtime.config.ts`), emitted to
 * `static/artifacts/runtime/<contract>/runtime.js`, committed and embedded
 * like the dashboard, and served from the artifact origin at
 * `/_crewlet/runtime/<contract>/runtime.js`. The origin places it as the
 * first script of every page whose version pins the contract, under the
 * response's nonce, so `window.crewlet` exists before any of the page's own
 * scripts run: an author never includes it.
 *
 * THE PAGE HOLDS NO CREDENTIAL AND NAMES NO ARTIFACT. Every call is a message
 * on the one port the host transferred after it checked this document's hello
 * (the design system's `FrameProtocol`), and the host makes the call to the
 * engine as the signed-in viewer, bound to the artifact and version it
 * embedded. Capabilities are checked here first (a fast, readable refusal for
 * a namespace the version did not declare) and by the engine always (the real
 * boundary).
 *
 * The surface is documented in `docs/reference/artifact-runtime.md`.
 */

import {
  FRAME_BRIDGE,
  FRAME_MESSAGE,
  parseFrameFragment,
  readFramePortMessage,
  readFrameTheme,
  type FrameTheme,
} from "@crewlethq/ui/FrameProtocol";
import {
  BRIDGE_MESSAGE,
  capabilityOf,
  readEvent,
  readDeclaredCapabilities,
  readInit,
  readResult,
  type BridgeConnection,
  type BridgeDoc,
  type BridgeEvent,
  type BridgeInit,
  type BridgeMethod,
  type BridgePerson,
  type BridgeRoster,
  type BridgeViewer,
  type BridgeWriteOutcome,
  type RuntimeCapability,
} from "./bridge.ts";
// RELATIVE, as in the bridge: the runtime is built alone, where the `~` alias does not exist.
import { RUNTIME_CONTRACT } from "../contract/artifacts.ts";
import { pathSegment } from "../lib/segment.ts";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type Unsubscribe = () => void;

/** A refusal: an Error whose `code` a page branches on. */
class RuntimeError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "CrewletError";
    this.code = code;
  }
}

/** The page's bridge to its host: the port, the calls out, the subscriptions in. */
class Bridge {
  private port: MessagePort | null = null;
  private init: BridgeInit | null = null;
  private next = 1;
  private readonly calls = new Map<
    number,
    { resolve: (v: unknown) => void; reject: (e: unknown) => void }
  >();
  private readonly queued: { method: BridgeMethod; params: Record<string, unknown>; id: number }[] =
    [];
  readonly onEvent = new Set<(event: BridgeEvent) => void>();
  readonly onTheme = new Set<(theme: FrameTheme) => void>();
  private resolveReady: () => void = () => {};
  readonly ready = new Promise<void>((resolve) => {
    this.resolveReady = resolve;
  });

  /**
   * @param declared What the origin wrote on this script's element: the
   *   frame's grant, answered until the host's handshake, which is the
   *   authority from then on.
   */
  constructor(private readonly declared: readonly RuntimeCapability[]) {}

  get capabilities(): readonly RuntimeCapability[] {
    return this.init?.capabilities ?? this.declared;
  }

  get viewer(): BridgeViewer | null {
    return this.init?.viewer ?? null;
  }

  set viewer(viewer: BridgeViewer | null) {
    if (this.init) this.init = { ...this.init, viewer };
  }

  get connection(): BridgeConnection {
    return this.init?.connection ?? "reconnecting";
  }

  set connection(state: BridgeConnection) {
    if (this.init) this.init = { ...this.init, connection: state };
  }

  /** Holds the port the host transferred, and reads everything that arrives on it. */
  adopt(port: MessagePort): void {
    this.port = port;
    port.addEventListener("message", (event: MessageEvent) => this.receive(event.data));
    port.start();
  }

  private receive(data: unknown): void {
    const theme = readFrameTheme(data);
    if (theme) {
      for (const fn of this.onTheme) fn(theme);
      return;
    }
    const init = readInit(data);
    if (init) {
      const first = this.init === null;
      this.init = init;
      if (first) {
        for (const call of this.queued.splice(0)) this.post(call.id, call.method, call.params);
        this.resolveReady();
      }
      return;
    }
    const result = readResult(data);
    if (result) {
      const pending = this.calls.get(result.id);
      if (!pending) return;
      this.calls.delete(result.id);
      if (result.ok) pending.resolve(result.value);
      else pending.reject(new RuntimeError(result.error.code, result.error.message));
      return;
    }
    const event = readEvent(data);
    if (event) {
      if (event.topic === "connection") this.connection = event.state;
      if (event.topic === "identity") this.viewer = event.viewer;
      for (const fn of this.onEvent) fn(event);
    }
  }

  /**
   * Whether the version declared a capability: from the page's first script,
   * as the origin told it, and as the host said at the handshake once it has.
   */
  has(capability: RuntimeCapability): boolean {
    return this.capabilities.includes(capability);
  }

  /** One call to the host, refused here for a capability the version did not declare. */
  call<T>(method: BridgeMethod, params: Record<string, unknown> = {}): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const id = this.next++;
      this.calls.set(id, { resolve: resolve as (v: unknown) => void, reject });
      if (!this.init) {
        this.queued.push({ id, method, params });
        return;
      }
      this.post(id, method, params);
    }).then((value) => value as T);
  }

  private post(id: number, method: BridgeMethod, params: Record<string, unknown>): void {
    const needs = capabilityOf(method);
    if (needs && !this.has(needs)) {
      const pending = this.calls.get(id);
      this.calls.delete(id);
      pending?.reject(
        new RuntimeError(
          "capability_not_declared",
          `this page's version does not declare the \`${needs}\` capability, so ${method} is not available`,
        ),
      );
      return;
    }
    this.port?.postMessage({ type: BRIDGE_MESSAGE.call, id, method, params });
  }

  /** Asks the host to give keyboard focus back to the page around the frame. */
  focusExit(): void {
    this.port?.postMessage({ type: FRAME_MESSAGE.focusExit });
  }
}

// READ WHILE THIS SCRIPT RUNS: `currentScript` names it only now.
const bridge = new Bridge(
  readDeclaredCapabilities(document.currentScript?.getAttribute("data-capabilities") ?? null),
);

// ---- the handshake ---------------------------------------------------------

const fragment = parseFrameFragment(window.location.hash);
let theme: FrameTheme = fragment.theme;
bridge.onTheme.add((next) => {
  theme = next;
});

window.addEventListener("message", (event: MessageEvent) => {
  if (event.source !== window.parent || window.parent === window) return;
  if (!readFramePortMessage(event.data)) return;
  const port = event.ports[0];
  if (port) bridge.adopt(port);
});

if (window.parent !== window && fragment.nonce) {
  // THE TARGET IS "*" because the host's origin is not this document's to
  // know, and nothing secret travels: the nonce is the frame's own, and the
  // host answers only the window it embedded.
  window.parent.postMessage(
    {
      type: FRAME_MESSAGE.hello,
      nonce: fragment.nonce,
      contract: RUNTIME_CONTRACT,
      bridge: FRAME_BRIDGE,
    },
    "*",
  );
}

// A PAGE THAT TRAPS TAB still lets its reader out: Mod+Escape hands focus back
// to the viewer around the frame.
window.addEventListener(
  "keydown",
  (event: KeyboardEvent) => {
    if (event.key === "Escape" && (event.metaKey || event.ctrlKey) && !event.defaultPrevented) {
      bridge.focusExit();
    }
  },
  true,
);

// ---- the surface -------------------------------------------------------------

/** A guard for one write's options. */
interface WriteOptions {
  ifVersion?: number;
}

/** One op as the host forwards it to the engine (`artifacts.DataOp`). */
interface Op {
  op: "set" | "merge" | "delete" | "increment" | "append" | "add" | "remove";
  collection: string;
  doc: string;
  if_version?: number;
  fields?: Record<string, Json>;
  field?: string;
  value?: Json;
}

function guard(opts: WriteOptions | undefined): { if_version?: number } {
  return opts?.ifVersion !== undefined ? { if_version: opts.ifVersion } : {};
}

function write(ops: Op[]): Promise<BridgeWriteOutcome> {
  return bridge.call<BridgeWriteOutcome>("data.write", { ops });
}

function opsFor(collection: string) {
  return {
    set: (id: string, fields: Record<string, Json>, opts?: WriteOptions): Op => ({
      op: "set",
      collection,
      doc: id,
      fields,
      ...guard(opts),
    }),
    merge: (id: string, fields: Record<string, Json>, opts?: WriteOptions): Op => ({
      op: "merge",
      collection,
      doc: id,
      fields,
      ...guard(opts),
    }),
    delete: (id: string, opts?: WriteOptions): Op => ({
      op: "delete",
      collection,
      doc: id,
      ...guard(opts),
    }),
    increment: (id: string, field: string, by: number, opts?: WriteOptions): Op => ({
      op: "increment",
      collection,
      doc: id,
      field,
      value: by,
      ...guard(opts),
    }),
    append: (id: string, field: string, value: Json, opts?: WriteOptions): Op => ({
      op: "append",
      collection,
      doc: id,
      field,
      value,
      ...guard(opts),
    }),
    add: (id: string, field: string, value: Json, opts?: WriteOptions): Op => ({
      op: "add",
      collection,
      doc: id,
      field,
      value,
      ...guard(opts),
    }),
    remove: (id: string, field: string, value: Json, opts?: WriteOptions): Op => ({
      op: "remove",
      collection,
      doc: id,
      field,
      value,
      ...guard(opts),
    }),
  };
}

/**
 * Ends a subscription at the host. Its answer carries nothing the page waits
 * for: a host that cannot be reached has dropped every subscription with the
 * port, and one that refuses has none by that id, so the outcome is the same
 * either way and nothing is left to report.
 */
function release(subscription: number): void {
  bridge.call("data.unsubscribe", { subscription }).then(
    () => undefined,
    () => undefined,
  );
}

/**
 * A collection's live documents, held for its subscribers and kept from the
 * host's events.
 *
 * A REFUSED SUBSCRIPTION IS SAID TO THE PAGE: `onError` is called once with
 * the refusal (`forbidden`, `not_found`, `capability_not_declared`,
 * `unavailable`), and the callback is never called, because an empty
 * collection and one the viewer may not read are different answers. A page
 * that passes no `onError` has the refusal reported where its errors are
 * seen, rather than waiting on a first delivery that will never come in
 * silence.
 */
function subscribeTo(
  collection: string,
  cb: (docs: BridgeDoc[], change: { changed: string[]; removed: string[] }) => void,
  onError?: (err: RuntimeError) => void,
): Unsubscribe {
  let stopped = false;
  let subscription = 0;
  const docs = new Map<string, BridgeDoc>();
  const listener = (event: BridgeEvent) => {
    if (event.topic !== "data" || event.subscription !== subscription || stopped) return;
    for (const doc of event.upsert) docs.set(doc.id, doc);
    for (const id of event.remove) docs.delete(id);
    cb([...docs.values()], { changed: event.upsert.map((d) => d.id), removed: [...event.remove] });
  };
  bridge.onEvent.add(listener);
  bridge
    .call<{ subscription: number; docs: BridgeDoc[] }>("data.subscribe", { collection })
    .then((answer) => {
      if (stopped) {
        release(answer.subscription);
        return;
      }
      subscription = answer.subscription;
      for (const doc of answer.docs) docs.set(doc.id, doc);
      cb([...docs.values()], { changed: answer.docs.map((d) => d.id), removed: [] });
    })
    .catch((err: unknown) => {
      if (stopped) return;
      bridge.onEvent.delete(listener);
      const refusal =
        err instanceof RuntimeError
          ? err
          : new RuntimeError("unavailable", "the subscription could not be made");
      if (onError) onError(refusal);
      else console.error("crewlet.data subscription refused:", refusal);
    });
  return () => {
    stopped = true;
    bridge.onEvent.delete(listener);
    if (subscription > 0) release(subscription);
  };
}

function collection(name: string) {
  const ops = opsFor(name);
  return {
    name,
    get: (id: string) => bridge.call<BridgeDoc | null>("data.get", { collection: name, id }),
    list: (opts: { cursor?: string; limit?: number } = {}) =>
      bridge.call<{ docs: BridgeDoc[]; cursor?: string }>("data.list", {
        collection: name,
        ...opts,
      }),
    query: (q: Record<string, Json>) =>
      bridge.call<{ docs: BridgeDoc[]; cursor?: string }>("data.query", {
        collection: name,
        query: q,
      }),
    subscribe: (
      cb: (docs: BridgeDoc[], change: { changed: string[]; removed: string[] }) => void,
      onError?: (err: RuntimeError) => void,
    ) => subscribeTo(name, cb, onError),
    set: (id: string, fields: Record<string, Json>, opts?: WriteOptions) =>
      write([ops.set(id, fields, opts)]),
    merge: (id: string, fields: Record<string, Json>, opts?: WriteOptions) =>
      write([ops.merge(id, fields, opts)]),
    delete: (id: string, opts?: WriteOptions) => write([ops.delete(id, opts)]),
    increment: (id: string, field: string, by: number, opts?: WriteOptions) =>
      write([ops.increment(id, field, by, opts)]),
    append: (id: string, field: string, value: Json, opts?: WriteOptions) =>
      write([ops.append(id, field, value, opts)]),
    add: (id: string, field: string, value: Json, opts?: WriteOptions) =>
      write([ops.add(id, field, value, opts)]),
    remove: (id: string, field: string, value: Json, opts?: WriteOptions) =>
      write([ops.remove(id, field, value, opts)]),
  };
}

function batch() {
  const pending: Op[] = [];
  const self = {
    set: (c: string, id: string, fields: Record<string, Json>, opts?: WriteOptions) => {
      pending.push(opsFor(c).set(id, fields, opts));
      return self;
    },
    merge: (c: string, id: string, fields: Record<string, Json>, opts?: WriteOptions) => {
      pending.push(opsFor(c).merge(id, fields, opts));
      return self;
    },
    delete: (c: string, id: string, opts?: WriteOptions) => {
      pending.push(opsFor(c).delete(id, opts));
      return self;
    },
    increment: (c: string, id: string, field: string, by: number, opts?: WriteOptions) => {
      pending.push(opsFor(c).increment(id, field, by, opts));
      return self;
    },
    append: (c: string, id: string, field: string, value: Json, opts?: WriteOptions) => {
      pending.push(opsFor(c).append(id, field, value, opts));
      return self;
    },
    add: (c: string, id: string, field: string, value: Json, opts?: WriteOptions) => {
      pending.push(opsFor(c).add(id, field, value, opts));
      return self;
    },
    remove: (c: string, id: string, field: string, value: Json, opts?: WriteOptions) => {
      pending.push(opsFor(c).remove(id, field, value, opts));
      return self;
    },
    commit: () => write(pending.splice(0)),
  };
  return self;
}

/** The room as the host last said it, for `presence.roster()` and its subscribers. */
let roster: BridgeRoster = { members: [], truncated: false };
const presenceListeners = new Set<(roster: BridgeRoster) => void>();
bridge.onEvent.add((event) => {
  if (event.topic !== "presence") return;
  roster = event.roster;
  for (const fn of presenceListeners) fn(roster);
});

const connectionListeners = new Set<(state: BridgeConnection) => void>();
const identityListeners = new Set<(viewer: BridgeViewer) => void>();
bridge.onEvent.add((event) => {
  if (event.topic === "connection") for (const fn of connectionListeners) fn(event.state);
  if (event.topic === "identity" && event.viewer)
    for (const fn of identityListeners) fn(event.viewer);
});

/** The reader's mode, resolved: their own preference, or the system's where they follow it. */
function resolvedMode(): "light" | "dark" {
  if (theme.mode === "light" || theme.mode === "dark") return theme.mode;
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}
const themeListeners = new Set<(mode: "light" | "dark") => void>();
bridge.onTheme.add(() => {
  const mode = resolvedMode();
  for (const fn of themeListeners) fn(mode);
});

/** The ticket path this document was served under, which every asset of the artifact is served beside. */
const ticketBase = (() => {
  const [, t, ticket] = window.location.pathname.split("/");
  return t === "t" && ticket ? `/t/${pathSegment(ticket)}` : "";
})();

const state = {
  get: (key: string) => bridge.call<Json | null>("state.get", { key }),
  set: (key: string, value: Json) => bridge.call<void>("state.set", { key, value }),
  delete: (key: string) => bridge.call<void>("state.delete", { key }),
  list: (prefix = "") => bridge.call<string[]>("state.list", { prefix }),
  /**
   * Opt in to `window.storage`, for a page written for Claude's artifact
   * storage: its `get`, `set`, `delete` and `list` over this viewer's own
   * state. Never installed by itself. A SHARED key is refused, because state
   * shared between viewers belongs in `crewlet.data`, where the collection's
   * rules decide who may change it.
   */
  installWindowStorage: () => {
    const sharedRefused = () =>
      Promise.reject(
        new RuntimeError(
          "invalid",
          "shared storage is not available here: keep state shared between viewers in crewlet.data",
        ),
      );
    const storage = {
      get: async (key: string, shared = false) => {
        if (shared) return sharedRefused();
        const value = await state.get(key);
        return value === null
          ? null
          : { key, value: typeof value === "string" ? value : JSON.stringify(value), shared };
      },
      set: async (key: string, value: string, shared = false) => {
        if (shared) return sharedRefused();
        await state.set(key, value);
        return { key, value, shared };
      },
      delete: async (key: string, shared = false) => {
        if (shared) return sharedRefused();
        await state.delete(key);
        return { key, deleted: true, shared };
      },
      list: async (prefix = "", shared = false) => {
        if (shared) return sharedRefused();
        return { keys: await state.list(prefix), prefix, shared };
      },
    };
    Object.defineProperty(window, "storage", { value: Object.freeze(storage), configurable: true });
  },
};

const crewlet = Object.freeze({
  version: RUNTIME_CONTRACT,
  ready: bridge.ready,
  capabilities: Object.freeze({
    has: (name: RuntimeCapability) => bridge.has(name),
    list: () => [...bridge.capabilities],
  }),
  connection: Object.freeze({
    get state(): BridgeConnection {
      return bridge.connection;
    },
    subscribe(cb: (state: BridgeConnection) => void): Unsubscribe {
      connectionListeners.add(cb);
      return () => void connectionListeners.delete(cb);
    },
  }),
  identity: Object.freeze({
    me: () => bridge.call<BridgeViewer>("identity.me"),
    people: () => bridge.call<BridgePerson[]>("identity.people"),
    subscribe(cb: (me: BridgeViewer) => void): Unsubscribe {
      identityListeners.add(cb);
      return () => void identityListeners.delete(cb);
    },
  }),
  data: Object.freeze({
    collection,
    batch,
    schema: () => bridge.call<unknown[]>("data.schema"),
    undo: () => bridge.call<BridgeWriteOutcome | null>("data.undo"),
    redo: () => bridge.call<BridgeWriteOutcome | null>("data.redo"),
  }),
  state: Object.freeze(state),
  presence: Object.freeze({
    roster: () => roster,
    subscribe(cb: (roster: BridgeRoster) => void): Unsubscribe {
      presenceListeners.add(cb);
      return () => void presenceListeners.delete(cb);
    },
    set(next: {
      activity?: "viewing" | "editing" | "idle";
      focus?: BridgeRoster["members"][number]["focus"];
    }): void {
      void bridge.call("presence.set", next).catch((err: unknown) => {
        console.error("crewlet.presence:", err);
      });
    },
  }),
  assets: Object.freeze({
    list: () =>
      bridge.call<{ id: string; name: string; contentType: string; size: number }[]>("assets.list"),
    url: (id: string) => `${ticketBase}/_assets/${pathSegment(id)}`,
  }),
  openLink: (url: string) => {
    let parsed: URL;
    try {
      parsed = new URL(url, window.location.href);
    } catch {
      return Promise.reject(
        new RuntimeError("invalid", "openLink takes an absolute https or http URL"),
      );
    }
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
      return Promise.reject(
        new RuntimeError("invalid", "openLink takes an absolute https or http URL"),
      );
    }
    return bridge.call<boolean>("openLink", { url: parsed.href });
  },
  // THE HOST PRINTS, never the frame: a sandboxed page has no print dialog of
  // its own. The dashboard prints this page alone, at its own size, and only
  // on the reader's own click (a call made with no gesture behind it is
  // refused), resolving true once the print was handed to the browser.
  print: () => bridge.call<boolean>("print"),
  theme: Object.freeze({
    get mode(): "light" | "dark" {
      return resolvedMode();
    },
    subscribe(cb: (mode: "light" | "dark") => void): Unsubscribe {
      themeListeners.add(cb);
      return () => void themeListeners.delete(cb);
    },
  }),
});

Object.defineProperty(window, "crewlet", { value: crewlet, configurable: false, writable: false });

void bridge.ready.then(() => window.dispatchEvent(new Event("crewlet:ready")));
