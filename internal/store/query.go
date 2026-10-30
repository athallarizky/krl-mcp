// Domain queries over the in-memory snapshot — ports of the production
// service logic (krl-schedule-service/internal/service), with the SQL
// replaced by plain scans. Output shapes are identical, so consumers of
// either system see the same contract.
package store

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ErrBadQuery signals malformed user input.
var ErrBadQuery = errors.New("krl-mcp: bad query")

// ErrStationNotFound signals a station input that matched nothing.
var ErrStationNotFound = errors.New("krl-mcp: station not found")

// ErrTripNotFound signals an unknown trip_id.
var ErrTripNotFound = errors.New("krl-mcp: trip not found")

// --- stations ---

// Stations returns the full catalog (sorted by ID).
func (s *Store) Stations() []Station {
	out := make([]Station, len(s.stations))
	copy(out, s.stations)
	return out
}

// normalizeName strips characters that differ between formal names and
// casual speech — "ST. TANGERANG" and "st tangerang" collapse to the same key.
func normalizeName(name string) string {
	q := strings.ToUpper(strings.TrimSpace(name))
	q = strings.NewReplacer(" ", "", ".", "", "-", "").Replace(q)
	return q
}

// ResolveStation accepts a station code ("THI") OR a name ("Tanah Tinggi",
// any case) and returns the canonical code. Four tiers, same as production:
// exact code → exact name → normalized name → substring (alphabetical first).
func (s *Store) ResolveStation(input string) (string, error) {
	q := strings.ToUpper(strings.TrimSpace(input))
	if q == "" {
		return "", fmt.Errorf("%w: station is required", ErrBadQuery)
	}

	if st := s.byID[q]; st != nil {
		return st.ID, nil
	}
	if st := s.byName[q]; st != nil {
		return st.ID, nil
	}
	if st := s.byNorm[normalizeName(q)]; st != nil {
		return st.ID, nil
	}

	var matches []string
	for _, st := range s.stations {
		if strings.Contains(strings.ToUpper(st.Name), q) {
			matches = append(matches, st.ID)
		}
	}
	if len(matches) > 0 {
		sort.Strings(matches) // deterministic: alphabetical first
		return matches[0], nil
	}
	return "", fmt.Errorf("%w: %q matched no station code or name", ErrStationNotFound, input)
}

// --- departures ---

// Departures returns one station's whole-day board within an HH:MM window,
// optionally filtered by exact terminus. Stations outside the capture scope
// yield an honest empty board.
func (s *Store) Departures(station, from, to, dest string) ([]Departure, error) {
	if station == "" {
		return nil, fmt.Errorf("%w: station is required", ErrBadQuery)
	}
	id, err := s.ResolveStation(station)
	if err != nil {
		return nil, err
	}
	fromQ, err := normalizeClock(from, "00:00")
	if err != nil {
		return nil, err
	}
	toQ, err := normalizeClock(to, "23:59")
	if err != nil {
		return nil, err
	}
	dest = strings.ToUpper(strings.TrimSpace(dest))

	board := s.boards[endpointCode(id)]
	out := make([]Departure, 0, len(board))
	for _, d := range board {
		if d.DepartsAt < fromQ || d.DepartsAt > toQ+":59" {
			continue
		}
		if dest != "" && d.Dest != dest {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DepartsAt < out[j].DepartsAt })
	return out, nil
}

