/**
 * A seat's extra grants beside its level, on any lane whose catalogue offers
 * them (Cloudflare's permission groups): the row's list of what is granted,
 * each removable, and Add grant, which picks one permission from the
 * vendor's live catalogue, where it applies and for how long. Both change the
 * tab's one draft, so the review, the plan and the step-up a write grant asks
 * for are the save's own.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  FormField,
  Inline,
  SegmentedControl,
  Select,
  Stack,
  Tag,
  Text,
  Textarea,
  type SelectOption,
} from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import type { AccessDesired, AccessGrant, AccessSeat } from "~/contract/access.ts";
import { Modal } from "~/components/Modal.tsx";
import { useClock } from "~/lib/clock.ts";
import { GRANT_HOURS, grantEntry, grantReach, hoursWords, newGrant } from "~/lib/grants.ts";
import { grantExpiry, grantLapsed, levelName, type Lane } from "./logic.ts";

/** The longest note a person gives a grant: the engine's own bound on a grant's reason. */
const MAX_REASON = 1500;

/** One grant as its tag says it: the permission in the vendor's words, where, and for how long. */
function grantLabel(lane: Lane, g: AccessGrant, now: number): string {
  const name = grantEntry(lane, g.permission)?.name ?? g.permission;
  return `${name} · ${grantReach(lane, g)} · ${grantExpiry(g, now)}`;
}

/** Who granted it and why, for the tag's title: never shown alone. */
function grantNote(g: AccessGrant): string {
  const by = g.granted_by ? `Granted by ${g.granted_by}` : "Not saved yet";
  return g.reason ? `${by}: ${g.reason}` : by;
}

/**
 * A seat's grants on one lane, as tags a person may take off, and Add grant.
 * A grant whose permission the connection cannot grant now is marked so, and
 * a lapsed one says Expired until the sweep drops it.
 */
export function GrantsCell({
  seat,
  lane,
  want,
  disabled,
  onRemove,
  onAdd,
}: {
  seat: AccessSeat;
  lane: Lane;
  want: AccessDesired;
  /** No grant may change here: the tab is held, read-only, or the seat is off. */
  disabled: boolean;
  onRemove: (index: number) => void;
  onAdd: () => void;
}) {
  // THE MINUTE, from the shared clock: an expiry reads in minutes, so the
  // cell draws again once a minute rather than every second.
  const now = useClock((t) => t - (t % 60_000));
  const grants = want.grants ?? [];
  return (
    <Inline gap={1} wrap>
      {grants.map((g, i) => {
        const entry = grantEntry(lane, g.permission);
        const variant = grantLapsed(g, now) ? "neutral" : entry?.unavailable ? "warning" : "info";
        const label = grantLabel(lane, g, now);
        const title = entry?.unavailable ? `${grantNote(g)}. ${entry.unavailable}.` : grantNote(g);
        return disabled ? (
          <Tag key={g.id ?? `new-${i}`} size="sm" variant={variant} title={title}>
            {label}
          </Tag>
        ) : (
          <Tag
            key={g.id ?? `new-${i}`}
            size="sm"
            variant={variant}
            title={title}
            onRemove={() => onRemove(i)}
            removeAriaLabel={`Remove ${seat.name}'s grant of ${entry?.name ?? g.permission}`}
          >
            {label}
          </Tag>
        );
      })}
      {!disabled && (
        <Button size="small" variant="ghost" leadingIcon={<PlusGlyph size="sm" />} onClick={onAdd}>
          Add grant
        </Button>
      )}
      {disabled && grants.length === 0 && (
        <Text as="span" variant="caption" tone="secondary">
          None
        </Text>
      )}
    </Inline>
  );
}

/**
 * Add grant: one permission from the lane's live catalogue, grouped as the
 * vendor groups them, a write marked as one and an unavailable one offered
 * disabled with why; the zones it applies to (none chosen is every zone the
 * company's agents reach); how long; and why, which the grant keeps.
 */
export function AddGrantDialog({
  seat,
  lane,
  onAdd,
  onClose,
}: {
  seat: AccessSeat;
  lane: Lane;
  onAdd: (grant: AccessGrant) => void;
  onClose: () => void;
}) {
  const [permission, setPermission] = useState("");
  const [zones, setZones] = useState<string[]>([]);
  const [hours, setHours] = useState<number>(GRANT_HOURS[0]);
  const [reason, setReason] = useState("");
  const entry = grantEntry(lane, permission);
  const scoped = entry !== undefined && entry.scope_kind !== "account";
  const options: SelectOption[] = (lane.grants ?? []).map((e) => ({
    value: e.id,
    label: e.name,
    text: `${e.name} ${e.id}`,
    hint: e.write ? "Edit" : "Read",
    ...(e.in_level
      ? { description: `Already in ${levelName(lane, e.in_level)} and above` }
      : e.name !== e.id
        ? { description: e.id }
        : {}),
    ...(e.unavailable ? { description: e.unavailable, disabled: true } : {}),
    group: e.group ?? "Permissions",
  }));
  const blocked = !entry
    ? "Choose a permission first."
    : !reason.trim()
      ? "Say why first."
      : undefined;
  const add = () => {
    if (blocked) return;
    onAdd(newGrant(permission, scoped ? zones : [], hours, reason, Date.now()));
    onClose();
  };
  return (
    <Modal
      open
      size="md"
      onClose={onClose}
      title={`Add a grant for ${seat.name}`}
      onSubmit={add}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            disabled={blocked !== undefined}
            title={blocked}
            onClick={add}
          >
            Add grant
          </Button>
        </>
      }
    >
      <Stack gap={4}>
        <Text as="p" variant="body" tone="secondary">
          {`One ${lane.name} permission beside ${seat.name}'s level, from what the company's connection can grant now. Nothing changes until you save the tab.`}
        </Text>
        <FormField label="Permission">
          <Select
            size="md"
            width="full"
            searchable
            searchPlaceholder="Find a permission"
            emptyMessage="No permission matches"
            ariaLabel={`${seat.name}'s new ${lane.name} permission`}
            placeholder="Choose a permission"
            value={permission}
            options={options}
            onChange={(v) => {
              setPermission(String(v));
              setZones([]);
            }}
          />
        </FormField>
        {entry?.write && (
          <Callout variant="warning">
            {`${entry.name} can change things at ${lane.name}, so saving it asks you to sign in again, as the most trusted level does.`}
          </Callout>
        )}
        {scoped && (lane.grant_scope ?? []).length > 0 && (
          <FormField
            as="fieldset"
            label="Zones"
            helper="None chosen is every zone the company's agents reach."
          >
            <Stack gap={1}>
              {(lane.grant_scope ?? []).map((z) => (
                <Checkbox
                  key={z}
                  label={z}
                  checked={zones.includes(z)}
                  onCheckedChange={(on) =>
                    setZones(on ? [...zones, z] : zones.filter((x) => x !== z))
                  }
                />
              ))}
            </Stack>
          </FormField>
        )}
        <FormField label="How long">
          <SegmentedControl
            size="sm"
            semantics="radio"
            label={`How long ${seat.name} holds it`}
            value={String(hours)}
            onValueChange={(v) => setHours(Number(v))}
            options={GRANT_HOURS.map((h) => ({ value: String(h), label: hoursWords(h) }))}
          />
        </FormField>
        <FormField
          label="Why"
          helper="Kept with the grant, for whoever reads the agent's access next."
        >
          <Textarea
            rows={2}
            maxLength={MAX_REASON}
            aria-label={`Why ${seat.name} needs it`}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </FormField>
      </Stack>
    </Modal>
  );
}
