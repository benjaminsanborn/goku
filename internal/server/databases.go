package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/benjaminsanborn/goku/internal/cloud"
	"github.com/benjaminsanborn/goku/internal/pgconf"
	"github.com/benjaminsanborn/goku/internal/store"
)

// Managed databases (design doc 14): a Postgres 18 container on a dedicated
// EC2 instance, with its data on a separate EBS volume, a postgresql.conf
// computed from the instance size, and connection details on the page.
//
// The machine is provisioned by the same code that provisions fleet
// instances, but it is not a fleet member: nothing deploys to it, and its
// security group opens 5432 rather than the app port range.

const (
	pgContainer = "goku-postgres"
	pgDataDir   = "/var/lib/goku-pgdata"
	pgConfPath  = "/etc/goku/postgresql.conf"
)

var dbNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	databases, err := s.Store.ListDatabases(r.Context(), orgFrom(r.Context()))
	respond(w, map[string]any{"databases": databases, "sizes": pgconf.Sizes}, err)
}

func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDatabase(r.Context(), orgFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	size, _ := pgconf.SizeFor(d.Type)
	respond(w, map[string]any{
		"database":        d,
		"size":            size,
		"config":          pgconf.Render(size, d.StorageGB, d.Overrides),
		"restart_params":  pgconf.RestartParams(),
		"connection_host": d.Endpoint,
		"connection_port": 5432,
	}, nil)
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string            `json:"name"`
		ProviderID string            `json:"provider_id"`
		Type       string            `json:"instance_type"`
		StorageGB  int               `json:"storage_gb"`
		Overrides  map[string]string `json:"config_overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !dbNameRe.MatchString(name) {
		httpError(w, http.StatusUnprocessableEntity, "database name must start with a letter and use only lowercase letters, digits, - and _")
		return
	}
	if _, ok := pgconf.SizeFor(in.Type); !ok {
		httpError(w, http.StatusUnprocessableEntity, "pick one of the offered instance sizes")
		return
	}
	if in.StorageGB < 20 || in.StorageGB > 4000 {
		httpError(w, http.StatusUnprocessableEntity, "storage must be between 20 and 4000 GB")
		return
	}

	org := orgFrom(r.Context())
	provider, err := s.Store.GetProvider(r.Context(), org, in.ProviderID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if provider.Kind != "aws" || provider.Status != "ready" {
		httpError(w, http.StatusUnprocessableEntity, "databases run on a verified AWS provider today")
		return
	}

	d, err := s.Store.CreateDatabase(r.Context(), org, store.NewDatabase{
		Name: name, ProviderID: provider.ID, Type: in.Type, StorageGB: in.StorageGB,
		Superuser: "goku", Password: randomPassword(), Overrides: in.Overrides,
	}, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go s.createDatabase(org, d.ID)
	respond(w, map[string]any{"database": d}, nil)
}

// handlePreviewDatabaseConfig renders the config a set of overrides would
// produce, so the create form and the config editor show the real file before
// anything is committed to.
func (s *Server) handlePreviewDatabaseConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type      string            `json:"instance_type"`
		StorageGB int               `json:"storage_gb"`
		Overrides map[string]string `json:"config_overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	size, ok := pgconf.SizeFor(in.Type)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "pick one of the offered instance sizes")
		return
	}
	if in.StorageGB <= 0 {
		in.StorageGB = 50
	}
	respond(w, map[string]any{
		"config":  pgconf.Render(size, in.StorageGB, in.Overrides),
		"restart": pgconf.NeedsRestart(nil, in.Overrides),
	}, nil)
}

