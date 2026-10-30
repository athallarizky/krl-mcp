// Package store loads the pinned open-krl artifacts into memory and serves
// every domain query from there — the whole point of krl-mcp: a fully
// self-contained MCP server with zero infrastructure (no database, no
// cache, no network). One 5 MB snapshot, parsed once at startup.
//
// Data provenance: github.com/open-krl/pipeline (CC-BY-4.0, see NOTICE) —
// the same artifacts that power the full production service, pinned here
// as a snapshot. This repo is a portfolio demo; see the README for the
// relationship to the production system.
package store

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// Station is one catalog entry with optional coordinates.
type Station struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Lat  *float64 `json:"lat"`
	Lon  *float64 `json:"lon"`
}

// Departure is one row of a station's departure board.
type Departure struct {
	TrainID   string  `json:"train_id"`
	Line      string  `json:"line"`
	Route     string  `json:"route"`
	Dest      string  `json:"dest"`
	DepartsAt string  `json:"departs_at"`
	ArrivesAt *string `json:"arrives_at"`
	Color     string  `json:"color"`
}

// TripStop is one timed stop of a train journey.
type TripStop struct {
	StopID    string `json:"stop_id"`
	Name      string `json:"name"`
	Seq       int    `json:"stop_seq"`
	Arrival   string `json:"arrival"`
	Departure string `json:"departure"`
}

// Trip is one journey: metadata plus its full ordered stop list.
type Trip struct {
	ID        string
	RouteID   string
	Headsign  string
	ServiceID string
	Stops     []TripStop
}

// stopRef locates one stop occurrence inside the trip map — the planner's
// in-memory equivalent of the trip_stops self-join.
type stopRef struct {
	tripID string
	seq    int
}

// Store is the parsed snapshot. Treat it as immutable after Load.
type Store struct {
	stations  []Station              // catalog order (sorted by ID)
	byID      map[string]*Station    // upper(ID) → station
	byName    map[string]*Station    // upper(name) → station
	byNorm    map[string]*Station    // normalized name → station
	boards    map[string][]Departure // station ID → whole-day board
	trips     map[string]*Trip
	stopIndex map[string][]stopRef // stop ID → occurrences across trips
	snapshot  string               // capture date from manifest.json
}

// Pinned artifact layout inside the embedded data tree.
const (
	snapshotDir = "data/openkrl/v2-snap6"
	gtfsDir     = "data/openkrl/gtfs-v0.1.0"
	osmCoords   = "data/coords/osm-yogya-solo-corridor.tsv"
)

// stationCodeOverrides maps catalog codes the board artifacts spell
// differently (the Grogol quirk: catalog GGL, boards/GTFS GRG). Mirrors the
// production service so both speak the same catalog.
var (
	toEndpoint = map[string]string{"GGL": "GRG"} // catalog → artifact
	toCatalog  = map[string]string{"GRG": "GGL"} // artifact → catalog
)

// catalogCode normalizes an artifact stop code into catalog domain.
func catalogCode(id string) string {
	if c, ok := toCatalog[id]; ok {
		return c
	}
	return id
}

// endpointCode normalizes a catalog code into the artifact filename domain.
func endpointCode(id string) string {
	if e, ok := toEndpoint[id]; ok {
		return e
	}
	return id
}

