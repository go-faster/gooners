package router

import (
	"testing"
)

func TestRouter(t *testing.T) {
	tests := []struct {
		name      string
		routes    []struct{ host, path, val string }
		reqHost   string
		reqPath   string
		wantVal   string
		wantFound bool
	}{
		{
			name: "exact match",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "mcp-val"},
			},
			reqHost:   "example.com",
			reqPath:   "/mcp",
			wantVal:   "mcp-val",
			wantFound: true,
		},
		{
			name: "prefix match boundary true",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "mcp-val"},
			},
			reqHost:   "",
			reqPath:   "/mcp/sub",
			wantVal:   "mcp-val",
			wantFound: true,
		},
		{
			name: "prefix match boundary false",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "mcp-val"},
			},
			reqHost:   "",
			reqPath:   "/mcp-sub",
			wantVal:   "",
			wantFound: false,
		},
		{
			name: "trailing slash route match exactly",
			routes: []struct{ host, path, val string }{
				{"", "/mcp/", "mcp-val"},
			},
			reqHost:   "",
			reqPath:   "/mcp/",
			wantVal:   "mcp-val",
			wantFound: true,
		},
		{
			name: "trailing slash route match sub",
			routes: []struct{ host, path, val string }{
				{"", "/mcp/", "mcp-val"},
			},
			reqHost:   "",
			reqPath:   "/mcp/sub",
			wantVal:   "mcp-val",
			wantFound: true,
		},
		{
			name: "trailing slash route no match partial",
			routes: []struct{ host, path, val string }{
				{"", "/mcp/", "mcp-val"},
			},
			reqHost:   "",
			reqPath:   "/mcp",
			wantVal:   "",
			wantFound: false,
		},
		{
			name: "root route",
			routes: []struct{ host, path, val string }{
				{"", "/", "root-val"},
			},
			reqHost:   "",
			reqPath:   "/anything",
			wantVal:   "root-val",
			wantFound: true,
		},
		{
			name: "empty path route",
			routes: []struct{ host, path, val string }{
				{"", "", "empty-val"},
			},
			reqHost:   "",
			reqPath:   "/anything",
			wantVal:   "empty-val",
			wantFound: true,
		},
		{
			name: "host specific",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "any-mcp"},
				{"api.example.com", "/mcp", "api-mcp"},
			},
			reqHost:   "api.example.com",
			reqPath:   "/mcp/test",
			wantVal:   "api-mcp",
			wantFound: true,
		},
		{
			name: "host fallback",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "any-mcp"},
				{"api.example.com", "/foo", "api-foo"},
			},
			reqHost:   "api.example.com",
			reqPath:   "/mcp/test",
			wantVal:   "any-mcp",
			wantFound: true,
		},
		{
			name: "longest match wins",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "short"},
				{"", "/mcp/long", "long"},
			},
			reqHost:   "",
			reqPath:   "/mcp/long/test",
			wantVal:   "long",
			wantFound: true,
		},
		{
			name: "longest match fallback to short",
			routes: []struct{ host, path, val string }{
				{"", "/mcp", "short"},
				{"", "/mcp/long", "long"},
			},
			reqHost:   "",
			reqPath:   "/mcp/test",
			wantVal:   "short",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New[string]()
			for _, rt := range tt.routes {
				r.Add(rt.host, rt.path, rt.val)
			}
			gotVal, gotFound := r.Lookup(tt.reqHost, tt.reqPath)
			if gotFound != tt.wantFound {
				t.Errorf("Lookup() gotFound = %v, want %v", gotFound, tt.wantFound)
			}
			if gotVal != tt.wantVal {
				t.Errorf("Lookup() gotVal = %v, want %v", gotVal, tt.wantVal)
			}
		})
	}
}
