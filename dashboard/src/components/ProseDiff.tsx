/**
 * A change to a page, drawn as the page reads (`lib/proseDiff.ts`): each block
 * rendered by the dashboard's markdown renderer, its changed words marked with
 * the design system's `DocumentDiffAdded` and `DocumentDiffRemoved`, inside
 * its `DocumentDiff`. Nothing that was removed is a live link.
 */

import { Fragment, type ReactNode } from "react";
import {
  DocumentDiff,
  DocumentDiffAdded,
  DocumentDiffRemoved,
  type DocumentDiffBlock,
} from "@crewlethq/ui";
import { renderBlocks, renderInline, type InlineOptions } from "~/lib/markdown.ts";
import type { CodeLine, PreviewEntry, ProseBlock, WordPart } from "~/lib/proseDiff.ts";

export function ProseDiff({
  entries,
  "aria-label": label,
}: {
  entries: readonly PreviewEntry[];
  "aria-label": string;
}) {
  const blocks: DocumentDiffBlock[] = entries.map((entry, i) =>
    entry.kind === "skipped"
      ? { change: "skipped", count: entry.count, of: entry.of, key: i }
      : { change: entry.block.change, content: drawn(entry.block, `d${i}-`), key: i },
  );
  return <DocumentDiff blocks={blocks} aria-label={label} />;
}

function drawn(block: ProseBlock, key: string): ReactNode {
  if (block.change !== "changed") {
    return renderBlocks(
      [block.block],
      { headings: "text", links: block.change === "removed" ? "text" : "anchor" },
      key,
    );
  }
  if (block.code) {
    return (
      <pre
        className="md-code"
        data-lang={block.block.kind === "code" ? block.block.lang || undefined : undefined}
      >
        <code>{block.code.map((line, i) => codeLine(line, `${key}c${i}`))}</code>
      </pre>
    );
  }
  let next = 0;
  return renderBlocks(
    [block.block],
    {
      headings: "text",
      inline: (_text, at, options) => words(block.runs[next++] ?? [], at, options),
    },
    key,
  );
}

function words(parts: readonly WordPart[], key: string, options: InlineOptions): ReactNode {
  return parts.map((part, i) => {
    const at = `${key}-w${i}`;
    if (part.op === "=")
      return <Fragment key={at}>{renderInline(part.text, at, options)}</Fragment>;
    if (part.op === "+") {
      return <DocumentDiffAdded key={at}>{renderInline(part.text, at, options)}</DocumentDiffAdded>;
    }
    return (
      <DocumentDiffRemoved key={at}>
        {renderInline(part.text, at, { ...options, links: "text" })}
      </DocumentDiffRemoved>
    );
  });
}

function codeLine(line: CodeLine, key: string): ReactNode {
  const text = `${line.text}\n`;
  if (line.op === "+") return <DocumentDiffAdded key={key}>{text}</DocumentDiffAdded>;
  if (line.op === "-") return <DocumentDiffRemoved key={key}>{text}</DocumentDiffRemoved>;
  return <Fragment key={key}>{text}</Fragment>;
}
