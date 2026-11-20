/**
 * A change card's records are drawn as themselves by one renderer: an artifact
 * with its kind's glyph, a seat with its face and name, each opened in place.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ArtifactKindGlyph } from "@crewlethq/ui";
import type {
  AssistantCardEvent,
  AssistantProposalCard,
  AssistantRef,
} from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { CardBlock, type CardHandlers } from "./blocks.tsx";
import type { AssistantPhrase } from "./phrase.ts";
import type { InAppTarget } from "./openInApp.ts";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-24T09:42:00Z"));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

/** The chart the faces are drawn from: `qa` an agent with a look, `ada` a person. */
function chart() {
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Nimbus",
      roles: [
        { name: "QA", handle: "qa", avatar: { character: "flasklet", color: "amber" } },
        { name: "Ada Lovelace", handle: "ada", kind: "human" },
      ],
    }),
  );
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  return { store, socket };
}

const seat = (handle: string, name: string): AssistantRef => ({
  kind: "seat",
  id: handle,
  name,
  open: { label: name, screen: "seat", href: `#/company/org/seats/${handle}` },
});

const deck: AssistantRef = {
  kind: "artifact",
  id: "d6333845-123a-57ea-9083-a60f2c1d34da",
  name: "Deck link previews for the spring launch",
  artifact_kind: "slides",
  open: {
    label: "Deck link previews for the spring launch",
    screen: "artifact",
    href: "#/artifacts/d6333845-123a-57ea-9083-a60f2c1d34da",
  },
};

const named = (ref: AssistantRef): AssistantPhrase => [{ ref }];

/** A share card as the engine sends it, after Apply. */
const shared: Partial<AssistantProposalCard> = {
  tool: "share_artifact",
  title: "Share artifact",
  state: "applied",
  changes: [
    { kind: "fact", field: "artifact", label: "Artifact", to: named(deck) },
    {
      kind: "added",
      field: "audience",
      label: "Shared with",
      to: [{ ref: seat("qa", "QA") }, { text: " as commenter" }],
    },
  ],
  audience: [named(seat("qa", "QA")), named(seat("ada", "Ada Lovelace"))],
  note: [{ ref: seat("qa", "QA") }, { text: " can open it." }],
  receipt: {
    summary: [
      { text: "Applied to " },
      {
        ref: {
          kind: "task",
          id: "ENG-12",
          name: "ENG-12",
          open: { label: "ENG-12", screen: "item", href: "#/worklet/ENG-12" },
        },
      },
    ],
  },
};

function draw(proposal: Partial<AssistantProposalCard>, opened: InAppTarget[] = []) {
  const on: CardHandlers = {
    approve: () => {},
    decline: () => {},
    prepareAgain: () => {},
    openHere: (target) => opened.push(target),
    ask: () => {},
    undo: () => {},
  };
  const event: AssistantCardEvent = {
    card_id: "c",
    message_id: "m",
    card: {
      type: "proposal",
      proposal: {
        proposal_id: "p",
        scope: "server",
        tool: "share_artifact",
        title: "Share artifact",
        changes: [],
        digest: `sha256:${"a".repeat(64)}`,
        expires_at: "2026-10-24T10:41:00Z",
        state: "proposed",
        approve_label: "Approve and share",
        ...proposal,
      },
    },
  };
  render(
    <ClientContext.Provider value={chart()}>
      <Router>
        <CardBlock event={event} local={undefined} on={on} opened={false} />
      </Router>
    </ClientContext.Provider>,
  );
  return opened;
}

/**
 * What a reference says, as a screen reader reads it: its words without its
 * mark. (jsdom's name computation drops the kit's kept spaces, so the name is
 * read off the text, as the kit's own suites read it.)
 */
function said(el: Element): string {
  const copy = el.cloneNode(true) as Element;
  for (const hidden of copy.querySelectorAll("[aria-hidden='true']")) hidden.remove();
  return copy.textContent ?? "";
}

/** The links whose words are `words`. */
function links(words: string): HTMLElement[] {
  return screen.getAllByRole("link").filter((link) => said(link) === words);
}

/** The one link whose words are `words`. */
function link(words: string): HTMLElement {
  const found = links(words);
  expect(found, words).toHaveLength(1);
  return found[0]!;
}

/** The row of the card's change list labelled `label`. */
function row(label: string): HTMLElement {
  const term = screen.getByText(label, { selector: "dt, th, [class*='label']" });
  return term.parentElement as HTMLElement;
}

