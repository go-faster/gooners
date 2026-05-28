package main

import (
	"log"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/systemd"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	pool := session.NewPool()
	defer pool.Shutdown()

	s := server.NewMCPServer("ssh-mcp", "0.1.0")
	core.Register(s, pool)
	fs.Register(s, pool)
	systemd.Register(s, pool)

	if err := server.ServeStdio(s); err != nil {
		log.Fatal(err)
	}
}
