# krl-mcp

A **fully self-contained [Model Context Protocol](https://modelcontextprotocol.io) server** for Jakarta's KRL Commuter train schedules — written in Go, running entirely from pinned data artifacts in memory. No database, no cache, no network: clone it, hand it to your MCP host, and ask it about trains.

Part of a larger system: this is the MCP face of [krl-schedule-service](https://github.com/athallarizky/krl-schedule-service) (production service with a REST API, Postgres, Redis, and a trip planner), rebuilt here as a zero-infrastructure demo that anyone can run in 60 seconds.

## Try it in 60 seconds

```bash
git clone https://github.com/athallarizky/krl-mcp && cd krl-mcp
go build -o krl-mcp ./cmd/krl-mcp

claude mcp add krl -- "$PWD/krl-mcp"
```

Then ask Claude Code:

> *"I'm at Monas (-6.1754, 106.8272), which KRL station is closest?"*
> *"I'm at Tanah Abang and must reach Kebayoran before 6 AM — which train?"*
> *"How does train 5133D get from Kranji to Duri?"*

No Docker. No Postgres. No API keys. The 5 MB snapshot in `data/` is parsed into memory at startup.

## Tools

| Tool | What it answers |
|---|---|
| `get_stations` | The 111-station catalog (Jabodetabek + Yogyakarta–Solo) with coordinates |
| `get_departures` | One station's departure board in a time window |
| `get_lines` | The commercial lines with canonical colors |
| `plan_trip` | Direct-trip planner: `depart_at` (earliest first) or `arrive_by` (latest-that-still-makes-it first), with intermediate stops |
| `find_nearest_station` | GPS coordinate → closest stations with distances |
| `get_trip_stops` | One train's full ordered stop sequence — the auditable route narrative |

Design details worth stealing:

- **Honest empty answers.** When no direct trip exists, `plan_trip` returns `meta.transfer_candidates` — the stops where a *one-transfer* route *is* possible (empty means none exists). An LLM caller can reason about transfers from data instead of inventing routes.
- **Station names just work.** `"Tanah Tinggi"`, `"tanah tinggi"`, `"TANAHTINGGI"`, and `"THI"` all resolve (4-tier resolution: exact code → exact name → normalized → substring).
- **The service-day clock.** `24:04` means 00:04 after midnight *of the same service day* — how GTFS keeps late-night trains sortable. The planner accepts hours up to 47.
- **Headsigns are not routes.** Train `5133D` says "ANGKE" but runs the southern ring via Manggarai — `get_trip_stops` exists because an LLM once confidently narrated the wrong direction from the headsign alone.

## Architecture

```
boards/*.json ──────┐
stations.json ──────┤  parse once  ┌──────────────────────────────┐
GTFS stop_times ────┼─────────────▶│  in-memory store (~5 MB RAM) │
GTFS trips ─────────┘              └──────────────┬───────────────┘
                                                  │ stdio (JSON-RPC 2.0)
                                                  ▼
                                        your MCP host (Claude Code, ...)
```

The SQL the production service runs against Postgres becomes plain scans here: the planner's trip self-join is a prebuilt `stop → (trip, seq)` index; line colors are reduced in-memory; station resolution is three maps plus a fallback scan. Same logic, same output shapes — zero moving parts.

## The story: my LLM found my bugs

I built this as my first MCP server — and the most valuable testing turned out to be *letting Claude use it*. Across dogfooding sessions, the LLM surfaced bugs that an integration suite never caught:

1. A **relative data path** that resolved against the host's working directory — the tool silently served empty boards.
2. A **name resolution** gap: the tool description promised "or case-insensitive name" but the code only uppercased — the LLM compared two queries and caught the lie.
3. A **route narration hallucination**: with no intermediate-stop data exposed, the model invented a plausible-but-wrong route from the train's headsign. The fix — exposing `via` and `get_trip_stops` — is why this repo can answer "which way does this train actually go" from data.

Each fix became a test with real, verified numbers (train `1903A` departs Tangerang `04:27:00`, arrives Duri `04:53:00` — every golden assertion in this repo was checked against the artifacts before it was written).

## Data

- **Schedule & station data**: [open-krl/pipeline](https://github.com/open-krl/pipeline) raw snapshot + GTFS release — © open-krl contributors, [CC-BY 4.0](https://creativecommons.org/licenses/by/4.0/). Pinned in `data/` with a manifest (capture date, source hashes).
- **Yogyakarta–Solo coordinates**: extracted from [OpenStreetMap](https://www.openstreetmap.org/copyright) (ODbL).

Caveat: the feed **merges weekday/weekend day types** — a listed train may not run every day. Verify critical trips.

## Related

- **[krl-schedule-service](https://github.com/athallarizky/krl-schedule-service)** — the production system behind this demo: REST `/v1` API with Swagger, lazy syncing, cache-aside + singleflight, curated door-side data, and an HTTP+MCP dual face over one service layer.

## License

Code: [MIT](LICENSE). Bundled data: see [NOTICE](NOTICE) (CC-BY 4.0 / ODbL — attribution required).
