/**
 * Runs of checks, listed and opened: a repository's and a change's Checks tabs list them a page at
 * a time (`code_checks`), and every checks view opens one in the pane here (`code_check`), its log
 * read on demand by its object (`code_check_log`).
 */

import { Fragment, useState, type ReactNode, type RefObject } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  CodeBlock,
  Count,
  EmptyValue,
  IconButton,
  Skeleton,
  TableDateCell,
  TableNumberCell,
  TableTextCell,
  Tag,
} from "@crewlethq/ui";
import { CircleCheckGlyph, CircleXGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { refName, shortOID } from "~/lib/code.ts";
import { checkPurposeWord, checkStateTone, checkStateWord } from "~/lib/checks.ts";
import { fmtBytes, fmtDuration, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeCheckDetail,
  CodeCheckFailure,
  CodeCheckRow,
  CodeChecksAnswer,
} from "~/protocol/index.ts";
import { ChangeRefPill } from "../ChangeRow.tsx";
import { commitHref, compareHref, repoHref } from "../paths.ts";
import { BranchChip, RefName } from "../ui.tsx";
import { EndedAgo, CheckMark } from "./marks.tsx";
import { checkedWords, tailLines, testsWords } from "./standing.ts";
import { Tail } from "./Tail.tsx";
import "~/styles/gitlet.css";

/**
 * How often a listing's newest page is read again: HALF A MINUTE. A check ends minutes after it starts, and the code log
 * pushes nothing to the dashboard, so this read is how a reader watching a
 * run sees it end; the pushes history's minute is for a log people read after
 * the fact.
 */
export const CHECKS_POLL_MS = 30_000;

/** An attempt's key, as one string: what a grid row and the pane are keyed by. */
export function attemptKey(r: CodeCheckRow): string {
  return `${r.repo_id}/${r.oid}/${r.target}/${r.name}/${r.attempt}`;
}

/** An attempt's state, in its word and its state's tone. */
export function CheckStateTag({ state }: { state: string }) {
  const tone = checkStateTone(state);
  const word = checkStateWord(state);
  return tone === "neutral" ? (
    <Tag appearance="outline">{word}</Tag>
  ) : (
    <Tag variant={tone}>{word}</Tag>
  );
}

/**
 * A page of attempts: what each check is, its state, the commit and the
 * branch it was checked for, what it was checking, how long it ran and when
 * it ended. Pressing one opens it.
 */
export function CheckGrid({
  rows,
  label,
  showRepo,
  open,
  onOpen,
}: {
  rows: readonly CodeCheckRow[];
  label: string;
  /** The repository column, for a listing that spans repositories. */
  showRepo: boolean;
  open: string;
  onOpen: (row: CodeCheckRow) => void;
}) {
  return (
    <DataGrid<CodeCheckRow>
      rows={[...rows]}
      rowKey={attemptKey}
      // THE ENGINE'S ORDER: newest ended first, or a commit's checks by
      // name. A head that sorted the pages loaded answers another question.
      serverSorted
      aria-label={label}
      isSelected={(r) => attemptKey(r) === open}
      onRowActivate={(r) => onOpen(r)}
      columns={[
        {
          key: "name",
          header: "Check",
          phoneLead: true,
          cell: (r) => (
            <span className="row gap-1 nowrap">
              <span className="mono">{r.name}</span>
              {r.attempt > 1 && <span className="t-caption">{`attempt ${r.attempt}`}</span>}
              {r.required && <span className="t-caption">required</span>}
            </span>
          ),
        },
        {
          key: "state",
          header: "State",
          shrink: true,
          cell: (r) => <CheckStateTag state={r.state} />,
        },
        ...(showRepo
          ? [
              {
                key: "repo",
                header: "Repository",
                shrink: true,
                cell: (r: CodeCheckRow) => <TableTextCell>{r.repo ?? r.repo_id}</TableTextCell>,
              },
            ]
          : []),
        {
          key: "commit",
          header: "Commit",
          shrink: true,
          cell: (r) => (
            <span className="row gap-1 nowrap" title={r.target}>
              {r.repo ? (
                <a className="t-link mono" href={commitHref(r.repo, r.oid)}>
                  {shortOID(r.oid)}
                </a>
              ) : (
                <span className="mono">{shortOID(r.oid)}</span>
              )}
              <span className="muted">for</span> <RefName gitRef={r.target} />
            </span>
          ),
        },
        {
          key: "purpose",
          header: "Checking",
          shrink: true,
          drop: 2,
          cell: (r) => <TableTextCell>{checkPurposeWord(r.purpose)}</TableTextCell>,
        },
        {
          key: "duration",
          header: "Ran",
          shrink: true,
          align: "right",
          cell: (r) => (
            <TableNumberCell
              value={r.duration_ms ?? null}
              format={fmtDuration}
              emptyLabel="Not ended"
            />
          ),
        },
        {
          key: "ended",
          header: "Ended",
          shrink: true,
          align: "right",
          drop: 1,
          cell: (r) => <EndedAt at={r.finished_at} />,
        },
      ]}
    />
  );
}

/** When an attempt ended, against the one clock; read HERE so a tick re-renders this cell alone. */
function EndedAt({ at }: { at: string | undefined }) {
  const now = useNow();
  if (!at) return <EmptyValue label="Not ended" />;
  return <TableDateCell value={at} now={now} />;
}

/** A repository's or a change's runs, paged, with the pane of the one opened beneath them. */
export function CheckListing({
  params,
  label,
  showRepo,
  empty,
}: {
  params: CodeParams;
  label: string;
  showRepo: boolean;
  empty: { title: string; hint: string };
}) {
  const first = useCodeQuery("code_checks", params, { pollMs: CHECKS_POLL_MS });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_checks", { ...params, cursor }),
    (a: CodeChecksAnswer) => a.checks ?? [],
    attemptKey,
    (a: CodeChecksAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  const more = paged.more;
  const [open, setOpen] = useState<CodeCheckRow | null>(null);
  const openKey = open ? attemptKey(open) : "";

  return (
    <>
      {first.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={5} label={`Loading ${label}`} />
      )}
      <QueryState
        error={first.error}
        refusal={first.refusal}
        detail={first.detail}
        loading={first.loading}
        empty={rows.length > 0 || more ? undefined : empty}
      >
        <CheckGrid
          rows={rows}
          label={label}
          showRepo={showRepo}
          open={openKey}
          onOpen={(r) => setOpen(attemptKey(r) === openKey ? null : r)}
        />
      </QueryState>
      {(rows.length > 0 || more || paged.pageError) && (
        <div className="row gap-2 wrap">
          {more ? (
            <Button
              size="small"
              variant="secondary"
              onClick={paged.next}
              disabled={paged.paging}
              loading={paged.paging}
            >
              Load more
            </Button>
          ) : (
            <span className="t-caption">That is every attempt this listing reaches.</span>
          )}
          {paged.pageError && <PageFailed error={paged.pageError} />}
          {rows.length > 0 && (
            <span className="t-caption">{plural(rows.length, "attempt")} listed</span>
          )}
        </div>
      )}
      {open && open.repo && (
        <CheckPane key={openKey} row={open} repo={open.repo} onClose={() => setOpen(null)} />
      )}
    </>
  );
}

/** One run read whole: its heading, tests, facts, failures, the end of its output, and its log on demand. */
export function CheckPane({
  row,
  repo,
  onClose,
  headingRef,
  later = null,
  onOpenLater,
}: {
  row: CodeCheckRow;
  repo: string;
  onClose: () => void;
  /** Where its host moves focus as it opens. */
  headingRef?: RefObject<HTMLHeadingElement | null>;
  /** A later attempt of the same check, where one is known, and how to open it. */
  later?: number | null;
  onOpenLater?: (attempt: number) => void;
}) {
  // READ AGAIN UNTIL IT HAS ENDED, so an open run's pane moves on as its row does.
  const [ended, setEnded] = useState(false);
  const read = useCodeQuery(
    "code_check",
    { name: repo, oid: row.oid, check: row.name, attempt: row.attempt, target: row.target },
    { pollMs: ended ? undefined : CHECKS_POLL_MS },
  );
  const d = withChange(read.data?.check, row);
  const shown = d ?? row;
  const hasEnded = d !== undefined && d.state !== "queued" && d.state !== "running";
  if (hasEnded !== ended) setEnded(hasEnded);
  return (
    <section className="code-check-pane" aria-label={`Check ${row.name}, attempt ${row.attempt}`}>
      <div className="code-check-pane__head">
        <CheckMark state={shown.state} />
        <h2 ref={headingRef} tabIndex={-1} className="code-check-pane__title">
          {paneHeading(shown)}
        </h2>
        {shown.required && (
          <Tag size="xs" appearance="outline">
            Required
          </Tag>
        )}
        <IconButton
          className="code-check-pane__close"
          label={`Close ${row.name}`}
          icon={<XGlyph />}
          size="sm"
          variant="ghost"
          onClick={onClose}
        />
      </div>
      <div className="code-check-pane__body">
        {later !== null && later > row.attempt && (
          <p className="code-check-pane__later">
            {`A later run, attempt ${later}, is its latest: `}
            {onOpenLater ? (
              <button type="button" className="t-link" onClick={() => onOpenLater(later)}>
                <strong>open it</strong>
              </button>
            ) : (
              <strong>open it from the list</strong>
            )}
          </p>
        )}
        <QueryState
          error={read.error}
          refusal={read.refusal}
          detail={read.detail}
          loading={read.loading}
        >
          {d && <CheckWhole repo={repo} d={d} />}
        </QueryState>
      </div>
    </section>
  );
}

/** A run read whole, with the change it judges from its listed row: only a standing listing names it. */
function withChange(
  d: CodeCheckDetail | undefined,
  row: CodeCheckRow,
): CodeCheckDetail | undefined {
  return (
    d && {
      ...d,
      change: d.change ?? row.change,
      source_ref: d.source_ref ?? row.source_ref,
      change_head: d.change_head ?? row.change_head,
    }
  );
}

/** "e2e failed for api!17", "unit passed on main's tip", "lint running". */
export function paneHeading(r: CodeCheckRow): string {
  const said = r.state ? `${r.name} ${checkStateWord(r.state)}` : r.name;
  if (r.change) return `${said} for ${r.change.ref}`;
  if (r.purpose === "tip") return `${said} on ${refName(r.target).name}'s tip`;
  return said;
}

function CheckWhole({ repo, d }: { repo: string; d: CodeCheckDetail }) {
  const tests = testsWords(d);
  const checked = checkedWords(d);
  const branch = d.source_ref && d.purpose !== "tip" ? d.source_ref : d.target;
  const facts: { label: string; value: ReactNode }[] = [{ label: "Repository", value: repo }];
  if (d.change) {
    facts.push({
      label: "Change",
      value: (
        <span className="code-check-pane__change">
          <ChangeRefPill change={d.change} />
          <span className="code-check-pane__change-title">{d.change.title}</span>
        </span>
      ),
    });
  }
  facts.push({
    label: "Commit",
    value: (
      <span>
        <a className="t-link mono" href={commitHref(repo, d.change_head ?? d.oid)}>
          {shortOID(d.change_head ?? d.oid)}
        </a>{" "}
        on <BranchChip name={refName(branch).name} />
      </span>
    ),
  });
  if (checked) {
    facts.push({
      label: "Checked",
      value: (
        <span>
          {checked.before}
          <RefName gitRef={d.target} />
          {checked.after}
        </span>
      ),
    });
  }
  facts.push({ label: "Ran", value: <Ran d={d} /> });
  if (d.def?.run) {
    facts.push({ label: "Runs", value: <code className="code-check-pane__run">{d.def.run}</code> });
  }
  if (d.reason) facts.push({ label: "Why it ended", value: d.reason.replaceAll("_", " ") });
  if (d.placement || d.runner) {
    facts.push({ label: "Ran on", value: [d.placement, d.runner].filter(Boolean).join(" · ") });
  }
  const lines = d.tail ? tailLines(d.tail) : [];
  return (
    <div className="col gap-3">
      {tests && (
        <p
          className="code-check-pane__tests"
          data-tone={
            tests.failed === null ? checkStateTone(d.state) : tests.failed ? "danger" : "success"
          }
        >
          {tests.failed !== null &&
            (tests.failed ? <CircleXGlyph size="sm" /> : <CircleCheckGlyph size="sm" />)}
          {tests.words}
        </p>
      )}
      {d.summary && d.summary !== tests?.words && <p>{d.summary}</p>}
      <dl className="code-facts" aria-label="The run">
        {facts.map((f) => (
          <div key={f.label}>
            <dt>{f.label}</dt>
            <dd>{f.value}</dd>
          </div>
        ))}
      </dl>
      {d.url && (
        <div className="row gap-2 wrap">
          <ButtonLink size="small" variant="secondary" href={d.url} external>
            Open in CI
          </ButtonLink>
        </div>
      )}
      {(d.failures?.length ?? 0) > 0 && (
        <Failures repo={repo} d={d} failures={d.failures ?? []} omitted={d.failures_omitted ?? 0} />
      )}
      {d.tail && <Tail d={d} lines={lines} />}
      {d.log && <LogPages repo={repo} d={d} object={d.log.object} size={d.log.size} />}
    </div>
  );
}

/** "1m 52s · ended 20 minutes ago · attempt 1". */
function Ran({ d }: { d: CodeCheckDetail }) {
  const parts: ReactNode[] = [];
  if (d.finished_at && d.duration_ms) parts.push(fmtDuration(d.duration_ms));
  if (d.finished_at) parts.push(<EndedAgo word="ended" at={d.finished_at} />);
  else if (d.started_at) parts.push(<EndedAgo word="started" at={d.started_at} />);
  else if (d.queued_at) parts.push(<EndedAgo word="queued" at={d.queued_at} />);
  parts.push(`attempt ${d.attempt}`);
  return (
    <span>
      {parts.map((p, i) => (
        <Fragment key={i}>
          {i > 0 && " · "}
          {p}
        </Fragment>
      ))}
    </span>
  );
}

/**
 * Where a failure is in the diff the attempt was checked on: the comparison
 * of its base and its commit, opened on the file at the failing line, which
 * the diff marks (or, where the line is outside what the diff changes, offers
 * in the file itself). Where the attempt records no base there is no diff: a
 * failure naming a line opens the file at the attempt's commit on that line,
 * and one naming none the commit's own page.
 */
export function failureHref(repo: string, d: CodeCheckRow, f: CodeCheckFailure): string {
  if (!f.path) return commitHref(repo, d.oid);
  if (!d.base) {
    return f.line
      ? repoHref(repo, { ref: d.oid, path: f.path, lines: String(f.line) })
      : commitHref(repo, d.oid);
  }
  return compareHref(repo, { base: d.base, head: d.oid, file: f.path, line: f.line });
}

function Failures({
  repo,
  d,
  failures,
  omitted,
}: {
  repo: string;
  d: CodeCheckDetail;
  failures: readonly CodeCheckFailure[];
  omitted: number;
}) {
  return (
    <div className="col gap-1">
      <h3 className="code-check-pane__part">
        Failures <Count value={failures.length + omitted} label="failures" />
      </h3>
      <ul className="col gap-1" aria-label="Failures">
        {failures.map((f, i) => (
          <li key={i} className="col">
            <span className="row gap-1 wrap">
              {f.path && (
                <a className="t-link mono" href={failureHref(repo, d, f)}>
                  {f.line ? `${f.path}:${f.line}` : f.path}
                </a>
              )}
              {f.test_id && <span className="mono">{f.test_id}</span>}
            </span>
            <span className="t-caption">{f.message}</span>
          </li>
        ))}
      </ul>
      {omitted > 0 && (
        <span className="t-caption">{`and ${plural(omitted, "more failure")} the run counted`}</span>
      )}
    </div>
  );
}

/** Where the log read so far stands. */
interface LogRead {
  text: string;
  next: number | null;
  reading: boolean;
  error: string;
}

/**
 * The log a failed run kept, a page at a time, on demand: never asked until
 * the reader asks, since a log is up to its whole size and the tail above
 * already holds where a failure is printed. Asked BY ITS OBJECT, which the
 * engine answers immutably: an object's key is minted once and never reused.
 */
function LogPages({
  repo,
  d,
  object,
  size,
}: {
  repo: string;
  d: CodeCheckDetail;
  object: string;
  size: number;
}) {
  const [log, setLog] = useState<LogRead>({ text: "", next: 0, reading: false, error: "" });
  const started = log.text !== "" || log.next !== 0;
  const read = async () => {
    if (log.next === null) return;
    setLog((l) => ({ ...l, reading: true, error: "" }));
    try {
      const page = await codeRest.query("code_check_log", {
        name: repo,
        oid: d.oid,
        check: d.name,
        attempt: d.attempt,
        target: d.target,
        object,
        offset: log.next,
      });
      setLog((l) => ({
        text: l.text + page.text,
        next: page.next ?? null,
        reading: false,
        error: "",
      }));
    } catch (err) {
      setLog((l) => ({
        ...l,
        reading: false,
        error: err instanceof Error ? err.message : "query_failed",
      }));
    }
  };
  return (
    <div className="col gap-1">
      {started && (
        <CodeBlock plain selectable focusWhenScrollable label="Its log" code={log.text} />
      )}
      <span className="row gap-2 wrap">
        {log.next !== null && (
          <Button
            size="small"
            variant="secondary"
            onClick={() => void read()}
            loading={log.reading}
          >
            {started ? "Read more of the log" : `Read its log, ${fmtBytes(size)}`}
          </Button>
        )}
        {log.next === null && <span className="t-caption">That is the whole log.</span>}
        {log.error && (
          <Callout variant="danger">{`Could not read the log (${log.error}). Try again.`}</Callout>
        )}
      </span>
    </div>
  );
}
