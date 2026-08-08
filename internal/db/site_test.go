package db

import (
	"context"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// TestSiteFromURLSurvivesArbitraryText guards the derivation against the
// text a URL column can actually hold. It runs on every record the
// detail view examines, so a panic here takes the page down.
func TestSiteFromURLSurvivesArbitraryText(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Text())
		site, ok := SiteFromURL(raw)
		if !ok && site != "" {
			ht.Fatalf("SiteFromURL(%q) reported failure but returned %q", raw, site)
		}
	})
}

// TestSiteFromURLOutputShape checks what each step of the SQL expression
// is supposed to have removed by the end.
//
// One thing the name suggests is not true: a result can still start with
// "www.", because only one prefix is stripped, so www.www.example.com
// derives www.example.com. Asserting otherwise would be asserting a
// behaviour siteExpr does not have, and the two derivations must agree.
func TestSiteFromURLOutputShape(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Text())
		site, ok := SiteFromURL(raw)
		if !ok {
			return
		}

		// No claim about "@" belongs here. The userinfo pattern is
		// anchored, so it strips through the first "@" only, and
		// "http://user@host@extra/" derives "host@extra". That is what
		// siteExpr does, and the two must agree before they must be
		// tidy. TestSiteFromURL covers the shape by example instead.
		if lowered := strings.ToLower(site); lowered != site {
			ht.Fatalf("SiteFromURL(%q) = %q, which is not lowercased", raw, site)
		}
		if strings.HasSuffix(site, ".") {
			ht.Fatalf("SiteFromURL(%q) = %q, which kept the root dot", raw, site)
		}
		if strings.ContainsAny(site, "/?#") {
			ht.Fatalf("SiteFromURL(%q) = %q, which reached past the authority", raw, site)
		}
	})
}

// TestSiteFromURLRecoversAConstructedHost is the round trip: a URL built
// around a host must group under that host, so a record inserted for one
// site is one the summary counts there.
func TestSiteFromURLRecoversAConstructedHost(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		host := hegel.Draw(ht, hegel.Domains())
		scheme := hegel.Draw(ht, hegel.SampledFrom([]string{"http", "https"}))
		path := hegel.Draw(ht, hegel.SampledFrom([]string{"", "/", "/a/b", "/x?y=1#z"}))

		want := strings.TrimPrefix(strings.ToLower(host), "www.")
		got, ok := SiteFromURL(scheme + "://" + host + path)
		if !ok {
			ht.Fatalf("SiteFromURL dropped %s://%s%s", scheme, host, path)
		}
		if got != want {
			ht.Fatalf("SiteFromURL(%s://%s%s) = %q, want %q", scheme, host, path, got, want)
		}
	})
}

// testIPv6Site is the bracketed literal the derivation keeps whole.
const testIPv6Site = "[::1]"

// siteDerivationCases are shared by the pure test below and the parity test
// against PostgreSQL, so a case added for one is checked by both.
var siteDerivationCases = []struct {
	name string
	url  string
	want string
	ok   bool
}{
	{name: "plain host", url: "https://example.com/page", want: testSiteHost, ok: true},
	{name: "strips www", url: "https://www.example.com/", want: testSiteHost, ok: true},
	{name: "strips port", url: "https://example.com:8443/x", want: testSiteHost, ok: true},
	{name: "strips userinfo", url: "https://" + testUserinfo + "@example.com/x", want: testSiteHost, ok: true},
	// The userinfo pattern is anchored, so it strips through the first
	// "@" and leaves the rest in the host. Kept as a case because the Go
	// port and siteExpr have to agree even where the result is odd.
	{name: "strips only the first userinfo", url: "https://a@b@example.com/x", want: "b@example.com", ok: true},
	{name: "lowercases", url: "https://EXAMPLE.COM/x", want: testSiteHost, ok: true},
	{name: "strips root dot", url: "https://example.com./x", want: testSiteHost, ok: true},
	{name: "keeps ipv6 literal", url: "http://[::1]:8080/x", want: testIPv6Site, ok: true},
	{name: "keeps subdomain", url: "https://mail.google.com/u/0", want: "mail.google.com", ok: true},
	{name: "no query or fragment", url: "https://example.com?a=b#c", want: testSiteHost, ok: true},
	{name: "uppercase scheme is not matched", url: "HTTPS://example.com/x", want: "", ok: false},
	{name: "no scheme", url: "example.com/x", want: "", ok: false},
	{name: "empty", url: "", want: "", ok: false},
	{name: "about page", url: "about:blank", want: "", ok: false},
}

