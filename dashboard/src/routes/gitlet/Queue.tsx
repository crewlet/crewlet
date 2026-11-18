/**
 * Gitlet › Queue: a box per repository's target branch, its train base and its entries on a spine,
 * read with `trains=true` in every scope; a tip's checks and a repository's gates are asked once.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { Callout, EmptyState, Skeleton } from "@crewlethq/ui";
import { ListOrderedGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { changeRef, isRepoName, refName } from "~/lib/code.ts";
import { queueBands, type QueueBand, type QueuePlace } from "~/lib/checks.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useLatestChecks } from "~/lib/useLatestChecks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeQueueAnswer, CodeRuleGate } from "~/protocol/index.ts";
import { useBranchDetail } from "./branchDetail.ts";
import { DequeueButton } from "./change/ChangeWrites.tsx";
import { CHECKS_POLL_MS } from "./checks/CheckList.tsx";
import { CodeList, type CodeListGroup } from "./CodeList.tsx";
import { REPO_PARAM, changeObject } from "./paths.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import {
  BaseRow,
  BoxHead,
  BoxNote,
  EntryChecks,
  EntryMeta,
  EntryState,
  PlaceBadge,
  RequiredChecks,
  type GateRead,
  type TipRead,
} from "./queue/Box.tsx";
import { gateAsksApproval, gateChecks } from "./queue/words.ts";

/** A queue moves when a check ends, at the checks listings' pace. */
const QUEUE_POLL_MS = CHECKS_POLL_MS;

/** Reads held by key as their readers report them; a key no longer listed is let go. */
function useHeld<V>(
  keys: readonly string[],
): [ReadonlyMap<string, V>, (key: string, value: V) => void] {
  const [held, setHeld] = useState<ReadonlyMap<string, V>>(() => new Map());
  const hold = useCallback(
    (key: string, value: V) => setHeld((now) => new Map(now).set(key, value)),
    [],
  );
  useEffect(() => {
    setHeld((now) => {
      const kept = [...now].filter(([key]) => keys.includes(key));
      return kept.length === now.size ? now : new Map(kept);
    });
  }, [keys]);
  return [held, hold];
}

/** A read of rules, or its refusal's sentence. */
type Rules<T> = { value: T } | { refused: string };

