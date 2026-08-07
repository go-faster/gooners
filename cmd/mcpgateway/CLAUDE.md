# cmd/mcpgateway

The gateway binary: flag parsing, config loading, telemetry and transport wiring. Everything about
how the gateway itself behaves — startup ordering, config reload, `blob_share` — lives with the
implementation in `internal/gateway/CLAUDE.md`.

## Build

```bash
go build ./cmd/mcpgateway
./mcpgateway -config gateway.toml
```

`cmd/mcpgateway/README.md` is the operator-facing documentation and is the file to update when a
flag or a config section changes.
