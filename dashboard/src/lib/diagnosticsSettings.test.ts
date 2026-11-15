/**
 * Settings › Diagnostics' two choices as plans for the one setting write: each
 * writes only its own keys, and nothing where the document already says so.
 */

import { expect, test } from "vitest";

import { insightsPlan, issueReportsPlan } from "./diagnosticsSettings.ts";

test("bug reports write the one switch, and nothing where the document already says so", () => {
  expect(issueReportsPlan(true)({})).toEqual({
    patch: { diagnostics: { issue_reports: { enabled: true } } },
    summary: "Turned on bug reports to Crewlet",
  });
  expect(issueReportsPlan(false)({})).toBeNull();
  const on = { diagnostics: { issue_reports: { enabled: true } } } as never;
  expect(issueReportsPlan(true)(on)).toBeNull();
  expect(issueReportsPlan(false)(on)).toEqual({
    patch: { diagnostics: { issue_reports: { enabled: false } } },
    summary: "Turned off bug reports to Crewlet",
  });
});

test("an insights choice writes only the keys it sets, and an unset share is off", () => {
  expect(insightsPlan({ share: "off" }, "off")({})).toBeNull();
  expect(insightsPlan({ share: "review", accepted: 2 }, "review")({})).toEqual({
    patch: { diagnostics: { insights: { share: "review", accepted: 2 } } },
    summary: "review",
  });
  const held = { diagnostics: { insights: { share: "review", accepted: 1 } } } as never;
  expect(insightsPlan({ share: "review", accepted: 1 }, "same")(held)).toBeNull();
  expect(insightsPlan({ accepted: 2 }, "accept")(held)).toEqual({
    patch: { diagnostics: { insights: { accepted: 2 } } },
    summary: "accept",
  });
});
