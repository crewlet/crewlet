// @vitest-environment node
/**
 * Each grammar against a table of lines a code host reads: consecutive lines,
 * so a state is carried, each drawn as `[kind:text]` runs around plain text.
 */
import { describe, expect, test } from "vitest";
import { contractBreaks } from "~/test/syntax.ts";
import { language, languageOfInfo, load, loaded, tokenize, type Lang } from "./index.ts";
import type { Grammar } from "./types.ts";

/** Each line's tokens as text, the state carried from line to line; each answer keeps the contract. */
function show(g: Grammar, lines: readonly string[]): string[] {
  let state = "";
  return lines.map((line) => {
    const answer = g.line(line, state);
    expect(contractBreaks(line, answer), line).toEqual([]);
    const { tokens, state: next } = answer;
    state = next;
    let at = 0;
    return tokens
      .map((t) => {
        const text = line.slice(at, t.end);
        at = t.end;
        return t.kind ? `[${t.kind}:${text}]` : text;
      })
      .join("");
  });
}

describe("the registry", () => {
  // Mutation: read the first extension rather than the last fails it.
  test("a path's language is its base name's, else its last extension's", () => {
    const table: [string, Lang | null][] = [
      ["Makefile", "makefile"],
      ["sub/GNUmakefile", "makefile"],
      ["Dockerfile", "dockerfile"],
      ["Containerfile", "dockerfile"],
      ["deploy/Dockerfile.dev", "dockerfile"],
      ["go.mod", "gomod"],
      ["go.work", "gomod"],
      ["go.sum", null],
      [".bashrc", "shell"],
      [".env", "shell"],
      ["Cargo.lock", "toml"],
      ["Pipfile", "toml"],
      ["cmd/web/main.go", "go"],
      ["web/App.TSX", "ts"],
      ["x.mjs", "ts"],
      ["tool.pyi", "python"],
      ["src/lib.rs", "rust"],
      ["run.zsh", "shell"],
      ["rules.mk", "makefile"],
      [".github/workflows/ci.yml", "yaml"],
      ["tsconfig.jsonc", "json"],
      ["pyproject.toml", "toml"],
      ["a.css", "css"],
      ["icon.svg", "markup"],
      ["schema.sql", "sql"],
      ["api.proto", "proto"],
      ["main.tf", "hcl"],
      ["README.md", "markdown"],
      ["a/b.c/README", null],
      ["x.ts.orig", null],
      ["notes.txt", null],
      [".gitignore", null],
    ];
    for (const [path, lang] of table) expect(language(path), path).toBe(lang);
  });

  // Mutation: reading the info string whole, or dropping the extension fallback, fails it.
  test("a fence's language is its info string's first word: a name, an alias or an extension", () => {
    const table: [string, Lang | null][] = [
      ["sh", "shell"],
      ["Bash", "shell"],
      ["shell", "shell"],
      ["go", "go"],
      ["golang", "go"],
      ["ts title", "ts"],
      ["javascript", "ts"],
      ["py", "python"],
      ["yml", "yaml"],
      ["Makefile", "makefile"],
      ["dockerfile", "dockerfile"],
      ["terraform", "hcl"],
      ["console", null],
      ["diff", null],
      ["text", null],
      ["", null],
    ];
    for (const [info, lang] of table) expect(languageOfInfo(info), info).toBe(lang);
  });

  // Mutation: a fresh promise per call fails it.
  test("each language loads to a grammar, once, and tokenizes only once loaded", async () => {
    expect(tokenize("proto", "message A {}", "")).toBeNull();
    const langs: Lang[] = [
      "go",
      "gomod",
      "ts",
      "python",
      "rust",
      "shell",
      "makefile",
      "dockerfile",
    ];
    langs.push("yaml", "json", "toml", "css", "markup", "sql", "proto", "hcl", "markdown");
    for (const lang of langs) {
      const first = load(lang);
      expect(load(lang), lang).toBe(first);
      expect(typeof (await first).line, lang).toBe("function");
      expect(loaded(lang), lang).toBe(await first);
    }
    expect(tokenize("proto", "message A {}", "")?.tokens[0]).toEqual({ end: 7, kind: "keyword" });
  });
});

