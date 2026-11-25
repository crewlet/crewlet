/**
 * A live block follows what is typed through the engine, and the key it needs
 * is made once when the form opens.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LiveBlock, mintsFirst, useMintFirst } from "./LiveBlock.tsx";
import type { SetupRequirement } from "~/contract/setup.ts";
import { eventually } from "~/test/settle.ts";

function stubFetch(handler: (path: string, body: Record<string, unknown>) => Response) {
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://engine.test").pathname;
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {};
    return Promise.resolve(handler(path, body));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return spy;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const key: SetupRequirement = {
  field: "federation_key",
  label: "Federation key",
  kind: "secret",
  config_path: "integrations.google_cloud.provisioning.federation_key",
  required: true,
  present: false,
  resolved: null,
  mintable: true,
  mint_first: true,
};

test("only a mint_first key not yet held is made first", () => {
  expect(mintsFirst([key])).toEqual(["federation_key"]);
  expect(mintsFirst([{ ...key, present: true }])).toEqual([]);
  expect(mintsFirst([{ ...key, mint_first: false }])).toEqual([]);
});

function Minter({ refresh }: { refresh: () => Promise<void> }) {
  const failed = useMintFirst([{ kind: "google_cloud", requirements: [key] }], refresh);
  return <p>{failed ?? "ok"}</p>;
}

test("the key is made once when the form opens, then the form is refreshed", async () => {
  const spy = stubFetch(() => new Response(JSON.stringify({ revision_id: "r1" }), { status: 201 }));
  const refresh = vi.fn(() => Promise.resolve());
  const { rerender } = render(<Minter refresh={refresh} />);
  rerender(<Minter refresh={refresh} />);
  await eventually(() => expect(refresh).toHaveBeenCalledTimes(1));
  expect(spy).toHaveBeenCalledTimes(1);
  const [input, init] = spy.mock.calls[0] ?? [];
  expect(new URL(String(input), "http://engine.test").pathname).toBe(
    "/setup/integrations/google_cloud/inputs",
  );
  expect(JSON.parse(String(init?.body))).toMatchObject({ generate: ["federation_key"] });
});

test("the block is redrawn from what is typed, and a refusal says why instead of drawing", async () => {
  const spy = stubFetch((_path, body) => {
    const values = body.values as Record<string, string>;
    if (values.identity_project === "Acme IDs") {
      return new Response(
        JSON.stringify({
          code: "invalid_input",
          detail: "This is not a Google Cloud project ID.",
          field: "identity_project",
        }),
        { status: 400 },
      );
    }
    return new Response(
      JSON.stringify({
        public: { text: `IDENTITY=${values.identity_project}`, language: "sh", live: true },
      }),
      { status: 200 },
    );
  });
  const view = { text: "IDENTITY=crewlet-acme-1a2b", language: "sh", live: true };
  const onRefused = vi.fn();
  const { rerender } = render(
    <LiveBlock
      kind="google_cloud"
      field="federation_key"
      view={view}
      typed={{ identity_project: "acme-ids" }}
    />,
  );
  expect(screen.getByText("IDENTITY=crewlet-acme-1a2b")).toBeTruthy();
  await eventually(() => expect(screen.getByText("IDENTITY=acme-ids")).toBeTruthy());
  expect(spy.mock.calls.at(-1)?.[1]?.body).toBe(
    JSON.stringify({ field: "federation_key", values: { identity_project: "acme-ids" } }),
  );
  rerender(
    <LiveBlock
      kind="google_cloud"
      field="federation_key"
      view={view}
      typed={{ identity_project: "Acme IDs" }}
      onRefused={onRefused}
    />,
  );
  await eventually(() =>
    expect(screen.getByText("Fix the answer above to see the block")).toBeTruthy(),
  );
  expect(screen.queryByText(/IDENTITY=/)).toBeNull();
  expect(onRefused).toHaveBeenLastCalledWith(
    "identity_project",
    "This is not a Google Cloud project ID.",
  );
});
