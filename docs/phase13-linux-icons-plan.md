# Phase 13: Linux Application Icons

## Context

Every application on a Linux device renders as a monogram chip. Not
some of them, and not the ones with unusual names: all of them, on sway
and on X11 alike.

One field breaks the chain. Icons reach the server only when the daemon
fills `WindowInfo.AppIcon`, and `maybeEnqueueAppIcon` returns
immediately when it is nil, so nothing is hashed, validated, queued, or
posted:

```go
// internal/tracker/app_icon.go:222
func (t *Tracker) maybeEnqueueAppIcon(info WindowInfo, now time.Time) {
	if info.AppIcon == nil || t.reporter == nil {
		return
	}
```

Only `detectorCore.ActiveWindow` sets that field, and only when its
`iconFor` hook is present. The single non-test caller of
`newAppIconCache` is `window_darwin.go:78`, behind `darwin && cgo`.
Both Linux detectors bypass `detectorCore` and return a bare literal:

```go
// sway_linux.go:158
return WindowInfo{AppName: swayAppName(node), Title: strings.TrimSpace(node.Name)}, nil

// window_linux.go:95
return WindowInfo{AppName: appName, Title: title}, nil
```

The cache, the PID-keyed dedup, the worker queue, the upload endpoint,
the storage and the dashboard delivery all work, and Linux reaches none
of them. Phase 6 said so, listing under Non-Goals:

> - Linux application icons or freedesktop desktop-entry resolution.

macOS hands over a frontmost PID and AppKit renders a bundle icon
directly, which made it the cheaper first slice.

### What the freedesktop side offers

Measurements from a live sway session decide this plan, not what the
specifications permit.

Five open windows, checked against every desktop entry on
`XDG_DATA_HOME` plus `XDG_DATA_DIRS`:

```text
app_id                                              exact  case-insensitive
firefox-beta                                        yes    yes
Slack                                               no     yes
brave-browser                                       yes    yes
com.mitchellh.ghostty                               yes    yes
org.telegram.desktop._42ce2b9c5259...               yes    yes
```

Every one resolves, and a capital letter causes the single miss.
`icon.AppKey` already lowercases and collapses whitespace, so the key
the daemon computes today, `slack`, is the lookup key this needs.

Across 304 desktop IDs, 265 declare an icon. Where that icon lives, and
in what format, decides how much of the specification this phase has to
implement. The rows are disjoint and sum to 304:

```text
  116  PNG in hicolor or /usr/share/pixmaps      reachable
   51  PNG, but only in a theme other than hicolor   reachable
   18  Icon= is an absolute path to a PNG        reachable
    8  Icon= is an absolute path to SVG or XPM   not this phase
   55  SVG or XPM only                           not this phase
   12  no icon file anywhere                     unreachable
    5  absolute path, file missing               unreachable
   39  no Icon= key
```

Reachable is `116 + 51 + 18 = 185` of the 265 that declare an icon, or
**70%**. Searching `hicolor` plus `/usr/share/pixmaps` alone gives
`116 + 18 = 134`, or **51%**. An SVG and XPM decoder would later reach
`185 + 8 + 55 = 248`, or **94%**.

Four of the absolute paths end in `.svg` and four in `.xpm`, so the
absolute-path row has to split: counting all 26 existing absolute paths
as reachable overstates the figure by three points.

The 51 in the second row rule out searching hicolor alone. They are not
obscure. The icon theme configured on this machine is Yaru, and Yaru
holds 482 PNGs that hicolor lacks, so a hicolor-only search leaves a
fifth of installed applications on monograms while their artwork sits
on disk.

Searching every theme is worse. 564 of 2134 PNG stems exist in more
than one theme. `steam`, `chromium-browser`, `filezilla` and 561 others
appear in Yaru, gnome, HighContrast and hicolor at once, so a flat
name-to-path index picks arbitrary artwork and can hand a user the
HighContrast variant of one application beside the Yaru variant of the
next.

## Decisions

### The identity is the key that already exists

`icon.AppKey(appName)` produces the lookup key, the upload key, the
storage key and the dashboard query key, exactly as it does on macOS.
A resolver that agrees with `AppKey` cannot drift from the row the
dashboard later reads. No new identity appears anywhere in this phase.

