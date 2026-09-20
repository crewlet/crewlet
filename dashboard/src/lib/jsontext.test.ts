/**
 * What a transcript's JSON blocks are allowed to change about a record.
 *
 * The invariant is narrow and it is the whole module: the WHITESPACE between
 * tokens may change and nothing else may. Every case below is one byte a
 * reader is entitled to see unaltered, or one shape the walk has to get right
 * to leave those bytes alone.
 *
 * The `what a re-encode would have lost` block is deliberately wider than the
 * records this engine writes today: two of its cases are live on that path
 * (a number's width and its spelling, both carried this far by Go's
 * json.Number) and three are library contract the engine's own encoder cannot
 * currently reach. They are asserted anyway — a function this one is allowed
 * to be replaced by has to lose nothing — and the module doc says which is
 * which, so nobody reads the three as evidence for a claim about the wire.
 */

import { describe, expect, it } from "vitest";
import { indentJSON } from "./jsontext.ts";

describe("indenting a JSON document", () => {
  it("writes what JSON.stringify writes, for a document that round-trips", () => {
    // The contract a reader depends on without being told it: these blocks
    // look like every other pretty-printed record in the product.
    const value = {
      item: "ENG-1",
      labels: ["a", "b"],
      meta: { deep: { deeper: true } },
      empty: {},
      none: [],
      nothing: null,
    };
    expect(indentJSON(JSON.stringify(value))).toBe(JSON.stringify(value, null, 2));
  });

  it("puts every key on its own line", () => {
    expect(indentJSON('{"a":1,"b":2}')).toBe('{\n  "a": 1,\n  "b": 2\n}');
  });

  it("keeps an empty object and an empty array on one line", () => {
    // Three lines to say "none" is three lines a reader has to read.
    expect(indentJSON('{"tags":[],"meta":{}}')).toBe('{\n  "tags": [],\n  "meta": {}\n}');
    expect(indentJSON("{}")).toBe("{}");
    expect(indentJSON("[ ]")).toBe("[]");
  });

  it("indents an array of objects", () => {
    expect(indentJSON('[{"a":1},{"b":2}]')).toBe(
      '[\n  {\n    "a": 1\n  },\n  {\n    "b": 2\n  }\n]',
    );
  });
});

describe("what a re-encode would have lost", () => {
  // Each of these is a byte the screen is supposed to be showing, and each is
  // a byte `JSON.stringify(JSON.parse(text), null, 2)` changes. They are the
  // reason this walks the text instead.

  it("keeps a number wider than 2^53 exactly as the model wrote it", () => {
    // Decoded and re-encoded this reads 9007199254740992 — an id off by one,
    // on the screen whose job is to say which object was acted on.
    expect(indentJSON('{"id":9007199254740993}')).toBe('{\n  "id": 9007199254740993\n}');
  });

  it("keeps a number's spelling", () => {
    expect(indentJSON('{"a":1.0,"b":1e3,"c":-0.0,"d":1E+2}')).toBe(
      '{\n  "a": 1.0,\n  "b": 1e3,\n  "c": -0.0,\n  "d": 1E+2\n}',
    );
  });

  it("keeps a duplicate key, which is the malformed call somebody opened this to see", () => {
    expect(indentJSON('{"id":1,"id":2}')).toBe('{\n  "id": 1,\n  "id": 2\n}');
  });

  it("keeps an escape unresolved", () => {
    expect(indentJSON('{"a":"\\u00e9\\n"}')).toBe('{\n  "a": "\\u00e9\\n"\n}');
  });

  it("keeps key order when the keys look like integers", () => {
    // A JavaScript object sorts these ahead of everything else, ascending,
    // whatever the wire said.
    expect(indentJSON('{"2":"b","1":"a","x":"c"}')).toBe(
      '{\n  "2": "b",\n  "1": "a",\n  "x": "c"\n}',
    );
  });
});

