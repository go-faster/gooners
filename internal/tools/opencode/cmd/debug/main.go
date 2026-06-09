// Debug tool to trace opencode session lifecycle via the API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	opencode "github.com/go-faster/gooners/internal/tools/opencode"
)

func pretty(label string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("=== %s ===\n%s\n\n", label, data)
}

func main() {
	cfg := opencode.Config{BaseURL: "http://localhost:4096"}
	client, err := opencode.NewClient(cfg, 60*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new client: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	// Health check
	health, err := client.Health(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: %v\n", err)
		os.Exit(1)
	}
	pretty("health", health)

	// Create session (no model override — use workspace default)
	session, err := client.CreateSession(ctx, opencode.Location{}, opencode.CreateSessionRequest{
		Title: "debug-test",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		os.Exit(1)
	}
	pretty("session", session)

	// Send prompt
	prompt, err := client.Prompt(ctx, opencode.Location{}, session.ID, opencode.PromptRequest{
		Prompt: opencode.PromptPayload{Text: "Reply with exactly: Hello from debug test"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "prompt: %v\n", err)
		os.Exit(1)
	}
	pretty("prompt response", prompt)

	// Messages
	msgs, err := client.Messages(ctx, opencode.Location{}, session.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "messages: %v\n", err)
	} else {
		pretty("messages", msgs)
	}

	// Context
	ctxData, err := client.Context(ctx, opencode.Location{}, session.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "context: %v\n", err)
	} else {
		pretty("context", ctxData)
	}
}
