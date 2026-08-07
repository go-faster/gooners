package gateway

import (
	"maps"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaKeyProtocolVersion is the request `_meta` key an SDK client attaches to
// declare the protocol revision it negotiated. Defined here rather than taken
// from the SDK because v1.6.1 does not export it; v1.7.0 names it
// [mcp.MetaKeyProtocolVersion] with this same value.
const metaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"

// proxyMeta returns the downstream request's `_meta` as it should reach an
// upstream.
//
// Everything the client sent is forwarded unchanged, so an upstream sees the
// same client it would have seen without a gateway in the path — except the
// negotiated protocol version, which is a property of one hop and not of the
// call. The gateway negotiates separately with the client and with each
// upstream, so forwarding the client's value tells the upstream a version that
// was never agreed on that connection; an SDK that validates it rejects the
// request.
func proxyMeta(m mcp.Meta) mcp.Meta {
	if _, ok := m[metaKeyProtocolVersion]; !ok {
		return m
	}
	out := make(mcp.Meta, len(m)-1)
	maps.Copy(out, m)
	delete(out, metaKeyProtocolVersion)
	return out
}
