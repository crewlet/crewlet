/**
 * The inline tokens: what operator-facing prose names, read out of its words.
 *
 * Its failure modes are the two directions of a guess. Too eager, and an
 * issue number grows a swatch, a hashtag turns yellow, a hex word becomes a
 * commit and a key inside a URL becomes a second link. Too trusting, and a
 * link whose words say "github.com" is drawn as GitHub wherever it goes, or a
 * look-alike host earns a vendor's mark. Every case below is one of those.
 */

import { describe, expect, test } from "vitest";
import type { LinkHost } from "~/contract/integrations.ts";
import {
  codeColour,
  codeHostOwners,
  describeLink,
  inlineTokens,
  messageScope,
  referenceTarget,
  repositoryTarget,
} from "./inlineTokens.ts";

/** What a run's tokens are, as `kind:text`, read under `owners`. */
function said(text: string, owners: readonly string[] = []): string[] {
  return inlineTokens(text, new Set(owners)).map((t) => `${t.kind}:${t.text}`);
}

const GITHUB: LinkHost = { kind: "github", url: "https://github.com", owners: ["gophrase"] };
const GHE: LinkHost = { kind: "github", url: "https://github.example.com" };
const GITLAB: LinkHost = { kind: "gitlab", url: "https://code.example.com/scm", owners: ["g"] };
const MATTERMOST: LinkHost = { kind: "mattermost", url: "https://chat.example.com" };
const JIRA: LinkHost = { kind: "jira", url: "https://jira.example.com" };
const WIKI: LinkHost = { kind: "confluence", url: "https://wiki.example.com/confluence" };

describe("colours", () => {
  test("a colour in prose has a digit and a letter, so an issue and a hashtag are words", () => {
    expect(said("Ship #ffe100 over #0b0b0f and #3a7.")).toEqual([
      "colour:#ffe100",
      "colour:#0b0b0f",
      "colour:#3a7",
    ]);
    expect(said("See #123, #456789, #facade, #bad and #beaded.")).toEqual([]);
  });

  test("a colour is six or three digits at word boundaries, never alpha and never inside a word", () => {
    expect(said("#ffe10080 #ffe1 x#ffe100 #ffe100x &#123a;")).toEqual([]);
  });

  test("a colour's swatch takes the six-digit lowercase form", () => {
    expect(inlineTokens("#3A7")).toMatchObject([{ kind: "colour", hex: "#33aa77" }]);
  });

  test("a code span that IS a colour is one whatever its digits, and one that holds more is code", () => {
    expect(codeColour("#fff")).toBe("#ffffff");
    expect(codeColour(" #DEBE42 ")).toBe("#debe42");
    expect(codeColour("rgb(255, 225, 0)")).toBe("#ffe100");
    expect(codeColour("rgb(255 225 0)")).toBe("#ffe100");
    expect(codeColour("rgb(256, 0, 0)")).toBeNull();
    expect(codeColour("color: #fff;")).toBeNull();
    expect(codeColour("ffe100")).toBeNull();
    expect(codeColour("#ffe10080")).toBeNull();
  });
});

describe("addresses", () => {
  test("a bare web address ends where the sentence takes over", () => {
    expect(
      said("Report: https://grafana.example.com/d/api-latency. Then (https://x.test/a_(b))."),
    ).toEqual([
      "address:https://grafana.example.com/d/api-latency",
      "address:https://x.test/a_(b)",
    ]);
  });

  test("an address swallows every key, colour and reference spelled inside it", () => {
    expect(said("https://github.com/gophrase/backend/pull/14#issuecomment-GP-66 #ffe100")).toEqual([
      "address:https://github.com/gophrase/backend/pull/14#issuecomment-GP-66",
      "colour:#ffe100",
    ]);
  });

  test("a scheme no browser should follow is not an address", () => {
    expect(said("javascript:alert(1) ftp://files.example.com/x")).toEqual([]);
  });
});