### Entries are keyed by desktop file ID, and Hidden masks

The lookup key is the desktop file ID from the naming specification,
not the basename. `applications/screensavers/swirl.desktop` has the ID
`screensavers-swirl`, with the separator turned into a dash. 22 entries
here are nested, and a basename index cannot see them.

The two visibility keys mean different things and get different
handling:

- `NoDisplay=true` means the application exists but does not belong in
  a menu. Its icon is valid. 106 entries here set it while declaring an
  icon, so skipping them discards 40% of the installed base for no
  reason.
- `Hidden=true` means the entry is deleted. It masks any
  lower-precedence entry with the same ID and stops the search instead
  of falling through to the copy in `/usr/share`.

An unreadable file, or one carrying no `[Desktop Entry]` group, claims
no ID at all. One truncated file in `~/.local/share/applications` would
otherwise delete a working entry in `/usr/share` and take the
application's icon with it, and `Hidden=true` is meant to be the only
way to ask for that. An entry that parses but declares no icon does
claim its ID: a real installed application should not borrow artwork
from a lower-precedence namesake.

### X11 and sway both get a StartupWMClass index

`StartupWMClass` ties a window's `WM_CLASS` to a desktop entry, and 67
entries here declare one. 34 of those values reach an entry that no
basename or ID match finds. `jetbrains-pycharm` names an entry called
`jetbrains-pycharm-b3d5c3dd-9c56-4af1-9db4-569ed5551fdd`, and
`TelegramDesktop` names one suffixed with a content hash.

The resolver builds a second index over `StartupWMClass`, consulted
after the ID match fails and before giving up, case-insensitive for the
same reason the ID match is.

Both indexes fill during one walk, at the moment an entry claims its
ID. Two entries may declare the same `StartupWMClass`, a vendor
shipping a stable and a beta build being the ordinary case, and a
second pass over a map would hand the class to a different entry on
every rebuild as map iteration order shifts. Filling the index inside
the walk keeps directory precedence and takes the post-masking winner
for free: a `Hidden=true` user entry with no `StartupWMClass` of its
own suppresses the ID, and the masked system entry cannot answer to its
`WM_CLASS`.

### Icon lookup follows the specification's theme chain

Size selection departs from the specification in two places, argued
below. The search order is the specification's:

1. Base directories in fixed order: `$XDG_DATA_HOME/icons`, `~/.icons`
   for legacy installs, each `$XDG_DATA_DIRS/icons`, then
   `/usr/share/pixmaps` as the unthemed fallback.
2. Themes in order: the configured theme, its `Inherits` chain
   depth-first, then `hicolor` as the guaranteed terminal. Yaru here
   declares `Inherits=Humanity,hicolor`.
3. Within a theme, only the subdirectories its `index.theme` lists,
   reading each group's `Size`, `Scale`, `Type`, and for `Type=Scalable`
   the `MinSize` and `MaxSize`, or for `Type=Threshold` the
   `Threshold`.

Directory names never supply a size. Yaru's `[48x48/apps]` group
happens to declare `Size=48`, but nothing requires that agreement, and
`16x16@2x` holds 32-pixel icons under `Scale=2`. One small INI parse
per theme removes the whole class of guess.

One parser serves both file types. `index.theme` and `.desktop` share
an INI dialect, so the desktop entry parser is written once and reused.
It decodes the escapes of `iconstring`, the value type `Icon=` carries:
`\s`, `\n`, `\t`, `\r` and `\\`. Without them an `Icon=` naming a path
with a space resolves to nothing.

### One theme can span several base directories

A theme is a name, not a directory, and its parts may sit in several
base directories at once. A user drops one replacement icon into
`~/.local/share/icons/Yaru` without copying the theme.

Two rules follow, and they differ:

- Metadata comes from the **first** `index.theme` found in base
  directory order. Later copies never merge, so a stale `Directories=`
  list in `/usr/share` cannot override the user's.
- Icon files are searched across **every** base directory holding that
  theme, in the same order. A user override in
  `~/.local/share/icons/Yaru/64x64/apps` beats
  `/usr/share/icons/Yaru/64x64/apps` for the same subdirectory.

Merging metadata would silently drop a subdirectory listed in one copy
and absent from another. Searching only the first base would hide user
overrides.

