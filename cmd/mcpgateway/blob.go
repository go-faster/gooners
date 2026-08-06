package main

import (
	"context"
	"log/slog"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/gateway"
)

// blobStoreFor builds the store behind blob_share, plus the function serving
// it. With no [blob] section it returns a store that refuses, so the tool is
// left unregistered rather than present and useless.
func blobStoreFor(cfg *gateway.Config, lg *slog.Logger) (blob.Attacher, func(context.Context) error, error) {
	if !cfg.Blob.Enabled() {
		return nil, func(context.Context) error { return nil }, nil
	}
	ttl, err := cfg.Blob.TTLDuration()
	if err != nil {
		return nil, nil, errors.Wrap(err, "ttl")
	}
	return cmdutil.BlobFlags{
		BaseURL: cfg.Blob.BaseURL,
		Addr:    cfg.Blob.Addr,
		Dir:     cfg.Blob.Dir,
		TTL:     ttl,
	}.Setup(cmdutil.BlobOptions{
		Name:   "mcpgateway",
		Logger: lg.With("component", "blob"),
	})
}
