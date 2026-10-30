// Package server wires the six schedule tools onto an MCP server. Handlers
// close over the in-memory store — no globals, no infrastructure, just
// parsed input → store query → structured output.
package server

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/athallarizky/krl-mcp/internal/store"
)

// New builds an MCP server over the loaded snapshot.
func New(s *store.Store) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "krl-mcp",
		Version: "1.0.0",
	}, nil)
	registerTools(server, s)
	return server
}

// stationOut is the public station shape.
type stationOut struct {
	ID   string   `json:"id" jsonschema:"station code, e.g. TNG"`
	Name string   `json:"name" jsonschema:"station name, uppercase"`
	Lat  *float64 `json:"lat" jsonschema:"WGS-84 latitude, null if unknown"`
	Lon  *float64 `json:"lon" jsonschema:"WGS-84 longitude, null if unknown"`
}

type stationsResult struct {
	Data []stationOut `json:"data" jsonschema:"all stations"`
}

type linesResult struct {
	Data []store.Line `json:"data" jsonschema:"all lines"`
}

type departuresInput struct {
	Station string `json:"station" jsonschema:"station code (e.g. TNG) or case-insensitive name"`
	From    string `json:"from,omitempty" jsonschema:"window start HH:MM, default 00:00"`
	To      string `json:"to,omitempty" jsonschema:"window end HH:MM, default 23:59"`
	Dest    string `json:"dest,omitempty" jsonschema:"optional exact destination filter, e.g. DURI"`
}

type departuresResult struct {
	Data []store.Departure `json:"data" jsonschema:"departures, sorted by time"`
	Meta struct {
		Station   string   `json:"station"`
		Window    []string `json:"window"`
		Dest      string   `json:"dest,omitempty"`
		Source    string   `json:"source"`
		FetchedAt string   `json:"fetched_at"`
	} `json:"meta"`
}

type planTripInput struct {
	From     string `json:"from" jsonschema:"origin station code (e.g. THI) or name (e.g. Tanah Tinggi)"`
	To       string `json:"to" jsonschema:"destination station code or name"`
	DepartAt string `json:"depart_at,omitempty" jsonschema:"earliest acceptable departure HH:MM — use this OR arrive_by"`
	ArriveBy string `json:"arrive_by,omitempty" jsonschema:"latest acceptable arrival HH:MM on the service-day clock: 24:30 means 00:30 next day"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max options, default 5, max 10"`
}

type nearestInput struct {
	Lat   float64 `json:"lat" jsonschema:"WGS-84 latitude of the user, e.g. -6.178"`
	Lon   float64 `json:"lon" jsonschema:"WGS-84 longitude of the user, e.g. 106.632"`
	Limit int     `json:"limit,omitempty" jsonschema:"max results, default 5, max 20"`
}

type nearestResult struct {
	Data []store.NearStation `json:"data" jsonschema:"nearest stations, closest first"`
}

type tripStopsInput struct {
	TripID string `json:"trip_id" jsonschema:"train number, e.g. 5133D"`
}

// registerTools mounts every tool. The data snapshot note (capture date)
// rides in the tool descriptions — consumers deserve to know the vintage.
func registerTools(s *mcp.Server, st *store.Store) {
	snapshot := st.Snapshot()

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_stations",
		Description: "List all KRL Commuter stations (Indonesia) with id, name, and coordinates. " +
			"Use to resolve a place name to a station id. Coverage: all 111 stations including Yogyakarta-Solo.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, stationsResult, error) {
		rows := st.Stations()
		out := stationsResult{Data: make([]stationOut, len(rows))}
		for i, r := range rows {
			out.Data[i] = stationOut{ID: r.ID, Name: r.Name, Lat: r.Lat, Lon: r.Lon}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_departures",
		Description: "Departure board of one KRL station within a time window " +
			"(times are HH:MM). Optionally filter by exact destination. " +
			"NOT a route planner: it lists trains leaving ONE station. " +
			"Pinned snapshot (" + snapshot + "); day types are merged in the feed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in departuresInput) (*mcp.CallToolResult, departuresResult, error) {
		rows, err := st.Departures(in.Station, in.From, in.To, in.Dest)
		if err != nil {
			return nil, departuresResult{}, err
		}
		out := departuresResult{Data: rows}
		out.Meta.Station = in.Station
		out.Meta.Window = []string{orDefault(in.From, "00:00"), orDefault(in.To, "23:59")}
		out.Meta.Dest = in.Dest
		out.Meta.Source = "pinned-snapshot"
		out.Meta.FetchedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_lines",
		Description: "List all KRL Commuter lines with their canonical color and traffic counts.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, linesResult, error) {
		return nil, linesResult{Data: st.Lines()}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "plan_trip",
		Description: "Plan a direct one-train trip between two stations (no transfers). " +
			"Exactly one of depart_at (earliest acceptable departure) or arrive_by " +
			"(latest acceptable arrival) in HH:MM. Times follow the GTFS service-day " +
			"clock: 24:xx means after midnight — pass arrive_by=24:30 for 00:30 next day. " +
			"Direct trips only: when none exists, meta carries note + transfer_candidates " +
			"(stops reachable from the origin AND with a direct service onward to your " +
			"destination — empty means no single-transfer route). Each option lists via " +
			"(intermediate stops) — call get_trip_stops for full detail. " +
			"Pinned snapshot (" + snapshot + "); day types are merged — verify critical trips.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in planTripInput) (*mcp.CallToolResult, store.PlanResult, error) {
		res, err := st.PlanTrip(store.PlanQuery{
			From:     in.From,
			To:       in.To,
			DepartAt: in.DepartAt,
			ArriveBy: in.ArriveBy,
			Limit:    in.Limit,
		})
		if err != nil {
			return nil, store.PlanResult{}, err
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "find_nearest_station",
		Description: "Find the KRL stations nearest to a WGS-84 coordinate " +
			"(latitude/longitude), closest first, with distance in meters. " +
			"Use when the user shares a location instead of a station name. " +
			"Coverage: all 111 stations including Yogyakarta-Solo.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in nearestInput) (*mcp.CallToolResult, nearestResult, error) {
		rows, err := st.NearestStations(in.Lat, in.Lon, in.Limit)
		if err != nil {
			return nil, nearestResult{}, err
		}
		return nil, nearestResult{Data: rows}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_trip_stops",
		Description: "Full ordered stop sequence of one train (trip_id from " +
			"plan_trip or get_departures), with station names and times. " +
			"Use it to narrate or verify WHICH WAY a train goes — headsigns " +
			"are not route descriptors.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tripStopsInput) (*mcp.CallToolResult, store.TripDetail, error) {
		res, err := st.TripStops(in.TripID)
		if err != nil {
			return nil, store.TripDetail{}, err
		}
		return nil, res, nil
	})
}

// orDefault returns def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
