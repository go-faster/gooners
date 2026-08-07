package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// newBlobGateway builds a gateway with a blob store over dir, mounted under
// prefix as an upstream would report it.
func newBlobGateway(t *testing.T, dir, prefix string) *Gateway {
	t.Helper()

	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://gw.invalid/blob",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)

	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
		Blob: BlobConfig{
			Addr:   ":8090",
			Mounts: []BlobMountConfig{{Name: "example-mcp", Dir: dir, Prefix: prefix}},
		},
	}
	g, err := New(cfg, Options{Blob: store})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(t.Context()) })
	return g
}

// TestBlobShare is the shared-volume case: an upstream writes a file and
// reports its own path, and the gateway turns that path into a URL.
func TestBlobShare(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out.png"), []byte("image"), 0o600))

	// The upstream's namespace, not the gateway's: the same volume is bound at
	// a different path on each side.
	g := newBlobGateway(t, dir, "/var/lib/example-mcp")

	res, out, err := g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{
		Path: "/var/lib/example-mcp/out.png",
	})
	require.NoError(t, err)
	require.Equal(t, "example-mcp", out.Mount)
	require.Equal(t, "out.png", out.Name)
	require.Equal(t, int64(len("image")), out.Size)
	require.NotEmpty(t, out.ExpiresAt)
	require.Contains(t, out.URL, "http://gw.invalid/blob/")
	// The id is what another server reads the object by, so it is reported
	// alongside the URL rather than only inside it.
	require.NotEmpty(t, out.Blob)
	require.Contains(t, out.URL, out.Blob)

	link, ok := res.Content[0].(*mcp.ResourceLink)
	require.True(t, ok, "got %T", res.Content[0])
	require.Equal(t, out.URL, link.URI)
	require.Equal(t, "image/png", link.MIMEType)

	// The file was not moved or copied out of the volume.
	require.FileExists(t, filepath.Join(dir, "out.png"))
}

func TestBlobShareRejects(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "deep.txt"), []byte("x"), 0o600))
	g := newBlobGateway(t, dir, "/var/lib/example-mcp")

	for _, tt := range []struct {
		name string
		path string
		want string
	}{
		{name: "Empty", path: "", want: "path is required"},
		{name: "OutsideEveryMount", path: "/etc/passwd", want: "not inside any shared directory"},
		{name: "TheMountItself", path: "/var/lib/example-mcp", want: "not inside any shared directory"},
		{name: "Traversal", path: "/var/lib/example-mcp/../../../etc/passwd", want: "not inside any shared directory"},
		{name: "Missing", path: "/var/lib/example-mcp/nope.txt", want: "share"},
		{name: "Directory", path: "/var/lib/example-mcp/sub", want: "directory"},
		{name: "PrefixLookalike", path: "/var/lib/example-mcp-other/x", want: "not inside any shared directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{Path: tt.path})
			require.ErrorContains(t, err, tt.want)
		})
	}

	// A path nested inside the mount is fine; only escapes are not.
	t.Run("NestedIsAllowed", func(t *testing.T) {
		_, out, err := g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{
			Path: "/var/lib/example-mcp/sub/deep.txt",
		})
		require.NoError(t, err)
		require.Equal(t, "deep.txt", out.Name)
	})
}

// TestBlobShareSymlinkOutOfMount: the prefix match is textual, so the
// filesystem is what actually stops a symlink leading out of the volume.
func TestBlobShareSymlinkOutOfMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.txt")))

	g := newBlobGateway(t, dir, "/var/lib/example-mcp")

	_, _, err := g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{
		Path: "/var/lib/example-mcp/link.txt",
	})
	// The message names the mount, so an operator can see which one refused.
	require.ErrorContains(t, err, "example-mcp")
}

// TestBlobShareUnregisteredWithoutStore: no store means no tool, and the name
// stays available to an upstream.
func TestBlobShareUnregisteredWithoutStore(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
	}
	g, err := New(cfg, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	require.False(t, g.reservedTool(blobShareName))

	// With a store it is reserved, so an upstream tool of the same name is
	// skipped rather than shadowing the gateway's.
	withStore := newBlobGateway(t, t.TempDir(), "/var/lib/example-mcp")
	require.True(t, withStore.reservedTool(blobShareName))
}