// normalizeClock validates HH:MM (strict, zero-padded) or returns the
// default; output is zero-padded HH:MM:SS for string comparisons.
func normalizeClock(v, def string) (string, error) {
	if v == "" {
		v = def
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: time must be HH:MM, got %q", ErrBadQuery, v)
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || len(parts[0]) != 2 || len(parts[1]) != 2 || mm > 59 {
		return "", fmt.Errorf("%w: time must be HH:MM, got %q", ErrBadQuery, v)
	}
	return fmt.Sprintf("%02d:%02d:00", hh, mm), nil
}

// --- lines ---

// Line is one commercial line with its canonical color and traffic counts.
type Line struct {
	Line           string `json:"line"`
	Color          string `json:"color"`
	RouteCount     int    `json:"route_count"`
	DepartureCount int    `json:"departure_count"`
}

// Lines derives commercial lines from all boards. Upstream is NOT
// line→color 1:1 (through-services carry their dominant leg's color), so
// each line collapses to one row whose color is the most-departures variant.
func (s *Store) Lines() []Line {
	type agg struct {
		routes map[string]bool
		count  int
	}
	per := map[string]map[string]*agg{} // line → color → agg
	var lineOrder []string
	for _, board := range s.boards {
		for _, d := range board {
			if per[d.Line] == nil {
				per[d.Line] = map[string]*agg{}
				lineOrder = append(lineOrder, d.Line)
			}
			if per[d.Line][d.Color] == nil {
				per[d.Line][d.Color] = &agg{routes: map[string]bool{}}
			}
			a := per[d.Line][d.Color]
			a.routes[d.Route] = true
			a.count++
		}
	}
	sort.Strings(lineOrder)

	out := make([]Line, 0, len(lineOrder))
	for _, line := range lineOrder {
		row := Line{Line: line}
		best := -1
		colors := make([]string, 0, len(per[line]))
		for c := range per[line] {
			colors = append(colors, c)
		}
		sort.Strings(colors)
		for _, c := range colors {
			a := per[line][c]
			row.RouteCount += len(a.routes)
			row.DepartureCount += a.count
			if a.count > best {
				best = a.count
				row.Color = c
			}
		}
		out = append(out, row)
	}
	return out
}

// --- nearest ---

// NearStation is one nearest-station candidate with great-circle distance.
type NearStation struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	DistanceM int     `json:"distance_m"`
}

const earthRadiusM = 6371000.0

// haversineM returns the great-circle distance in meters between two
// WGS-84 points (same formula as production — stable meter values).
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	p1 := math.Pi * lat1 / 180
	p2 := math.Pi * lat2 / 180
	dLat := math.Pi * (lat2 - lat1) / 180
	dLon := math.Pi * (lon2 - lon1) / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadiusM * 2 * math.Asin(math.Sqrt(a))
}

// NearestStations returns the stations closest to a point, sorted by
// distance. Stations without coordinates are skipped (sparse by design).
func (s *Store) NearestStations(lat, lon float64, limit int) ([]NearStation, error) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, fmt.Errorf("%w: lat/lon out of range (lat %v, lon %v)", ErrBadQuery, lat, lon)
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	near := make([]NearStation, 0, len(s.stations))
	for i := range s.stations {
		st := &s.stations[i]
		if st.Lat == nil || st.Lon == nil {
			continue
		}
		near = append(near, NearStation{
			ID:        st.ID,
			Name:      st.Name,
			Lat:       *st.Lat,
			Lon:       *st.Lon,
			DistanceM: int(math.Round(haversineM(lat, lon, *st.Lat, *st.Lon))),
		})
	}
	sort.Slice(near, func(i, j int) bool { return near[i].DistanceM < near[j].DistanceM })
	if len(near) > limit {
		near = near[:limit]
	}
	return near, nil
}

// --- trip detail ---

// TripDetail is one journey's metadata plus its full ordered stop list.
type TripDetail struct {
	TripID    string     `json:"trip_id"`
	RouteID   string     `json:"route_id"`
	Headsign  string     `json:"headsign"`
	ServiceID string     `json:"service_id"`
	Stops     []TripStop `json:"stops"`
}

