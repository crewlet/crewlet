/**
 * What the syntax suites share: the contract every grammar's answer keeps,
 * and one file of real shape a language, read by the suites and the bench.
 */
import type { Lang } from "~/lib/syntax/index.ts";
import { KINDS, type Line } from "~/lib/syntax/types.ts";

/** The longest state a grammar may leave. */
export const MAX_STATE = 64;

const high = (c: number) => c >= 0xd800 && c <= 0xdbff;
const low = (c: number) => c >= 0xdc00 && c <= 0xdfff;

/** How an answer for `text` breaks the contract (cover, canonical, bounded state); empty when it keeps it. */
export function contractBreaks(text: string, answer: Line): string[] {
  const out: string[] = [];
  const { tokens, state } = answer;
  let at = 0;
  for (let k = 0; k < tokens.length; k++) {
    const { end, kind } = tokens[k]!;
    if (!Number.isInteger(end) || end <= at) out.push(`token ${k} ends at ${end}, after ${at}`);
    if (kind !== null && !KINDS.includes(kind)) out.push(`token ${k} is of kind ${kind}`);
    if (k > 0 && tokens[k - 1]!.kind === kind)
      out.push(`tokens ${k - 1} and ${k} are both ${kind}`);
    if (end < text.length && high(text.charCodeAt(end - 1)) && low(text.charCodeAt(end))) {
      out.push(`token ${k} ends inside a surrogate pair at ${end}`);
    }
    at = end;
  }
  if (at !== text.length) out.push(`the tokens cover ${at} of ${text.length}`);
  if (typeof state !== "string" || state.length > MAX_STATE) {
    out.push(`the state is ${JSON.stringify(state).slice(0, 80)}`);
  }
  return out;
}

