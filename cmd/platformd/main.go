// Command platformd runs the control plane: REST API, git server, MCP endpoint, and UI.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/benjaminsanborn/platform/internal/server"
	"github.com/benjaminsanborn/platform/internal/store"
)

func main() {
	dsn := envOr("DATABASE_URL", "postgres://localhost:5432/platform_development")
	port := envOr("PORT", "8080")
	token := envOr("PLATFORM_TOKEN", "dev-token")
	webDist := envOr("WEB_DIST", "web/dist")
	dataDir, err := filepath.Abs(envOr("PLATFORM_DATA", "data"))
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	baseURL := envOr("PLATFORM_BASE_URL", "http://localhost:"+port)

	st, err := store.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	srv := &server.Server{Store: st, Token: token, WebDist: webDist, DataDir: dataDir, BaseURL: baseURL}

	fmt.Printf("platformd — control plane\n")
	fmt.Printf("  ui:   %s\n", baseURL)
	fmt.Printf("  api:  %s/v1\n", baseURL)
	fmt.Printf("  git:  %s/git/<project>.git\n", baseURL)
	fmt.Printf("  mcp:  %s/mcp\n", baseURL)
	fmt.Printf("\ninstall into Claude Code:\n")
	fmt.Printf("  claude mcp add --transport http platform %s/mcp --header \"Authorization: Bearer %s\"\n\n", baseURL, token)

	if err := http.ListenAndServe(":"+port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
