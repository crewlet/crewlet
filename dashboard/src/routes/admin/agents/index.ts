/**
 * The console's Agent setup chunk: every seat as it RUNS — the seats list, a
 * seat's setup with its turns, memory, schedules and settings, the schedules,
 * the tool skills and every agent's memory — and the console's seat and
 * schedule peeks. See `app/lazyScreen.ts`.
 *
 * The seats list is the person shell's own roster (`routes/team/People.tsx`),
 * drawn here rather than copied: one seat is one row in both shells, and the
 * row's link opens the seat's page in the shell it is drawn in (`seatPath`).
 */

export { Diaries, Diary } from "./Diaries.tsx";
export { SchedulePeek, Schedules } from "./Schedules.tsx";
export { SeatPeek as SeatSetupPeek } from "./SeatPeek.tsx";
export { SeatScreen as SeatSetup } from "./Seat.tsx";
export { Skills } from "./Skills.tsx";
export { People as Seats } from "~/routes/team/People.tsx";