// TripStops returns one journey's ordered stop sequence with station names —
// the auditable "which way does this train go" answer.
func (s *Store) TripStops(tripID string) (TripDetail, error) {
	t := s.trips[strings.ToUpper(strings.TrimSpace(tripID))]
	if t == nil {
		return TripDetail{}, fmt.Errorf("%w: %q", ErrTripNotFound, tripID)
	}
	stops := make([]TripStop, len(t.Stops))
	copy(stops, t.Stops)
	for i := range stops {
		if st := s.byID[stops[i].StopID]; st != nil {
			stops[i].Name = st.Name
		}
	}
	return TripDetail{
		TripID:    t.ID,
		RouteID:   t.RouteID,
		Headsign:  t.Headsign,
		ServiceID: t.ServiceID,
		Stops:     stops,
	}, nil
}

// --- planner ---

// serviceDayHourMax is the highest hour the planner clock accepts (GTFS
// service days extend past 24h; 47 covers every realistic timetable).
const serviceDayHourMax = 47

// parseServiceTime validates "HH:MM" on the GTFS service-day clock (hours
// 00–47) and normalizes to "HH:MM:SS". Deliberately NOT time.Parse("15:04"):
// that format rejects 24:xx — exactly the late-night values the planner
// must accept (a listed 24:04 departure means 00:04 after midnight of the
// same service day).
func parseServiceTime(v string) (string, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return "", fmt.Errorf("%w: time must be HH:MM, got %q", ErrBadQuery, v)
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || mm > 59 || hh > serviceDayHourMax {
		return "", fmt.Errorf("%w: invalid service-day time %q", ErrBadQuery, v)
	}
	return v + ":00", nil
}

// minutesOf converts "HH:MM:SS" to minutes since service-day start.
func minutesOf(v string) int {
	hh, _ := strconv.Atoi(v[:2])
	mm, _ := strconv.Atoi(v[3:5])
	return hh*60 + mm
}

// PlanQuery is one planner request. Exactly one of DepartAt / ArriveBy is
// set — the mode IS which field is present.
type PlanQuery struct {
	From     string
	To       string
	DepartAt string // "HH:MM", earliest acceptable departure
	ArriveBy string // "HH:MM", latest acceptable arrival (service-day clock)
	Limit    int
}

// PlanOption is one direct-trip candidate. Via lists intermediate stop
// codes (exclusive, in travel order) so callers narrate the route from
// data instead of guessing from the headsign; StopCount is inclusive.
type PlanOption struct {
	TrainID     string   `json:"train_id"`
	Headsign    string   `json:"headsign"`
	RouteID     string   `json:"route_id"`
	Origin      string   `json:"origin"`
	DepartsAt   string   `json:"departs_at"`
	Dest        string   `json:"dest"`
	ArrivesAt   string   `json:"arrives_at"`
	DurationMin int      `json:"duration_min"`
	Via         []string `json:"via"`
	StopCount   int      `json:"stop_count"`
}

// PlanMeta is the honesty layer: coverage scope, why a result is empty, and
// which stops make a ONE-transfer route possible (empty candidates = no
// single-transfer route — reason multi-transfer or other modes).
type PlanMeta struct {
	From               string   `json:"from"`
	To                 string   `json:"to"`
	Mode               string   `json:"mode"`
	Time               string   `json:"time"`
	Service            string   `json:"service"`
	Scope              string   `json:"scope,omitempty"`
	Note               string   `json:"note,omitempty"`
	TransferCandidates []string `json:"transfer_candidates,omitempty"`
}

// PlanResult is the planner answer.
type PlanResult struct {
	Options []PlanOption `json:"data"`
	Meta    PlanMeta     `json:"meta"`
}

