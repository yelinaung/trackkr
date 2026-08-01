# Phase 11: Application Categories

## Goal

Add user-defined categories such as `Work`, `Communication`, and
`Entertainment`. The dashboard groups tracked time by category, and an
authenticated management page controls category names, colors, and application
defaults.

Application activity has two category layers:

1. An application default applies to past and future records.
2. A record override applies to one stored activity record and takes precedence
   over the application default.

Changing either layer leaves `activity_records` untouched. The daemon,
authenticated ingestion API, and browser extensions keep their current
contracts.

The first release excludes website categories, multiple categories per
application or record, productivity scores, category filters, category detail
pages, and exports.

## Behavior

- Users can create, rename, recolor, and delete categories.
- Each application has zero or one default category.
- Each activity record may override its application default.
- Clearing an override restores inheritance from the application default.
- An explicit Uncategorized override is different from clearing an override.
- Activity without a default or override contributes to `Uncategorized`.
- Changing an application default updates every inherited historical record and
  every future record. Existing record overrides remain unchanged.
- Deleting a category removes defaults and overrides that point to it. Affected
  records inherit another applicable default or become Uncategorized.
- Deleting a category never deletes activity.
- Category totals use effective activity after browser alias folding and
  desktop/browser overlap removal.
- Browser aliases share one application identity. For example, `Google Chrome`
  and `google-chrome` share one default.
- Users can read and mutate only their own categories, defaults, overrides, and
  activity records.

## Category Resolution

Every effective record contribution resolves its category in this order:

1. Stored record override.
2. Canonical application's default.
3. Virtual Uncategorized bucket.

A missing override row means inheritance. An override row with a null
`category_id` means explicit Uncategorized. No database row represents the
virtual Uncategorized bucket.

## Schema

Migration `008_categories` adds the category tables and one activity lookup
index. Matching down migration files remove them in reverse dependency order.

```sql
CREATE TABLE categories (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    color_key   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (id, user_id),
    CHECK (
        name = REGEXP_REPLACE(BTRIM(name), '[[:space:]]+', ' ', 'g')
    ),
    CHECK (CHAR_LENGTH(name) BETWEEN 1 AND 64),
    CHECK (color_key IN (
        'coral', 'amber', 'leaf', 'teal',
        'sky', 'indigo', 'rose', 'slate'
    ))
);

CREATE UNIQUE INDEX categories_user_name_unique
    ON categories (user_id, LOWER(name));

CREATE TABLE application_category_assignments (
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_key     TEXT NOT NULL,
    category_id BIGINT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, app_key),
    FOREIGN KEY (category_id, user_id)
        REFERENCES categories(id, user_id) ON DELETE CASCADE,
    CHECK (
        app_key = LOWER(
            REGEXP_REPLACE(BTRIM(app_key), '[[:space:]]+', ' ', 'g')
        )
    ),
    CHECK (OCTET_LENGTH(app_key) BETWEEN 1 AND 255)
);

CREATE INDEX application_category_assignments_category
    ON application_category_assignments (category_id);

CREATE TABLE activity_record_category_overrides (
    activity_record_id BIGINT PRIMARY KEY,
    user_id            BIGINT NOT NULL,
    category_id        BIGINT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (activity_record_id)
        REFERENCES activity_records(id) ON DELETE CASCADE,
    FOREIGN KEY (category_id, user_id)
        REFERENCES categories(id, user_id) ON DELETE CASCADE
);

CREATE INDEX activity_record_category_overrides_category
    ON activity_record_category_overrides (category_id)
    WHERE category_id IS NOT NULL;

CREATE INDEX idx_activity_records_device_ended_app
    ON activity_records (device_id, ended_at DESC, id DESC)
    INCLUDE (producer, app_name);
```

### Ownership

The category-side composite foreign keys prevent a non-null application default
or record override from pointing at another user's category. The override table
stores `user_id` for that check.