// Load parses the pinned snapshot (from the embedded FS) into memory.
// Every input is validated — a broken artifact fails loudly at startup,
// not mid-query.
func Load(fsys fs.FS) (*Store, error) {
	s := &Store{
		byID:      map[string]*Station{},
		byName:    map[string]*Station{},
		byNorm:    map[string]*Station{},
		boards:    map[string][]Departure{},
		trips:     map[string]*Trip{},
		stopIndex: map[string][]stopRef{},
	}
	snap := snapshotDir

	if err := s.loadStations(fsys, snap); err != nil {
		return nil, err
	}
	if err := s.loadCoords(fsys); err != nil {
		return nil, err
	}
	if err := s.loadBoards(fsys, snap); err != nil {
		return nil, err
	}
	if err := s.loadTrips(fsys); err != nil {
		return nil, err
	}

	// materialize the sorted catalog slice LAST — the loaders above mutate
	// stations through the map pointers (coordinates), so the slice must be
	// built from the final state
	s.stations = make([]Station, 0, len(s.byID))
	for _, st := range s.byID {
		s.stations = append(s.stations, *st)
	}
	sort.Slice(s.stations, func(i, j int) bool { return s.stations[i].ID < s.stations[j].ID })

	if raw, err := fs.ReadFile(fsys, snap+"/manifest.json"); err == nil {
		var m struct {
			SnapshotDate string `json:"snapshot_date"`
		}
		if json.Unmarshal(raw, &m) == nil {
			s.snapshot = m.SnapshotDate
		}
	}
	return s, nil
}

// loadStations parses the station master (WIL header rows excluded — they
// are regions, not stations; fg_enable is deliberately not trusted: the
// Serang line operates while flagged disabled upstream).
func (s *Store) loadStations(fsys fs.FS, snap string) error {
	raw, err := fs.ReadFile(fsys, snap+"/stations.json")
	if err != nil {
		return fmt.Errorf("store: stations.json: %w", err)
	}
	var env struct {
		Status int `json:"status"`
		Data   []struct {
			ID   string `json:"sta_id"`
			Name string `json:"sta_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("store: stations.json: %w", err)
	}
	if env.Status != 200 || len(env.Data) == 0 {
		return fmt.Errorf("store: stations.json: bad envelope status=%d rows=%d", env.Status, len(env.Data))
	}
	for _, d := range env.Data {
		if strings.HasPrefix(d.ID, "WIL") {
			continue
		}
		st := &Station{ID: d.ID, Name: d.Name}
		// the maps hold the authoritative pointers; s.stations is materialized
		// at the END of Load — coordinates are filled through these pointers
		// after loadStations, and slice copies would silently miss them
		s.byID[strings.ToUpper(st.ID)] = st
		s.byName[strings.ToUpper(st.Name)] = st
		s.byNorm[normalizeName(st.Name)] = st
	}
	if len(s.byID) == 0 {
		return fmt.Errorf("store: stations.json: zero stations after WIL filter")
	}
	return nil
}

// loadCoords fills stations.lat/lon from the GTFS stops file, then from an
// OSM extract for whatever is still missing (the Yogyakarta–Solo line) —
// OSM never overwrites GTFS-grade values, mirroring the production importer.
func (s *Store) loadCoords(fsys fs.FS) error {
	rows, err := readCSV(fsys, gtfsDir+"/stops.txt")
	if err != nil {
		return fmt.Errorf("store: stops.txt: %w", err)
	}
	for i, rec := range rows {
		if i == 0 || len(rec) < 4 {
			continue
		}
		st := s.byID[strings.ToUpper(catalogCode(rec[0]))]
		if st == nil {
			continue
		}
		lat, err1 := strconv.ParseFloat(rec[2], 64)
		lon, err2 := strconv.ParseFloat(rec[3], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		st.Lat, st.Lon = &lat, &lon
	}

	osm, err := readTSV(fsys, osmCoords)
	if err != nil {
		return fmt.Errorf("store: osm coords: %w", err)
	}
	for _, rec := range osm {
		if len(rec) < 3 {
			continue
		}
		st := s.byName[strings.ToUpper(rec[0])]
		if st == nil || st.Lat != nil {
			continue // unknown to the catalog, or already GTFS-grade
		}
		lat, err1 := strconv.ParseFloat(rec[1], 64)
		lon, err2 := strconv.ParseFloat(rec[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		st.Lat, st.Lon = &lat, &lon
	}
	return nil
}

// loadBoards parses every station board file. Files are keyed by endpoint
// code (GGL's board lives in GRG.json). Stations outside the capture scope
// (Yogyakarta–Solo, Serang) simply have no entry — Departures degrades to an
// honest empty board.
func (s *Store) loadBoards(fsys fs.FS, snap string) error {
	files, err := fs.Glob(fsys, snap+"/boards/*.json")
	if err != nil || len(files) == 0 {
		return fmt.Errorf("store: boards/: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			return fmt.Errorf("store: %s: %w", f, err)
		}
		var env struct {
			Status int `json:"status"`
			Data   []struct {
				TrainID  string `json:"train_id"`
				Line     string `json:"ka_name"`
				Route    string `json:"route_name"`
				Dest     string `json:"dest"`
				TimeEst  string `json:"time_est"`
				DestTime string `json:"dest_time"`
				Color    string `json:"color"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("store: %s: %w", f, err)
		}
		if env.Status != 200 {
			return fmt.Errorf("store: %s: bad envelope status=%d", f, env.Status)
		}
		code := strings.TrimSuffix(pathBase(f), ".json")
		board := make([]Departure, 0, len(env.Data))
		for _, d := range env.Data {
			var arr *string
			if d.DestTime != "" {
				t := d.DestTime
				arr = &t
			}
			board = append(board, Departure{
				TrainID:   d.TrainID,
				Line:      d.Line,
				Route:     d.Route,
				Dest:      d.Dest,
				DepartsAt: d.TimeEst,
				ArrivesAt: arr,
				Color:     d.Color,
			})
		}
		s.boards[code] = board
	}
	return nil
}