export function Queue() {
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const named = repo !== "" && isRepoName(repo);
  const params = useMemo(
    () => (named ? { repo, trains: "true" } : { trains: "true" }),
    [named, repo],
  );
  const read = useCodeQuery("code_queue", params, {
    pollMs: QUEUE_POLL_MS,
    enabled: repo === "" || named,
  });
  const paged = usePaged(
    read,
    params,
    (cursor) => codeRest.query("code_queue", { ...params, cursor }),
    (a: CodeQueueAnswer) => a.queue ?? [],
    (r) => r.entry_id,
    (a: CodeQueueAnswer) => a.next ?? "",
  );
  // A page answers the trains of the targets it reaches; an entry takes the first that names it.
  const bands = useMemo(
    () =>
      queueBands(
        paged.items,
        paged.answers.flatMap((a) => a.trains ?? []),
        paged.answers.flatMap((a) => a.ejected ?? []),
      ),
    [paged.items, paged.answers],
  );

  const tipOf = useMemo(
    () =>
      new Map(
        bands.flatMap((b) =>
          b.places.flatMap((p) => (p.candidate ? [[p.candidate.tip_oid, b] as const] : [])),
        ),
      ),
    [bands],
  );
  const tipKeys = useMemo(() => [...tipOf.keys()], [tipOf]);
  const repos = useMemo(() => [...new Set(bands.map((b) => b.repo))], [bands]);
  const boxKeys = useMemo(() => bands.map((b) => b.key), [bands]);
  const [tips, holdTip] = useHeld<TipRead>(tipKeys);
  const [gates, holdGates] = useHeld<Rules<readonly CodeRuleGate[]>>(repos);
  const [rules, holdRules] = useHeld<Rules<readonly string[]>>(boxKeys);

  const gateOf = useCallback(
    (band: QueueBand): GateRead => {
      const g = gates.get(band.repo);
      const r = rules.get(band.key);
      if (g && "refused" in g) return { checks: null, refusal: g.refused, approval: true };
      if (r && "refused" in r) return { checks: null, refusal: r.refused, approval: true };
      if (!g || !r) return { checks: undefined, approval: true };
      return {
        checks: gateChecks(r.value, g.value),
        approval: gateAsksApproval(r.value, g.value),
      };
    },
    [gates, rules],
  );

  const bandOf = useMemo(
    () => new Map(bands.flatMap((b) => b.places.map((p) => [p.row.entry_id, b] as const))),
    [bands],
  );
  const groups = useMemo<CodeListGroup<QueuePlace>[]>(
    () =>
      bands.map((band, index) => {
        const gate = gateOf(band);
        // The base of the train holding the box's first entry: its lowest ordinal.
        const train = band.places
          .flatMap((p) => (p.candidate ? [p.candidate] : []))
          .sort((a, b) => a.ordinal - b.ordinal)[0];
        return {
          key: band.key,
          heading: <BoxHead band={band} atLeast={paged.more && index === bands.length - 1} />,
          aside: <RequiredChecks gate={gate} />,
          first: train && (
            <BaseRow repo={band.repo} branch={refName(band.target).name} base={train.base_oid} />
          ),
          last: <BoxNote band={band} approval={gate.approval} />,
          rows: band.places,
        };
      }),
    [bands, gateOf, paged.more],
  );

  return (
    <>
      <PageToolbar>
        <RepoPicker size="sm" repo={repo} onChoose={setRepo} every="Every repository" />
      </PageToolbar>
      {repo !== "" && !named ? (
        <Callout variant="warning">
          “{repo}” is not a repository’s name: one is lowercase letters, digits and hyphens,
          beginning and ending with a letter or a digit, at most 63 in all.
        </Callout>
      ) : (
        <>
          {[...tipOf].map(([tip, band]) => (
            <TipChecks key={tip} repo={band.repo} oid={tip} target={band.target} onRead={holdTip} />
          ))}
          {repos.map((name) => (
            <RepoGates key={name} repo={name} onRead={holdGates} />
          ))}
          {bands.map((band) => (
            <BranchRules
              key={band.key}
              box={band.key}
              repo={band.repo}
              branch={refName(band.target).name}
              onRead={holdRules}
            />
          ))}
          {read.loading && !read.data && (
            <Skeleton variant="text" rows={6} label="Loading the queues" />
          )}
          {read.error === "bad_params" && (
            <p className="code-queue__refused">
              Every queue’s trains are unavailable on this fleet yet.
            </p>
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
                description="A change joins its branch’s queue from its merge box when the branch’s gate requires checks, and leaves when it lands or is taken out."
              />
            ) : (
              <CodeList<QueuePlace>
                look="boxes"
                groups={groups}
                rowKey={(p) => p.row.entry_id}
                aria-label="Merge queues"
                peek={(p) => changeObject(p.row.repo, p.row.number)}
                lead={(p) => <PlaceBadge place={p.row.place} />}
                title={(p) => p.row.title || changeRef(p.row.repo, p.row.number)}
                titleAfter={(p) =>
                  p.row.title ? (
                    <span className="code-queue__ref mono">
                      {changeRef(p.row.repo, p.row.number)}
                    </span>
                  ) : null
                }
                meta={(p) => <EntryMeta band={bandOf.get(p.row.entry_id)!} place={p} />}
                facts={(p) => {
                  const band = bandOf.get(p.row.entry_id)!;
                  return (
                    <div className="code-queue__facts">
                      <span className="code-queue__state">
                        <EntryState state={p.row.state} />
                      </span>
                      <div className="code-queue__cell">
                        <EntryChecks
                          band={band}
                          place={p}
                          gate={gateOf(band)}
                          tip={p.candidate ? tips.get(p.candidate.tip_oid) : undefined}
                        />
                      </div>
                      <span className="code-queue__out">
                        <DequeueButton
                          changeRef={changeRef(p.row.repo, p.row.number)}
                          target={refName(band.target).name}
                          queued
                          label="Take out"
                          leadingIcon={<XGlyph size="sm" />}
                        />
                      </span>
                    </div>
                  );
                }}
                paged={paged}
                moreLabel="Load more queued changes"
              />
            )}
          </QueryState>
        </>
      )}
    </>
  );
}

/** A candidate tip's checks for its branch at their latest attempts, read once for its entries. */
function TipChecks({
  repo,
  oid,
  target,
  onRead,
}: {
  repo: string;
  oid: string;
  target: string;
  onRead: (tip: string, read: TipRead) => void;
}) {
  const { rows, error, detail } = useLatestChecks({ repo, oid, target }, QUEUE_POLL_MS);
  useEffect(() => {
    if (rows || error) onRead(oid, { rows, error, detail });
  }, [rows, error, detail, oid, onRead]);
  return null;
}

/** A repository's rules' merge gates, read once for its boxes. */
function RepoGates({
  repo,
  onRead,
}: {
  repo: string;
  onRead: (repo: string, read: Rules<readonly CodeRuleGate[]>) => void;
}) {
  const read = useCodeQuery("code_repo", { name: repo, gates: true });
  useEffect(() => {
    if (read.error) onRead(repo, { refused: read.detail ?? read.error });
    else if (read.data) onRead(repo, { value: read.data.repo?.gates ?? [] });
  }, [read.error, read.detail, read.data, repo, onRead]);
  return null;
}

/** The rule patterns the engine matched to a box's branch. */
function BranchRules({
  box,
  repo,
  branch,
  onRead,
}: {
  box: string;
  repo: string;
  branch: string;
  onRead: (box: string, read: Rules<readonly string[]>) => void;
}) {
  const detail = useBranchDetail(repo, branch);
  const patterns = detail.row?.protection?.patterns;
  useEffect(() => {
    if (detail.error) onRead(box, { refused: detail.detail ?? detail.error });
    else if (!detail.loading) onRead(box, { value: patterns ?? [] });
  }, [detail.error, detail.detail, detail.loading, patterns, box, onRead]);
  return null;
}
