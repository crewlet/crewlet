/**
 * Spend, budgets and the company calendar, derived from a demo company's
 * per-day usage rows.
 *
 * The engine keeps a seat's spend as one row per company day (the replicated
 * `usage` domain, ADR-0020) and answers every named window, every series and
 * every budget window by summing those rows on the company's clock (ADR-0018,
 * ADR-0019). This does the same over [DemoSpendDay] rows, so the spend
 * screens, the seat page's figures and the budget meters all agree with each
 * other, and with what a scenario changes.
 */

import type {
  AgentSpendRow,
  Bucket,
  BudgetMeter,
  BudgetWindow,
  BudgetsAnswer,
  OrgBudget,
  Rollup,
  SeriesBand,
  SeriesPoint,
  TokenBudget,
  TokenSeries,
} from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { agentSeats, companyZone, unitOfSeat } from "./derive.ts";
import { agentIdFor } from "./ids.ts";

/** One seat's spend in one phase, on one model, on one company day. */
export interface DemoSpendDay {
  /** The company date, `2026-10-12`. */
  day: string;
  handle: string;
  phase: string;
  model: string;
  /** The `providers.llm` key the calls went through. */
  provider: string;
  input: number;
  output: number;
  cache_read: number;
  calls: number;
  /** Turns that ended that day, and how many failed: on the execute row only. */
  turns?: number;
  failed?: number;
}

/** The share of a ceiling at which the engine calls a window `near`. */
const NEAR_FRACTION = 0.8;

/** How far back a named window may reach, in company days (`usage`'s horizon). */
const HORIZON_DAYS = 181;

// ---- the company calendar ------------------------------------------------

/** The company date `instant` falls on, `YYYY-MM-DD`. */
export function dateIn(instant: Date, zone: string): string {
  return new Intl.DateTimeFormat("en-CA", {
    timeZone: zone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(instant);
}

/** How far `zone` is ahead of UTC at `instant`, in milliseconds. */
function offsetAt(instant: Date, zone: string): number {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      hourCycle: "h23",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    })
      .formatToParts(instant)
      .map((p) => [p.type, p.value]),
  );
  const local = Date.UTC(
    Number(parts.year),
    Number(parts.month) - 1,
    Number(parts.day),
    Number(parts.hour),
    Number(parts.minute),
    Number(parts.second),
  );
  return local - Math.floor(instant.getTime() / 1000) * 1000;
}

/**
 * The first instant of the company date `day`: the midnight the engine cuts
 * a day at, found from the zone's offset rather than assumed.
 */
export function startOfDay(day: string, zone: string): Date {
  const [y, m, d] = day.split("-").map(Number) as [number, number, number];
  const guess = Date.UTC(y, m - 1, d);
  let at = guess - offsetAt(new Date(guess), zone);
  // A second pass settles a midnight on the far side of a clock change.
  at = guess - offsetAt(new Date(at), zone);
  return new Date(at);
}

/** The date `days` after `day`. */
export function addDays(day: string, days: number): string {
  const [y, m, d] = day.split("-").map(Number) as [number, number, number];
  return new Date(Date.UTC(y, m - 1, d + days)).toISOString().slice(0, 10);
}

/** The ISO week a date falls in, `2026-W42`, and its Monday. */
function isoWeek(day: string): { label: string; monday: string } {
  const [y, m, d] = day.split("-").map(Number) as [number, number, number];
  const weekday = (new Date(Date.UTC(y, m - 1, d)).getUTCDay() + 6) % 7;
  const monday = addDays(day, -weekday);
  // The week belongs to the year its Thursday falls in.
  const year = Number(addDays(monday, 3).slice(0, 4));
  const jan4 = new Date(Date.UTC(year, 0, 4));
  const firstMonday = Date.UTC(year, 0, 4 - ((jan4.getUTCDay() + 6) % 7));
  const week = 1 + Math.round((Date.parse(`${monday}T00:00:00Z`) - firstMonday) / (7 * 86_400_000));
  return { label: `${year}-W${String(week).padStart(2, "0")}`, monday };
}