Record ownership follows the existing
`activity_records -> devices -> users` relationship. Override writes select
the record through its owning device, and reads join an override only when its
`user_id` matches the device owner.

The schema does not add `UNIQUE (id, device_id)` to `activity_records`. Such
a constraint would create a second B-tree beside the primary key on the hottest
insert table solely to support a composite record-side foreign key. The scoped
read and write predicates avoid that permanent cost while retaining the
inexpensive category-side foreign key.

The override table needs no `device_id` column or index. Device deletion first
cascades to `activity_records), then each activity deletion finds its override
through the override table's primary key. Category deletion uses the two
category indexes and does not scan either assignment table.

### Null Overrides

PostgreSQL applies composite foreign keys with `MATCH SIMPLE` by default. When
`category_id` is null, the composite category foreign key does not validate
`user_id`.

The public upsert writes the record owner's `user_id`. The activity queries
also require the override's `user_id` to equal the device owner's ID. A
malformed explicit-null row inserted outside the application therefore fails
closed and behaves as if no override exists. A later public upsert replaces its
stored `user_id` with `EXCLUDED.user_id` and repairs the row.

### Normalization

Category names preserve case. A shared Go normalizer trims leading and trailing
whitespace and collapses internal whitespace before persistence. The database
check rejects unnormalized names.

Application keys use `icon.AppKey` for lowercasing, trimming, and internal
whitespace collapse. The database check enforces the stored form. The
255-byte limit matches `icon.MaxKeyBytes`.

### Timestamps

Insert defaults initialize `updated_at`. Every category update, application
assignment upsert, and record override upsert sets `updated_at = NOW()`.
Deleting an assignment or override removes its row. No timestamp trigger is
needed.

## Migration Rollout

Migration `008` creates one index on `activity_records`. The repository runs
each golang-migrate file in a transaction, where `CREATE INDEX CONCURRENTLY`
is invalid. The migration therefore uses ordinary `CREATE INDEX), which
blocks activity writes while PostgreSQL builds
`idx_activity_records_device_ended_app`.

Production rollout requires a maintenance window and a build-time estimate from
a production-sized copy. A zero-downtime rollout first needs separately
designed support for non-transactional migrations; adding `CONCURRENTLY` to
this migration is not valid.

## Models

Add these database models:

```go
type CategoryRow struct {
    ID        int64
    UserID    int64
    Name      string
    ColorKey  string
    CreatedAt time.Time
    UpdatedAt time.Time
}

type CategorySummaryRow struct {
    CategoryRow
    AssignedAppCount int
}

type AppCategoryAssignmentRow struct {
    UserID     int64
    AppKey     string
    CategoryID int64
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type ActivityRecordCategoryOverrideRow struct {
    ActivityRecordID int64
    UserID           int64
    CategoryID       *int64
    CreatedAt        time.Time
    UpdatedAt        time.Time
}

type CategoryTotalRow struct {
    CategoryID *int64
    Name       string
    ColorKey   string
    Seconds    int64
}

type KnownApplicationRow struct {
    AppKey   string
    AppName  string
    LastSeen time.Time
}

type EditableActivityCursor struct {
    EndedAt time.Time
    ID      int64
}

type EditableActivityFilter struct {
    CanonicalAppName string
    Start            time.Time
    End              time.Time
    DeviceID         *int64
    Before           *EditableActivityCursor
    Limit            int
}

type EditableActivityPage struct {
    Records []ActivityRecordRow
    Next    *EditableActivityCursor
}
```

Add `CategoryTotals []CategoryTotalRow` to `ActivitySummary`. The field
contains totals from the full effective-duration traversal, not the bounded
`Records` timeline.

Add `CategoryOverridePresent bool` and `CategoryOverrideID *int64` to
`ActivityRecordRow`. The boolean distinguishes an absent override from an
explicit-null override.

