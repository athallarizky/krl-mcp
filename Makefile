.PHONY: build test run-demo

build:
	go build -o krl-mcp ./cmd/krl-mcp

test:
	go test ./...

# register the locally built binary with Claude Code (repo root CWD so the
# pinned data/ resolves; `claude mcp remove krl` to clean up)
run-demo: build
	claude mcp remove krl 2>/dev/null || true
	claude mcp add krl -- "$$PWD/krl-mcp"
	@echo "registered — open a new Claude Code session and ask about trains"
