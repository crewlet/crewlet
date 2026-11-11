import { describe, expect, test } from "vitest";
import type { HeldDelegation } from "~/contract/delegated.ts";
import {
  delegatedRuleOf,
  delegationEscalated,
  delegationWaiting,
  escalatedByWords,
  newestFlag,
  overruledWords,
  readDismissed,
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