`CategoryTotalRow.CategoryID` is nil only for the virtual Uncategorized
total. `ActivityRecordCategoryOverrideRow.CategoryID` is nil only for a
stored explicit-Uncategorized override.

## Application Identity

Extract the current canonicalization logic into
`canonicalAppName(producer, appName)`. Keep `CanonicalAppName(record)` as a
thin wrapper for existing callers. Queries that return separate `producer` and
`app_name` columns call the extracted function.

Add an exported helper that converts a canonical display name to its assignment
key through `icon.AppKey`. No category code may define a second normalization
algorithm.

The reporting pipeline becomes:

```text
raw activity records
  -> browser/desktop overlap removal
  -> effective duration per source record and canonical application
  -> record override, application default, or Uncategorized
  -> application totals and category totals
```

Canonicalization and overlap removal must precede categorization. Earlier
categorization would double count covered browser activity and could assign
`Google Chrome` separately from `google-chrome`.

## Known Applications

The management page lists recently observed applications. Its query:

- joins `activity_records` through the user's devices;
- considers records whose `ended_at` falls within the previous 180 days;
- groups raw `(producer, app_name)` pairs;
- orders groups by `MAX(ended_at) DESC`;
- returns at most 500 raw pairs;
- canonicalizes and merges those pairs in Go.

The cutoff and result limit control different costs. The cutoff bounds the
index range. `LIMIT 500` bounds only the grouped result, so PostgreSQL may
still examine every matching row in the 180-day range.

`idx_activity_records_device_ended_app` can support a
`(device_id, ended_at)` range scan for each device. PostgreSQL may choose
another plan when table statistics favor one. Pass the cutoff and limit into
the query; do not embed `NOW()`, which would make tests time-dependent.

When several raw names collapse to one key, retain the canonical display name
and latest `ended_at`. Assignments older than 180 days still affect reports.
The record editor remains available for applications absent from the recent
management list.

