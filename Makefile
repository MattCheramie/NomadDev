MODULE  := github.com/mattcheramie/nomaddev
BIN_DIR := bin

.PHONY: build run test test-race lint fmt vet tidy clean ci

build:
	go build -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient

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