/** One calendar window of `period` containing `day`: its label and its first and last dates. */
export function windowOf(
  period: "day" | "week" | "month",
  day: string,
): { label: string; first: string; next: string } {
  if (period === "day") return { label: day, first: day, next: addDays(day, 1) };
  if (period === "week") {
    const { label, monday } = isoWeek(day);
    return { label, first: monday, next: addDays(monday, 7) };
  }
  const [y, m] = day.split("-").map(Number) as [number, number];
  const first = `${y}-${String(m).padStart(2, "0")}-01`;
  const next = new Date(Date.UTC(y, m, 1)).toISOString().slice(0, 10);
  return { label: first.slice(0, 7), first, next };
}

// ---- sums -----------------------------------------------------------------

const zero = (): Bucket => ({
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: 0,
  calls: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
});

function add(into: Bucket, row: DemoSpendDay): void {
  into.input_tokens += row.input;
  into.output_tokens += row.output;
  into.total_tokens += row.input + row.output;
  into.calls += row.calls;
  into.cache_read_tokens = (into.cache_read_tokens ?? 0) + row.cache_read;
}

function sumBy<K extends string>(
  rows: DemoSpendDay[],
  keyOf: (row: DemoSpendDay) => string,
  name: K,
): (Bucket & Record<K, string>)[] {
  const map = new Map<string, Bucket>();
  for (const row of rows) {
    const key = keyOf(row);
    if (!map.has(key)) map.set(key, zero());
    add(map.get(key)!, row);
  }
  return [...map]
    .map(([key, bucket]) => ({ ...bucket, [name]: key }) as Bucket & Record<K, string>)
    .sort((a, b) => b.total_tokens - a.total_tokens);
}

/** The rows of the company days `from` to `to`, inclusive, optionally one seat's. */
function rowsIn(c: DemoCompany, from: string, to: string, seat?: string): DemoSpendDay[] {
  return c.spend.daily.filter((r) => r.day >= from && r.day <= to && (!seat || r.handle === seat));
}

/** A named spend window as `tokens` answers it. */
export function rollupOf(
  c: DemoCompany,
  from: string,
  to: string,
  now: Date,
  seat?: string,
): Rollup {
  const zone = companyZone(documentOf(c));
  const rows = rowsIn(c, from, to, seat);
  const totals = zero();
  for (const row of rows) add(totals, row);
  const seats = agentSeats(documentOf(c));
  const byAgent: AgentSpendRow[] = sumBy(rows, (r) => r.handle, "handle").map((bucket) => {
    const mine = rows.filter((r) => r.handle === bucket.handle);
    const byPhase: Record<string, Bucket> = {};
    for (const row of mine) {
      byPhase[row.phase] ??= zero();
      add(byPhase[row.phase]!, row);
    }
    const found = seats.find((s) => s.handle === bucket.handle);
    return {
      ...bucket,
      role: found?.name ?? bucket.handle,
      agent_id: found?.agentId ?? "",
      by_phase: byPhase,
      turns: mine.reduce((n, r) => n + (r.turns ?? 0), 0),
      failed: mine.reduce((n, r) => n + (r.failed ?? 0), 0),
    };
  });
  const providers = sumBy(rows, (r) => r.provider, "provider_key").map((bucket) => {
    const mine = rows.filter((r) => r.provider === bucket.provider_key);
    const bySeat = sumBy(mine, (r) => r.handle, "handle");
    return {
      ...bucket,
      models: sumBy(mine, (r) => r.model, "model").map((m) => m.model),
      seats: bySeat.slice(0, 3).map((s) => s.handle),
      seats_total: bySeat.length,
    };
  });
  const days = Math.round((Date.parse(to) - Date.parse(from)) / 86_400_000) + 1;
  const rollup: Rollup = {
    since: startOfDay(from, zone).toISOString(),
    until: startOfDay(addDays(to, 1), zone).toISOString(),
    from,
    to,
    days,
    horizon: { days: HORIZON_DAYS, floor: addDays(dateIn(now, zone), -HORIZON_DAYS + 1) },
    totals,
    by_phase: sumBy(rows, (r) => r.phase, "phase"),
    by_model: sumBy(rows, (r) => r.model, "model"),
    by_provider: providers,
    by_worker: [],
    by_agent: byAgent,
  };
  if (seat) {
    rollup.agent_id = agentIdFor(documentOf(c).name ?? "", seat);
    rollup.seat = seat;
  }
  return rollup;
}

