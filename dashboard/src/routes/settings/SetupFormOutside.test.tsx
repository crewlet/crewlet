/**
 * A setup whose last field a block prints, run at the app: Google Cloud's
 * provider and AWS's stack output. The earlier answers save on their own, the
 * block follows what is saved, and the printed field is needed to finish only.
 */

import { useState } from "react";
import { cleanup, fireEvent, render as rtlRender, screen } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { afterEach, expect, test, vi } from "vitest";
import { SetupForm, checkBlocked, checkLabel, holdsSave, outsideAfterSave } from "./SetupForm.tsx";
import type { SetupRequirement, SetupToolState, SetupVerdict } from "~/protocol/index.ts";
import { eventually } from "~/test/settle.ts";

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});

const OLD = "crewlet-provisioner@acme-old-id.iam.gserviceaccount.com";
const NEW = "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com";
const PROVIDER =
  "projects/1/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine";
const blockFor = (provisioner: string) =>
  `(\nPROV=${provisioner}\ngcloud iam workload-identity-pools ...\n)\n`;

/** Google Cloud's form as the engine answers it, for one saved state. */
function gcp(saved: {
  provisioner?: string;
  provider?: string;
  satisfied?: boolean;
  atlas?: string;
}): SetupToolState {
  const held = saved.provisioner !== undefined;
  return {
    key: "google_cloud",
    configured: held,
    enabled: held,
    satisfied: saved.satisfied ?? false,
    form_complete: false,
    can_provision: true,
    requirements: [
      {
        field: "provisioner",
        label: "Provisioner service account",
        kind: "id",
        config_path: "integrations.google_cloud.provisioning.provisioner",
        required: true,
        connect: true,
        value: saved.provisioner ?? "",
        present: held,
        resolved: held,
      },
      {
        field: "federation_key",
        label: "Federation key",
        kind: "secret",
        config_path: "integrations.google_cloud.provisioning.federation_key",
        required: true,
        connect: true,
        mintable: true,
        check_note: "It is tested with the provider.",
        value: held ? "${GOOGLE_CLOUD_FEDERATION_KEY}" : "",
        present: held,
        resolved: held,
        ...(held
          ? {
              public: {
                text: blockFor(saved.provisioner ?? ""),
                language: "sh",
                drawn_from: ["provisioner", "atlas_read"],
              },
            }
          : {}),
      },
      {
        field: "provider",
        label: "Workload identity provider",
        kind: "id",
        config_path: "integrations.google_cloud.provisioning.provider",
        required: true,
        connect: true,
        printed_by: "federation_key",
        checkable: true,
        check_with: ["provisioner", "federation_key"],
        value: saved.provider ?? "",
        present: saved.provider !== undefined,
        resolved: saved.provider !== undefined,
      },
      {
        field: "atlas_read",
        label: "See more in Atlas",
        kind: "toggle",
        config_path: "integrations.google_cloud.provisioning.atlas_read",
        required: false,
        default: "false",
        value: saved.atlas ?? "false",
        present: saved.atlas === "true",
        resolved: saved.atlas === "true",
      },
    ],
  };
}

/** The writes the form sends, each answered with the state `next` gives. */
function stubWrites(next: (body: { values: Record<string, string> }) => SetupToolState) {
  const bodies: unknown[] = [];
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    if (String(init?.method ?? "GET").toUpperCase() === "GET") {
      return Promise.resolve(new Response("[]", { status: 200 }));
    }
    const body = JSON.parse(String(init?.body ?? "{}")) as { values: Record<string, string> };
    bodies.push({ path: new URL(String(input), "http://engine.test").pathname, body });
    return Promise.resolve(
      new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [], state: next(body) }), {
        status: 201,
      }),
    );
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return bodies;
}

/** The form over a page that redraws it from each write's answer, as the page does. */
function Page({ first, onClose }: { first: SetupToolState; onClose: () => void }) {
  const [tool, setTool] = useState(first);
  return (
    <SetupForm
      sections={[{ name: "Google Cloud", tool }]}
      title="Google Cloud"
      onClose={onClose}
      onDone={() => {
        if (latest.current) setTool(latest.current);
      }}
    />
  );
}
const latest: { current: SetupToolState | null } = { current: null };

function open(first: SetupToolState, next: (values: Record<string, string>) => SetupToolState) {
  latest.current = null;
  const onClose = vi.fn();
  const bodies = stubWrites((body) => {
    latest.current = next(body.values);
    return latest.current;
  });
  rtlRender(
    <ToastProvider>
      <Page first={first} onClose={onClose} />
    </ToastProvider>,
    { wrapper: LayerHost },
  );
  return { onClose, bodies };
}

