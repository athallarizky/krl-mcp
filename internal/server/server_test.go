package server_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolsEndToEnd builds the real binary and drives it over stdio with
// the official SDK client — exactly how Claude Code would run it. `go test
// ./...` needs nothing but this repository.
func TestToolsEndToEnd(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "krl-mcp")
	build := exec.Command("go", "build", "-o", bin, "./cmd/krl-mcp")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1"}, nil)
	// run the child from the repo root so the pinned data/ resolves
	cmd := exec.Command(bin)
	cmd.Dir = root
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	// --- plan_trip: the flagship flow, by station names, arrive_by mode
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "plan_trip",
		Arguments: map[string]any{
			"from": "Tanah Abang", "to": "Kebayoran", "arrive_by": "06:00", "limit": 1,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("plan_trip: %v %+v", err, res)
	}
	var plan struct {
		Data []struct {
			TrainID   string `json:"train_id"`
			ArrivesAt string `json:"arrives_at"`
		} `json:"data"`
		Meta struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plan.Meta.From != "THB" || plan.Meta.To != "KBY" || len(plan.Data) != 1 ||
		plan.Data[0].ArrivesAt > "06:00:00" {
		t.Fatalf("plan = %+v — want THB→KBY, one option arriving before 06:00", plan)
	}

	// --- get_departures by name
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_departures",
		Arguments: map[string]any{"station": "Tanah Tinggi", "from": "07:00", "to": "08:00"},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_departures: %v %+v", err, res)
	}
	var board struct {
		Data []struct {
			TrainID string `json:"train_id"`
		} `json:"data"`
		Meta struct {
			Station string `json:"station"`
			Source  string `json:"source"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &board); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if board.Meta.Station != "" || len(board.Data) == 0 {
		// meta carries the RAW input in this shape; the rows are the contract
		t.Logf("departures: %d rows", len(board.Data))
	}
	if len(board.Data) == 0 {
		t.Fatal("THI 07-08 returned zero rows")
	}

	// --- error path travels as a tool error, not a crash
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "plan_trip",
		Arguments: map[string]any{"from": "THI", "to": "THI", "depart_at": "05:00"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("same-station query should be a tool error")
	}
}
