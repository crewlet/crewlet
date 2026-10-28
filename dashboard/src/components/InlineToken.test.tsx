/**
 * Inline tokens as every Markdown surface draws them: a swatch beside a
 * colour, a vendor's mark beside a link to that vendor, a repository linked
 * to its code host, a key and a handle linked to what they name as words
 * alone.
 *
 * What a token becomes is decided by the company's own records, never by the
 * words: a link labelled "github.com" that goes elsewhere carries no GitHub
 * mark, a key whose project the company does not have is a word, a reference
 * links only to a code host the company configured, and nothing in a model's
 * prose or inside a control becomes a link at all.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import type { ReactNode } from "react";
import { FrameReadings } from "~/app/Shell.tsx";
import {
  ASSISTANT_WORDS,
  renderMarkdown,
  renderTokens,
  type RenderOptions,
} from "~/lib/markdown.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { InertWebSocket } from "~/test/client.tsx";
import { withDerived } from "~/test/org.ts";
import type { LinkHost } from "~/contract/integrations.ts";

afterEach(cleanup);

const ORG = {
  name: "GoPhrase",
  timezone: "UTC",
  roles: [
    { name: "Miles", handle: "miles", kind: "human" },
    { name: "Backend Engineer", handle: "backend-engineer", kind: "agent" },
  ],
  units: [],
};

const VIEWER = {
  login: "U0MILES",
  grants: ["worklet", "doclet"],
  handle: "miles",
  owner: "miles",
  name: "Miles",
  kind: "human",
  acts: [],
};

/** Drawn as the frame draws it: inside the client and the frame's readings. */
async function draw(
  node: ReactNode,
  { hosts = [{ kind: "github", url: "https://github.com" }] as LinkHost[], projects = ["GP"] } = {},
) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const answers: Record<string, unknown> = {
    viewer: VIEWER,
    // The frame's own readings, answered empty.
    work_inbox: { handle: "miles", notices: [], primary_reasons: [], unread: 0, primary: 0 },
    link_hosts: { hosts },
    work_projects: {
      projects: projects.map((key) => ({ key, name: key })),
      total: projects.length,
    },
  };
  socket.query = ((what: string) => Promise.resolve(answers[what] ?? {})) as typeof socket.query;
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <FrameReadings>
        <div>{node}</div>
      </FrameReadings>
    </ClientContext.Provider>,
  );
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
  return view.container;
}

const md = (source: string, options: RenderOptions = {}) => renderMarkdown(source, options);

/** GitHub with the company's organization, as `link_hosts` answers it. */
const GOPHRASE: LinkHost[] = [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }];

/** The colour a swatch is drawn in, or null. */
function swatch(el: Element | null): string | null {
  const chip = el?.querySelector<HTMLElement>(".crewlet-color-chip");
  return chip?.style.getPropertyValue("--crewlet-color-chip-value") || null;
}

describe("colours", () => {
  test("a colour in prose and a code span that is one colour are drawn with a swatch of it", async () => {
    const el = await draw(md("Ship #ffe100 over `#0B0B0F`, not `color: #fff;` or #123."));
    const chips = [...el.querySelectorAll<HTMLElement>(".crewlet-color-chip")].map((c) =>
      c.style.getPropertyValue("--crewlet-color-chip-value"),
    );
    expect(chips).toEqual(["#ffe100", "#0b0b0f"]);
    // THE WORDS STAY: the value is read, never a colour alone.
    expect(el.textContent).toContain("#ffe100");
    expect(el.textContent).toContain("#0B0B0F");
    // The chip is decoration; the value beside it is what is read.
    expect(el.querySelector(".crewlet-color-chip")?.getAttribute("aria-hidden")).toBe("true");
  });
});