### Getting the configured theme is the weakest link

Reading `gtk-icon-theme-name` from `$XDG_CONFIG_HOME/gtk-3.0/settings.ini`
and falling back to `hicolor` looks sufficient. Tried against the
machine this plan was written on:

```text
~/.config/gtk-4.0/settings.ini   exists, no icon-theme key
~/.config/gtk-3.0/settings.ini   exists, no icon-theme key
~/.gtkrc-2.0                     absent
/etc/gtk-3.0/settings.ini        gtk-icon-theme-name = Yaru
gsettings                        'Yaru'
```

That reader finds nothing and selects `hicolor`, losing the 51 entries
whose only PNG is in Yaru, which is the entire reason the theme chain
exists. The phase's main mechanism fails silently and completely, on
the developer's own machine.

GTK does not treat `settings.ini` as the source of truth. It reads the
desktop-wide setting from the Wayland settings portal or XSettings
first, and many systems never write `settings.ini` at all.

So four sources answer in explicit priority order, and a test can drive
each one:

1. `icon_theme` in trackkr's own config, or `TRACKKR_ICON_THEME`.
   Deterministic, needing no desktop integration, and the documented
   escape hatch when the guesses below are wrong.
2. `gsettings get org.gnome.desktop.interface icon-theme`, when
   `gsettings` is on `PATH`. The real answer lives here on this machine
   and on most GTK desktops. It stays optional, it matches the
   dependency the daemon already carries on `xdotool` and `xprop`, and
   it runs once per index build.

    `exec.CommandContext` bounds it with a short deadline, like every
    other exec in `internal/tracker`. `gsettings` reads over D-Bus, and
    a wedged session bus must not stall an index build. Step 4 exists
    so the phase never depends on that bus directly.
3. `gtk-icon-theme-name` from, in order: `$XDG_CONFIG_HOME/gtk-4.0`,
   `$XDG_CONFIG_HOME/gtk-3.0`, each `$XDG_CONFIG_DIRS/gtk-4.0` and
   `gtk-3.0`, then `/etc/gtk-4.0` and `/etc/gtk-3.0`, which are GTK
   system paths holding this machine's answer, then `~/.gtkrc-2.0`.
4. `hicolor`.

`~/.gtkrc-2.0` hangs off `HOME`, never off `XDG_CONFIG_HOME`, so
`iconRoots` carries the home directory as its own field. Deriving that
path from a config directory works only while the two happen to agree.

Querying the portal over D-Bus stays out of scope. It is the only fully
correct answer, and it costs a D-Bus dependency and a session bus the
daemon may not have. The override at step 1 makes a wrong guess
recoverable.

### PNG this phase; SVG stays on the monogram

63 entries, 55 by name and 8 by absolute path, have only vector or XPM
artwork, and they keep their monograms. Covering them means a
rasterizer. `oksvg` and `rasterx` are the usual pairing, and they bring
partial SVG support, a new dependency, and a new class of
untrusted-input parsing into a daemon that decodes nothing but PNG.

The trade is 70% now for no new dependency against 94% later. It stays
a self-contained follow-up because the resolver's contract holds: a
vector candidate is not eligible, and the search continues past it.

### Size selection deviates from the specification, deliberately

The image contract is Phase 6's, unchanged: a 64x64 canvas,
`icon.MaxPNGBytes` of 64 KiB, `icon.MaxDimension` of 128. The canvas
size travels as `icon.NormalizedDimension`, so the search target and
the resample target cannot drift apart.

Two separate departures follow.

Directory eligibility keeps the specification's `Type` semantics and
measures every group by `Size * Scale`:

```text
Fixed      Size * Scale == 64
Scalable   MinSize * Scale <= 64 <= MaxSize * Scale
Threshold  (Size - Threshold) * Scale <= 64 <= (Size + Threshold) * Scale
```

An exact match ends the search within that theme.

The specification rejects any group whose `Scale` differs from the
requested scale, so a toolkit asking for 64 at scale 1 never sees
`16x16@2x`. That rule is right for what it describes: a scale-2 file
fills a 16-point box on a HiDPI display and is not meant to be drawn at
32 points. Trackkr has no display and no points. It resamples once to a
64-pixel raster and stores it, and `16x16@2x` is 32 physical pixels of
artwork on disk. Folding `Scale` into the size, instead of gating on
it, is what "best available source" means in this pipeline.

