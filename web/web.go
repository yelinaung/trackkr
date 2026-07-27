// Package web holds the dashboard's templates and static assets.
//
// It exists as a package because go:embed patterns cannot escape the
// directory of the file declaring them, so internal/server cannot embed
// this tree directly.
package web

import "embed"

// Templates holds the HTML templates, parsed once at server startup.
//
//go:embed templates
var Templates embed.FS

// Static holds the vendored CSS and JS, the project stylesheet, and the
// self-hosted fonts. Nothing is fetched from a CDN: the CSP allows only
// 'self', and the dashboard should work offline.
//
//go:embed static
var Static embed.FS
