/**
 * The kind lens has three answers, and all three are reachable.
 *
 * The screen's own comment says why there are three rather than a checkbox:
 * auditing the tool-skill catalogue, browsing everything else, and seeing the
 * lot are three different questions and a two-valued control can only ask two
 * of them. What the comment could not say is that the THIRD one has to be
 * spelled with a word.
 *
 * A filter set to the empty string is DELETED from the URL by the router, and
 * a deleted parameter reads back as this screen's own fallback. Spelled `""`,
 * choosing All therefore put the reader straight back on Pages, with the
 * address bar agreeing with them, which is exactly the state the three values
 * exist to avoid. Nothing else in the tree would notice that: the control
 * still draws three options and the query still goes out, it simply goes out
 * narrowed. So the sentinel is held here, at the wire, rather than left as a
 * string literal for the next reader to tidy back to nothing.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Pages } from "./Pages.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

/** Every `pages` question this screen asked, in order, as the engine saw it. */
function mount(): Record<string, unknown>[] {
  location.hash = "#/pages";
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    if (what === "pages") {
      asked.push(params ?? {});
      return Promise.resolve({ pages: [], limit: 50, offset: 0 });
    }
    if (what === "containers") return Promise.resolve({ containers: [] });
    return Promise.resolve(null);
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Pages />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

/** What the link would carry, read the way a reader would send it. */
function kindInUrl(): string | null {
  const at = location.hash.indexOf("?");
  return at < 0 ? null : new URLSearchParams(location.hash.slice(at + 1)).get("kind");
}

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("each of the three kinds asks the engine a different question", async () => {
  const asked = mount();
  await screen.findByRole("heading", { name: "Pages" });

  // The browse the screen opens on: everything but the tool-skill pages, and
  // no `kind` on the link, because this is the default.
  await waitFor(() => expect(asked.at(-1)).toEqual({ skills: false }));
  expect(kindInUrl()).toBeNull();

  fireEvent.click(screen.getByRole("radio", { name: "Tool skills" }));
  await waitFor(() => expect(asked.at(-1)).toEqual({ skills: true }));
  expect(kindInUrl()).toBe("skills");

  // THE THIRD STATE, which is the one that has to survive a round trip
  // through the URL: no `skills` on the wire at all, and a parameter the
  // router keeps.
  fireEvent.click(screen.getByRole("radio", { name: "All" }));
  await waitFor(() => expect(asked.at(-1)).toEqual({}));
  expect(kindInUrl()).toBe("all");
  expect(screen.getByRole("radio", { name: "All" })).toHaveProperty("ariaChecked", "true");
});
