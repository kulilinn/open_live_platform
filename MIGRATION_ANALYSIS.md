# LiveMedia Rust -> Go migration analysis

## 1. Source architecture

LiveMedia is a Rust workspace with these runtime surfaces:

- `api` is the main HTTP API. It owns authentication, user/session state, live-room HTTP endpoints, feed/message, gifts, subscriptions, upload, payment, notifications, DM, and internal/admin routes. It builds a shared `AppState` around MySQL, Redis, S3, Kafka, etcd configuration, rate limiting, caches, and RPC.
- `chat` is the live-room service. It keeps room actors in memory, tracks members/admins/muted/kicked users, routes join/leave/message/status operations, and exposes the service RPC endpoint.
- `agent` is the WebSocket relay. It authenticates a token/signature pair, upgrades `/api/connector/{token}/{sig}`, then proxies device/H5 connections and room events.
- `service` is the internal RPC and service-discovery layer. It uses Zenoh liveness registration plus HTTP JSON calls with scheduling methods such as fixed, modulus, hash, and round-robin.
- `chatbot` reacts to live lifecycle commands and gifts. It updates room member state, sends welcome/gift actions, and watches etcd-backed config.
- `admin` and `admin_mcn` expose management APIs for users, rooms, gifts, payments, streamer/agency data, roles, menus, and statistics.
- `edge` consumes Kafka events and calls internal services. `cache`, `entity`, `protocol`, `utils`, `push`, `migration`, and generated crates provide persistence, protocols, utility, and deployment support.

## 2. Main business flows

1. HTTP client signs up/signs in. The API issues a token and stores a session.
2. A creator starts a live room. The live service creates room state and marks it online.
3. Audience joins or leaves the room. The service updates members and publishes room events.
4. Clients connect to the WebSocket relay. The relay validates token/signature and forwards chat and control messages.
5. Room chat and lifecycle events are published to subscribers. Gifts and chatbot commands are consumed asynchronously.
6. Admin APIs inspect and mutate users, rooms, gifts, and platform settings.
7. Edge consumers receive Kafka events and dispatch them to the platform.

## 3. Go migration scope

The Go implementation keeps the primary runtime path small and explicit:

- `cmd/platform`: one process that serves the public API, live-room endpoints, admin endpoints, edge event endpoint, WebSocket relay, and a separate internal gRPC listener.
- `internal/auth`: in-memory users and bearer sessions with a simple token format.
- `internal/live`: in-memory room state with start, close, join, leave, info, members, admin, mute, kick, and chat operations.
- `internal/store/postgres`: durable accounts, sessions, room state, moderation, and chat messages. The app selects this backend when `DATABASE_URL` is set. WebSocket subscriptions remain process-local.
- `api/platform/v1/platform.proto`: `platform.v1.PlatformService/GetStats` accepts an empty request and returns the account count (`users`) and full room list (`rooms`).
- `internal/relay`: WebSocket authentication and room message forwarding.
- `internal/admin`: compact management handlers backed by the same live/auth state.
- `internal/edge`: event ingestion and dispatch hooks.

The Go migration replaces `/internal/rpc/platform.stats` and the custom JSON RPC client and registry with gRPC. `PLATFORM_GRPC_ADDR` configures the listener, which defaults to `127.0.0.1:9090`. Frontend clients continue to use HTTP and WebSocket. The Go service has no interoperability with the Rust Zenoh service layer.

The Go service uses a PostgreSQL schema for its core tables. The Rust application's MySQL data and migration history require a separate import. The Redis, etcd, S3, Kafka, Agora and payment integrations are outside this port.

## 4. Deliberate implementation style

The port uses direct handlers, concrete structs, and minimal branching. The internal stats service uses generated gRPC bindings. The port does not recreate Rust's error taxonomy, middleware stack, generated entities, or every legacy route. Unsupported legacy areas return a compact JSON response from a single fallback route.
