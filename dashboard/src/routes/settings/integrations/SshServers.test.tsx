/**
 * A server is read for Atlas only after a person has seen what will run.
 *
 * The switch writes nothing: it opens the question, which lists the exact
 * commands the engine answered, and only its second press sends the one
 * field. A server without container reads is shown the ports command alone.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { SshServers } from "./SshServers.tsx";
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
  render(<SshServers managed={null} />);

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
  render(<SshServers managed={null} />);

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
  render(<SshServers managed={null} />);

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
  render(<SshServers managed="The settings are managed by the operator." />);

  fireEvent.click(await screen.findByRole("switch", { name: "Read for Atlas" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(sent).toEqual([]);
});
