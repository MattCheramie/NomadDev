MODULE  := github.com/mattcheramie/nomaddev
BIN_DIR := bin

.PHONY: build build-docker run test test-race test-docker lint fmt vet tidy clean ci

build:
	go build -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-docker:
	go build -tags docker -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -tags docker -o $(BIN_DIR)/sandbox ./cmd/sandbox
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient

test-docker:
	go test -tags docker -race -count=1 ./internal/sandbox/...

run: build
	./$(BIN_DIR)/orchestrator

test:
	go test ./...

test-race:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

ci: vet test-race
