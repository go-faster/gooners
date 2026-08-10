// Package all registers every tunnel provider, for a binary that would rather
// link them all than choose.
//
// Import it for side effects only:
//
//	import _ "github.com/go-faster/gooners/tunnel/all"
//
// A binary that cares what it links should import the provider packages it
// wants instead: pulling in ngrok's SDK to run cloudflared is a lot of build
// for nothing.
package all

import (
	// Registering the providers is the entire purpose of this package.
	_ "github.com/go-faster/gooners/tunnel/cloudflared"
	_ "github.com/go-faster/gooners/tunnel/ngrok"
)
