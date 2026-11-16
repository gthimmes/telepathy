.PHONY: all ui build test run demo clean

all: build

ui:            ## build the web UI into internal/ui/dist (embedded by the Go binary)
	cd web && npm ci && npm run build

build: ui      ## build the single binary
	go build -o bin/telepathy ./cmd/telepathy

test:
	go vet ./...
	go test ./...

run: build     ## run with simulated telemetry
	./bin/telepathy serve --demo

clean:
	rm -rf bin internal/ui/dist/assets internal/ui/dist/index.html