Migration tests inspect the index definition through `pg_indexes). They do
not assert a planner choice. Release verification runs
`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` against representative data and
records rows examined, buffer activity, and execution time. A small result does
not prove a small scan.

## Query Contract

Extend `WebQuerier` with category-specific methods:

```go
ListCategories(ctx context.Context, userID int64) ([]db.CategorySummaryRow, error)
CreateCategory(ctx context.Context, userID int64, name, colorKey string) (*db.CategoryRow, error)
UpdateCategory(ctx context.Context, userID, categoryID int64, name, colorKey string) (*db.CategoryRow, error)
DeleteCategory(ctx context.Context, userID, categoryID int64) error
ListKnownApplications(ctx context.Context, userID int64, since time.Time, limit int) ([]db.KnownApplicationRow, error)
ListAppCategoryAssignments(ctx context.Context, userID int64, appKeys []string) (map[string]db.CategoryRow, error)
SetAppCategory(ctx context.Context, userID int64, appKey string, categoryID *int64) error
ListEditableActivityRecords(ctx context.Context, userID int64, filter db.EditableActivityFilter) (*db.EditableActivityPage, error)
SetActivityRecordCategoryOverride(ctx context.Context, userID, recordID int64, categoryID *int64) error
DeleteActivityRecordCategoryOverride(ctx context.Context, userID, recordID int64) error
SetActivityRecordApplicationCategory(ctx context.Context, userID, recordID int64, categoryID *int64) error
```

`ListCategories` left-joins application assignments and returns each
category's application-default count. Record overrides do not affect the count.

`KnownApplicationRow` carries no category field. The handler loads assignments
once for the returned application keys through
`ListAppCategoryAssignments`. An empty key list returns an empty map without
issuing an `ANY` query.

All mutations scope their predicates to `user_id`. A missing or foreign-owned
category or record returns `pgx.ErrNoRows`; handlers translate it to
`404 Not Found` without revealing ownership.

`SetAppCategory` upserts a non-null category and deletes the assignment for a
nil category. The composite foreign key rejects foreign-owned categories.

## Editable Records Query

`ListEditableActivityRecords` reads raw source records through independent
keyset pagination. It never reads from the 5,000-row
`ActivitySummary.Records` timeline.

The query applies the dashboard's half-open overlap predicate:

```sql
ar.started_at < $end AND ar.ended_at > $start
```

The cursor predicate is additional and cannot replace either window bound.
Records crossing the start or end boundary remain editable. Records ending at
`Start` or starting at `End` do not overlap. The page clips displayed
intervals to `[Start, End)`, matching the activity detail view.

### Subject Matching

Place one subject-matching helper beside `browserFamilies`. Given a canonical
application name, the helper returns SQL parameters derived from the existing
family definition.

For a known browser family, the query matches:

- the family's trusted producer, regardless of stored `app_name`; or
- the desktop producer with one of the family's desktop keys.

For an unknown application, the query requires the desktop producer and an
exact stored name. Known-family producers are excluded. A
`producer = 'chrome'` row named `Whatever` therefore appears under Google
Chrome and never under an unknown `Whatever` page.

The helper passes producer and alias values as query parameters. SQL and server
code must not duplicate the Firefox or Chrome family lists.

### Ordering and Index Use

The editor orders records by `(ended_at DESC, id DESC)`, requests one row past
the page limit, and stores the next cursor in `EditableActivityPage.Next`.
The server caps the page size at 100.

For one selected device, `idx_activity_records_device_ended_app` can supply
the filter and order. For all devices, no single scan of an index led by
`device_id` produces a global order. A representative large-data plan should
scan the relevant device ranges and sort the combined rows before applying the
limit. PostgreSQL may choose different scan nodes on small tables.

The expected all-device Sort does not justify another activity index. Release
verification records separate one-device and all-device plans and accepts the
Sort. Migration 006 removed `UNIQUE (device_id, started_at)` and replaced it
with `UNIQUE (device_id, record_id)`; the old constraint cannot support this
query. The editor and known-application query share the single new index.

### Override Join

`queryActivityRecords` and `ListEditableActivityRecords` use the following
left-join rule:

```sql
LEFT JOIN activity_record_category_overrides o
  ON o.activity_record_id = ar.id
 AND o.user_id = d.user_id