describe("addresses and links", () => {
  test("a bare address carries its app's mark, says the reference it is and links where it goes", async () => {
    const el = await draw(md("See https://github.com/gophrase/backend/pull/14 now."));
    const link = el.querySelector("a")!;
    expect(link.getAttribute("href")).toBe("https://github.com/gophrase/backend/pull/14");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    // ITS NAME IS ITS WORDS, the app read before them and the tab after:
    // no aria-label replaces what a voice user sees (WCAG 2.5.3).
    expect(link.getAttribute("aria-label")).toBeNull();
    expect(link.textContent).toBe("GitHub: gophrase/backend#14 (opens in a new tab)");
    expect(link.getAttribute("title")).toBe("https://github.com/gophrase/backend/pull/14");
    expect(link.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    expect(el.textContent).toContain("gophrase/backend#14");
  });

  test("a labelled link carries the mark of where it goes, never of what its words say", async () => {
    const el = await draw(
      md(
        "[github.com/gophrase/backend](https://login.example.net/github) and [the spec](https://github.com/gophrase/web/pull/31)",
      ),
    );
    const [spoof, real] = [...el.querySelectorAll("a")];
    expect(spoof!.getAttribute("href")).toBe("https://login.example.net/github");
    expect(spoof!.querySelector("[data-vendor]")).toBeNull();
    expect(real!.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    expect(real!.textContent).toContain("the spec");
  });

  test("a look-alike host earns no vendor's mark", async () => {
    const el = await draw(md("https://github.com.login.example.net/gophrase/backend/pull/14"));
    expect(el.querySelector("[data-vendor]")).toBeNull();
    expect(el.textContent).toContain("github.com.login.example.net");
  });

  // A LABEL IS NEVER READ AS A TOKEN: only where a link goes says what it is.
  // Mutation: tokenise a label again and each case draws GitHub's mark on a
  // link to another host.
  test.each([
    ["an address", "[https://github.com/gophrase/backend/pull/14](https://evil.example.net/x)"],
    ["a reference", "[gophrase/backend#14](https://evil.example.net/x)"],
    ["a repository", "[gophrase/backend](https://evil.example.net/x)"],
  ])("a label that spells %s draws no mark on a link that goes elsewhere", async (_, source) => {
    for (const options of [{}, ASSISTANT_WORDS]) {
      const el = await draw(md(source, options), { hosts: GOPHRASE });
      expect(el.querySelector("[data-vendor]")).toBeNull();
      expect(el.textContent).not.toContain("GitHub");
      // THE WORDS AS WRITTEN, never the reference a token would have said.
      expect(el.textContent).toContain(source.slice(1, source.indexOf("]")));
      cleanup();
    }
  });

  test("an address on a tenant anybody can register says its host after its label", async () => {
    const el = await draw(
      md(
        "https://evil.atlassian.net/wiki/spaces/D/pages/2/Sign+in+to+GitHub and https://evil.atlassian.net/browse/GP-1",
      ),
    );
    expect(el.textContent).toContain("Sign in to GitHub (evil.atlassian.net)");
    expect(el.textContent).toContain("GP-1 (evil.atlassian.net)");
  });

  test("an autolink is an address like a bare one", async () => {
    const el = await draw(md("<https://acme.atlassian.net/browse/GP-1>"));
    expect(el.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("atlassian");
    expect(el.querySelector("a")?.getAttribute("href")).toBe(
      "https://acme.atlassian.net/browse/GP-1",
    );
  });
});

describe("references, keys and seats", () => {
  test("a bare reference links to the company's code host, and its commit after it too", async () => {
    const el = await draw(md("PR gophrase/backend#14 at b5362d2."));
    const hrefs = [...el.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs).toEqual([
      "https://github.com/gophrase/backend/issues/14",
      "https://github.com/gophrase/backend/commit/b5362d2",
    ]);
  });

  test("with no code host a reference is its mark and its words, linked nowhere", async () => {
    const el = await draw(md("PR gophrase/backend#14."), { hosts: [] });
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
  });

  test("a bare repository under one of the company's owners links to its code host", async () => {
    const el = await draw(md("Assigned to you in gophrase/backend, and/or src/app or 2024/10."), {
      hosts: GOPHRASE,
    });
    const links = [...el.querySelectorAll("a")];
    expect(links.map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend",
    ]);
    expect(links[0]!.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    expect(el.textContent).toContain("and/or src/app or 2024/10");
  });

  test("a repository quoted in code is code", async () => {
    const el = await draw(md("Clone `gophrase/backend` first."), { hosts: GOPHRASE });
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")).toBeNull();
  });

  test("with no owners a bare owner/repo is words", async () => {
    const el = await draw(md("Assigned to you in gophrase/backend."));
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")).toBeNull();
  });

  test("with two code hosts a reference goes to the host whose owners hold it", async () => {
    const el = await draw(md("PR gophrase/backend#14 and platform/api#3."), {
      hosts: [
        { kind: "github", url: "https://github.com", owners: ["gophrase"] },
        { kind: "gitlab", url: "https://gitlab.example.com", owners: ["platform"] },
      ],
    });
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend/issues/14",
      "https://gitlab.example.com/platform/api/-/issues/3",
    ]);
  });

  test("with two code hosts a reference either could own is words", async () => {
    const el = await draw(md("PR gophrase/backend#14."), {
      hosts: [
        { kind: "github", url: "https://github.com" },
        { kind: "gitlab", url: "https://gitlab.example.com" },
      ],
    });
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")).toBeNull();
  });

  test("a key links to its work item only when its project is the company's, as its words alone", async () => {
    const el = await draw(md("GP-66 follows ISO-8601 and UTF-8."));
    const links = [...el.querySelectorAll("a")];
    expect(links.map((a) => a.getAttribute("href"))).toEqual(["#/worklet/GP-66"]);
    // NO GLYPH and nothing said before it: the key is what is read.
    expect(links[0]!.querySelector(".crewlet-inline-ref__mark")).toBeNull();
    expect(links[0]!.textContent).toBe("GP-66");
    expect(el.textContent).toContain("ISO-8601 and UTF-8");
  });

  test("a handle links to its seat as its words alone, and a handle the chart lacks is words", async () => {
    const el = await draw(md("@backend-engineer asked @nobody."));
    const links = [...el.querySelectorAll("a")];
    expect(links).toHaveLength(1);
    expect(links[0]!.getAttribute("href")).toBe("#/company/org/seats/backend-engineer");
    expect(links[0]!.getAttribute("title")).toBe("Backend Engineer");
    // NO FACE and nothing said before it.
    expect(links[0]!.querySelector(".crewlet-inline-ref__mark")).toBeNull();
    expect(links[0]!.textContent).toBe("@backend-engineer");
    expect(el.textContent).toContain("@nobody");
  });
});

describe("where nothing may be a link", () => {
  test("a model's prose draws every token with its mark and none of them as a link", async () => {
    const el = await draw(
      md(
        "GP-66 is gophrase/backend#14, see https://github.com/gophrase/backend/pull/14 @miles",
        ASSISTANT_WORDS,
      ),
    );
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelectorAll("[data-vendor]").length).toBeGreaterThanOrEqual(2);
  });

  test("words inside a control carry their marks and swatches and never a link", async () => {
    const el = await draw(
      <button type="button">{renderTokens("Ship #ffe100 for gophrase/backend#14")}</button>,
    );
    expect(el.querySelector("a")).toBeNull();
    expect(swatch(el)).toBe("#ffe100");
    expect(el.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
  });

  test("a link's own label never holds a second link", async () => {
    const el = await draw(md("[see GP-66 and https://example.com/a](https://example.com/oncall)"));
    expect(el.querySelectorAll("a")).toHaveLength(1);
  });
});

describe("reading order", () => {
  test("a caller's prose reads first, and a token is read only in the words it left", async () => {
    const seen: string[] = [];
    const el = await draw(
      md("GP-66 then GP-66", {
        prose: (text, key) => {
          seen.push(text);
          const at = text.indexOf("GP-66");
          return at < 0
            ? text
            : [
                text.slice(0, at),
                <b key={key} data-chip="">
                  GP-66
                </b>,
                text.slice(at + 5),
              ];
        },
      }),
    );
    expect(seen).toEqual(["GP-66 then GP-66"]);
    expect(el.querySelector("[data-chip]")?.textContent).toBe("GP-66");
    // The second is the key's link, read in what the chip left.
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "#/worklet/GP-66",
    ]);
  });

  test("a search's marks fall in the words around a token, never inside it", async () => {
    const el = await draw(md("backend ships gophrase/backend#14", { highlight: ["backend"] }));
    const marks = [...el.querySelectorAll("mark")].map((m) => m.textContent);
    expect(marks).toEqual(["backend"]);
  });
});
