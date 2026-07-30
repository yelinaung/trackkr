package db

import (
	"regexp"
	"strings"
)

// The Go port of siteExpr, one compiled pattern per step of that SQL
// expression. The steps are kept separate rather than fused into a single
// pattern so the two derivations can be read side by side, and so
// TestSiteFromURLMatchesSQL can prove they agree.
var (
	siteAuthorityPattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://([^/?#]*)`)
	siteUserinfoPattern  = regexp.MustCompile(`^[^@]*@`)
	sitePortPattern      = regexp.MustCompile(`^(\[[^\]]+\]|[^:]+)(:[0-9]+)?$`)
	siteRootDotPattern   = regexp.MustCompile(`\.$`)
	siteWWWPattern       = regexp.MustCompile(`^www\.`)
)

// SiteFromURL derives the display host that GetSiteTotals groups on.
//
// The dashboard sums per site in SQL but a detail view has to decide, record
// by record, which ones belong to the site being examined. Deriving the host
// a second way would split hairs the totals do not: a record grouped under
// example.com in the summary must be one the detail view also counts, so this
// reproduces siteExpr step for step rather than reaching for net/url, whose
// parsing differs at exactly the edges that matter (uppercase schemes,
// bracketed IPv6 literals, malformed authorities).
//
// The second return is false for anything the SQL would have produced NULL
// for, which HAVING then drops: a URL with no parseable scheme and authority.
func SiteFromURL(rawURL string) (string, bool) {
	match := siteAuthorityPattern.FindStringSubmatch(rawURL)
	if match == nil {
		return "", false
	}

	site := siteUserinfoPattern.ReplaceAllString(match[1], "")
	site = sitePortPattern.ReplaceAllString(site, "$1")
	site = strings.ToLower(site)
	site = siteRootDotPattern.ReplaceAllString(site, "")
	return siteWWWPattern.ReplaceAllString(site, ""), true
}