/** The live window: today and yesterday on the company clock, with no turn tail of its own. */
export function liveRollupOf(c: DemoCompany, now: Date): Rollup {
  const zone = companyZone(documentOf(c));
  const today = dateIn(now, zone);
  const named = rollupOf(c, addDays(today, -1), today, now);
  const { from: _from, to: _to, days: _days, horizon: _horizon, ...live } = named;
  return {
    ...live,
    by_agent: live.by_agent.map(({ turns: _t, failed: _f, ...row }) => row),
    by_turn: [],
    aggregated_through: now.toISOString(),
  };
}

/** The same spend with a time axis, as `token_series` answers it. */
export function seriesOf(
  c: DemoCompany,
  params: { group: string; bucket: "day" | "week"; from: string; to: string; seat?: string },
  now: Date,
): TokenSeries {
  const doc = documentOf(c);
  const zone = companyZone(doc);
  const rows = rowsIn(c, params.from, params.to, params.seat);
  // A SEAT'S BAND IS KEYED ON ITS AGENT ID and a unit's on its KEY, since two
  // of either may share a name; what each is called is the band's `label`.
  const seats = agentSeats(doc);
  const labels = new Map<string, string>();
  const groupOf = (row: DemoSpendDay): string => {
    switch (params.group) {
      case "phase":
        return row.phase;
      case "model":
        return row.model;
      case "provider":
        return row.provider;
      case "seat": {
        const id = agentIdFor(doc.name ?? "", row.handle);
        labels.set(id, seats.find((s) => s.handle === row.handle)?.name ?? row.handle);
        return id;
      }
      case "unit": {
        const unit = unitOfSeat(doc, row.handle);
        if (unit.key) labels.set(unit.key, unit.name);
        return unit.key;
      }
      default:
        return "";
    }
  };
  const bucketOf = (day: string) =>
    params.bucket === "week" ? windowOf("week", day) : windowOf("day", day);
  const points = new Map<string, SeriesPoint>();
  for (let day = params.from; day <= params.to; day = addDays(day, 1)) {
    const w = bucketOf(day);
    const point = points.get(w.label) ?? {
      ...zero(),
      at: startOfDay(w.first, zone).toISOString(),
      window: w.label,
      days: 0,
      groups: {},
      other: zero(),
    };
    point.days += 1;
    points.set(w.label, point);
  }
  const totals = zero();
  const grouped = zero();
  for (const row of rows) {
    add(totals, row);
    const point = points.get(bucketOf(row.day).label);
    const group = groupOf(row);
    if (!point || group === "") continue;
    add(grouped, row);
    add(point, row);
    point.groups[group] ??= zero();
    add(point.groups[group]!, row);
  }
  const bands: SeriesBand[] = sumBy(
    rows.filter((r) => groupOf(r) !== ""),
    groupOf,
    "group",
  ).map((band) => {
    const out: SeriesBand = { ...band, other: false, folded: 0 };
    const label = labels.get(band.group);
    if (label) out.label = label;
    if (params.group === "seat")
      out.handle = seats.find((s) => s.agentId === band.group)?.handle ?? band.group;
    if (params.group === "unit") {
      const parts = sumBy(
        rows.filter((r) => groupOf(r) === band.group),
        (r) => r.handle,
        "group",
      );
      out.seats = parts.length;
      out.parts = parts.map((part) => ({ ...part, handle: part.group }));
    }
    return out;
  });
  const days = Math.round((Date.parse(params.to) - Date.parse(params.from)) / 86_400_000) + 1;
  const series: TokenSeries = {
    group: params.group as TokenSeries["group"],
    bucket: params.bucket,
    since: startOfDay(params.from, zone).toISOString(),
    until: startOfDay(addDays(params.to, 1), zone).toISOString(),
    from: params.from,
    to: params.to,
    days,
    horizon: { days: HORIZON_DAYS, floor: addDays(dateIn(now, zone), -HORIZON_DAYS + 1) },
    series: [...points.values()],
    by_group: bands,
    totals,
    grouped,
  };
  if (params.seat) series.seat = params.seat;
  return series;
}

