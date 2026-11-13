/**
 * What a holder of Atlas does to one entry, on its sheet and on its page:
 * confirm it, correct a fact, set its owner, draw or remove a relation, link
 * the page that documents it, merge it with another or split it, retire it or
 * bring it back, and, for an administrator, purge it.
 *
 * Every control is drawn for every reader and held with its reason where the
 * reader may not press it. Every write is the person's own claim: it outranks
 * what an integration or an agent reports and never erases it, so where the
 * two later differ both are shown and the entry waits in Review.
 */

import { useMemo, useState } from "react";
import {
  Button,
  Checkbox,
  ConfirmField,
  ConfirmModal,
  FormField,
  Input,
  Menu,
  SegmentedControl,
  Select,
  Skeleton,
  confirmMatches,
  type MenuEntry,
  type SelectOption,
} from "@crewlethq/ui";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import type {
  AtlasEntry,
  AtlasEntryAnswer,
  AtlasFact,
  AtlasFactView,
  AtlasRelation,
  AtlasSummary,
} from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import { LABEL_MAX, WAITING, WriteDialog } from "./AddEntry.tsx";
import { ClaimList } from "./Decisions.tsx";
import { EntryPicker, OwnerField, PageField, type OwnerChoice } from "./pickers.tsx";
import { FactValue } from "./parts.tsx";
import { KINDS, RELATIONS, entries, factLabel, valueWord } from "./words.ts";
import { entryArg, factSpec, factsOf, relationsFrom } from "./writes.ts";

/** Why a purge is held for anybody but an administrator. */
export const PURGE_HELD = "Purging takes Admin access.";

type Dialog =
  "correct" | "owner" | "relate" | "unrelate" | "page" | "merge" | "split" | "retire" | "purge";

/** The names of the entries one entry's relations point at, by id. */
export type Names = ReadonlyMap<string, string>;

