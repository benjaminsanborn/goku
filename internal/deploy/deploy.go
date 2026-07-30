// Package deploy runs kamal-style container deployments on the control-plane
// host: build an image from the project repo at a SHA, provision app
// databases on the host's postgres, run the container with a 12-factor env
// contract (PORT, DATABASE_URL, DATA_DIR), health-check it, route
// <project>.<domain> through Caddy, and supersede the previous container.
package deploy

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Target describes where and how apps run on this host.
type Target struct {
	AppsCaddyFile string // per-project site blocks, imported by Caddyfile
	AppDomain     string // apps live at <project>.<AppDomain>
	PGSuperDSN    string // control-plane DSN, used to provision app roles/dbs
}

type Logf func(format string, args ...any)

type Manifest struct {
	Services  map[string]Service
	Resources map[string]Resource
	Routes    []Route
}

type Route struct {
	Domain  string `yaml:"domain"`
	Service string `yaml:"service"`
}

type Service struct {
	Type        string            `yaml:"type"`
	Port        int               `yaml:"port"`
	HealthCheck string            `yaml:"health_check"`
	Env         map[string]string `yaml:"env"`
	// HostMounts is honored only for operator-org projects (the control
	// plane deploying itself needs docker.sock, repos, caddy config).
	HostMounts []string `yaml:"host_mounts"`
}

type Resource struct {
	Type string `yaml:"type"`
}