```

Each query selects `o.activity_record_id IS NOT NULL` and `o.category_id`.
The pair preserves all three states: absent override, explicit Uncategorized,
and assigned category. Joining overrides with the source rows avoids a second
round trip and an `ANY` parameter containing as many as 25,000 record IDs.

## Mutation Semantics

The record-override methods use different operations for two nil states:

- `SetActivityRecordCategoryOverride(..., nil)` stores explicit
  Uncategorized.
- `DeleteActivityRecordCategoryOverride(...)` deletes the override and
  restores inheritance.

The upsert selects `activity_records` through `devices` and restricts the
record to the signed-in user. A non-null category joins `categories` on that
user. The conflict branch updates `user_id`, `category_id`, and
`updated_at`. Override deletion uses the record/device ownership join.

`SetActivityRecordApplicationCategory` runs in one transaction:

1. Load the owned source record.
2. Derive its canonical application key from trusted `producer` and
   `app_name`.
3. Update or remove the application default.
4. Delete the selected record's override.
5. Commit.

Deleting the selected record's override makes it inherit the new default
immediately. The endpoint never accepts a submitted application key.

## Aggregation

One application's effective time can span several record overrides, so category
totals cannot be derived from `AppTotalRow`.

Refactor the deduplicator around one internal traversal. For every source
record, the traversal emits effective duration after window clipping and
browser coverage subtraction. Application totals and category totals consume
that stream. Neither calculation uses the capped timeline.

`GetActivitySummary` loads application defaults for every canonical app key
in the complete raw source set. Each source row already carries override state
from the left join.

Accumulate exact `time.Duration` values before rounding. Application totals
retain their current rounding behavior. Category destinations within one
application use largest-remainder allocation:

1. Floor every destination's exact share.
2. Compare fractional remainders.
3. Assign remaining seconds in descending remainder order.
4. Break equal remainders by destination key ascending.

Uncategorized uses destination key `0`; stored categories use their positive
IDs. Uncategorized therefore wins an equal-remainder tie before any stored
category. The allocated category seconds sum exactly to the published
application seconds.

`ActivitySummary.CategoryTotals` contains totals from the full source set.
The dashboard must not rebuild them from `ActivitySummary.Records` or the
20-row `displayTotals` slice. Only application presentation is truncated.
Render every non-empty category total; at most 50 stored categories and one
Uncategorized bucket can appear.

Sort category totals by seconds descending, then name ascending. Omit
Uncategorized when it has zero seconds.

### Regression Boundary

The aggregation refactor must leave application totals unchanged. Every
existing activity-deduplication and browser-family test must pass without an
expected-value edit. Application names, integer seconds, ordering, coverage
subtraction, and truncation flags must remain identical.

If an existing expectation changes, stop and inspect the traversal. Category
tests add behavior; they do not redefine application totals.

## Category Management Page

Add a `Categories` link to the authenticated navigation and a
`/categories` page.

The category section contains:

- a create form with name and color;
- one row per category with color, name, and application-default count;
- rename and recolor controls;
- a delete action that explains the fallback to another default or
  Uncategorized.

The application section contains:

- up to 500 applications observed in the previous 180 days;
- an icon or existing monogram fallback;
- canonical display name and last-seen date;
- a selector with every category and Uncategorized;
- an empty state for users with no reported activity.

Category creation and editing use ordinary POST-and-redirect forms. Invalid
input rerenders the page with `400 Bad Request` and preserves entered values.
Duplicate names use the existing `isUniqueViolation` helper for PostgreSQL
code `23505`. Database failures use the existing generic error path.

Assignment and deletion controls may use HTMX. Successful swaps update affected
rows and counts. No project-owned JavaScript is required.

## Record Editor

Add a `Records` section to the application detail page. The existing session
list cannot host record controls: `detailSessions` merges adjacent source
records, and `SessionView` carries no record ID.

The Records section calls `ListEditableActivityRecords` and displays 100 raw
source records per page. Each row shows the clipped interval, device, title,
resolved category, and inheritance state.

Each row has three actions:

- `Apply to this record` stores the selected override.
- `Make application default` changes the default and clears this record's
  override in one transaction.
- `Follow application default` deletes this record's override.

The selector includes Uncategorized. Applying it to one record stores a null
`category_id`; making it the application default deletes the application
assignment. Labels must make the selected scope explicit before submission.

The record editor also categorizes applications outside the 180-day discovery
window. `Make application default` derives the key from the selected stored
record, so the management page's discovery window limits convenience, not
functionality.

After a successful mutation, redirect to the same application detail URL with
its date, period, device, and record-page filters. An HTMX request may rerender
the complete activity panel. Never update only one row while leaving visible
category totals or inherited labels stale.

## Routes

Register these routes inside the existing session and CSRF middleware:

```text
GET    /categories
POST   /categories
POST   /categories/{id}
DELETE /categories/{id}
POST   /categories/assignments
POST   /activity/records/{id}/category
```

Use `POST` for category edits so ordinary HTML forms work without HTMX. The
assignment endpoint accepts `app_key` and an optional `category_id`; an
empty category ID removes the application default.

The record endpoint accepts:

- `scope=record|application`;
- `action=set|uncategorized|inherit`;
- a positive `category_id` for `action=set`.

`inherit` is valid only for record scope. Application scope calls
`SetActivityRecordApplicationCategory` and never accepts `app_key`.

Add the category methods to `fakeWeb`. Keep `APIQuerier` unchanged because
device ingestion does not use categories.

## Validation and Limits

Use shared constants:

```text
maximum categories per user: 50
maximum category name length: 64 characters
maximum application key length: 255 bytes
allowed colors: coral, amber, leaf, teal, sky, indigo, rose, slate
```

Handlers:

- normalize category names before persistence;
- reject empty or overlong names;
- accept only a fixed color key;
- reject empty, non-canonical, or oversized application keys;
- reject unknown record scopes and actions;
- reject invalid scope/action combinations;
- parse positive category IDs strictly;
- scope every operation to the signed-in user;
- return `404` for absent and foreign-owned resources;
- render `400 Bad Request` for duplicate names.

The database remains the final authority for limits and uniqueness. Category
creation starts a transaction, locks the stable user row with
`SELECT id FROM users WHERE id = $1 FOR UPDATE`, counts the user's categories,
and inserts only below the limit. The user-row lock serializes concurrent
creation even when the user has no category rows. A count inside an unlocked
`INSERT ... SELECT` remains racy under `READ COMMITTED`.

## Styling and Accessibility

Keep the existing visual language and responsive breakpoints. Map color keys to
fixed CSS classes; never interpolate submitted text into a class or style
attribute.

- Pair every color swatch with a visible category name.
- Give every form control an explicit label.
- Announce mutation results through a status region.
- Name the category in each delete control.
- Preserve keyboard focus after HTMX swaps.
- Test category and dashboard layouts at desktop and mobile widths.
- Keep empty, error, and loading states understandable without color.

## Verification

### Database

- Apply and roll back migration `008`.
- Verify the two category-cascade index definitions through `pg_indexes);
  do not assert planner choice.