// TestBlobShareDiscoveryToolsStayIndependent: reserving blob_share must not
// reserve the discovery names on a gateway that has no discovery.
func TestBlobShareDiscoveryToolsStayIndependent(t *testing.T) {
	g := newBlobGateway(t, t.TempDir(), "/var/lib/example-mcp")

	require.True(t, g.reservedTool(blobShareName))
	require.False(t, g.reservedTool(searchToolsName))
	require.False(t, g.reservedTool(describeToolsName))
}

func TestMatchBlobMount(t *testing.T) {
	mounts := newBlobMounts([]BlobMountConfig{
		{Name: "outer", Dir: "/srv/outer", Prefix: "/data"},
		{Name: "inner", Dir: "/srv/inner", Prefix: "/data/inner"},
		{Name: "trailing", Dir: "/srv/trailing", Prefix: "/logs/"},
		{Name: "implicit", Dir: "/srv/implicit"},
	})

	for _, tt := range []struct {
		name      string
		path      string
		wantMount string
		wantRel   string
	}{
		{name: "Outer", path: "/data/a.txt", wantMount: "outer", wantRel: "a.txt"},
		// The nested mount wins, so overlapping mounts resolve to the most
		// specific one rather than to whichever was configured first.
		{name: "InnerWins", path: "/data/inner/a.txt", wantMount: "inner", wantRel: "a.txt"},
		{name: "TrailingSlashPrefix", path: "/logs/x.log", wantMount: "trailing", wantRel: "x.log"},
		{name: "PrefixDefaultsToDir", path: "/srv/implicit/x", wantMount: "implicit", wantRel: "x"},
		{name: "Nested", path: "/data/a/b/c.txt", wantMount: "outer", wantRel: "a/b/c.txt"},
		{name: "Uncleaned", path: "/data//a/./c.txt", wantMount: "outer", wantRel: "a/c.txt"},
		{name: "Unmatched", path: "/other/a.txt"},
		{name: "MountItself", path: "/data"},
		{name: "Lookalike", path: "/database/a.txt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, rel, ok := matchBlobMount(mounts, tt.path)
			if tt.wantMount == "" {
				require.False(t, ok)
				return
			}
			require.True(t, ok)
			require.Equal(t, tt.wantMount, m.name)
			require.Equal(t, tt.wantRel, rel)
		})
	}
}

// absDir makes a slash-rooted mount dir absolute for the platform the test runs
// on. A mount's dir is a path on the gateway's own host and is validated with
// [filepath.IsAbs], which on Windows means "fully qualified": /srv/m has no
// volume there, so it names the current drive's root and is reported relative
// (golang/go#8145, golang/go#56217 — working as intended, and frozen by the Go 1
// compatibility promise). A mount's prefix is the upstream's slash-separated
// namespace, which no filepath rule applies to, and stays as written.
//
// The volume is taken from a real path rather than hardcoded, since nothing
// says CI runs on C:. [filepath.Join] cannot do this — it strips the leading
// separator and would append to the whole working directory.
func absDir(p string) string {
	return filepath.VolumeName(os.TempDir()) + p
}

