/**
 * Company › Every change: the tracker's whole log, over a window you choose —
 * every change to the company's work, an agent's commits as well as a
 * person's.
 *
 * BESIDE THE AUDIT LOG, NOT INSIDE IT. The audit log answers "what did PEOPLE
 * and keys change", across the four subsystems that record a writer; this
 * answers "what happened to the work", whoever did it. Both are an admin's
 * view of the whole company, so both are the console's — a teammate reads a
 * task's and a project's own history, on the task and the project.
 *
 * The screen is the project History lens's own component
 * (`routes/work/History.tsx`) over the whole company, so the two logs can
 * never draw one change two ways.
 */

import { History } from "~/routes/work/History.tsx";

export function EveryChange() {
  return <History />;
}
