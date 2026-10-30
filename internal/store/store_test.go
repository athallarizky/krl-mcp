package store

import (
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot resolves the repository root regardless of the test's working
// directory — `go test ./...` works from anywhere with zero setup.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// loadOnce loads the pinned snapshot for the whole test binary.
func loadOnce(t *testing.T) *Store {
	t.Helper()
	s, err := Load(repoRoot(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

// TestLoadPinsTheSnapshot proves the artifact contract: the full catalog,
// full coordinate coverage, and every board parsed.
func TestLoadPinsTheSnapshot(t *testing.T) {
	s := loadOnce(t)

	if got := len(s.Stations()); got != 111 {
		t.Fatalf("stations = %d, want 111", got)
	}
	var withCoords int
	for _, st := range s.Stations() {
		if st.Lat != nil {
			withCoords++
		}
	}
	if withCoords != 111 {
		t.Fatalf("stations with coords = %d, want 111 (94 GTFS + 17 OSM)", withCoords)
	}
	if len(s.boards) != 93 {
		t.Fatalf("boards = %d, want 93", len(s.boards))
	}
	if len(s.trips) != 1139 {
		t.Fatalf("trips = %d, want 1139", len(s.trips))
	}
	if s.Snapshot() == "" {
		t.Fatal("snapshot date missing — manifest.json not parsed")
	}
}

// TestResolveStation exercises all four tiers, including the casual forms
// real users (and LLMs) actually type.
func TestResolveStation(t *testing.T) {
	s := loadOnce(t)
	cases := []struct{ in, want string }{
		{"THI", "THI"},          // exact code
		{"thi", "THI"},          // case-insensitive code
		{"Tanah Tinggi", "THI"}, // exact name
		{"tanah tinggi", "THI"}, // lowercase name
		{"TANAHTINGGI", "THI"},  // normalized name
	}
	for _, c := range cases {
		got, err := s.ResolveStation(c.in)
		if err != nil || got != c.want {
			t.Errorf("ResolveStation(%q) = %q, %v — want %q", c.in, got, err, c.want)
		}
	}
	if _, err := s.ResolveStation("NOSUCH"); !errors.Is(err, ErrStationNotFound) {
		t.Errorf("unknown station err = %v, want ErrStationNotFound", err)
	}
}

// TestDeparturesGolden pins the Tangerang morning board — the same numbers
// the production service asserts, now served from memory.
func TestDeparturesGolden(t *testing.T) {
	s := loadOnce(t)

	rows, err := s.Departures("TNG", "07:00", "08:00", "")
	if err != nil {
		t.Fatalf("departures: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("TNG 07-08 returned zero rows")
	}
	first := rows[0]
	if first.TrainID != "1923A" || first.DepartsAt != "07:07:00" {
		t.Fatalf("first = %+v, want 1923A 07:07:00 (TNG is the line's first station)", first)
	}
	if first.ArrivesAt == nil || *first.ArrivesAt != "07:33:00" {
		t.Fatalf("arrival = %v, want 07:33:00 (terminus Duri, 26 min)", first.ArrivesAt)
	}

	// out-of-scope station: honest empty, not an error
	rows, err = s.Departures("YK", "00:00", "23:59", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("YK (no board file) = %d rows, %v — want empty, nil", len(rows), err)
	}
}

// TestPlanTripGolden ports the production planner's verified assertions:
// both modes, the pass-through case, the no-direct contract, and the
// past-midnight Karet through-service.
func TestPlanTripGolden(t *testing.T) {
	s := loadOnce(t)

	// depart_at: first train TNG→DU after 04:00 is 1903A, 26 minutes
	res, err := s.PlanTrip(PlanQuery{From: "TNG", To: "DU", DepartAt: "04:00"})
	if err != nil || len(res.Options) == 0 {
		t.Fatalf("TNG→DU: %v, %d options", err, len(res.Options))
	}
	if o := res.Options[0]; o.TrainID != "1903A" || o.DepartsAt != "04:27:00" ||
		o.ArrivesAt != "04:53:00" || o.DurationMin != 26 || o.StopCount != 11 || len(o.Via) != 9 {
		t.Fatalf("first = %+v", o)
	}
	if res.Meta.Service != "merged_day_types" {
		t.Fatalf("service = %q", res.Meta.Service)
	}

	// arrive_by: latest THB→KBY arriving by 05:30 is 1616D at 05:25
	res, err = s.PlanTrip(PlanQuery{From: "THB", To: "KBY", ArriveBy: "05:30"})
	if err != nil || len(res.Options) == 0 {
		t.Fatalf("THB→KBY arrive_by: %v, %d options", err, len(res.Options))
	}
	if o := res.Options[0]; o.TrainID != "1616D" || o.ArrivesAt != "05:25:00" {
		t.Fatalf("first = %+v, want 1616D 05:25", o)
	}

	// no direct trip TNG→KBY: honest empty with ZERO transfer candidates
	// (no single-transfer route exists — the real route takes 2 transfers)
	res, err = s.PlanTrip(PlanQuery{From: "TNG", To: "KBY", DepartAt: "04:00"})
	if err != nil || len(res.Options) != 0 || res.Meta.Note == "" || len(res.Meta.TransferCandidates) != 0 {
		t.Fatalf("TNG→KBY: %d options, note=%q, candidates=%v, err=%v",
			len(res.Options), res.Meta.Note, res.Meta.TransferCandidates, err)
	}

	// the findings case: KRI→BPR has exactly one transfer point — Duri
	res, err = s.PlanTrip(PlanQuery{From: "KRI", To: "BPR", DepartAt: "15:00"})
	if err != nil || len(res.Meta.TransferCandidates) != 1 || res.Meta.TransferCandidates[0] != "DU" {
		t.Fatalf("KRI→BPR candidates = %v, err = %v — want [DU]", res.Meta.TransferCandidates, err)
	}

	// past midnight on the service-day clock: the Karet through-service
	res, err = s.PlanTrip(PlanQuery{From: "DU", To: "THB", DepartAt: "23:30"})
	if err != nil {
		t.Fatalf("DU→THB: %v", err)
	}
	var karet bool
	for _, o := range res.Options {
		if o.TrainID == "5752C" {
			karet = o.DepartsAt == "24:04:00" && o.ArrivesAt == "24:11:00"
		}
	}
	if !karet {
		t.Fatal("5752C (Karet, 24:04→24:11) missing from late-night options")
	}

	// validation: mode XOR and same-station
	if _, err := s.PlanTrip(PlanQuery{From: "TNG", To: "DU"}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("no mode: err = %v", err)
	}
	if _, err := s.PlanTrip(PlanQuery{From: "TNG", To: "tangerang", DepartAt: "05:00"}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("same station: err = %v", err)
	}
}

// TestNearestGolden pins the Sudirman pair ordering and the OSM-coords
// coverage on the Yogyakarta–Solo line.
func TestNearestGolden(t *testing.T) {
	s := loadOnce(t)

	near, err := s.NearestStations(-6.1990, 106.8230, 2)
	if err != nil || len(near) != 2 {
		t.Fatalf("nearest: %v, %d results", err, len(near))
	}
	if near[0].ID != "SUD" || near[1].ID != "SUDB" {
		t.Fatalf("order = %s,%s — want SUD,SUDB", near[0].ID, near[1].ID)
	}
	if math.Abs(float64(near[0].DistanceM)-382) > 50 {
		t.Fatalf("SUD distance = %d, want ~382", near[0].DistanceM)
	}

	near, err = s.NearestStations(-7.7932, 110.3665, 1)
	if err != nil || len(near) != 1 || near[0].ID != "YK" {
		t.Fatalf("Yogyakarta: %v, %+v — want YK first (OSM coords)", err, near)
	}

	if _, err := s.NearestStations(95, 0, 5); !errors.Is(err, ErrBadQuery) {
		t.Errorf("lat out of range: err = %v", err)
	}
}

// TestTripStops pins the route-narrative contract: 5133D is the train whose
// southern-ring path an LLM once narrated wrongly — the stop list IS the
// narrative.
func TestTripStops(t *testing.T) {
	s := loadOnce(t)

	d, err := s.TripStops("5133D")
	if err != nil {
		t.Fatalf("5133D: %v", err)
	}
	if len(d.Stops) != 19 || d.Headsign == "" {
		t.Fatalf("5133D = %d stops, headsign %q", len(d.Stops), d.Headsign)
	}
	var kri, mri, thb = -1, -1, -1
	for i, st := range d.Stops {
		if st.Name == "" {
			t.Fatalf("stop %s has no name joined", st.StopID)
		}
		switch st.StopID {
		case "KRI":
			kri = i
		case "MRI":
			mri = i
		case "THB":
			thb = i
		}
	}
	if !(0 <= kri && kri < mri && mri < thb) {
		t.Fatalf("southern ring order broken: kri=%d mri=%d thb=%d", kri, mri, thb)
	}

	if _, err := s.TripStops("NOSUCH"); !errors.Is(err, ErrTripNotFound) {
		t.Errorf("unknown trip: err = %v", err)
	}
}
