/**
 * Company › Atlas, changed: what a holder of Atlas adds, corrects and decides,
 * what each control sends the engine, and what a reader who may not write is
 * told instead. Every control is drawn for every reader; none is hidden.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import {
  disputedServer,
  entryAnswer,
  fact,
  grownCompany,
  reviewAnswer,
  summary,
  waitingItems,
} from "~/test/atlas.ts";
import type { AtlasEdge, AtlasEntryAnswer } from "~/contract/atlas.ts";
import { mountAtlas } from "~/test/atlasMount.tsx";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { Atlas } from "./Atlas.tsx";
import { ChangesPage } from "./ChangesPage.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { touchingSentence } from "./entry.tsx";
import { SEATLESS } from "./access.tsx";
import { PURGE_HELD } from "./EntryActions.tsx";
import { Review } from "./Review.tsx";
import { addArgs, draftKey, factsOf, relationsFrom, reviewDecisions, undecided } from "./writes.ts";

const ATLAS_ACTS = [
  "atlas_add_entry",
  "atlas_correct",
  "atlas_confirm",
  "atlas_relate",
  "atlas_unrelate",
  "atlas_set_owner",
  "atlas_retire",
  "atlas_restore",
  "atlas_merge",
  "atlas_split",
];

/** A holder of Atlas who is no administrator. */
const ADA = {
  login: "U0ADA",
  grants: ["atlas"],
  handle: "ada",
  owner: "ada",
  name: "Ada",
  kind: "human",
  acts: ATLAS_ACTS,
};

type Reply = { status: number; body: unknown };
let posted: { tool: string; args: Record<string, unknown> }[];
let reply: (tool: string) => Reply;

const applied = (tool: string, receipt: unknown = {}): Reply => ({
  status: 200,
  body: { tool, outcome: "applied", position: "CREWLET_ATLAS_LOG@1:9", receipt },
});

