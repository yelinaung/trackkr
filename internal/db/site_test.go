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

		if strings.Contains(site, "@") {
			ht.Fatalf("SiteFromURL(%q) = %q, which kept userinfo", raw, site)
		}
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
	{name: "lowercases", url: "https://EXAMPLE.COM/x", want: testSiteHost, ok: true},
	{name: "strips root dot", url: "https://example.com./x", want: testSiteHost, ok: true},
	{name: "keeps ipv6 literal", url: "http://[::1]:8080/x", want: "[::1]", ok: true},
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