/** A file of real shape in each language. */
export const SAMPLES: Readonly<Record<Lang, string>> = {
  go: `// Package web serves the site.
package web

import (
\t"fmt"
\t"net/http"
)

/* Routes lists every path the site answers,
   one handler a path. */
type Routes map[string]http.HandlerFunc

const usage = \`web serves the site.

Usage: web [-addr :8080]\`

func (r Routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
\th, ok := r[req.URL.Path]
\tif !ok {
\t\thttp.NotFound(w, req)
\t\treturn
\t}
\th(w, req)
\tfmt.Fprintf(w, "%d bytes\\n", 0x1F+len(usage))
}
`,
  gomod: `module example.com/site

go 1.26

require (
\tgolang.org/x/net v0.30.0
\tgithub.com/example/lib v1.2.3-0.20240101000000-abcdef123456 // indirect
)

replace example.com/old => ../old
`,
  ts: `import { useState, type ReactNode } from "react";

/** A counter with a label. */
export function Counter({ label }: { label: string }): ReactNode {
  const [n, setN] = useState(0);
  const text = \`\${label}: \${n > 1 ? \`\${n} times\` : "once"}\`;
  const parts = text.split(/[:,]\\s*/g).length / 2;
  return (
    <button type="button" onClick={() => setN(n + 1)} data-parts={parts}>
      {text}
    </button>
  );
}

class Store<T> extends Map<string, T> implements Iterable<[string, T]> {
  /* the default size,
     read once */
  static size = 1_000n;
}
`,
  python: `import os

@dataclass
class Config:
    """A configuration.

    Read once at start."""
    path: str = r"C:\\temp"

    def load(self, name: str) -> dict:
        # read "the file" once
        with open(os.path.join(self.path, name)) as f:
            return {"name": name, "size": 0x10, "text": f"{name}!"}
`,
  rust: `use std::fmt;

/// A point on the plane.
#[derive(Debug, Clone)]
pub struct Point<'a> {
    name: &'a str,
    x: f64,
}

/* nested /* block */ comment */
impl<'a> fmt::Display for Point<'a> {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let raw = r#"a "quoted" word"#;
        write!(f, "{} {} {}", self.name, self.x + 1.5f64, raw)?;
        Ok(())
    }
}
`,
  shell: `#!/bin/sh
set -eu

# Deploy the site.
deploy() {
  local target="\${1:-staging}"
  echo "deploying to $target" >&2
  for host in $(cat hosts); do
    ssh "$host" 'systemctl restart site' || exit 1
  done
}

cat <<-EOF > /tmp/notes
\tDeployed at $(date).
\tEOF
deploy "$@"
`,
  makefile: `# Build the site.
GO ?= go
BIN := bin/site

.PHONY: all test
all: $(BIN)

$(BIN): $(wildcard *.go)
\t$(GO) build -o $@ ./cmd/site
\techo "built $@" # done

define HELP
make all   builds the site
endef

test:
\t$(GO) test ./... \\
\t\t-race
`,
  dockerfile: `# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN go build -o /out/site ./cmd/site && \\
    strip /out/site
RUN <<EOF
echo "built"
EOF

FROM gcr.io/distroless/base
COPY --from=build /out/site /site
ENV PORT=8080
ENTRYPOINT ["/site", "-addr", ":$PORT"]
`,
  yaml: `# Continuous integration.
name: ci
on:
  push:
    branches: [main]
env:
  GOFLAGS: "-mod=readonly # not a comment"
  RETRIES: 3
jobs:
  test: &test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: |
          go test ./...
          echo done
      - name: lint
        run: make lint
  race: *test
`,
  json: `{
  // the project's settings
  "name": "site",
  "version": "1.0.0",
  "private": true,
  /* scripts run by
     the package manager */
  "scripts": { "build": "vite build", "test": null },
  "files": ["dist", 3, -1.5e3]
}
`,
  toml: `# The package.
[package]
name = "site"
version = "0.1.0"
released = 1979-05-27T07:32:00Z

[[bin]]
name = "site"
path = "src/main.rs"

[dependencies.serde]
features = ["derive"]
description = """
A long
description."""
`,
  css: `/* The page's layout,
   read once. */
@media (width > 40em) {
  .page > main #content:hover {
    color: #fff !important;
    margin: 0 auto 1.5rem;
    background: url(img/bg.png) no-repeat;
  }
}

a::after {
  content: "\\2192";
}
`,
  markup: `<!DOCTYPE html>
<html lang="en">
<head>
  <!-- the page's head,
       kept short -->
  <title>Site &amp; more</title>
  <style>body { margin: 0 }</style>
</head>
<body class="page"
      data-theme="dark">
  <p>Hello &#169; <a href="/about">about</a></p>
  <script>if (a < b) { go(); }</script>
  <svg><![CDATA[ raw
  text ]]></svg>
</body>
</html>
`,
  sql: `-- The accounts table.
CREATE TABLE accounts (
  id BIGINT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT 'it''s',
  created timestamp
);

/* a function,
   in plpgsql */
CREATE FUNCTION touch() RETURNS trigger AS $$
BEGIN
  NEW.created := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

SELECT count(*) FROM accounts WHERE id > 10;
`,
  proto: `syntax = "proto3";

package site.v1;

/* The site's service,
   one call a page. */
service Pages {
  rpc Get(GetRequest) returns (Page);
}

message Page {
  string path = 1;
  repeated string tags = 2;
  map<string, int64> counts = 3;
}
`,
  hcl: `# The site's bucket.
terraform {
  required_version = ">= 1.6"
}

resource "aws_s3_bucket" "site" {
  bucket = "site-\${var.env}"
  tags   = { Name = "site", Count = 3 }
}

/* a policy,
   inline */
locals {
  policy = <<-EOT
    {"Version": "2012-10-17"}
  EOT
}
`,
  markdown: `# Site

A small site, served by \`web\` ([docs](https://example.com/docs)).

## Build

\`\`\`sh
make all
\`\`\`

- one
- two

> A quote.

<!-- a comment
     across lines -->

| a | b |
| - | - |
`,
};