// PlanTrip answers "how do I get from A to B" — direct, one-train options
// only. depart_at ranks earliest-first; arrive_by ranks latest-that-makes-
// it first (minimize waiting). Empty results are a contracted answer, not
// an error.
func (s *Store) PlanTrip(q PlanQuery) (PlanResult, error) {
	departAt, arriveBy := q.DepartAt != "", q.ArriveBy != ""
	if departAt == arriveBy { // both set, or neither
		return PlanResult{}, fmt.Errorf("%w: exactly one of depart_at or arrive_by is required", ErrBadQuery)
	}
	mode, rawTime := "depart_at", q.DepartAt
	if arriveBy {
		mode, rawTime = "arrive_by", q.ArriveBy
	}
	timeQ, err := parseServiceTime(rawTime)
	if err != nil {
		return PlanResult{}, err
	}

	from, err := s.ResolveStation(q.From)
	if err != nil {
		return PlanResult{}, err
	}
	to, err := s.ResolveStation(q.To)
	if err != nil {
		return PlanResult{}, err
	}
	if from == to {
		return PlanResult{}, fmt.Errorf("%w: from and to are the same station (%s)", ErrBadQuery, from)
	}

	meta := PlanMeta{From: from, To: to, Mode: mode, Time: timeQ,
		Service: "merged_day_types"} // feed merges day types; verify critical trips

	limit := q.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	// collect candidates: every trip visiting `from` before `to`
	type cand struct {
		opt      PlanOption
		fromRefs stopRef
		toSeq    int
	}
	var cands []cand
	for _, ref := range s.stopIndex[from] {
		trip := s.trips[ref.tripID]
		var toSeq int = -1
		for _, st := range trip.Stops {
			if st.StopID == to && st.Seq > ref.seq {
				toSeq = st.Seq
				break
			}
		}
		if toSeq < 0 {
			continue
		}
		var dep, arr string
		for _, st := range trip.Stops {
			switch st.Seq {
			case ref.seq:
				dep = st.Departure
			case toSeq:
				arr = st.Arrival
			}
		}
		if mode == "depart_at" && dep < timeQ {
			continue
		}
		if mode == "arrive_by" && arr > timeQ {
			continue
		}
		via := make([]string, 0, len(trip.Stops))
		for _, st := range trip.Stops {
			if st.Seq > ref.seq && st.Seq < toSeq {
				via = append(via, st.StopID)
			}
		}
		cands = append(cands, cand{
			opt: PlanOption{
				TrainID:     trip.ID,
				Headsign:    trip.Headsign,
				RouteID:     trip.RouteID,
				Origin:      from,
				DepartsAt:   dep,
				Dest:        to,
				ArrivesAt:   arr,
				DurationMin: minutesOf(arr) - minutesOf(dep),
				Via:         via,
				StopCount:   toSeq - ref.seq + 1,
			},
			fromRefs: ref,
			toSeq:    toSeq,
		})
	}

	if len(cands) == 0 {
		// honest empty: explain, then compute the single-transfer candidates
		// (stops directly reachable from origin ∩ stops with direct onward
		// service to the destination — the in-memory self-join)
		meta.Note = "no direct trip found"
		reachable := map[string]bool{}
		for _, ref := range s.stopIndex[from] {
			for _, st := range s.trips[ref.tripID].Stops {
				if st.Seq > ref.seq {
					reachable[st.StopID] = true
				}
			}
		}
		connecting := map[string]bool{}
		for _, ref := range s.stopIndex[to] {
			for _, st := range s.trips[ref.tripID].Stops {
				if st.Seq < ref.seq {
					connecting[st.StopID] = true
				}
			}
		}
		for stop := range reachable {
			if stop != from && stop != to && connecting[stop] {
				meta.TransferCandidates = append(meta.TransferCandidates, stop)
			}
		}
		sort.Strings(meta.TransferCandidates)
		return PlanResult{Options: []PlanOption{}, Meta: meta}, nil
	}

	sort.Slice(cands, func(i, j int) bool {
		if mode == "depart_at" {
			return cands[i].opt.DepartsAt < cands[j].opt.DepartsAt
		}
		return cands[i].opt.ArrivesAt > cands[j].opt.ArrivesAt
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	options := make([]PlanOption, len(cands))
	for i := range cands {
		options[i] = cands[i].opt
	}
	return PlanResult{Options: options, Meta: meta}, nil
}
