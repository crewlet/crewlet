-- A task names the atlas entries it touches as `atlas_entry` relations on its
-- own record (ADR-0140), and the atlas's map marks every entry with open work:
-- one count per entry, over every such edge in the company. The reverse edge
-- index leads with the other end across every kind, so that count read every
-- dependency, link and page reference to keep the few that name an entry.
--
-- Partial, so it holds those edges alone and costs a task that names no entry
-- nothing.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. An index derives from
-- rows every node already holds alike, and changes no row.
CREATE INDEX tracker_relations_entry_idx ON tracker_relations (other_id, task_id)
    WHERE kind = 'atlas_entry'; -- the open work on each atlas entry, and one entry's tasks