describe("go", () => {
  // Mutation: drop the raw string from `multiline` fails the first case.
  test("a raw string across three lines", async () => {
    expect(show(await load("go"), ["x := `one", "two", "three` + y"])).toEqual([
      "x [punctuation::=] [string:`one]",
      "[string:two]",
      "[string:three`] [punctuation:+] y",
    ]);
  });
  test("a block comment opening mid-line and closing two lines on", async () => {
    expect(show(await load("go"), ["a := 1 /* opens", "inside", "closes */ b := 2"])).toEqual([
      "a [punctuation::=] [number:1] [comment:/* opens]",
      "[comment:inside]",
      "[comment:closes */] b [punctuation::=] [number:2]",
    ]);
  });
  test("a rune holding a quote, then a string", async () => {
    expect(show(await load("go"), ['r := \'"\'; s := "x"'])).toEqual([
      'r [punctuation::=] [string:\'"\'][punctuation:;] s [punctuation::=] [string:"x"]',
    ]);
  });
  test("a method's receiver, name and result", async () => {
    expect(show(await load("go"), ["func (s *T) Name(x int) error {"])).toEqual([
      "[keyword:func] [punctuation:(]s [punctuation:*]T[punctuation:)] [function:Name][punctuation:(]x [type:int][punctuation:)] [type:error] [punctuation:{]",
    ]);
  });
  test("numbers in every base", async () => {
    expect(show(await load("go"), ["n := 0x_FF + 1e9 + 3i + 0o17"])).toEqual([
      "n [punctuation::=] [number:0x_FF] [punctuation:+] [number:1e9] [punctuation:+] [number:3i] [punctuation:+] [number:0o17]",
    ]);
  });
  test("a string inside a comment", async () => {
    expect(show(await load("go"), ['// a "string" in a comment'])).toEqual([
      '[comment:// a "string" in a comment]',
    ]);
  });
  test("a type's name", async () => {
    expect(show(await load("go"), ["type Streamer struct {"])).toEqual([
      "[keyword:type] [type:Streamer] [keyword:struct] [punctuation:{]",
    ]);
  });
});

describe("gomod", () => {
  // Mutation: read a version as plain fails it.
  test("a module, a require block, versions, a replace and a comment", async () => {
    expect(
      show(await load("gomod"), [
        "module example.com/x",
        "",
        "require (",
        "\tgolang.org/x/net v1.2.3-0.20240101000000-abcdef123456 // indirect",
        ")",
        "replace a => ../a",
      ]),
    ).toEqual([
      "[keyword:module] example.com/x",
      "",
      "[keyword:require] [punctuation:(]",
      "\tgolang.org/x/net [number:v1.2.3-0.20240101000000-abcdef123456] [comment:// indirect]",
      "[punctuation:)]",
      "[keyword:replace] a [punctuation:=>] ../a",
    ]);
  });
});

describe("ts", () => {
  // Mutation: read `/` after an operand as a regex fails the second case.
  test("a template across lines whose substitution holds a nested template", async () => {
    expect(
      show(await load("ts"), ["const t = `a ${`b ${x}`} c", "d ${ {k: 1}.k } e", "f`;"]),
    ).toEqual([
      "[keyword:const] t [punctuation:=] [string:`a ][punctuation:${][string:`b ][punctuation:${]x[punctuation:}][string:`][punctuation:}][string: c]",
      "[string:d ][punctuation:${] [punctuation:{]k[punctuation::] [number:1][punctuation:}.]k [punctuation:}][string: e]",
      "[string:f`][punctuation:;]",
    ]);
  });
  test("division, and a regex holding a slash in its class", async () => {
    expect(show(await load("ts"), ["r = a / b / c;", "x = /re\\/g[/]/g;"])).toEqual([
      "r [punctuation:=] a [punctuation:/] b [punctuation:/] c[punctuation:;]",
      "x [punctuation:=] [string:/re\\/g[/]/g][punctuation:;]",
    ]);
  });
  test("a JSX tag with a string and an expression, and its close", async () => {
    expect(show(await load("ts"), ['<Foo bar="x" baz={y}>', "</Foo>"])).toEqual([
      '[punctuation:<][keyword:Foo] [function:bar][punctuation:=][string:"x"] [function:baz][punctuation:={]y[punctuation:}>]',
      "[punctuation:</][keyword:Foo][punctuation:>]",
    ]);
  });
  test("comment openers inside strings", async () => {
    expect(show(await load("ts"), ['const s = "//" + "/*";'])).toEqual([
      '[keyword:const] s [punctuation:=] [string:"//"] [punctuation:+] [string:"/*"][punctuation:;]',
    ]);
  });
  test("a bigint with separators", async () => {
    expect(show(await load("ts"), ["const n = 1_000n;"])).toEqual([
      "[keyword:const] n [punctuation:=] [number:1_000n][punctuation:;]",
    ]);
  });
  test("a class's names", async () => {
    expect(show(await load("ts"), ["class A extends B implements C {}"])).toEqual([
      "[keyword:class] [type:A] [keyword:extends] [type:B] [keyword:implements] [type:C] [punctuation:{}]",
    ]);
  });
  test("a member named like a keyword", async () => {
    expect(show(await load("ts"), ["obj.type = typeof x;"])).toEqual([
      "obj[punctuation:.]type [punctuation:=] [keyword:typeof] x[punctuation:;]",
    ]);
  });
});

