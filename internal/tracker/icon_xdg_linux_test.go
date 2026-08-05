//go:build linux

package tracker

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	demoKey       = "demo"
	settingsTheme = "FromSettings"
	// Fragments repeated across the hand-written index.theme fixtures.
	themeNameOnly = "Name=Only"
	typeFixed     = "Type=Fixed"
	themeHeader   = "[Icon Theme]"
)

// iconTree builds a fake XDG layout under t.TempDir. Every test drives
// the resolver through one of these rather than the real machine, so
// what passes here does not depend on what happens to be installed.
type iconTree struct {
	t    *testing.T
	root string
}

func newIconTree(t *testing.T) *iconTree {
	t.Helper()
	return &iconTree{t: t, root: t.TempDir()}
}

func (t *iconTree) write(rel, content string) string {
	t.t.Helper()
	path := filepath.Join(t.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.t.Fatal(err)
	}
	return path
}

// writePNG writes a square PNG of the given size, tinted so two
// candidates can be told apart by decoding the result.
func (t *iconTree) writePNG(rel string, size int, shade uint8) string {
	t.t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, color.NRGBA{R: shade, G: shade, B: shade, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.t.Fatal(err)
	}
	return t.write(rel, buf.String())
}

func (t *iconTree) path(rel string) string { return filepath.Join(t.root, rel) }

// theme writes an index.theme declaring one fixed-size apps directory
// per size, plus an optional Inherits line.
func (t *iconTree) theme(name, inherits string, sizes ...int) {
	t.t.Helper()
	dirs := make([]string, 0, len(sizes))
	groups := make([]string, 0, len(sizes))
	for _, size := range sizes {
		sub := sizeDirName(size)
		dirs = append(dirs, sub)
		groups = append(groups, "["+sub+"]\nContext=Applications\nSize="+itoa(size)+"\nType=Fixed\n")
	}
	body := "[Icon Theme]\nName=" + name + "\n"
	if inherits != "" {
		body += "Inherits=" + inherits + "\n"
	}
	body += "Directories=" + strings.Join(dirs, ",") + "\n\n" + strings.Join(groups, "\n")
	// Themes are declared in the system base; a test proving that one
	// theme spans bases writes its extra files directly.
	t.write(filepath.Join("usr", "icons", name, "index.theme"), body)
}

func sizeDirName(size int) string { return itoa(size) + "x" + itoa(size) + "/apps" }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// roots builds an iconRoots over the fake tree, user directory first.
func (t *iconTree) roots(bases ...string) *iconRoots {
	t.t.Helper()
	roots := &iconRoots{}
	for _, base := range bases {
		roots.applicationDirs = append(roots.applicationDirs, t.path(filepath.Join(base, "applications")))
		roots.themeBases = append(roots.themeBases, t.path(filepath.Join(base, "icons")))
		roots.configDirs = append(roots.configDirs, t.path(filepath.Join(base, "config")))
	}
	return roots
}

// resolver returns a resolver that never shells out and whose clock the
// test controls.
func (t *iconTree) resolver(roots *iconRoots, theme string) *iconResolver {
	t.t.Helper()
	r := newIconResolver(roots, theme)
	r.lookupTheme = func(*iconRoots) string { return "" }
	return r
}

func decodeShade(t *testing.T, app *icon.App) uint8 {
	t.Helper()
	if app == nil {
		t.Fatal("no icon resolved")
	}
	img, err := png.Decode(bytes.NewReader(app.PNG))
	if err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	r, _, _, _ := img.At(icon.NormalizedDimension/2, icon.NormalizedDimension/2).RGBA()
	return uint8(r >> 8) //nolint:gosec // The shade was written as an 8-bit value.
}

func TestResolverPrefersTheSmallestIconAtOrAbove64(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nName=Demo\nIcon=demo\n")
	tree.theme("Only", "", 48, 64, 128)
	tree.writePNG("usr/icons/Only/48x48/apps/demo.png", 48, 10)
	tree.writePNG("usr/icons/Only/64x64/apps/demo.png", 64, 20)
	tree.writePNG("usr/icons/Only/128x128/apps/demo.png", 128, 30)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 20 {
		t.Errorf("shade = %d, want 20 (the 64x64 icon)", shade)
	}
}

