/**
 * The pane's head controls — Snooze, Done and the rest — each ONE gesture on
 * this person's own inbox (`mark_inbox`), naming exactly the notices the open
 * row holds.
 *
 * A GESTURE, NEVER THE DOCUMENT. Done marks these records read and nothing
 * else: every other mark, every snooze and the read position stay where they
 * are (`tracker.Writer.MarkInbox`). The write this replaced sent the whole
 * inbox back, so a Done pressed in one tab erased a snooze set in another.
 *
 * A DECISION HOLDS ITS NOTICES. An ask is marked through the notices that told
 * this person about it; a parked run, a stopped seat and a condition are not
 * notices at all — they leave the list when they are answered or cleared — so
 * their Snooze and Done are drawn disabled with that sentence rather than
 * hidden.
 */

import { useState } from "react";
import { Button, FormField, Input, Menu, Modal, type MenuEntry } from "@crewlethq/ui";
import { CheckGlyph, ClockGlyph, EllipsisGlyph, UndoGlyph } from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { fmtMinute, fromWall, toWall } from "~/lib/format.ts";
import { usePanelAlign } from "~/lib/media.ts";
import { useNavigator } from "~/app/router.tsx";
import { snoozePresets } from "./model.ts";
import type { WorkInboxNotice } from "~/protocol/index.ts";

/** Why a row that is not a notice cannot be marked. */
export const NOT_A_NOTICE =
  "This is not a notice — it leaves your inbox when it is answered or cleared.";

export function NoticeActions({
  notices,
  itemKey,
  now,
  zone,
  maxSnoozeAhead,
}: {
  /** The notices this row stands for; empty for a row that is not one. */
  notices: readonly WorkInboxNotice[];
  /** The work item the row is on, for "Open", or "". */
  itemKey: string;
  now: number;
  /** The company's clock, which "tomorrow at nine" is read on. */
  zone: string | undefined;
  /** The engine's bound on a snooze, in seconds, where it said one. */
  maxSnoozeAhead: number | undefined;
}) {
  const write = useAct("mark_inbox");
  const nav = useNavigator();
  const align = usePanelAlign("end");
  const [picking, setPicking] = useState(false);
  const ids = notices.map((n) => n.record_id);
  const unread = notices.filter((n) => !n.read).map((n) => n.record_id);
  const snoozed = notices.filter((n) => n.snoozed).map((n) => n.record_id);
  const label = notices.length === 1 ? "this notice" : `these ${notices.length} notices`;

  const snooze = (until: string, words: string) =>
    void write.run(
      { snooze: ids.map((record_id) => ({ record_id, until })) },
      { done: `Snoozed until ${words}` },
    );

  const presets = snoozePresets(now, zone, maxSnoozeAhead);
  const snoozeEntries: MenuEntry[] = [
    ...presets.map((p) => ({
      key: p.key,
      label: p.label,
      // A COMPANY-CLOCK PRESET SAYS ITS DAY AND WHOSE NINE O'CLOCK it is;
      // the hour says the reader's own time it comes back at.
      hint: p.day ? `${p.day} · company time` : fmtMinute(p.until),
      onSelect: () => snooze(p.until, fmtMinute(p.until)),
    })),
    { kind: "separator" as const, key: "sep" },
    { key: "pick", label: "Pick a date and time…", onSelect: () => setPicking(true) },
  ];

  const blockedAll = notices.length === 0 ? NOT_A_NOTICE : undefined;
  const more: MenuEntry[] = [
    ...(itemKey
      ? [{ key: "open", label: `Open ${itemKey}`, onSelect: () => nav.to(["work", itemKey]) }]
      : []),
    ...(notices.length > 0 && unread.length === 0
      ? [
          {
            key: "unread",
            label: "Mark unread",
            icon: <UndoGlyph size="sm" />,
            disabled: !write.access.can,
            hint: write.access.can ? undefined : write.access.reason,
            onSelect: () => void write.run({ unread: ids }, { done: "Marked unread" }),
          },
        ]
      : []),
  ];

  return (
    <div className="inbox-pane-actions">
      {snoozed.length > 0 ? (
        <WriteButton
          write={write}
          size="small"
          variant="ghost"
          leadingIcon={<ClockGlyph size="sm" />}
          showRefusal={false}
          onPress={() => void write.run({ unsnooze: snoozed }, { done: "Back in your inbox" })}
        >
          Unsnooze
        </WriteButton>
      ) : write.access.can && !blockedAll && !write.busy ? (
        <Menu
          label={`Snooze ${label}`}
          icon={<ClockGlyph size="sm" />}
          trigger="Snooze"
          triggerVariant="ghost"
          align={align}
          items={snoozeEntries}
        />
      ) : (
        // THE SAME CONTROL, DISABLED WITH ITS REASON: a reader who cannot act,
        // or a row that is not a notice, still sees that Snooze exists and why
        // it is not theirs to press.
        <WriteButton
          write={write}
          size="small"
          variant="ghost"
          leadingIcon={<ClockGlyph size="sm" />}
          showRefusal={false}
          blocked={blockedAll}
          onPress={() => undefined}
        >
          Snooze
        </WriteButton>
      )}
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        leadingIcon={<CheckGlyph size="sm" />}
        showRefusal={false}
        blocked={
          blockedAll ??
          (unread.length === 0 ? "Already read — nothing here to mark done." : undefined)
        }
        onPress={() => void write.run({ read: unread }, { done: "Marked done" })}
      >
        Done
      </WriteButton>
      {more.length > 0 && (
        <Menu
          label="More"
          icon={<EllipsisGlyph size="sm" />}
          triggerVariant="ghost"
          align={align}
          items={more}
        />
      )}
      <RefusalNote write={write} />
      {picking && (
        <PickDialog
          now={now}
          maxSnoozeAhead={maxSnoozeAhead}
          onClose={() => setPicking(false)}
          onPick={(until) => {
            setPicking(false);
            snooze(until, fmtMinute(until));
          }}
        />
      )}
    </div>
  );
}

