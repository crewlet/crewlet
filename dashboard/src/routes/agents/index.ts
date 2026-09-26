/**
 * The Agents workspace's chunk: its screens and its peeks, less the org
 * editor, which is a chunk of its own (`routes/org`). See `app/lazyScreen.ts`.
 */

export { OrgChart, Teams, UnitPeek, UnitScreen } from "./Company.tsx";
export { People } from "./People.tsx";
export { SchedulePeek, Schedules } from "./Schedules.tsx";
export { SeatPeek, SeatScreen } from "./Seat.tsx";