func TestSiteFromURL(t *testing.T) {
	t.Parallel()
	for _, tc := range siteDerivationCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SiteFromURL(tc.url)
			if ok != tc.ok {
				t.Fatalf("SiteFromURL(%q) ok = %v, want %v", tc.url, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("SiteFromURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestSiteFromURLMatchesSQL is the reason the Go port is a port rather than a
// call to net/url. A record the summary grouped under one host must be a
// record the detail view counts under that same host, and the only way to know
// the two derivations agree is to run both.
func TestSiteFromURLMatchesSQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for _, tc := range siteDerivationCases {
		var sql *string
		err := pool.QueryRow(ctx,
			`SELECT `+siteExpr+` FROM (VALUES ($1::text)) AS ar(url)`, tc.url).Scan(&sql)
		if err != nil {
			t.Fatalf("%s: querying siteExpr: %v", tc.name, err)
		}

		got, ok := SiteFromURL(tc.url)
		if !ok {
			if sql != nil {
				t.Errorf("%s: Go dropped %q, SQL grouped it as %q", tc.name, tc.url, *sql)
			}
			continue
		}
		if sql == nil {
			t.Errorf("%s: Go derived %q from %q, SQL dropped it", tc.name, got, tc.url)
			continue
		}
		if *sql != got {
			t.Errorf("%s: SQL = %q, Go = %q", tc.name, *sql, got)
		}
	}
}

// drawSiteURL builds URLs compositionally so most cases reach the
// derivation instead of the drop path.
//
// Raw text would almost never match the authority pattern, spending
// every case proving that nonsense is rejected. A minority of raw draws
// is mixed in by the caller to cover that path deliberately.
func drawSiteURL(tc hegel.TestCase) string {
	scheme := hegel.Draw(tc, hegel.SampledFrom([]string{
		"http", "https", "ftp", "s3+http",
		"HTTPS", // uppercase is not matched, so it must be dropped
	}))

	host := hegel.Draw(tc, hegel.SampledFrom([]string{
		testSiteHost, "WWW.Example.COM", "www." + testSiteHost,
		testSiteHost + ".", "sub." + testSiteHost, testIPv6Site, "[2001:db8::1]",
		"127.0.0.1", "xn--80ak6aa92e.com", "", "a..b",
	}))

	var url strings.Builder
	url.WriteString(scheme)
	url.WriteString("://")
	if userinfo := hegel.Draw(tc, hegel.SampledFrom([]string{
		"", "user@", "user:pw@", "a@b@", "@",
	})); userinfo != "" {
		url.WriteString(userinfo)
	}
	url.WriteString(host)
	url.WriteString(hegel.Draw(tc, hegel.SampledFrom([]string{"", ":8443", ":0", ":"})))
	url.WriteString(hegel.Draw(tc, hegel.SampledFrom([]string{
		"", "/", "/a/b", "?q=1", "#frag", "/x?y=1#z",
	})))
	return url.String()
}

// TestSiteFromURLMatchesSQLOverGeneratedURLs is why the Go derivation is
// a port of siteExpr rather than a call to net/url. A record the summary
// groups under one host must be one the detail view counts there, and
// the only way to know the two agree is to run both.
//
// The case count is lowered because every case is a database round trip.
// CI sets TRACKKR_TEST_DSN, so this does not skip there.
func TestSiteFromURLMatchesSQLOverGeneratedURLs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	hegel.Test(t, func(ht *hegel.T) {
		raw := drawSiteURL(ht)
		if hegel.Draw(ht, hegel.Integers(0, 4)) == 0 {
			raw = hegel.Draw(ht, hegel.Text())
		}

		var fromSQL *string
		if err := pool.QueryRow(
			ctx,
			`SELECT `+siteExpr+` FROM (VALUES ($1::text)) AS ar(url)`, raw,
		).Scan(&fromSQL); err != nil {
			ht.Fatalf("querying siteExpr for %q: %v", raw, err)
		}

		got, ok := SiteFromURL(raw)
		switch {
		case !ok && fromSQL != nil:
			ht.Fatalf("Go dropped %q, SQL grouped it as %q", raw, *fromSQL)
		case ok && fromSQL == nil:
			ht.Fatalf("Go derived %q from %q, SQL dropped it", got, raw)
		case ok && *fromSQL != got:
			ht.Fatalf("for %q: SQL = %q, Go = %q", raw, *fromSQL, got)
		}
	}, hegel.WithTestCases(200))
}