describe("a string is copied whole", () => {
  it("does not read structure out of prose", () => {
    expect(indentJSON('{"body":"a { b, c: d [ e"}')).toBe('{\n  "body": "a { b, c: d [ e"\n}');
  });

  it("ends a string at the quote that closes it, not at an escaped one", () => {
    // The comma inside the string is the half that matters: a scan that
    // ended the string at the escaped quote would indent on it.
    expect(indentJSON('{"a":"x\\", y","b":2}')).toBe('{\n  "a": "x\\", y",\n  "b": 2\n}');
    expect(indentJSON('{"a":"x\\\\","b":2}')).toBe('{\n  "a": "x\\\\",\n  "b": 2\n}');
  });
});

describe("text that is not a JSON document", () => {
  it("comes back exactly as it arrived", () => {
    // A provider that answered with prose, or a record cut short, is a
    // reading the transcript has to be able to show.
    const broken = '{"a":1,';
    expect(indentJSON(broken)).toBe(broken);
    expect(indentJSON("not json at all")).toBe("not json at all");
    expect(indentJSON("")).toBe("");
    expect(indentJSON("   ")).toBe("   ");
  });

  it("leaves a bare scalar alone", () => {
    expect(indentJSON('"a sentence"')).toBe('"a sentence"');
    expect(indentJSON("42")).toBe("42");
    expect(indentJSON("null")).toBe("null");
    expect(indentJSON("true")).toBe("true");
  });
});

describe("idempotence", () => {
  it("lands already-indented text on the same shape", () => {
    // One house style, whether the producer sent wire text or the client
    // stringified a decoded map at four spaces.
    const once = indentJSON('{"a":[1,2],"b":{"c":3}}');
    expect(indentJSON(once)).toBe(once);
    expect(indentJSON(JSON.stringify({ a: [1, 2], b: { c: 3 } }, null, 4))).toBe(once);
    expect(indentJSON('{\n\t"a"\t:\t1\n}')).toBe('{\n  "a": 1\n}');
  });
});

describe("documents a transcript actually carries", () => {
  it("handles a large body without cutting it", () => {
    const body = "x".repeat(50_000);
    const out = indentJSON(JSON.stringify({ body }));
    expect(out).toBe(`{\n  "body": "${body}"\n}`);
  });
});

describe("the depth the indent stops growing at", () => {
  function nest(depth: number): string {
    let doc = "1";
    for (let i = 0; i < depth; i++) doc = `{"a":${doc}}`;
    return doc;
  }

  // THE CONTROL, and it is the one that matters: without it the clamp could be
  // 0 — "indenting is off" — and every assertion below would still pass.
  it("indents a document inside the ceiling exactly as JSON.stringify does", () => {
    const doc = nest(31);
    expect(indentJSON(doc)).toBe(JSON.stringify(JSON.parse(doc), null, 2));
  });

  it("stops adding margin past it, and keeps the newlines", () => {
    const out = indentJSON(nest(40));
    const deepest = out.split("\n").find((line) => line.trim() === '"a": 1')!;
    // 32 levels of two spaces, and not the 40 the document nests to.
    expect(deepest).toBe(" ".repeat(64) + '"a": 1');
  });

  // WHY THE CLAMP IS THERE AT ALL. Indentation is quadratic in nesting depth
  // and the depth is a third party's to choose — a 10 kB record 5000 deep
  // asks for fifty megabytes on the main thread unclamped. Bounded, the
  // output stays within a small factor of the input.
  it("keeps a deeply nested document within a constant factor of its input", () => {
    const doc = nest(5_000);
    expect(indentJSON(doc).length).toBeLessThan(doc.length * 40);
  });

  it("does not blow the stack on one, either", () => {
    // The scan is a loop with a counter rather than a recursion, so depth is
    // not a cliff.
    expect(() => indentJSON(nest(5_000))).not.toThrow();
  });
});
