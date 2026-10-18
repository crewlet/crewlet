/**
 * The History tab: an artifact's versions, newest first, the one picked with
 * its files and what it declared, and the artifact's own change record.
 *
 * SELECTING IS NOT RESTORING. Picking a version shows it; Restore is its own
 * control and publishes the old version again as a new one, every file
 * carried, so nothing a reader does here rewrites history. A label names a
 * version, and a labelled version is kept past the retention that retires the
 * oldest unlabelled ones.
 *
 * DATA IS LIVE AND IS NOT VERSIONED WITH THE PAGE: every version shows
 * today's data, which the Data tab's history records cell by cell.
 *
 * COMPARING IS ADDRESSED: the version a reader compares the picked one with is
 * `against=` and the way they compare is `lens=`, so a link to a comparison
 * opens it ([ArtifactCompare]).
 */

import { useMemo, useState } from "react";
import { useParam } from "~/app/router.tsx";
import {
  Button,
  FormField,
  Input,
  Modal,
  VersionTimeline,
  useToast,
  type VersionItem,
} from "@crewlethq/ui";
import { QueryState, Section } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, plural, relTime, utf8Bytes } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { ARTIFACT_LABEL_MAX_BYTES } from "~/contract/artifacts.ts";
import type {
  ArtifactDetail,
  ArtifactVersionSummary,
  LogRefusal,
  QueryRefusal,
} from "~/protocol/index.ts";
import { labelVersion, restoreVersion } from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { CAPABILITY_WORDS, actorName, historyWords } from "~/lib/artifacts/words.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import type { PresenceReport } from "./presence.ts";
import {
  ArtifactCompare,
  COMPARE_LENSES,
  drawnSideBySide,
  type CompareLens,
} from "./ArtifactCompare.tsx";