func TestBlobConfigValidate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     BlobConfig
		wantErr string
	}{
		{name: "Disabled"},
		{
			name:    "MountsWithoutAddr",
			cfg:     BlobConfig{Mounts: []BlobMountConfig{{Name: "m", Dir: absDir("/srv/m")}}},
			wantErr: "nothing would serve them",
		},
		{
			name: "Valid",
			cfg:  BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{{Name: "m", Dir: absDir("/srv/m")}}},
		},
		{
			name:    "BadTTL",
			cfg:     BlobConfig{Addr: ":8090", TTL: "soon"},
			wantErr: "ttl",
		},
		{
			name:    "NoMountName",
			cfg:     BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{{Dir: absDir("/srv/m")}}},
			wantErr: "name is required",
		},
		{
			name: "DuplicateMountName",
			cfg: BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{
				{Name: "m", Dir: absDir("/srv/a")}, {Name: "m", Dir: absDir("/srv/b")},
			}},
			wantErr: "duplicated",
		},
		{
			name:    "NoMountDir",
			cfg:     BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{{Name: "m"}}},
			wantErr: "dir is required",
		},
		{
			name:    "RelativeMountDir",
			cfg:     BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{{Name: "m", Dir: "srv/m"}}},
			wantErr: "must be absolute",
		},
		{
			name:    "RelativePrefix",
			cfg:     BlobConfig{Addr: ":8090", Mounts: []BlobMountConfig{{Name: "m", Dir: absDir("/srv/m"), Prefix: "rel"}}},
			wantErr: "must be absolute",
		},
		{
			name: "S3",
			cfg: BlobConfig{
				S3:     BlobS3Config{Endpoint: "minio:9000", Bucket: "b"},
				Mounts: []BlobMountConfig{{Name: "m", Dir: absDir("/srv/m")}},
			},
		},
		{
			name:    "S3WithoutBucket",
			cfg:     BlobConfig{S3: BlobS3Config{Endpoint: "minio:9000"}},
			wantErr: "needs s3.bucket",
		},
		{
			name:    "S3AndAddr",
			cfg:     BlobConfig{Addr: ":8090", S3: BlobS3Config{Endpoint: "minio:9000", Bucket: "b"}},
			wantErr: "two different backends",
		},
		{
			name:    "S3AndDir",
			cfg:     BlobConfig{Dir: "/var/tmp/gw", S3: BlobS3Config{Endpoint: "minio:9000", Bucket: "b"}},
			wantErr: "two different backends",
		},
		{
			// s3.bucket alone does not select the backend, so it is not half a
			// configuration; it is a section that does nothing.
			name:    "S3BucketWithoutEndpoint",
			cfg:     BlobConfig{S3: BlobS3Config{Bucket: "b"}, Mounts: []BlobMountConfig{{Name: "m", Dir: absDir("/srv/m")}}},
			wantErr: "nothing would serve them",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestBlobReloadSwapsMounts: which directories are shared is config, so it
// reloads; where the store listens is not, so it does not.
func TestBlobReloadSwapsMounts(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(second, "new.txt"), []byte("x"), 0o600))

	g := newBlobGateway(t, first, "/var/lib/first")

	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
		Blob: BlobConfig{
			Addr:   ":8090",
			Mounts: []BlobMountConfig{{Name: "second", Dir: second, Prefix: "/var/lib/second"}},
		},
	}
	res, err := g.Reload(t.Context(), next)
	require.NoError(t, err)
	require.NotContains(t, res.RestartRequired, "blob", "only the mount list changed")

	_, out, err := g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{
		Path: "/var/lib/second/new.txt",
	})
	require.NoError(t, err)
	require.Equal(t, "second", out.Mount)

	// The old mount is gone, not merely shadowed.
	_, _, err = g.handleBlobShare(t.Context(), &mcp.CallToolRequest{}, blobShareInput{
		Path: "/var/lib/first/anything.txt",
	})
	require.ErrorContains(t, err, "not inside any shared directory")
}

func TestBlobRestartRequired(t *testing.T) {
	base := BlobConfig{Addr: ":8090", BaseURL: "http://h/blob", Dir: "/var/tmp/gw", TTL: "5m"}

	for _, tt := range []struct {
		name string
		next BlobConfig
		want bool
	}{
		{name: "Same", next: base},
		{
			name: "MountsOnly",
			next: BlobConfig{
				Addr: base.Addr, BaseURL: base.BaseURL, Dir: base.Dir, TTL: base.TTL,
				Mounts: []BlobMountConfig{{Name: "m", Dir: "/srv/m"}},
			},
		},
		{name: "Addr", next: BlobConfig{Addr: ":9999", BaseURL: base.BaseURL, Dir: base.Dir, TTL: base.TTL}, want: true},
		{name: "BaseURL", next: BlobConfig{Addr: base.Addr, BaseURL: "http://other/blob", Dir: base.Dir, TTL: base.TTL}, want: true},
		{name: "Dir", next: BlobConfig{Addr: base.Addr, BaseURL: base.BaseURL, Dir: "/other", TTL: base.TTL}, want: true},
		{name: "TTL", next: BlobConfig{Addr: base.Addr, BaseURL: base.BaseURL, Dir: base.Dir, TTL: "1h"}, want: true},
		{name: "Disabled", next: BlobConfig{}, want: true},
		{
			// The bucket is authenticated against once, at startup.
			name: "S3",
			next: BlobConfig{S3: BlobS3Config{Endpoint: "minio:9000", Bucket: "b"}},
			want: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, blobStoreChanged(base, tt.next))
		})
	}
}

// TestBlobRestartRequiredS3 covers the bucket fields against each other, which
// the HTTP baseline in [TestBlobRestartRequired] cannot reach.
func TestBlobRestartRequiredS3(t *testing.T) {
	base := BlobConfig{S3: BlobS3Config{Endpoint: "minio:9000", Bucket: "b", Prefix: "tenants/alice"}}

	same := base
	require.False(t, blobStoreChanged(base, same))

	moved := base
	moved.S3.Prefix = "tenants/bob"
	require.True(t, blobStoreChanged(base, moved), "the tenancy boundary is fixed at startup")
}

