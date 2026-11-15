/**
 * An SSH server is saved whole, so its form sends back what it does not draw
 * and the revision it was read from.
 *
 * The invariants worth breaking a build over: an edit keeps a server's
 * allowlist and its Atlas switch; every save names the revision the list was
 * read from; a new server cannot take a registered server's name (a PUT to it
 * replaces that server); keys a scan filled in are pinned only once compared;
 * and a server an agent is granted is not removed.
 */

import { expect, test } from "vitest";
import { holdReason } from "~/lib/formHold.ts";
import type { SetupSSHServer } from "~/contract/setup.ts";
import { formOf, lastTestLine, removeHeld, serverBody, serverFormHeld } from "./sshServer.ts";

const HOST: SetupSSHServer = {
  id: "api-1",
  address: "api-1.example.com",
  port: 22,
  fingerprints: ["SHA256:abc"],
  host_keys: ["ssh-ed25519 AAAAC3Nza api-1"],
  users: { read: "crewlet-read", operate: "crewlet-ops" },
  description: "Production API",
  inventory: true,
  read_allowlist: ["/usr/bin/uptime"],
  granted_to: [],
};

// AN EDIT SENDS THE SERVER WHOLE: what the form does not draw as the server
// holds it, and the revision it was read from. Mutation: drop the allowlist,
// the Atlas switch or if_match from the body, and this fails.
test("an edit sends what it does not draw back as it was, built on the revision read", () => {
  const form = { ...formOf(HOST), description: "The API" };
  expect(serverBody(form, HOST, "rev-7")).toEqual({
    address: "api-1.example.com",
    port: 0,
    host_keys: ["ssh-ed25519 AAAAC3Nza api-1"],
    users: { read: "crewlet-read", operate: "crewlet-ops" },
    description: "The API",
    containers: false,
    inventory: true,
    read_allowlist: ["/usr/bin/uptime"],
    if_match: "rev-7",
  });
  expect(serverBody(formOf(null), null, "rev-7")).toMatchObject({
    inventory: false,
    read_allowlist: [],
    if_match: "rev-7",
  });
});

// A NEW SERVER NEVER TAKES A REGISTERED NAME, keys a scan filled in wait for
// their comparison, and each field is the engine's shape. Mutation: let any of
// them through, and this fails.
test("a server form is held until it is one the engine takes and its keys were compared", () => {
  const ok = { ...formOf(null), id: "db-1", address: "db-1.example.com", keys: "ssh-ed25519 AAAA" };
  expect(holdReason(serverFormHeld(ok, ["api-1"], false, false))).toBeUndefined();
  expect(holdReason(serverFormHeld({ ...ok, id: "api-1" }, ["api-1"], false, false))).toMatch(
    /registered already/,
  );
  expect(holdReason(serverFormHeld({ ...ok, id: "DB_1" }, [], false, false))).toMatch(
    /lowercase letters/,
  );
  expect(holdReason(serverFormHeld({ ...ok, address: "ssh://db-1" }, [], false, false))).toMatch(
    /no scheme/,
  );
  expect(holdReason(serverFormHeld({ ...ok, port: "70000" }, [], false, false))).toMatch(
    /1 to 65535/,
  );
  expect(holdReason(serverFormHeld({ ...ok, read: " " }, [], false, false))).toMatch(/read user/);
  expect(holdReason(serverFormHeld({ ...ok, keys: "\n" }, [], false, false))).toMatch(/host keys/);
  expect(holdReason(serverFormHeld(ok, [], false, true))).toMatch(/Compare each fingerprint/);
  // A value in the wrong shape is its field's; one not typed yet is the form's.
  expect(serverFormHeld({ ...ok, port: "70000" }, [], false, false)?.field).toBe("port");
  expect(serverFormHeld({ ...ok, address: "ssh://db-1" }, [], false, false)?.field).toBe("address");
  expect(serverFormHeld({ ...ok, id: "" }, [], false, false)?.field).toBeUndefined();
  // AN EDIT KEEPS ITS NAME, which is its own.
  expect(holdReason(serverFormHeld({ ...formOf(HOST) }, ["api-1"], true, false))).toBeUndefined();
});

// A SERVER AN AGENT IS GRANTED IS NOT REMOVED, as the engine refuses it.
// Mutation: let a granted server's Remove through, and this fails.
test("a server an agent is granted is held from removal, naming the agent", () => {
  expect(removeHeld(HOST)).toBeUndefined();
  expect(removeHeld({ ...HOST, granted_to: ["sre-lead"] })).toMatch(/sre-lead is granted api-1/);
});

// THE LATEST TEST SAYS WHERE IT FAILED. Mutation: say "failed" with no step,
// and this fails.
test("a failed test names the step it failed at", () => {
  expect(lastTestLine(HOST)).toBeNull();
  const failed = {
    ...HOST,
    last_test: {
      host: "api-1",
      authority: "SHA256:ca",
      at: "2026-11-15T10:00:00Z",
      by: "jane",
      by_kind: "human",
      ok: false,
      steps: [
        { id: "host_key", ok: true, detail: "" },
        { id: "gate_version", ok: false, detail: "the gate answers version 2" },
      ],
    },
  };
  expect(lastTestLine(failed)).toBe(
    "Test connection failed at gate version: the gate answers version 2",
  );
});