export function EntryActions({ answer, names }: { answer: AtlasEntryAnswer; names: Names }) {
  const { entry } = answer;
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const confirm = useAct("atlas_confirm");
  const restore = useAct("atlas_restore");
  const correct = useWriteAccess("atlas_correct");
  const owner = useWriteAccess("atlas_set_owner");
  const relate = useWriteAccess("atlas_relate");
  const unrelate = useWriteAccess("atlas_unrelate");
  const merge = useWriteAccess("atlas_merge");
  const split = useWriteAccess("atlas_split");
  const retire = useWriteAccess("atlas_retire");
  const admin = (useHeldGrants() ?? []).includes("admin");
  const subject = entryArg(entry.ref);
  const retired = entry.state === "retired";
  const drawn = removable(entry);
  const draws = relationsFrom(entry.ref.kind).length > 0;
  const close = () => setDialog(null);
  const items: MenuEntry[] = [
    { key: "owner", label: "Set owner", onSelect: () => setDialog("owner"), ...menuHold(owner) },
    {
      key: "relate",
      label: "Add relation",
      onSelect: () => setDialog("relate"),
      ...(draws
        ? menuHold(relate)
        : {
            disabled: true,
            description: `A ${KINDS[entry.ref.kind].word.toLowerCase()} draws no relation to another entry.`,
          }),
    },
    {
      key: "unrelate",
      label: "Remove relation",
      onSelect: () => setDialog("unrelate"),
      ...(drawn.length > 0
        ? menuHold(unrelate)
        : { disabled: true, description: "It draws no relation to remove." }),
    },
    { key: "page", label: "Link a page", onSelect: () => setDialog("page"), ...menuHold(relate) },
    { kind: "separator", key: "identity" },
    { key: "merge", label: "Merge with", onSelect: () => setDialog("merge"), ...menuHold(merge) },
    { key: "split", label: "Split", onSelect: () => setDialog("split"), ...menuHold(split) },
    { kind: "separator", key: "life" },
    retired
      ? {
          key: "restore",
          label: "Restore",
          onSelect: () =>
            void restore.run({ atlas_entry: subject }, { done: `Restored ${entry.name}` }),
          ...menuHold(restore.access),
        }
      : {
          key: "retire",
          label: "Retire",
          onSelect: () => setDialog("retire"),
          ...menuHold(retire),
        },
    {
      key: "purge",
      label: "Purge",
      danger: true,
      onSelect: () => setDialog("purge"),
      ...(admin ? {} : { disabled: true, description: PURGE_HELD }),
    },
  ];
  return (
    <div className="atlas-actions">
      <div className="atlas-actions__row">
        <WriteButton
          write={confirm}
          size="small"
          variant="secondary"
          showRefusal={false}
          blocked={
            retired
              ? "It is retired. Restore it first."
              : entry.state === "confirmed"
                ? "A person, the settings or the engine’s records already stand behind it."
                : undefined
          }
          onPress={() =>
            void confirm.run({ atlas_entry: subject }, { done: `Confirmed ${entry.name}` })
          }
        >
          Confirm
        </WriteButton>
        <Button
          size="small"
          variant="secondary"
          disabledReason={correct.can ? undefined : correct.reason}
          title={correct.can ? undefined : correct.reason}
          onClick={() => setDialog("correct")}
        >
          Correct
        </Button>
        <Menu label={`More to do with ${entry.name}`} trigger="More" align="end" items={items} />
      </div>
      <RefusalNote write={confirm} />
      <RefusalNote write={restore} />
      {dialog === "correct" && <CorrectDialog entry={entry} onClose={close} />}
      {dialog === "owner" && <OwnerDialog entry={entry} onClose={close} />}
      {dialog === "relate" && <RelateDialog entry={entry} onClose={close} />}
      {dialog === "unrelate" && (
        <UnrelateDialog entry={entry} drawn={drawn} names={names} onClose={close} />
      )}
      {dialog === "page" && <PageDialog entry={entry} onClose={close} />}
      {dialog === "merge" && <MergeDialog answer={answer} onClose={close} />}
      {dialog === "split" && <SplitDialog entry={entry} onClose={close} />}
      {dialog === "retire" && <RetireDialog entry={entry} onClose={close} />}
      {dialog === "purge" && <PurgeDialog entry={entry} onClose={close} />}
    </div>
  );
}

/** The relations of an entry a person may remove by name: to an entry, or to a page. */
function removable(entry: AtlasEntry) {
  return entry.relations.filter(
    (edge) => edge.relation !== "owned_by" && (edge.to || (edge.external === "page" && edge.name)),
  );
}

/** A fact's value as a form writes it. */
function wordOf(fact: AtlasFact, text: string): string {
  return factSpec(fact).type === "word" ? valueWord(text) : text;
}

/**
 * Correct a fact: the value the person knows, beside what the atlas shows now
 * and every other value a source holds. The person's value is then shown; a
 * source that later reports another is shown beside it, and the entry waits
 * in Review.
 */
function CorrectDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_correct");
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const carried = useMemo(() => factsOf(entry.ref.kind), [entry.ref.kind]);
  const [fact, setFact] = useState<AtlasFact>(
    carried.includes("purpose") ? "purpose" : carried[0]!,
  );
  const [value, setValue] = useState("");
  const [wrong, setWrong] = useState(false);
  const [member, setMember] = useState(entryArg(entry.ref));
  const spec = factSpec(fact);
  const many = spec.traits.includes("many");
  // A LABEL IS SOMEBODY'S WORDS; every other value is written as a machine reads it.
  const prose = spec.type === "label";
  const perHost = spec.traits.includes("per_host") && entry.members.length > 1;
  const shown: AtlasFactView[] = entry.facts.filter(
    (view) => view.fact === fact && (!perHost || !view.of || entryArg(view.of) === member),
  );
  const subject = perHost ? member : entryArg(entry.ref);
  const label = factLabel({ fact });
  const blocked = wrong
    ? shown.length === 0
      ? "The atlas shows no value to call wrong"
      : many && value === ""
        ? "Pick the value that is wrong"
        : undefined
    : value.trim() === ""
      ? "Give the value"
      : undefined;
  return (
    <WriteDialog
      title={`Correct ${entry.name}`}
      subtitle="Your value is shown from now on. If a source later reports another, both are shown and the entry waits in Review. Nothing settles in silence."
      write={write}
      blocked={blocked}
      submitLabel={wrong ? "Record it as wrong" : "Record the value"}
      onClose={onClose}
      onSubmit={() =>
        write.run(
          wrong
            ? { atlas_entry: subject, fact, wrong: true, ...(many ? { value } : {}) }
            : { atlas_entry: subject, fact, value: value.trim() },
          {
            done: wrong
              ? `Recorded that ${label} of ${entry.name} is wrong`
              : `Set ${label} of ${entry.name}`,
          },
        )
      }
    >
      <FormField label="Fact" required>
        {(field) => (
          <Select
            id={field.id}
            value={fact}
            searchable
            options={carried.map((one) => ({ value: one, label: factLabel({ fact: one }) }))}
            onChange={(next) => {
              setFact(next as AtlasFact);
              setValue("");
              setWrong(false);
            }}
          />
        )}
      </FormField>
      {perHost && (
        <FormField label="On" required helper="Each host of this entry keeps its own value.">
          {(field) => (
            <Select
              id={field.id}
              value={member}
              options={entry.members.map((one) => ({ value: entryArg(one), label: one.key }))}
              onChange={(next) => setMember(String(next))}
            />
          )}
        </FormField>
      )}
      <FormField label="The atlas shows" as="div">
        {shown.length === 0 ? (
          <p className="muted">No source says it yet.</p>
        ) : (
          <div className="atlas-compare">
            {shown.map((view) => (
              <ClaimList
                key={`${view.value.text}:${view.of?.key ?? ""}`}
                shown={view}
                others={view.others ?? []}
                seatName={seatName}
                now={now}
              />
            ))}
          </div>
        )}
      </FormField>
      {wrong && many ? (
        <FormField label="The value that is wrong" required>
          {(field) => (
            <Select
              id={field.id}
              value={value}
              placeholder="Pick a value"
              options={shown.map((view) => ({
                value: view.value.text,
                label: wordOf(fact, view.value.text),
              }))}
              onChange={(next) => setValue(String(next))}
            />
          )}
        </FormField>
      ) : (
        !wrong && (
          <FormField
            label={many ? "A value to add" : "Your value"}
            required
            limit={prose ? { length: [...value].length, max: LABEL_MAX } : undefined}
            helper={
              many
                ? "This fact holds several values. Yours is added beside the others."
                : "Written as the fact’s own grammar takes it. The engine refuses anything shaped like a credential."
            }
          >
            {(field) =>
              spec.words.length > 0 ? (
                <Select
                  id={field.id}
                  value={value}
                  placeholder="Pick a value"
                  options={spec.words.map((word) => ({ value: word, label: valueWord(word) }))}
                  onChange={(next) => setValue(String(next))}
                />
              ) : (
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  className={prose ? undefined : "mono"}
                  autoCapitalize="none"
                  spellCheck={prose}
                  value={value}
                  onChange={(event) => setValue(event.target.value)}
                />
              )
            }
          </FormField>
        )
      )}
      <Checkbox
        label="The value shown is wrong, and I do not know the right one"
        description="It is recorded as wrong and no longer shown, with its history kept."
        checked={wrong}
        disabled={shown.length === 0}
        onCheckedChange={(checked) => {
          setWrong(checked);
          setValue("");
        }}
      />
    </WriteDialog>
  );
}

function OwnerDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_set_owner");
  const held = entry.relations.find(
    (edge) => edge.relation === "owned_by" && !edge.suggested && edge.name,
  );
  const before: OwnerChoice =
    held && (held.external === "seat" || held.external === "unit")
      ? { kind: held.external, name: held.name ?? "" }
      : null;
  const [owner, setOwner] = useState<OwnerChoice>(before);
  const same = owner?.kind === before?.kind && owner?.name === before?.name;
  return (
    <WriteDialog
      title={`Who owns ${entry.name}`}
      subtitle="One seat or one unit of the chart, in place of any owner a person named before."
      size="sm"
      write={write}
      blocked={same ? "That is its owner now" : undefined}
      submitLabel={owner ? "Set owner" : "Clear the owner"}
      onClose={onClose}
      onSubmit={() =>
        write.run(
          owner
            ? { atlas_entry: entryArg(entry.ref), [owner.kind]: owner.name }
            : { atlas_entry: entryArg(entry.ref), clear: true },
          { done: owner ? `Set the owner of ${entry.name}` : `Cleared the owner of ${entry.name}` },
        )
      }
    >
      <OwnerField value={owner} onChange={setOwner} nobody="No owner" />
    </WriteDialog>
  );
}

function RelateDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_relate");
  const offered = useMemo(() => relationsFrom(entry.ref.kind), [entry.ref.kind]);
  const [relation, setRelation] = useState<AtlasRelation>(offered[0]!.relation);
  const [to, setTo] = useState<{ id: string; name: string }>({ id: "", name: "" });
  const kinds = offered.find((one) => one.relation === relation)?.to ?? [];
  return (
    <WriteDialog
      title={`Add a relation from ${entry.name}`}
      subtitle="A relation points from what relies to what is relied on, and is drawn as confirmed by you."
      write={write}
      blocked={to.id === "" ? "Pick the entry it points at" : undefined}
      submitLabel="Add relation"
      onClose={onClose}
      onSubmit={() =>
        write.run(
          { atlas_entry: entryArg(entry.ref), relation, to: to.id },
          { done: `Recorded that ${entry.name} ${RELATIONS[relation].toLowerCase()} ${to.name}` },
        )
      }
    >
      <FormField label="Relation" required>
        {(field) => (
          <Select
            id={field.id}
            value={relation}
            options={offered.map((one) => ({
              value: one.relation,
              label: RELATIONS[one.relation],
              description: `To ${one.to.map((kind) => KINDS[kind].word.toLowerCase()).join(", ")}`,
            }))}
            onChange={(next) => {
              setRelation(next as AtlasRelation);
              setTo({ id: "", name: "" });
            }}
          />
        )}
      </FormField>
      <EntryPicker
        label="Entry"
        helper={`What ${entry.name} ${RELATIONS[relation].toLowerCase()}.`}
        kinds={kinds}
        exclude={entry.id}
        value={to.id}
        onChange={(id, summary) => setTo({ id, name: summary?.name ?? "" })}
      />
    </WriteDialog>
  );
}

function UnrelateDialog({
  entry,
  drawn,
  names,
  onClose,
}: {
  entry: AtlasEntry;
  drawn: ReturnType<typeof removable>;
  names: Names;
  onClose: () => void;
}) {
  const write = useAct("atlas_unrelate");
  const [picked, setPicked] = useState("");
  const options: SelectOption[] = drawn.map((edge, i) => ({
    value: String(i),
    label: `${RELATIONS[edge.relation]} ${
      edge.to ? (names.get(edge.to) ?? "an entry you are not shown") : "a Doclet page"
    }`,
    description: edge.to ? undefined : edge.name,
  }));
  const edge = picked === "" ? undefined : drawn[Number(picked)];
  return (
    <WriteDialog
      title={`Remove a relation from ${entry.name}`}
      subtitle="It is recorded that the relation does not hold. What a source reported stays in the entry’s history."
      size="sm"
      write={write}
      blocked={edge ? undefined : "Pick the relation to remove"}
      submitLabel="Remove relation"
      onClose={onClose}
      onSubmit={async () =>
        edge
          ? write.run(
              {
                atlas_entry: entryArg(entry.ref),
                relation: edge.relation,
                ...(edge.to ? { to: edge.to } : { doclet_page: edge.name }),
              },
              { done: `Removed a relation from ${entry.name}` },
            )
          : null
      }
    >
      <FormField label="Relation" required>
        {(field) => (
          <Select
            id={field.id}
            value={picked}
            placeholder="Pick a relation"
            options={options}
            onChange={(next) => setPicked(String(next))}
          />
        )}
      </FormField>
    </WriteDialog>
  );
}

function PageDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_relate");
  const [page, setPage] = useState<{ id: string; title: string } | null>(null);
  return (
    <WriteDialog
      title={`Link a page to ${entry.name}`}
      subtitle="The atlas keeps one line about an entry. Anything longer lives on a Doclet page, linked here."
      write={write}
      blocked={page ? undefined : "Pick the page"}
      submitLabel="Link the page"
      onClose={onClose}
      onSubmit={async () =>
        page
          ? write.run(
              { atlas_entry: entryArg(entry.ref), relation: "documented_in", doclet_page: page.id },
              { done: `Linked ${page.title} to ${entry.name}` },
            )
          : null
      }
    >
      <PageField value={page} onChange={setPage} />
    </WriteDialog>
  );
}

/** One side of a merge or a split, as the preview draws it. */
function Side({ title, entry }: { title: string; entry: AtlasEntry }) {
  return (
    <div className="atlas-side">
      <span className="t-label">{title}</span>
      <strong>{entry.name}</strong>
      <span className="muted">
        {KINDS[entry.ref.kind].word} · <span className="mono">{entry.ref.key}</span>
      </span>
      <span className="muted">
        {entry.facts.length.toLocaleString()} {entry.facts.length === 1 ? "fact" : "facts"},{" "}
        {entry.relations.length.toLocaleString()}{" "}
        {entry.relations.length === 1 ? "relation" : "relations"}, from{" "}
        {entry.sources.length.toLocaleString()} {entry.sources.length === 1 ? "source" : "sources"}
      </span>
    </div>
  );
}

/** The facts of one value two entries both carry and differ on. */
function differing(one: AtlasEntry, other: AtlasEntry) {
  return one.facts.flatMap((mine) => {
    if (factSpec(mine.fact).traits.some((trait) => trait === "many" || trait === "per_host")) {
      return [];
    }
    const theirs = other.facts.find((view) => view.fact === mine.fact);
    return theirs && theirs.value.text !== mine.value.text ? [{ mine, theirs }] : [];
  });
}

/**
 * Merge: two entries of one kind said to be the same thing. THE PREVIEW IS
 * BEFORE THE PRESS: both entries side by side, and every fact they hold
 * different values of, which then stand on one entry.
 */