func ParseManifest(raw string) (*Manifest, error) {
	var doc struct {
		Services  map[string]Service  `yaml:"services"`
		Resources map[string]Resource `yaml:"resources"`
		Routes    []Route             `yaml:"routes"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("goku.yaml does not parse: %w", err)
	}
	return &Manifest{Services: doc.Services, Resources: doc.Resources, Routes: doc.Routes}, nil
}

// Port allocates a host port per deployment (project+sha) so a new container
// never fights its predecessor for the same bind — blue-green needs both
// alive at once. avoid is the currently-routed port, stepped over on the
// rare hash collision.
func Port(project, sha string, avoid int) int {
	h := fnv.New32a()
	h.Write([]byte("app/" + project + "/" + sha))
	p := 30000 + int(h.Sum32()%10000)
	if p == avoid {
		p++
	}
	return p
}

func run(logf Logf, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			logf("%s", tail(text, 30))
		}
		return text, fmt.Errorf("%s %s failed: %w", name, args[0], err)
	}
	return text, nil
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Build produces a docker image from the bare repo at sha.
func Build(repoPath, project, sha string, logf Logf) (string, error) {
	image := fmt.Sprintf("goku-app/%s:%s", project, sha[:12])
	dir, err := os.MkdirTemp("", "goku-build-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	logf("exporting %s @ %s", project, sha[:12])
	// git archive | tar keeps the build context clean (no .git).
	archive := exec.Command("git", "--git-dir", repoPath, "archive", sha)
	untar := exec.Command("tar", "-x", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	untar.Stdin = pipe
	if err := untar.Start(); err != nil {
		return "", err
	}
	if err := archive.Run(); err != nil {
		return "", fmt.Errorf("git archive: %w", err)
	}
	if err := untar.Wait(); err != nil {
		return "", fmt.Errorf("untar: %w", err)
	}
	if _, err := os.Stat(dir + "/Dockerfile"); err != nil {
		return "", fmt.Errorf("no Dockerfile at the root of this branch — goku builds services from a Dockerfile")
	}

	logf("building image %s", image)
	if _, err := run(logf, "docker", "build", "-q", "-t", image, dir); err != nil {
		return "", err
	}
	return image, nil
}

// EnsureAppDatabases provisions a host-postgres role and one database per
// manifest database resource; returns the env vars for the container.
func EnsureAppDatabases(t Target, project, password string, m *Manifest, logf Logf) (map[string]string, error) {
	env := map[string]string{}
	names := []string{}
	for name, r := range m.Resources {
		if r.Type == "database" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return env, nil
	}

	role := "goku_app_" + sanitize(project)
	psql := func(query string) error {
		cmd := exec.Command("psql", t.PGSuperDSN, "-v", "ON_ERROR_STOP=1", "-qAt", "-c", query)
		out, err := cmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "already exists") {
			return fmt.Errorf("psql: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := psql(fmt.Sprintf(`create role %s login password '%s'`, role, password)); err != nil {
		return nil, err
	}
	if err := psql(fmt.Sprintf(`alter role %s login password '%s'`, role, password)); err != nil {
		return nil, err
	}
	// PG16+: creating a database owned by another role requires SET ROLE on it.
	grant := `do $$ begin execute format('grant %I to %I with set true', '` + role + `', current_user); end $$;`
	if err := psql(grant); err != nil {
		return nil, err
	}
	for i, name := range names {
		db := fmt.Sprintf("goku_app_%s_%s", sanitize(project), sanitize(name))
		if err := psql(fmt.Sprintf(`create database %s owner %s`, db, role)); err != nil {
			return nil, err
		}
		url := fmt.Sprintf("postgres://%s:%s@localhost:5432/%s?sslmode=disable", role, password, db)
		env["GOKU_DATABASE_"+strings.ToUpper(sanitize(name))+"_URL"] = url
		if i == 0 {
			env["DATABASE_URL"] = url
		}
		logf("database ready: %s", db)
	}
	return env, nil
}

// Run starts the app container (host networking; the app must honor PORT).
func Run(project, image string, port int, env map[string]string, hostMounts []string, logf Logf) (string, error) {
	name := fmt.Sprintf("goku-%s-%d", sanitize(project), time.Now().Unix())
	args := []string{
		"run", "-d", "--name", name,
		"--network", "host",
		"--restart", "unless-stopped",
		"--volume", fmt.Sprintf("goku-app-%s:/data", sanitize(project)),
		"--env", fmt.Sprintf("PORT=%d", port),
		"--env", "DATA_DIR=/data",
	}
	for _, m := range hostMounts {
		args = append(args, "--volume", m)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	args = append(args, image)
	logf("starting container %s on port %d", name, port)
	if _, err := run(logf, "docker", args...); err != nil {
		return "", err
	}
	return name, nil
}

// HealthCheck polls the app until it answers 200 on the health path.
func HealthCheck(port int, path string, timeout time.Duration, logf Logf) error {
	if path == "" {
		path = "/"
	}
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	deadline := time.Now().Add(timeout)
	logf("health check %s", url)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode < 400 {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("no healthy response from %s within %s", url, timeout)
}

// StopPrevious stops and removes older containers for the project, keeping
// the current one.
func StopPrevious(project, keep string, logf Logf) {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=goku-"+sanitize(project)+"-", "--format", "{{.Names}}").Output()
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if name == keep {
			continue
		}
		logf("stopping previous container %s", name)
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}
}

// WriteRoutes regenerates the Caddy site blocks for all healthy apps and
// reloads Caddy.
// WriteRoutes regenerates the Caddy site blocks: host → port.
func WriteRoutes(t Target, routes map[string]int, logf Logf) error {
	if t.AppsCaddyFile == "" {
		logf("no apps caddy file configured — skipping routing")
		return nil
	}
	var b strings.Builder
	b.WriteString("# generated by gokud — one site block per healthy app deployment\n")
	hosts := make([]string, 0, len(routes))
	for h := range routes {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		fmt.Fprintf(&b, "%s {\n\treverse_proxy localhost:%d\n}\n", h, routes[h])
	}
	if err := os.WriteFile(t.AppsCaddyFile, []byte(b.String()), 0o644); err != nil {
		return err
	}
	// Reload via Caddy's admin API (localhost:2019) — no privileges needed.
	if _, err := run(logf, "caddy", "reload", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return fmt.Errorf("caddy reload: %w", err)
	}
	logf("routes updated")
	return nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