Ranking the near misses is the second departure.
`DirectorySizeDistance` is symmetric, so given 48 and 128 for a 64
request it returns 16 and 64 and picks 48. This phase picks 128,
because trackkr is not choosing an icon to display at 64. It chooses a
source to resample to exactly 64, once, and store forever. Downscaling
128 keeps detail. Upscaling 48 invents it, and the result sits soft in
a list where every neighbouring icon is crisp.

So the smallest eligible candidate at or above 64 wins. When nothing in
the chain reaches 64, the largest below it gets upscaled instead of
refused, since a soft icon still reads better than two letters in a
coloured square. A directory covering the target through a range,
`Scalable` or `Threshold`, ranks as supplying the target rather than
its nominal `Size`, or a spanning group loses to a larger fixed one.

Theme order outranks size throughout. A 48x48 from the configured theme
beats a 256x256 from `hicolor`, because the user chose the theme and
did not choose the resolution.

The implementation marks both deviations where the eligibility test and
the comparison live, so the next reader does not correct either back.

### A bad file falls through to the next candidate

A path that exists is not yet an answer. A candidate resolves only once
its bytes have been read within `MaxSourceBytes` and normalized. A
truncated PNG, an oversized one, or a file the daemon cannot open moves
to the next candidate.

Next means next in the search order already established, not next
theme. A corrupt 64x64 in the configured theme is followed by that
theme's other eligible sizes, then by every base directory holding that
theme, and only then by the inherited themes and `hicolor`. Jumping to
the next theme on the first bad file would discard the user's chosen
artwork over one damaged size, and the failure would be invisible: a
monogram looks the same whether the search found nothing or gave up
early.

### Reads are bounded before anything is decoded

`internal/favicon` bounds its source at `maxSourceBytes` of 256 KiB *in
the fetcher*, not in the normalizer:

```go
// internal/favicon/fetcher.go:263
data, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBytes+1))
```

Moving the normalizer without that limit leaves the daemon reading an
arbitrarily large file off disk and decoding it, with only the
dimension checks standing in the way, and those run after the bytes are
already in memory. A 900x900 PNG can be tens of megabytes.

The limit travels with the code. `icon.Normalize` enforces
`MaxSourceBytes` before touching `image.DecodeConfig`, and the resolver
reads candidates through an `io.LimitReader` instead of `os.ReadFile`.

The bound is `MaxSourceBytes+1`, followed by a `len(data) >
MaxSourceBytes` comparison, matching the fetcher exactly. The `+1` is
the whole mechanism. A reader capped at `MaxSourceBytes` cannot
distinguish a file sitting at the limit from one truncated to it: a 300
KiB icon would arrive silently cut to 256 KiB, pass the length check,
then fail inside `image.DecodeConfig` on a half-read PNG. The icon goes
missing either way, but the failure reports corrupt artwork instead of
an oversized file, and the "refused before decode" test fails.

### Base directories take the specification's defaults, and only absolute paths

The lists above name `XDG_DATA_HOME`, `XDG_DATA_DIRS` and
`XDG_CONFIG_HOME`, all three routinely unset. `XDG_DATA_HOME` most of
all, because almost nothing sets it and almost everything assumes it.
Joining an empty string naively yields a relative path, and a daemon
started from any working directory then searches that directory instead
of the user's:

```text
XDG_DATA_HOME    unset or relative  ->  $HOME/.local/share
XDG_DATA_DIRS    unset or empty     ->  /usr/local/share:/usr/share
XDG_CONFIG_HOME  unset or relative  ->  $HOME/.config
```

The base directory specification requires an absolute path in each of
these variables and treats a relative one as invalid. Entries within
`XDG_DATA_DIRS` are filtered one by one, so a single bad element does
not discard the rest of the list.

`HOME` unset leaves nothing to fall back to, and the resolver then
contributes no user-level directory instead of searching `/.config`.

`DefaultDataDir` in `internal/tracker/config.go:246` takes the looser
reading and accepts any non-empty `XDG_DATA_HOME`. Changing it is out
of scope here, and the two should not be mistaken for one
implementation.

