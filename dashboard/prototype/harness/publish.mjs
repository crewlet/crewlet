// @ts-check
/**
 * What makes a built variant publishable: the finishing pass `build.mjs`
 * runs on each variant's folder, kept apart from that script so the checks
 * can run it on what they build (`check/layout.mjs`) and hold its rules
 * (`check/publish.test.ts`).
 *
 * The publisher serves a folder as it stands, so everything a page fetches
 * has to be in it under a relative address, and it refuses an XML file that
 * uses DTD machinery. The pass moves each variant's faces into the folder
 * every variant shares, drops the icon nothing asks for, takes the document
 * type line out of every SVG, and checks the shape and every address that is
 * left.
 */

import {
  existsSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { JSDOM } from "jsdom";
import { ENGINE_BASE } from "./vite.mjs";

/** The shared faces, beside the variant folders. */
export const FONTS = "fonts";

/** Where every SVG element lives. */
const SVG_NS = "http://www.w3.org/2000/svg";

/**
 * Every file under `dir`, with its size.
 *
 * @param {string} dir
 * @returns {{ path: string, bytes: number }[]}
 */
export function filesUnder(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [{ path, bytes: statSync(path).size }];
  });
}

/**
 * An SVG the publisher takes: the line declaring its external document type
 * removed and every other byte kept. The brand mark `@crewlethq/icons` ships
 * opens with SVG 1.0's, which names a DTD on w3.org and changes nothing a
 * browser draws.
 *
 * Only a declaration on a line of its own, with no internal subset, is
 * removed; anything else that uses DTD machinery is refused, naming `name`,
 * as is a result that no longer parses as an SVG document.
 *
 * @param {string} text
 * @param {string} name what the file is called in a refusal
 * @returns {{ text: string, removed: boolean }}
 */
export function publishableSvg(text, name) {
  const line = /^[^\S\r\n]*<!DOCTYPE\s[^[>\r\n]*>[^\S\r\n]*\r?\n/m.exec(text);
  const root = text.search(/<[A-Za-z_]/);
  const removable = line !== null && (root < 0 || line.index < root);
  const kept = removable
    ? text.slice(0, line.index) + text.slice(line.index + line[0].length)
    : text;
  if (/<!(DOCTYPE|ENTITY)\b/i.test(kept)) {
    throw new Error(
      `${name} declares a document type or an entity that is not one line of the prolog, which the publisher refuses.`,
    );
  }
  const { DOMParser } = new JSDOM("").window;
  const doc = new DOMParser().parseFromString(kept, "image/svg+xml");
  const svg = doc.documentElement;
  // A parser that fails either replaces the document with a `parsererror`
  // root (jsdom) or keeps what it read and inserts one (a browser), so both
  // are asked: the root is the SVG element, and nothing failed inside it.
  if (
    doc.getElementsByTagName("parsererror").length > 0 ||
    svg?.localName !== "svg" ||
    svg.namespaceURI !== SVG_NS
  ) {
    throw new Error(`${name} does not parse as an SVG document once its document type is removed.`);
  }
  return { text: kept, removed: removable };
}

/**
 * One variant's output, made publishable: its faces moved to the shared
 * folder beside it, the icon nothing references removed, every SVG without
 * its document type line, and its shape and addresses checked. Answers how
 * many document types it removed, which says whether removing them is still
 * needed.
 *
 * @param {string} folder
 * @returns {{ doctypesRemoved: number }}
 */
export function finishVariant(folder) {
  const shared = join(dirname(folder), FONTS);
  const own = join(folder, FONTS);
  if (existsSync(own)) {
    for (const name of readdirSync(own)) {
      const from = join(own, name);
      const to = join(shared, name);
      if (existsSync(to)) {
        // A face's name carries its content hash, so a clash is the same
        // bytes; the licence beside them is the one unhashed name, and two
        // builds with two different licences is a fact to stop on.
        if (!readFileSync(to).equals(readFileSync(from))) {
          throw new Error(`${relative(process.cwd(), from)} differs from the shared ${name}.`);
        }
        rmSync(from);
      } else {
        renameSync(from, to);
      }
    }
    rmSync(own, { recursive: true });
  }
  // The shell names the SVG icon; the .ico is what an engine serves from a
  // route of its own, and nothing in a prototype asks for it.
  rmSync(join(folder, "favicon.ico"), { force: true });

  // NO DTD MACHINERY, which the publisher refuses in any XML file it serves.
  let doctypesRemoved = 0;
  for (const { path } of filesUnder(folder)) {
    const name = relative(process.cwd(), path);
    if (/\.svg$/i.test(path)) {
      const { text, removed } = publishableSvg(readFileSync(path, "utf-8"), name);
      if (removed) {
        writeFileSync(path, text);
        doctypesRemoved += 1;
      }
    } else if (/\.xml$/i.test(path) && /<!(DOCTYPE|ENTITY)\b/i.test(readFileSync(path, "utf-8"))) {
      throw new Error(
        `${name} declares a document type or an entity, which the publisher refuses.`,
      );
    }
  }

  const assets = readdirSync(join(folder, "assets"));
  const scripts = assets.filter((f) => f.endsWith(".js"));
  const sheets = assets.filter((f) => f.endsWith(".css"));
  if (scripts.length !== 1 || sheets.length !== 1 || assets.length !== 2) {
    throw new Error(
      `${folder} should hold one script and one stylesheet, and holds ${assets.join(", ")}.`,
    );
  }
  // NOTHING NAMES THE ENGINE'S OWN BASE, which is a broken URL on any other
  // host: a replacement module copied from a shipped one carries the literal
  // the relative-base rewrite exists for.
  for (const file of ["index.html", ...assets.map((f) => join("assets", f))]) {
    if (readFileSync(join(folder, file), "utf-8").includes(ENGINE_BASE.slice(1))) {
      throw new Error(
        `${join(folder, file)} names ${ENGINE_BASE.slice(1)}, which no host but the engine serves.`,
      );
    }
  }
  // EVERY URL THE SHEET NAMES RESOLVES, the shared faces included.
  const sheet = join(folder, "assets", sheets[0] ?? "");
  for (const match of readFileSync(sheet, "utf-8").matchAll(/url\(\s*["']?([^"')]+)["']?\s*\)/g)) {
    const url = match[1] ?? "";
    if (/^(data:|https?:|#)/.test(url)) continue;
    const target = resolve(dirname(sheet), url.split(/[?#]/)[0] ?? "");
    if (!existsSync(target))
      throw new Error(`${sheet} names ${url}, which the build did not write.`);
  }
  return { doctypesRemoved };
}