// The specification's DirectorySizeDistance would choose 48 here. This
// pipeline resamples to 64 and stores the result, so downscaling 128
// beats upscaling 48. Asserted so the deviation cannot be reverted
// quietly.
func TestResolverDownscalesRatherThanUpscales(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme("Only", "", 48, 128)
	tree.writePNG("usr/icons/Only/48x48/apps/demo.png", 48, 10)
	tree.writePNG("usr/icons/Only/128x128/apps/demo.png", 128, 30)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 30 {
		t.Errorf("shade = %d, want 30 (the 128x128 icon)", shade)
	}
}

func TestResolverUpscalesWhenNothingReaches64(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme("Only", "", 16, 48)
	tree.writePNG("usr/icons/Only/16x16/apps/demo.png", 16, 10)
	tree.writePNG("usr/icons/Only/48x48/apps/demo.png", 48, 40)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 40 {
		t.Errorf("shade = %d, want 40 (the largest below the target)", shade)
	}
}

// The user chose the theme and did not choose the resolution, so a
// smaller icon from it outranks a larger one from hicolor.
func TestResolverPrefersTheConfiguredThemeOverHicolor(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme("Chosen", hicolorTheme, 48)
	tree.writePNG("usr/icons/Chosen/48x48/apps/demo.png", 48, 11)
	tree.theme(hicolorTheme, "", 64)
	tree.writePNG("usr/icons/hicolor/64x64/apps/demo.png", 64, 22)

	got := tree.resolver(tree.roots("usr"), "Chosen").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 11 {
		t.Errorf("shade = %d, want 11 (the configured theme)", shade)
	}
}

func TestResolverFollowsInheritsAndTerminatesOnCycles(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	// A declares B, B declares A: a cycle that must not hang.
	tree.theme("A", "B", 64)
	tree.theme("B", "A", 64)
	tree.writePNG("usr/icons/B/64x64/apps/demo.png", 64, 55)

	done := make(chan uint8, 1)
	go func() {
		done <- decodeShade(t, tree.resolver(tree.roots("usr"), "A").resolve(t.Context(), "demo"))
	}()
	select {
	case shade := <-done:
		if shade != 55 {
			t.Errorf("shade = %d, want 55 (inherited theme)", shade)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolver did not terminate on an Inherits cycle")
	}
}

// A theme is a name, not a directory: metadata comes from the first
// index.theme in base order while files are searched under every base.
func TestResolverSpansBaseDirectoriesForOneTheme(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	// Only the system copy declares the directories.
	tree.theme("Shared", "", 64)
	tree.writePNG("usr/icons/Shared/64x64/apps/demo.png", 64, 60)
	// The user drops one replacement in without copying the theme.
	tree.writePNG("home/icons/Shared/64x64/apps/demo.png", 64, 70)

	got := tree.resolver(tree.roots("home", "usr"), "Shared").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 70 {
		t.Errorf("shade = %d, want 70 (the user override)", shade)
	}
}

func TestResolverEntryLookup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		setup func(*iconTree)
		want  uint8
	}{
		{
			name: "case insensitive id",
			key:  "slack",
			setup: func(tree *iconTree) {
				tree.write("usr/applications/Slack.desktop", "[Desktop Entry]\nIcon=demo\n")
			},
			want: 64,
		},
		{
			name: "nested entry uses a dashed id",
			key:  "screensavers-swirl",
			setup: func(tree *iconTree) {
				tree.write("usr/applications/screensavers/swirl.desktop", "[Desktop Entry]\nIcon=demo\n")
			},
			want: 64,
		},
		{
			name: "startupwmclass reaches an unmatched id",
			key:  "jetbrains-pycharm",
			setup: func(tree *iconTree) {
				tree.write("usr/applications/jetbrains-pycharm-9a62.desktop",
					"[Desktop Entry]\nIcon=demo\nStartupWMClass=jetbrains-pycharm\n")
			},
			want: 64,
		},
		{
			name: "nodisplay keeps its icon",
			key:  demoKey,
			setup: func(tree *iconTree) {
				tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\nNoDisplay=true\n")
			},
			want: 64,
		},
		{
			name:  "no entry falls back to the key as an icon name",
			key:   demoKey,
			setup: func(*iconTree) {},
			want:  64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tree := newIconTree(t)
			tt.setup(tree)
			tree.theme("Only", "", 64)
			tree.writePNG("usr/icons/Only/64x64/apps/demo.png", 64, tt.want)

			got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), tt.key)
			if shade := decodeShade(t, got); shade != tt.want {
				t.Errorf("shade = %d, want %d", shade, tt.want)
			}
		})
	}
}

