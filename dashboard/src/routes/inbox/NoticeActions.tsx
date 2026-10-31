/**
 * What a reader does with an update: mark it read (or unread again), put it
 * off, or bring it back. Each is ONE gesture on this person's own inbox,
 * naming exactly the one notice that is open, through ITS SOURCE'S tool —
 * `mark_inbox` for a tracker notice, `code_mark_inbox` for the code host's —
 * since each domain keeps the marks over its own notices on its own log.
 *
 * A GESTURE, NEVER THE DOCUMENT. Marking read marks this record and nothing
 * else: every other mark, every snooze and the read position stay where they
 * are (`tracker.Writer.MarkInbox`). The write this replaced sent the whole
 * inbox back, so a Done pressed in one tab erased a snooze set in another.
 *
 * UPDATES ONLY. A question, a parked run, a stopped seat and a condition leave
 * the inbox when they are answered or cleared, which is the engine's rule for
 * decisions, so they carry none of these.
 */

import { useState } from "react";
import { Button, FormField, Input, Menu, Modal, Stack, type MenuEntry } from "@crewlethq/ui";
import { ClockGlyph } from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { fmtMinute, fromWall, toWall } from "~/lib/format.ts";
import { snoozePresets } from "./model.ts";
/** What a mark needs of a notice, whichever source it came from. */
export interface Markable {
  record_id: string;
  read: boolean;
  snoozed?: boolean;
}

export function NoticeActions({
  notice,
  source,
  now,
  zone,
  maxSnoozeAhead,
  onSettled,
}: {
  notice: Markable;
  /** The domain the notice is kept by, whose tool marks it. */
  source: "tracker" | "code";
  now: number;
  /** The company's clock, which "tomorrow at nine" is read on. */
  zone: string | undefined;
  /** The engine's bound on a snooze, in seconds, where it said one. */
  maxSnoozeAhead: number | undefined;
  /** The notice was read or put off here, so it leaves the group it was in. */
  onSettled: () => void;
}) {
  // BOTH TOOLS, NAMED AS LITERALS, and the notice's source's used: each is a
  // write on its own log, read back at that log's floor.
  const tracker = useAct("mark_inbox");
  const code = useAct("code_mark_inbox");
  const write = source === "code" ? code : tracker;
  const [picking, setPicking] = useState(false);
  // WHAT THIS PANE DID, until the next read says it too: the button turns into
  // its opposite the moment the write lands, rather than a poll later.
  const [read, setRead] = useState(notice.read);
  const [snoozed, setSnoozed] = useState(Boolean(notice.snoozed));
  const id = notice.record_id;

  const mark = async (args: Parameters<typeof write.run>[0], done: string): Promise<boolean> => {
    const result = await write.run(args, { done });
    return Boolean(result && (result.kind === "applied" || result.kind === "pending"));
  };
  const snooze = async (until: string) => {
    if (await mark({ snooze: [{ record_id: id, until }] }, `Snoozed until ${fmtMinute(until)}`)) {
      setSnoozed(true);
      onSettled();
    }
  };
  const markRead = async () => {
    if (await mark({ read: [id] }, "Marked as read")) {
      setRead(true);
      onSettled();
    }
  };
  const markUnread = async () => {
    if (await mark({ unread: [id] }, "Marked as unread")) setRead(false);
  };
  const unsnooze = async () => {
    if (await mark({ unsnooze: [id] }, "Back in your inbox")) setSnoozed(false);
  };

  const entries: MenuEntry[] = [
    ...snoozePresets(now, zone, maxSnoozeAhead).map((p) => ({
      key: p.key,
      label: p.label,
      // A COMPANY-CLOCK PRESET SAYS ITS DAY AND WHOSE NINE O'CLOCK it is; the
      // hour says the reader's own time it comes back at.
      hint: p.day ? `${p.day}, company time` : fmtMinute(p.until),
      onSelect: () => void snooze(p.until),
    })),
    { kind: "separator" as const, key: "sep" },
    { key: "pick", label: "Pick a date and time…", onSelect: () => setPicking(true) },
  ];

  return (
    <Stack gap={2}>
      <Stack gap={2} align="center" wrap>
        {snoozed ? (
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            onPress={() => void unsnooze()}
          >
            Unsnooze
          </WriteButton>
        ) : read ? (
          <WriteButton
            write={write}
            variant="secondary"
            showRefusal={false}
            onPress={() => void markUnread()}
          >
            Mark as unread
          </WriteButton>
        ) : (
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            onPress={() => void markRead()}
          >
            Mark as read
          </WriteButton>
        )}
        {!snoozed &&
          (write.access.can && !write.busy ? (
            <Menu
              label="Snooze this update"
              icon={<ClockGlyph size="sm" />}
              trigger="Snooze"
              triggerVariant="secondary"
              align="start"
              items={entries}
            />
          ) : (
            // THE SAME CONTROL, DISABLED WITH ITS REASON: a reader who cannot
            // act still sees that Snooze exists and why it is not theirs.
            <WriteButton
              write={write}
              variant="secondary"
              leadingIcon={<ClockGlyph size="sm" />}
              showRefusal={false}
              onPress={() => undefined}
            >
              Snooze
            </WriteButton>
          ))}
      </Stack>
      <RefusalNote write={write} />
      {picking && (
        <PickDialog
          now={now}
          maxSnoozeAhead={maxSnoozeAhead}
          onClose={() => setPicking(false)}
          onPick={(until) => {
            setPicking(false);
            void snooze(until);
          }}
        />
      )}
    </Stack>
  );
}

/**
 * A snooze to any moment inside the engine's bound, in the READER's own clock,
 * the browser's zone (`lib/prefs.ts` `zone`), which is what a date typed into
 * a field means to them. There is no zone preference to name any more, so the
 * helper names the browser's.
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
          ? `A snooze can end at most ${Math.round(maxSnoozeAhead! / 86_400)} days ahead, by ${fmtMinute(new Date(limit).toISOString())}.`
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
        helper="In this browser's time zone."
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
