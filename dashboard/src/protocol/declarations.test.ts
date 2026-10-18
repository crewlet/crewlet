// @vitest-environment node

/**
 * The wire declares each of its shapes ONCE.
 *
 * `protocol/` and `contract/` are the client half of the engine's protocol,
 * and every shape in them is imported by name — so two modules declaring one
 * name is two shapes a screen can reach for, each compiling, each agreeing
 * with whichever file it came from. Structural typing hides it while the two
 * are identical and keeps hiding it the day one gains a field: a value built
 * from one passes where the other is asked for until the field is read. It
 * happened once already — a log position declared beside the chat answers and
 * again beside the session floor that parses the same point off a write's
 * answer.
 *
 * INTERFACES AND TYPE ALIASES, because those are what structural typing lets
 * two copies of pass for one another without a word from the compiler.
 */

import { expect, test } from "vitest";

import { modules, parse, type Node } from "../test/source.ts";

test("no interface or type alias is declared twice across the wire's modules", () => {
  const where = new Map<string, string[]>();
  let walked = 0;
  for (const { path, text, lang } of modules()) {
    if (!path.startsWith("protocol/") && !path.startsWith("contract/")) continue;
    walked++;
    for (const statement of parse(text, lang).body as Node[]) {
      const declared =
        statement.type === "ExportNamedDeclaration"
          ? (statement.declaration as Node | null)
          : statement;
      if (
        !declared ||
        (declared.type !== "TSInterfaceDeclaration" && declared.type !== "TSTypeAliasDeclaration")
      ) {
        continue;
      }
      const name = (declared.id as { name: string }).name;
      where.set(name, [...(where.get(name) ?? []), path]);
    }
  }
  // NOT VACUOUS: the walk read the wire, and found the shapes it is made of.
  expect(walked).toBeGreaterThan(20);
  expect(where.size).toBeGreaterThan(200);
  const twice = [...where]
    .filter(([, paths]) => paths.length > 1)
    .map(([name, paths]) => `${name}: ${paths.join(", ")}`);
  expect(
    twice,
    "declare the shape once and import it — `types.ts` re-exports what another module declares",
  ).toEqual([]);
});
