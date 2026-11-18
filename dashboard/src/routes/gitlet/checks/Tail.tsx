/** The end of a run's output: numbered from its place in the whole output, its failing lines tinted. */

import { useLayoutEffect, useRef, useState } from "react";
import { CodeBlock, CopyButton } from "@crewlethq/ui";
import type { CodeCheckDetail } from "~/protocol/index.ts";
import { failingLines, tailRangeWords } from "./standing.ts";

const LABEL = "The end of its output";

export function Tail({ d, lines }: { d: CodeCheckDetail; lines: readonly string[] }) {
  const range = tailRangeWords(d, lines.length);
  return (
    <div className="col gap-1">
      <div className="code-check-tail__head">
        <h3 className="code-check-pane__part">{LABEL}</h3>
        {range && <span className="t-caption code-check-tail__range">{range}</span>}
        <CopyButton text={d.tail ?? ""} title="Copy the end of its output" />
      </div>
      {range ? (
        <Numbered d={d} lines={lines} />
      ) : (
        // AN OLDER BUILD COUNTS NO LINES: the tail as it is, unnumbered.
        <CodeBlock plain selectable focusWhenScrollable label={LABEL} code={d.tail ?? ""} />
      )}
    </div>
  );
}

function Numbered({ d, lines }: { d: CodeCheckDetail; lines: readonly string[] }) {
  const box = useRef<HTMLDivElement>(null);
  // A TAB STOP ONLY WHILE IT SCROLLS SIDEWAYS, so a keyboard can reach what is past its edge.
  const [scrolls, setScrolls] = useState(false);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return undefined;
    const measure = () => setScrolls(el.scrollWidth > el.clientWidth);
    measure();
    if (typeof ResizeObserver === "undefined") return undefined;
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  }, [lines]);
  const failing = failingLines(lines, d.failures ?? []);
  const first = d.tail_first_line ?? 1;
  return (
    <div
      ref={box}
      className="code-check-tail"
      role="region"
      aria-label={LABEL}
      tabIndex={scrolls ? 0 : undefined}
    >
      <pre>
        <code>
          {lines.map((line, i) => (
            <span
              key={i}
              className="code-check-tail__line"
              data-fail={failing.has(i) ? "" : undefined}
            >
              <span className="code-check-tail__n" aria-hidden="true">
                {first + i}
                {i === 0 && d.tail_cut && (
                  <span title="This line began before the part kept">…</span>
                )}
              </span>
              {`${line}\n`}
            </span>
          ))}
        </code>
      </pre>
    </div>
  );
}
