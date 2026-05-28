# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

`github.com/go-faster/gooners` is a monorepo for AI tools, skills, and MCP (Model Context Protocol) servers built on go-faster technologies.

## Module

```
module github.com/go-faster/gooners
go 1.26.3
```

## Commands

```bash
# Build all packages
go build ./...

# Run all tests
go test ./...

# Run a single test
go test ./path/to/pkg -run TestName

# Lint and format
golangci-lint fmt ./...
golangci-lint run ./...
```

## Go Standards

- Use `golangci-lint fmt ./...` for formatting (not `gofmt` or `goimports` directly).
- When using `ogen`-generated HTTP clients/servers, prefer a single `default` response over multiple error codes — required for the `convenient errors` feature.
- Avoid loading generated files into context; use search tools or gopls instead.
