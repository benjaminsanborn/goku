// Package backup produces encrypted off-box backups of everything goku
// can't rebuild: database containers and hosted git repos. Bundles are
// AES-256 encrypted (key on the host — store a copy in a password manager!)
// and force-pushed as a single orphan commit to a private GitHub repo, so
// the remote always holds exactly the latest bundle.
package backup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Config struct {
	DataDir string // /var/lib/goku — repos live here; local bundles in backups/
	KeyFile string // /etc/goku/backup.key — created on first run
	Repo    string // owner/name of the private GitHub backup repo
	Token   string // GitHub token able to push to Repo
}

const restoreDoc = `# Restoring a goku backup

1. Decrypt:  openssl enc -d -aes-256-cbc -pbkdf2 -pass file:backup.key -in <bundle>.tar.gz.enc | tar -xz
2. Repos:    copy repos/ back to /var/lib/goku/repos (chown to the service user)
3. Databases: for each db/<container>.sql, start the matching postgres container
   (goku redeploys create them) and:  docker exec -i <container> psql -U <role> -d postgres < db/<container>.sql
   The role name is in the first lines of the dump.
4. The backup.key file is NOT in this repo — it lives on the goku host at
   /etc/goku/backup.key. Keep a copy in a password manager.
`

// EnsureKey creates the encryption key on first use and returns it.
func EnsureKey(keyFile string) (string, error) {
	if b, err := os.ReadFile(keyFile); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// Run produces one backup: local bundle with retention, then (if configured)
// the encrypted off-box push. Returns a human summary.
func Run(cfg Config) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	backupsDir := filepath.Join(cfg.DataDir, "backups")
	work := filepath.Join(backupsDir, "work-"+stamp)
	if err := os.MkdirAll(filepath.Join(work, "db"), 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	// 1. Dump every database container.
	out, _ := exec.Command("docker", "ps", "--filter", "name=goku-db-", "--format", "{{.Names}}").Output()
	dbs := strings.Fields(string(out))
	for _, c := range dbs {
		dump, err := exec.Command("docker", "exec", c, "sh", "-c", `pg_dumpall -U "$POSTGRES_USER"`).Output()
		if err != nil {
			return "", fmt.Errorf("dump %s: %w", c, err)
		}
		if err := os.WriteFile(filepath.Join(work, "db", c+".sql"), dump, 0o600); err != nil {
			return "", err
		}
	}

	// 2. Bundle (db dumps + repos directory, extractable in place) + retention.
	bundle := filepath.Join(backupsDir, "goku-backup-"+stamp+".tar.gz")
	if err := exec.Command("tar", "-czf", bundle, "-C", work, "db", "-C", cfg.DataDir, "repos").Run(); err != nil {
		return "", fmt.Errorf("bundle: %w", err)
	}
	prune(backupsDir, 7)

	summary := fmt.Sprintf("backup %s: %d database(s), repos, %s", stamp, len(dbs), fileSize(bundle))

	// 4. Off-box: encrypt + force-push a single-commit repo.
	if cfg.Repo != "" && cfg.Token != "" {
		key, err := EnsureKey(cfg.KeyFile)
		if err != nil {
			return "", err
		}
		_ = key
		enc := bundle + ".enc"
		if err := exec.Command("openssl", "enc", "-aes-256-cbc", "-pbkdf2",
			"-pass", "file:"+cfg.KeyFile, "-in", bundle, "-out", enc).Run(); err != nil {
			return "", fmt.Errorf("encrypt: %w", err)
		}
		defer os.Remove(enc)
		if err := pushOffBox(cfg, enc, stamp); err != nil {
			return summary + " — OFF-BOX PUSH FAILED: " + err.Error(), nil
		}
		summary += ", pushed to github.com/" + cfg.Repo
	} else {
		summary += " (off-box push not configured)"
	}
	writeStamp(cfg.DataDir)
	return summary, nil
}

func pushOffBox(cfg Config, encBundle, stamp string) error {
	dir, err := os.MkdirTemp("", "goku-backup-push-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := exec.Command("cp", encBundle, filepath.Join(dir, "latest.tar.gz.enc")).Run(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "RESTORE.md"), []byte(restoreDoc), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "STAMP"), []byte(stamp+"\n"), 0o644); err != nil {
		return err
	}
	if err := run("add", "-A"); err != nil {
		return err
	}
	if err := run("-c", "user.name=goku-backup", "-c", "user.email=backup@goku.host",
		"commit", "-q", "-m", "backup "+stamp); err != nil {
		return err
	}
	remote := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", cfg.Token, cfg.Repo)
	// Force-push the orphan commit: the remote holds only the latest bundle.
	return run("push", "-q", "--force", remote, "main")
}

// Stale reports whether the last successful backup is older than maxAge.
func Stale(dataDir string, maxAge time.Duration) bool {
	b, err := os.ReadFile(filepath.Join(dataDir, "backups", "last"))
	if err != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return err != nil || time.Since(t) > maxAge
}

func writeStamp(dataDir string) {
	_ = os.WriteFile(filepath.Join(dataDir, "backups", "last"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func prune(dir string, keep int) {
	entries, err := filepath.Glob(filepath.Join(dir, "goku-backup-*.tar.gz"))
	if err != nil || len(entries) <= keep {
		return
	}
	sort.Strings(entries)
	for _, old := range entries[:len(entries)-keep] {
		_ = os.Remove(old)
	}
}

func fileSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	kb := fi.Size() / 1024
	if kb > 1024 {
		return fmt.Sprintf("%.1fMB", float64(kb)/1024)
	}
	return fmt.Sprintf("%dKB", kb)
}
