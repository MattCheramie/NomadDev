MODULE       := github.com/mattcheramie/nomaddev
BIN_DIR      := bin
NPM          := npm
EXPO         := npx expo
SPA_DIST     := internal/wsserver/dist
ANDROID_DIR  := build/android
ANDROID_APK  := $(ANDROID_DIR)/nomaddev.apk
ANDROID_APP_ID := dev.nomaddev.mobile
# gogio is installed under $GOPATH/bin by `go install gioui.org/cmd/gogio@latest`.
# Make picks it up from PATH if present, otherwise from go env GOPATH.
GOGIO        := $(shell command -v gogio || echo $$(go env GOPATH)/bin/gogio)

# GO_PACKAGES filters out mobile/node_modules/ — npm pulls a flatted package
# that ships a stray flatted.go file, which Go's recursive `./...` walk would
# otherwise discover and try to test.
GO_PACKAGES = $(shell go list ./... | grep -v '/mobile/node_modules/')

.PHONY: build build-docker build-gemini build-openai build-anthropic build-github build-all run test test-race \
        test-docker test-gemini test-openai test-anthropic test-github test-github-live \
        lint fmt vet tidy clean ci \
        build-mobile build-full dev-mobile clean-mobile test-mobile \
        android-tools android-debug android-install android-release android-debug-keystore android-clean \
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

# build-openai also enables runtime=deepseek (DeepSeek shares the OpenAI client).
build-openai:
	go build -tags openai -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-anthropic:
	go build -tags anthropic -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-github:
	go build -tags github -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
	go build -o $(BIN_DIR)/wsclient ./cmd/wsclient
	go build -o $(BIN_DIR)/sandbox ./cmd/sandbox

build-all:
	go build -tags "docker gemini openai anthropic github" -o $(BIN_DIR)/orchestrator ./cmd/orchestrator
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

test-openai:
	go test -tags openai -race -count=1 ./internal/middleware/...

test-anthropic:
	go test -tags anthropic -race -count=1 ./internal/middleware/...

test-github:
	go test -tags github -race -count=1 ./internal/githubmcp/...

# Opt-in live round-trip against a real github-mcp-server subprocess.
# Requires NOMADDEV_GITHUB_TOKEN exported and the upstream binary on PATH
# (or NOMADDEV_GITHUB_MCP_BIN). Skips silently otherwise — safe for CI.
test-github-live:
	go test -tags github -race -count=1 -run TestLive ./internal/githubmcp/...

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

# ---------------------------------------------------------------------------
# Native Go mobile app (Gio + gogio).
#
# Phase M1 ships the foundation: a placeholder Gio app at cmd/nomaddev-mobile
# that builds into a real APK for sideload onto an Android device. Subsequent
# milestones port the React Native screens (Onboard, Chat, Settings, Config).
#
# Requirements for `android-debug` to succeed:
#   * Go toolchain (Go 1.25+).
#   * gogio   — `make android-tools` installs it.
#   * Android SDK + NDK reachable via $ANDROID_SDK_ROOT (or $ANDROID_HOME).
#     The SDK needs platform 34+ and build-tools 34+; the NDK needs r25+.
#   * JDK 17+ on PATH (Gradle still ships with gogio in v0.10).
#
# On CI the Android SDK is provisioned by android-actions/setup-android.
# ---------------------------------------------------------------------------
android-tools:
	go install gioui.org/cmd/gogio@v0.10.0

android-debug:
	@mkdir -p $(ANDROID_DIR)
	$(GOGIO) -target android -arch arm64,arm \
	    -appid $(ANDROID_APP_ID) \
	    -version 0.1.0.1 \
	    -schemes nomaddev \
	    -o $(ANDROID_APK) \
	    ./cmd/nomaddev-mobile

android-install: android-debug
	adb install -r $(ANDROID_APK)

# android-release builds a signed APK ready to attach to a GitHub Release.
# Required env vars (matched on the gogio flags below):
#
#   ANDROID_KEYSTORE        — path to the JKS / PKCS12 keystore file
#   ANDROID_KEYSTORE_PASS   — keystore password (or set GOGIO_SIGNPASS)
#   ANDROID_VERSION         — semver+versioncode, e.g. 0.1.0.1
#                             (defaults to 0.1.0.1 when unset, but every
#                             release must bump the trailing integer)
#
# Use `make android-debug-keystore` to generate a throwaway keystore for
# local smoke tests; the real release keystore is provisioned by
# infrastructure and never committed.
ANDROID_VERSION ?= 0.1.0.1
ANDROID_RELEASE_APK := $(ANDROID_DIR)/nomaddev-release.apk

android-release:
	@if [ -z "$$ANDROID_KEYSTORE" ]; then \
	  echo "android-release: ANDROID_KEYSTORE must point at a JKS / PKCS12 file" >&2; \
	  exit 2; \
	fi
	@if [ ! -f "$$ANDROID_KEYSTORE" ]; then \
	  echo "android-release: ANDROID_KEYSTORE=$$ANDROID_KEYSTORE does not exist" >&2; \
	  exit 2; \
	fi
	@mkdir -p $(ANDROID_DIR)
	GOGIO_SIGNPASS="$$ANDROID_KEYSTORE_PASS" $(GOGIO) -target android \
	    -arch arm64,arm \
	    -appid $(ANDROID_APP_ID) \
	    -version $(ANDROID_VERSION) \
	    -schemes nomaddev \
	    -signkey "$$ANDROID_KEYSTORE" \
	    -o $(ANDROID_RELEASE_APK) \
	    ./cmd/nomaddev-mobile

# android-debug-keystore generates a throwaway PKCS12 keystore at
# build/android/debug.keystore with the password "debug". Never use this
# for a real release — the password is published in this Makefile.
android-debug-keystore:
	@mkdir -p $(ANDROID_DIR)
	keytool -genkey -v \
	    -keystore $(ANDROID_DIR)/debug.keystore \
	    -storetype PKCS12 \
	    -storepass debug \
	    -alias nomaddev \
	    -keyalg RSA -keysize 2048 -validity 10000 \
	    -dname "CN=NomadDev Debug, OU=mobile, O=NomadDev, L=Unknown, S=Unknown, C=US"

android-clean:
	rm -rf $(ANDROID_DIR)
