/**
 * A project's labels and its own fields, edited where About lists them: what
 * each press sends to `write_project`, and that the fields go whole.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ProjectFields, ProjectLabels, identifierOf, optionsAfter } from "./ProjectSettings.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { withDerived } from "~/test/org.ts";
import type { WorkProjectDetail } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const JANE = {
  login: "founder",
  grants: ["admin", "worklet"],
  admin: true,
  worklet: true,
  doclet: true,
  chatlet: true,
  gitlet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["write_project"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  failed: null,
  failedRefusal: null,
  configWriter: true,
  configManagedBy: [],
  assistant: "no_model",
  assistantModel: null,
  assistantReadsImages: false,
  assistantAssigned: null,
  assistantCanAssign: false,
  chatVendors: [],
} as unknown as ViewerState;

let sent: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(JANE);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(withDerived({ name: "Nimbus", roles: [] }) as never);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({
        tool: new URL(url).pathname.split("/").pop()!,
        args: (JSON.parse(init.body as string) as { args: Record<string, unknown> }).args,
      });
      return new Response(
        JSON.stringify({ outcome: "applied", position: "CREWLET_TRACKER_LOG@1:3" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const NOTE = { id: "f-note", slug: "note", name: "Note", type: "text" };
const OLD = { id: "f-old", slug: "old", name: "Old", type: "text", archived: true };
const IMPACT = { id: "f-impact", slug: "impact", name: "Impact", type: "text" };

const DETAIL = {
  key: "ENG",
  name: "Engineering",
  tags: [{ slug: "api", label: "API" }],
  archived_tags: [{ slug: "legacy", label: "Legacy", archived: true }],
  own_fields: [NOTE, OLD],
  fields: [{ type: "", fields: [IMPACT, NOTE] }],
  types: [{ slug: "bug", name: "Bug" }],
  policy_stamp: 1,
} as unknown as WorkProjectDetail;

const mount = (node: React.ReactNode) =>
  render(
    <LayerHost>
      <ToastProvider>{node}</ToastProvider>
    </LayerHost>,
  );

// A LABEL IS DECLARED BY ITS WORDS, which the engine makes a slug of; renamed
// by its slug, which never changes; and archived after the press says it is
// one-way. Mutation: send the rename keyed by label and the slug map is wrong.
test("labels are declared, renamed and archived through write_project's tag facets", async () => {
  mount(<ProjectLabels detail={DETAIL} />);
  // THE ARCHIVED ONES ARE LISTED APART, since the tasks under one keep it.
  expect(screen.getByText("Legacy")).toBeTruthy();
  expect(screen.getByText(/They take no new work/)).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Add label" }));
  let dialog = await screen.findByRole("dialog", { name: "Add a label to Engineering" });
  fireEvent.change(within(dialog).getByLabelText("Label"), { target: { value: "Needs design" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add label" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({
    tool: "write_project",
    args: { project: "ENG", tags_add: [{ slug: "Needs design", label: "Needs design" }] },
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(screen.getByRole("button", { name: "Rename the label API" }));
  dialog = await screen.findByRole("dialog", { name: "Rename the label API" });
  fireEvent.change(within(dialog).getByLabelText("Label"), { target: { value: "Public API" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1]!.args).toEqual({ project: "ENG", tags_rename: { api: "Public API" } });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(screen.getByRole("button", { name: "Archive the label API" }));
  const confirm = await screen.findByRole("alertdialog");
  expect(confirm.textContent).toContain("cannot be brought back");
  fireEvent.click(within(confirm).getByRole("button", { name: "Archive label" }));
  await waitFor(() => expect(sent).toHaveLength(3));
  expect(sent[2]!.args).toEqual({ project: "ENG", tags_archive: ["api"] });
});

// THE FIELDS GO WHOLE: `fields` REPLACES the project's own declarations, so a
// press sends every one the project holds (the archived one included) with
// its change, and never the effective set, which would copy the company's
// field into the project. Mutation: build `fields` from `detail.fields` and
// Impact rides along while Old is dropped.
test("a field is added and archived by sending the project's own set whole", async () => {
  mount(<ProjectFields detail={DETAIL} />);
  // THE COMPANY'S FIELD IS LISTED, and not as the project's own.
  expect(
    within(screen.getByRole("list", { name: "This project's fields" })).queryByText("Impact"),
  ).toBeNull();
  expect(screen.getByText("Impact")).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Add field" }));
  const dialog = await screen.findByRole("dialog", { name: "Add a field to Engineering" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Customer tier" } });
  expect((within(dialog).getByLabelText("Slug") as HTMLInputElement).value).toBe("customer_tier");
  fireEvent.click(within(dialog).getByRole("button", { name: "Add field" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]!.args).toEqual({
    project: "ENG",
    fields: [
      { id: "f-note", slug: "note", name: "Note", type: "text" },
      { id: "f-old", slug: "old", name: "Old", type: "text", archived: true },
      { slug: "customer_tier", name: "Customer tier", type: "text" },
    ],
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(screen.getByRole("button", { name: "Archive the field Note" }));
  const confirm = await screen.findByRole("alertdialog");
  fireEvent.click(within(confirm).getByRole("button", { name: "Archive field" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1]!.args).toEqual({
    project: "ENG",
    fields: [
      { id: "f-note", slug: "note", name: "Note", type: "text", archived: true },
      { id: "f-old", slug: "old", name: "Old", type: "text", archived: true },
    ],
  });
});

test("a field's slug is an identifier the engine takes", () => {
  expect(identifierOf("Customer tier")).toBe("customer_tier");
  expect(identifierOf("  Q3: revenue / cost ")).toBe("q3_revenue_cost");
  expect(identifierOf("2nd reviewer")).toBe("f_2nd_reviewer");
  expect(identifierOf("x".repeat(40))).toHaveLength(32);
});

// A CHOICE TAKEN OUT IS ARCHIVED, never dropped: a task that chose it holds its
// id, and a dropped option would leave that value naming nothing.
test("a choice kept keeps its id, a new one is minted, and one taken out is archived", () => {
  const was = [
    { id: "o-1", slug: "low", name: "Low" },
    { id: "o-2", slug: "high", name: "High" },
  ];
  expect(optionsAfter(was, ["High", "Urgent"])).toEqual([
    { id: "o-2", slug: "high", name: "High" },
    { slug: "urgent", name: "Urgent" },
    { id: "o-1", slug: "low", name: "Low", archived: true },
  ]);
});

// A READER WHO CANNOT WRITE IS TOLD WHY on every control, and none sends: the
// gate's own reasons for a reader nobody signed in and one the engine does not
// make the change for. Mutation: draw a row's Rename as a plain button and it
// stays pressable for both.
test.each([
  ["nobody signed in", { ...JANE, anonymous: true, login: "" }, WRITE_REASONS.anonymous],
  ["a person the engine does not serve", { ...JANE, acts: [] }, WRITE_REASONS.not_served],
] as const)(
  "for %s every label and field control is held, with the reason",
  (_who, viewer, reason) => {
    vi.mocked(useViewer).mockReturnValue(viewer as unknown as ViewerState);
    mount(
      <>
        <ProjectLabels detail={DETAIL} />
        <ProjectFields detail={DETAIL} />
      </>,
    );
    for (const name of [
      "Add label",
      "Rename the label API",
      "Archive the label API",
      "Add field",
      "Edit the field Note",
      "Archive the field Note",
    ]) {
      const button = screen.getByRole("button", { name });
      expect(button.getAttribute("aria-disabled"), name).toBe("true");
      fireEvent.click(button);
    }
    expect(document.body.textContent).toContain(reason);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(sent).toHaveLength(0);
  },
);