func TestResolverHiddenMasksLowerPrecedenceEntries(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	// The user deletes the entry; the system copy must not answer, not
	// by ID and not through the class mapping it alone declares.
	tree.write("home/applications/demo.desktop", "[Desktop Entry]\nHidden=true\n")
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\nStartupWMClass=DemoClass\n")
	tree.theme("Only", "", 64)
	tree.writePNG("usr/icons/Only/64x64/apps/demo.png", 64, 90)

	resolver := tree.resolver(tree.roots("home", "usr"), "Only")
	if got := resolver.resolve(t.Context(), "demo"); got != nil {
		t.Error("a Hidden entry still resolved by id")
	}
	if got := resolver.resolve(t.Context(), "democlass"); got != nil {
		t.Error("a Hidden entry resurfaced through its StartupWMClass")
	}
}

func TestResolverAbsoluteIconPaths(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	absolute := tree.writePNG("opt/vendor/logo.png", 64, 77)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon="+absolute+"\n")
	tree.write("usr/applications/missing.desktop", "[Desktop Entry]\nIcon="+tree.path("opt/gone.png")+"\n")

	resolver := tree.resolver(tree.roots("usr"), hicolorTheme)
	if shade := decodeShade(t, resolver.resolve(t.Context(), "demo")); shade != 77 {
		t.Errorf("shade = %d, want 77", shade)
	}
	if got := resolver.resolve(t.Context(), "missing"); got != nil {
		t.Error("an absolute path to a missing file resolved")
	}
}

// Icon= naming a path with a space only works if the iconstring escapes
// are decoded.
func TestResolverUnescapesIconStrings(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	absolute := tree.writePNG("opt/My App/logo.png", 64, 33)
	escaped := strings.ReplaceAll(absolute, " ", `\s`)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon="+escaped+"\n")

	got := tree.resolver(tree.roots("usr"), hicolorTheme).resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 33 {
		t.Errorf("shade = %d, want 33", shade)
	}
}

func TestParseDesktopFileGroupsAndPrecedence(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	path := tree.write("entry.desktop", strings.Join([]string{
		"Icon=stray",
		"[Desktop Entry]",
		"# a comment",
		"Icon=first",
		"Icon=second",
		"Icon[de]=localised",
		"[Desktop Action open]",
		"Icon=action",
	}, "\n"))

	groups := parseDesktopFile(path)
	if got := groups["Desktop Entry"]["Icon"]; got != "first" {
		t.Errorf("Icon = %q, want %q: the first value in the group wins", got, "first")
	}
	if got := groups["Desktop Action open"]["Icon"]; got != "action" {
		t.Errorf("action group Icon = %q", got)
	}
}

func TestResolverFallsThroughUnusableCandidates(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme("Chosen", hicolorTheme, 64, 128)
	// Corrupt at the best size, valid at the next size in the same theme,
	// valid again in hicolor. The same-theme candidate must win.
	tree.write("usr/icons/Chosen/64x64/apps/demo.png", "not a png at all")
	tree.writePNG("usr/icons/Chosen/128x128/apps/demo.png", 128, 12)
	tree.theme(hicolorTheme, "", 64)
	tree.writePNG("usr/icons/hicolor/64x64/apps/demo.png", 64, 99)

	got := tree.resolver(tree.roots("usr"), "Chosen").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 12 {
		t.Errorf("shade = %d, want 12: fallback left the configured theme too early", shade)
	}
}

// Unreadability is simulated structurally rather than with permissions:
// a suite running as root reads a 0o000 file happily, and containers run
// as root often enough that such a test would pass without exercising
// anything.
func TestResolverSkipsUnreadableCandidates(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme("Chosen", hicolorTheme, 64)
	// A directory where a file should be.
	if err := os.MkdirAll(tree.path("usr/icons/Chosen/64x64/apps/demo.png"), 0o750); err != nil {
		t.Fatal(err)
	}
	tree.theme(hicolorTheme, "", 64)
	tree.writePNG("usr/icons/hicolor/64x64/apps/demo.png", 64, 44)

	if shade := decodeShade(t, tree.resolver(tree.roots("usr"), "Chosen").resolve(t.Context(), "demo")); shade != 44 {
		t.Errorf("shade = %d, want 44", shade)
	}

	dangling := newIconTree(t)
	dangling.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	dangling.theme("Chosen", hicolorTheme, 64)
	link := dangling.path("usr/icons/Chosen/64x64/apps/demo.png")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dangling.path("nowhere.png"), link); err != nil {
		t.Fatal(err)
	}
	dangling.theme(hicolorTheme, "", 64)
	dangling.writePNG("usr/icons/hicolor/64x64/apps/demo.png", 64, 45)

	if shade := decodeShade(t, dangling.resolver(dangling.roots("usr"), "Chosen").resolve(t.Context(), "demo")); shade != 45 {
		t.Errorf("dangling symlink: shade = %d, want 45", shade)
	}
}

