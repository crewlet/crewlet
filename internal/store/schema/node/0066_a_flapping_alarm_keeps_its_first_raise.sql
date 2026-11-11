-- A flapping alarm keeps its first raise.
--
-- An alarm instance raised again less than the relapse window after it cleared
-- continues the same run, and every age read off it (the fleet's marks, the
-- administrators' notice) counts from the run's first raise. `run` is that
-- first raise, NULL where it is the instance's own `since`. A row whose
-- `cleared` is set is an instance that cleared at that instant, kept so a
-- raise inside the window after a restart still resumes its run; NULL is an
-- instance still up.
--
-- WHO HAS TO AGREE ON IT: this node alone, as every row of the table.
ALTER TABLE alarm_standing ADD COLUMN run INTEGER;
ALTER TABLE alarm_standing ADD COLUMN cleared INTEGER;
