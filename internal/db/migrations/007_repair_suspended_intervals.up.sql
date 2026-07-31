-- Repair records whose interval outran the duration they measured.
--
-- A daemon that measured elapsed time with a monotonic clock but stamped wall
-- clock timestamps disagreed with itself across a machine suspend: the
-- monotonic clock stops while the machine sleeps, so lid-closed time landed in
-- ended_at and never in duration_s. The dashboard derives time from the
-- interval, so each sleep was charted as continuous use of whatever
-- application happened to be frontmost when the lid closed.
--
-- duration_s is the trustworthy half: it is what the daemon actually measured.
-- Bring the interval back to it.
--
-- The two-second slack matches the ingest guard that now rejects this shape at
-- the boundary. A producer truncates its duration to whole seconds while
-- keeping sub-second timestamps, so a fraction of a second of excess is
-- normal and must not be rewritten.
UPDATE activity_records
SET ended_at = started_at + make_interval(secs => duration_s)
WHERE duration_s > 0
  AND ended_at > started_at + make_interval(secs => duration_s) + interval '2 seconds';