describe("references and commits", () => {
  test("a reference names an issue, a merge request or a commit by its path", () => {
    expect(said("PR gophrase/backend#14, group/sub/app!7 and gophrase/backend@7f3c2a91.")).toEqual([
      "reference:gophrase/backend#14",
      "reference:group/sub/app!7",
      "reference:gophrase/backend@7f3c2a91",
    ]);
  });

  test("a reference is never the tail of a path or an address", () => {
    expect(said("/srv/gophrase/backend#14 ./a/b#2 x@a/b#3")).toEqual([]);
  });

  test("a commit is a SHA the prose calls one, after a reference or the word commit", () => {
    expect(said("gophrase/backend#14 at b5362d2, then at head b5362d22aa")).toEqual([
      "reference:gophrase/backend#14",
      "commit:b5362d2",
      "commit:b5362d22aa",
    ]);
    expect(inlineTokens("gophrase/backend#14 at b5362d2")[1]).toMatchObject({
      after: { path: "gophrase/backend", sep: "#" },
    });
    expect(said("Fixed in commit 9fceb02d0ae598e95dc970b74767f19372d61af8.")).toEqual([
      "commit:9fceb02d0ae598e95dc970b74767f19372d61af8",
    ]);
  });

  test("a hex word with no reference before it is a word", () => {
    expect(said("deadbeef1 at 1234567 at abcdefa head b5362d2 at B5362D2")).toEqual([]);
    // All digits, or all letters, is a number or a word even after a reference.
    expect(said("a/b#1 at 1234567 at deadbee")).toEqual(["reference:a/b#1"]);
  });
});

describe("keys and mentions", () => {
  test("a key stands alone, and a slash between two keys delimits them", () => {
    expect(said("GP-66, GP-10/GP-11 and (GP-7).")).toEqual([
      "key:GP-66",
      "key:GP-10",
      "key:GP-11",
      "key:GP-7",
    ]);
    expect(said("GP-660x aGP-1 GP-1-b gp-1 GP-1_x")).toEqual([]);
  });

  test("a key inside a path is part of the path", () => {
    expect(said("worklet/GP-11 /worklet/GP-12 ./GP-13")).toEqual([]);
  });

  test("a mention is a handle after @, and an email address is never one", () => {
    expect(said("@miles, ask @backend-engineer. ops@gophrase.example @Miles")).toEqual([
      "mention:@miles",
      "mention:@backend-engineer",
    ]);
  });
});