/**
 * A snooze to any moment inside the engine's bound, in the READER's own clock —
 * the one their preference names, which is what a date typed into a field
 * means to them.
 */
function PickDialog({
  now,
  maxSnoozeAhead,
  onClose,
  onPick,
}: {
  now: number;
  maxSnoozeAhead: number | undefined;
  onClose: () => void;
  onPick: (until: string) => void;
}) {
  const [wall, setWall] = useState(toWall(now + 3_600_000));
  const at = fromWall(wall);
  const limit = maxSnoozeAhead && maxSnoozeAhead > 0 ? now + maxSnoozeAhead * 1_000 : null;
  const problem =
    at === null
      ? "Enter a date and a time."
      : at <= now
        ? "Choose a moment in the future."
        : limit !== null && at > limit
          ? `A snooze can end at most ${Math.round(maxSnoozeAhead! / 86_400)} days ahead — by ${fmtMinute(new Date(limit).toISOString())}.`
          : null;
  return (
    <Modal
      open
      size="sm"
      title="Snooze until"
      icon={<ClockGlyph />}
      onClose={onClose}
      onSubmit={() => {
        if (!problem && at !== null) onPick(new Date(at).toISOString());
      }}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabledReason={problem ?? undefined}
            onClick={() => {
              if (!problem && at !== null) onPick(new Date(at).toISOString());
            }}
          >
            Snooze
          </Button>
        </>
      }
    >
      <FormField
        label="Comes back at"
        htmlFor="inbox-snooze-at"
        helper="In your own time zone, as your preferences set it."
        error={problem && at !== null ? problem : undefined}
      >
        <Input
          id="inbox-snooze-at"
          type="datetime-local"
          value={wall}
          min={toWall(now)}
          max={limit !== null ? toWall(limit) : undefined}
          onChange={(event) => setWall(event.target.value)}
        />
      </FormField>
    </Modal>
  );
}