describe("python", () => {
  // Mutation: drop the string prefixes' hook fails the first two cases.
  test("a raw triple-quoted string across lines", async () => {
    expect(show(await load("python"), ['s = r"""a "q" \\d', "b", '""" + x'])).toEqual([
      's [punctuation:=] [string:r"""a "q" \\d]',
      "[string:b]",
      '[string:"""] [punctuation:+] x',
    ]);
  });
  test("an f-string", async () => {
    expect(show(await load("python"), ['f"{x}"'])).toEqual(['[string:f"{x}"]']);
  });
  test("a decorator, a definition and a class", async () => {
    expect(show(await load("python"), ["@decorator", "def name(x):", "class Name:"])).toEqual([
      "[function:@decorator]",
      "[keyword:def] [function:name][punctuation:(]x[punctuation:):]",
      "[keyword:class] [type:Name][punctuation::]",
    ]);
  });
  test("a hash inside a string is no comment", async () => {
    expect(show(await load("python"), ['s = "# in a string"  # a comment'])).toEqual([
      's [punctuation:=] [string:"# in a string"]  [comment:# a comment]',
    ]);
  });
});

describe("rust", () => {
  // Mutation: count no hashes in a raw string fails the first case.
  test("a raw string with hashes holding a quote and a hash", async () => {
    expect(show(await load("rust"), ['let s = r##"a "# b"##;'])).toEqual([
      '[keyword:let] s [punctuation:=] [string:r##"a "# b"##][punctuation:;]',
    ]);
  });
  test("nested block comments across lines", async () => {
    expect(show(await load("rust"), ["/* a /* b */", "c */ let x = 1;"])).toEqual([
      "[comment:/* a /* b */]",
      "[comment:c */] [keyword:let] x [punctuation:=] [number:1][punctuation:;]",
    ]);
  });
  test("a lifetime beside a char", async () => {
    expect(show(await load("rust"), ["fn f<'a>(x: &'a str) -> char { 'a' }"])).toEqual([
      "[keyword:fn] [function:f][punctuation:<][keyword:'a][punctuation:>(]x[punctuation::] [punctuation:&][keyword:'a] [type:str][punctuation:)] [punctuation:->] [type:char] [punctuation:{] [string:'a'] [punctuation:}]",
    ]);
  });
  test("a macro and suffixed numbers", async () => {
    expect(show(await load("rust"), ['println!("{}", 0u8 + 1.5f64);'])).toEqual([
      '[function:println][punctuation:!(][string:"{}"][punctuation:,] [number:0u8] [punctuation:+] [number:1.5f64][punctuation:);]',
    ]);
  });
});

describe("shell", () => {
  // Mutation: keep a heredoc's tabs when its `<<-` strips them fails the first case.
  test("a heredoc that strips tabs, closed by a tab-indented delimiter", async () => {
    expect(
      show(await load("shell"), ["cat <<-EOF > out", "\tbody $x", "\tEOF", "echo done"]),
    ).toEqual([
      "cat [punctuation:<<-][string:EOF] [punctuation:>] out",
      "[string:\tbody $x]",
      "[string:\tEOF]",
      "[function:echo] done",
    ]);
  });
  test("a single-quoted string across lines", async () => {
    expect(show(await load("shell"), ["echo 'one", "two' three"])).toEqual([
      "[function:echo] [string:'one]",
      "[string:two'] three",
    ]);
  });
  test("a hash inside a word, and one starting a comment", async () => {
    expect(show(await load("shell"), ["echo a#b #c"])).toEqual([
      "[function:echo] a#b [comment:#c]",
    ]);
  });
  test("variables and a function's definition", async () => {
    expect(show(await load("shell"), ['echo "${HOME}" $1 ${PATH}', "f() {"])).toEqual([
      '[function:echo] [string:"${HOME}"] [type:$1] [type:${PATH}]',
      "[function:f][punctuation:()] [punctuation:{]",
    ]);
  });
});

