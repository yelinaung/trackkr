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

`GetActivitySummary` loads the selected raw window once, applies interval
subtraction, and derives both the timeline and app totals from the same
effective records. The SQL query fetches at most 25,001 source rows: 25,000 for
processing and one truncation probe. The timeline then renders at most 5,000
effective records.

This two-level bound prevents one dashboard request from materialising an
unbounded day twice. If the source bound is reached, the dashboard says that
both its chart and totals are incomplete; it must not claim the totals cover
the whole day. Site totals remain a separate bounded SQL aggregation over
URL-bearing activity.

Residual desktop slices shorter than one second are discarded. The extension
does not report such visits, and rendering those gaps with the minimum visual
bar width would turn sub-second intervals into misleading multi-minute bars.

Query-time correction was chosen over ingestion suppression because it:

- corrects existing history immediately;
- preserves desktop-only Firefox time when the extension is absent or paused;
- avoids timing races between independently delivered desktop and extension
  batches;
- requires no source column or data migration.

## Verification

Pure unit tests cover partial overlap, merged browser coverage, device
isolation, adjacent intervals, sub-second residuals, non-Firefox records, and
effective app totals. PostgreSQL integration tests verify timeline slices, app
totals, site totals, and the 25,000-row source bound.