// loadTrips parses the GTFS release (trips + stop_times) into the journey
// map and the stop index the planner scans instead of a SQL self-join.
func (s *Store) loadTrips(fsys fs.FS) error {
	rows, err := readCSV(fsys, gtfsDir+"/trips.txt")
	if err != nil {
		return fmt.Errorf("store: trips.txt: %w", err)
	}
	for i, rec := range rows {
		if i == 0 || len(rec) < 4 {
			continue
		}
		s.trips[rec[2]] = &Trip{ID: rec[2], RouteID: rec[0], ServiceID: rec[1], Headsign: rec[3]}
	}

	srows, err := readCSV(fsys, gtfsDir+"/stop_times.txt")
	if err != nil {
		return fmt.Errorf("store: stop_times.txt: %w", err)
	}
	for i, rec := range srows {
		if i == 0 || len(rec) < 5 {
			continue
		}
		seq, err := strconv.Atoi(rec[4])
		if err != nil {
			return fmt.Errorf("store: stop_times.txt: bad sequence %q", rec[4])
		}
		trip := s.trips[rec[0]]
		if trip == nil {
			return fmt.Errorf("store: stop_times.txt: row references unknown trip %q", rec[0])
		}
		stop := catalogCode(rec[3])
		trip.Stops = append(trip.Stops, TripStop{
			StopID:    stop,
			Seq:       seq,
			Arrival:   rec[1],
			Departure: rec[2],
		})
	}
	for _, t := range s.trips {
		sort.Slice(t.Stops, func(i, j int) bool { return t.Stops[i].Seq < t.Stops[j].Seq })
		for _, st := range t.Stops {
			s.stopIndex[st.StopID] = append(s.stopIndex[st.StopID], stopRef{t.ID, st.Seq})
		}
	}
	if len(s.trips) == 0 {
		return fmt.Errorf("store: zero trips parsed")
	}
	return nil
}

// Snapshot returns the capture date from manifest.json ("" when absent) —
// surfaced by the server descriptions so consumers know the data vintage.
func (s *Store) Snapshot() string { return s.snapshot }

// BoardCount returns how many station boards the snapshot carries.
func (s *Store) BoardCount() int { return len(s.boards) }

// TripCount returns how many journeys the GTFS release carries.
func (s *Store) TripCount() int { return len(s.trips) }

// readCSV parses a comma-separated file with a header row from fsys.
func readCSV(fsys fs.FS, name string) ([][]string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	return r.ReadAll()
}

// readTSV parses a tab-separated file without a header (raw Overpass csv).
func readTSV(fsys fs.FS, name string) ([][]string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, "\t"))
	}
	return out, nil
}

// pathBase returns the final element of a slash-separated path (embed FS
// always uses forward slashes).
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