describe("makefile", () => {
  // Mutation: read a tab's line as a rule fails the first case.
  test("a recipe line read as shell", async () => {
    expect(show(await load("makefile"), ["build: dep", '\techo "$(VERSION)" $@'])).toEqual([
      "[function:build][punctuation::] dep",
      '\t[function:echo] [string:"$(VERSION)"] [type:$@]',
    ]);
  });
  test("a define block", async () => {
    expect(show(await load("makefile"), ["define banner", "  echo hi", "endef"])).toEqual([
      "[keyword:define] banner",
      "[string:  echo hi]",
      "[keyword:endef]",
    ]);
  });
  test("assignments", async () => {
    expect(show(await load("makefile"), ["X ?= y", "Y := $(X) z"])).toEqual([
      "[type:X] [punctuation:?=] y",
      "[type:Y] [punctuation::=] [type:$(X)] z",
    ]);
  });
});

describe("dockerfile", () => {
  // Mutation: drop the shell's state from `r` fails the second case.
  test("a stage's name", async () => {
    expect(show(await load("dockerfile"), ["FROM golang:1.26 AS build"])).toEqual([
      "[keyword:FROM] golang:1.26 [keyword:AS] build",
    ]);
  });
  test("a RUN heredoc across lines", async () => {
    expect(
      show(await load("dockerfile"), ["RUN <<END", "apt-get install -y x", "END", "WORKDIR /src"]),
    ).toEqual([
      "[keyword:RUN] [punctuation:<<][string:END]",
      "[string:apt-get install -y x]",
      "[string:END]",
      "[keyword:WORKDIR] /src",
    ]);
  });
  test("a continued RUN", async () => {
    expect(show(await load("dockerfile"), ["RUN go build && \\", "    echo $HOME"])).toEqual([
      "[keyword:RUN] go build [punctuation:&&] \\",
      "    [function:echo] [type:$HOME]",
    ]);
  });
  test("a parser directive", async () => {
    expect(show(await load("dockerfile"), ["# syntax=docker/dockerfile:1"])).toEqual([
      "[comment:# syntax=docker/dockerfile:1]",
    ]);
  });
});

describe("yaml", () => {
  // Mutation: end a block scalar at a blank line fails the third case.
  test("a quoted value holding a hash", async () => {
    expect(show(await load("yaml"), ['key: "a # b"'])).toEqual([
      '[function:key][punctuation::] [string:"a # b"]',
    ]);
  });
  test("a list item's key", async () => {
    expect(show(await load("yaml"), ["- key: v"])).toEqual([
      "[punctuation:-] [function:key][punctuation::] v",
    ]);
  });
  test("a block scalar ending at the dedent", async () => {
    expect(show(await load("yaml"), ["run: |", "  line one", "", "  line two", "next: 1"])).toEqual(
      [
        "[function:run][punctuation::] [punctuation:|]",
        "[string:  line one]",
        "",
        "[string:  line two]",
        "[function:next][punctuation::] [number:1]",
      ],
    );
  });
  test("an anchor and an alias", async () => {
    expect(show(await load("yaml"), ["a: &anchor 1", "b: *anchor"])).toEqual([
      "[function:a][punctuation::] [type:&anchor] [number:1]",
      "[function:b][punctuation::] [type:*anchor]",
    ]);
  });
  test("a document marker and a comment", async () => {
    expect(show(await load("yaml"), ["--- # doc"])).toEqual(["[punctuation:---] [comment:# doc]"]);
  });
});

