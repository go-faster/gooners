# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/ssh-mcp ./cmd/ssh-mcp

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S mcp \
 && adduser -S mcp -G mcp -h /home/mcp

USER mcp
WORKDIR /home/mcp

COPY --from=builder /out/ssh-mcp /usr/local/bin/ssh-mcp

# Default: stdio transport (works with most MCP clients).
# For HTTP (recommended when running in Docker):
#   docker run ... -p 8080:8080 ssh-mcp -transport streamable-http -addr :8080
ENTRYPOINT ["ssh-mcp"]
