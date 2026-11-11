import { describe, expect, test } from "vitest";
import type { HeldDelegation } from "~/contract/delegated.ts";
import {
  delegatedRuleOf,
  delegationEscalated,
  delegationWaiting,
  escalatedByWords,
  isReleaseAct,
  kindWords,
  mergeMethodWords,
  newestFlag,
  overruledWords,
  readDismissed,
  releaseAllowWords,
  releaseApproverWords,
  releaseRequestOf,
  verdictWords,
  withDelegateWords,
  writeDismissed,
} from "./delegation.ts";

const NAMES: Record<string, string> = {
  "security-engineer": "Security Engineer",
  "ops-lead": "Ops Lead",
};
const name = (h: string) => NAMES[h] ?? h;

function delegation(over: Partial<HeldDelegation> = {}): HeldDelegation {
  return { rule: 1, delegates: ["security-engineer"], given: [], escalated: null, ...over };
}

const ESCALATED = { overruled: ["deadline"], engine: ["No delegate answered in time."], at: "" };

describe("who a held call waits on", () => {
  // THE ROW SAYS IT, never a guess: delegated with no escalation waits on its
  // delegates; escalated, it is a person's; no delegation, a person's at once.
  test("a call is with its delegates until it is escalated", () => {
    expect(delegationWaiting({ delegation: delegation() })).not.toBeNull();
    expect(delegationEscalated({ delegation: delegation() })).toBeNull();
    const up = delegation({ escalated: ESCALATED });
    expect(delegationWaiting({ delegation: up })).toBeNull();
    expect(delegationEscalated({ delegation: up })).toBe(up);
    expect(delegationWaiting({ delegation: null })).toBeNull();
    expect(delegationWaiting(undefined)).toBeNull();
  });

  test("a waiting call names its delegates and its rule", () => {
    expect(withDelegateWords(delegation(), name)).toBe("With delegate Security Engineer (rule 1)");
    expect(
      withDelegateWords(delegation({ rule: 3, delegates: ["security-engineer", "sre"] }), name),
    ).toBe("With delegates Security Engineer and sre (rule 3)");
  });
});

describe("who escalated a call", () => {
  // THE DELEGATES WHO SAID ESCALATE, or Crewlet where none did: a delegate
  // that allowed is never named as having escalated what the engine overruled.
  test("names the delegates that escalated, or Crewlet", () => {
    const at = "2026-11-09T10:00:00Z";
    const one = delegation({
      escalated: ESCALATED,
      given: [
        {
          by: "security-engineer",
          verdict: "escalate",
          confidence: "high",
          reason: "",
          findings: [],
          at,
        },
      ],
    });
    expect(escalatedByWords(one, name)).toBe("Escalated by Security Engineer (rule 1)");
    const overruled = delegation({
      escalated: ESCALATED,
      given: [
        {
          by: "security-engineer",
          verdict: "allow",
          confidence: "high",
          reason: "",
          findings: [],
          at,
        },
      ],
    });
    expect(escalatedByWords(overruled, name)).toBe("Escalated by Crewlet (rule 1)");
    expect(escalatedByWords(delegation({ escalated: ESCALATED }), name)).toBe(
      "Escalated by Crewlet (rule 1)",
    );
  });
});

describe("words for the engine's closed words", () => {
  test("a verdict is a word and a state; an unknown one is said as written", () => {
    expect(verdictWords("allow")).toEqual({ label: "Allowed", tone: "success" });
    expect(verdictWords("deny")).toEqual({ label: "Denied", tone: "danger" });
    expect(verdictWords("hold_on")).toEqual({ label: "hold on", tone: "neutral" });
  });

  test("an overruling is a phrase; a class this build does not name is said as written", () => {
    expect(overruledWords("low_confidence")).toBe("low confidence");
    expect(overruledWords("deadline")).toBe("no delegate answered in time");
    expect(overruledWords("authorized_key")).toBe("authorized key");
  });

  // A MERGE'S AND A DEPLOY'S UNMET CONDITIONS and the path classes no delegate
  // allows (`delegated.Unmet*`, `delegated.Path*`) each read as a phrase, never
  // as the engine's word with its underscores taken out.
  test("a merge's or a deploy's unmet condition and every path class is a phrase", () => {
    const words = [
      "facts_unread",
      "head_moved",
      "not_open",
      "no_required_checks",
      "checks_not_passed",
      "not_mergeable",
      "not_ready",
      "no_approval",
      "paths_unclassified",
      "not_merged",
      "not_contained",
      "ci_pipelines",
      "code_owners",
      "secret_files",
      "access_policies",
      "infrastructure",
      "migrations",
      "crewlet_company",
      "rule_path",
    ];
    for (const word of words) {
      expect(overruledWords(word), word).not.toBe(word.replaceAll("_", " "));
    }
    expect(overruledWords("head_moved")).toBe("the change's head is not the one asked about");
    expect(overruledWords("rule_path")).toBe("it touches a path the rule always puts to people");
  });

  test("each act a rule may delegate has its words; an unknown one is said as written", () => {
    expect(kindWords("ssh_operate")).toBe("Commands on servers");
    expect(kindWords("pr_merge")).toBe("Merges");
    expect(kindWords("deploy")).toBe("Deploys");
    expect(kindWords("launch" as never)).toBe("launch");
  });
});