describe("json", () => {
  // Mutation: read a key as a value fails the first case.
  test("keys, values and constants", async () => {
    expect(show(await load("json"), ['{ "a": 1, "b": [true, null, -2.5e3] }'])).toEqual([
      '[punctuation:{] [function:"a"][punctuation::] [number:1][punctuation:,] [function:"b"][punctuation::] [punctuation:[][keyword:true][punctuation:,] [keyword:null][punctuation:,] [number:-2.5e3][punctuation:]] [punctuation:}]',
    ]);
  });
  test("comments, one across lines", async () => {
    expect(show(await load("json"), ["// c", "{ /* a", ' b */ "k": "v" }'])).toEqual([
      "[comment:// c]",
      "[punctuation:{] [comment:/* a]",
      '[comment: b */] [function:"k"][punctuation::] [string:"v"] [punctuation:}]',
    ]);
  });
});

describe("toml", () => {
  // Mutation: drop `D` from the state fails the second case.
  test("table headers", async () => {
    expect(show(await load("toml"), ["[[bin]]", "[a.b]"])).toEqual([
      "[punctuation:[[][type:bin][punctuation:]]]",
      "[punctuation:[][type:a][punctuation:.][type:b][punctuation:]]",
    ]);
  });
  test("a multi-line string across lines", async () => {
    expect(show(await load("toml"), ['s = """', "a", 'b"""'])).toEqual([
      '[function:s] [punctuation:=] [string:"""]',
      "[string:a]",
      '[string:b"""]',
    ]);
  });
  test("a date and a constant", async () => {
    expect(show(await load("toml"), ["d = 1979-05-27T07:32:00Z", "x = true"])).toEqual([
      "[function:d] [punctuation:=] [number:1979-05-27T07:32:00Z]",
      "[function:x] [punctuation:=] [keyword:true]",
    ]);
  });
});

describe("css", () => {
  // Mutation: read `#fff` as an id fails the third case.
  test("a comment across lines", async () => {
    expect(show(await load("css"), ["/* a", "b */ a {}"])).toEqual([
      "[comment:/* a]",
      "[comment:b */] [keyword:a] [punctuation:{}]",
    ]);
  });
  test("selectors", async () => {
    expect(show(await load("css"), [".a #b > c:hover {"])).toEqual([
      "[type:.a] [type:#b] [punctuation:>] [keyword:c][punctuation::]hover [punctuation:{]",
    ]);
  });
  test("a colour and an important", async () => {
    expect(show(await load("css"), ["a { color: #fff !important; }"])).toEqual([
      "[keyword:a] [punctuation:{] [function:color][punctuation::] [number:#fff] [keyword:!important][punctuation:;] [punctuation:}]",
    ]);
  });
  test("an at-rule's condition", async () => {
    expect(show(await load("css"), ["@media (width > 40em) {"])).toEqual([
      "[keyword:@media] [punctuation:(]width [punctuation:>] [number:40em][punctuation:)] [punctuation:{]",
    ]);
  });
  test("a url", async () => {
    expect(show(await load("css"), ["a { background: url(x.png); }"])).toEqual([
      "[keyword:a] [punctuation:{] [function:background][punctuation::] [function:url][punctuation:(][string:x.png][punctuation:);] [punctuation:}]",
    ]);
  });
});

describe("markup", () => {
  // Mutation: read a script's body as markup fails the fourth case.
  test("a comment across lines", async () => {
    expect(show(await load("markup"), ["<!-- a", "b --> <p>"])).toEqual([
      "[comment:<!-- a]",
      "[comment:b -->] [punctuation:<][keyword:p][punctuation:>]",
    ]);
  });
  test("an attribute value across lines", async () => {
    expect(show(await load("markup"), ['<a title="one', 'two" href=x>'])).toEqual([
      '[punctuation:<][keyword:a] [function:title][punctuation:=][string:"one]',
      '[string:two"] [function:href][punctuation:=][string:x][punctuation:>]',
    ]);
  });
  test("CDATA across lines", async () => {
    expect(show(await load("markup"), ["<![CDATA[ a < b", "]]> <p>"])).toEqual([
      "[punctuation:<![CDATA[][string: a < b]",
      "[string:]]>] [punctuation:<][keyword:p][punctuation:>]",
    ]);
  });
  test("a script's body is text", async () => {
    expect(show(await load("markup"), ["<script>if (a <b) {}</script>"])).toEqual([
      "[punctuation:<][keyword:script][punctuation:>]if (a <b) {}[punctuation:</][keyword:script][punctuation:>]",
    ]);
  });
  test("an entity", async () => {
    expect(show(await load("markup"), ["a &amp; b"])).toEqual(["a [number:&amp;] b"]);
  });
});

