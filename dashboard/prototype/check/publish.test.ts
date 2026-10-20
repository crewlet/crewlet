/**
 * A built variant is something the publisher takes. The brand mark the
 * shipped config copies into every variant (`vite.config.ts`, from
 * `@crewlethq/icons`) loses exactly the line declaring its document type and
 * still parses as an SVG, and an SVG the finishing pass cannot make
 * publishable is refused, naming it, rather than shipped.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { publishableSvg } from "../harness/publish.mjs";

/** The file `vite.config.ts` copies to every build as `crewlet-icon.svg`. */
const SHIPPED = join(
  process.cwd(),
  "node_modules",
  "@crewlethq",
  "icons",
  "svg",
  "crewlet-icon.svg",
);
const SVG_NS = "http://www.w3.org/2000/svg";

test("the shipped mark loses exactly its document type line and still parses as an SVG", () => {
  const text = readFileSync(SHIPPED, "utf-8");
  const lines = text.split("\n");
  const doctype = lines.findIndex((line) => line.startsWith("<!DOCTYPE"));
  expect(doctype).toBeGreaterThanOrEqual(0);

  const { text: kept, removed } = publishableSvg(text, "crewlet-icon.svg");
  expect(removed).toBe(true);
  expect(kept.split("\n")).toEqual(lines.filter((_, i) => i !== doctype));
  expect(kept).not.toMatch(/<!(DOCTYPE|ENTITY)/i);

  const doc = new DOMParser().parseFromString(kept, "image/svg+xml");
  expect(doc.getElementsByTagName("parsererror")).toHaveLength(0);
  expect(doc.documentElement.localName).toBe("svg");
  expect(doc.documentElement.namespaceURI).toBe(SVG_NS);
  expect(doc.getElementsByTagName("path").length).toBeGreaterThan(0);
});

test("leaves an SVG with no document type as it is", () => {
  const plain = `<?xml version="1.0"?>\n<svg xmlns="${SVG_NS}"><g/></svg>\n`;
  expect(publishableSvg(plain, "plain.svg")).toEqual({ text: plain, removed: false });
});

test("refuses, naming it, an SVG it cannot make publishable", () => {
  const svg = `<svg xmlns="${SVG_NS}"><g/></svg>`;
  for (const [text, why] of [
    [`<!DOCTYPE svg [<!ENTITY mark "x">]>\n${svg}`, /declares a document type or an entity/],
    [`${svg}\n<!-- <!ENTITY mark "x"> -->`, /declares a document type or an entity/],
    [
      `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.0//EN" "svg10.dtd">${svg}`,
      /declares a document type/,
    ],
    [`<svg xmlns="${SVG_NS}"><g></svg>`, /does not parse as an SVG document/],
    [`<svg><g/></svg>`, /does not parse as an SVG document/],
    [`<html xmlns="http://www.w3.org/1999/xhtml"/>`, /does not parse as an SVG document/],
  ] as const) {
    expect(() => publishableSvg(text, "mark.svg")).toThrow(why);
    expect(() => publishableSvg(text, "mark.svg")).toThrow(/^mark\.svg /);
  }
});
