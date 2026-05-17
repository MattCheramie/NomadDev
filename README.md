# ⚡ NomadDev

NomadDev is an experimental, mobile-first remote execution environment. It provides a secure, natural-language-driven interface for managing remote servers, testing code, and orchestrating containers from your phone without exposing an SSH port or relying on messy terminal emulators.

By combining mesh networking, ephemeral container sandboxing, and LLM-driven RPC mapping, NomadDev allows you to interact with a headless VPS daemon securely and seamlessly.

## 🏗️ Architecture

The system is built on a "local-first" philosophy extended to remote infrastructure. Data and execution remain strictly within your private mesh network. 

The architecture is divided into five modular, decoupled components:

1. **The Secure Mesh (Connectivity):** A Tailscale overlay network ensuring the remote host and mobile client communicate exclusively over a private IP range.
2. **The Orchestrator Daemon (Backend):** A lightweight, concurrent WebSocket server written in Go that acts as the central nervous system, handling secure client connections and job routing.
3. **The Ephemeral Sandbox (Worker):** A Go-based wrapper around the Docker SDK (utilizing gVisor/MicroVMs) to safely execute arbitrary code and system commands in total isolation.
4. **The NLP-to-RPC Middleware (Logic):** A translation layer that utilizes the Google GenAI SDK to map natural language requests to predefined JSON schemas and remote procedure calls (RPC).
5. **The Control Hub (Client):** A React Native mobile application that consumes JSON event streams to render a clean, native UI instead of raw terminal output.

---

## 🗺️ Project Roadmap

### Phase 1: Mesh & Foundation
*Objective: Establish secure, passwordless communication between devices.*
- [ ] Configure host VPS with Ubuntu 24.04.
- [ ] Install and configure Tailscale subnet routing.
- [ ] Verify ICMP and basic TCP packet transmission exclusively over the Tailscale IP range.
- [ ] Disable public SSH access on the host (port 22).

Design notes and a non-destructive provisioning checklist live at
[`infra/`](./infra/) — see `infra/README.md` and `infra/scripts/provision.sh`.

### Phase 2: Headless Orchestrator (Go) — done
*Objective: Build the core message relay system.*
- [x] Initialize the Go module and set up a basic TCP listener.
- [x] Implement a WebSocket server utilizing `gorilla/websocket`.
- [x] Create a standard JSON event structure for inbound/outbound payloads.
- [x] Implement JWT-based authentication to reject unauthorized WebSocket connections.
- [x] Build a robust logging and state-recovery mechanism for dropped connections.

Implementation lives under [`cmd/orchestrator`](./cmd/orchestrator/) and
[`internal/`](./internal/). See [`docs/architecture.md`](./docs/architecture.md),
[`docs/events.md`](./docs/events.md), and [`docs/auth.md`](./docs/auth.md).

### Phase 3: Ephemeral Sandbox Runner
*Objective: Safely execute commands and capture outputs without risking the host system.*
- [x] Integrate the official Docker SDK for Go.
- [x] Create a function to dynamically pull and spin up lightweight worker images (e.g., Alpine or Ubuntu).
- [x] Implement secure volume bind-mounts for a designated workspace directory.
- [x] Build an execution loop that runs `bash` commands inside the container and streams `stdout`/`stderr` back to the Orchestrator via channels.
- [x] Implement hard timeouts and resource limits (RAM/CPU) for the sandbox.

### Phase 4: NLP Function Middleware
*Objective: Standardize natural language into actionable system commands.*
- [ ] Integrate the Gemini API via Google AI Studio.
- [ ] Define JSON schemas for core system tools (e.g., `execute_script`, `read_file`, `write_patch`).
- [ ] Build the loop that receives user intent, queries the LLM, and captures the resulting Function Call.
- [ ] Map the generated Function Calls directly to the Go Sandbox Runner from Phase 3.
- [ ] Format execution results back into JSON for the LLM to interpret.

### Phase 5: Mobile Control Hub
*Objective: Ditch the terminal for a native, reactive mobile interface.*
- [ ] Scaffold a new React Native (or Expo) project.
- [ ] Implement a WebSocket client that connects to the Orchestrator's Tailscale IP.
- [ ] Build the main chat/event feed UI components.
- [ ] Create custom UI cards for "Action Approvals" (intercepting sensitive commands before they run).
- [ ] Implement background synchronization to fetch state history upon app resume.

---

## 🚀 Running the orchestrator (Phase 2)

```sh
export NOMADDEV_JWT_SECRET="$(head -c 48 /dev/urandom | base64 | tr -d '\n')"
make build
./bin/orchestrator -listen :8080
```

In another shell, mint a token and connect:

```sh
TOKEN="$(go run ./scripts/gen-jwt -sub matt -sid sess-1 -ttl 1h)"
./bin/wsclient -url ws://127.0.0.1:8080/ws -token "$TOKEN" -send ping
```

Run the test suite:

```sh
go test -race -count=1 ./...
```

See [`.env.example`](./.env.example) for all configuration knobs.

---

## 🛡️ Security Considerations

NomadDev is designed with paranoia as a feature. The public internet never touches the orchestrator. The LLM never touches the host system. The client never touches raw SSH. 

*   **No Open Ports:** Bypasses traditional firewall risks via Tailscale.
*   **Total Isolation:** Execution occurs entirely within ephemeral Docker containers.
*   **Human-in-the-Loop:** Destructive commands parsed by the middleware require explicit biometric approval on the mobile client.
