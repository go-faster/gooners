package s3

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// TestParseID is the gate between a string the model supplied and a bucket key,
// so the cases that matter are the ones that must not parse.
func TestParseID(t *testing.T) {
	const u = "0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6"

	for _, tt := range []struct {
		name      string
		id        string
		namespace string
	}{
		{"Simple", "tgmcp/" + u, "tgmcp"},
		{"Dashes", "ssh-mcp/" + u, "ssh-mcp"},
		{"DotsAndUnderscores", "a.b_c/" + u, "a.b_c"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ns, got, err := ParseID(tt.id)
			require.NoError(t, err)
			require.Equal(t, tt.namespace, ns)
			require.Equal(t, u, got)
		})
	}

	for _, tt := range []struct {
		name string
		id   string
	}{
		{"Empty", ""},
		{"NoNamespace", u},
		{"EmptyNamespace", "/" + u},
		{"NoUUID", "tgmcp/"},
		{"NotAUUID", "tgmcp/hello"},
		{"UppercaseUUID", "tgmcp/0192F4A0-1B2C-4D3E-8F90-A1B2C3D4E5F6"},
		{"UUIDWithoutDashes", "tgmcp/0192f4a01b2c4d3e8f90a1b2c3d4e5f6"},
		{"Traversal", "../" + u},
		{"TraversalInNamespace", "a/../../b/" + u},
		{"NestedNamespace", "a/b/" + u},
		{"Absolute", "/tgmcp/" + u},
		{"TrailingSlash", "tgmcp/" + u + "/"},
		{"TrailingComponent", "tgmcp/" + u + "/etc/passwd"},
		{"LeadingDot", ".hidden/" + u},
		{"Newline", "tgmcp/" + u + "\n"},
		{"URL", "https://example.com/tgmcp/" + u},
	} {
		t.Run("Rejects"+tt.name, func(t *testing.T) {
			_, _, err := ParseID(tt.id)
			require.Error(t, err, "id %q must not parse", tt.id)
		})
	}
}

// TestKeyStaysUnderPrefix is the tenancy invariant: whatever an id says, the key
// it produces is inside the store's own prefix.
func TestKeyStaysUnderPrefix(t *testing.T) {
	const u = "0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6"
	s := &Store{prefix: "tenants/alice"}

	key, err := s.parse("tgmcp/" + u)
	require.NoError(t, err)
	require.Equal(t, "tenants/alice/tgmcp/"+u, key)

	// An id shaped like another tenant's key is not a key at all.
	for _, id := range []string{
		"../bob/tgmcp/" + u,
		"tenants/bob/" + u,
		"/tenants/bob/tgmcp/" + u,
	} {
		_, err := s.parse(id)
		require.Error(t, err, "id %q must not resolve to a key", id)
	}
}

func TestWithTenant(t *testing.T) {
	const u = "0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6"
	alice := &Store{prefix: "tenants/alice", namespace: "gw"}
	bob := alice.WithTenant("tenants/bob")

	require.Equal(t, "tenants/alice", alice.prefix, "the original is untouched")
	require.Equal(t, "tenants/bob", bob.prefix)
	require.Equal(t, alice.namespace, bob.namespace)

	aliceKey, err := alice.parse("tgmcp/" + u)
	require.NoError(t, err)
	bobKey, err := bob.parse("tgmcp/" + u)
	require.NoError(t, err)
	require.NotEqual(t, aliceKey, bobKey, "the same id names a different object per tenant")
}

func TestCleanPrefix(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{".", ""},
		{"blobs", "blobs"},
		{"/blobs", "blobs"},
		{"blobs/", "blobs"},
		{"/blobs/", "blobs"},
		{"tenants//alice", "tenants/alice"},
		{"tenants/./alice", "tenants/alice"},
	} {
		require.Equal(t, tt.want, cleanPrefix(tt.in), "cleanPrefix(%q)", tt.in)
	}
}

func TestNewIDIsScopedAndParses(t *testing.T) {
	s := &Store{namespace: "tgmcp"}
	id, err := s.newID()
	require.NoError(t, err)

	ns, _, err := ParseID(id)
	require.NoError(t, err)
	require.Equal(t, "tgmcp", ns, "an id names the server that minted it")

	other, err := s.newID()
	require.NoError(t, err)
	require.NotEqual(t, id, other)
}

func TestOptionsValidate(t *testing.T) {
	base := func() Options {
		return Options{Endpoint: "s3.example.com", Bucket: "b", Namespace: "tgmcp"}
	}

	t.Run("Defaults", func(t *testing.T) {
		o := base()
		require.NoError(t, o.setDefaults())
		require.Equal(t, blob.DefaultTTL, o.URLTTL)
		require.Positive(t, o.MaxSize)
		require.NotNil(t, o.Now)
		require.NotNil(t, o.Logger)
	})

	for _, tt := range []struct {
		name   string
		mutate func(*Options)
	}{
		{"NoEndpoint", func(o *Options) { o.Endpoint = "" }},
		{"NoBucket", func(o *Options) { o.Bucket = "" }},
		{"NoNamespace", func(o *Options) { o.Namespace = "" }},
		{"SlashInNamespace", func(o *Options) { o.Namespace = "a/b" }},
		{"LeadingDotNamespace", func(o *Options) { o.Namespace = ".a" }},
		{"NegativeTTL", func(o *Options) { o.URLTTL = -1 }},
		{"TTLOverSevenDays", func(o *Options) { o.URLTTL = maxURLTTL + 1 }},
		{"NegativeMaxSize", func(o *Options) { o.MaxSize = -1 }},
	} {
		t.Run("Rejects"+tt.name, func(t *testing.T) {
			o := base()
			tt.mutate(&o)
			require.Error(t, o.setDefaults())
		})
	}
}

// TestNamespaceCannotEscapeThePrefix pairs with [TestOptionsValidate]: a
// namespace is the first component of every key this store writes, so one
// carrying a slash would let a server write outside its own space.
func TestNamespaceCannotEscapeThePrefix(t *testing.T) {
	for _, ns := range []string{"a/b", "../b", "..", ".", "", "/a"} {
		o := Options{Endpoint: "s3.example.com", Bucket: "b", Namespace: ns}
		require.Error(t, o.setDefaults(), "namespace %q must be refused", ns)
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, tt := range []struct {
		in     string
		host   string
		secure bool
	}{
		{"s3.example.com", "s3.example.com", true},
		{"s3.example.com:9000", "s3.example.com:9000", true},
		{"https://s3.example.com", "s3.example.com", true},
		{"http://localhost:9000", "localhost:9000", false},
	} {
		host, secure, err := parseEndpoint(tt.in)
		require.NoError(t, err, tt.in)
		require.Equal(t, tt.host, host, tt.in)
		require.Equal(t, tt.secure, secure, "%s: a bare host must not fall back to plaintext", tt.in)
	}

	for _, in := range []string{"ftp://example.com", "https://"} {
		_, _, err := parseEndpoint(in)
		require.Error(t, err, in)
	}
}

// TestAmbientCredentials covers the default chain, and pins the property that
// makes it safe to build one without asking: it reads the environment and the
// shared credentials file, and never reaches the instance metadata service.
// That address is link-local, which the egress policy blocks by default, and a
// store quietly reaching for it is what the policy exists to prevent.
func TestAmbientCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	creds := ambientCredentials()
	require.NotNil(t, creds)

	got, err := creds.GetWithContext(nil)
	require.NoError(t, err)
	require.Equal(t, "AKIAEXAMPLE", got.AccessKeyID)
	require.Equal(t, "secret", got.SecretAccessKey)
}
