//go:build linux

package tracker

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	// resolverIndexLifetime bounds how long a newly installed application
	// keeps its monogram. A lookup miss deliberately does not rebuild, so
	// this constant alone decides that wait.
	resolverIndexLifetime = 15 * time.Minute

	// hicolorTheme is the fallback every icon theme ultimately inherits.
	hicolorTheme = "hicolor"

	// themeChainLimit stops a cycle or a pathological Inherits graph from
	// walking forever. No real theme is nested anywhere near this deep.
	themeChainLimit = 16

	// desktopEntrySuffix is what a desktop file ID is derived from.
	desktopEntrySuffix = ".desktop"

	// maxDesktopFileBytes bounds one INI read. Real entries are under a
	// kilobyte; Yaru's index.theme is the largest seen at a few KiB.
	maxDesktopFileBytes = 256 << 10

	// gsettingsTimeout bounds the desktop-setting query. gsettings talks
	// to D-Bus, and a wedged session bus must not stall an index build.
	gsettingsTimeout = 2 * time.Second
)

// Icon theme directory types, as index.theme spells them. Threshold is
// the specification's default for a group that names no Type.
const (
	dirTypeFixed     = "fixed"
	dirTypeScalable  = "scalable"
	dirTypeThreshold = "threshold"

	// defaultThreshold is the specification's default for a Threshold
	// directory that names none.
	defaultThreshold = 2
)

// iconRoots is where the resolver looks, resolved once from the
// environment. Every field is absolute; see xdgIconRoots.
type iconRoots struct {
	// applicationDirs hold desktop entries, most specific first.
	applicationDirs []string
	// themeBases hold icon themes, most specific first.
	themeBases []string
	// unthemedDirs are searched after every theme: /usr/share/pixmaps
	// belongs to no theme but holds icons entries still name.
	unthemedDirs []string
	// configDirs hold the GTK settings files the theme name is read from.
	configDirs []string
	// homeDir is where ~/.gtkrc-2.0 lives. It is carried separately
	// because that path is anchored to HOME, not to XDG_CONFIG_HOME,
	// which a user may point somewhere else entirely.
	homeDir string
}

// desktopEntry is the part of a .desktop file the resolver reads.
type desktopEntry struct {
	icon           string
	startupWMClass string
	// hidden marks a deleted entry: it masks the same ID in a
	// lower-precedence directory rather than being skipped.
	hidden bool
}

// iconResolver turns a normalized application key into PNG bytes by
// walking the freedesktop desktop-entry and icon-theme layouts.
//
// Its roots arrive as arguments rather than being read from the
// environment, so a test builds a fake tree with t.TempDir and never
// depends on what the machine running it happens to have installed.
type iconResolver struct {
	roots *iconRoots
	// themeName is the configured theme, or "" to detect it per rebuild.
	themeName string
	// lookupTheme reads the desktop's configured theme. Injected so a
	// test never shells out.
	lookupTheme func(*iconRoots) string
	// now and lifetime drive index expiry.
	now      func() time.Time
	lifetime time.Duration

	mu        sync.Mutex
	built     time.Time
	entries   map[string]desktopEntry // by lowercased desktop file ID
	byWMClass map[string]desktopEntry // by lowercased StartupWMClass
	chain     []themeDir              // every searchable directory, in order
}

// themeDir is one directory a theme declares, with the metadata that
// decides whether it can serve a requested size.
type themeDir struct {
	path string
	// order is the position of the owning theme in the search chain, so a
	// smaller icon from the configured theme still outranks a larger one
	// from an inherited theme.
	order int
	size  int
	scale int
	// minSize and maxSize bound a Type=Scalable directory; threshold
	// widens a Type=Threshold one either side of size.
	minSize   int
	maxSize   int
	threshold int
	kind      string
	unthemed  bool
}

func newIconResolver(roots *iconRoots, themeName string) *iconResolver {
	return &iconResolver{
		roots:       roots,
		themeName:   themeName,
		lookupTheme: gsettingsTheme,
		now:         time.Now,
		lifetime:    resolverIndexLifetime,
	}
}

