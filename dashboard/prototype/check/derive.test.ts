/**
 * The demo engine derives a company's hierarchy exactly as the engine does.
 *
 * Every org edit made in a prototype is drawn from `derive()`, so a rule it
 * got wrong would draw a chart the engine would not. The expected answers are
 * the ENGINE'S OWN golden files (`internal/config/testdata/derived`), read
 * from the engine's tree at run time, so a change to a rule there fails here
 * until the port follows it. Each document is that case's `.company.yaml`,
 * written as the JSON it parses to, and read as the engine reads a file: its
 * identities minted (`config.MintIdentities`) before it is derived.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import { derive, mintIdentities } from "../harness/engine/derive.ts";

const GOLDEN = join(process.cwd(), "..", "internal", "config", "testdata", "derived");

const CASES: Record<string, CompanyDocument> = {
  "lead-and-channel-inheritance": {
    name: "Acme",
    units: [
      {
        name: "Engineering",
        type: "division",
        lead: "vp-engineering",
        channel: "#engineering",
        roles: [{ name: "VP Engineering" }],
        children: [
          {
            name: "Backend",
            roles: [{ name: "Backend Developer" }],
            children: [{ name: "Storage", roles: [{ name: "Storage Developer" }] }],
          },
          {
            name: "Infrastructure",
            channel: "#infra",
            lead: "sre",
            roles: [{ name: "Site Reliability", handle: "sre" }],
          },
          { name: "Security", lead: "ghost", roles: [{ name: "Security Analyst" }] },
        ],
      },
    ],
  },
  "placement-and-references": {
    name: "Acme",
    roles: [
      {
        name: "Founder",
        kind: "human",
        contact: { slack_user_id: "U0FOUNDER" },
        manages: ["product-lead", "hiring", "ghost"],
      },
      { name: "Designer", unit: "product" },
      { name: "Wanderer", unit: "nowhere" },
    ],
    units: [
      {
        name: "Product",
        id: "product",
        lead: "product-lead",
        roles: [
          { name: "Product Lead" },
          { name: "Sarah Chen", kind: "human", contact: { slack_user_id: "U0SARAH" } },
        ],
      },
      { name: "Hiring", id: "hiring" },
    ],
  },
  "root-seat-manages-a-unit": {
    name: "Acme",
    roles: [{ name: "Chief Executive", manages: ["backend"] }],
    units: [
      {
        name: "Backend",
        id: "backend",
        lead: "backend-lead",
        roles: [{ name: "Backend Lead" }, { name: "Developer" }],
      },
    ],
  },
  "unit-reference-shields": {
    name: "Acme",
    units: [
      {
        name: "Engineering",
        id: "engineering",
        lead: "tech-lead",
        roles: [
          { name: "Tech Lead" },
          { name: "Engineering Manager", manages: ["engineering"] },
          { name: "Developer A" },
          { name: "Developer B" },
        ],
      },
      {
        name: "Design",
        id: "design",
        lead: "design-lead",
        roles: [
          { name: "Design Lead" },
          { name: "Designer", manages: ["illustrator"] },
          { name: "Illustrator" },
          { name: "Researcher" },
        ],
      },
    ],
  },
  handles: {
    name: "Acme",
    roles: [
      { name: "\u0130lker Demir" },
      { name: "Grace Hopper", handle: "grace" },
      { name: "  Ada   Lovelace  " },
    ],
  },
};

describe("derive", () => {
  test.each(Object.entries(CASES))("%s derives as the engine derives it", (name, doc) => {
    const golden: unknown = JSON.parse(readFileSync(join(GOLDEN, `${name}.derived.json`), "utf-8"));
    // AS A FILE IS READ: every unit's key and seat's handle minted first.
    const read = structuredClone(doc);
    mintIdentities(read);
    expect(derive(read)).toEqual(golden);
  });
});
