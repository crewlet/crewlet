/**
 * An SSH server as its form holds it, the one body its PUT sends, and why the
 * form or a removal is held.
 *
 * A SERVER IS SENT WHOLE (`PUT /setup/integrations/ssh/hosts/{id}` replaces
 * it), so an edit starts from every field the answer carries, its key lines
 * and its allowlist included, and sends back what it did not change as it
 * was, with the revision the answer was read from as its `if_match`: a change
 * made meanwhile is refused rather than overwritten.
 *
 * Each sentence restates a rule the engine holds (`config.SSHHost`), never a
 * second one: the engine's validation decides every save again.
 */

import type { SetupSSHServer } from "~/contract/setup.ts";

/** The server fields a person types, as text. */
export interface ServerForm {
  id: string;
  address: string;
  /** Empty is the engine's 22. */
  port: string;
  read: string;
  operate: string;
  description: string;
  containers: boolean;
  /** The host key lines it pins, one a line. */
  keys: string;
}

/** The longest id and description the engine takes (`config.SSHHost`). */
const MAX_ID = 40;
const MAX_DESCRIPTION = 200;

/** A form for a new server, or one started from a registered server. */
export function formOf(host: SetupSSHServer | null): ServerForm {
  if (!host) {
    return {
      id: "",
      address: "",
      port: "",
      read: "crewlet-read",
      operate: "",
      description: "",
      containers: false,
      keys: "",
    };
  }
  return {
    id: host.id,
    address: host.address,
    port: host.port === 22 ? "" : String(host.port),
    read: host.users.read,
    operate: host.users.operate ?? "",
    description: host.description ?? "",
    containers: Boolean(host.containers),
    keys: host.host_keys.join("\n"),
  };
}

/** The key lines a form pins: one a non-empty line. */
export function keyLines(keys: string): string[] {
  return keys
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
}

/**
 * Why a server form cannot be saved yet, or undefined: an id of the engine's
 * shape that no other server holds (a PUT to a registered id replaces that
 * server), an address with no scheme, user or port, a port in range, a read
 * user, a key line at least, a description of one line within its bound, and
 * a scan's fingerprints compared where the keys came from one.
 */
export function serverFormHeld(
  form: ServerForm,
  taken: readonly string[],
  editing: boolean,
  scanUnconfirmed: boolean,
): string | undefined {
  const id = form.id.trim();
  if (!editing) {
    if (!new RegExp(`^[a-z0-9-]{1,${MAX_ID}}$`).test(id)) {
      return `Name the server: up to ${MAX_ID} lowercase letters, digits and hyphens.`;
    }
    if (taken.includes(id)) return `A server named ${id} is registered already.`;
  }
  const address = form.address.trim();
  if (address === "" || /[\s/@]|:\/\//.test(address)) {
    return "Give the server's host name or IP address, with no scheme, user, port or path.";
  }
  const port = form.port.trim();
  if (port !== "") {
    const n = Number(port);
    if (!Number.isInteger(n) || n < 1 || n > 65535) return "The port is a number from 1 to 65535.";
  }
  if (form.read.trim() === "") return "Name the read user the server setup creates.";
  if (keyLines(form.keys).length === 0) {
    return "Pin its host keys: read them from the server, or paste their lines.";
  }
  if (form.description.includes("\n") || form.description.trim().length > MAX_DESCRIPTION) {
    return `The description is one line of up to ${MAX_DESCRIPTION} characters.`;
  }
  if (scanUnconfirmed) return "Compare each fingerprint with the server's console first.";
  return undefined;
}

/**
 * The body of the server's PUT: the form's fields, and every field the form
 * does not draw (whether Atlas reads it, its allowlist) as the server holds
 * it, with the revision it was read from.
 */
export function serverBody(
  form: ServerForm,
  host: SetupSSHServer | null,
  revision: string,
): Record<string, unknown> {
  const port = form.port.trim() === "" ? 0 : Number(form.port);
  const operate = form.operate.trim();
  return {
    address: form.address.trim(),
    port,
    host_keys: keyLines(form.keys),
    users: { read: form.read.trim(), ...(operate ? { operate } : {}) },
    description: form.description.trim(),
    containers: form.containers,
    inventory: host?.inventory ?? false,
    read_allowlist: host?.read_allowlist ?? [],
    if_match: revision,
  };
}

/**
 * Why a server cannot be removed, or undefined: the engine refuses it while
 * an agent names it, whether that agent's access is on or off.
 */
export function removeHeld(host: SetupSSHServer): string | undefined {
  if (host.granted_to.length === 0) return undefined;
  return `${host.granted_to.join(", ")} ${host.granted_to.length === 1 ? "is" : "are"} granted ${host.id}: take it from ${host.granted_to.length === 1 ? "that agent" : "those agents"} first.`;
}

/** A server's latest Test connection in a line, or null where none was pressed since it changed. */
export function lastTestLine(host: SetupSSHServer): string | null {
  const t = host.last_test;
  if (!t) return null;
  if (t.ok) return `Test connection passed, by ${t.by}.`;
  const failed = t.steps.find((s) => !s.ok);
  return failed
    ? `Test connection failed at ${failed.id.replaceAll("_", " ")}: ${failed.detail}`
    : `Test connection failed, by ${t.by}.`;
}
