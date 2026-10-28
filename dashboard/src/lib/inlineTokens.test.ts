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
  referenceTarget,
  repositoryTarget,
} from "./inlineTokens.ts";

/** What a run's tokens are, as `kind:text`, read under `owners`. */
function said(text: string, owners: readonly string[] = []): string[] {
  return inlineTokens(text, new Set(owners)).map((t) => `${t.kind}:${t.text}`);
}

const GITHUB: LinkHost = { kind: "github", url: "https://github.com" };
const GHE: LinkHost = { kind: "github", url: "https://github.example.com" };
const GITLAB: LinkHost = { kind: "gitlab", url: "https://code.example.com/scm" };
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
    ).toMatchObject({ vendor: "atlassian", label: "Brand tokens" });
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
    // A TENANT ANYBODY CAN REGISTER says its host after the label, so a page
    // titled "Sign in to GitHub" is read as the site it is on; a site the
    // company configured is its own and its labels stand alone.
    expect(describeLink("https://acme.atlassian.net/browse/GP-1", [])).toMatchObject({
      label: "GP-1 (acme.atlassian.net)",
      code: false,
    });
    expect(
      describeLink("https://acme.atlassian.net/wiki/spaces/D/pages/2/Sign+in+to+GitHub", [])?.label,
    ).toBe("Sign in to GitHub (acme.atlassian.net)");
    const own: LinkHost[] = [
      { kind: "jira", url: "https://acme.atlassian.net" },
      { kind: "confluence", url: "https://acme.atlassian.net/wiki" },
    ];
    expect(describeLink("https://acme.atlassian.net/browse/GP-1", own)?.label).toBe("GP-1");
    expect(
      describeLink("https://acme.atlassian.net/wiki/spaces/D/pages/2/Brand+tokens", own)?.label,
    ).toBe("Brand tokens");
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
    });
    expect(describeLink(`https://example.com/${"a".repeat(80)}`, [])?.label).toHaveLength(48);
    expect(describeLink("javascript:alert(1)", [])).toBeNull();
    expect(describeLink("#/worklet/GP-1", [])).toBeNull();
    expect(describeLink("mailto:ops@example.com", [])).toBeNull();
  });
});

describe("where a bare reference points", () => {
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

describe("cost", () => {
  // EVERY PATTERN IS LINEAR IN ITS RUN: a comment an agent wrote is rendered by
  // every reader, so a run that costs seconds freezes everybody. The bound is
  // two orders above what a linear pass takes here and two below the
  // quadratic passes these replaced (about 2.7 s and 0.6 s at this size).
  //
  // Mutation: count the brackets again per character trimmed, or bring back
  // the lazy pattern around a word, and the first two cases exceed it.
  test.each([
    ["an address closed by 20,000 brackets", `https://x.test/${")".repeat(20_000)}`],
    ["a word of 20,000 brackets and a letter", `${")".repeat(20_000)}a`],
    ["20,000 slashes", `${"a/".repeat(10_000)}b`],
    ["20,000 stops", "a.".repeat(10_000)],
    ["10,000 numbers", "#1".repeat(10_000)],
    ["5,000 keys", "GP-1/".repeat(5_000)],
    ["10,000 handles", "@a".repeat(10_000)],
    ["3,000 commits", "commit ".repeat(3_000)],
  ])("%s reads in linear time", (_, text) => {
    const start = performance.now();
    inlineTokens(text, new Set(["a", "gp"]));
    expect(performance.now() - start).toBeLessThan(250);
  });
});