### The resolver takes its roots as arguments

Phase 6 set a boundary for `internal/icon`:

> The package performs no HTTP, database, filesystem, or platform-native
> work.

That sentence lives in `docs/phase6-icons-plan.md`, not in the package,
whose doc comment says only "Package icon defines application icon
identity and image validation". Since this phase widens what the
package does, the constraint moves into `internal/icon/app.go`'s
package comment, where the next person adding a file will read it.

The constraint is worth keeping, so the filesystem walk lives in
`internal/tracker` behind a `linux` build tag.

The resolver reads no environment variable of its own. Its constructor
takes the application directories, icon base directories, home
directory and configured theme name, and one small function derives
them from the environment. A test then builds a fake XDG tree with
`t.TempDir()` and never touches the real machine, which separates a
test that passes everywhere from one that passes on the author's
laptop.

### The rasterizer moves to internal/icon and is shared

`internal/favicon` already holds the normalizer this needs: decode any
format, scale to 64x64 with `draw.CatmullRom` onto a transparent
canvas, encode PNG, validate against the shared contract.

Duplicating it in the daemon would leave two normalizers whose output
must match with nothing checking that it does. It moves to
`internal/icon/raster.go`, gains the byte limit above, and
`internal/favicon` calls it. Decoding and scaling bytes is pure
computation, no different from the PNG validation already there, so the
package's stated boundary holds. `golang.org/x/image` is already direct
in `go.mod`, so nothing new is added.

### Indexes are built once; the cache owns a goroutine

The daemon polls every few seconds for the life of the session, so the
entry index, the `StartupWMClass` index and the per-theme directory
lists build on first use and hold for `resolverIndexLifetime`. A miss
never triggers a rebuild, because a newly installed application is not
worth a filesystem walk.

```go
// resolverIndexLifetime bounds how long a newly installed application
// keeps its monogram. A miss does not rebuild, so this constant alone
// decides that wait.
const resolverIndexLifetime = 15 * time.Minute
```

Fifteen minutes weighs the walk against the wait. The walk visits 304
desktop entries and roughly 2100 PNGs on this machine, milliseconds of
`os.ReadDir` on a warm page cache, at most four times an hour. The wait
is a quarter of an hour of monogram after installing something: long
enough to notice, short enough that nobody files a bug, and cheap to
lower once someone measures the walk on a slower disk.

Per-application results keep using `appIconCache` unchanged: five
minutes positive, thirty seconds negative, thirty days before a
re-upload.

`appIconCacheKey` is `{PID, Key}`, and Linux passes `PID: 0` for every
window by design. Sway's tree does carry a `pid`, so a reader will
reasonably wonder whether to plumb it through. They should not. The PID
earns its place on macOS because resolution goes through the running
process's bundle, and a relaunch genuinely needs re-resolving.
Freedesktop resolution is name-based, so a PID in the key would re-walk
the filesystem for an identical answer every time an application
restarts.

That cache starts a worker goroutine in its constructor and stops it
only in `Close`, so ownership has to be explicit. The daemon closes a
detector through a duck-typed `interface{ Close() }`
(`cmd/trackkrd/main.go:34`), which means:

- `SwayWindowDetector.Close` closes the cache as well as the IPC
  connection, and stays idempotent.
- `XWindowDetector` has no `Close` today and gains one, or its worker
  leaks for the life of the process.

## New Files

- `internal/tracker/icon_xdg_linux.go`: INI parser, entry and
  `StartupWMClass` indexes, theme chain, directory lists, size
  selection, bounded reads.
- `internal/tracker/icon_xdg_linux_test.go`
- `internal/icon/raster.go`: the shared normalizer, with
  `MaxSourceBytes` and an exported `NormalizedDimension`. The canvas
  size stops being a literal 64 and crosses the package boundary to
  become the resolver's search target, so "the smallest icon at or
  above 64" above means at or above that constant.
- `internal/icon/raster_test.go`

## Changed Files

- `internal/tracker/sway_linux.go`: hold a resolver and cache, set
  `AppIcon`, close both.
- `internal/tracker/window_linux.go`: the same for `XWindowDetector`,
  which also gains `Close`. `NewWindowDetector` builds one resolver and
  hands it to whichever detector it returns.
