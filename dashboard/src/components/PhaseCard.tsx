/**
 * One phase of one turn, rendered as a stable round ledger.
 *
 * Four rules, each of which fixes a specific way the previous surface moved
 * under the reader:
 *
 *  1. **Identity is `turn|phase|iteration`** (see lib/phases.ts), so a phase
 *     that finishes updates IN PLACE instead of being removed and re-inserted
 *     somewhere else in the list.
 *  2. **Open/closed is latched.** Once a reader opens a phase it stays open
 *     until they close it. The previous surface defaulted a row to open only
 *     while it was live and closed the instant it completed — hiding the
 *     transcript at exactly the moment it became complete — and slammed the
 *     whole turn card shut when its last phase finished, for the same reason.
 *  3. **Tool calls are grouped by their own `round`,** which only appends.
 *     Nothing above the insertion point can move. The previous surface
 *     distributed them across inter-paragraph slots with arithmetic whose
 *     divisor grew every round, so every earlier badge was re-placed each time
 *     a new tool ran.
 *  4. **The header says the same things in the same places, always** — phase,
 *     model, rounds, tokens, decision — so a live phase and a finished one are
 *     the same shape and the row does not change height when it completes.
 */

import { useState, type ReactNode } from "react";
import { Badge, Button, PhaseTag, cx } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { fmtCount, fmtDateTime, relTime } from "~/lib/format.ts";
import { decisionLabel, rounds, splitThinking, type PhaseRecord } from "~/lib/phases.ts";
import { staleness } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { href } from "~/app/router.tsx";

function Disclosure({
  label,
  count,
  children,
  defaultOpen,
  mono,
}: {
  label: ReactNode;
  count?: ReactNode;
  children: ReactNode;
  defaultOpen?: boolean;
  mono?: boolean;
}) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div className="disclosure">
      <button className="disclosure-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <Icon name={open ? "chevronDown" : "chevronRight"} size="xs" />
        <span className={cx("truncate", mono && "mono")}>{label}</span>
        {count != null && <span className="count-chip">{count}</span>}
      </button>
      {open && <div className="disclosure-body">{children}</div>}
    </div>
  );
}

function ToolRow({
  name,
  args,
  result,
  failed,
}: {
  name: string;
  args: string;
  result: string;
  failed: boolean;
}) {
  return (
    <div className={cx("tool-row", failed && "failed")}>
      <Disclosure
        mono
        label={
          <>
            {failed && <Icon name="alert" size="xs" style={{ color: "var(--critical-ink)" }} />}
            {name}
          </>
        }
      >
        <div className="col gap-1">
          <div className="t-label">Arguments</div>
          <pre className="code plain">{args || "{}"}</pre>
          <div className="t-label">{failed ? "Error" : "Result"}</div>
          <pre className="code">{result || "(empty)"}</pre>
        </div>
      </Disclosure>
    </div>
  );
}