export function HistoryTab({
  detail,
  versions,
  versionsError,
  versionsRefusal,
  showing,
  onShow,
  room,
  presence,
  bridgeStores,
}: {
  detail: ArtifactDetail;
  versions: ArtifactVersionSummary[];
  versionsError: string | null;
  versionsRefusal: QueryRefusal | LogRefusal | null;
  showing: number;
  onShow: (version: number) => void;
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const toast = useToast();
  const [picked, setPicked] = useState(showing);
  const [labelling, setLabelling] = useState<number | null>(null);
  const publish = useArtifactAccess({ app: true, role: "editor", held: detail.role });
  const update = useArtifactAccess({ role: "editor", held: detail.role });
  const restore = useKeyedGesture();
  const [againstParam, setAgainst] = useParam("against", "");
  const [lensParam, setLens] = useParam("lens", "source", "section");
  const lens: CompareLens = (COMPARE_LENSES as readonly string[]).includes(lensParam)
    ? (lensParam as CompareLens)
    : "source";
  // A VERSION TO COMPARE WITH is one this artifact still keeps and not the one
  // picked; anything else in the address compares nothing.
  const againstAsked = Number(againstParam);
  const against =
    Number.isInteger(againstAsked) &&
    againstAsked !== picked &&
    versions.some((v) => v.number === againstAsked)
      ? againstAsked
      : undefined;

  const items: VersionItem[] = useMemo(
    () =>
      versions.map((v) => ({
        version: v.number,
        kind: v.restored_from ? "restore" : "publish",
        ...(v.label ? { label: v.label } : {}),
        ...(v.forced ? { summary: "Published over a newer version" } : {}),
        author: people.seatPrincipal(v.author.name),
        at: v.created_at,
        ...(v.restored_from ? { restoredFrom: v.restored_from } : {}),
      })),
    [versions, people],
  );
  const oldest = versions.reduce((n, v) => Math.min(n, v.number), Number.POSITIVE_INFINITY);
  const current = versions.find((v) => v.number === picked);

  if (detail.kind === "sheet" && versions.length <= 1) {
    return (
      <>
        <p className="artifact-note">
          A sheet's content is its data, which is live: the Data tab keeps every change.
        </p>
        <ChangeRecord detail={detail} />
      </>
    );
  }
  return (
    <div className="artifact-history-tab">
      <QueryState
        error={versionsError}
        refusal={versionsRefusal}
        loading={versions.length === 0 && versionsError === null}
      >
        <VersionTimeline
          className="artifact-versions"
          versions={items}
          latest={detail.head_number}
          selected={picked}
          onSelect={setPicked}
          {...(against !== undefined ? { against } : {})}
          onCompare={(n: number | null) => setAgainst(n === null ? "" : String(n))}
          oldestKept={Number.isFinite(oldest) ? oldest : undefined}
          now={now}
          {...(publish.can
            ? {
                onRestore: async (n: number) => {
                  const answer = await restore.run(
                    restoreVersion(detail.id, n, detail.head_number),
                  );
                  if (answer?.kind === "done") {
                    toast.ok(
                      `Restored version ${n} as version ${String(answer.body.number ?? detail.head_number + 1)}`,
                    );
                  } else if (answer) {
                    toast.show({
                      variant: "danger",
                      title: "Could not restore it",
                      message: answer.text,
                    });
                  }
                },
              }
            : {})}
          {...(update.can ? { onLabel: (n: number) => setLabelling(n) } : {})}
        />
      </QueryState>
      <div className="artifact-version-detail">
        {current && (
          <Section
            title={`Version ${current.number}${current.label ? `: ${current.label}` : ""}`}
            hint={`${actorName(current.author, people.seatName)}, ${relTime(current.created_at, now)}`}
            actions={
              <Button size="small" variant="secondary" onClick={() => onShow(current.number)}>
                View it
              </Button>
            }
          >
            <VersionFiles detail={detail} version={current} />
          </Section>
        )}
        {!publish.can && <p className="artifact-note">{publish.reason}</p>}
        <ChangeRecord detail={detail} />
      </div>
      {against !== undefined && (
        <div className="artifact-compare-row">
          <ArtifactCompare
            detail={detail}
            picked={picked}
            against={against}
            lens={drawnSideBySide(detail.kind) ? lens : "source"}
            onLens={setLens}
            versions={versions}
            room={room}
            presence={presence}
            bridgeStores={bridgeStores}
          />
        </div>
      )}
      <LabelDialog
        detail={detail}
        version={labelling}
        current={versions.find((v) => v.number === labelling)?.label ?? ""}
        onClose={() => setLabelling(null)}
      />
    </div>
  );
}

/** One version's files and what it declared. */
function VersionFiles({
  detail,
  version,
}: {
  detail: ArtifactDetail;
  version: ArtifactVersionSummary;
}) {
  const answer = useQuery("artifact_version", { id: detail.id, n: version.number });
  const files = answer.data?.manifest.files ?? [];
  return (
    <QueryState error={answer.error} refusal={answer.refusal} loading={answer.loading}>
      <dl className="artifact-facts">
        <dt>Files</dt>
        <dd>
          {plural(version.files, "file")}, {fmtBytes(version.bytes)}
        </dd>
        {version.contract && (
          <>
            <dt>Runtime</dt>
            <dd>Contract {version.contract}</dd>
          </>
        )}
        <dt>Can</dt>
        <dd>
          {(version.capabilities ?? []).length === 0 ? (
            "Nothing beyond showing itself"
          ) : (
            <ul className="artifact-list" role="list">
              {(version.capabilities ?? []).map((c) => (
                <li key={c}>{CAPABILITY_WORDS[c]}</li>
              ))}
            </ul>
          )}
        </dd>
      </dl>
      {files.length > 0 && (
        <ul className="artifact-files" role="list">
          {files.map((f) => (
            <li key={f.path} className="artifact-file">
              <span className="mono truncate">{f.path}</span>
              <span className="artifact-file-meta">
                {f.content_type.split(";")[0]}, {fmtBytes(f.object.size)}
                {f.path === answer.data?.manifest.entry ? ", the entry" : ""}
              </span>
            </li>
          ))}
        </ul>
      )}
    </QueryState>
  );
}

/** The artifact's change record: every publish, share and trash, newest first, as the reader's role shows it. */
function ChangeRecord({ detail }: { detail: ArtifactDetail }) {
  const people = useArtifactPeople();
  const now = useNow();
  const answer = useQuery("artifact_history", { id: detail.id, limit: 50 });
  const entries = (answer.data?.entries ?? []).filter((e) => e.op !== "opened");
  const audienceName = (raw: string) => {
    if (raw === "company") return `everyone in ${people.company}`;
    const [kind, id] = raw.split(":");
    return kind === "unit" ? people.unitName(id ?? "") : people.seatName(id ?? "");
  };
  return (
    <Section title="Changes" hint="What was published, shared and changed, and by whom">
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        loading={answer.loading}
        empty={
          entries.length === 0
            ? { title: "Nothing recorded yet", hint: "Changes appear here as they are made." }
            : undefined
        }
      >
        <ol className="artifact-history" role="list">
          {entries.map((entry) => (
            <li key={entry.position} className="artifact-history-row">
              <span className="artifact-history-who">
                {actorName(entry.actor, people.seatName)}
              </span>
              <span className="artifact-history-what">
                {historyWords(entry, { audience: audienceName, seat: people.seatName })}
                {entry.work_item ? ` for ${entry.work_item}` : ""}
                {entry.elevated ? " (as an administrator)" : ""}
              </span>
              <time className="artifact-history-when" dateTime={entry.at} title={entry.at}>
                {relTime(entry.at, now)}
              </time>
            </li>
          ))}
        </ol>
      </QueryState>
    </Section>
  );
}

/** Names one version, or clears its name. */
function LabelDialog({
  detail,
  version,
  current,
  onClose,
}: {
  detail: ArtifactDetail;
  version: number | null;
  current: string;
  onClose: () => void;
}) {
  const [text, setText] = useState(current);
  const gesture = useKeyedGesture();
  const toast = useToast();
  const bytes = utf8Bytes(text.trim());
  const tooLong = bytes > ARTIFACT_LABEL_MAX_BYTES;
  const save = async () => {
    if (version === null || tooLong) return;
    const answer = await gesture.run(labelVersion(detail.id, version, text.trim()));
    if (answer?.kind === "done") {
      toast.ok(text.trim() ? `Labelled version ${version}` : `Cleared version ${version}'s label`);
      onClose();
    }
  };
  return (
    <Modal
      open={version !== null}
      onClose={onClose}
      title={version !== null ? `Label version ${version}` : ""}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            onClick={() => void save()}
            loading={gesture.busy}
            disabledReason={tooLong ? "The label is too long." : undefined}
          >
            Save
          </Button>
        </>
      }
    >
      <FormField
        label="Label"
        helper={`A labelled version is kept however many are published after it. At most ${ARTIFACT_LABEL_MAX_BYTES} bytes.`}
        error={
          tooLong
            ? `The label is ${bytes} bytes and the most is ${ARTIFACT_LABEL_MAX_BYTES}.`
            : gesture.answer?.kind === "refused" || gesture.answer?.kind === "unknown"
              ? gesture.answer.text
              : undefined
        }
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            width="full"
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Review draft"
          />
        )}
      </FormField>
    </Modal>
  );
}