describe("a held merge or deploy", () => {
  const HEAD = "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345";
  const MERGE = { change: "web!42", head: HEAD, method: "squash", reason: "Fixes the login loop." };
  const DEPLOY = {
    repo: "web",
    commit: HEAD,
    changes: ["web!41", 42],
    environment: "production",
    rollback: "Deploy 1a2b3c4 again.",
    reason: "Ships the login fix.",
  };

  test("only a merge and a deploy are release acts", () => {
    expect(isReleaseAct("pr_merge")).toBe(true);
    expect(isReleaseAct("deploy")).toBe(true);
    expect(isReleaseAct("ssh_operate")).toBe(false);
    expect(isReleaseAct(undefined)).toBe(false);
  });

  test("its arguments read as the request, each value as the agent sent it", () => {
    expect(releaseRequestOf("pr_merge", MERGE)).toEqual({ act: "pr_merge", ...MERGE });
    // A METHOD LEFT OUT is the engine's default, said by the method's words.
    const bare = { change: MERGE.change, head: HEAD, reason: MERGE.reason };
    expect(releaseRequestOf("pr_merge", bare)).toEqual({ act: "pr_merge", ...bare, method: "" });
    expect(releaseRequestOf("deploy", DEPLOY)).toEqual({
      act: "deploy",
      ...DEPLOY,
      changes: ["web!41", "42"],
    });
  });

  // READ WHOLE OR NOT AT ALL: a key the tool does not take, or a value of
  // another type, leaves the arguments to be drawn as sent. Mutation: drop
  // the key check, and the extra key is never shown.
  test("arguments it cannot read whole are not read as a request", () => {
    expect(releaseRequestOf("pr_merge", { ...MERGE, force: true })).toBeNull();
    expect(releaseRequestOf("pr_merge", { ...MERGE, head: 42 })).toBeNull();
    expect(releaseRequestOf("pr_merge", { ...MERGE, reason: undefined })).toBeNull();
    expect(releaseRequestOf("deploy", { ...DEPLOY, changes: "web!41" })).toBeNull();
    expect(releaseRequestOf("deploy", { ...DEPLOY, changes: [{ n: 41 }] })).toBeNull();
    expect(releaseRequestOf("deploy", MERGE)).toBeNull();
    expect(releaseRequestOf("ssh_operate", { host: "web-1", command: "uptime" })).toBeNull();
    expect(releaseRequestOf("pr_merge", null)).toBeNull();
    expect(releaseRequestOf("pr_merge", undefined)).toBeNull();
  });

  test("a merge's method is said in words, an unknown one as written", () => {
    expect(mergeMethodWords("")).toBe("A merge commit");
    expect(mergeMethodWords("merge")).toBe("A merge commit");
    expect(mergeMethodWords("squash")).toBe("One squashed commit");
    expect(mergeMethodWords("rebase")).toBe("rebase");
  });

  test("who may answer it names the rule's people where a rule routed it", () => {
    expect(releaseApproverWords("pr_merge", "SRE", 2)).toBe(
      "Administrators, SRE's leads and the people rule 2 escalates to may answer it. " +
        "Answering asks you to confirm it\u2019s you unless you did recently.",
    );
    expect(releaseApproverWords("deploy", "Release Bot", null)).toMatch(
      /^Administrators, Release Bot's leads and the people a delegated decisions rule for deploys escalates to may answer it\./,
    );
    expect(releaseAllowWords("pr_merge", "SRE")).toMatch(/^Allow once merges exactly this head/);
    expect(releaseAllowWords("deploy", "SRE")).toMatch(/Crewlet does not deploy it\.$/);
  });
});

describe("an answer a delegate gave", () => {
  const event = (type: string, payload: Record<string, unknown>) => ({ type, payload });
  // ONLY THE ANSWER EVENTS, and only a whole rule number: a person's answer
  // carries none, and any other event's field of the same name is not this.
  test("is read off the answer events alone", () => {
    expect(delegatedRuleOf(event("executor_answered", { delegated_rule: 2 }))).toBe(2);
    expect(delegatedRuleOf(event("executor_answer_given", { delegated_rule: 1 }))).toBe(1);
    expect(delegatedRuleOf(event("executor_answered", {}))).toBeNull();
    expect(delegatedRuleOf(event("executor_answered", { delegated_rule: 0 }))).toBeNull();
    expect(delegatedRuleOf(event("executor_finished", { delegated_rule: 2 }))).toBeNull();
  });
});

describe("the Home notices put away", () => {
  function memory(): Storage {
    const kept = new Map<string, string>();
    return {
      getItem: (k: string) => kept.get(k) ?? null,
      setItem: (k: string, v: string) => void kept.set(k, v),
    } as Storage;
  }

  test("each notice is kept apart, and a refused storage keeps nothing and throws nothing", () => {
    const store = memory();
    writeDismissed(() => store, { decided: "2026-11-09" });
    writeDismissed(() => store, { flagged: "2026-11-09T10:00:00Z" });
    expect(readDismissed(() => store)).toEqual({
      decided: "2026-11-09",
      flagged: "2026-11-09T10:00:00Z",
    });
    const refused = () => {
      throw new Error("blocked");
    };
    expect(readDismissed(refused)).toEqual({});
    expect(() => writeDismissed(refused, { decided: "x" })).not.toThrow();
  });

  test("the flagged notice is keyed by its newest flag", () => {
    expect(newestFlag([{ at: "2026-11-08T10:00:00Z" }, { at: "2026-11-09T09:00:00Z" }])).toBe(
      "2026-11-09T09:00:00Z",
    );
    expect(newestFlag([])).toBe("");
  });
});
