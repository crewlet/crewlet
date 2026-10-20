import { describe, expect, test } from "vitest";
import {
  KIT_RELEASE,
  capabilitiesUsed,
  entryOf,
  fileKind,
  fingerprint,
  readIntake,
  skeleton,
  titleOf,
  withoutCommonFolder,
  type PageFile,
} from "./source.ts";

const text = (path: string, body: string): PageFile => ({ path, kind: "text", text: body });

describe("the page as it is brought", () => {
  // THE SKELETON PASSES THE CHECKS AS IT STANDS: a viewport, a title and the
  // kit's theming at the release this dashboard ships.
  test("the skeleton links the shipped kit release and names itself", () => {
    const page = skeleton();
    expect(page).toContain(`/_crewlet/kit/${KIT_RELEASE}/kit.css`);
    expect(page).toContain(`/_crewlet/kit/${KIT_RELEASE}/theme.js`);
    expect(page).toContain('name="viewport"');
    expect(titleOf(page)).toBe("Untitled page");
  });

  test("the title is read as a reader sees it: references decoded, whitespace collapsed", () => {
    expect(titleOf("<title>\n  Q3 &amp; Q4 &#8212; plan&nbsp;\n</title>")).toBe("Q3 & Q4 — plan");
    expect(titleOf("<TITLE data-x>Hi</TITLE >")).toBe("Hi");
    expect(titleOf("<p>No title</p>")).toBe("");
    expect(titleOf("<title>&bogus; &#x110000;</title>")).toBe("&bogus; &#x110000;");
  });

  test("the capabilities a script reaches for are read off every text file", () => {
    const used = capabilitiesUsed([
      text("index.html", "<script>crewlet.data.collection('votes')</script>"),
      text("app.js", "await crewlet .presence.join(); crewlet.identity.self()"),
      { path: "logo.png", kind: "bytes", blob: new Blob(["crewlet.state"]) },
    ]);
    expect([...used].sort()).toEqual(["db", "identity", "presence"]);
  });

  test("the page is index.html, else the shallowest page by name", () => {
    expect(entryOf([text("b.html", ""), text("index.html", "")])).toBe("index.html");
    expect(entryOf([text("docs/a.html", ""), text("z.htm", "")])).toBe("z.htm");
    expect(entryOf([text("app.css", "")])).toBe("");
  });

  test("a folder brought whole is the page: its one folder is dropped from every path", () => {
    expect(withoutCommonFolder(["site/index.html", "site/css/a.css"])).toEqual([
      "index.html",
      "css/a.css",
    ]);
    expect(withoutCommonFolder(["index.html", "css/a.css"])).toEqual(["index.html", "css/a.css"]);
    expect(withoutCommonFolder(["a/x.html", "b/y.html"])).toEqual(["a/x.html", "b/y.html"]);
  });

  test("what a page cannot hold is left out and named; text is read, bytes kept", async () => {
    const intake = await readIntake([
      { path: "site/index.html", file: new File(["<p>hi</p>"], "index.html") },
      { path: "site/logo.png", file: new File([new Uint8Array([1, 2])], "logo.png") },
      { path: "site/notes.docx", file: new File(["x"], "notes.docx") },
    ]);
    expect(intake.files.map((f) => [f.path, f.kind])).toEqual([
      ["index.html", "text"],
      ["logo.png", "bytes"],
    ]);
    expect(intake.skipped).toEqual(["notes.docx"]);
    expect(fileKind("A.SVG")).toBe("text");
    expect(fileKind("README")).toBeNull();
  });

  test("the fingerprint moves with the text and the capabilities, not their order", () => {
    const files = [text("index.html", "a")];
    expect(fingerprint(files, ["db", "state"])).toBe(fingerprint(files, ["state", "db"]));
    expect(fingerprint(files, ["db"])).not.toBe(fingerprint(files, []));
    expect(fingerprint([text("index.html", "b")], [])).not.toBe(fingerprint(files, []));
  });
});
