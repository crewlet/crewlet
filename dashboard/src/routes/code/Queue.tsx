/**
 * Code › Queue: the merge queues of the company's gated branches — what each
 * holds, in the order it lands them, what is being tested on what, and what
 * a queue put out lately and why.
 *
 * # A band per repository and branch
 *
 * A queue is one branch's (`code_queue` rows carry their repository and
 * target), so the screen draws a BAND for each, its entries in queue order:
 * the place, the change, who enqueued it, where it stands, the train base it
 * is tested on, a meter of the required checks of what it is tested in, and
 * how long it has been in the queue against the slowest of those checks'
 * declared timeouts — the queue waits for its slowest required check.
 *
 * # One read of checks per candidate, shared by its entries
 *
 * A candidate tests a batch of entries at once (up to `batch`, 4 unset), so
 * its tip's checks are the same answer for every entry it holds: the band
 * asks `code_checks` ONCE per candidate's tip and hands the rows to each of
 * its entries' meter and timer. Asked per entry and per cell, a queue at its
 * deepest — 8 candidates of 8 changes — would be 128 reads every half minute
 * for every reader watching it, sixteen times the 8 that answer it.
 *
 * # Every repository's live entries, one repository's detail
 *
 * Unnarrowed, the engine answers every repository's live entries the reader
 * may read, and no trains: what a train tests and what a queue ejected are a
 * NAMED repository's (`repo=`), as the engine answers them, on its first page.
 * So the meter, the train base and the ejections appear once a repository is
 * chosen, and the screen says so rather than drawing them empty.
 *
 * # A page at a time, like every code listing
 *
 * The engine answers fifty entries a page, and one branch's queue may hold
 * up to 1 024 waiting (`code.MaxQueueEntries`), so Load more follows `next`
 * through the pager every code listing shares (`lib/usePaged.ts`), the
 * newest page held still from its press. Each page answers the trains of the
 * targets it reaches, so the bands draw every page's.
 *
 * # Ejected, in the danger tone, with what the engine found
 *
 * An entry the queue put out in the last day is drawn under its band in the
 * danger tone with the reason in words, and the required checks that failed
 * on what it was tested in or the paths its merge conflicted on — never a
 * guess at who is to blame.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  SegmentedMeter,
  Select,
  Skeleton,
  Tag,
} from "@crewlethq/ui";
import { ListOrderedGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href, useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { changePath, changeRef, isRepoName, refName, shortOID } from "~/lib/code.ts";
import {
  ejectReasonWord,
  longestTimeout,
  queueBands,
  queueStateWord,
  requiredMeter,
  type QueueBand,
  type QueuePlace,
} from "~/lib/checks.ts";
import { fmtElapsed, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type {
  CodeCheckRow,
  CodeEjectedRow,
  CodeParty,
  CodeQueueAnswer,
  CodeRepoSummary,
} from "~/protocol/index.ts";
import { CHECKS_POLL_MS } from "./checks/CheckList.tsx";
import { REPO_PARAM } from "./paths.ts";

/**
 * How often the queues are read again: HALF A MINUTE, the checks listings'
 * pace — a queue moves when a check ends, and the code log pushes nothing to
 * the dashboard.
 */
const QUEUE_POLL_MS = CHECKS_POLL_MS;

