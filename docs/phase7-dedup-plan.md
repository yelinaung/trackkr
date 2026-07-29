# Phase 7: Desktop and Firefox Deduplication

## Status

Implemented as a dashboard query correction. No stored activity is deleted or
rewritten.

## Decision

Firefox extension records and desktop window records can describe the same
focused browsing interval. The dashboard gives the extension observation
precedence only where the two intervals actually overlap:

- a Firefox extension record has normalized app key `firefox` and a non-empty
  URL;
- a desktop Firefox record has normalized app key `firefox` and no URL;
- overlap is considered only within the same device;
- extension intervals are merged before subtraction;
- non-overlapping desktop time remains visible, including slices before and
  after an extension record;
- adjacent intervals that only touch are not duplicates;
- other applications and URL-bearing records are unchanged.

This rule uses URL presence rather than application-name casing. Linux and
macOS can report different display names, while extension records consistently
carry the page URL that desktop records cannot observe.

## Query Behavior

`GetActivityRecords` loads the selected raw window, applies interval
subtraction, orders the effective records, and only then applies the timeline
limit. This prevents duplicate raw rows from hiding later effective activity or
causing a false truncation warning.

`GetAppTotals` uses the same effective records and clips them to the requested
window before aggregation. Site totals already exclude desktop records because
they group only URL-bearing activity, so their behavior does not change.

Query-time correction was chosen over ingestion suppression because it:

- corrects existing history immediately;
- preserves desktop-only Firefox time when the extension is absent or paused;
- avoids timing races between independently delivered desktop and extension
  batches;
- requires no source column or data migration.

## Verification

Pure unit tests cover partial overlap, merged browser coverage, device
isolation, adjacent intervals, non-Firefox records, and effective app totals.
The PostgreSQL integration test verifies timeline slices, app totals, and site
totals from one overlapping desktop/extension pair.
