// @vitest-environment node
/**
 * The dashboard's sentences carry no em dash: every string, template and run
 * of JSX text any of its modules ships, read through the one walk every copy
 * gate shares (`shippedText`), so a comment is free to use one. The copy uses
 * commas, colons, parentheses and full stops instead.
 *
 * ONE GATE FOR THE WHOLE DASHBOARD. It was two, one for the Chatlet workspace
 * and one for Spend, and an em dash outside both shipped in a navigation hint,
 * the direct-message refusal, the org builder's contact help and a trace
 * tooltip with nothing to stop it.
 *
 * A TEXT THAT IS AN EM DASH AND NOTHING ELSE is a mark rather than a
 * sentence (a group's React key, a cell's empty value) and is not judged,
 * but only where it is WHOLE (`ShippedText.whole`): a string literal, a
 * template with nothing substituted, or JSX text that is its element's only
 * child. The constant part of a template beside a substitution (a space, the
 * dash and a space, then `${held}`) and JSX text beside an expression (the
 * dash, then `{count}`) trim to a lone dash too, and each is the joint of a
 * sentence: excused by its trimmed text alone, three such joints shipped in
 * the merge panel and Settings › Code.
 */

import { expect, test } from "vitest";
import { modules, shippedText } from "~/test/source.ts";

/** Spelled by its code point, so this file holds no em dash of its own. */
const EM_DASH = String.fromCodePoint(0x2014);

test("the dashboard's shipped sentences carry no em dash", () => {
  const read = modules().filter((m) => !m.path.startsWith("test/"));
  expect(read.length, "the gate read too few of the dashboard's modules").toBeGreaterThan(300);
  const found: string[] = [];
  for (const module of read) {
    for (const { text, line, whole } of shippedText(module)) {
      if (text.includes(EM_DASH) && !(whole && text.trim() === EM_DASH)) {
        found.push(`${module.path}:${line} ${text.trim()}`);
      }
    }
  }
  expect(found).toEqual([]);
});

/*
 * ONE SPELLING OF COLOUR in the dashboard's own words. Brand and its setup,
 * the inline colour reference, the artifact schema's field and every brand
 * sentence say "colour"; the seat form's picker and the note widget said
 * "Color", so adding a seat and setting the brand spelled one word two ways.
 * Only prose is judged (a text with a space, or one that starts with a
 * capital): a CSS property, a data key and a dotted path are the platform's
 * names, as is a font family's own name.
 */
const COLOR = /(?<![-.\w])colou?r(?:s|ed|ing)?(?![-.\w])/gi;
const FONT_NAMES = /\b(?:Apple|Noto|Segoe UI) Color Emoji\b/g;

test("the dashboard's shipped prose spells colour one way", () => {
  const read = modules().filter((m) => !m.path.startsWith("test/") && !/\.test\./.test(m.path));
  const found: string[] = [];
  for (const module of read) {
    for (const { text, line } of shippedText(module)) {
      const prose = text.replace(FONT_NAMES, "").trim();
      if (!/\s/.test(prose) && !/^[A-Z]/.test(prose)) continue;
      for (const [word] of prose.matchAll(COLOR)) {
        if (!/^colour/i.test(word)) found.push(`${module.path}:${line} ${word}`);
      }
    }
  }
  expect(found).toEqual([]);
});
