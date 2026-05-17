MODULE   := github.com/mattcheramie/nomaddev
BIN_DIR  := bin
NPM      := npm
EXPO     := npx expo
SPA_DIST := internal/wsserver/dist

# GO_PACKAGES filters out mobile/node_modules/ — npm pulls a flatted package
# that ships a stray flatted.go file, which Go's recursive `./...` walk would
# otherwise discover and try to test.
GO_PACKAGES = $(shell go list ./... | grep -v '/mobile/node_modules/')

.PHONY: build build-docker build-gemini build-all run test test-race test-docker test-gemini \
        lint fmt vet tidy clean ci \
        build-mobile build-full dev-mobile clean-mobile test-mobile \
        docker-image docker-up docker-down quickstart-docker quickstart-systemd \
        gen-secret

build:
	go build -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-docker:
	go build -tags docker -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -tags docker -o $(BIN_DIR)/sandbox ./cmd/sandbox
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient

build-gemini:
	go build -tags gemini -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-all:
	go build -tags "docker gemini" -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -tags docker -o $(BIN_DIR)/sandbox ./cmd/sandbox
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient

# Phase 5: SPA build pipeline.
build-mobile:
	cd mobile && $(NPM) install --no-audit --no-fund
	cd mobile && $(EXPO) export --platform web --output-dir ../$(SPA_DIST)

build-full: build-mobile build

dev-mobile:
	cd mobile && $(EXPO) start --web

test-mobile:
	cd mobile && $(NPM) test --silent

clean-mobile:
	rm -rf mobile/dist $(SPA_DIST)/_expo $(SPA_DIST)/assets $(SPA_DIST)/metadata.json $(SPA_DIST)/favicon.ico
	git checkout -- $(SPA_DIST)/index.html 2>/dev/null || true

test-docker:
	go test -tags docker -race -count=1 -timeout 180s ./internal/sandbox/...

test-gemini:
	go test -tags gemini -race -count=1 ./internal/middleware/...

run: build
	./$(BIN_DIR)/orchestrator

test:
	go test $(GO_PACKAGES)

test-race:
	go test -race -count=1 $(GO_PACKAGES)

vet:
	go vet $(GO_PACKAGES)

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

ci: vet test-race

# Phase 6 deploy artifacts. The Dockerfile is multi-stage (mobile export →
# Go build → distroless/static); docker-compose mounts a named volume for
# /var/lib/nomaddev (sessions.db, history.db, workspace).
docker-image:
	docker build -t nomaddev/orchestrator:dev .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

# Phase 6 one-command deploys. Both quickstart scripts are fully executable
# (no # TODO: discipline) — they wrap the happy path for fresh-VPS installs.
quickstart-docker:
	sudo bash infra/scripts/quickstart-docker.sh

quickstart-systemd:
	sudo bash infra/scripts/quickstart-systemd.sh

# Print a NOMADDEV_JWT_SECRET=… line backed by /dev/urandom.
gen-secret:
	@bash infra/scripts/gen-secret.sh
