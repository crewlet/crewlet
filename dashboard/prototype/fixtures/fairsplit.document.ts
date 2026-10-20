/**
 * FairSplit's company document: the configuration the demo company runs, as
 * the guarded `/config` surface serves it.
 *
 * A two-seat engineering team under one person: Miles, a human seat at the
 * root who answers decisions from the dashboard inbox, and the Product and
 * Engineering team he set up to build FairSplit, led by Crewlet-PM with
 * Crewlet-SWE reporting to it. Every credential is a `${VAR}` reference, as
 * the surface redacts one; none of the values behind them exist.
 *
 * AS THE ENGINE STORES IT: every write mints a unit's key and a seat's handle
 * where the document declares none (`config.MintIdentities`), and a lead and
 * a `manages:` entry name a seat by its HANDLE and a unit by its KEY, so the
 * document states all of them. Miles's browser credential is bound to his
 * seat in the identity directory, never in the document.
 *
 * Its own module because the engine's setup surface is captured from this
 * same document (`fixtures/engine/README.md`), so the connect forms a
 * prototype draws are the ones the engine would draw for this company.
 */

import type { CompanyDocument } from "~/protocol/types.ts";

const PM_GOAL =
  "Turn Miles's direction into a clear product brief and a prioritised FAIR backlog with testable acceptance criteria, and keep the work moving.";

const SWE_GOAL =
  "Build FairSplit from the FAIR backlog: small, tested changes that meet each work item's acceptance criteria, with a design page before the code.";

/** The company document, fresh on every call so a caller may change its copy. */
export function fairsplitDocument(): CompanyDocument {
  return {
    name: "FairSplit",
    mission:
      "Help groups of friends split shared costs fairly and settle up with the fewest payments.",
    vision: "Splitting a bill is never the awkward part of a trip again.",
    policies: [
      "Every amount is exact to the cent, and the rounding rule is written down.",
      "Work is tracked on the FAIR board, and nothing ships without acceptance criteria.",
      "Ask Miles before changing the MVP scope.",
    ],
    timezone: "America/Los_Angeles",
    providers: {
      llm: {
        default: {
          type: "anthropic",
          model: "claude-sonnet-5",
          api_keys: ["${ANTHROPIC_API_KEY}"],
        },
      },
    },
    roles: [
      {
        name: "Miles",
        kind: "human",
        handle: "miles",
        email: "miles@fairsplit.example",
        availability: "Weekdays, Pacific time; answers decisions in the dashboard inbox",
        manages: ["pm"],
      },
    ],
    units: [
      {
        name: "Product and Engineering",
        id: "product-and-engineering",
        type: "team",
        purpose: "Plan, build and ship FairSplit, and keep Miles informed through the FAIR board.",
        lead: "pm",
        goals: [
          "Ship the FairSplit MVP: a Python command-line tool that records a group's shared expenses and prints the fewest payments that settle everyone up, exact to the cent.",
          "Keep the FAIR board current: every piece of work is a work item with an owner, acceptance criteria and a status that matches reality.",
          "Post a stand-up note every weekday morning.",
        ],
        project: "FAIR",
        space: "FAIR",
        roles: [
          {
            name: "Crewlet-PM",
            handle: "pm",
            goal: PM_GOAL,
            backstory:
              "A product manager who writes things down: a brief before a backlog, acceptance criteria before code, and a decision record whenever Miles answers a question.",
            responsibilities: [
              "Own the product brief and the MVP scope in the FAIR space",
              "File, prioritise and assign FAIR work items with acceptance criteria the Software Engineer can test against",
              "Review delivered work against its acceptance criteria, then close it or send it back with what is missing",
              "Ask Miles for a decision when the scope or a trade-off is his to make, with a recommendation and its evidence",
              "Post the team's stand-up note every weekday morning",
            ],
            behavioral_guidelines: [
              "Keep every question to Miles to one decision with two to four options.",
              "Never close a work item whose acceptance criteria have not been checked.",
            ],
            avatar: { character: "crewlet", color: "rose" },
            llm: "default",
            token_budget: { day: 4_000_000 },
            schedules: [
              {
                name: "daily-standup",
                cron: "30 9 * * 1-5",
                task: "Write today's stand-up note for the Product and Engineering team: what moved on the FAIR board yesterday, what is in progress, and what is waiting on Miles.",
              },
            ],
          },
          {
            name: "Crewlet-SWE",
            handle: "engineer",
            goal: SWE_GOAL,
            backstory:
              "A careful Python engineer who writes the test first and keeps every change small enough to review in one sitting.",
            responsibilities: [
              "Design each FAIR work item on a page in the FAIR space before writing its code",
              "Implement FAIR work items in small, reviewed changes with tests",
              "Report progress and blockers on the work item, not in chat",
            ],
            avatar: { character: "gemlet", color: "cyan" },
            llm: "default",
            token_budget: { day: 4_000_000 },
          },
        ],
      },
    ],
  };
}
