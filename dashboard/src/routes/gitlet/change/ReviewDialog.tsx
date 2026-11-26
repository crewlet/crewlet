/** The review of a change's head: the three verdicts as menu items, held as the reader's access and
 *  authorship leave them, and the one dialog each opens. */
import { useState, type ReactNode } from "react";
import { Button, Composer, Menu, Stack, type MenuItem } from "@crewlethq/ui";
import {
  CheckGlyph,
  ChevronDownGlyph,
  FileDiffGlyph,
  MessageSquareGlyph,
} from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { authoredBy } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { draftedRevisions, findingOf, forgetDrafts, readDrafts } from "~/lib/reviewDrafts.ts";
import { useAct } from "~/lib/useAct.ts";
import { useDictation } from "~/lib/dictation/source.ts";
import { menuHold, type WriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import type { CodeChangeDetail, CodeVerdict } from "~/protocol/index.ts";
import { useStandingFindings } from "./reads.ts";

/** Why the reader's verdict on a change they wrote is held: an author's review counts for nothing,
 *  so the engine takes none (`own_change`), and the step that is theirs is asking somebody else. */
export const OWN_CHANGE_HELD =
  "You wrote this change, so your review would not count. Ask somebody else to review it.";

/** A menu item's hold: the reader's access first, then `blocked`'s sentence where it is given. */
export function heldItem(access: WriteAccess, blocked: string | undefined) {
  return access.can
    ? blocked === undefined
      ? {}
      : { disabled: true, description: blocked }
    : menuHold(access);
}

/** Approve, Request changes and Comment as menu items, each choosing its verdict. */
export function verdictItems(
  change: CodeChangeDetail,
  access: WriteAccess,
  viewer: ViewerState,
  choose: (verdict: CodeVerdict) => void,
): MenuItem[] {
  const blocked = authoredBy(change, viewer) ? OWN_CHANGE_HELD : undefined;
  const item = (key: CodeVerdict, label: string, icon: ReactNode) => ({
    key,
    label,
    icon,
    onSelect: () => choose(key),
    ...heldItem(access, blocked),
  });
  return [
    item("approve", "Approve", <CheckGlyph size="sm" />),
    item("request_changes", "Request changes", <FileDiffGlyph size="sm" />),
    item("comment", "Comment", <MessageSquareGlyph size="sm" />),
  ];
}

/**
 * Why a review is held as saying nothing: changes asked or a comment with no summary or finding.
 */
export function saysNothing(
  verdict: CodeVerdict,
  summary: string,
  findings: number,
): string | null {
  if (verdict === "approve" || summary.trim() !== "" || findings > 0) return null;
  return verdict === "request_changes"
    ? "Say what to change: write a summary, or draft a finding on its lines."
    : "Write the comment, or draft a finding on its lines.";
}

/** What each verdict's dialog is called, and what its press says it did. */
const VERDICT_TITLES: Readonly<Record<string, { title: string; done: string }>> = {
  approve: { title: "Approve", done: "Approved" },
  request_changes: { title: "Request changes", done: "Asked for changes on" },
  comment: { title: "Comment", done: "Reviewed" },
};

/** One review of the head: verdict, summary, drafts and the reader's standing findings it carries
 *  (checked by default; one left out is superseded), held until every standing one is read, and
 *  sent as ONE `code_review_change`, the drafts forgotten only once the engine takes it. */
export function ReviewDialog({
  change,
  verdict,
  onClose,
}: {
  change: CodeChangeDetail;
  verdict: CodeVerdict;
  onClose: () => void;
}) {
  const write = useAct("code_review_change");
  const viewer = useViewer();
  const [summary, setSummary] = useState("");
  const dictation = useDictation();
  const drafts = readDrafts(change.id, change.revision);
  const elsewhere = draftedRevisions(change.id).filter((d) => d.revision !== change.revision);
  const read = useStandingFindings(change, viewer.owner);
  const standing = read.findings ?? [];
  const [dropped, setDropped] = useState<ReadonlySet<string>>(new Set());
  const carries = standing.filter((f) => !dropped.has(f.id)).map((f) => f.id);
  const words = VERDICT_TITLES[verdict] ?? { title: "Review", done: "Reviewed" };
  const held =
    read.findings !== null
      ? (saysNothing(verdict, summary, drafts.length + carries.length) ?? undefined)
      : read.error === null
        ? "Reading your standing findings…"
        : "Your standing findings were not read: a review sent now would supersede those it leaves out.";
  const submit = async () => {
    // HELD HERE TOO, where Enter reaches it past the button.
    if (held !== undefined) return;
    const result = await write.run(
      {
        change: change.ref,
        revision: change.revision,
        verdict,
        ...(summary.trim() ? { summary } : {}),
        ...(drafts.length > 0 ? { findings: drafts.map(findingOf) } : {}),
        ...(carries.length > 0 ? { carries } : {}),
      },
      { done: `${words.done} ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      // FORGOTTEN ONCE TAKEN: an unknown answer keeps them, for the Retry
      // the toast offers sends the same review again.
      forgetDrafts(change.id, change.revision);
      onClose();
    }
  };
  return (
    <Modal
      open
      size="md"
      title={`${words.title} ${change.ref}`}
      subtitle={`Revision ${change.revision}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton write={write} variant="primary" blocked={held} onPress={() => void submit()}>
            {words.title}
          </WriteButton>
        </>
      }
    >
      <Composer
        label="Summary"
        placeholder="What you found, in a sentence or two"
        value={summary}
        rows={4}
        onChange={setSummary}
        dictation={dictation}
      />
      <p className="muted">
        {drafts.length === 0
          ? "No finding is drafted on this revision's lines."
          : `${drafts.length} drafted finding${drafts.length === 1 ? "" : "s"} on this revision will be raised with it.`}
        {elsewhere.length > 0 &&
          ` Drafts on revision ${elsewhere.map((d) => d.revision).join(", ")} stay in this tab: a review raises findings on the revision it judges.`}
      </p>
      {read.error !== null ? (
        <Stack gap={2}>
          <QueryState
            error={read.error}
            refusal={read.refusal}
            {...(read.detail ? { detail: read.detail } : {})}
            loading={false}
          >
            {null}
          </QueryState>
          <p className="muted">
            {held}{" "}
            <Button size="small" variant="secondary" onClick={read.retry}>
              Read them again
            </Button>
          </p>
        </Stack>
      ) : (
        held !== undefined && <p className="muted">{held}</p>
      )}
      {standing.length > 0 && (
        <fieldset className="code-carries">
          <legend>Your standing findings it keeps standing</legend>
          {standing.map((f) => (
            <label key={f.id} className="row gap-1">
              <input
                type="checkbox"
                checked={!dropped.has(f.id)}
                onChange={(event) =>
                  setDropped((was) => {
                    const next = new Set(was);
                    if (event.target.checked) next.delete(f.id);
                    else next.add(f.id);
                    return next;
                  })
                }
              />
              {f.title} <span className="muted mono">{f.path}</span>
            </label>
          ))}
        </fieldset>
      )}
    </Modal>
  );
}

/** Files changed's one primary: the head's drafts counted, each verdict opening the review dialog. */
export function FinishReview({ change, drafts }: { change: CodeChangeDetail; drafts: number }) {
  const [verdict, setVerdict] = useState<CodeVerdict | null>(null);
  const review = useAct("code_review_change");
  const viewer = useViewer();
  return (
    <>
      <Menu
        label={`Finish your review of ${change.ref}${drafts > 0 ? `, ${plural(drafts, "drafted finding")}` : ""}`}
        trigger={
          <span className="code-finish">
            Finish review
            {drafts > 0 && (
              <span className="code-finish__count" aria-hidden="true">
                {fmtCount(drafts)}
              </span>
            )}
          </span>
        }
        trailingIcon={<ChevronDownGlyph size="sm" />}
        triggerVariant="primary"
        align="end"
        items={verdictItems(change, review.access, viewer, setVerdict)}
      />
      {verdict && (
        <ReviewDialog change={change} verdict={verdict} onClose={() => setVerdict(null)} />
      )}
    </>
  );
}