func TestResolverReturnsNilWhenEveryCandidateFails(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=demo\n")
	tree.theme(hicolorTheme, "", 64)
	tree.write("usr/icons/hicolor/64x64/apps/demo.png", "still not a png")

	if got := tree.resolver(tree.roots("usr"), hicolorTheme).resolve(t.Context(), "demo"); got != nil {
		t.Error("a corrupt-only application resolved to something")
	}
}

func TestResolverBoundsSourceReadsAtTheLimit(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.theme(hicolorTheme, "", 64)
	dir := "usr/icons/hicolor/64x64/apps/"

	// A valid PNG padded past the limit with a trailing comment chunk is
	// still a decodable image, so only the byte bound rejects it.
	oversized := append(pngBytes(t, 64, 8), make([]byte, icon.MaxSourceBytes)...)
	tree.write(dir+"big.png", string(oversized))
	if got := tree.resolver(tree.roots("usr"), hicolorTheme).resolve(t.Context(), "big"); got != nil {
		t.Error("a source past MaxSourceBytes resolved")
	}

	tree.write(dir+"small.png", string(pngBytes(t, 64, 8)))
	if got := tree.resolver(tree.roots("usr"), hicolorTheme).resolve(t.Context(), "small"); got == nil {
		t.Error("a source within MaxSourceBytes did not resolve")
	}
}

func pngBytes(t *testing.T, size int, shade uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, color.NRGBA{R: shade, G: shade, B: shade, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestResolverRebuildsIndexAfterLifetime(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.theme(hicolorTheme, "", 64)
	// The artwork exists from the start under a name the key does not
	// match, so only the desktop entry can connect the two. That isolates
	// the cached walk: candidate files are stat-ed on every lookup, and a
	// test keyed on a new file would prove nothing about the index.
	tree.writePNG("usr/icons/hicolor/64x64/apps/vendor-logo.png", 64, 88)
	resolver := tree.resolver(tree.roots("usr"), hicolorTheme)

	now := time.Now()
	resolver.now = func() time.Time { return now }

	if got := resolver.resolve(t.Context(), "late"); got != nil {
		t.Fatal("resolved before the entry existed")
	}

	// Installed after the first walk: a miss must not rebuild.
	tree.write("usr/applications/late.desktop", "[Desktop Entry]\nIcon=vendor-logo\n")
	if got := resolver.resolve(t.Context(), "late"); got != nil {
		t.Error("a lookup miss rebuilt the index")
	}

	now = now.Add(resolver.lifetime + time.Second)
	if got := resolver.resolve(t.Context(), "late"); got == nil {
		t.Error("the index never rebuilt after its lifetime")
	}
}

func TestConfiguredThemePriority(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	roots := tree.roots("home", "usr")

	// The file the naive reader would have used carries nothing, and the
	// answer sits where this machine actually keeps it.
	tree.write("home/config/gtk-3.0/settings.ini", "[Settings]\ngtk-font-name=Sans 10\n")
	tree.write("usr/config/gtk-3.0/settings.ini", "[Settings]\ngtk-icon-theme-name=FromFile\n")

	resolver := tree.resolver(roots, "")
	if got := resolver.configuredTheme(); got != "FromFile" {
		t.Errorf("theme = %q, want FromFile: the file search missed the lower-precedence directory", got)
	}

	resolver.lookupTheme = func(*iconRoots) string { return settingsTheme }
	if got := resolver.configuredTheme(); got != settingsTheme {
		t.Errorf("theme = %q, want %q: the desktop setting outranks files", got, settingsTheme)
	}

	override := tree.resolver(roots, "FromConfig")
	override.lookupTheme = func(*iconRoots) string { return settingsTheme }
	if got := override.configuredTheme(); got != "FromConfig" {
		t.Errorf("theme = %q, want FromConfig: the explicit override outranks everything", got)
	}

	bare := newIconTree(t)
	empty := bare.resolver(bare.roots("usr"), "")
	if got := empty.configuredTheme(); got != hicolorTheme {
		t.Errorf("theme = %q, want %q as the terminal fallback", got, hicolorTheme)
	}
}

func TestDirectoryMatchesSizeByType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  themeDir
		want bool
	}{
		{"fixed exact", themeDir{kind: dirTypeFixed, size: 64, scale: 1}, true},
		{"fixed near", themeDir{kind: dirTypeFixed, size: 48, scale: 1}, false},
		{"scalable spanning", themeDir{kind: dirTypeScalable, minSize: 8, maxSize: 512, scale: 1}, true},
		{"scalable below", themeDir{kind: dirTypeScalable, minSize: 8, maxSize: 32, scale: 1}, false},
		{"threshold at size", themeDir{kind: dirTypeThreshold, size: 64, scale: 1, threshold: defaultThreshold}, true},
		{"threshold inside its band", themeDir{kind: dirTypeThreshold, size: 48, scale: 1, threshold: 16}, true},
		{"threshold outside its band", themeDir{kind: dirTypeThreshold, size: 48, scale: 1, threshold: 8}, false},
		// A 32x32@2x file is 64 real pixels, which is exactly the source
		// this pipeline wants; the nominal size alone would reject it.
		{"scaled fixed reaching the target", themeDir{kind: dirTypeFixed, size: 32, scale: 2}, true},
		{"scaled fixed overshooting", themeDir{kind: dirTypeFixed, size: 64, scale: 2}, false},
		{"scaled scalable spanning", themeDir{kind: dirTypeScalable, minSize: 8, maxSize: 64, scale: 2}, true},
		{"zero scale", themeDir{kind: dirTypeFixed, size: 64, scale: 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := directoryMatchesSize(&tt.dir, icon.NormalizedDimension); got != tt.want {
				t.Errorf("directoryMatchesSize = %v, want %v", got, tt.want)
			}
		})
	}
}