// resolve returns the icon for a normalized application key, or nil when
// the desktop offers none this phase can decode.
//
// Every failure is nil rather than an error. The monogram the dashboard
// falls back to is a designed state, not a broken one, and an
// application with no installed icon is the ordinary case rather than a
// fault worth surfacing.
func (r *iconResolver) resolve(ctx context.Context, key string) *icon.App {
	if key == "" || ctx.Err() != nil {
		return nil
	}
	r.ensureIndex()

	r.mu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry, ok = r.byWMClass[key]
	}
	chain := r.chain
	r.mu.Unlock()

	// An application whose key names an icon directly is common enough to
	// try even when it has no desktop entry at all.
	name := key
	switch {
	case ok && entry.hidden:
		return nil
	case ok && entry.icon != "":
		name = entry.icon
	}

	png := r.loadNamed(ctx, name, chain)
	if png == nil {
		return nil
	}
	return &icon.App{Key: key, PNG: png}
}

// loadNamed walks every candidate for an icon name in search order and
// returns the first that survives reading and normalization.
//
// A candidate that exists is not yet an answer. A truncated PNG, an
// oversized one, or a path that turns out to be a directory moves to the
// next candidate in the same order -- the rest of this theme before any
// inherited one -- because giving up on the first bad file would discard
// the user's chosen artwork over one damaged size.
func (r *iconResolver) loadNamed(ctx context.Context, name string, chain []themeDir) []byte {
	if filepath.IsAbs(name) {
		return readIconFile(name)
	}
	for _, candidate := range rankedCandidates(name, chain) {
		if ctx.Err() != nil {
			return nil
		}
		if png := readIconFile(candidate); png != nil {
			return png
		}
	}
	return nil
}

// readIconFile reads one candidate within the byte bound and normalizes
// it, returning nil for anything that is not usable artwork.
func readIconFile(path string) []byte {
	file, err := os.Open(path) // nosemgrep // gitlab-advanced-sast-exclude -- path is composed from indexed icon roots.
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return nil
	}
	// MaxSourceBytes+1 so a file exactly at the limit is distinguishable
	// from one truncated to it; see icon.MaxSourceBytes.
	data, err := io.ReadAll(io.LimitReader(file, icon.MaxSourceBytes+1))
	if err != nil || len(data) > icon.MaxSourceBytes {
		return nil
	}
	png, err := icon.Normalize(data)
	if err != nil {
		return nil
	}
	return png
}

// ensureIndex builds the entry and directory indexes, or reuses them
// until they expire.
//
// A lookup miss does not rebuild. A newly installed application is not
// worth a filesystem walk, and it resolves on the next natural expiry.
func (r *iconResolver) ensureIndex() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries != nil && r.now().Sub(r.built) < r.lifetime {
		return
	}
	r.entries, r.byWMClass = r.buildEntries()
	r.chain = r.buildChain()
	r.built = r.now()
}