// AN ARTIFACT IS ITS KIND'S GLYPH AND ITS TITLE, A LINK TO ITS PAGE. Mutation:
// draw the value as its words again and the row has no link and no glyph.
test("an artifact the card names is a link to its page with its kind's glyph", () => {
  draw(shared);
  const artifact = link("Slides: Deck link previews for the spring launch");
  expect(artifact.getAttribute("href")).toBe("#/artifacts/d6333845-123a-57ea-9083-a60f2c1d34da");
  const mark = artifact.querySelector(".crewlet-inline-ref__mark svg");
  const slides = document.createElement("div");
  slides.innerHTML = renderToStaticMarkup(<ArtifactKindGlyph kind="slides" />);
  expect(mark?.innerHTML).toBe(slides.querySelector("svg")?.innerHTML);
});

// A SEAT IS ITS FACE AND ITS NAME, wherever the card names it: who will see
// it, what changed and the sentence under the rows. Never its handle.
test("a seat the card names is its face and its name, never its handle", () => {
  draw(shared);
  const seats = links("Agent: QA");
  // The audience row, the Shared with row and the note.
  expect(seats).toHaveLength(3);
  for (const seat of seats) {
    expect(seat.getAttribute("href")).toBe("#/company/org/seats/qa");
    const face = seat.querySelector(".crewlet-inline-ref__mark--face .crewlet-avatar");
    expect(face?.classList.contains("crewlet-avatar--character")).toBe(true);
  }
  const words = said(document.body);
  expect(words).toContain("Agent: QA as commenter");
  expect(words).toContain("Agent: QA can open it.");
  expect(words).not.toMatch(/\bqa\b/);
  const person = link("Person: Ada Lovelace");
  expect(person.querySelector(".crewlet-avatar")?.classList.contains("crewlet-avatar--human")).toBe(
    true,
  );
});

// A PLAIN PRESS OPENS THE RECORD BESIDE THE CONVERSATION through the drawer's
// one way of moving the page; a press asking for a new tab is the browser's.
test("a record opens in place on a plain press, and in a tab on a modified one", () => {
  const opened = draw(shared);
  const artifact = link("Slides: Deck link previews for the spring launch");
  expect(fireEvent.click(artifact)).toBe(false);
  expect(opened).toEqual([
    { path: ["artifacts", "d6333845-123a-57ea-9083-a60f2c1d34da"], query: {} },
  ]);
  expect(fireEvent.click(artifact, { metaKey: true })).toBe(true);
  expect(opened).toHaveLength(1);
});

// A RECEIPT NAMES WHAT IT CHANGED AS THE RECORD, as the rows do.
test("a receipt names the task it changed as the task", () => {
  draw(shared);
  const task = link("Task: ENG-12");
  expect(task.getAttribute("href")).toBe("#/worklet/ENG-12");
  expect(task.closest("[class*='receipt'], [role='status']")).not.toBeNull();
});

// AN ADDRESS THAT IS NOT ONE OF THIS DASHBOARD'S OWN IS NEVER FOLLOWED: the
// record is its mark and its words.
test("a record whose address is not this dashboard's own is words, never a link", () => {
  const elsewhere: AssistantRef = {
    ...seat("qa", "QA"),
    open: { label: "QA", screen: "seat", href: "https://example.com/qa" },
  };
  draw({ ...shared, receipt: undefined, note: named(elsewhere), audience: undefined });
  // The Shared with row's seat links; the note's, whose address is elsewhere, does not.
  expect(links("Agent: QA")).toHaveLength(1);
  const refs = [...document.querySelectorAll(".crewlet-inline-ref")].filter(
    (ref) => said(ref) === "Agent: QA",
  );
  expect(refs).toHaveLength(2);
  expect(refs.filter((ref) => ref.tagName !== "A")).toHaveLength(1);
});

// A LIST VALUE IS A LIST OF RECORDS, each drawn as itself.
test("a list of records is one record a line", () => {
  draw({
    ...shared,
    receipt: undefined,
    changes: [
      {
        kind: "added",
        field: "members",
        label: "Members",
        to: [{ ref: seat("qa", "QA") }, { text: ", " }, { ref: seat("ada", "Ada Lovelace") }],
        items: [named(seat("qa", "QA")), named(seat("ada", "Ada Lovelace"))],
      },
    ],
  });
  const items = within(row("Members")).getAllByRole("listitem");
  expect(items.map(said)).toEqual(["Agent: QA", "Person: Ada Lovelace"]);
});