- Enforce case-insensitive category-name uniqueness per user while permitting
  the same name for different users.
- Reject unnormalized names, invalid colors, and non-canonical application keys.
- Verify category, default, and override updates advance `updated_at`. Seed
  and commit an older sentinel timestamp, run the public update in a new
  transaction, then compare against the sentinel. Do not compare two `NOW()`
  values from one transaction.
- Race two category creations for the final slot and confirm the user-row lock
  keeps the count at 50.
- Count application defaults per category without counting record overrides.
- Cover assignment upsert, removal, and category deletion.
- Distinguish no override, explicit Uncategorized, and assigned override.
- Confirm clearing an override restores inheritance.
- Reject public override mutations for foreign-owned records and categories.
- Reject a direct non-null override whose `user_id` does not own the category.
- Insert a direct explicit-null override with a wrong `user_id`. Confirm the
  owner's summary ignores it, the wrong user cannot read it, and a public
  upsert repairs it.
- Confirm record and device deletion cascade to record overrides.
- Confirm category deletion removes defaults and overrides but leaves activity.
- Return an empty assignment map without querying when the key list is empty.
- Verify known-application cutoff, result limit, canonicalization, and latest
  timestamp selection.

### Editable Records

- Use table-driven cases for the half-open overlap predicate. Include records
  crossing each boundary and records touching each boundary.
- Confirm displayed intervals are clipped to the requested window.
- Verify small keyset pages are stable, non-overlapping, and gap-free.
- Interleave records from several devices and confirm global
  `(ended_at DESC, id DESC)` order across cursors.
- Give the handler a truncated activity summary and an editable page containing
  a record absent from that summary. Confirm the editor renders the record
  without creating 5,001 fixtures.
- Confirm a trusted Chrome-producer row named `Whatever` appears under Google
  Chrome and not under an unknown `Whatever` subject.
- Derive test cases from every configured browser producer and desktop alias.
- Record representative one-device and all-device `EXPLAIN` output. Accept the
  all-device Sort and compare rows examined, buffers, and execution time.

### Aggregation