// A 16x16@2x directory holds 32-pixel icons. Reading the group's Scale
// rather than the directory name is what keeps that straight.
func TestResolverUsesDeclaredScaleNotDirectoryNames(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/icons/Only/index.theme", strings.Join([]string{
		themeHeader, themeNameOnly, "Directories=huge/apps,small/apps", "",
		"[huge/apps]", "Size=32", "Scale=2", typeFixed, "",
		"[small/apps]", "Size=16", "Scale=1", typeFixed,
	}, "\n"))
	tree.writePNG("usr/icons/Only/huge/apps/demo.png", 64, 21)
	tree.writePNG("usr/icons/Only/small/apps/demo.png", 16, 31)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), "demo")
	if shade := decodeShade(t, got); shade != 21 {
		t.Errorf("shade = %d, want 21: Size*Scale=64 should have won", shade)
	}
}

// Both detectors are asserted, not just sway. Testing one would let a
// missed assignment in the other ship green, which is the exact shape of
// the bug this work exists to fix.
func TestLinuxDetectorsAttachResolvedIcons(t *testing.T) {
	t.Parallel()

	stub := func(shade uint8) *appIconCache {
		return newAppIconCache(time.Now, func(context.Context, appInfo) *icon.App {
			return &icon.App{Key: "demo", PNG: pngBytes(t, 64, shade)}
		})
	}

	// The cache resolves off the polling path, so the first call primes it
	// and returns nil; the icon lands on a later poll. That is the macOS
	// behaviour too, and it is why a window is never delayed by a
	// filesystem walk.
	settle := func(t *testing.T, next func() *icon.App) *icon.App {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if got := next(); got != nil {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}

	t.Run("sway", func(t *testing.T) {
		t.Parallel()
		detector := &SwayWindowDetector{icons: stub(50)}
		t.Cleanup(detector.Close)

		if got := settle(t, func() *icon.App { return detector.appIcon(t.Context(), "demo") }); got == nil {
			t.Fatal("sway detector attached no icon")
		}
		bare := &SwayWindowDetector{}
		if got := bare.appIcon(t.Context(), "demo"); got != nil {
			t.Error("sway detector invented an icon without a cache")
		}
	})

	t.Run("x11", func(t *testing.T) {
		t.Parallel()
		detector := &XWindowDetector{icons: stub(60)}
		t.Cleanup(detector.Close)

		if got := settle(t, func() *icon.App { return detector.appIcon(t.Context(), "demo") }); got == nil {
			t.Fatal("x11 detector attached no icon")
		}
		bare := &XWindowDetector{}
		if got := bare.appIcon(t.Context(), "demo"); got != nil {
			t.Error("x11 detector invented an icon without a cache")
		}
	})
}

// The cache starts a worker in its constructor, so a detector that
// forgets to close it leaks one per session for the life of the process.
func TestLinuxDetectorCloseStopsTheIconWorkerIdempotently(t *testing.T) {
	t.Parallel()

	before := runtime.NumGoroutine()

	sway := &SwayWindowDetector{icons: newAppIconCache(time.Now, nil)}
	x11 := &XWindowDetector{icons: newAppIconCache(time.Now, nil)}
	sway.Close()
	sway.Close()
	x11.Close()
	x11.Close()

	// Closing is asynchronous only in that the worker observes a closed
	// channel; give it a moment rather than asserting on a race.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines = %d, want at most %d: an icon worker outlived Close", runtime.NumGoroutine(), before)
}

// These use t.Setenv and so cannot be parallel: it panics in a parallel
// test. Everything else in this file drives injected roots instead,
// which is what keeps the rest parallel.
func TestXDGIconRootsAppliesSpecificationDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_DATA_DIRS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_DIRS", "")

	roots := xdgIconRoots()

	if want := filepath.Join(home, ".local", "share", "applications"); roots.applicationDirs[0] != want {
		t.Errorf("first application dir = %q, want %q", roots.applicationDirs[0], want)
	}
	for _, want := range []string{"/usr/local/share/applications", "/usr/share/applications"} {
		if !slices.Contains(roots.applicationDirs, want) {
			t.Errorf("application dirs %v missing the default %q", roots.applicationDirs, want)
		}
	}
	if want := filepath.Join(home, ".config"); !slices.Contains(roots.configDirs, want) {
		t.Errorf("config dirs %v missing %q", roots.configDirs, want)
	}
	if !slices.Contains(roots.configDirs, "/etc/xdg") || !slices.Contains(roots.configDirs, "/etc") {
		t.Errorf("config dirs %v missing the system defaults", roots.configDirs)
	}
}

