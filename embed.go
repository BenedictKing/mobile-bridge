// Package mobilebridge embeds the browser assets served by the gateway.
package mobilebridge

import "embed"

// WebFS holds web/ verbatim; the Go gateway serves it exactly like the
// Python gateway serves the directory (no build step, same file names).
//
//go:embed web
var WebFS embed.FS
