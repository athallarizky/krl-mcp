// Package asset embeds the pinned open-krl data snapshot into the binary.
// krl-mcp ships as a single static file: no data directory, no working-
// directory assumptions — the exact failure mode (cwd-relative data path
// serving empty boards) an LLM found in the production service's early
// days. Embedding kills that bug class for good.
package asset

import "embed"

// FS holds everything under data/ (snapshot, GTFS release, OSM coords).
//
//go:embed all:data
var FS embed.FS