- `internal/favicon/fetcher.go`: call `icon.Normalize`, drop the local
  copy, its four constants, and the image decoder imports that came
  with it. `maxSourceBytes` becomes `icon.MaxSourceBytes` at the three
  fetch call sites.
- `internal/favicon/fetcher_test.go`: the three normalizer unit tests
  move out with the function, and the two surviving fetch tests gain a
  dimension assertion.
- `internal/icon/app.go`: move Phase 6's no-filesystem constraint into
  the package doc comment.
- `internal/tracker/config.go`: add the optional `icon_theme` field and
  its `TRACKKR_ICON_THEME` override.
- `internal/tracker/config_test.go`: `clearTrackkrEnv` neutralises the
  four existing `TRACKKR_*` variables by name and has to gain the
  fifth, or every config test starts depending on the shell that ran
  it.
- `docs/phase6-icons-plan.md`: strike the Non-Goals line this phase
  closes, pointing at this document.

## Steps

1. Move the normalizer into `internal/icon/raster.go` and add
   `MaxSourceBytes`. Favicons change behaviour only by the limit moving
   inward.

    The three unit tests move with it, so they prove nothing about the
    move. The fetch path proves it: `TestFetcherFallsBackToHTMLIcon`
    and `TestFetcherFallsBackToConventionalPNG` drive a whole fetch
    through `icon.Normalize` and must pass with no edit at all.
2. Write the INI parser: group-scoped, first value wins, `iconstring`
   escapes decoded. It serves `.desktop` and `index.theme`.
3. Derive the base directories, with the specification's defaults and
   the absolute-path filter.
4. Build the entry index in one post-masking pass: desktop file IDs,
   `Hidden` masking, `StartupWMClass` reverse index filled inside the
   same walk.
5. Resolve the theme name through the four-step priority order, then
   build the theme chain and per-theme directory lists from
   `index.theme`, with `DirectoryMatchesSize` eligibility.
6. Assemble the resolver: key in, `*icon.App` out, each candidate read,
   bounded and normalized before it counts as resolved, and the next
   candidate tried when one fails.
7. Wire it into both Linux detectors through `appIconCache`, with
   `Close` on both.
8. Verify on a real session against the applications below.

## Tests

Database-free, and parallel wherever the resolver's injected roots
allow. Only the environment derivation runs serially, since `t.Setenv`
panics in a parallel test.

- Entry lookup honours base directory order: the same ID in
  `XDG_DATA_HOME` and in `/usr/share` resolves to the former.
- Case-insensitive match: `Slack` finds `slack.desktop`. The one
  real-world miss gets a named test.
- Nested entries: `applications/foo/bar.desktop` is found as `foo-bar`,
  not as `bar`.
- `Hidden=true` in a higher-precedence directory masks the entry in a
  lower one, and the lookup returns nil instead of falling through.
- Masking precedes indexing: a `Hidden=true` user entry carrying no
  `StartupWMClass` still suppresses the masked system entry that does,
  so the class mapping cannot resurrect it.
- An unparseable entry claims no ID, checked three ways: a file whose
  only group is `[Desktop Action open]`, one truncated before any
  group, and an empty one. Each must leave the `/usr/share` entry
  reachable. The system entry's `Icon=` differs from the lookup key on
  purpose, or the key-as-icon-name fallback rescues the lookup and the
  test passes with the bug present.
- An entry that parses but declares no icon still claims its ID.
- `NoDisplay=true` resolves normally.
- `StartupWMClass` finds an entry whose ID does not match the key, and
  loses to an exact ID match when both exist.
- Colliding `StartupWMClass` values follow directory precedence, run
  twenty times: one pass over a two-element map picks correctly often
  enough to look green.
- Parser: values before `[Desktop Entry]` are ignored, a later
  `[Desktop Action foo]` group cannot supply `Icon=`, a duplicate key
  keeps the first, and `Icon=/opt/My\sApp/icon.png` unescapes to a path
  with a space that resolves.
- Theme chain: the configured theme wins over `hicolor` even at a
  smaller size, `Inherits` is followed depth-first, and a cycle in
  `Inherits` terminates.
- `icon_theme` precedence, serial with `t.Setenv` after
  `clearTrackkrEnv`: the file value is read, `TRACKKR_ICON_THEME`
  overrides it, and an empty variable does not blank a configured
  value, matching the four overrides already there.
