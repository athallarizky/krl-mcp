// Command krl-mcp runs a fully self-contained KRL Commuter schedule MCP
// server over stdio: the pinned open-krl snapshot is EMBEDDED in the binary
// and parsed into memory at startup — no database, no cache, no network,
// no working-directory assumptions. Spawn it from any MCP host (Claude
// Code, Claude Desktop, Cursor, ...).
//
// NB: logs go to stderr — stdout IS the protocol.
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	asset "github.com/athallarizky/krl-mcp"
	"github.com/athallarizky/krl-mcp/internal/server"
	"github.com/athallarizky/krl-mcp/internal/store"
)

// main wires the embedded snapshot → store → MCP server over stdio until
// the host disconnects.
func main() {
	log.SetFlags(0)

	st, err := store.Load(asset.FS)
	if err != nil {
		log.Fatal(err)
	}
	if snap := st.Snapshot(); snap != "" {
		log.Printf("krl-mcp: snapshot %s — %d stations, %d boards, %d trips",
			snap, len(st.Stations()), st.BoardCount(), st.TripCount())
	}

	if err := server.New(st).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