beforeEach(() => {
  posted = [];
  reply = (tool) => applied(tool);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      const answer = reply(tool);
      return new Response(JSON.stringify(answer.body), {
        status: answer.status,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** The sentence a held control is described by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

/**
 * The disputed server's page, for `viewer`. The settings declare it (an SSH
 * server); `{ declared: false }` draws it as only people, agents and an
 * integration said it, for a case about the reader rather than the entry.
 */
function mountEntry(viewer: unknown, tab = "", { declared = true } = {}) {
  const company = grownCompany();
  const mounted = mountAtlas(
    `#/company/atlas/${company.web1.id}${tab ? `?tab=${tab}` : ""}`,
    <EntryPage id={company.web1.id} />,
    company,
    {
      entry: (id) =>
        id !== company.web1.id
          ? undefined
          : declared
            ? disputedServer(company)
            : undeclared(disputedServer(company)),
      answers: { viewer },
    },
  );
  return { company, ...mounted };
}

/** An entry's answer with every claim the settings or the engine's records made said by a person. */
function undeclared(answer: AtlasEntryAnswer): AtlasEntryAnswer {
  const person = { kind: "person" as const, name: "ada" };
  const computed = (kind: string) => kind === "settings" || kind === "records";
  return {
    ...answer,
    entry: {
      ...answer.entry,
      sources: answer.entry.sources.filter((s) => !computed(s.kind)),
      relations: answer.entry.relations.map((edge) =>
        computed(edge.source.kind) ? { ...edge, source: person } : edge,
      ),
    },
  };
}

describe("what the write controls send", () => {
  test("an account's key is its vendor and what was typed, and only what was filled in is sent", () => {
    const draft = {
      kind: "vendor_account" as const,
      key: "acme-cdn",
      name: "Acme CDN",
      vendor: "other" as const,
      account: "not-for-an-account",
      purpose: "  Serves the storefront's images ",
      owner: { kind: "unit" as const, name: "platform" },
      page: null,
    };
    expect(draftKey(draft)).toBe("other/acme-cdn");
    expect(addArgs(draft)).toEqual({
      kind: "vendor_account",
      key: "other/acme-cdn",
      name: "Acme CDN",
      purpose: "Serves the storefront's images",
      unit: "platform",
    });
    expect(
      addArgs({ ...draft, kind: "datastore", key: "acme/orders-db", account: "an-id" }).belongs_to,
    ).toBe("an-id");
  });

  test("a kind is offered only the facts and relations the engine's vocabulary gives it", () => {
    expect(factsOf("service")).toEqual(["name", "purpose", "environment", "importance"]);
    expect(factsOf("host")).toContain("address");
    expect(relationsFrom("hostname").map((one) => one.relation)).toEqual([
      "resolves_to",
      "in_zone",
      "fronted_by",
      "deployed_by",
      "monitored_by",
      "secured_by",
      "belongs_to",
    ]);
    expect(relationsFrom("vendor_account")).toEqual([]);
  });

  test("a disagreement is decided by keeping the value shown or taking the other, an entry by confirming or retiring", () => {
    const [disagreement, guess] = waitingItems(grownCompany());
    expect(reviewDecisions(disagreement!).map((one) => [one.tool, one.label, one.args])).toEqual([
      [
        "atlas_confirm",
        "Keep 10.0.4.12",
        { atlas_entry: "host:ssh/web-1", fact: "address", value: "10.0.4.12" },
      ],
      [
        "atlas_confirm",
        "Take 10.0.4.19",
        { atlas_entry: "host:ssh/web-1", fact: "address", value: "10.0.4.19" },
      ],
    ]);
    expect(reviewDecisions(guess!).map((one) => one.tool)).toEqual([
      "atlas_confirm",
      "atlas_retire",
    ]);
    // WHAT THE SETTINGS DECLARE IS SETTLED THERE: no button that could only be refused.
    const declared = {
      ...disagreement!,
      fact: { ...disagreement!.fact!, source: { kind: "settings" as const, revision: "r7" } },
    };
    expect(reviewDecisions(declared)).toEqual([]);
    expect(undecided(declared)).toMatch(/settled there/);
    expect(undecided(disagreement!)).toBeNull();
  });

  test("the words over an entry's diagram count what the diagram draws, the entries around it among them", () => {
    expect(touchingSentence({ usedBy: 0, reliesOn: 0, around: 1 })).toBe(
      "Nothing relies on it, it relies on nothing, and 1 stands around it.",
    );
    // Mutation: count none as "0 entries" again.
    expect(touchingSentence({ usedBy: 0, reliesOn: 0, around: 0 })).toBe(
      "Nothing relies on it, and it relies on nothing.",
    );
    expect(touchingSentence({ usedBy: 1, reliesOn: 2, around: 0 })).toBe(
      "1 entry relies on it, and it relies on 2.",
    );
  });
});

describe("a reader who may not write", () => {
  test("is shown every control held, with why: nobody signed in", async () => {
    const viewer = { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] };
    mountAtlas("#/company/atlas", <Atlas />, grownCompany(), { answers: { viewer } });
    const add = await screen.findByRole("button", { name: "Add entry" });
    await waitFor(() => expect(reasonOf(add)).toContain(WRITE_REASONS.anonymous));
    expect(add.getAttribute("aria-disabled")).toBe("true");
  });

  test("is told the engine does not make the change for them, or the access it takes", async () => {
    mountEntry({ ...ADA, acts: [] });
    const confirm = await screen.findByRole("button", { name: "Confirm" });
    await waitFor(() => expect(reasonOf(confirm)).toContain(WRITE_REASONS.not_served));
    expect(reasonOf(screen.getByRole("button", { name: "Correct" }))).toContain(
      WRITE_REASONS.not_served,
    );
    cleanup();
    mountEntry({ ...ADA, grants: ["worklet"], acts: [] });
    const held = await screen.findByRole("button", { name: "Correct" });
    await waitFor(() => expect(reasonOf(held)).toMatch(/Atlas access/));
    fireEvent.click(held);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posted).toEqual([]);
  });

  // A SIGN-IN WITH NO SEAT (an API token, an account bound to none) is offered
  // no atlas change by the engine, whatever it holds. MUTATION: leave the
  // shared catch-all sentence and every control says "this can't be changed
  // from here", with neither the cause nor anywhere to go.
  test("a sign-in with no seat is told that is why, once, with where a person takes a seat", async () => {
    const token = { ...ADA, grants: ["admin", "atlas"], handle: "", owner: "token:ci", acts: [] };
    mountEntry(token, "", { declared: false });
    const confirm = await screen.findByRole("button", { name: "Confirm" });
    await waitFor(() => expect(reasonOf(confirm)).toContain(SEATLESS));
    expect(reasonOf(screen.getByRole("button", { name: "Correct" }))).toContain(SEATLESS);
    const note = document.querySelector(".atlas-actions__note")!;
    expect(note.textContent).toContain(SEATLESS);
    expect(
      within(note as HTMLElement)
        .getByRole("link")
        .getAttribute("href"),
    ).toBe("#/company/org");
    fireEvent.click(screen.getByRole("button", { name: /More to do with web-1/ }));
    // SAID ONCE, above the items, which stay listed and held. Mutation: leave
    // each item its own copy of the sentence.
    const menu = await screen.findByRole("menu");
    const panel =
      menu.closest("[role='dialog'], .crewlet-menu, .crewlet-popover") ?? menu.parentElement!;
    expect(panel.textContent!.split(SEATLESS)).toHaveLength(2);
    for (const name of [/Set owner/, /Link a page/, /Merge with/, /Split/, /Retire/, /Purge/]) {
      const item = await screen.findByRole("menuitem", { name });
      expect(item.getAttribute("aria-disabled"), String(name)).toBe("true");
      expect(item.textContent, String(name)).not.toContain(SEATLESS);
      expect(item.textContent, String(name)).not.toContain(WRITE_REASONS.not_served);
    }
  });

  test("a reader who holds a seat is never told they hold none", async () => {
    mountEntry({ ...ADA, acts: [] }, "", { declared: false });
    const confirm = await screen.findByRole("button", { name: "Confirm" });
    await waitFor(() => expect(reasonOf(confirm)).toContain(WRITE_REASONS.not_served));
    expect(document.querySelector(".atlas-actions__note")).toBeNull();
  });

  // A PURGED ENTRY IS LEFT: the toast says it is gone, and its page stayed
  // drawn with every control live until a reload said Not Found. Mutation:
  // drop the leave on success and the address stays on the erased entry.
  test("a purge of the entry leaves its page for the atlas", async () => {
    const admin = { ...ADA, grants: ["atlas", "admin"], acts: [...ATLAS_ACTS, "atlas_purge"] };
    // UNDECLARED: an entry the settings or the engine's records declare holds Purge.
    const { company } = mountEntry(admin, "", { declared: false });
    fireEvent.click(await screen.findByRole("button", { name: /More to do with web-1/ }));
    const purge = await screen.findByRole("menuitem", { name: /Purge/ });
    await waitFor(() => expect(purge.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(purge);
    const dialog = await screen.findByRole("alertdialog", { name: "Purge from the atlas" });
    // THE KEY IS TYPED BACK, as the dialog asks.
    const key = disputedServer(company).entry.ref.key;
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: key } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Erase for good" }));
    await waitFor(() => expect(posted.map((one) => one.tool)).toEqual(["atlas_purge"]));
    await waitFor(() => expect(location.hash).toBe("#/company/atlas"));
    expect(location.hash).not.toContain(company.web1.id);
  });

  test("a purge is held for anybody but an administrator, in words", async () => {
    mountEntry(ADA, "", { declared: false });
    fireEvent.click(await screen.findByRole("button", { name: /More to do with web-1/ }));
    const purge = await screen.findByRole("menuitem", { name: /Purge/ });
    expect(purge.getAttribute("aria-disabled")).toBe("true");
    expect(purge.textContent).toContain(PURGE_HELD);
  });
});

/*
 * WHAT THE COMPANY SETTINGS DECLARE IS CHANGED THERE. The engine reads a
 * settings entry (a model, a tool server, an SSH server) again on every read
 * and refuses to retire or purge it, to change an owner they name, or to take
 * back a relation they declare: a retired or purged model came straight back.
 * The More menu offered all of it, enabled, and nothing said the settings
 * re-create the entry. Now those are held with who declares it and where it is
 * changed, a note beside the controls says so with the page, and what the
 * engine records beside a declared entry (a page, an owner they name none
 * for, a merge, a split) stays offered. Mutation: build the menu without
 * `declaredBy` and Retire and Purge are enabled again.
 */
describe("an entry the company settings declare", () => {
  /** An administrator who holds Atlas and may purge. */
  const ADMIN = { ...ADA, grants: ["admin", "atlas"], acts: [...ATLAS_ACTS, "atlas_purge"] };
  const SETTINGS = { kind: "settings" as const, revision: "r7" };

  async function mountModel(relations: AtlasEdge[] = []) {
    const model = summary("model_provider", "default", { key: "llm/default", vendor: "anthropic" });
    mountAtlas(`#/company/atlas/${model.id}`, <EntryPage id={model.id} />, grownCompany(), {
      entry: (id) => (id === model.id ? entryAnswer(model, { relations }) : undefined),
      answers: { viewer: ADMIN },
    });
    fireEvent.click(await screen.findByRole("button", { name: /More to do with default/ }));
  }
  const item = (name: RegExp) => screen.findByRole("menuitem", { name });

  test("holds what the engine refuses, with where it is changed, and offers what it records", async () => {
    await mountModel();
    for (const [name, says] of [
      [/Retire/, "The company settings declare it. Remove it there to retire it."],
      [/Purge/, "The company settings declare it, so the next read brings it back."],
    ] as const) {
      const held = await item(name);
      expect(held.getAttribute("aria-disabled"), String(name)).toBe("true");
      expect(held.textContent, String(name)).toContain(says);
    }
    for (const name of [/Set owner/, /Link a page/, /Merge with/, /Split/]) {
      await waitFor(async () =>
        expect((await item(name)).getAttribute("aria-disabled"), String(name)).not.toBe("true"),
      );
    }
    const note = document.querySelector<HTMLElement>(".atlas-actions__note")!;
    expect(note.textContent).toContain("The company settings declare this entry");
    expect(within(note).getByRole("link", { name: "Open Models" }).getAttribute("href")).toBe(
      "#/settings/models",
    );
  });

  test("an owner or a relation the settings declare is changed there", async () => {
    const declared = {
      source: SETTINGS,
      confidence: "declared" as const,
      word: "confirmed" as const,
      observed_at: "2026-11-11T08:00:00Z",
      standing: "settled" as const,
    };
    await mountModel([
      { relation: "owned_by", external: "unit", name: "platform", ...declared },
      { relation: "documented_in", external: "page", name: "page-1", ...declared },
    ]);
    const owner = await item(/Set owner/);
    expect(owner.getAttribute("aria-disabled")).toBe("true");
    expect(owner.textContent).toContain("The company settings name its owner. Change it there.");
    const unrelate = await item(/Remove relation/);
    expect(unrelate.getAttribute("aria-disabled")).toBe("true");
    expect(unrelate.textContent).toContain("The company settings declare its relations.");
  });
});

describe("a holder of Atlas", () => {
  test("adds an entry in one write, and the engine's refusal stays beside the form", async () => {
    reply = (tool) => ({
      status: 422,
      body: {
        error: "invalid",
        tool,
        detail: "The key looks like a credential, so nothing was recorded.",
      },
    });
    mountAtlas("#/company/atlas", <Atlas />, grownCompany(), { answers: { viewer: ADA } });
    const add = await screen.findByRole("button", { name: "Add entry" });
    await waitFor(() => expect(add.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(add);
    const dialog = await screen.findByRole("dialog", { name: "Add entry" });
    fireEvent.change(within(dialog).getByLabelText(/^Name/), {
      target: { value: "Checkout API" },
    });
    // A SERVICE'S KEY FOLLOWS ITS NAME until somebody types one.
    expect((within(dialog).getByLabelText(/^Key/) as HTMLInputElement).value).toBe("checkout-api");
    fireEvent.change(within(dialog).getByLabelText(/^Purpose/), {
      target: { value: "Takes payments" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add entry" }));
    await waitFor(() =>
      expect(posted).toEqual([
        {
          tool: "atlas_add_entry",
          args: {
            kind: "service",
            key: "checkout-api",
            name: "Checkout API",
            purpose: "Takes payments",
          },
        },
      ]),
    );
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "looks like a credential",
    );
    expect(screen.getByRole("dialog", { name: "Add entry" })).toBeTruthy();
  });

  test("corrects a fact with the atlas's value and the differing one shown beside the field", async () => {
    mountEntry(ADA);
    const correct = await screen.findByRole("button", { name: "Correct" });
    await waitFor(() => expect(correct.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(correct);
    const dialog = await screen.findByRole("dialog", { name: "Correct web-1" });
    expect(within(dialog).getByText(/Nothing settles in silence/)).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText(/^Your value/), {
      target: { value: "Runs the storefront" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Record the value" }));
    await waitFor(() =>
      expect(posted).toEqual([
        {
          tool: "atlas_correct",
          args: { atlas_entry: "host:ssh/web-1", fact: "purpose", value: "Runs the storefront" },
        },
      ]),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  test("decides a disagreement on the entry's Sources tab, one press a decision", async () => {
    mountEntry(ADA, "sources");
    const section = await screen.findByRole("region", { name: "Where the sources disagree" });
    const take = await within(section).findByRole("button", { name: "Take 10.0.4.19" });
    await waitFor(() => expect(take.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(take);
    await waitFor(() =>
      expect(posted).toEqual([
        {
          tool: "atlas_confirm",
          args: { atlas_entry: "host:ssh/web-1", fact: "address", value: "10.0.4.19" },
        },
      ]),
    );
  });

  test("reads an entry's whole history from the atlas log on its History tab", async () => {
    const { asked, company } = mountEntry(ADA, "history");
    await screen.findByRole("region", { name: "Every change, newest first" });
    expect(asked.some((one) => one.what === "atlas_log" && one.params.id === company.web1.id)).toBe(
      true,
    );
  });
});

describe("Review", () => {
  test("lists what waits with its decisions, counted by reason", async () => {
    const company = grownCompany();
    mountAtlas("#/company/atlas/review", <Review />, company, {
      answers: { viewer: ADA, atlas_review: reviewAnswer(waitingItems(company)) },
    });
    expect(await screen.findByRole("link", { name: "web-1" })).toBeTruthy();
    expect(screen.getByText("10.0.4.12")).toBeTruthy();
    expect(screen.getByText("10.0.4.19")).toBeTruthy();
    expect(screen.getByRole("tab", { name: /Everything/ }).textContent).toContain("2");
    expect(screen.getByRole("button", { name: "Keep 10.0.4.12" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retire" })).toBeTruthy();
  });

  test("shows an agent's record with its evidence: the seat, the turn and the call it cited", async () => {
    const company = grownCompany();
    mountAtlas("#/company/atlas/review", <Review />, company, {
      answers: { viewer: ADA, atlas_review: reviewAnswer(waitingItems(company)) },
    });
    // THE ENTRY ONLY AN AGENT REPORTED: who said it, and the call behind it.
    const reported = await screen.findByRole("list", {
      name: "What was reported, and the call behind it",
    });
    expect(within(reported).getByText("read_channel")).toBeTruthy();
    expect(within(reported).getByText(/the engine could check nothing behind it/)).toBeTruthy();
    expect(within(reported).getByRole("link", { name: /turn/i })).toBeTruthy();
    // AND THE AGENT'S VALUE BESIDE A PERSON'S names its call too.
    expect(screen.getByText("ssh_read")).toBeTruthy();
    expect(screen.getByText(/whose answer the engine checked the record against/)).toBeTruthy();
  });

  test("a bulk decision lists what it decides and sends only on the dialog's own press", async () => {
    const company = grownCompany();
    mountAtlas("#/company/atlas/review", <Review />, company, {
      answers: { viewer: ADA, atlas_review: reviewAnswer(waitingItems(company)) },
    });
    for (const box of await screen.findAllByRole("checkbox")) fireEvent.click(box);
    fireEvent.click(await screen.findByRole("button", { name: "Accept as shown" }));
    const dialog = await screen.findByRole("dialog", { name: "Accept 2 items as shown?" });
    expect(within(dialog).getByText(/keep 10\.0\.4\.12/)).toBeTruthy();
    expect(within(dialog).getByText(/ledger/)).toBeTruthy();
    // AN AGENT'S ENTRY IS LISTED WITH WHO REPORTED IT, before it is confirmed.
    expect(within(dialog).getByText(/reported by sre/)).toBeTruthy();
    // LISTED, NOT SENT: nothing leaves until the dialog's own button.
    expect(posted).toEqual([]);
    const send = within(dialog).getByRole("button", { name: "Accept as shown" });
    await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(send);
    await waitFor(() =>
      expect(posted).toEqual([
        {
          tool: "atlas_confirm",
          args: { atlas_entry: "host:ssh/web-1", fact: "address", value: "10.0.4.12" },
        },
        { tool: "atlas_confirm", args: { atlas_entry: "service:ledger" } },
      ]),
    );
  });

  test("says since when each item has waited, in words", async () => {
    // Three hours after the first item began waiting; the other waits from 41 days back.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-11T13:30:00Z"));
    const company = grownCompany();
    const [disagreement, guess] = waitingItems(company);
    const items = [disagreement!, { ...guess!, since: "2026-10-01T09:00:00Z" }];
    mountAtlas("#/company/atlas/review", <Review />, company, {
      answers: { viewer: ADA, atlas_review: reviewAnswer(items) },
    });
    expect(await screen.findByText("Waiting since 3 hours ago")).toBeTruthy();
    expect(screen.getByText("Waiting since Oct 01")).toBeTruthy();
  });

  test("says so when nothing waits", async () => {
    mountAtlas("#/company/atlas/review", <Review />, grownCompany(), {
      answers: { viewer: ADA, atlas_review: reviewAnswer([]) },
    });
    expect(await screen.findByText("Nothing waits for a person")).toBeTruthy();
  });
});

describe("the band and Changes", () => {
  test("the band counts what waits in Review and leads to every change", async () => {
    const company = grownCompany();
    mountAtlas("#/company/atlas", <Atlas />, company, {
      answers: { viewer: ADA, atlas_review: reviewAnswer(waitingItems(company)) },
    });
    const review = await screen.findByRole("link", { name: "Review: 2 waiting" });
    expect(review.getAttribute("href")).toBe("#/company/atlas/review");
    expect(screen.getByRole("link", { name: "Every change" }).getAttribute("href")).toBe(
      "#/company/atlas/changes",
    );
  });

  test("Changes reads the company's log, each line led by its entry", async () => {
    const { asked } = mountAtlas("#/company/atlas/changes", <ChangesPage />, grownCompany(), {
      answers: { viewer: ADA },
    });
    await screen.findByRole("region", { name: "Every change" });
    expect(asked.some((one) => one.what === "atlas_log" && one.params.id === undefined)).toBe(true);
    expect(asked.some((one) => one.what === "atlas_activity")).toBe(true);
  });
});

// A FIXTURE'S OWN SHAPES, so a suite that builds an entry by hand reads as the engine's.
test("the fixtures draw an unconfirmed entry and a contested fact as the engine sends them", () => {
  expect(summary("service", "ledger", { word: "unconfirmed" }).word).toBe("unconfirmed");
  expect(fact("port", { type: "port", text: "22" }).standing).toBe("settled");
});