describe("what an address is", () => {
  test("a code host's pull request, issue, commit and repository read as their references", () => {
    expect(describeLink("https://github.com/gophrase/backend/pull/14/files", [])).toEqual({
      vendor: "github",
      label: "gophrase/backend#14",
      code: true,
      kind: "GitHub",
      // ON NO HOST OF THE COMPANY'S, nothing is rebuilt.
      canonical: null,
      place: { path: "gophrase/backend", number: "14" },
    });
    expect(describeLink("https://github.com/gophrase/backend/commit/b5362d22aa", [])?.label).toBe(
      "gophrase/backend@b5362d2",
    );
    expect(describeLink("https://gitlab.com/g/sub/app/-/merge_requests/7", [])).toMatchObject({
      vendor: "gitlab",
      label: "g/sub/app!7",
    });
    expect(describeLink("https://github.com/gophrase/backend", [])?.label).toBe("gophrase/backend");
  });

  test("a self-hosted app is the company's only where its own hosts say so, path prefix and all", () => {
    expect(describeLink("https://github.example.com/a/b/pull/2", [])?.vendor).toBeNull();
    expect(describeLink("https://github.example.com/a/b/pull/2", [GHE])?.label).toBe("a/b#2");
    expect(describeLink("https://code.example.com/scm/g/app/-/issues/3", [GITLAB])).toMatchObject({
      vendor: "gitlab",
      label: "g/app#3",
    });
    expect(describeLink("https://code.example.com/other/g/app", [GITLAB])?.vendor).toBeNull();
    expect(
      describeLink("https://chat.example.com/team/channels/design-review", [MATTERMOST]),
    ).toMatchObject({
      vendor: "mattermost",
      label: "~design-review",
    });
    expect(describeLink("https://jira.example.com/browse/MOB-12", [JIRA])).toMatchObject({
      vendor: "atlassian",
      label: "MOB-12",
      kind: "Jira",
    });
    expect(
      describeLink("https://wiki.example.com/confluence/spaces/DES/pages/1/Brand+tokens", [WIKI]),
    ).toMatchObject({ vendor: "atlassian", label: "DES page 1" });
  });

  test("Slack and Atlassian Cloud are theirs on their own domains", () => {
    expect(
      describeLink("https://gophrase.slack.com/archives/C07LQ2V4K/p1729950000123456", []),
    ).toMatchObject({
      vendor: "slack",
      label: "Message in gophrase",
    });
    expect(describeLink("https://gophrase.slack.com/archives/C07LQ2V4K", [])?.label).toBe(
      "Channel in gophrase",
    );
    // A TENANT ANYBODY CAN REGISTER says its host after the label; a site the
    // company configured is its own and its labels stand alone. A PAGE IS
    // LABELLED BY ITS SPACE AND ID, never by the title its address carries.
    expect(describeLink("https://acme.atlassian.net/browse/GP-1", [])).toMatchObject({
      label: "GP-1 (acme.atlassian.net)",
      code: false,
    });
    expect(
      describeLink("https://acme.atlassian.net/wiki/spaces/D/pages/2/Sign+in+to+GitHub", [])?.label,
    ).toBe("D page 2 (acme.atlassian.net)");
    const own: LinkHost[] = [
      { kind: "jira", url: "https://acme.atlassian.net" },
      { kind: "confluence", url: "https://acme.atlassian.net/wiki" },
    ];
    expect(describeLink("https://acme.atlassian.net/browse/GP-1", own)?.label).toBe("GP-1");
    expect(
      describeLink("https://acme.atlassian.net/wiki/spaces/D/pages/2/Brand+tokens", own)?.label,
    ).toBe("D page 2");
  });

  // A LABEL IS ONLY WHAT AN ADDRESS IS REBUILT FROM: a Confluence page's
  // title and a path segment no app allows are the writer's words, and a
  // label made of them says whatever the writer chose beside a vendor's mark.
  //
  // Mutation: label a page by its title, or a channel by any segment, and
  // these show the writer's words.
  test("no label carries words the rebuilt address drops", () => {
    const own: LinkHost[] = [
      { kind: "confluence", url: "https://acme.atlassian.net/wiki" },
      MATTERMOST,
    ];
    const page = describeLink(
      "https://acme.atlassian.net/wiki/spaces/D/pages/2/Sign+in+to+GitHub",
      own,
    );
    expect(page?.label).toBe("D page 2");
    expect(page?.label).not.toContain("GitHub");
    const channel = describeLink("https://chat.example.com/team/channels/Sign%20in%20to%20GitHub", [
      MATTERMOST,
    ]);
    expect(channel?.label).not.toContain("Sign in");
    expect(channel?.canonical).toBeNull();
    const post = describeLink("https://chat.example.com/Sign%20in/pl/abc123", [MATTERMOST]);
    expect(post?.label).not.toContain("Sign in");
    expect(post?.canonical).toBeNull();
  });

  // A CONFLUENCE PAGE IS REBUILT AS ITS SPACE AND ID ALONE: a segment before
  // `spaces` is the writer's, and so is the title after the id.
  //
  // Mutation: rebuild from the written path up to the id and the first two
  // carry the writer's segment to the company's host.
  test("a Confluence page on the company's site is rebuilt as spaces/<key>/pages/<id>", () => {
    const wiki: LinkHost[] = [{ kind: "confluence", url: "https://acme.atlassian.net/wiki" }];
    const site: LinkHost[] = [{ kind: "confluence", url: "https://acme.atlassian.net" }];
    const canonical = (raw: string, hosts: LinkHost[]) => describeLink(raw, hosts)?.canonical;
    expect(canonical("https://acme.atlassian.net/wiki/SECRET/spaces/D/pages/2/T", wiki)).toBeNull();
    expect(canonical("https://acme.atlassian.net/wiki/x/y/spaces/D/pages/2/T", site)).toBeNull();
    expect(canonical("https://acme.atlassian.net/wiki/spaces/D/pages/2/Title?x=1", wiki)).toBe(
      "https://acme.atlassian.net/wiki/spaces/D/pages/2",
    );
    expect(canonical("https://acme.atlassian.net/wiki/spaces/~me/pages/2/T", site)).toBe(
      "https://acme.atlassian.net/wiki/spaces/~me/pages/2",
    );
    expect(canonical("https://acme.atlassian.net/wiki/spaces/D/pages/abc/T", wiki)).toBeNull();
  });

  test("a segment that decodes to a slash, or that no code host allows, labels nothing", () => {
    // `a%2Fevil` decoded read as two segments, labelling another repository.
    const spoof = describeLink("https://github.com/a%2Fevil/b/pull/1", []);
    expect(spoof?.label).not.toContain("a/evil");
    expect(spoof).toMatchObject({ vendor: "github", code: false });
    expect(describeLink("https://github.com/a/b/commit/not-a-sha", [])?.label).toBe(
      "github.com/a/b/commit/not-a-sha",
    );
    expect(describeLink("https://gitlab.com/g%2Fx/app/-/issues/3", [])?.code).toBe(false);
  });

  test("a look-alike host, a credential before the host and a host by another port are nobody's", () => {
    for (const raw of [
      "https://github.com.login.example.net/gophrase/backend/pull/14",
      "https://github.com@login.example.net/gophrase/backend/pull/14",
      "https://login.example.net/github.com/gophrase/backend/pull/14",
      "https://slack.com.example.net/archives/C1/p1",
      "https://notatlassian.net/browse/GP-1",
      "https://github.example.com:8443/a/b/pull/2",
    ]) {
      expect(describeLink(raw, [GHE])?.vendor, raw).toBeNull();
    }
  });

  test("an address is shown as its host and path, cut, and anything that is no web address is nothing", () => {
    expect(describeLink("https://grafana.example.com/d/api-latency?orgId=1", [])).toEqual({
      vendor: null,
      label: "grafana.example.com/d/api-latency",
      code: false,
      kind: null,
      canonical: null,
      place: null,
    });
    expect(describeLink(`https://example.com/${"a".repeat(80)}`, [])?.label).toHaveLength(48);
    expect(describeLink("javascript:alert(1)", [])).toBeNull();
    expect(describeLink("#/worklet/GP-1", [])).toBeNull();
    expect(describeLink("mailto:ops@example.com", [])).toBeNull();
  });
});

