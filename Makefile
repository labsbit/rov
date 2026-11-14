.PHONY: build install test test-docker lint update-golden

build:            ## build ./rov
	go build -o rov ./cmd/rov

install:          ## go install into $(go env GOBIN) or ~/go/bin
	go install ./cmd/rov

test:             ## unit + integration (real tmux on a throwaway socket; skipped without tmux)
	go test ./...

test-docker:      ## also the docker tests (needs a docker daemon)
	go test -tags dockertest ./...

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...

update-golden:    ## rewrite golden files
	go test ./internal/cli ./internal/configedit ./internal/tui -update
