/**
 * The base company reads as the company the design screenshots were taken of.
 *
 * The facts a reader compares a proposal against (two stopped agents paused
 * by Miles a day ago, the PM's week of turns, two decisions and six unread
 * updates, six integrations nobody connected) are asserted on the SHIPPED
 * screens, booted on `fairsplit()` itself rather than on a topic's scenario,
 * so a fixture change that drifts from the screenshots fails here. It runs
 * once, with the dashboard as it ships: a variant may draw these facts
 * differently, and that is what it is for.
 */

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { beforeAll, describe, expect, test, vi } from "vitest";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import { makeDemoClient } from "../harness/demoClient.ts";
import { installRestShim } from "../harness/restShim.ts";

async function settle(): Promise<void> {
  let last = "";
  let steady = 0;
  for (let waited = 0; waited < 15_000 && steady < 4; waited += 120) {
    await new Promise((r) => setTimeout(r, 120));
    const now = document.body.innerHTML;
    steady = now === last ? steady + 1 : 0;
    last = now;
  }
}

const shown = () => (document.getElementById("root")?.textContent ?? "").replace(/\s+/g, " ");

/** Each screen, and what it must say. */
const FACTS: [string, RegExp[]][] = [
  [
    "#/agents",
    [
      /FairSplit/,
      /Product and Engineering/,
      /Lead: Crewlet-PM/,
      /Working 0/,
      /Needs you 0/,
      /Stopped 2/,
      /Idle 0/,
    ],
  ],
  [
    "#/agents/roster",
    [/Stopped\s*2 seats/, /Crewlet-PM@pm/, /Paused by Miles · 25h/, /Human teammates/],
  ],
  ["#/agents/teams", [/Above every unit/, /Plan, build and ship FairSplit/, /2 seats/]],
  ["#/agents/schedules", [/daily-standup/, /30 9 \* \* 1-5/, /skipped · missed/]],
  [
    "#/agents/seats/pm",
    [
      /Paused by Miles 1d ago: .weekly limit\../,
      /Turns · 7 days\s*70\s*\+70 vs last week/,
      /Approved first pass\s*80%\s*0 sent back/,
      /Median turn\s*4\.2s\s*p90 12s/,
      /Tokens · today\s*0\s*of 4M budget/,
      /Its last turn failed 1d ago/,
      /Last 14 days · 59 failed/,
      /Model\s*default/,
    ],
  ],
  [
    "#/inbox",
    [
      /Inbox, 6 unread/,
      /Should the MVP support unequal splits, or equal splits only\?/,
      /How should the MVP store a group's data locally\?/,
      /Crewlet-SWE is stopped/,
      /Crewlet-PM is stopped/,
      /You reported it @pm Received the brief and backlog/,
    ],
  ],
  [
    "#/settings/integrations",
    [
      /Third-party apps reach this engine at https:\/\/fairsplit\.example\.com/,
      /Connected\s*0\s*Available\s*6/,
      /(Not in use.*){6}/,
      /Configuration, epoch 8/,
    ],
  ],
];

describe("the base company, on the shipped screens", () => {
  beforeAll(async () => {
    vi.spyOn(console, "info").mockImplementation(() => {});
    const client = makeDemoClient(fairsplit());
    installRestShim(client.engine);
    client.socket.start();
    document.body.innerHTML = '<div id="root"></div>';
    history.replaceState(null, "", "#/home");
    createRoot(document.getElementById("root")!).render(
      <StrictMode>
        <ClientContext.Provider value={{ store: client.store, socket: client.socket }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>
      </StrictMode>,
    );
    await settle();
  });

  test.each(FACTS)("%s says what the screenshot says", async (route, facts) => {
    location.hash = route;
    await settle();
    const text = shown();
    for (const fact of facts) expect(text).toMatch(fact);
  });
});
