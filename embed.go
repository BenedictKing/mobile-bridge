// Package mobilebridge embeds the browser assets served by the gateway.
package mobilebridge

import "embed"

// WebFS holds the web/ sources verbatim. The HTTP server prepares compressed
// production responses in memory, so building the gateway still needs only Go.
//
//go:embed web
var WebFS embed.FS