// A relative value joined naively would make the daemon search whatever
// directory it happened to be started from.
func TestXDGIconRootsIgnoresRelativePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "relative/data")
	t.Setenv("XDG_CONFIG_HOME", "also/relative")
	t.Setenv("XDG_DATA_DIRS", "not/absolute:/opt/share")
	t.Setenv("XDG_CONFIG_DIRS", "")

	roots := xdgIconRoots()

	for _, dir := range slices.Concat(roots.applicationDirs, roots.themeBases, roots.configDirs) {
		if !filepath.IsAbs(dir) {
			t.Errorf("root %q is relative", dir)
		}
		if strings.HasPrefix(dir, "relative") || strings.HasPrefix(dir, "also") || strings.HasPrefix(dir, "not") {
			t.Errorf("root %q was built from a relative variable", dir)
		}
	}
	if want := filepath.Join(home, ".local", "share", "applications"); roots.applicationDirs[0] != want {
		t.Errorf("first application dir = %q, want the default %q", roots.applicationDirs[0], want)
	}
	// One bad element must not discard the absolute ones beside it.
	if !slices.Contains(roots.applicationDirs, "/opt/share/applications") {
		t.Errorf("application dirs %v dropped the absolute element", roots.applicationDirs)
	}
}

func TestXDGIconRootsWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_DIRS", "")
	t.Setenv("XDG_CONFIG_DIRS", "")

	roots := xdgIconRoots()

	for _, dir := range slices.Concat(roots.applicationDirs, roots.themeBases, roots.configDirs) {
		if !filepath.IsAbs(dir) {
			t.Fatalf("root %q is relative with HOME unset", dir)
		}
	}
	if slices.Contains(roots.configDirs, "/.config") {
		t.Error("HOME unset produced a root-level .config")
	}
}

// A scalable or threshold directory covering the target supplies the
// target, so it must not be ranked as though it supplied its nominal
// Size. Ranking on Size alone demoted a spanning directory below a
// larger fixed one.
func TestResolverRanksSpanningDirectoriesAtTheTarget(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/icons/Only/index.theme", strings.Join([]string{
		themeHeader, themeNameOnly, "Directories=span/apps,big/apps", "",
		// Covers 64 through its range while naming a small nominal size.
		"[span/apps]", "Size=16", "MinSize=8", "MaxSize=512", "Type=Scalable", "",
		"[big/apps]", "Size=128", typeFixed,
	}, "\n"))
	tree.writePNG("usr/icons/Only/span/apps/demo.png", 64, 15)
	tree.writePNG("usr/icons/Only/big/apps/demo.png", 128, 25)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), demoKey)
	if shade := decodeShade(t, got); shade != 15 {
		t.Errorf("shade = %d, want 15: the spanning directory serves 64 exactly", shade)
	}
}

