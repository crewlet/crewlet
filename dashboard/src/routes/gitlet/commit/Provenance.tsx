/** How a commit arrived and how it landed, in sentences: who pushed it, how and to which branches,
 *  and each change it belongs to with the merge and the gate it went through, as rows keep them. */

import { Fragment, type ReactNode } from "react";
import { Card } from "@crewlethq/ui";
import { GitCommitVerticalGlyph, GitMergeGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { AgoTime } from "~/components/AgoTime.tsx";
import { changePath, commitPath, parseChangeRef, refName, shortOID } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import type {
  CodeChangeLanding,
  CodeCheckRollup,
  CodeCommitAnswer,
  CodePushCommand,
} from "~/protocol/index.ts";
import { ChangeStateTag } from "../ChangeRow.tsx";
import { Face, GITLET, RefName, useWhoName, type Who } from "../ui.tsx";
import "~/styles/gitlet.css";

/** Every way a push's commits came to be, in words (`gitlet.PushVias`). */
export const VIA_WORDS: Readonly<Record<string, string>> = {
  git: "a git push",
  engine: "a commit the engine built from a tool's call",
  merge: "a change merged",
  expiry: "a ref expired",
  queue: "a merge queue landed it",
  import: "imported from another host",
  mirror: "synced from the repository it mirrors",
};

/** One piece of a gate's sentence: words, a seat or person named, or a commit linked. */
export type GatePart = string | { who: string; person: boolean } | { commit: string };

/** The merger as a part: a seat by its chart name, a person by their handle, else "A person". */
const mergerOf = (l: CodeChangeLanding): GatePart =>
  l.merged_by ? { who: l.merged_by, person: l.merged_by_kind === "person" } : "A person";

/** "A", "A and B", "A, B and C", with "and more" where the list is bounded. */
function listed(names: readonly GatePart[], more: boolean): GatePart[] {
  const out: GatePart[] = [];
  names.forEach((n, i) => {
    if (i > 0) out.push(i === names.length - 1 && !more ? " and " : ", ");
    out.push(n);
  });
  if (more) out.push(" and more");
  return out;
}

/** Where a rollup of checks stood, in words; `gone` once retention has swept them. */
export function checksWords(checks: CodeCheckRollup | undefined, gone: boolean): string {
  if (!checks) return gone ? "its checks are no longer kept" : "no check ran on what it merged";
  const read = checks.more ? ", of the checks read" : "";
  switch (checks.state) {
    case "passed": {
      if (checks.required > 0) {
        return `${checks.required_passed} of ${plural(checks.required, "required check")} passed${read}`;
      }
      const n = (checks.counts.passed ?? 0) + (checks.counts.skipped ?? 0);
      return `${plural(n, "check")} passed${read}`;
    }
    case "failing": {
      const names = (checks.failing ?? []).map((f) => f.name);
      if (names.length === 0) {
        return `${plural(checks.counts.failed ?? 0, "check")} failed${read}`;
      }
      const more = checks.failing_more ? [`${fmtCount(checks.failing_more)} more`] : [];
      const all = [...names, ...more];
      const said = all.length === 1 ? all[0]! : `${all.slice(0, -1).join(", ")} and ${all.at(-1)!}`;
      return `${said} failed${read}`;
    }
    case "running":
      return "its checks were still running";
    case "cancelled":
      return "its checks were cancelled";
  }
  return "its checks had not finished";
}

/** The gate a landing went through, as one sentence of parts (`GatePart`). */
export function gateWords(l: CodeChangeLanding): GatePart[] {
  if (l.merged_via === "push") return ["It merged when its head was pushed to its branch."];
  const gate = l.gate ?? {};
  const names = (gate.approvals ?? []).map((a) => ({ who: a.handle, person: a.kind === "person" }));
  const approvals: GatePart[] =
    names.length > 0
      ? ["approved by ", ...listed(names, Boolean(gate.approvals_more))]
      : ["with no approval"];
  const checks = checksWords(gate.checks, Boolean(gate.gone));
  const train: GatePart[] =
    l.merged_via === "queue" && gate.checks_on && gate.checks
      ? ["the train's checks on ", { commit: gate.checks_on }, ` ${checks}`]
      : [checks];
  const merger = mergerOf(l);
  if (l.override_reason) {
    return [
      merger,
      ` merged it past its gate: “${l.override_reason}”. As it stood: `,
      ...approvals,
      "; ",
      ...train,
      ".",
    ];
  }
  const how =
    l.merged_via === "queue"
      ? " added it to the merge queue, and it landed through its gate: "
      : " merged it through its gate: ";
  return [merger, how, ...approvals, "; ", ...train, "."];
}

/** A change a commit belongs to: its reference linking its page, titled, then its pill. */
function ChangeRef({ change }: { change: CodeChangeLanding }) {
  const at = parseChangeRef(change.ref);
  return (
    <>
      {at ? (
        <a
          className="mono t-link code-prov__ref"
          href={href(changePath(at.repo, at.number))}
          title={change.title}
        >
          {change.ref}
        </a>
      ) : (
        <strong className="mono">{change.ref}</strong>
      )}{" "}
      <ChangeStateTag state={change.state} />
    </>
  );
}

/** Someone a sentence names, with their face, by name. */
function Named({ who, person }: { who: Who; person?: boolean }) {
  const whoName = useWhoName();
  return (
    <span className="code-prov__who">
      <Face who={who} size="text" />{" "}
      <strong>{person && typeof who === "string" ? who : whoName(who)}</strong>
    </span>
  );
}

/** A gate's parts, drawn: names with faces, a commit linked to its page. */
function Gate({ repo, parts }: { repo: string; parts: readonly GatePart[] }) {
  return (
    <>
      {parts.map((p, i) =>
        typeof p === "string" ? (
          <Fragment key={i}>{p}</Fragment>
        ) : "commit" in p ? (
          <a
            key={i}
            className="mono t-link"
            href={href(commitPath(repo, p.commit))}
            title={p.commit}
          >
            {shortOID(p.commit)}
          </a>
        ) : (
          <Named key={i} who={p.who} person={p.person} />
        ),
      )}
    </>
  );
}

/** One row: its glyph in a quiet disc, its sentence, a second line, and its time at the end. */
function Row({
  glyph,
  children,
  more,
  at,
}: {
  glyph: "push" | "merge" | "revision";
  children: ReactNode;
  more?: ReactNode;
  at?: string | undefined;
}) {
  const Glyph =
    glyph === "push" ? UploadGlyph : glyph === "merge" ? GitMergeGlyph : GitCommitVerticalGlyph;
  return (
    <li className="code-prov__row">
      <span className="code-prov__disc" aria-hidden="true">
        <Glyph size="sm" />
      </span>
      <p className="code-prov__sentence">{children}</p>
      {more && <p className="code-prov__more">{more}</p>}
      {at && (
        <span className="code-prov__time">
          <AgoTime value={at} clock />
        </span>
      )}
    </li>
  );
}

/** Refs a push moved, each as its chip: "to main, v1.2". */
function Refs({ moved }: { moved: readonly CodePushCommand[] }) {
  return (
    <>
      {moved.map((cmd, i) => (
        <Fragment key={cmd.ref}>
          {i > 0 && ", "}
          <RefName gitRef={cmd.ref} />
        </Fragment>
      ))}
    </>
  );
}

/** "it moved one branch", "it moved 2 refs". */
function movedWords(moved: readonly CodePushCommand[]): string {
  const branches = moved.every((c) => refName(c.ref).kind === "branch");
  const n = moved.length === 1 ? "one" : fmtCount(moved.length);
  const what = branches
    ? moved.length === 1
      ? "branch"
      : "branches"
    : moved.length === 1
      ? "ref"
      : "refs";
  return `it moved ${n} ${what}`;
}

/** How the commit was pushed: who, the run behind a seat's push, how, and where to. */
function PushRow({ answer }: { answer: CodeCommitAnswer }) {
  const by = answer.pushed_by;
  if (!by) {
    return (
      <Row glyph="push">
        {answer.coverage?.complete
          ? "No push the index keeps introduced it: it came in a push that brought more commits than the index lists, or before the push history the repository keeps."
          : "The index of pushes on the node that answered has not read the push that introduced it yet."}
      </Row>
    );
  }
  const push = answer.push;
  const run = push?.run ?? by.principal.run;
  const seat = by.principal.seat;
  const operator = by.principal.operator_id;
  const moved = (push?.commands ?? []).filter((cmd) => cmd.result === "ok");
  const to = moved.length > 0 && (
    <>
      {" to "}
      <Refs moved={moved} />
    </>
  );
  const words = VIA_WORDS[by.via] ?? by.via;
  const via = words.charAt(0).toUpperCase() + words.slice(1);
  const second = (
    <>
      {via}
      {push
        ? moved.length > 0
          ? ` · ${movedWords(moved)}`
          : ""
        : " · which refs it moved is no longer kept"}
      {run?.run_id && run.turn_id && (
        <>
          {" · executor "}
          <a className="mono t-link" href={href(["live", "executors", run.turn_id])}>
            {run.run_id}
          </a>
        </>
      )}
    </>
  );
  if (by.via === "import" || by.via === "mirror") {
    return (
      <Row glyph="push" more={second} at={by.at}>
        {by.via === "import"
          ? "Imported from another host"
          : "Synced from the repository it mirrors"}
        {to}
      </Row>
    );
  }
  const pusher = seat ? (
    <>
      <Named who={seat} />
      {operator && <span className="mono"> via {operator}</span>}
    </>
  ) : (
    <strong className="mono">{operator ?? "someone"}</strong>
  );
  // CLAUSES BY COMMAS: who, the run, how; git's "with git" binds to the pusher.
  const clauses: ReactNode[] = [pusher];
  if (run?.turn_id) {
    clauses.push(
      <a className="t-link" href={href(["live", "turns", run.turn_id])}>
        {run.work_key ? (
          <>
            a run for <strong className="mono">{run.work_key}</strong>
          </>
        ) : (
          "a run"
        )}
      </a>,
    );
  }
  if (by.via === "engine") clauses.push("via the engine");
  if (by.via === "git" && clauses.length > 1) clauses.push("with git");
  return (
    <Row glyph="push" more={second} at={by.at}>
      {"Pushed by "}
      {clauses.map((clause, i) => (
        <Fragment key={i}>
          {i > 0 && ", "}
          {clause}
        </Fragment>
      ))}
      {by.via === "git" && clauses.length === 1 && " with git"}
      {to && (clauses.length > 1 ? "," : "")}
      {to}
    </Row>
  );
}

/** Whether a landing carries the facts of how it merged; an older node answers none. */
const knownLanding = (l: CodeChangeLanding) => l.merged_via !== undefined;

const UNAVAILABLE = "How it landed is unavailable on this fleet yet.";

/** One change the commit merged: Gitlet's merge into its branch for the merger. */
function MergedRow({
  repo,
  landing,
  branch,
}: {
  repo: string;
  landing: CodeChangeLanding;
  branch: string | undefined;
}) {
  if (!knownLanding(landing)) {
    return (
      <Row glyph="merge" more={UNAVAILABLE}>
        {"Merged "}
        <ChangeRef change={landing} />
      </Row>
    );
  }
  const into = branch && (
    <>
      {" into "}
      <RefName gitRef={branch} />
    </>
  );
  return (
    <Row
      glyph="merge"
      more={<Gate repo={repo} parts={gateWords(landing)} />}
      at={landing.merged_at}
    >
      <Named who={GITLET} />
      {landing.merged_via === "queue" ? " landed " : " merged "}
      <ChangeRef change={landing} />
      {landing.merged_via === "queue" && " from the merge queue"}
      {into}
      {landing.merged_by && (
        <>
          {" for "}
          <Named who={landing.merged_by} person={landing.merged_by_kind === "person"} />
        </>
      )}
    </Row>
  );
}

/** A revision the commit heads: a merged change's landing, or the revision of a live one. */
function RevisionRow({ repo, change, n }: { repo: string; change: CodeChangeLanding; n: number }) {
  if (change.state === "merged" && knownLanding(change)) {
    return (
      <Row
        glyph="merge"
        more={<Gate repo={repo} parts={gateWords(change)} />}
        at={change.merged_at}
      >
        {"Landed with "}
        <ChangeRef change={change} />
        {change.merge_oid && change.merged_via !== "push" && (
          <>
            {" in merge commit "}
            <a
              className="mono t-link"
              href={href(commitPath(repo, change.merge_oid))}
              title={change.merge_oid}
            >
              {shortOID(change.merge_oid)}
            </a>
            {" by "}
            <Named who={GITLET} />
          </>
        )}
      </Row>
    );
  }
  return (
    <Row glyph="revision" more={change.state === "merged" ? UNAVAILABLE : undefined}>
      {`Revision ${n} of `}
      <ChangeRef change={change} />
    </Row>
  );
}

/** The provenance card: how the commit was pushed, then how it landed, a row each. */
export function Provenance({ answer }: { answer: CodeCommitAnswer }) {
  const c = answer.commit;
  const merged = answer.merged ?? [];
  const revisions = answer.revisions ?? [];
  // A MERGE COMMIT GITLET WROTE says itself in its merge row, never as a push too.
  const viaMerge = answer.pushed_by?.via === "merge" || answer.pushed_by?.via === "queue";
  const ownMerge = merged.some((m) => m.merge_oid === c.oid && knownLanding(m));
  const branch = (answer.push?.commands ?? []).find(
    (cmd) => cmd.result === "ok" && refName(cmd.ref).kind === "branch",
  )?.ref;
  return (
    <Card as="section" aria-label="Provenance" padding="none" className="code-prov-card">
      <ul className="code-prov">
        {!(viaMerge && ownMerge) && <PushRow answer={answer} />}
        {merged.map((m) => (
          <MergedRow
            key={`merged:${m.ref}`}
            repo={answer.repo}
            landing={m}
            branch={m.merge_oid === c.oid ? branch : undefined}
          />
        ))}
        {revisions.map((r) => (
          <RevisionRow
            key={`rev:${r.change.ref}:${r.n}`}
            repo={answer.repo}
            change={r.change}
            n={r.n}
          />
        ))}
        {merged.length === 0 && revisions.length === 0 && (
          <Row glyph="revision">
            No change names this commit: it is no change's revision or merge.
          </Row>
        )}
      </ul>
      {answer.more && (
        <p className="code-prov__bound t-caption">More changes name it than one page lists.</p>
      )}
    </Card>
  );
}