// handleConfigureDatabase applies overrides to a running database: reload
// when it can, restart when a postmaster parameter changed, and roll back to
// the previous file if Postgres doesn't come back.
func (s *Server) handleConfigureDatabase(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	d, err := s.Store.GetDatabaseSecrets(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	var in struct {
		Overrides map[string]string `json:"config_overrides"`
		DryRun    bool              `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	size, _ := pgconf.SizeFor(d.Type)
	rendered := pgconf.Render(size, d.StorageGB, in.Overrides)
	restart := pgconf.NeedsRestart(d.Overrides, in.Overrides)
	if in.DryRun {
		respond(w, map[string]any{"config": rendered, "restart": restart}, nil)
		return
	}
	if d.Status != "available" {
		httpError(w, http.StatusConflict, "database is "+d.Status+" — wait for it to settle")
		return
	}
	if err := s.Store.SetDatabaseOverrides(r.Context(), org, d.ID, in.Overrides, s.actorFrom(r)); err != nil {
		respond(w, nil, err)
		return
	}
	go s.applyDatabaseConfig(org, d.ID, rendered, restart)
	respond(w, map[string]any{"applying": true, "restart": restart}, nil)
}

func (s *Server) handleRebootDatabase(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	d, err := s.Store.GetDatabaseSecrets(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go func() {
		ctx := context.Background()
		log := d.EventLog + event("restarting Postgres")
		s.Store.SetDatabaseStatus(ctx, d.ID, "rebooting", log)
		exe := s.sshExecutor(d.Endpoint, d.SSHKey)
		defer exe.cleanup()
		exe.run("docker restart " + pgContainer)
		if err := waitForPostgres(exe, d.Superuser); err != nil {
			s.Store.SetDatabaseStatus(ctx, d.ID, "failed", log+event("did not come back: %v", err))
			return
		}
		s.Store.SetDatabaseStatus(ctx, d.ID, "available", log+event("back up"))
	}()
	respond(w, map[string]any{"rebooting": d.Name}, nil)
}

// handleDatabaseCredentials reveals the superuser password. Deliberately a
// separate, audited call rather than a field on the database itself.
func (s *Server) handleDatabaseCredentials(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	d, err := s.Store.GetDatabaseSecrets(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.Store.Audit(r.Context(), org, s.actorFrom(r), "database.reveal", "database/"+d.Name, nil)
	respond(w, map[string]any{
		"host": d.Endpoint, "port": 5432, "user": d.Superuser, "password": d.Password,
		"url": fmt.Sprintf("postgres://%s:%s@%s:5432/postgres?sslmode=disable", d.Superuser, d.Password, d.Endpoint),
	}, nil)
}

// handleDatabaseLogs tails the Postgres container's log.
func (s *Server) handleDatabaseLogs(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDatabaseSecrets(r.Context(), orgFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if d.Endpoint == "" {
		respond(w, map[string]any{"log": "not running yet"}, nil)
		return
	}
	exe := s.sshExecutor(d.Endpoint, d.SSHKey)
	defer exe.cleanup()
	out, err := exe.run(fmt.Sprintf("docker logs --tail 300 %s 2>&1", pgContainer))
	if err != nil {
		respond(w, map[string]any{"log": "could not read logs: " + err.Error()}, nil)
		return
	}
	respond(w, map[string]any{"log": out}, nil)
}

// handleListLogicalDatabases reads the server's databases live — Postgres is
// the source of truth, not a table here.
func (s *Server) handleListLogicalDatabases(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDatabaseSecrets(r.Context(), orgFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if d.Status != "available" {
		respond(w, map[string]any{"databases": []any{}}, nil)
		return
	}
	exe := s.sshExecutor(d.Endpoint, d.SSHKey)
	defer exe.cleanup()
	out, err := exe.run(psql(d.Superuser, "postgres",
		"select datname || '|' || pg_size_pretty(pg_database_size(datname)) from pg_database where not datistemplate order by 1"))
	if err != nil {
		respond(w, map[string]any{"databases": []any{}, "error": err.Error()}, nil)
		return
	}
	list := []map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, size, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || name == "" {
			continue
		}
		list = append(list, map[string]string{"name": name, "size": size})
	}
	respond(w, map[string]any{"databases": list}, nil)
}

// handleAddLogicalDatabase creates a database and its owner role, returning
// the connection string once — the whole last mile for v1.
func (s *Server) handleAddLogicalDatabase(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	d, err := s.Store.GetDatabaseSecrets(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !dbNameRe.MatchString(name) {
		httpError(w, http.StatusUnprocessableEntity, "database name must start with a letter and use only lowercase letters, digits, - and _")
		return
	}
	if d.Status != "available" {
		httpError(w, http.StatusConflict, "database is "+d.Status)
		return
	}

	password := randomPassword()
	exe := s.sshExecutor(d.Endpoint, d.SSHKey)
	defer exe.cleanup()
	// Role first: the database is created owned by it, so the app's own role
	// can manage its schema without the superuser.
	if _, err := exe.run(psql(d.Superuser, "postgres",
		fmt.Sprintf("create role %s login password %s", name, sqlLiteral(password)))); err != nil {
		respond(w, nil, fmt.Errorf("create role: %w", err))
		return
	}
	if _, err := exe.run(psql(d.Superuser, "postgres",
		fmt.Sprintf("create database %s owner %s", name, name))); err != nil {
		respond(w, nil, fmt.Errorf("create database: %w", err))
		return
	}
	s.Store.Audit(r.Context(), org, s.actorFrom(r), "database.add", "database/"+d.Name, map[string]any{"database": name})
	respond(w, map[string]any{
		"name": name,
		"url":  fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", name, password, d.Endpoint, name),
	}, nil)
}

// handleDeleteDatabase destroys the server and its data volume. The typed
// name is the only guard there is until backups exist.
func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	d, err := s.Store.GetDatabase(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if r.URL.Query().Get("confirm") != d.Name {
		httpError(w, http.StatusUnprocessableEntity, "type the database name to confirm — this destroys the data volume")
		return
	}
	provider, err := s.Store.GetProvider(r.Context(), org, d.ProviderID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	netProvider, _ := s.Store.ProviderByKind(r.Context(), org, "tailscale")
	deleted, err := s.Store.DeleteDatabase(r.Context(), org, d.ID, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if deleted.ExternalID != "" {
			_ = cloud.AWSFrom(provider.Credentials, provider.Region).Terminate(ctx, deleted.ExternalID, deleted.KeyName)
		}
		if netProvider != nil {
			_ = cloud.TailscaleFrom(netProvider.Credentials).RemoveDevice(ctx, "goku-db-"+deleted.Name)
		}
	}()
	respond(w, map[string]any{"deleted": true}, nil)
}

// createDatabase provisions the machine and brings Postgres up on it. Every
// step appends to the event feed the database page shows.
func (s *Server) createDatabase(org, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	d, err := s.Store.GetDatabaseSecrets(ctx, org, id)
	if err != nil {
		return
	}
	provider, err := s.Store.GetProvider(ctx, org, d.ProviderID)
	if err != nil {
		return
	}
	var log strings.Builder
	logf := func(format string, args ...any) {
		log.WriteString(event(format, args...))
		s.Store.SetDatabaseStatus(ctx, id, "creating", log.String())
	}
	fail := func(err error) {
		log.WriteString(event("failed: %v", err))
		s.Store.SetDatabaseStatus(ctx, id, "failed", log.String())
	}

	size, _ := pgconf.SizeFor(d.Type)
	opts := cloud.Options{
		Name:         d.Name,
		Type:         d.Type,
		Purpose:      "db",
		DataVolumeGB: d.StorageGB,
		Ingress: []cloud.PortRange{
			{From: 22, To: 22, Note: "goku control plane ssh"},
			{From: 5432, To: 5432, Note: "postgres"},
		},
		Setup: postgresSetup(d, pgconf.Render(size, d.StorageGB, d.Overrides)),
	}

	netProvider, err := s.Store.ProviderByKind(ctx, org, "tailscale")
	if err != nil {
		fail(err)
		return
	}
	var ts cloud.Tailscale
	if netProvider != nil {
		ts = cloud.TailscaleFrom(netProvider.Credentials)
		logf("minting a tailnet auth key from %s", netProvider.Name)
		key, err := ts.MintAuthKey(ctx)
		if err != nil {
			fail(err)
			return
		}
		opts.TailscaleAuthKey = key
	} else {
		logf("no tailnet — locking 5432 to this control plane")
		cidr, err := cloud.EgressIP(ctx)
		if err != nil {
			fail(err)
			return
		}
		opts.AllowCIDR = cidr + "/32"
	}

	machine, err := cloud.AWSFrom(provider.Credentials, provider.Region).Provision(ctx, opts, cloud.Logf(logf))
	if err != nil {
		fail(err)
		return
	}
	logf("launched %s (%s) with a %d GB data volume", machine.InstanceID, machine.Type, d.StorageGB)

	host := machine.PublicIP
	if opts.TailscaleAuthKey != "" {
		logf("waiting for %s to join the tailnet", opts.Hostname())
		ip, err := ts.WaitForDevice(ctx, opts.Hostname(), cloud.Logf(logf))
		if err != nil {
			fail(err)
			return
		}
		host = ip
		logf("joined the tailnet as %s (%s)", opts.Hostname(), ip)
	}
	if err := s.Store.AttachDatabaseMachine(ctx, id, host, machine.PrivateKey, machine.InstanceID, machine.KeyName); err != nil {
		fail(err)
		return
	}

	logf("waiting for Postgres %s to accept connections", d.Version)
	exe := s.sshExecutor(host, machine.PrivateKey)
	defer exe.cleanup()
	if err := waitForPostgresSlowly(ctx, exe, d.Superuser); err != nil {
		fail(err)
		return
	}
	// Preloaded at startup; the extension itself still has to be created.
	exe.run(psql(d.Superuser, "postgres", "create extension if not exists pg_stat_statements"))

	log.WriteString(event("available at %s:5432", host))
	s.Store.SetDatabaseStatus(ctx, id, "available", log.String())
}

// applyDatabaseConfig writes the rendered file, reloads or restarts, and
// restores the previous file if Postgres doesn't come back — the failure this
// feature most needs to survive.
func (s *Server) applyDatabaseConfig(org, id, rendered string, restart bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	d, err := s.Store.GetDatabaseSecrets(ctx, org, id)
	if err != nil {
		return
	}
	log := d.EventLog
	note := func(format string, args ...any) {
		log += event(format, args...)
		s.Store.SetDatabaseStatus(ctx, id, "modifying", log)
	}
	note("applying configuration (%s)", map[bool]string{true: "restart required", false: "reload"}[restart])

	exe := s.sshExecutor(d.Endpoint, d.SSHKey)
	defer exe.cleanup()

	if _, err := exe.run(fmt.Sprintf("sudo cp %s %s.bak", pgConfPath, pgConfPath)); err != nil {
		s.Store.SetDatabaseStatus(ctx, id, "available", log+event("could not back up the current config: %v", err))
		return
	}
	if _, err := exe.run(writeFile(pgConfPath, rendered)); err != nil {
		s.Store.SetDatabaseStatus(ctx, id, "available", log+event("could not write the config: %v", err))
		return
	}

	if restart {
		exe.run("docker restart " + pgContainer)
	} else {
		exe.run("docker exec " + pgContainer + " psql -U " + d.Superuser + " -d postgres -c 'select pg_reload_conf()'")
	}
	if err := waitForPostgres(exe, d.Superuser); err != nil {
		note("Postgres did not come back — restoring the previous config")
		exe.run(fmt.Sprintf("sudo cp %s.bak %s", pgConfPath, pgConfPath))
		exe.run("docker restart " + pgContainer)
		reason, _ := exe.run("docker logs --tail 20 " + pgContainer + " 2>&1")
		if err := waitForPostgres(exe, d.Superuser); err != nil {
			s.Store.SetDatabaseStatus(ctx, id, "failed", log+event("rollback failed: %v\n%s", err, reason))
			return
		}
		s.Store.SetDatabaseStatus(ctx, id, "available", log+event("rolled back, the database is on its previous config:\n%s", strings.TrimSpace(reason)))
		return
	}
	s.Store.SetDatabaseStatus(ctx, id, "available", log+event("configuration applied"))
}

// postgresSetup is the cloud-init tail: mount the data volume, write the
// config, run Postgres.
func postgresSetup(d *store.Database, conf string) string {
	var b strings.Builder
	b.WriteString(`
# --- goku managed postgres ---
# The data volume is the one disk with no filesystem and nothing mounted.
DATA=""
for dev in $(lsblk -dnro NAME,TYPE | awk '$2=="disk"{print $1}'); do
  if [ -z "$(lsblk -nro MOUNTPOINT /dev/$dev | tr -d '\n ')" ]; then DATA=/dev/$dev; fi
done
mkdir -p ` + pgDataDir + `
if [ -n "$DATA" ]; then
  blkid "$DATA" >/dev/null 2>&1 || mkfs.ext4 -F "$DATA"
  echo "UUID=$(blkid -s UUID -o value $DATA) ` + pgDataDir + ` ext4 defaults,nofail 0 2" >> /etc/fstab
  mount -a
fi
mkdir -p /etc/goku
`)
	b.WriteString("cat > " + pgConfPath + " <<'GOKUPGCONF'\n")
	b.WriteString(conf)
	b.WriteString("\nGOKUPGCONF\n")
	fmt.Fprintf(&b, `docker run -d --name %s --restart unless-stopped \
  -p 5432:5432 \
  -v %s:/var/lib/postgresql \
  -v %s:/etc/postgresql/postgresql.conf:ro \
  -e POSTGRES_USER=%s \
  -e POSTGRES_PASSWORD=%s \
  -e POSTGRES_DB=postgres \
  -e PGDATA=/var/lib/postgresql/data \
  postgres:%s -c config_file=/etc/postgresql/postgresql.conf
`, pgContainer, pgDataDir, pgConfPath, shq(d.Superuser), shq(d.Password), d.Version)
	return b.String()
}

// waitForPostgres polls pg_isready for about a minute — long enough for a
// restart, short enough that a broken config is reported promptly.
func waitForPostgres(exe executor, user string) error {
	for i := 0; i < 20; i++ {
		if _, err := exe.run(fmt.Sprintf("docker exec %s pg_isready -U %s", pgContainer, shq(user))); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("Postgres did not accept connections within a minute")
}

// waitForPostgresSlowly allows for the whole of first boot: cloud-init
// installing docker, pulling the image, and initdb.
func waitForPostgresSlowly(ctx context.Context, exe executor, user string) error {
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		if _, err := exe.run(fmt.Sprintf("docker exec %s pg_isready -U %s", pgContainer, shq(user))); err == nil {
			return nil
		}
	}
	return fmt.Errorf("Postgres did not come up within 8 minutes — check the instance's cloud-init log")
}

// psql runs one statement as the superuser inside the container.
func psql(user, database, sql string) string {
	return fmt.Sprintf("docker exec %s psql -U %s -d %s -At -v ON_ERROR_STOP=1 -c %s",
		pgContainer, shq(user), shq(database), shq(sql))
}

// writeFile ships content over ssh without a second connection.
func writeFile(path, content string) string {
	return fmt.Sprintf("sudo tee %s > /dev/null <<'GOKUFILE'\n%s\nGOKUFILE", path, content)
}

// shq single-quotes a value for a remote shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sqlLiteral quotes a value for SQL (passwords, which then pass through the
// shell quoting in psql()).
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func event(format string, args ...any) string {
	return time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
}

func randomPassword() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "goku-" + fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}