// buildEntries indexes desktop entries by ID and by StartupWMClass.
//
// Both indexes come from the same masked set in one pass. Masking at
// lookup time instead would leave a deleted entry reachable through its
// class mapping: a user Hidden=true file carrying no StartupWMClass of
// its own would suppress the ID while the masked system entry kept
// answering to its WM_CLASS.
func (r *iconResolver) buildEntries() (byID, byClass map[string]desktopEntry) {
	byID = make(map[string]desktopEntry)
	byClass = make(map[string]desktopEntry)

	for _, dir := range r.roots.applicationDirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable directory is a miss, not a failure: an
				// application with no icon is the ordinary outcome here.
				return nil //nolint:nilerr // Walk errors are skipped, not propagated.
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), desktopEntrySuffix) {
				return nil
			}
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				// WalkDir only yields paths under dir, so this is
				// unreachable in practice and still not worth aborting the
				// walk of every other entry over.
				return nil //nolint:nilerr // One unnameable entry is skipped, not fatal.
			}
			// The desktop file ID joins nested directories with dashes:
			// applications/foo/bar.desktop is foo-bar.
			id := strings.ToLower(strings.TrimSuffix(
				strings.ReplaceAll(rel, string(os.PathSeparator), "-"), desktopEntrySuffix,
			))
			if _, seen := byID[id]; seen {
				return nil
			}
			// A file that cannot be read, or that carries no [Desktop
			// Entry] group, does not claim the ID. Claiming it would let
			// one truncated file in ~/.local/share/applications shadow a
			// working entry in /usr/share and take the application's icon
			// with it -- a masking rule Hidden=true is supposed to be the
			// only way to invoke.
			groups := parseDesktopFile(path)
			group, ok := groups["Desktop Entry"]
			if !ok {
				return nil
			}
			entry := entryFromGroup(group)
			byID[id] = entry

			// The class index is filled here rather than by ranging over
			// byID afterwards. Two entries may declare the same
			// StartupWMClass -- a vendor shipping a stable and a beta
			// build, say -- and map iteration order is randomized, so that
			// pass handed the class to a different entry on each rebuild
			// and the window's artwork changed with it. Filling it inside
			// the walk keeps directory precedence, and the entry is
			// already the post-masking winner for its ID.
			if entry.hidden || entry.startupWMClass == "" {
				return nil
			}
			class := icon.AppKey(entry.startupWMClass)
			if _, seen := byClass[class]; !seen {
				byClass[class] = entry
			}
			return nil
		})
	}
	return byID, byClass
}

func entryFromGroup(group map[string]string) desktopEntry {
	return desktopEntry{
		icon:           group["Icon"],
		startupWMClass: group["StartupWMClass"],
		hidden:         strings.EqualFold(group["Hidden"], "true"),
	}
}

// buildChain expands the configured theme into every directory the
// search may visit, in order: the theme, its Inherits chain depth-first,
// hicolor, then the unthemed directories.
func (r *iconResolver) buildChain() []themeDir {
	var (
		dirs  []themeDir
		seen  = make(map[string]bool)
		queue = []string{r.configuredTheme()}
		order int
	)
	if queue[0] != hicolorTheme {
		queue = append(queue, hicolorTheme)
	}

	for len(queue) > 0 && order < themeChainLimit {
		name := queue[0]
		queue = queue[1:]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		meta, inherits := r.themeMetadata(name)
		for _, sub := range meta {
			sub.order = order
			dirs = append(dirs, sub)
		}
		order++
		// Inherited themes are visited after the current one, and before
		// anything a later sibling inherits.
		queue = append(inherits, queue...)
	}

	for _, dir := range r.roots.unthemedDirs {
		dirs = append(dirs, themeDir{path: dir, order: order, unthemed: true})
	}
	return dirs
}

// themeMetadata reads one theme's directory list and inheritance.
//
// A theme can span several base directories. Metadata comes from the
// first index.theme in base order, so a stale Directories= list in
// /usr/share cannot override the user's; the directories it names are
// then looked for under every base holding that theme, so a single
// replacement icon dropped into ~/.local/share/icons wins for its
// subdirectory without the user copying the whole theme.
func (r *iconResolver) themeMetadata(name string) (dirs []themeDir, inherits []string) {
	var groups map[string]map[string]string
	for _, base := range r.roots.themeBases {
		candidate := filepath.Join(base, name, "index.theme")
		if parsed := parseDesktopFile(candidate); len(parsed) > 0 {
			groups = parsed
			break
		}
	}
	if groups == nil {
		return nil, nil
	}

	header := groups["Icon Theme"]
	for parent := range strings.SplitSeq(header["Inherits"], ",") {
		if parent = strings.TrimSpace(parent); parent != "" {
			inherits = append(inherits, parent)
		}
	}

	for sub := range strings.SplitSeq(header["Directories"], ",") {
		sub = strings.TrimSpace(sub)
		if sub == "" {
			continue
		}
		group, ok := groups[sub]
		if !ok {
			continue
		}
		meta := directoryMetadata(group)
		for _, base := range r.roots.themeBases {
			meta.path = filepath.Join(base, name, sub)
			dirs = append(dirs, meta)
		}
	}
	return dirs, inherits
}