function MergeDialog({ answer, onClose }: { answer: AtlasEntryAnswer; onClose: () => void }) {
  const { entry } = answer;
  const write = useAct("atlas_merge");
  const [other, setOther] = useState<AtlasSummary | null>(null);
  const asked = useMemo(() => ({ id: other?.id ?? "" }), [other]);
  const read = useQuery("atlas_entry", asked, { enabled: other !== null });
  const theirs = other && read.data?.entry.id === other.id ? read.data.entry : null;
  const differ = theirs ? differing(entry, theirs) : [];
  return (
    <WriteDialog
      title={`Merge ${entry.name} with another entry`}
      subtitle="For two entries that are one thing and that no exact key joins."
      size="lg"
      write={write}
      blocked={!other ? "Pick the other entry" : !theirs ? "Reading the other entry" : undefined}
      submitLabel="Merge the two"
      onClose={onClose}
      onSubmit={async () =>
        theirs
          ? write.run(
              { atlas_entry: entryArg(entry.ref), other: entryArg(theirs.ref) },
              { done: `Merged ${entry.name} and ${theirs.name}` },
            )
          : null
      }
    >
      <EntryPicker
        label="The other entry"
        helper={`Another ${KINDS[entry.ref.kind].word.toLowerCase()}: two kinds are never one entry.`}
        kinds={[entry.ref.kind]}
        exclude={entry.id}
        value={other?.id ?? ""}
        onChange={(_id, summary) => setOther(summary)}
      />
      {other &&
        (theirs ? (
          <div className="atlas-preview" role="group" aria-label="What the merge does">
            <span className="t-label">Before it applies</span>
            <div className="atlas-preview__sides">
              <Side title="This entry" entry={entry} />
              <Side title="The other" entry={theirs} />
            </div>
            <p>
              They are shown as one entry, known by both keys, with the facts, relations and history
              of both. A &ldquo;not the same&rdquo; somebody recorded between them is withdrawn
              first.
            </p>
            {differ.length === 0 ? (
              <p className="muted">They hold no fact with two different values.</p>
            ) : (
              <>
                <p>
                  They differ on {differ.length.toLocaleString()}{" "}
                  {differ.length === 1 ? "fact" : "facts"}. The engine shows the value of the most
                  trusted source, a person&rsquo;s first, with the other beside it, and where two
                  equally trusted sources disagree the entry waits in Review.
                </p>
                <ul className="atlas-list">
                  {differ.map(({ mine, theirs: other }) => (
                    <li key={mine.fact}>
                      <strong>{factLabel(mine)}</strong>: <FactValue value={mine.value} /> here,{" "}
                      <FactValue value={other.value} /> on the other
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>
        ) : read.loading ? (
          <Skeleton variant="text" rows={3} label="Reading the other entry" />
        ) : (
          <p className="muted">{read.detail ?? "The other entry could not be read."}</p>
        ))}
    </WriteDialog>
  );
}

/**
 * Split: two keys said to be two things. An entry several keys merged into
 * names the two to keep apart; an entry of one key names another entry it
 * must never be merged with.
 */
function SplitDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_split");
  const merged = entry.members.length > 1;
  const [one, setOne] = useState(entryArg(entry.members[0] ?? entry.ref));
  const [two, setTwo] = useState(merged ? entryArg(entry.members[1]!) : "");
  const [other, setOther] = useState<AtlasSummary | null>(null);
  const members: SelectOption[] = entry.members.map((member) => ({
    value: entryArg(member),
    label: member.key,
  }));
  const second = merged ? two : other ? entryArg(other) : "";
  const secondName = merged ? two.slice(two.indexOf(":") + 1) : (other?.name ?? "");
  const firstName = merged ? one.slice(one.indexOf(":") + 1) : entry.name;
  return (
    <WriteDialog
      title={merged ? `Split ${entry.name}` : `Keep ${entry.name} apart from another entry`}
      subtitle="A person’s “not the same” always wins over a key two entries share and over a merge."
      write={write}
      blocked={
        second === ""
          ? "Pick the other one"
          : second === one
            ? "Pick two different keys"
            : undefined
      }
      submitLabel={merged ? "Split them" : "Keep them apart"}
      onClose={onClose}
      onSubmit={() =>
        write.run(
          { atlas_entry: one, other: second },
          { done: `Recorded that ${firstName} and ${secondName} are not the same` },
        )
      }
    >
      {merged ? (
        <>
          <p className="muted">
            Sources name this entry by {entries(entry.members.length).replace("entries", "keys")}.
            Pick the two that are not one thing.
          </p>
          <FormField label="This key" required>
            {(field) => (
              <Select
                id={field.id}
                value={one}
                options={members}
                onChange={(next) => setOne(String(next))}
              />
            )}
          </FormField>
          <FormField label="Is not the same as" required>
            {(field) => (
              <Select
                id={field.id}
                value={two}
                options={members}
                onChange={(next) => setTwo(String(next))}
              />
            )}
          </FormField>
        </>
      ) : (
        <EntryPicker
          label="The other entry"
          helper={`Another ${KINDS[entry.ref.kind].word.toLowerCase()} it must never be merged with.`}
          kinds={[entry.ref.kind]}
          exclude={entry.id}
          value={other?.id ?? ""}
          onChange={(_id, summary) => setOther(summary)}
        />
      )}
      {second !== "" && second !== one && (
        <div className="atlas-preview" role="group" aria-label="What the split does">
          <span className="t-label">Before it applies</span>
          <p>
            <span className="mono">{firstName}</span> and <span className="mono">{secondName}</span>{" "}
            are shown as {merged ? "two entries, where they are one now" : "two entries, as now"},
            and are never merged again, whatever key they share. Each keeps the facts, relations and
            history recorded under its own key.
          </p>
        </div>
      )}
    </WriteDialog>
  );
}

function RetireDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_retire");
  return (
    <ConfirmModal
      open
      title={`Retire ${entry.name}?`}
      message="It leaves the map at once. It stays in its history and under the retired entries, and Restore brings it back."
      confirmLabel="Retire"
      cancelLabel="Keep it"
      onClose={onClose}
      submitting={write.busy}
      busyReason={WAITING}
      confirmDisabled={!write.access.can}
      confirmDisabledReason={write.access.can ? undefined : write.access.reason}
      onConfirm={async () => {
        const result = await write.run(
          { atlas_entry: entryArg(entry.ref) },
          { done: `Retired ${entry.name}` },
        );
        if (result?.kind === "refused") throw new Error(result.sentence);
        onClose();
      }}
    >
      <RefusalNote write={write} />
    </ConfirmModal>
  );
}

/**
 * Purge: an administrator erases an entry, or one value wherever it appears,
 * on every node and from the history. Nothing undoes it, so it is worded as
 * what it is, typed back to confirm, and sent only after the engine's fresh
 * sign-in check.
 */
function PurgeDialog({ entry, onClose }: { entry: AtlasEntry; onClose: () => void }) {
  const write = useAct("atlas_purge");
  const [what, setWhat] = useState<"entry" | "value">("entry");
  const [value, setValue] = useState("");
  const [typed, setTyped] = useState("");
  const values = [...new Set(entry.facts.map((view) => view.value.text))];
  const phrase = what === "entry" ? entry.ref.key : value;
  const blocked =
    what === "value" && value === ""
      ? "Pick the value to erase"
      : confirmMatches(typed, phrase)
        ? undefined
        : "Type it back to confirm";
  return (
    <WriteDialog
      title={`Purge from the atlas`}
      subtitle="For something that must not be kept: a name recorded by mistake, or a value that turned out to be a credential."
      write={write}
      blocked={blocked}
      submitLabel="Erase for good"
      danger
      alert
      onClose={onClose}
      onSubmit={() =>
        write.run(what === "entry" ? { atlas_entry: entryArg(entry.ref) } : { value }, {
          done: what === "entry" ? `Purged ${entry.name}` : "Purged a value from the atlas",
        })
      }
    >
      <SegmentedControl
        label="What to erase"
        semantics="radio"
        value={what}
        onValueChange={(next) => {
          setWhat(next as "entry" | "value");
          setTyped("");
        }}
        options={[
          { value: "entry", label: "This entry" },
          { value: "value", label: "One value" },
        ]}
      />
      {what === "value" && (
        <FormField
          label="Value"
          required
          helper="Exactly as the atlas holds it. It is erased from every entry that carries it."
        >
          {(field) => (
            <Select
              id={field.id}
              value={value}
              placeholder="Pick a value"
              options={values.map((text) => ({ value: text, label: text }))}
              onChange={(next) => {
                setValue(String(next));
                setTyped("");
              }}
            />
          )}
        </FormField>
      )}
      <p>
        {what === "entry"
          ? `This erases ${entry.name} and everything recorded about it, on every node, its history included, and keeps it from being recorded again.`
          : "This erases the value from every entry and from the history, on every node, and keeps it from being recorded again."}{" "}
        <strong>Nothing undoes it.</strong> The engine asks you to confirm who you are before it
        runs.
      </p>
      {phrase !== "" && <ConfirmField phrase={phrase} value={typed} onValueChange={setTyped} />}
    </WriteDialog>
  );
}