describe("where a bare reference points", () => {
  // A HOST LINKS ONLY UNDER AN OWNER IT NAMES, and one that names none
  // vouches for no owner at all: closed, never open.
  //
  // Mutation: let a host with no owners take any owner and the first case
  // links.
  test("a host that names no owners, or not this one, links no reference", () => {
    const bare: LinkHost = { kind: "github", url: "https://github.com" };
    expect(referenceTarget("gophrase/backend", "#", "14", [bare])).toEqual({
      vendor: "github",
      href: null,
    });
    expect(referenceTarget("other/backend", "#", "14", [GITHUB])).toEqual({
      vendor: "github",
      href: null,
    });
    expect(
      describeLink("https://github.com/gophrase/backend/pull/14", [bare])?.canonical,
    ).toBeNull();
    expect(describeLink("https://github.com/other/x", [GITHUB])?.canonical).toBeNull();
    expect(
      describeLink("https://code.example.com/scm/g/app/-/issues/3", [{ ...GITLAB, owners: [] }])
        ?.canonical,
    ).toBeNull();
  });

  test("one code host links it there, GitHub's issue address standing for a pull request too", () => {
    expect(referenceTarget("gophrase/backend", "#", "14", [GITHUB])).toEqual({
      vendor: "github",
      href: "https://github.com/gophrase/backend/issues/14",
    });
    expect(referenceTarget("g/sub/app", "!", "7", [GITLAB])).toEqual({
      vendor: "gitlab",
      href: "https://code.example.com/scm/g/sub/app/-/merge_requests/7",
    });
    expect(referenceTarget("g/app", "@", "b5362d2", [GITLAB])?.href).toBe(
      "https://code.example.com/scm/g/app/-/commit/b5362d2",
    );
  });

  test("with no code host, the spelling names the vendor and nothing links", () => {
    expect(referenceTarget("a/b", "#", "1", [])).toEqual({ vendor: "github", href: null });
    expect(referenceTarget("a/b", "!", "1", [])).toEqual({ vendor: "gitlab", href: null });
    expect(referenceTarget("a/b", "@", "b5362d2", [])).toBeNull();
  });

  test("with both code hosts, a reference either could own stays text", () => {
    expect(referenceTarget("a/b", "#", "1", [GITHUB, GITLAB])).toBeNull();
    expect(referenceTarget("a/b", "@", "b5362d2", [GITHUB, GITLAB])).toBeNull();
    expect(referenceTarget("a/b", "!", "1", [GITHUB, GITLAB])?.vendor).toBe("gitlab");
  });
});