const provisionerBox = () => screen.getByLabelText("Provisioner service account");
const save = () => fireEvent.click(screen.getByRole("button", { name: /^(Save|Connect)$/ }));

// THE EARLIER ANSWER SAVES WHILE THE PRINTED ONE IS EMPTY: the block that prints
// it is drawn from what is saved, so holding the save would wait on a block the
// form cannot draw. Mutation: hold a save for a printed field (holdsSave as
// neededField) and the form refuses with "Fill this in to continue."
test("a half set up form saves a new provisioner while the provider is empty", async () => {
  const { bodies, onClose } = open(gcp({ provisioner: OLD }), (v) =>
    gcp({ provisioner: v.provisioner }),
  );
  fireEvent.change(provisionerBox(), { target: { value: NEW } });
  save();
  await eventually(() => expect(bodies).toHaveLength(1));
  expect(bodies[0]).toEqual({
    path: "/setup/integrations/google_cloud/inputs",
    body: { values: { provisioner: NEW }, generate: [] },
  });
  expect(screen.queryByText("Fill this in to continue.")).toBeNull();
  // THE NEXT STEP IS THE BLOCK, redrawn for what was saved, so the form stays.
  await eventually(() => expect(screen.getByText(new RegExp(`PROV=${NEW}`))).toBeDefined());
  expect(onClose).not.toHaveBeenCalled();
});

// THE BLOCK FOLLOWS WHAT IS SAVED, never the box: while a field it is drawn from
// holds an unsaved change it is put away with why, and back once the change is.
// Mutation: drop the staleness check and the old project's block stays on show.
test("the block is put away while a field it is drawn from has an unsaved change", () => {
  open(gcp({ provisioner: OLD, provider: PROVIDER, satisfied: true }), () => gcp({}));
  expect(screen.getByText(new RegExp(`PROV=${OLD}`))).toBeDefined();
  fireEvent.change(provisionerBox(), { target: { value: NEW } });
  expect(screen.queryByText(new RegExp(`PROV=${OLD}`))).toBeNull();
  expect(screen.getByText("Save to update this block")).toBeDefined();
  expect(
    screen.getByText(
      "You changed Provisioner service account. This block is drawn from what is saved, so save to " +
        "update it, then run it again and paste its new last line into Workload identity provider.",
    ),
  ).toBeDefined();
  fireEvent.change(provisionerBox(), { target: { value: OLD } });
  expect(screen.getByText(new RegExp(`PROV=${OLD}`))).toBeDefined();
});

// CHANGED AFTER THE BLOCK RAN: the save redraws the block and says to run it
// again and paste its new last line, until a new value is pasted. Mutation:
// close the form on save, or never flag the changed block, and this fails.
test("a save that changes the block under a pasted provider says to run it again", async () => {
  const { onClose } = open(gcp({ provisioner: OLD, provider: PROVIDER, satisfied: true }), (v) =>
    gcp({ provisioner: v.provisioner ?? OLD, provider: v.provider ?? PROVIDER, satisfied: true }),
  );
  fireEvent.change(provisionerBox(), { target: { value: NEW } });
  save();
  await eventually(() =>
    expect(screen.getByText("This block changed when you saved")).toBeDefined(),
  );
  expect(
    screen.getByText("Run it again, then paste its new last line into Workload identity provider."),
  ).toBeDefined();
  expect(screen.getByText(new RegExp(`PROV=${NEW}`))).toBeDefined();
  expect(onClose).not.toHaveBeenCalled();
  fireEvent.change(screen.getByLabelText("Workload identity provider"), {
    target: { value: PROVIDER.replace("projects/1/", "projects/2/") },
  });
  expect(screen.queryByText("This block changed when you saved")).toBeNull();
});

// THE FIRST SAVE IS PROGRESS, NOT A CONNECT: it mints the key and draws the
// block, so the toast says saved and the form stays on the block. Mutation:
// keep the old "connected on connect" toast and this fails.
test("the first save draws the block in place and does not say connected", async () => {
  // The engine stores what the first save sent, the toggles a new form opens On included.
  const { onClose, bodies } = open(gcp({}), (v) =>
    gcp({ provisioner: v.provisioner, atlas: v.atlas_read }),
  );
  fireEvent.change(provisionerBox(), { target: { value: NEW } });
  save();
  await eventually(() => expect(screen.getByText(new RegExp(`PROV=${NEW}`))).toBeDefined());
  expect((bodies[0] as { body: { generate: string[] } }).body.generate).toEqual(["federation_key"]);
  await eventually(() =>
    expect(screen.getAllByText(/Google Cloud settings saved/).length).toBeGreaterThan(0),
  );
  expect(screen.queryAllByText(/Google Cloud connected/)).toHaveLength(0);
  expect(onClose).not.toHaveBeenCalled();
});

