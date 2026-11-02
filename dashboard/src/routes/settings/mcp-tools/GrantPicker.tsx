/**
 * Who gets a server, one control for the wizard's "Who gets it" step and the
 * server's Access tab: every agent, or chosen teams and agents. Choosing the
 * second opens the team tree under its card (the kit's `MemberPicker`): each
 * team a row with its three-state box and count that opens to its agents,
 * each agent with its face, its name and what it does, an agent a chosen
 * team reaches marked so and left out by unticking it, and the count on top
 * as the choice moves.
 *
 * TEAMS FIRST, AGENTS AS OVERRIDES: a team's grant reaches whoever joins it,
 * which a list of names cannot, so the overrides are exceptions to a grant
 * rather than the grant itself.
 *
 * A TEAM LISTS EVERY AGENT BENEATH IT, at any depth, because that is who its
 * grant reaches (a unit key in `access.units` reaches the agent seats of
 * every unit under it): an agent in a nested team is listed under each team
 * above it, and the picker answers for the agent from whichever row it is
 * ticked in. Listing a team's own members alone would have counted a grant
 * to Engineering as missing the agents in Engineering's Backend. The org
 * projection already holds each unit's agents at any depth (`allSeats`), so
 * nothing here walks the chart.
 *
 * WHAT IT CHOOSES IS A FORM'S VALUE, written as the entry's `access`; who the
 * saved grant actually reaches is the engine's answer (`reaches` on the
 * server's detail), and the engine's dry run judges every name.
 */

import { useMemo } from "react";
import {
  ChoiceCards,
  MemberPicker,
  type MemberPickerGroup,
  type MemberPickerMember,
} from "@crewlethq/ui";
import { ListChecksGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";

/**
 * A server's grant as the form holds it: every agent (no `access` written),
 * or the units, the seats and the seats left out of a chosen unit.
 */
export interface Access {
  every: boolean;
  units: readonly string[];
  seats: readonly string[];
  except: readonly string[];
}

export const EVERY: Access = { every: true, units: [], seats: [], except: [] };

const names = (v: unknown): string[] =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];

/** The grant an entry's `access` field says: absent is every agent; `{}` is nobody. */
export function accessOf(field: unknown): Access {
  if (typeof field !== "object" || field === null || Array.isArray(field)) return EVERY;
  const a = field as Record<string, unknown>;
  return { every: false, units: names(a.units), seats: names(a.seats), except: names(a.except) };
}

/**
 * The entry's `access` for a grant: undefined for every agent, which the
 * entry says by leaving the field out; otherwise the lists it names, `{}`
 * when it names nobody (which the engine takes, and advises on).
 */
export function accessEntity(a: Access): Record<string, string[]> | undefined {
  if (a.every) return undefined;
  return {
    ...(a.units.length ? { units: [...a.units] } : {}),
    ...(a.seats.length ? { seats: [...a.seats] } : {}),
    ...(a.except.length ? { except: [...a.except] } : {}),
  };
}

/** Whether two grants write the same entry. */
export function sameAccess(a: Access, b: Access): boolean {
  return JSON.stringify(accessEntity(a) ?? null) === JSON.stringify(accessEntity(b) ?? null);
}

function member(seat: Seat): MemberPickerMember {
  return {
    key: seat.handle,
    name: seat.name,
    ...(seat.goal ? { detail: seat.goal } : {}),
    avatar: <SeatAvatar name={seat.name} kind="agent" avatar={seat.avatar} size="sm" />,
  };
}

/** The chart's agent seats as the picker groups them: every team with an agent beneath it, and the agents in none. */
function useAgentTree(): {
  groups: MemberPickerGroup[];
  ungrouped: MemberPickerMember[];
  /** The outermost teams, open at first: a nested team's agents are listed there already. */
  open: string[];
  total: number;
} {
  const org = useOrg();
  return useMemo(() => {
    const index = indexOrg(org);
    const groups = index.units
      .map((u) => ({
        key: u.id,
        label: u.name,
        members: u.allSeats.filter((s) => s.kind === "agent").map(member),
      }))
      .filter((g) => g.members.length > 0);
    const ungrouped = index.seats.filter((s) => s.kind === "agent" && !s.unit).map(member);
    const total = index.seats.filter((s) => s.kind === "agent").length;
    const open = index.topUnits.map((u) => u.id);
    return { groups, ungrouped, open, total };
  }, [org]);
}

export function GrantPicker({
  value,
  onChange,
  label,
  disabled = false,
}: {
  value: Access;
  onChange: (a: Access) => void;
  /** Names the chooser for a screen reader: "Who gets Sentry". */
  label: string;
  disabled?: boolean;
}) {
  const { groups, ungrouped, open, total } = useAgentTree();
  return (
    <ChoiceCards
      label={label}
      minCardWidth="sm"
      density="compact"
      value={value.every ? "every" : "chosen"}
      onChange={(v) => {
        if (disabled) return;
        onChange(v === "every" ? EVERY : { ...value, every: false });
      }}
      options={[
        {
          value: "every",
          label: "Every agent",
          media: <UsersGlyph size="lg" />,
          description: `${total} today, and anyone who joins`,
          disabled,
        },
        {
          value: "chosen",
          label: "Chosen teams and agents",
          media: <ListChecksGlyph size="lg" />,
          description: "Pick teams, then adjust agents",
          disabled,
          detail: (
            <MemberPicker
              label={`${label}: teams and agents`}
              groups={groups}
              ungrouped={ungrouped}
              defaultOpen={open}
              value={{ groups: value.units, members: value.seats, except: value.except }}
              onChange={(v) =>
                onChange({
                  every: false,
                  units: [...v.groups],
                  seats: [...v.members],
                  except: [...v.except],
                })
              }
            />
          ),
        },
      ]}
    />
  );
}
