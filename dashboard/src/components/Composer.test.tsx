/**
 * A reply in the Inbox and a comment on a task send as every message box
 * does: Enter sends and Shift+Enter is a new line, the command key with Enter
 * still sends, an input method's Enter and the `@` list's Enter never do, an
 * empty box sends nothing, and a touch keyboard's Return is a new line. The
 * keys are the kit's (`useSendOnEnter`); this holds the box to them.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Composer } from "./Composer.tsx";
import { AnswerAsk } from "./writes.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { withDerived } from "~/test/org.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const JANE: ViewerState = {
  login: "founder",
  grants: ["admin", "worklet", "doclet"],
  admin: true,
  worklet: true,
  doclet: true,
  chatlet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["comment_on_work_item"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatVendors: [],
};

const ORG = withDerived({
  name: "Nimbus",
  roles: [
    { name: "Ada Okonkwo", handle: "ada", kind: "agent" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
});

let sent: { tool: string; args: Record<string, unknown> }[];
let restoreMedia: (() => void) | null = null;

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(JANE);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(ORG as never);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      sent.push({ tool: new URL(url).pathname.split("/").pop()!, args: body.args });
      return new Response(
        JSON.stringify({ outcome: "applied", position: "CREWLET_TRACKER_LOG@1:3" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  restoreMedia?.();
  restoreMedia = null;
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** A phone: the primary pointer is a finger and no pointer is fine. */
function onAPhone() {
  const original = window.matchMedia;
  window.matchMedia = ((query: string) => ({
    matches: query === "(pointer: coarse)",
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
  restoreMedia = () => {
    window.matchMedia = original;
  };
}

const mount = (node: React.ReactNode) =>
  render(
    <LayerHost>
      <ToastProvider>{node}</ToastProvider>
    </LayerHost>,
  );

function reply() {
  mount(<Composer item="NIM-7" mode={{ kind: "reply" }} to="Ada Okonkwo" index={indexOrg(ORG)} />);
  return screen.getByRole("combobox", { name: "Reply to Ada Okonkwo" }) as HTMLTextAreaElement;
}

const type = (field: HTMLTextAreaElement, value: string) => {
  fireEvent.change(field, { target: { value, selectionStart: value.length } });
  field.setSelectionRange(value.length, value.length);
  fireEvent.select(field);
};

test("Enter sends a reply and Shift+Enter is a new line", async () => {
  const field = reply();
  type(field, "On it");
  expect(fireEvent.keyDown(field, { key: "Enter", shiftKey: true })).toBe(true);
  expect(sent).toHaveLength(0);
  expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(false);
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({ tool: "comment_on_work_item", args: { item: "NIM-7", body: "On it" } });
});

test("the command key with Enter still sends", async () => {
  const field = reply();
  type(field, "On it");
  fireEvent.keyDown(field, { key: "Enter", ctrlKey: true });
  await waitFor(() => expect(sent).toHaveLength(1));
});

test("an empty box sends nothing and writes no line", () => {
  const field = reply();
  expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(false);
  type(field, "   ");
  fireEvent.keyDown(field, { key: "Enter" });
  expect(sent).toHaveLength(0);
});

test("an input method's Enter never sends", () => {
  const field = reply();
  type(field, "了解しました");
  fireEvent.compositionStart(field);
  fireEvent.keyDown(field, { key: "Enter", isComposing: true });
  fireEvent.keyDown(field, { key: "Enter", keyCode: 229 });
  fireEvent.compositionEnd(field);
  expect(sent).toHaveLength(0);
});

test("with the @ list open, Enter picks the highlighted seat and sends nothing", async () => {
  const field = reply();
  type(field, "thanks @ad");
  expect(await screen.findByRole("listbox", { name: "Mention somebody" })).toBeTruthy();
  fireEvent.keyDown(field, { key: "Enter" });
  expect(sent).toHaveLength(0);
  await waitFor(() => expect(field.value).toBe("thanks @ada "));
});

test("the field's description names the send keys", () => {
  const field = reply();
  const description = document.getElementById(field.getAttribute("aria-describedby")!)!;
  expect(description.textContent).toBe("Enter to send, Shift plus Enter for a new line");
});

test("on a touch keyboard Return is a new line and Send sends", async () => {
  onAPhone();
  const field = reply();
  type(field, "On it");
  expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(true);
  expect(sent).toHaveLength(0);
  expect(field.hasAttribute("aria-describedby")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: /^send$/i }));
  await waitFor(() => expect(sent).toHaveLength(1));
});

test("an answer in words sends on Enter, and its hint says the keys", async () => {
  mount(
    <AnswerAsk
      item="NIM-7"
      comment="c-1"
      asker="Ada Okonkwo"
      options={[]}
      open
      onOpenChange={() => {}}
      onAnswered={() => {}}
      layout="buttons"
    />,
  );
  const field = screen.getByRole("textbox", { name: "Reply to Ada Okonkwo" });
  expect(document.querySelector(".crewlet-composer__hint")!.textContent).toMatch(
    /to send, .*for a new line$/,
  );
  expect(document.body.textContent).not.toMatch(/Command|⌘/);
  fireEvent.change(field, { target: { value: "Ship the smaller one" } });
  fireEvent.keyDown(field, { key: "Enter", shiftKey: true });
  expect(sent).toHaveLength(0);
  fireEvent.keyDown(field, { key: "Enter" });
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]!.args).toMatchObject({
    item: "NIM-7",
    answers: "c-1",
    body: "Ship the smaller one",
  });
});