// FINISHING: the pasted provider completes the setup, so the form closes and
// says connected. Mutation: keep the form open whenever a block exists.
test("pasting the provider and saving finishes, closes and says connected", async () => {
  const { onClose } = open(gcp({ provisioner: NEW }), (v) =>
    gcp({ provisioner: NEW, provider: v.provider, satisfied: true }),
  );
  fireEvent.change(screen.getByLabelText("Workload identity provider"), {
    target: { value: PROVIDER },
  });
  save();
  await eventually(() => expect(onClose).toHaveBeenCalled());
  await eventually(() =>
    expect(screen.getAllByText(/Google Cloud connected/).length).toBeGreaterThan(0),
  );
});

// THE TEST UNDER A FIELD THAT IS NO KEY IS A CONNECTION TEST, enabled once the
// provider is pasted (the key Crewlet made counts once held), and its result
// names the part that failed. Mutation: count the minted key only when typed,
// and the control never enables.
test("the provider's test is named Test connection and runs with the held key", async () => {
  const verdict: SetupVerdict = {
    outcome: "refused",
    summary: "Google's Security Token Service refused Crewlet's federation token.",
    facts: [{ label: "Federation", value: "Failed" }],
    warnings: [{ action: "Run the Cloud Shell block again." }],
  };
  const checks: unknown[] = [];
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(init?.method ?? "GET").toUpperCase() === "GET") {
        return Promise.resolve(new Response("[]", { status: 200 }));
      }
      checks.push({
        path: new URL(String(input), "http://engine.test").pathname,
        body: JSON.parse(String(init?.body)),
      });
      return Promise.resolve(new Response(JSON.stringify(verdict), { status: 200 }));
    }),
  });
  rtlRender(
    <SetupForm
      sections={[{ name: "Google Cloud", tool: gcp({ provisioner: NEW }) }]}
      title="Google Cloud"
      onClose={() => {}}
      onDone={() => {}}
    />,
    { wrapper: LayerHost },
  );
  const button = screen.getByRole("button", { name: "Test connection" });
  expect(button.getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(screen.getByLabelText("Workload identity provider"), {
    target: { value: PROVIDER },
  });
  expect(button.getAttribute("aria-disabled")).not.toBe("true");
  fireEvent.click(button);
  await screen.findByText(verdict.summary);
  expect(screen.getByText("Federation")).toBeDefined();
  expect(checks).toHaveLength(1);
  expect(checks[0]).toMatchObject({
    path: "/setup/integrations/google_cloud/verify",
    body: { field: "provider", value: PROVIDER, values: { provisioner: NEW } },
  });
});

const field = (over: Partial<SetupRequirement>): SetupRequirement => ({
  field: "f",
  label: "F",
  kind: "id",
  config_path: "x.f",
  required: true,
  present: false,
  ...over,
});

// ONE RULE FOR WHAT HOLDS A SAVE, one name for each control, and what a save
// leaves at the app, as pure answers.
test("a printed field never holds a save, and the control is named for what it checks", () => {
  const none = () => "";
  expect(holdsSave(field({}), none)).toBe(true);
  expect(holdsSave(field({ printed_by: "key" }), none)).toBe(false);
  expect(checkLabel(field({}))).toBe("Test connection");
  expect(checkLabel(field({ kind: "secret" }))).toBe("Test key");
  expect(checkLabel(field({ kind: "secret", check_with: ["site"] }))).toBe("Test keys");
  expect(checkBlocked(field({}), ["Stack output"])).toBe(
    "Fill in Stack output to test the connection",
  );
  expect(checkBlocked(field({}), [])).toBeUndefined();
});

test("a save leaves a block to run where the printed value is missing or its block changed", () => {
  const block = (text: string) =>
    field({
      field: "key",
      kind: "secret",
      mintable: true,
      present: true,
      public: { text, language: "sh" },
    });
  const printed = (present: boolean) => field({ field: "out", printed_by: "key", present });
  expect(outsideAfterSave([block("a"), printed(false)], [block("a"), printed(false)])).toEqual({
    rerun: [],
    pending: true,
  });
  expect(outsideAfterSave([block("a"), printed(true)], [block("b"), printed(true)])).toEqual({
    rerun: ["key"],
    pending: false,
  });
  expect(outsideAfterSave([block("a"), printed(true)], [block("a"), printed(true)])).toEqual({
    rerun: [],
    pending: false,
  });
});