export function Queue() {
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const named = repo !== "" && isRepoName(repo);
  const choices = useCodeQuery("code_repos");
  const params = useMemo(() => (named ? { repo } : {}), [named, repo]);
  const hold = usePageHold<CodeQueueAnswer>(params);
  const read = useCodeQuery("code_queue", params, {
    pollMs: QUEUE_POLL_MS,
    enabled: repo === "" || named,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    read,
    (cursor) => codeRest.query("code_queue", { ...params, cursor }),
    (a: CodeQueueAnswer) => a.queue ?? [],
    (r) => r.entry_id,
    (a: CodeQueueAnswer) => a.next ?? "",
  );
  // EVERY PAGE'S TRAINS: a page answers the trains of the targets it
  // reaches, and an entry is matched to the first that names it — the
  // newest page's, which is read first and polled.
  const bands = useMemo(
    () =>
      queueBands(
        paged.items,
        paged.answers.flatMap((a) => a.trains ?? []),
        paged.answers.flatMap((a) => a.ejected ?? []),
      ),
    [paged.items, paged.answers],
  );

  return (
    <>
      <div className="toolbar">
        <QueueRepoPicker repo={repo} onChoose={setRepo} listing={choices.data?.repos ?? []} />
        {repo === "" && (
          <span className="t-caption">
            Every repository’s queues; choose one for its trains, its checks and what it put out
            lately.
          </span>
        )}
      </div>
      {repo !== "" && !named ? (
        <Callout variant="warning">
          “{repo}” is not a repository’s name: one is lowercase letters, digits and hyphens,
          beginning and ending with a letter or a digit, at most 63 in all.
        </Callout>
      ) : (
        <>
          {read.loading && !read.data && (
            <Skeleton variant="text" rows={6} label="Loading the queues" />
          )}
          <QueryState
            error={read.error}
            refusal={read.refusal}
            detail={read.detail}
            loading={read.loading}
          >
            {bands.length === 0 && !paged.more ? (
              <EmptyState
                icon={<ListOrderedGlyph size={32} />}
                title={
                  named ? `No queue of ${repo} holds a change` : "No merge queue holds a change"
                }
                description="A change joins its branch’s queue when it is enqueued to merge into a branch whose gate requires checks; it leaves when it lands, or the queue puts it out."
              />
            ) : (
              <div className="col gap-4">
                {bands.map((b) => (
                  <Band key={b.key} band={b} detailed={named} />
                ))}
              </div>
            )}
          </QueryState>
          {(paged.more || paged.pageError) && (
            <div className="row gap-2 wrap">
              {paged.more && (
                <Button
                  size="small"
                  variant="secondary"
                  onClick={paged.older}
                  disabled={paged.paging}
                  loading={paged.paging}
                >
                  Load more
                </Button>
              )}
              {paged.pageError && <PageFailed error={paged.pageError} />}
              {paged.held && <span className="t-caption">Paused while you page on.</span>}
            </div>
          )}
        </>
      )}
    </>
  );
}

function QueueRepoPicker({
  repo,
  onChoose,
  listing,
}: {
  repo: string;
  onChoose: (name: string) => void;
  listing: readonly CodeRepoSummary[];
}) {
  // EVERY REPOSITORY IS AN OPTION OF ITS OWN, the unnarrowed answer; the
  // chosen name stays one where the listing's first page lacks it, so an
  // address naming any repository reads as itself.
  const options = [
    { value: "", label: "Every repository" },
    ...listing.map((r) => ({ value: r.name, label: r.name })),
  ];
  if (repo !== "" && !options.some((o) => o.value === repo))
    options.splice(1, 0, { value: repo, label: repo });
  return (
    <Select
      width="auto"
      searchable
      value={repo}
      onChange={(value) => onChoose(String(value))}
      options={options}
      ariaLabel="Repository"
      emptyMessage="No repository matches"
      active={repo !== ""}
    />
  );
}

/** One branch's queue: its live entries in queue order, then what it put out. */
function Band({ band, detailed }: { band: QueueBand; detailed: boolean }) {
  const seatBadge = useSeatBadgeOf();
  const title = `${band.repo} · ${refName(band.target).name}`;
  // THE CANDIDATES' TIPS, each read once ([TipChecks]) and its rows held
  // here, by tip, for every entry the candidate tests.
  const tips = useMemo(
    () => [...new Set(band.places.flatMap((p) => (p.candidate ? [p.candidate.tip_oid] : [])))],
    [band.places],
  );
  const [checksOf, setChecksOf] = useState<ReadonlyMap<string, readonly CodeCheckRow[]>>(
    () => new Map(),
  );
  const hold = useCallback(
    (tip: string, rows: readonly CodeCheckRow[]) =>
      setChecksOf((held) => new Map(held).set(tip, rows)),
    [],
  );
  // A TIP NO CANDIDATE HOLDS ANY MORE is let go: a train that ended took its
  // candidates with it, and a screen left open all day would otherwise keep
  // the checks of every candidate the queue ever built.
  useEffect(() => {
    setChecksOf((held) => {
      const kept = [...held].filter(([tip]) => tips.includes(tip));
      return kept.length === held.size ? held : new Map(kept);
    });
  }, [tips]);
  // A candidate's required checks as its entries read them: the rows checked
  // for this band's branch alone, undefined until its tip answers.
  const checksFor = (p: QueuePlace) => {
    const rows = p.candidate ? checksOf.get(p.candidate.tip_oid) : undefined;
    return rows?.filter((r) => r.target === p.row.target_ref);
  };
  return (
    <section aria-label={`The queue of ${title}`}>
      {tips.map((tip) => (
        <TipChecks key={tip} repo={band.repo} oid={tip} onRows={hold} />
      ))}
      <Card>
        <Card.Header>
          <Card.Title>{title}</Card.Title>
          <span className="t-caption">{plural(band.places.length, "change")} queued</span>
        </Card.Header>
        {band.places.length > 0 && (
          <DataGrid<QueuePlace>
            rows={band.places}
            rowKey={(p) => p.row.entry_id}
            // QUEUE ORDER IS THE ENGINE'S, the order it lands them.
            serverSorted
            aria-label={`Queued into ${title}`}
            columns={[
              {
                key: "place",
                header: "#",
                shrink: true,
                align: "right",
                cell: (p) => <span className="mono">{p.row.place}</span>,
              },
              {
                key: "change",
                header: "Change",
                phoneLead: true,
                cell: (p) => (
                  <a className="t-link mono" href={href(changePath(p.row.repo, p.row.number))}>
                    {changeRef(p.row.repo, p.row.number)}
                  </a>
                ),
              },
              {
                key: "by",
                header: "Enqueued by",
                shrink: true,
                cell: (p) => (
                  <Party
                    party={p.row.enqueued_by}
                    name={seatBadge(p.row.enqueued_by.handle).name}
                  />
                ),
              },
              {
                key: "state",
                header: "State",
                shrink: true,
                cell: (p) =>
                  p.row.state === "testing" ? (
                    <Tag variant="info">{queueStateWord(p.row.state)}</Tag>
                  ) : (
                    <Tag appearance="outline">{queueStateWord(p.row.state)}</Tag>
                  ),
              },
              {
                key: "base",
                header: "Train base",
                shrink: true,
                drop: 2,
                cell: (p) =>
                  p.candidate ? (
                    <a
                      className="t-link mono"
                      href={href(["code", "repos", p.row.repo, "commits", p.candidate.base_oid])}
                      title={p.candidate.base_oid}
                    >
                      {shortOID(p.candidate.base_oid)}
                    </a>
                  ) : (
                    <EmptyValue label={detailed ? "Not yet tested" : "Choose a repository"} />
                  ),
              },
              {
                key: "checks",
                header: "Required checks",
                floor: "10rem",
                drop: 1,
                cell: (p) =>
                  p.candidate ? (
                    <CandidateChecks rows={checksFor(p)} />
                  ) : (
                    <EmptyValue label={detailed ? "Not yet tested" : "Choose a repository"} />
                  ),
              },
              {
                key: "elapsed",
                header: "In the queue",
                shrink: true,
                cell: (p) => <Elapsed place={p} rows={checksFor(p)} />,
              },
            ]}
          />
        )}
        {band.ejected.length > 0 && <Ejected repo={band.repo} rows={band.ejected} />}
      </Card>
    </section>
  );
}

/** Who enqueued an entry: a seat by its name, a person by theirs. */
function Party({ party, name }: { party: CodeParty; name: string }) {
  return (
    <span title={party.kind === "person" ? "A person" : "A seat"}>{name || party.handle}</span>
  );
}

/**
 * The checks of one candidate's tip, asked once for every entry it tests:
 * nothing drawn, its rows handed to the band ([Band]) as each answer lands.
 */
function TipChecks({
  repo,
  oid,
  onRows,
}: {
  repo: string;
  oid: string;
  onRows: (tip: string, rows: readonly CodeCheckRow[]) => void;
}) {
  const read = useCodeQuery("code_checks", { repo, oid }, { pollMs: QUEUE_POLL_MS });
  useEffect(() => {
    if (read.data) onRows(oid, read.data.checks ?? []);
  }, [read.data, oid, onRows]);
  return null;
}

/**
 * The required checks of what an entry is tested in — its candidate's tip,
 * checked for its branch — as one meter: passed, failed, running, the rest
 * not yet a verdict. `rows` is undefined until the tip's checks answer.
 */
function CandidateChecks({ rows }: { rows: readonly CodeCheckRow[] | undefined }) {
  if (!rows) return <span className="muted">…</span>;
  const meter = requiredMeter(rows);
  if (meter.total === 0) return <span className="muted">none queued yet</span>;
  return (
    <SegmentedMeter
      size="compact"
      segments={meter.parts}
      total={meter.total}
      remainderLabel="not yet a verdict"
    />
  );
}

/**
 * How long an entry has been in the queue, against the slowest declared
 * timeout of the required checks it is tested in (`rows`, its candidate's,
 * where it is being tested): read HERE against the one clock, so a tick
 * re-renders this cell alone.
 */
function Elapsed({
  place,
  rows,
}: {
  place: QueuePlace;
  rows: readonly CodeCheckRow[] | undefined;
}) {
  const now = useNow();
  const elapsed = fmtElapsed(now - Date.parse(place.row.enqueued_at));
  const timeout = longestTimeout(rows ?? []);
  if (timeout === 0) return <span>{elapsed}</span>;
  return (
    <span title="The slowest required check's declared timeout">{`${elapsed} of ${fmtElapsed(timeout * 1000)}`}</span>
  );
}

/** What the queue put out lately, each in the danger tone with what the engine found. */
function Ejected({ repo, rows }: { repo: string; rows: readonly CodeEjectedRow[] }) {
  return (
    <div className="col gap-2" aria-label="Put out lately">
      <strong>{`Put out in the last day: ${rows.length}`}</strong>
      {rows.map((e) => (
        <Callout key={e.entry_id} variant="danger">
          <span className="col gap-1">
            <span className="row gap-1 wrap">
              <Tag variant="danger">ejected</Tag>
              <a className="t-link mono" href={href(changePath(repo, e.number))}>
                {changeRef(repo, e.number)}
              </a>
              <span>{ejectReasonWord(e.reason)}</span>
            </span>
            {(e.checks?.length ?? 0) > 0 && (
              <span className="t-caption">
                Failed: <span className="mono">{e.checks!.join(", ")}</span>
              </span>
            )}
            {(e.paths?.length ?? 0) > 0 && (
              <span className="t-caption">
                Conflicted on: <span className="mono">{e.paths!.join(", ")}</span>
              </span>
            )}
          </span>
        </Callout>
      ))}
    </div>
  );
}
