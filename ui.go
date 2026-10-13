package main

import _ "embed"

// uiPage is the Management Center sidebar page. It is static: account data
// and edits go through the management-authenticated API from the browser.
//
//go:embed ui.html
var uiPage []byte

// The page runs inline script and style, talks only to its own origin, and
// may be framed only by the Management Center on the same origin.
const uiContentSecurityPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
