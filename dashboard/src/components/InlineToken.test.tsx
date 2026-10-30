/**
 * Inline tokens as every Markdown surface draws them: a swatch beside a
 * colour, a vendor's mark beside a link to that vendor, a repository linked
 * to its code host, a key with the task glyph and a handle as its words,
 * each linked to what it names.
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
import { TokenScope } from "~/components/InlineToken.tsx";
import {
  ASSISTANT_WORDS,
  CHECKED_WORDS,
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
  {
    hosts = [{ kind: "github", url: "https://github.com" }] as LinkHost[],
    projects = ["GP"],
    viewer = VIEWER as Record<string, unknown>,
    rooms = [] as { id: string; name: string; kind: string }[],
    frames = [] as Record<string, unknown>[],
    asked = [] as string[],
    services = undefined as Record<string, string> | undefined,
  } = {},
) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyOrg(withDerived(services ? { ...ORG, services } : ORG) as never);
  for (const frame of frames) store.applyChatChange(frame as never);
  const socket = new LiveSocket(store);
  const answers: Record<string, unknown> = {
    viewer,
    // The frame's own readings, answered empty.
    work_inbox: { handle: "miles", notices: [], primary_reasons: [], unread: 0, primary: 0 },
    link_hosts: { hosts },
    work_projects: {
      projects: projects.map((key) => ({ key, name: key })),
      total: projects.length,
    },
    chat_room_names: {
      read_level: "stale",
      complete: true,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
      rooms,
    },
  };
  socket.query = ((what: string) => {
    asked.push(what);
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
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
    for (const options of [{}, ASSISTANT_WORDS, CHECKED_WORDS]) {
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
    expect(el.textContent).toContain("D page 2 (evil.atlassian.net)");
    expect(el.textContent).not.toContain("Sign in to GitHub");
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
    const el = await draw(md("PR gophrase/backend#14 at b5362d2."), { hosts: GOPHRASE });
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

  test("a key links to its work item with the task glyph only when its project is the company's", async () => {
    const el = await draw(md("GP-66 follows ISO-8601 and UTF-8."));
    const links = [...el.querySelectorAll("a")];
    expect(links.map((a) => a.getAttribute("href"))).toEqual(["#/worklet/GP-66"]);
    expect(links[0]!.querySelector(".crewlet-inline-ref__mark svg")).not.toBeNull();
    expect(links[0]!.textContent).toBe("Task: GP-66");
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

// LINKS IN WORDS AN AGENT OR A MODEL WROTE: the assistant's answers read
// `checked` (ASSISTANT_WORDS), and so do the Inbox and a coding run's question
// (CHECKED_WORDS), and the two are one rule. A link is live only where the
// dashboard built it, from checked parts, to the company's own place; any
// other address is its words, readable and copyable, and no control.
describe.each([
  ["the assistant", ASSISTANT_WORDS],
  ["the Inbox", CHECKED_WORDS],
])("checked links, in %s", (_, options) => {
  const COMPANY: LinkHost[] = [
    { kind: "github", url: "https://github.com", owners: ["gophrase"] },
    { kind: "jira", url: "https://gophrase.atlassian.net" },
    { kind: "mattermost", url: "https://chat.example.com" },
  ];
  const hrefs = (el: Element) => [...el.querySelectorAll("a")].map((a) => a.getAttribute("href"));

  test("a key, a handle, a reference and a repository link where they name", async () => {
    const el = await draw(
      md("GP-66 for @backend-engineer: gophrase/backend#14 in gophrase/web.", options),
      { hosts: COMPANY },
    );
    expect(hrefs(el)).toEqual([
      "#/worklet/GP-66",
      "#/company/org/seats/backend-engineer",
      "https://github.com/gophrase/backend/issues/14",
      "https://github.com/gophrase/web",
    ]);
  });

  test("an address on a host the company configured opens as rebuilt, with nothing it carried", async () => {
    const el = await draw(
      md(
        "https://github.com/gophrase/backend/pull/14?ref=x#discussion_r1 and " +
          "https://gophrase.atlassian.net/browse/GP-1?atl=1 and " +
          "[the room](https://chat.example.com/team/channels/design-review?leak=1)",
        options,
      ),
      { hosts: COMPANY },
    );
    expect(hrefs(el)).toEqual([
      "https://github.com/gophrase/backend/pull/14",
      "https://gophrase.atlassian.net/browse/GP-1",
      "https://chat.example.com/team/channels/design-review",
    ]);
  });

  test("a Confluence page's own canonical address opens where links are checked", async () => {
    const el = await draw(
      md("See https://acme.atlassian.net/wiki/spaces/ENG/pages/123.", options),
      {
        hosts: [{ kind: "confluence", url: "https://acme.atlassian.net/wiki" }],
      },
    );
    expect(hrefs(el)).toEqual(["https://acme.atlassian.net/wiki/spaces/ENG/pages/123"]);
  });

  // Mutation: let `checked` open an address as written and every case here
  // grows an anchor.
  test.each([
    ["an address that would carry what it read", "https://evil.example.net/c?q=the%20secret"],
    ["a look-alike host", "https://github.com.login.example.net/gophrase/backend/pull/14"],
    ["a credential before the host", "https://github.com@login.example.net/gophrase/backend"],
    ["a tenant anybody can register", "https://evil.slack.com/archives/C1/p1"],
    ["a code host the company did not configure", "https://gitlab.com/g/app/-/issues/3"],
  ])("%s is its words and no link", async (__, address) => {
    const el = await draw(md(`Open ${address} now.`, options), { hosts: COMPANY });
    expect(el.querySelector("a")).toBeNull();
    // WHOLE, AS WRITTEN: what a reader would copy is where it would go.
    expect(el.textContent).toContain(address);
  });

  test("a labelled link that is not the company's is its label and where it names, no link", async () => {
    const el = await draw(
      md("[the report](https://evil.example.net/r?q=1) and [this task](#/settings/iam)", options),
      { hosts: COMPANY },
    );
    expect(el.querySelector("a")).toBeNull();
    expect(el.textContent).toContain("the report (evil.example.net/r)");
    expect(el.textContent).toContain("this task");
  });
});

// A LINK IS BUILT ONLY TO THE COMPANY'S OWN OWNERS where the code host names
// them: an owner and a name are the writer's words, and a writer that read a
// secret could spell it into a repository's name.
//
// Mutation: drop the owner check in referenceTarget or describeGitHub and
// each of these grows an anchor.
describe("somebody else's repository", () => {
  test.each([
    ["a reference", "attacker/exfil-SECRET#1"],
    ["a bound number", "#7 in evil/repo-X"],
    ["a commit after one", "attacker/x#1 at b5362d2"],
  ])("%s under an owner the company does not name is its mark and no link", async (_, text) => {
    for (const options of [{}, CHECKED_WORDS]) {
      const el = await draw(md(`See ${text}.`, options), { hosts: GOPHRASE });
      expect(el.querySelector("a")).toBeNull();
      expect(el.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
      cleanup();
    }
  });

  test("a host that names no owners links no reference and opens no address where links are checked", async () => {
    const bare: LinkHost[] = [{ kind: "github", url: "https://github.com" }];
    const el = await draw(
      md("See gophrase/backend#14 and https://github.com/gophrase/backend/pull/14.", CHECKED_WORDS),
      { hosts: bare },
    );
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
  });

  test("a longer path than owner/repo is no GitHub reference", async () => {
    const el = await draw(md("See a/b/c/d/SECRET#2."), { hosts: GOPHRASE });
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")).toBeNull();
  });

  test("an address to it on the configured host opens nothing where links are checked", async () => {
    const el = await draw(md("See https://github.com/attacker/x now.", CHECKED_WORDS), {
      hosts: GOPHRASE,
    });
    expect(el.querySelector("a")).toBeNull();
    expect(el.textContent).toContain("https://github.com/attacker/x");
  });

  test("the company's own owner still links", async () => {
    const el = await draw(
      md("See gophrase/backend#1 and https://github.com/gophrase/x.", CHECKED_WORDS),
      {
        hosts: GOPHRASE,
      },
    );
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend/issues/1",
      "https://github.com/gophrase/x",
    ]);
  });
});

// A ROOM IS NAMED IN PROSE AS `#name` (rule 1a), and a room's name may be
// spelled as a colour or an issue's number: only the company's rooms, as the
// frame reads them for this reader, say which the words mean.
describe("rooms", () => {
  const CHATTER = { ...VIEWER, grants: [...VIEWER.grants, "chatlet"] };
  const ROOMS = [
    { id: "room-e2e", name: "e2e", kind: "public" },
    { id: "room-2026", name: "2026", kind: "private" },
  ];

  test("a room the reader may read links to it, though its name is spelled as a colour or a number", async () => {
    const el = await draw(
      md("Ask in #e2e, not #2026, about gophrase/backend; ship #ffe100.", ASSISTANT_WORDS),
      { hosts: GOPHRASE, viewer: CHATTER, rooms: ROOMS },
    );
    const links = [...el.querySelectorAll("a")];
    expect(links.map((a) => a.getAttribute("href"))).toEqual([
      "#/chatlet/room-e2e",
      "#/chatlet/room-2026",
      "https://github.com/gophrase/backend",
    ]);
    // ITS NAME IS ITS WORDS, what it is read before them, and its mark drawn.
    expect(links[0]!.textContent).toBe("Room: #e2e");
    expect(links[0]!.querySelector(".crewlet-inline-ref__mark svg")).not.toBeNull();
    const chips = [...el.querySelectorAll<HTMLElement>(".crewlet-color-chip")];
    expect(chips.map((c) => c.style.getPropertyValue("--crewlet-color-chip-value"))).toEqual([
      "#ffe100",
    ]);
  });

  test("a code span holding a room's name is code, and one holding a colour a swatch", async () => {
    const el = await draw(md("`#e2e` is a room and `#e2f` a colour."), {
      viewer: CHATTER,
      rooms: ROOMS,
    });
    const chips = [...el.querySelectorAll<HTMLElement>(".crewlet-color-chip")];
    expect(chips.map((c) => c.style.getPropertyValue("--crewlet-color-chip-value"))).toEqual([
      "#ee22ff",
    ]);
    expect(el.textContent).toContain("#e2e is a room");
    expect(el.querySelector("a")).toBeNull();
  });

  // Mutation: read the names for a reader who holds no Chatlet, or drop the
  // gate, and the reader below is asked for rooms and draws a link.
  test("for a reader who holds no Chatlet, no room is asked for and the words read by their shape", async () => {
    const asked: string[] = [];
    const el = await draw(md("Ask in #e2e, not #2026."), { rooms: ROOMS, asked });
    expect(asked).not.toContain("chat_room_names");
    expect(el.querySelector("a")).toBeNull();
    expect(swatch(el)).toBe("#ee22ee");
  });

  // Mutation: ask the names whatever chat the company runs, and the vendor's
  // company below is asked for rooms it has none of.
  test("on a company whose chat is a vendor's, no room is asked for and none is drawn", async () => {
    const asked: string[] = [];
    const el = await draw(md("Ask in #e2e."), {
      viewer: CHATTER,
      rooms: ROOMS,
      asked,
      services: { tracker: "worklet", knowledge: "doclet", chat: "vendor" },
    });
    expect(asked).not.toContain("chat_room_names");
    expect(el.querySelector("a")).toBeNull();
    expect(swatch(el)).toBe("#ee22ee");
  });

  test("a room this tab has seen a frame for is a room before the names read holds it", async () => {
    const el = await draw(md("Post it in #b2b."), {
      viewer: CHATTER,
      frames: [
        {
          channel_id: "room-b2b",
          channel_name: "b2b",
          channel_kind: "public",
          op: "post",
          op_id: "op-1",
          message_id: "m-1",
          channel_seq: 1,
          at: "2026-01-01T00:01:00Z",
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 5 },
        },
      ],
    });
    expect(el.querySelector("a")?.getAttribute("href")).toBe("#/chatlet/room-b2b");
  });

  test("after a caller's chip for a room, its later mention is the room's token", async () => {
    const el = await draw(
      md("#e2e is waiting; ask in #e2e again.", {
        ...ASSISTANT_WORDS,
        prose: (text, key) =>
          text.startsWith("#e2e")
            ? [
                <b key={key} data-chip="">
                  #e2e
                </b>,
                text.slice(4),
              ]
            : text,
      }),
      { viewer: CHATTER, rooms: ROOMS },
    );
    expect(el.querySelector("[data-chip]")?.textContent).toBe("#e2e");
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "#/chatlet/room-e2e",
    ]);
    expect(swatch(el)).toBeNull();
  });

  test("inside a control a room is its mark and its words, never a link", async () => {
    const el = await draw(<button type="button">{renderTokens("Post in #e2e")}</button>, {
      viewer: CHATTER,
      rooms: ROOMS,
    });
    expect(el.querySelector("a")).toBeNull();
    expect(el.textContent).toBe("Post in Room: #e2e");
    expect(swatch(el)).toBeNull();
  });
});

// A BARE NUMBER IS PLACED BY ITS MESSAGE (rule 3b), and an ask is one message
// across its question, context and options.
describe("bare numbers", () => {
  test("a number the sentence binds is one reference to that repository", async () => {
    const el = await draw(md("See pull request #7 in gophrase/frontend, or gophrase/web PR #9."), {
      hosts: GOPHRASE,
    });
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/frontend/issues/7",
      "https://github.com/gophrase/web/issues/9",
    ]);
  });

  test("a number the message names in full, or its one repository, places it", async () => {
    const el = await draw(
      md("The review on gophrase/backend#11 asks for changes.\n\nStart after #11 and #12."),
      { hosts: GOPHRASE },
    );
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend/issues/11",
      "https://github.com/gophrase/backend/issues/11",
      "https://github.com/gophrase/backend/issues/12",
    ]);
  });

  test("a message naming two repositories places no bare number", async () => {
    const el = await draw(md("gophrase/backend and gophrase/web, then #11."), {
      hosts: GOPHRASE,
    });
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend",
      "https://github.com/gophrase/web",
    ]);
  });

  test("a scope spans the renders of one message: a title placed by its context", async () => {
    const el = await draw(
      <TokenScope texts={["Apply the #11 PR body?", "Assigned to you in gophrase/backend."]}>
        <h1>{renderTokens("Apply the #11 PR body?", "q", "text", true)}</h1>
        {md("Assigned to you in gophrase/backend.", { scoped: true })}
      </TokenScope>,
      { hosts: GOPHRASE },
    );
    // THE TITLE IS NO CONTROL: its reference carries the mark and no link.
    expect(el.querySelector("h1 [data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    expect(el.querySelector("h1 a")).toBeNull();
    expect(el.querySelector("h1")?.textContent).toContain("#11");
  });

  // A REPLY UNDER AN ASK IS ITS OWN MESSAGE: a render that does not say it is
  // part of the scope around it reads its own words, so the ask's repository
  // never places the reply's numbers.
  //
  // Mutation: let a run read the enclosing scope whatever its render says,
  // and the reply's #11 links to the ask's repository.
  test("a render under a scope that is not part of it is its own message", async () => {
    const el = await draw(
      <TokenScope texts={["Apply the #11 PR body?", "Assigned to you in gophrase/backend."]}>
        <section data-reply="plain">{md("Done with #11.")}</section>
        <section data-reply="own">{md("Merged in gophrase/web, see #5.")}</section>
      </TokenScope>,
      { hosts: GOPHRASE },
    );
    expect(el.querySelector("[data-reply=plain] [data-vendor]")).toBeNull();
    expect(
      [...el.querySelectorAll("[data-reply=own] a")].map((a) => a.getAttribute("href")),
    ).toEqual(["https://github.com/gophrase/web", "https://github.com/gophrase/web/issues/5"]);
  });

  test("with nothing to place it, a number is a word", async () => {
    const el = await draw(md("Our #1 priority is #11."), { hosts: GOPHRASE });
    expect(el.querySelector("a")).toBeNull();
    expect(el.querySelector("[data-vendor]")).toBeNull();
  });

  test("a number in code is code", async () => {
    const el = await draw(md("gophrase/backend: `#11` stays."), { hosts: GOPHRASE });
    expect([...el.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend",
    ]);
  });
});