export function PhaseCard({
  record,
  defaultOpen,
  showRole,
}: {
  record: PhaseRecord;
  defaultOpen?: boolean;
  showRole?: boolean;
}) {
  // Latched: seeded from `defaultOpen` and then owned by the reader. A phase
  // completing is not a reason to hide it.
  const [open, setOpen] = useState(!!defaultOpen);
  const now = useNow();
  const ledger = rounds(record.tools);
  const { thinking, answer } = splitThinking(record.response);
  const stale = record.live ? staleness(record.at, now) : "";

  return (
    <article className={cx("phase-card", record.failed && "failed", record.live && "live")}>
      <header className="phase-head" onClick={() => setOpen((v) => !v)}>
        <Icon name={open ? "chevronDown" : "chevronRight"} size="xs" />
        <PhaseTag phase={record.phase} />
        {record.iteration > 1 && (
          <span className="t-caption" title="self-iterate round">
            iter {record.iteration}
          </span>
        )}
        {showRole && record.role && <span className="t-cell truncate">{record.role}</span>}

        <span className="spacer" />

        {/* Everything below is present on BOTH a live and a finished phase, in
            the same order, so the row does not reshape when it completes. */}
        {record.decision && (
          <Badge tone={record.decision === "self_iterate" ? "caution" : "neutral"}>
            {decisionLabel(record.phase, record.decision)}
          </Badge>
        )}
        {record.exhaustedRounds && (
          <Badge tone="caution" title="the phase ran out of tool rounds">
            round cap
          </Badge>
        )}
        {record.rescueFired && (
          <Badge tone="caution" title="the phase did not submit on its first run and was re-asked">
            rescued
          </Badge>
        )}
        {record.worker && <Badge tone="neutral">worker: {record.worker}</Badge>}
        {record.backend === "sandbox" && (
          <Badge tone="info" icon="terminal">
            {record.codingAgent || "sandbox"}
          </Badge>
        )}
        {record.failed && <Badge tone="critical">{record.errorKind || "failed"}</Badge>}
        {record.live && (
          <Badge tone={stale === "stalled" ? "critical" : stale ? "caution" : "info"} dot>
            {stale === "stalled" ? "no update in 10m" : stale ? "no update in 2m" : "running"}
          </Badge>
        )}

        <span className="phase-meta mono">{record.model || "—"}</span>
        <span className="phase-meta t-num" title="tool rounds used">
          {ledger.length || record.roundNum > 0
            ? `${Math.max(ledger.length, record.roundNum)}r`
            : "—"}
        </span>
        <span className="phase-meta t-num" title="total tokens">
          {record.totalTokens ? fmtCount(record.totalTokens) : "—"}
        </span>
        <time className="phase-meta" dateTime={record.at} title={fmtDateTime(record.at)}>
          {relTime(record.at, now)}
        </time>
      </header>

      {open && (
        <div className="phase-body">
          {record.failed && record.error && (
            <div className="banner critical">
              <Icon name="alert" size="sm" />
              <span>{record.error}</span>
            </div>
          )}
          {record.notes && (
            <div className="banner neutral">
              <Icon name="info" size="sm" />
              <span>{record.notes}</span>
            </div>
          )}

          {/* The round ledger. Rounds append; nothing above an insertion can
              move, which is the whole point. */}
          {ledger.length > 0 && (
            <section className="col gap-1">
              <div className="t-label">
                Tool rounds
                <span className="faint"> · in the order the engine recorded them</span>
              </div>
              <ol className="round-ledger">
                {ledger.map((r) => (
                  <li key={r.round} className="round">
                    <div className="round-mark t-num">{r.round + 1}</div>
                    <div className="round-tools">
                      {r.tools.map((t, i) => (
                        <ToolRow key={`${t.name}-${i}`} {...t} />
                      ))}
                    </div>
                  </li>
                ))}
              </ol>
            </section>
          )}

          {thinking && (
            <Disclosure label="Reasoning" count={`${thinking.length} chars`}>
              <pre className="code">{thinking}</pre>
            </Disclosure>
          )}

          {answer.trim() && (
            <section className="col gap-1">
              <div className="t-label">Model output</div>
              <pre className="code">{answer.trim()}</pre>
            </section>
          )}

          {record.live && !answer.trim() && !ledger.length && (
            <div className="t-caption">
              The model has not answered yet. This row updates as each round lands.
            </div>
          )}

          {(record.systemPrompt || record.userPrompt) && (
            <Disclosure label="Prompt" count={`${record.phase} phase`}>
              <div className="col gap-3">
                {record.systemPrompt && (
                  <div className="col gap-1">
                    <div className="t-label">System</div>
                    <pre className="code">{record.systemPrompt}</pre>
                  </div>
                )}
                {record.userPrompt && (
                  <div className="col gap-1">
                    <div className="t-label">User</div>
                    <pre className="code">{record.userPrompt}</pre>
                  </div>
                )}
              </div>
            </Disclosure>
          )}

          {(record.toolsAvailable.length > 0 || record.toolCatalogue.length > 0) && (
            <Disclosure
              label="Tool surface"
              count={record.toolsAvailable.length + record.toolCatalogue.length}
            >
              <div className="col gap-2">
                {record.toolsAvailable.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Callable this round
                      <span className="faint"> · full JSON schemas were sent</span>
                    </div>
                    <div className="row wrap gap-1">
                      {record.toolsAvailable.map((t) => (
                        <Badge key={t} mono outline>
                          {t}
                        </Badge>
                      ))}
                    </div>
                  </div>
                )}
                {record.toolCatalogue.length > 0 && (
                  <div className="col gap-1">
                    <div className="t-label">
                      Offered as prose
                      <span className="faint"> · discoverable, not yet callable</span>
                    </div>
                    <div className="row wrap gap-1">
                      {record.toolCatalogue.map((t) => (
                        <Badge key={t} mono outline>
                          {t}
                        </Badge>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            </Disclosure>
          )}

          <footer className="phase-foot">
            <span className="t-caption">
              {record.inputTokens ? `${fmtCount(record.inputTokens)} in` : ""}
              {record.outputTokens ? ` · ${fmtCount(record.outputTokens)} out` : ""}
              {record.providerKey ? ` · provider ${record.providerKey}` : ""}
            </span>
            <span className="spacer" />
            {record.conversationKey && (
              <a
                className="t-caption"
                href={href(["conversations"], { key: record.conversationKey })}
              >
                {record.conversationKey}
              </a>
            )}
            {record.eventId && (
              <a className="t-caption" href={href(["events", record.eventId])}>
                event →
              </a>
            )}
          </footer>
        </div>
      )}
    </article>
  );
}