// A Threshold directory covers a band around its Size. Treating it as
// exact-size-only left Size=48 Threshold=16 looking undersized.
func TestResolverAcceptsThresholdBands(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("usr/icons/Only/index.theme", strings.Join([]string{
		themeHeader, themeNameOnly, "Directories=band/apps,big/apps", "",
		"[band/apps]", "Size=48", "Threshold=16", "Type=Threshold", "",
		"[big/apps]", "Size=256", typeFixed,
	}, "\n"))
	tree.writePNG("usr/icons/Only/band/apps/demo.png", 48, 17)
	tree.writePNG("usr/icons/Only/big/apps/demo.png", 256, 27)

	got := tree.resolver(tree.roots("usr"), "Only").resolve(t.Context(), demoKey)
	if shade := decodeShade(t, got); shade != 17 {
		t.Errorf("shade = %d, want 17: Size=48 Threshold=16 covers 64", shade)
	}
}

// ~/.gtkrc-2.0 is anchored to HOME, not to XDG_CONFIG_HOME. Deriving it
// from a config directory silently missed it whenever the two differ.
func TestGtkrcIsFoundWithACustomConfigHome(t *testing.T) {
	home := t.TempDir()
	config := t.TempDir() // Deliberately not under HOME.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_CONFIG_DIRS", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_DATA_DIRS", t.TempDir())

	if err := os.WriteFile(filepath.Join(home, ".gtkrc-2.0"),
		[]byte(`gtk-icon-theme-name="FromGtkrc"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The environment-derived roots must carry HOME itself, which is the
	// fix: ~/.gtkrc-2.0 cannot be reached from XDG_CONFIG_HOME when the
	// two point at different places.
	if got := xdgIconRoots().homeDir; got != home {
		t.Fatalf("homeDir = %q, want %q", got, home)
	}

	// Search it in isolation. The real /etc/gtk-3.0/settings.ini is a
	// legitimate higher-precedence root on this machine, so leaving it in
	// would test the developer's desktop rather than this code path.
	roots := &iconRoots{configDirs: []string{config}, homeDir: home}
	resolver := newIconResolver(roots, "")
	resolver.lookupTheme = func(*iconRoots) string { return "" }
	if got := resolver.configuredTheme(); got != "FromGtkrc" {
		t.Errorf("theme = %q, want FromGtkrc: ~/.gtkrc-2.0 was not searched", got)
	}

	// And a settings.ini still outranks it.
	if err := os.MkdirAll(filepath.Join(config, "gtk-3.0"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "gtk-3.0", "settings.ini"),
		[]byte("[Settings]\ngtk-icon-theme-name=FromIni\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolver.configuredTheme(); got != "FromIni" {
		t.Errorf("theme = %q, want FromIni: settings.ini outranks gtkrc", got)
	}
}

// A desktop file that cannot be parsed must not claim its ID. Claiming
// it would make one truncated file in a high-precedence directory mask a
// working entry below it, which is a deletion, and Hidden=true is meant
// to be the only way to ask for that.
func TestResolverUnparseableEntriesDoNotShadow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{"no desktop entry group", "[Desktop Action open]\nIcon=wrong\n"},
		{"truncated before any group", "Icon=wrong\n"},
		{"empty file", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tree := newIconTree(t)
			tree.write("home/applications/demo.desktop", tt.content)
			// The icon is named differently from the key on purpose. An
			// entry that resolves through its own name would be rescued by
			// the key-as-icon-name fallback, and the test would pass with
			// the shadowing bug still in place.
			tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=vendor-logo\n")
			tree.theme(hicolorTheme, "", 64)
			tree.writePNG("usr/icons/hicolor/64x64/apps/vendor-logo.png", 64, 81)

			got := tree.resolver(tree.roots("home", "usr"), hicolorTheme).resolve(t.Context(), demoKey)
			if shade := decodeShade(t, got); shade != 81 {
				t.Errorf("shade = %d, want 81: the system entry was shadowed", shade)
			}
		})
	}
}

// An entry that parses but declares no icon still claims its ID: it is a
// real installed application, and the lookup should stop there rather
// than borrow a lower-precedence entry's artwork.
func TestResolverParsedEntryWithoutIconStillClaimsItsID(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("home/applications/demo.desktop", "[Desktop Entry]\nName=Demo\n")
	tree.write("usr/applications/demo.desktop", "[Desktop Entry]\nIcon=other\n")
	tree.theme(hicolorTheme, "", 64)
	tree.writePNG("usr/icons/hicolor/64x64/apps/other.png", 64, 82)
	// The key itself is tried when an entry names no icon, so give that
	// nothing to find.

	if got := tree.resolver(tree.roots("home", "usr"), hicolorTheme).resolve(t.Context(), demoKey); got != nil {
		t.Error("a higher-precedence entry without an icon borrowed the lower one's artwork")
	}
}

// Two entries may legitimately declare the same StartupWMClass -- a
// vendor shipping a stable and a beta build, say. Directory precedence
// has to decide, not map iteration order, or the window's artwork
// changes on every index rebuild.
func TestResolverStartupWMClassCollisionsFollowPrecedence(t *testing.T) {
	t.Parallel()

	// Repeated because the failure mode is randomized: one run of a map
	// range would pick the right entry often enough to look green.
	for range 20 {
		tree := newIconTree(t)
		tree.write("home/applications/vendor-beta.desktop",
			"[Desktop Entry]\nIcon=beta-logo\nStartupWMClass=VendorApp\n")
		tree.write("usr/applications/vendor-stable.desktop",
			"[Desktop Entry]\nIcon=stable-logo\nStartupWMClass=VendorApp\n")
		tree.theme(hicolorTheme, "", 64)
		tree.writePNG("usr/icons/hicolor/64x64/apps/beta-logo.png", 64, 71)
		tree.writePNG("usr/icons/hicolor/64x64/apps/stable-logo.png", 64, 72)

		got := tree.resolver(tree.roots("home", "usr"), hicolorTheme).resolve(t.Context(), "vendorapp")
		if shade := decodeShade(t, got); shade != 71 {
			t.Fatalf("shade = %d, want 71: the user directory must win the class", shade)
		}
	}
}

// A hidden entry claims its ID and contributes no class mapping, so the
// next entry declaring that class takes it.
func TestResolverHiddenEntryYieldsItsClassToTheNextEntry(t *testing.T) {
	t.Parallel()

	tree := newIconTree(t)
	tree.write("home/applications/vendor-beta.desktop", "[Desktop Entry]\nHidden=true\n")
	tree.write("usr/applications/vendor-beta.desktop",
		"[Desktop Entry]\nIcon=beta-logo\nStartupWMClass=VendorApp\n")
	tree.write("usr/applications/vendor-stable.desktop",
		"[Desktop Entry]\nIcon=stable-logo\nStartupWMClass=VendorApp\n")
	tree.theme(hicolorTheme, "", 64)
	tree.writePNG("usr/icons/hicolor/64x64/apps/beta-logo.png", 64, 73)
	tree.writePNG("usr/icons/hicolor/64x64/apps/stable-logo.png", 64, 74)

	got := tree.resolver(tree.roots("home", "usr"), hicolorTheme).resolve(t.Context(), "vendorapp")
	if shade := decodeShade(t, got); shade != 74 {
		t.Errorf("shade = %d, want 74: the masked entry kept its class mapping", shade)
	}
}

// Reading a missing key from a nil map yields the zero value in Go, so
// the chained lookup in gtkSettingsTheme is safe when parseDesktopFile
// returns nil or the file has no [Settings] group. That is subtle
// enough to be worth pinning rather than re-deriving.
func TestGtkSettingsThemeSurvivesMissingAndMalformedFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		roots *iconRoots
	}{
		{"no roots at all", &iconRoots{}},
		{"directory with no settings files", &iconRoots{configDirs: []string{t.TempDir()}}},
		{"directory that does not exist", &iconRoots{configDirs: []string{"/nonexistent-icon-roots"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gtkSettingsTheme(tc.roots); got != "" {
				t.Errorf("theme = %q, want empty", got)
			}
		})
	}

	t.Run("files without a Settings group", func(t *testing.T) {
		t.Parallel()
		tree := newIconTree(t)
		tree.write("usr/config/gtk-3.0/settings.ini", "[NotSettings]\ngtk-icon-theme-name=Ignored\n")
		tree.write("usr/config/gtk-4.0/settings.ini", "this is not ini at all")

		roots := &iconRoots{configDirs: []string{tree.path("usr/config")}}
		if got := gtkSettingsTheme(roots); got != "" {
			t.Errorf("theme = %q, want empty: only the [Settings] group counts", got)
		}
	})
}