- Assign one application to a category.
- Sum several applications into one category.
- Sum unassigned applications into Uncategorized.
- Omit an empty Uncategorized bucket.
- Fold Chrome and Firefox aliases before category lookup.
- Apply categories after browser overlap removal.
- Give record overrides precedence over application defaults.
- Give explicit Uncategorized precedence over an application default.
- Restore the current application default after clearing an override.
- Combine effective slices from one source record under one override.
- Allocate sub-second splits with largest remainder.
- Resolve equal remainders by destination key, with Uncategorized key `0`.
- Compute category totals from the full source set when the test applies a
  timeline cap smaller than its fixture.
- Preserve the full category total when more than 20 applications contribute.
- Keep category totals equal to published application seconds.
- Sort equal-duration categories by name.
- Reassign historical activity without modifying `activity_records`.
- Run every existing deduplication and browser-family test without changing an
  expected value.

### HTTP and Templates

- Require a session for category pages and mutations.
- Require a valid CSRF token for every mutation.
- Cover category CRUD, application defaults, record overrides, explicit
  Uncategorized, and inheritance reset.
- Reject invalid IDs, names, colors, keys, scopes, actions, and combinations.
- Render duplicate names as `400 Bad Request`.
- Return `404` for foreign-owned IDs without revealing ownership.
- Derive application-scope record edits from the owned database record.
- Use the generic error response for database failures.
- Render correct HTMX partials and redirect full requests with
  `303 See Other`.
- Show Categories in authenticated navigation.
- Render empty and populated management states.
- Preserve current application assignments in selectors.
- Distinguish inherited, overridden, and explicit-Uncategorized record rows.
- Label record scope separately from application-default scope.
- Render dashboard category totals from the full summary.
- Restrict category colors to the fixed class list.
- Include CSRF fields and accessible labels in every form.

## Implementation Order

1. Add migration `008_categories`, indexes, models, constants, and database
   tests. Measure the activity index build before production.
2. Extract `canonicalAppName(producer, appName)` and add the assignment-key
   helper.
3. Add the time-windowed known-application query. Test aliases and inspect a
   representative plan.
4. Add category CRUD, application defaults, paginated editable records, and
   record overrides. Join override state into summary and editor queries.
5. Refactor the effective-duration traversal. Add category precedence and
   largest-remainder allocation without changing existing application totals.
6. Extend `WebQuerier`, `fakeWeb`, page data, routes, and handlers.
7. Add category templates, HTMX partials, navigation, and fixed color classes.
8. Add the paginated record editor to application details.
9. Render complete category totals and application labels on the dashboard.
10. Run formatting, tests, race tests, lint, and hooks.

Run these commands throughout implementation:

```sh
mise run format
mise run test
mise run test-race
mise run lint
```

Start PostgreSQL with `mise run db` for migration, query, ownership,
concurrency, and plan tests. Run `mise run hooks` before committing.

## Acceptance Criteria

- Users can manage up to 50 named, colored categories.
- Users can assign every known application to one category or Uncategorized.
- Application defaults affect inherited past and future records immediately.
- Users can override one source record without changing its application default.
- Users can store explicit Uncategorized or clear an override to resume
  inheritance.
- Record overrides persist across restarts and survive later default changes.
- Paginated application details reach records beyond the timeline cap.
- The record editor categorizes applications outside the 180-day discovery
  window.
- Category totals exactly partition application totals after deduplication.
- Canonical browser aliases cannot receive conflicting defaults.
- Category deletion never deletes or rewrites activity.
- Category routes never expose or mutate another user's data.
- Existing ingestion clients, the daemon, and extensions continue unchanged.
- Production rollout accounts for the activity index's write lock or first adds
  non-transactional migration support.
- Dashboard and management pages work without project-owned JavaScript.
- Formatting, tests, race tests, lint, and hooks pass.

## Deferred Work

- Website categories.
- Multiple categories or free-form tags per application or record.
- Productivity weights or scores.
- Category timeline filters.
- Category detail pages with sessions, titles, and daily breakdowns.
- Category assignment and total exports.
- Category-specific tracking rules in the daemon or extensions.