// directoryMetadata reads the sizing keys of one theme subdirectory.
// Directory names are never parsed for size: nothing requires 48x48 to
// declare Size=48, and 16x16@2x is a 32-pixel icon carrying Scale=2.
func directoryMetadata(group map[string]string) themeDir {
	size := atoiDefault(group["Size"], 0)
	dir := themeDir{
		size:    size,
		scale:   atoiDefault(group["Scale"], 1),
		kind:    strings.ToLower(strings.TrimSpace(group["Type"])),
		minSize: atoiDefault(group["MinSize"], size),
		maxSize: atoiDefault(group["MaxSize"], size),
		// The specification's default Threshold is 2, so a directory that
		// names none still covers a band rather than one exact size.
		threshold: atoiDefault(group["Threshold"], defaultThreshold),
	}
	if dir.kind == "" {
		dir.kind = dirTypeThreshold
	}
	return dir
}

func atoiDefault(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// rankedCandidates lists every path that could hold an icon name, best
// first.
//
// Ranking deviates from the specification deliberately. The spec's
// DirectorySizeDistance is symmetric, so given 48 and 128 for a 64
// request it picks 48. trackkr never displays these files: it resamples
// every one to exactly NormalizedDimension and stores that forever.
// Downscaling 128 keeps detail, upscaling 48 invents it, so the
// smallest candidate at or above the target wins, and only when nothing
// reaches it does the largest below get upscaled.
//
// Theme order outranks size either way: the user chose the theme and did
// not choose the resolution.
func rankedCandidates(name string, chain []themeDir) []string {
	type scored struct {
		path  string
		order int
		// rank is 0 for an exact match, 1 for a usable larger icon, and 2
		// for a fallback smaller one.
		rank int
		size int
	}

	var found []scored
	for _, dir := range chain {
		path := filepath.Join(dir.path, name+".png")
		if !regularFile(path) {
			continue
		}
		if dir.unthemed {
			found = append(found, scored{path: path, order: dir.order, rank: 1, size: icon.NormalizedDimension})
			continue
		}
		effective := dir.effectiveSize(icon.NormalizedDimension)
		switch {
		case directoryMatchesSize(&dir, icon.NormalizedDimension):
			found = append(found, scored{path: path, order: dir.order, rank: 0, size: effective})
		case effective >= icon.NormalizedDimension:
			found = append(found, scored{path: path, order: dir.order, rank: 1, size: effective})
		default:
			found = append(found, scored{path: path, order: dir.order, rank: 2, size: effective})
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.order != b.order {
			return a.order < b.order
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.rank == 2 {
			// Below the target: the largest is the least bad upscale.
			return a.size > b.size
		}
		return a.size < b.size
	})

	paths := make([]string, 0, len(found))
	for _, candidate := range found {
		paths = append(paths, candidate.path)
	}
	return paths
}

// directoryMatchesSize is the specification's eligibility test, applied
// in real pixels.
//
// The specification compares nominal sizes and requires Scale to equal
// the requested scale, which is right for a toolkit picking a file to
// blit. This pipeline wants source material for one resample, and a
// 32x32@2x file is 64 real pixels -- exactly the target. So Scale
// multiplies through every comparison instead of gating them, and all
// three directory types are evaluated over their scaled ranges.
func directoryMatchesSize(dir *themeDir, want int) bool {
	if dir.scale <= 0 {
		return false
	}
	low, high := dir.pixelRange()
	return low <= want && want <= high
}

// pixelRange is the span of real pixel sizes a directory can serve.
func (d *themeDir) pixelRange() (low, high int) {
	switch d.kind {
	case dirTypeScalable:
		return d.minSize * d.scale, d.maxSize * d.scale
	case dirTypeFixed:
		return d.size * d.scale, d.size * d.scale
	default: // Threshold, and anything unrecognised.
		return (d.size - d.threshold) * d.scale, (d.size + d.threshold) * d.scale
	}
}

// effectiveSize is what a directory is worth when ranking near misses:
// the real pixel size it would supply for the requested size.
//
// A scalable or threshold directory covering the target supplies exactly
// the target, so it must not be ranked as though it supplied its nominal
// Size -- that is what demoted a spanning directory below a larger fixed
// one.
func (d *themeDir) effectiveSize(want int) int {
	low, high := d.pixelRange()
	switch {
	case want < low:
		return low
	case want > high:
		return high
	default:
		return want
	}
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// parseDesktopFile reads the INI dialect shared by .desktop files and
// index.theme: groups in brackets, key=value beneath, first value
// winning, and iconstring escapes decoded.
func parseDesktopFile(path string) map[string]map[string]string {
	file, err := os.Open(path) // nosemgrep // gitlab-advanced-sast-exclude -- path is composed from indexed icon roots.
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	groups := make(map[string]map[string]string)
	current := ""
	scanner := bufio.NewScanner(io.LimitReader(file, maxDesktopFileBytes))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := groups[current]; !ok {
				groups[current] = make(map[string]string)
			}
			continue
		}
		if current == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// A localised key such as Icon[de] is not the plain one and must
		// not overwrite it.
		key = strings.TrimSpace(key)
		if strings.ContainsRune(key, '[') {
			continue
		}
		if _, seen := groups[current][key]; !seen {
			groups[current][key] = unescapeValue(strings.TrimSpace(value))
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	return groups
}

// unescapeValue decodes the escapes the desktop entry specification
// defines for string values. Without it an Icon= naming a path with a
// space resolves to nothing.
func unescapeValue(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			out.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 's':
			out.WriteByte(' ')
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case '\\':
			out.WriteByte('\\')
		default:
			out.WriteByte('\\')
			out.WriteByte(value[i])
		}
	}
	return out.String()
}

// configuredTheme returns the icon theme to search first.
//
// Reading gtk-3.0/settings.ini alone is not enough, and quietly so: GTK
// takes the desktop-wide setting from the Wayland settings portal or
// XSettings and treats settings.ini as a fallback many systems never
// write. On the machine this was developed against, both user
// settings.ini files exist without the key while the real answer sits in
// /etc/gtk-3.0/settings.ini and in gsettings -- so a settings.ini-only
// reader selects hicolor and loses every themed icon.
func (r *iconResolver) configuredTheme() string {
	if name := strings.TrimSpace(r.themeName); name != "" {
		return name
	}
	if r.lookupTheme != nil {
		if name := strings.TrimSpace(r.lookupTheme(r.roots)); name != "" {
			return name
		}
	}
	if name := gtkSettingsTheme(r.roots); name != "" {
		return name
	}
	return hicolorTheme
}

// gsettingsTheme asks the desktop for its icon theme, when gsettings is
// installed. It is optional: a session without it falls through to the
// settings files.
func gsettingsTheme(*iconRoots) string {
	path, err := exec.LookPath("gsettings")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), gsettingsTimeout)
	defer cancel()

	// No //nolint:gosec here, unlike the X11 detector's exec calls: every
	// argument after the LookPath-validated binary is a constant, so gosec
	// does not flag it and nolintlint rejects the dead directive. The
	// nosemgrep marker is still needed for the SAST job.
	out, err := exec.CommandContext(ctx, path, // nosemgrep // gitlab-advanced-sast-exclude -- path validated by LookPath, arguments are constant.
		"get", "org.gnome.desktop.interface", "icon-theme").Output()
	if err != nil {
		return ""
	}
	// gsettings quotes its answer: 'Yaru'.
	return strings.Trim(strings.TrimSpace(string(out)), "'\"")
}