- Theme name resolution, in priority order and each independently: the
  config override beats `gsettings`, `gsettings` beats every file, the
  file search finds `/etc/gtk-3.0/settings.ini` when neither
  `$XDG_CONFIG_HOME/gtk-4.0` nor `gtk-3.0` carries the key, and
  `hicolor` terminates the chain. An injected lookup stands in for
  `gsettings`, so the suite never requires the binary.
- `~/.gtkrc-2.0` is found when `XDG_CONFIG_HOME` points outside `HOME`,
  and a `settings.ini` still outranks it. The assertion runs over
  isolated roots, because the real `/etc/gtk-3.0/settings.ini` is a
  legitimate higher-precedence source and would otherwise test the
  developer's desktop.
- Missing and malformed GTK settings files yield an empty theme name
  rather than a panic: no roots at all, an empty directory, a directory
  that does not exist, and files whose only group is `[NotSettings]` or
  that are not INI. Reading a missing key from a nil map yields the
  zero value in Go, which is subtle enough to pin.
- One theme across two base directories: `index.theme` in
  `/usr/share/icons/T` supplies the metadata while a file in
  `~/.local/share/icons/T` wins for the same subdirectory, and a second
  `index.theme` in the user directory does not merge its `Directories=`
  into the first.
- Directory metadata drives size, not directory names: a group named
  `[huge/apps]` carrying `Size=64` is selected for 64, and `16x16@2x`
  with `Scale=2` counts as 32 rather than 16.
- Eligibility by `Type`: a `Scalable` group with `MinSize=8` and
  `MaxSize=512` is a candidate for 64, a `Threshold` group with
  `Size=48` and `Threshold=16` is one, and the same group with
  `Threshold=8` is not.
- The scale deviation, asserted directly: a group with `Size=32` and
  `Scale=2` is eligible for 64 and beats a `Size=48`, `Scale=1` group
  in the same theme. The specification would reject it outright, so
  reverting to a `Scale`-must-match gate fails here.
- A spanning directory ranks at the target: a `Scalable` group with
  `Size=16`, `MinSize=8`, `MaxSize=512` beats a `Size=128` fixed group,
  which ranking on nominal `Size` gets backwards.
- Size selection: 64 wins outright when present, and with only 48 and
  128 available, 128 wins. The second case is the documented ranking
  deviation from `DirectorySizeDistance`, which would pick 48, asserted
  so that reverting it fails loudly instead of quietly changing what
  every Linux user sees.
- 48 wins when nothing in the chain reaches 64.
- `Icon=` absolute path is used directly, and an absolute path to a
  missing file yields nil rather than an error.
- Bounded reads: a 300 KiB PNG whose dimensions are otherwise valid is
  refused before decode, and the refusal is cached no differently from
  other misses.
- Bounded reads distinguish at the boundary: a file of exactly
  `MaxSourceBytes` normalizes, and one a single byte larger is refused.
  Capping the reader at `MaxSourceBytes` instead of `MaxSourceBytes+1`
  passes the first and fails the second.
- Ordered fallback, three candidates: a corrupt 64x64 in the configured
  theme, a valid 128x128 in the same theme, and a valid 64x64 in
  `hicolor`. The same-theme 128 must win. Two candidates cannot
  distinguish correct fallback from a jump straight to `hicolor`.
- Only when every candidate in every theme fails does the resolver
  return nil.
- Unreadable candidates are simulated with a directory at the candidate
  path and with a dangling symlink, never with file permissions: a
  suite running as root reads a `0o000` file happily, and containers
  run as root often enough that such a test would pass without
  exercising anything.
- Environment derivation, each in its own serial test using `t.Setenv`:
  unset `XDG_DATA_HOME` falls back to `$HOME/.local/share`, unset
  `XDG_DATA_DIRS` to `/usr/local/share:/usr/share`, unset
  `XDG_CONFIG_HOME` to `$HOME/.config`. A relative value in any of them
  is ignored instead of joined, one relative element inside
  `XDG_DATA_DIRS` does not discard the absolute ones, and `HOME` unset
  contributes no user-level directory.
- Source bounds still reject a 2000x2000 PNG and a declared-small,
  actually-large one.
