/**
 * A server is read for Atlas only after a person has seen what will run, and
 * is added, edited and removed here as the engine takes it.
 *
 * The switch writes nothing: it opens the question, which lists the exact
 * commands the engine answered, and only its second press sends the one
 * field. A server without container reads is shown the ports command alone.
 * A server is added only once the fingerprints a scan read are compared, and
 * then shows its setup; an edit sends it whole on the revision it was read
 * from; a server an agent is granted is not removed; and a reader who may not
 * change the configuration is held with why.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { SshServers, said } from "./SshServers.tsx";
import { RestError } from "~/protocol/rest.ts";
import type { SetupSSHServer } from "~/contract/setup.ts";

type Sent = { method: string; path: string; body: unknown };

const PORTS = "/usr/bin/ss -tln";
const CONTAINERS = "/usr/bin/sudo -n /usr/bin/docker ps";

function server(id: string, more: Partial<SetupSSHServer> = {}): SetupSSHServer {
  return {
    id,
    address: `${id}.acme.example`,
    port: 22,
    fingerprints: [],
    host_keys: [],
    users: { read: "crewlet-read" },
    granted_to: [],
    ...more,
  };
}

function stubEngine(hosts: SetupSSHServer[], sent: Sent[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === "/setup/integrations/ssh/hosts/scan") {
        sent.push({ method, path, body: JSON.parse(String(init?.body)) });
        return new Response(
          JSON.stringify({
            host_keys: [
              {
                line: "ssh-ed25519 AAAAED db-1",
                type: "ssh-ed25519",
                fingerprint_sha256: "SHA256:ed",
              },
              {
                line: "ssh-dss AAAADSS db-1",
                type: "ssh-dss",
                fingerprint_sha256: "SHA256:dss",
                problem: "DSA keys are refused",
              },
            ],
          }),
        );
      }
      if (path.endsWith("/setup") && method === "GET") {
        return new Response(JSON.stringify({ script: "#!/bin/sh\nHOST_ID=db-1\n", version: 3 }));
      }
      if (method !== "GET") {
        const body = init?.body ? JSON.parse(String(init.body)) : null;
        sent.push({ method, path, body });
        const host = hosts.find((h) => path.endsWith(`/hosts/${h.id}/inventory`));
        if (host) host.inventory = (body as { inventory: boolean }).inventory;
        return new Response(JSON.stringify({ key: "ssh", revision_id: "r2", epoch: 2 }));
      }
      return new Response(
        JSON.stringify({
          key: "ssh",
          configured: true,
          enabled: true,
          requirements: [],
          ssh: {
            revision_id: "rev-7",
            setup_version: 3,
            retired_ca_public_keys: [],
            inventory_commands: [
              { command: PORTS, containers: false },
              { command: CONTAINERS, containers: true },
            ],
            hosts,
          },
        }),
      );
    }),
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("switching a server on shows the exact commands first and writes on the second press", async () => {
  const sent: Sent[] = [];
  stubEngine([server("web-1", { containers: true }), server("db-1")], sent);
  mountAs(ADMIN_VIEWER);

  const switches = await screen.findAllByRole("switch", { name: "Read for Atlas" });
  expect(switches).toHaveLength(2);
  expect(switches.every((s) => !(s as HTMLInputElement).checked)).toBe(true);

  fireEvent.click(switches[0]!);
  const dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain("Let Crewlet read web-1 for Atlas?");
  expect(dialog.textContent).toContain(PORTS);
  expect(dialog.textContent).toContain(CONTAINERS);
  expect(dialog.textContent).toContain("drops its command line");
  // NOTHING IS WRITTEN by the switch itself.
  expect(sent).toEqual([]);

  fireEvent.click(within(dialog).getByRole("button", { name: "Switch on" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({
    method: "PUT",
    path: "/setup/integrations/ssh/hosts/web-1/inventory",
    body: { inventory: true },
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() =>
    expect(
      (screen.getAllByRole("switch", { name: "Read for Atlas" })[0] as HTMLInputElement).checked,
    ).toBe(true),
  );
});

test("a server without container reads is shown the ports command alone", async () => {
  const sent: Sent[] = [];
  stubEngine([server("db-1")], sent);
  mountAs(ADMIN_VIEWER);

  fireEvent.click(await screen.findByRole("switch", { name: "Read for Atlas" }));
  const dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain(PORTS);
  expect(dialog.textContent).not.toContain("docker");
  expect(dialog.textContent).toContain("Containers are not listed on this server");

  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(sent).toEqual([]);
});

test("switching a server off is asked once and sends that one field", async () => {
  const sent: Sent[] = [];
  stubEngine([server("web-1", { inventory: true })], sent);
  mountAs(ADMIN_VIEWER);

  const toggle = await screen.findByRole("switch", { name: "Read for Atlas" });
  expect((toggle as HTMLInputElement).checked).toBe(true);
  fireEvent.click(toggle);
  const dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain("Stop reading web-1 for Atlas?");
  expect(sent).toEqual([]);

  fireEvent.click(within(dialog).getByRole("button", { name: "Switch off" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]!.body).toEqual({ inventory: false });
});

test("a company whose settings are managed elsewhere cannot switch it", async () => {
  const sent: Sent[] = [];
  stubEngine([server("web-1")], sent);
  mountAs(ADMIN_VIEWER, "The settings are managed by the operator.");

  fireEvent.click(await screen.findByRole("switch", { name: "Read for Atlas" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(sent).toEqual([]);
});

/** The sentence a held button is described by. */
function heldReason(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

/** The list as a reader the viewer answer names reads it, with the frame's socket. */
function mountAs(viewer: unknown, managed: string | null = null) {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <SshServers managed={managed} />
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

// A SERVER IS ADDED ONLY ONCE THE KEYS A SCAN READ ARE COMPARED: the scan
// reads the address, a key the engine cannot pin is shown and left out, the
// press is held until the comparison is ticked, and the save is one PUT of the
// server on the revision the list was read from, after which its setup shows.
// Mutation: pin before the comparison, pin a refused key, drop if_match, or
// skip the setup, and this fails.
test("a server is added once its scanned fingerprints are compared, and then shows its setup", async () => {
  const sent: Sent[] = [];
  stubEngine([server("api-1")], sent);
  mountAs(ADMIN_VIEWER);

  const add = await screen.findByRole("button", { name: "Add a server" });
  await waitFor(() => expect(add.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(add);
  const dialog = await screen.findByRole("dialog", { name: "Add a server" });
  fireEvent.change(within(dialog).getByLabelText(/^Name/), { target: { value: "db-1" } });
  fireEvent.change(within(dialog).getByLabelText(/^Address/), {
    target: { value: "db-1.example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Read its host keys" }));
  expect(await within(dialog).findByText("SHA256:ed")).toBeTruthy();
  expect(within(dialog).getByText(/not pinned, DSA keys are refused/)).toBeTruthy();
  const save = within(dialog).getByRole("button", { name: "Add server" });
  fireEvent.click(save);
  expect(sent.filter((s) => s.method === "PUT")).toEqual([]);
  fireEvent.click(within(dialog).getByRole("checkbox", { name: /I compared each fingerprint/ }));
  fireEvent.click(save);
  await waitFor(() => expect(sent.filter((s) => s.method === "PUT")).toHaveLength(1));
  expect(sent.find((s) => s.method === "PUT")).toEqual({
    method: "PUT",
    path: "/setup/integrations/ssh/hosts/db-1",
    body: {
      address: "db-1.example.com",
      port: 0,
      host_keys: ["ssh-ed25519 AAAAED db-1"],
      users: { read: "crewlet-read" },
      description: "",
      containers: false,
      inventory: false,
      read_allowlist: [],
      if_match: "rev-7",
    },
  });
  const setup = await screen.findByRole("dialog", { name: "Set up db-1" });
  expect(await within(setup).findByText(/HOST_ID=db-1/)).toBeTruthy();
});

// AN EDIT SENDS THE SERVER WHOLE, keeping what it does not draw. Mutation:
// drop the allowlist or the Atlas switch from the edit, and this fails.
test("an edit sends the server whole, its allowlist and Atlas switch as they were", async () => {
  const sent: Sent[] = [];
  stubEngine(
    [
      server("api-1", {
        host_keys: ["ssh-ed25519 AAAAAPI api-1"],
        inventory: true,
        read_allowlist: ["/usr/bin/uptime"],
      }),
    ],
    sent,
  );
  mountAs(ADMIN_VIEWER);

  const edit = await screen.findByRole("button", { name: "Edit api-1" });
  await waitFor(() => expect(edit.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "Edit api-1" });
  fireEvent.change(within(dialog).getByLabelText(/^Description/), {
    target: { value: "The API" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({
    method: "PUT",
    path: "/setup/integrations/ssh/hosts/api-1",
    body: {
      description: "The API",
      host_keys: ["ssh-ed25519 AAAAAPI api-1"],
      inventory: true,
      read_allowlist: ["/usr/bin/uptime"],
      if_match: "rev-7",
    },
  });
});

// A SERVER AN AGENT IS GRANTED IS NOT REMOVED, and one nobody is granted is
// removed after a confirmation. Mutation: let a granted server's Remove
// through, or remove on the first press, and this fails.
test("a server is removed after a confirmation, and held while an agent is granted it", async () => {
  const sent: Sent[] = [];
  stubEngine([server("api-1", { granted_to: ["sre-lead"] }), server("db-1")], sent);
  mountAs(ADMIN_VIEWER);

  const granted = await screen.findByRole("button", { name: "Remove api-1" });
  await waitFor(() => expect(heldReason(granted)).toMatch(/sre-lead is granted api-1/));
  fireEvent.click(granted);
  expect(screen.queryByRole("alertdialog")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Remove db-1" }));
  const confirm = await screen.findByRole("alertdialog");
  expect(sent).toEqual([]);
  fireEvent.click(within(confirm).getByRole("button", { name: "Remove server" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ method: "DELETE", path: "/setup/integrations/ssh/hosts/db-1" });
});

// A READER WHO MAY NOT CHANGE THE CONFIGURATION SEES EVERY WRITE, held with
// why. Mutation: hide a control from them, or let a press through, and this
// fails.
test("a reader who may not change the configuration is held from every server write, with why", async () => {
  const sent: Sent[] = [];
  stubEngine([server("api-1")], sent);
  mountAs({ ...ADMIN_VIEWER, grants: ["worklet"] });

  const add = await screen.findByRole("button", { name: "Add a server" });
  await waitFor(() => expect(heldReason(add)).toBe(CONFIG_WRITE_REASONS.no_grant));
  for (const name of ["Edit api-1", "Test the connection to api-1", "Remove api-1"]) {
    expect(heldReason(screen.getByRole("button", { name })), name).toBe(
      CONFIG_WRITE_REASONS.no_grant,
    );
  }
  fireEvent.click(add);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(sent).toEqual([]);
});

// The engine's detail says which way a read failed; the code's generic sentence is the fallback.
test("a refusal says the engine's detail alone, and its code's sentence only without one", () => {
  const detail = "web-1.example.com does not resolve to an address. Check the host name.";
  const generic = "The server could not be reached, or it presented no key Crewlet can pin.";
  expect(said(new RestError(502, { error: "server_unreachable", message: generic, detail }))).toBe(
    detail,
  );
  expect(said(new RestError(502, { error: "server_unreachable", message: generic }))).toBe(generic);
});