describe("sql", () => {
  // Mutation: close a `'` string at its doubled quote fails the first case.
  test("a doubled quote", async () => {
    expect(show(await load("sql"), ["SELECT 'it''s'"])).toEqual([
      "[keyword:SELECT] [string:'it''s']",
    ]);
  });
  test("a dollar-quoted body across lines", async () => {
    expect(show(await load("sql"), ["AS $$ BEGIN", "RETURN 1; END $$;"])).toEqual([
      "[keyword:AS] [string:$$ BEGIN]",
      "[string:RETURN 1; END $$][punctuation:;]",
    ]);
  });
  test("a comment, a table and a call", async () => {
    expect(
      show(await load("sql"), [
        "-- comment",
        "CREATE TABLE t (id BIGINT)",
        "select count(*) from t",
      ]),
    ).toEqual([
      "[comment:-- comment]",
      "[keyword:CREATE] [keyword:TABLE] [type:t] [punctuation:(]id [type:BIGINT][punctuation:)]",
      "[keyword:select] [function:count][punctuation:(*)] [keyword:from] t",
    ]);
  });
});

describe("proto", () => {
  // Mutation: drop `capitalTypes` fails it.
  test("a message, a service and a comment", async () => {
    expect(
      show(await load("proto"), [
        "message A {",
        "  repeated string x = 1;",
        "}",
        "service S { rpc Get(Req) returns (Res); } /* c */",
      ]),
    ).toEqual([
      "[keyword:message] [type:A] [punctuation:{]",
      "  [keyword:repeated] [type:string] x [punctuation:=] [number:1][punctuation:;]",
      "[punctuation:}]",
      "[keyword:service] [type:S] [punctuation:{] [keyword:rpc] [function:Get][punctuation:(][type:Req][punctuation:)] [keyword:returns] [punctuation:(][type:Res][punctuation:);] [punctuation:}] [comment:/* c */]",
    ]);
  });
});

describe("hcl", () => {
  // Mutation: close a string at a quote inside `${...}` fails the second case.
  test("a resource's block", async () => {
    expect(show(await load("hcl"), ['resource "aws_s3_bucket" "b" {'])).toEqual([
      '[keyword:resource] [string:"aws_s3_bucket"] [string:"b"] [punctuation:{]',
    ]);
  });
  test("an interpolation holding quotes", async () => {
    expect(show(await load("hcl"), ['  name = "x-${var.y == "a" ? 1 : 2}"'])).toEqual([
      '  [function:name] [punctuation:=] [string:"x-${var.y == "a" ? 1 : 2}"]',
    ]);
  });
  test("a heredoc across lines", async () => {
    expect(
      show(await load("hcl"), ["  policy = <<-EOT", '    {"a": 1}', "  EOT", "  count = 1"]),
    ).toEqual([
      "  [function:policy] [punctuation:=] [punctuation:<<-][string:EOT]",
      '[string:    {"a": 1}]',
      "[string:  EOT]",
      "  [function:count] [punctuation:=] [number:1]",
    ]);
  });
  test("both comments", async () => {
    expect(show(await load("hcl"), ["# c", "// c"])).toEqual(["[comment:# c]", "[comment:// c]"]);
  });
});

describe("markdown", () => {
  // Mutation: close a fence with any longer-or-shorter run fails the first case.
  test("a fence of four backticks is not closed by three", async () => {
    expect(show(await load("markdown"), ["````go", "```", "x", "````", "after"])).toEqual([
      "[punctuation:````]go",
      "[string:```]",
      "[string:x]",
      "[punctuation:````]",
      "after",
    ]);
  });
  test("a tilde fence", async () => {
    expect(show(await load("markdown"), ["~~~", "x", "~~~"])).toEqual([
      "[punctuation:~~~]",
      "[string:x]",
      "[punctuation:~~~]",
    ]);
  });
  test("a heading, inline code and a link", async () => {
    expect(show(await load("markdown"), ["# Heading", "a `code` and [a](https://x)"])).toEqual([
      "[keyword:# Heading]",
      "a [string:`code`] and [punctuation:[]a[punctuation:](][string:https://x][punctuation:)]",
    ]);
  });
  test("a list and a quote", async () => {
    expect(show(await load("markdown"), ["- item", "> quote"])).toEqual([
      "[punctuation:-] item",
      "[punctuation:>] quote",
    ]);
  });
});
