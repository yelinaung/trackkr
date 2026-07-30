package db

import (
	"slices"

	"github.com/yelinaung/trackkr/internal/icon"
	"github.com/yelinaung/trackkr/internal/identity"
)

// browserFamily groups the names one browser is known by with the producer that
// reports its tabs.
//
// The names differ per platform for the same browser: macOS reports
// "Google Chrome" and the Linux X11 detector reports "google-chrome". Both are
// the same application to a reader, so both must lose their time to a Chrome
// extension observation and both must aggregate into one total.
type browserFamily struct {
	producer identity.Producer
	// canonical is the display name every record in the family aggregates
	// under, whichever alias it was stored as.
	canonical string
	// desktopKeys are the normalized names the native detector may report.
	desktopKeys []string
	// iconKeys are tried in order when resolving the family's application
	// icon, so a macOS row wins over a Linux row for the same user.
	iconKeys []string
}

// The canonical display names, exported so callers and tests name one browser
// one way.
const (
	FirefoxAppName = "Firefox"
	ChromeAppName  = "Google Chrome"
)

var browserFamilies = []browserFamily{
	{
		producer:    identity.ProducerFirefox,
		canonical:   FirefoxAppName,
		desktopKeys: []string{"firefox"},
		iconKeys:    []string{"firefox"},
	},
	{
		producer:    identity.ProducerChrome,
		canonical:   ChromeAppName,
		desktopKeys: []string{"google chrome", "google-chrome"},
		iconKeys:    []string{"google chrome", "google-chrome"},
	},
}

// familyForProducer returns the family whose tabs that producer reports.
// The desktop producer belongs to no family: it observes every application.
func familyForProducer(producer identity.Producer) (browserFamily, bool) {
	for _, family := range browserFamilies {
		if family.producer == producer {
			return family, true
		}
	}
	return browserFamily{}, false
}

// familyForDesktopName returns the family a native observation belongs to.
func familyForDesktopName(appName string) (browserFamily, bool) {
	key := icon.AppKey(appName)
	for _, family := range browserFamilies {
		if slices.Contains(family.desktopKeys, key) {
			return family, true
		}
	}
	return browserFamily{}, false
}

// canonicalAppName folds a record onto its family's display name so one browser
// does not appear as two rows. An application in no known family keeps the name
// it was stored under.
func canonicalAppName(record *ActivityRecordRow) string {
	if family, ok := familyForProducer(record.Producer); ok {
		return family.canonical
	}
	if family, ok := familyForDesktopName(record.AppName); ok {
		return family.canonical
	}
	return record.AppName
}

// coverageKey identifies whose time a browser observation may subtract: one
// family on one device. Keying on the producer rather than the application name
// is the point -- the producer comes from the route the record arrived on, so
// an extension cannot name itself into another browser's coverage.
type coverageKey struct {
	deviceID int64
	producer identity.Producer
}

// browserCoverageKey reports the coverage a record contributes to, and whether
// it contributes at all. Only a URL-bearing record from a known browser
// producer covers anything.
func browserCoverageKey(record *ActivityRecordRow) (coverageKey, bool) {
	if !hasActivityURL(record) {
		return coverageKey{}, false
	}
	if _, ok := familyForProducer(record.Producer); !ok {
		return coverageKey{}, false
	}
	return coverageKey{deviceID: record.DeviceID, producer: record.Producer}, true
}

// desktopCoverageKey reports which coverage may subtract from this record.
//
// Only a native observation of a known browser yields to an extension, and only
// to its own family: Chrome tab coverage must never erase desktop Firefox time.
// A URL-bearing record from an unknown producer renders normally and subtracts
// nothing.
func desktopCoverageKey(record *ActivityRecordRow) (coverageKey, bool) {
	if record.Producer != identity.ProducerDesktop {
		return coverageKey{}, false
	}
	family, ok := familyForDesktopName(record.AppName)
	if !ok {
		return coverageKey{}, false
	}
	return coverageKey{deviceID: record.DeviceID, producer: family.producer}, true
}

// AppIconKeys returns the icon keys to try for an application, most preferred
// first. A browser in a known family offers every alias it is stored under; any
// other application offers only its own normalized name.
func AppIconKeys(appName string) []string {
	if family, ok := familyForDesktopName(appName); ok {
		return family.iconKeys
	}
	if key := icon.AppKey(appName); key != "" && len(key) <= icon.MaxKeyBytes {
		return []string{key}
	}
	return nil
}
