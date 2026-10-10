/**
 * The Agents workspace's chunk: its screens and its peeks, less the org
 * editor, which is a chunk of its own (`routes/org`). See `app/lazyScreen.ts`.
 */

export { Teams, UnitPeek, UnitScreen } from "./Units.tsx";
export { OrgChart } from "./OrgChart.tsx";
export { People } from "./People.tsx";
export { SchedulePeek, Schedules } from "../admin/agents/Schedules.tsx";
export { SeatScreen } from "../admin/agents/Seat.tsx";
export { SeatPeek } from "../admin/agents/SeatPeek.tsx";