// ---- budgets --------------------------------------------------------------

const PERIODS = ["day", "week", "month"] as const;

/** One scope's three windows on the company clock: capped or not. */
function windowsOf(
  c: DemoCompany,
  budget: TokenBudget | undefined,
  seat: string | undefined,
  now: Date,
): BudgetWindow[] {
  const zone = companyZone(documentOf(c));
  const today = dateIn(now, zone);
  return PERIODS.map((period) => {
    const w = windowOf(period, today);
    const used = rowsIn(c, w.first, addDays(w.next, -1), seat).reduce(
      (n, r) => n + r.input + r.output,
      0,
    );
    const limit = budget?.[period];
    const window: BudgetWindow = {
      period,
      window: w.label,
      starts_at: startOfDay(w.first, zone).toISOString(),
      resets_at: startOfDay(w.next, zone).toISOString(),
      used,
      state:
        limit === undefined
          ? "ok"
          : used >= limit
            ? "refusing"
            : used >= limit * NEAR_FRACTION
              ? "near"
              : "ok",
    };
    if (limit !== undefined) window.limit = limit;
    return window;
  });
}

/** Every scope's day, week and month, as `budgets` answers. */
export function budgetsOf(c: DemoCompany, now: Date): BudgetsAnswer {
  const doc = documentOf(c);
  return {
    timezone: companyZone(doc),
    durable: true,
    near_fraction: NEAR_FRACTION,
    org: { windows: windowsOf(c, doc.token_budget as TokenBudget | undefined, undefined, now) },
    seats: agentSeats(doc).map((s) => ({
      agent_id: s.agentId,
      role: s.name,
      handle: s.handle,
      windows: windowsOf(c, seatBudget(c, s.handle), s.handle, now),
    })),
  };
}

function seatBudget(c: DemoCompany, handle: string): TokenBudget | undefined {
  const find = (
    roles: { handle?: string; name: string; token_budget?: TokenBudget }[] | undefined,
  ) =>
    roles?.find(
      (r) =>
        (r.handle || "") === handle || r.name.toLowerCase().replace(/[^a-z0-9]+/g, "-") === handle,
    );
  const doc = documentOf(c);
  const walk = (units: typeof doc.units): TokenBudget | undefined => {
    for (const u of units ?? []) {
      const found = find(u.roles)?.token_budget ?? walk(u.children);
      if (found) return found;
    }
    return undefined;
  };
  return find(doc.roles)?.token_budget ?? walk(doc.units);
}

/** A seat's live meter: its capped windows only, as the `agents` push carries them. */
export function seatMeterOf(c: DemoCompany, handle: string, now: Date): BudgetMeter | null {
  const windows = windowsOf(c, seatBudget(c, handle), handle, now).filter(
    (w) => w.limit !== undefined,
  );
  return windows.length > 0 ? { windows } : null;
}

/** The org meter, as the `budget` push carries it: its capped windows only. */
export function orgMeterOf(c: DemoCompany, now: Date, seq: number): OrgBudget {
  const doc = documentOf(c);
  return {
    meter_id: "node-0",
    seq,
    timezone: companyZone(doc),
    org: {
      windows: windowsOf(c, doc.token_budget as TokenBudget | undefined, undefined, now).filter(
        (w) => w.limit !== undefined,
      ),
    },
  };
}