describe("repositories", () => {
  const OWNERS = ["gophrase"];

  test("a bare owner/repo is a repository only under an owner the code hosts name", () => {
    expect(said("assigned to you in gophrase/backend.", OWNERS)).toEqual([
      "repository:gophrase/backend",
    ]);
    expect(said("GoPhrase/Backend", OWNERS)).toEqual(["repository:GoPhrase/Backend"]);
    expect(said("assigned to you in gophrase/backend.")).toEqual([]);
  });

  test.each(["and/or", "src/app", "client/server", "2024/10", "1/2", "UI/UX"])(
    "%s is words: no company works under its first part",
    (text) => {
      expect(said(`read ${text} here`, OWNERS)).toEqual([]);
    },
  );

  test.each([
    "gophrase/backend/src",
    "/gophrase/backend",
    "./gophrase/backend",
    "~gophrase/backend",
    "gophrase/backend.py",
    "x.gophrase/backend",
    "https://example.com/gophrase/backend",
  ])("%s is a path or an address, not a repository", (text) => {
    expect(said(text, OWNERS).filter((t) => t.startsWith("repository:"))).toEqual([]);
  });

  test("a reference is a reference, not a repository and a number", () => {
    expect(said("gophrase/backend#11", OWNERS)).toEqual(["reference:gophrase/backend#11"]);
  });

  test("a repository goes to its owner's code host, and nowhere when two hosts name the owner", () => {
    const github: LinkHost = { kind: "github", url: "https://github.com", owners: ["gophrase"] };
    expect(repositoryTarget("GoPhrase", "backend", [github])).toEqual({
      vendor: "github",
      href: "https://github.com/GoPhrase/backend",
    });
    const both: LinkHost = {
      kind: "gitlab",
      url: "https://gitlab.example.com",
      owners: ["gophrase"],
    };
    expect(repositoryTarget("gophrase", "backend", [github, both])).toBeNull();
    expect(repositoryTarget("other", "backend", [github])).toBeNull();
    expect(codeHostOwners([github, { kind: "jira", url: "https://j.example.com" }])).toEqual(
      new Set(["gophrase"]),
    );
  });
});

describe("bare numbers", () => {
  const GH: LinkHost[] = [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }];
  /** A run's references as `path#id` for its text, read in a message of `texts`. */
  const placed = (text: string, texts: readonly string[] = [text]) =>
    inlineTokens(text, new Set(["gophrase"]), messageScope(texts, GH)).flatMap((t) =>
      t.kind === "reference" ? [`${t.text}=${t.path}${t.sep}${t.id}`] : [],
    );

  test.each([
    ["#7 in gophrase/frontend", "#7 in gophrase/frontend=gophrase/frontend#7"],
    ["#7 of gophrase/frontend.", "#7 of gophrase/frontend=gophrase/frontend#7"],
    ["gophrase/frontend PR #7", "gophrase/frontend PR #7=gophrase/frontend#7"],
    ["gophrase/frontend pull request #7", "gophrase/frontend pull request #7=gophrase/frontend#7"],
    ["gophrase/frontend issue #7", "gophrase/frontend issue #7=gophrase/frontend#7"],
    ["platform/api MR #7", "platform/api MR #7=platform/api!7"],
    ["acme/widget #7", "acme/widget #7=acme/widget#7"],
  ])("the sentence binds %s", (text, want) => {
    expect(placed(text)).toEqual([want]);
  });

  test("the same number in full elsewhere in the message places it, and nothing else does", () => {
    const texts = ["Review on gophrase/backend#11 and gophrase/web#3.", "Then #11, not #3x or #4."];
    expect(placed(texts[1]!, texts)).toEqual(["#11=gophrase/backend#11"]);
  });

  test("a number under two repositories is a word", () => {
    const texts = ["gophrase/backend#11 and gophrase/web#11.", "Then #11."];
    expect(placed(texts[1]!, texts)).toEqual([]);
  });

  test("the message's one repository places a number, from an address too", () => {
    expect(placed("in gophrase/backend; start after #11")).toContain("#11=gophrase/backend#11");
    const texts = ["See https://github.com/gophrase/backend/pull/11/files.", "Then #12."];
    expect(placed(texts[1]!, texts)).toEqual(["#12=gophrase/backend#12"]);
  });

  test.each([
    "#011",
    "&#11;",
    "C#11",
    "a#11",
    "x/#11",
    "#11a",
    "#11-b",
    "#1.5",
    "https://example.com/#11",
    "/issues/#11",
  ])("%s is no bare number", (text) => {
    expect(placed(text, [text, "gophrase/backend"])).toEqual([]);
  });

  test("with nothing to place it, a number is a word", () => {
    expect(placed("Our #1 priority is #11.")).toEqual([]);
  });

  // THE CHOSEN TRADE-OFF, held so changing it is a decision: in a message that
  // names one repository, a rank and a heading's number are its issues too,
  // as GitHub's own autolinking reads them there.
  test("in a message naming one repository, a rank and a heading's number are placed too", () => {
    const texts = ["Assigned in gophrase/backend.", "Our #1 priority.", "#2 heading"];
    expect(placed(texts[1]!, texts)).toEqual(["#1=gophrase/backend#1"]);
    expect(placed(texts[2]!, texts)).toEqual(["#2=gophrase/backend#2"]);
  });
});

