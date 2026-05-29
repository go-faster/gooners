package main

import (
	"log"
	"os"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/systemd"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	uploadRoot, err := os.Getwd()
	if err != nil {
		log.Fatalf("getting working directory: %v", err)
	}

	pool := session.NewPool()
	defer pool.Shutdown()

	s := server.NewMCPServer("ssh-mcp", "0.1.0")
	core.Register(s, pool)
	fs.Register(s, pool, uploadRoot)
	systemd.Register(s, pool)

	if err := server.ServeStdio(s); err != nil {
		log.Fatal(err)
	}
}
