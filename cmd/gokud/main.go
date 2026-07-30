// Command gokud runs the control plane: REST API, git server, MCP endpoint, and UI.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benjaminsanborn/goku/internal/backup"
	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/server"
	"github.com/benjaminsanborn/goku/internal/store"
)

func main() {
	// Operator subcommand: create an organization + owner token directly
	// against the database. Runs on the server host only — there is no
	// network signup. Usage: gokud create-org <name>
	if len(os.Args) > 2 && os.Args[1] == "create-org" {
		createOrg(os.Args[2])
		return
	}
	// Operator subcommand: run a backup now. Usage: gokud backup
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		loadServerEnv()
		runBackupNow()
		return
	}

	dsn := envOr("DATABASE_URL", "postgres://localhost:5432/goku_development")
	port := envOr("PORT", "8080")
	token := envOr("GOKU_TOKEN", "dev-token")
	webDist := envOr("WEB_DIST", "web/dist")
	dataDir, err := filepath.Abs(envOr("GOKU_DATA", "data"))
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	baseURL := envOr("GOKU_BASE_URL", "http://localhost:"+port)

	st, err := store.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	srv := &server.Server{
		Store: st, Token: token, WebDist: webDist, DataDir: dataDir, BaseURL: baseURL,
		Deploy: deploy.Target{
			AppsCaddyFile: os.Getenv("GOKU_APPS_CADDY"),
			AppDomain:     envOr("GOKU_APP_DOMAIN", "localhost"),
			PGSuperDSN:    dsn,
		},
		WebhookSecret: os.Getenv("GOKU_WEBHOOK_SECRET"),
		OAuth: server.OAuthConfig{
			GitHubClientID:     os.Getenv("GOKU_GITHUB_CLIENT_ID"),
			GitHubClientSecret: os.Getenv("GOKU_GITHUB_CLIENT_SECRET"),
			GoogleClientID:     os.Getenv("GOKU_GOOGLE_CLIENT_ID"),
			GoogleClientSecret: os.Getenv("GOKU_GOOGLE_CLIENT_SECRET"),
		},
	}

	// Nightly encrypted backups (db containers + repos), off-box when
	// configured; the loop re-checks hourly and runs when >20h stale.
	go func() {
		for {
			if backup.Stale(dataDir, 20*time.Hour) {
				if summary, err := backupRun(st, dataDir); err != nil {
					log.Printf("backup failed: %v", err)
				} else {
					log.Printf("%s", summary)
				}
			}
			time.Sleep(time.Hour)
		}
	}()

	// Register this host as an ordinary fleet member of the operator's org.
	if host, err := os.Hostname(); err == nil {
		if id, err := st.EnsureLocalInstance(context.Background(), host); err == nil {
			go srv.VerifyInstanceByID(st.DefaultOrgID, id)
		}
	}

	fmt.Printf("gokud — control plane\n")
	fmt.Printf("  ui:   %s\n", baseURL)
	fmt.Printf("  api:  %s/v1\n", baseURL)
	fmt.Printf("  git:  %s/git/<project>.git\n", baseURL)
	fmt.Printf("  mcp:  %s/mcp\n", baseURL)
	fmt.Printf("\ninstall into Claude Code:\n")
	fmt.Printf("  claude mcp add --transport http goku %s/mcp --header \"Authorization: Bearer %s\"\n\n", baseURL, token)

	if err := http.ListenAndServe(":"+port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func createOrg(name string) {
	loadServerEnv()
	dsn := envOr("DATABASE_URL", "postgres://localhost:5432/goku_development")
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	org, err := st.CreateOrg(context.Background(), name)
	if err != nil {
		log.Fatal(err)
	}
	token, err := st.CreateToken(context.Background(), org.ID, "owner")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("organization %q created\n\n  token: %s\n\n", org.Name, token)
	fmt.Println("hand this to the user — it is shown only once. they run:")
	fmt.Printf("  goku login --token %s\n", token)
}

// loadServerEnv makes create-org work over plain ssh by reading the systemd
// env file when DATABASE_URL isn't already set.
func loadServerEnv() {
	if os.Getenv("DATABASE_URL") != "" {
		return
	}
	b, err := os.ReadFile("/etc/goku/gokud.env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func backupRun(st *store.Store, dataDir string) (string, error) {
	return backup.Run(backup.Config{
		DataDir: dataDir,
		KeyFile: envOr("GOKU_BACKUP_KEY", "/etc/goku/backup.key"),
		Repo:    os.Getenv("GOKU_BACKUP_REPO"),
		Token:   st.GitHubTokenForOrg(context.Background(), st.DefaultOrgID),
	})
}

func runBackupNow() {
	dsn := envOr("DATABASE_URL", "postgres://localhost:5432/goku_development")
	dataDir, _ := filepath.Abs(envOr("GOKU_DATA", "data"))
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	summary, err := backupRun(st, dataDir)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(summary)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