describe("cost", () => {
  // EVERY PATTERN IS LINEAR IN ITS RUN: a comment an agent wrote is rendered by
  // every reader, so a run that costs seconds freezes everybody.
  //
  // GROWTH, NOT A CLOCK: a run four times longer must take well under the
  // sixteen times a quadratic pass takes, here under eight. An absolute bound
  // passed both a linear pass and a quadratic one at the size a loaded
  // runner could afford, so it held nothing; a ratio is the machine's speed
  // divided out. Each run is timed after a warm-up, as the median of five
  // samples, each sample enough calls to take 10 ms, so a timer's grain or one
  // stalled sample decides nothing.
  //
  // Mutation: count the brackets again per character trimmed, bring back the
  // lazy pattern around a word, or scan the text before every bound number,
  // and its case exceeds it.
  const OWNERS = new Set(["a", "gp"]);
  const sample = (text: string, calls: number) => {
    const start = performance.now();
    for (let i = 0; i < calls; i++) inlineTokens(text, OWNERS);
    return performance.now() - start;
  };
  const median = (xs: number[]) => xs.sort((x, y) => x - y)[Math.floor(xs.length / 2)]!;
  /** How much longer a run four times the size takes. */
  const growth = (make: (n: number) => string, n: number) => {
    const small = make(n);
    const large = make(4 * n);
    sample(small, 1);
    sample(large, 1);
    let calls = 1;
    while (calls < 256 && sample(small, calls) < 10) calls *= 2;
    const times = (text: string) => median(Array.from({ length: 5 }, () => sample(text, calls)));
    return times(large) / times(small);
  };

  test.each<[string, (n: number) => string]>([
    ["an address closed by brackets", (n) => `https://x.test/${")".repeat(n)}`],
    ["a word of brackets and a letter", (n) => `${")".repeat(n)}a`],
    ["slashes", (n) => `${"a/".repeat(n / 2)}b`],
    ["stops", (n) => "a.".repeat(n / 2)],
    ["numbers", (n) => "#1".repeat(n / 2)],
    ["keys between slashes", (n) => "GP-1/".repeat(n / 5)],
    ["handles", (n) => "@a".repeat(n / 2)],
    ["the word commit", (n) => "commit ".repeat(n / 7)],
    ["bound numbers", (n) => "#1 in a/b ".repeat(n / 10)],
    ["repositories before a number", (n) => "a/b PR ".repeat(n / 7)],
    ["repositories and numbers", (n) => "a/b #1 ".repeat(n / 7)],
  ])("%s read in linear time", (_, make) => {
    expect(growth(make, 5_000)).toBeLessThan(8);
  });
});