// TestBlobInstructions checks what a downstream client is actually told at
// initialize: the tool description alone is not enough, because a client that
// never looks at blob_share holds a host path with no reason to look it up.
func TestBlobInstructions(t *testing.T) {
	newGateway := func(t *testing.T, withStore bool, instructions string) *Gateway {
		t.Helper()

		cfg := &Config{
			Server:    ServerConfig{Name: "gw", Instructions: instructions},
			Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
		}
		var opts Options
		if withStore {
			store, err := blob.NewHTTP(blob.HTTPOptions{
				BaseURL: "http://gw.invalid/blob",
				FS:      blob.Dir(t.TempDir()),
			})
			require.NoError(t, err)
			cfg.Blob = BlobConfig{Addr: ":8090"}
			opts.Blob = store
		}
		g, err := New(cfg, opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close(t.Context()) })
		return g
	}

	// instructionsOf is what the client sees, not what the config said.
	instructionsOf := func(t *testing.T, g *Gateway) string {
		t.Helper()

		serverTr, clientTr := mcp.NewInMemoryTransports()
		go func() { _ = g.Server().Run(t.Context(), serverTr) }()

		sess, err := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil).
			Connect(t.Context(), clientTr, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sess.Close() })
		return sess.InitializeResult().Instructions
	}

	t.Run("NoStore", func(t *testing.T) {
		got := instructionsOf(t, newGateway(t, false, "Gateway to prod."))
		require.Equal(t, "Gateway to prod.", got, "a gateway without a store must not advertise the tool")
	})
	t.Run("AppendsToOperators", func(t *testing.T) {
		got := instructionsOf(t, newGateway(t, true, "Gateway to prod."))
		require.Contains(t, got, "Gateway to prod.", "the operator's own instructions must survive")
		require.Contains(t, got, blobShareName)
	})
	t.Run("StandsAlone", func(t *testing.T) {
		got := instructionsOf(t, newGateway(t, true, ""))
		require.Equal(t, blobInstructions, got)
	})
}

// TestBlobShareDescriptionNamesMounts checks that an agent can tell which paths
// are servable from tools/list alone, and that a reload changing the mount list
// changes what it reads.
func TestBlobShareDescriptionNamesMounts(t *testing.T) {
	t.Run("Description", func(t *testing.T) {
		require.Contains(t,
			blobShareDescription(newBlobMounts([]BlobMountConfig{
				{Name: "example-mcp", Dir: "/mnt/example", Prefix: "/var/lib/example-mcp"},
			})),
			"example-mcp at /var/lib/example-mcp")
		require.Contains(t, blobShareDescription(nil), "No shared directories are configured")
	})

	t.Run("RefreshedOnReload", func(t *testing.T) {
		store, err := blob.NewHTTP(blob.HTTPOptions{
			BaseURL: "http://gw.invalid/blob",
			FS:      blob.Dir(t.TempDir()),
		})
		require.NoError(t, err)

		cfg := func(prefix string) *Config {
			return &Config{
				Server:    ServerConfig{Name: "gw"},
				Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
				Blob: BlobConfig{
					Addr:   ":8090",
					Mounts: []BlobMountConfig{{Name: "m", Dir: t.TempDir(), Prefix: prefix}},
				},
			}
		}
		g, err := New(cfg("/var/lib/first"), Options{Blob: store})
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close(t.Context()) })

		serverTr, clientTr := mcp.NewInMemoryTransports()
		go func() { _ = g.Server().Run(t.Context(), serverTr) }()
		sess, err := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil).
			Connect(t.Context(), clientTr, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sess.Close() })

		descriptionOf := func(t *testing.T) string {
			t.Helper()

			list, err := sess.ListTools(t.Context(), &mcp.ListToolsParams{})
			require.NoError(t, err)
			for _, tool := range list.Tools {
				if tool.Name == blobShareName {
					return tool.Description
				}
			}
			t.Fatalf("%s is not in tools/list", blobShareName)
			return ""
		}
		require.Contains(t, descriptionOf(t), "/var/lib/first")

		_, err = g.Reload(t.Context(), cfg("/var/lib/second"))
		require.NoError(t, err)
		got := descriptionOf(t)
		require.Contains(t, got, "/var/lib/second")
		require.NotContains(t, got, "/var/lib/first", "the old prefix must not still be advertised")
	})
}
