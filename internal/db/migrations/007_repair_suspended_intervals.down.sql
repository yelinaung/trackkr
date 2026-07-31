-- Irreversible by design.
--
-- The up migration discards the inflated ended_at values, and nothing in the
-- row records what they were. Restoring them would mean inventing sleep spans
-- that were never real activity in the first place, so the down migration is
-- deliberately empty rather than destructive.
SELECT 1;