- Normalizer parity: the three `normalizeImage` tests move to
  `internal/icon/raster_test.go` with the function and lose the now
  redundant `Image` from their names, becoming `TestNormalize`,
  `TestNormalizeAcceptsICO` and `TestNormalizeRejectsOversizedDimensions`.
  Grepping the old names finds nothing, so the rename is recorded here
  and does not read as a deletion.

    They cannot stay in `internal/favicon` and change only their call
    site: they also read `maxSourceDimension`, which is package-private
    and moves too. Exporting it to keep three tests in place would
    widen the package's API for nothing.

    What stays proves parity. `TestFetcherFallsBackToHTMLIcon` and
    `TestFetcherFallsBackToConventionalPNG` run the full fetch path
    through `icon.Normalize`. No `internal/favicon/testdata` exists to
    diff against, since every test builds its images inline with
    `png.Encode`, so an end-to-end path is the available check.

    Both gain a dimension assertion to be worth anything. They called
    `icon.ValidatePNG` alone, and their fixtures are already valid
    PNGs, so a fetch path returning the untouched 24x24 or 32x32 source
    would have passed. Asserting the result is `NormalizedDimension`
    square makes them detect a normalizer that stopped being called:
    stubbing out `icon.Normalize` fails them with
    `result is 24x24, want 64x64`.
- Indexes build once and then rebuild: a resolver over a counting fake
  root reports one walk across many lookups, and a second walk once an
  injected clock passes `resolverIndexLifetime`. The rebuild case keys
  on a new desktop entry, not a new icon file, because candidate files
  are stat-ed on every lookup and would resolve without any rebuild.
- Lifecycle: `SwayWindowDetector.Close` and `XWindowDetector.Close`
  each stop the cache worker, survive a second call, and leave no
  goroutine behind under `-race`.
- Both detectors are wired, asserted separately: `SwayWindowDetector`
  and `XWindowDetector` each set `AppIcon` from a stub resolver, and
  each leave it nil when the resolver is nil. Testing only sway would
  let a missed assignment in `window_linux.go` ship green, which is the
  exact shape of the bug this phase exists to fix.

## Out of Scope

- SVG and XPM rasterization, and with it the 63 entries whose only
  artwork is vector.
- Icon theme scale preference beyond `Size * Scale`. The daemon
  produces one 64x64 raster, and HiDPI variants of the same icon are a
  server-side and dashboard concern if they ever matter.
- `Context` filtering. Restricting to `Context=Applications` would be
  defensible, but the entry's `Icon=` name is already application
  specific, and no sampled case resolved to a mimetype icon.
- Per-window icons. Sway's tree carries none, and a title-bar icon is
  not what the dashboard shows.
- Windows, and any change to macOS acquisition.
- Any change to the upload endpoint, storage, pruning, or dashboard
  rendering. This phase fills one nil field, and everything downstream
  is already built and already tested.

## Manual Verification

```bash
# Resolve what the running session would ask for.
swaymsg -t get_tree | rg -o '"app_id": "[^"]+"' | sort -u

# The configured theme the chain should start from, asked in the same
# priority order the resolver uses. The first non-empty answer wins;
# on the machine this plan was written on that is gsettings, and the
# only file carrying the key is the one under /etc.
echo "${TRACKKR_ICON_THEME:-unset}"
gsettings get org.gnome.desktop.interface icon-theme 2>/dev/null
rg -n gtk-icon-theme-name \
  ~/.config/gtk-4.0/settings.ini ~/.config/gtk-3.0/settings.ini \
  /etc/gtk-4.0/settings.ini /etc/gtk-3.0/settings.ini \
  ~/.gtkrc-2.0 2>/dev/null

# Checking only ~/.config/gtk-3.0/settings.ini is the mistake the
# theme-name section exists to prevent: it is empty here, and a correct
# resolver would look like a broken one.

# After running the daemon for one poll interval:
rtk psql -c "select app_key, width, height, octet_length(png) \
  from app_icons order by updated_at desc limit 10"
```

The dashboard should show artwork for `firefox-beta`,
`com.mitchellh.ghostty`, `brave-browser` and `slack`, and keep the
monogram for `Alacritty`, whose icon is SVG-only.