// gtkSettingsTheme reads gtk-icon-theme-name from the GTK settings
// files, newest toolkit version first.
//
// /etc/gtk-4.0 and /etc/gtk-3.0 are GTK's own system paths rather than
// XDG ones, and are where a distribution's default usually lands, so
// they are searched after the XDG directories rather than not at all.
func gtkSettingsTheme(roots *iconRoots) string {
	for _, dir := range roots.configDirs {
		for _, version := range []string{"gtk-4.0", "gtk-3.0"} {
			groups := parseDesktopFile(filepath.Join(dir, version, "settings.ini"))
			if name := strings.TrimSpace(groups["Settings"]["gtk-icon-theme-name"]); name != "" {
				return name
			}
		}
	}
	// ~/.gtkrc-2.0 is not INI: it is a flat list of assignments.
	if roots.homeDir != "" {
		if name := gtkrcTheme(filepath.Join(roots.homeDir, ".gtkrc-2.0")); name != "" {
			return name
		}
	}
	return ""
}

func gtkrcTheme(path string) string {
	file, err := os.Open(path) // nosemgrep // gitlab-advanced-sast-exclude -- path is derived from the resolver's own roots.
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(io.LimitReader(file, maxDesktopFileBytes))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || strings.TrimSpace(key) != "gtk-icon-theme-name" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

// xdgIconRoots derives the search roots from the environment, applying
// the base directory specification's defaults.
//
// The defaults matter more than they look. XDG_DATA_HOME is unset on
// most systems, and joining an empty string yields a relative path, so a
// daemon started from any working directory would search that directory
// instead of the user's. The specification also requires these variables
// to hold absolute paths and a relative one to be ignored, which is
// applied per element so one bad entry does not discard the list.
func xdgIconRoots() *iconRoots {
	home := os.Getenv("HOME")
	dataHome := absoluteOr(os.Getenv("XDG_DATA_HOME"), joinHome(home, ".local", "share"))
	configHome := absoluteOr(os.Getenv("XDG_CONFIG_HOME"), joinHome(home, ".config"))
	dataDirs := absoluteList(os.Getenv("XDG_DATA_DIRS"), "/usr/local/share", "/usr/share")
	configDirs := absoluteList(os.Getenv("XDG_CONFIG_DIRS"), "/etc/xdg")

	roots := &iconRoots{unthemedDirs: []string{"/usr/share/pixmaps"}}
	if filepath.IsAbs(home) {
		roots.homeDir = home
	}
	if dataHome != "" {
		roots.applicationDirs = append(roots.applicationDirs, filepath.Join(dataHome, "applications"))
		roots.themeBases = append(roots.themeBases, filepath.Join(dataHome, "icons"))
	}
	if home != "" && filepath.IsAbs(home) {
		// ~/.icons predates XDG and is still populated by some installers.
		roots.themeBases = append(roots.themeBases, filepath.Join(home, ".icons"))
	}
	for _, dir := range dataDirs {
		roots.applicationDirs = append(roots.applicationDirs, filepath.Join(dir, "applications"))
		roots.themeBases = append(roots.themeBases, filepath.Join(dir, "icons"))
	}
	if configHome != "" {
		roots.configDirs = append(roots.configDirs, configHome)
	}
	roots.configDirs = append(roots.configDirs, configDirs...)
	// /etc is GTK's own system location rather than an XDG one, and is
	// where a distribution's default usually lands -- on the machine this
	// was developed against it is the only file carrying the answer. It
	// belongs in the roots rather than hardcoded in the reader, so a test
	// controls every directory the resolver touches.
	roots.configDirs = append(roots.configDirs, "/etc")
	return roots
}

func joinHome(home string, parts ...string) string {
	if home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func absoluteOr(value, fallback string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return fallback
}

// absoluteList splits a colon-separated variable, keeping only absolute
// entries and falling back to the specification's default when nothing
// usable remains.
func absoluteList(value string, fallback ...string) []string {
	var dirs []string
	for dir := range strings.SplitSeq(value, string(os.PathListSeparator)) {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.Clean(dir))
		}
	}
	if len(dirs) == 0 {
		return fallback
	}
	return dirs
}
