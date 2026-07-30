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
	Domain  string   `yaml:"domain"`
	Service string   `yaml:"service"`
	Paths   []string `yaml:"paths"`
}

type Service struct {
	Type        string            `yaml:"type"`
	Port        int               `yaml:"port"`
	HealthCheck string            `yaml:"health_check"`
	Env         map[string]string `yaml:"env"`
	// HostMounts is honored only for operator-org projects (the control
	// plane deploying itself needs docker.sock, repos, caddy config).
	HostMounts []string `yaml:"host_mounts"`
	// Web services: Target is the Dockerfile stage holding built assets,
	// Dist the directory inside that stage, SPA enables index.html fallback.
	Target string `yaml:"target"`
	Dist   string `yaml:"dist"`
	SPA    bool   `yaml:"spa"`
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

// Port allocates a host port per deployment (seeded by the deployment id,
// not the sha, so even a same-sha redeploy never fights its predecessor for
// a bind — blue-green needs both alive at once).
func Port(project, service, seed string, avoid map[int]bool) int {
	h := fnv.New32a()
	h.Write([]byte("app/" + project + "/" + service + "/" + seed))
	p := 30000 + int(h.Sum32()%10000)
	for avoid[p] {
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

// DBPort allocates a stable port for a project's database container.
func DBPort(project, resource string) int {
	h := fnv.New32a()
	h.Write([]byte("db/" + project + "/" + resource))
	return 25000 + int(h.Sum32()%4000)
}

// EnsureDatabaseContainers materializes each database resource as a
// long-lived postgres container with its own volume (not blue-greened —
// data outlives deployments); returns the env contract.
func EnsureDatabaseContainers(project, password string, m *Manifest, logf Logf) (map[string]string, error) {
	env := map[string]string{}
	names := []string{}
	for name, r := range m.Resources {
		if r.Type == "database" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	role := "goku_app_" + sanitize(project)

	for i, name := range names {
		cname := fmt.Sprintf("goku-db-%s-%s", sanitize(project), sanitize(name))
		port := DBPort(project, name)
		if out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", cname).Output(); strings.TrimSpace(string(out)) != "true" {
			_ = exec.Command("docker", "rm", "-f", cname).Run() // clear stopped remnant; the volume persists
			logf("starting database container %s (postgres:18) on port %d", cname, port)
			if _, err := run(logf, "docker", "run", "-d", "--name", cname,
				"--restart", "unless-stopped",
				"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
				"-v", cname+":/var/lib/postgresql",
				"-e", "POSTGRES_USER="+role,
				"-e", "POSTGRES_PASSWORD="+password,
				"-e", "POSTGRES_DB="+sanitize(name),
				"postgres:18"); err != nil {
				return nil, err
			}
		}
		ready := false
		for t := 0; t < 30; t++ {
			if err := exec.Command("docker", "exec", cname, "pg_isready", "-U", role).Run(); err == nil {
				ready = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !ready {
			return nil, fmt.Errorf("database container %s did not become ready", cname)
		}
		url := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", role, password, port, sanitize(name))
		env["GOKU_DATABASE_"+strings.ToUpper(sanitize(name))+"_URL"] = url
		if i == 0 {
			env["DATABASE_URL"] = url
		}
		logf("database ready: %s", cname)
	}
	return env, nil
}

// BuildWebImage synthesizes a static-server image for a web service: export
// the named Dockerfile stage, take its dist directory, serve it with caddy
// (SPA fallback optional).
func BuildWebImage(repoPath, project, sha string, svc Service, logf Logf) (string, error) {
	if svc.Target == "" || svc.Dist == "" {
		return "", fmt.Errorf("web service needs target (Dockerfile stage) and dist (asset path in that stage)")
	}
	image := fmt.Sprintf("goku-app/%s-web:%s", project, sha[:12])
	dir, err := os.MkdirTemp("", "goku-web-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	ctxDir := dir + "/ctx"
	outDir := dir + "/out"
	os.MkdirAll(ctxDir, 0o755)
	archive := exec.Command("git", "--git-dir", repoPath, "archive", sha)
	untar := exec.Command("tar", "-x", "-C", ctxDir)
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

	logf("exporting web assets (stage %s)", svc.Target)
	if _, err := run(logf, "docker", "buildx", "build", "--target", svc.Target,
		"--output", "type=local,dest="+outDir, ctxDir); err != nil {
		return "", err
	}
	assets := outDir + "/" + strings.TrimPrefix(svc.Dist, "/")
	if _, err := os.Stat(assets); err != nil {
		return "", fmt.Errorf("dist path %s not found in stage %s", svc.Dist, svc.Target)
	}

	serveDir := dir + "/serve"
	os.MkdirAll(serveDir, 0o755)
	if _, err := run(logf, "cp", "-r", assets, serveDir+"/dist"); err != nil {
		return "", err
	}
	// admin off: host networking would collide with the host caddy's :2019.
	caddyfile := "{\n\tadmin off\n}\n:{$PORT}\nroot * /srv\nencode gzip\nfile_server\n"
	if svc.SPA {
		caddyfile = "{\n\tadmin off\n}\n:{$PORT}\nroot * /srv\nencode gzip\ntry_files {path} /index.html\nfile_server\n"
	}
	os.WriteFile(serveDir+"/Caddyfile", []byte(caddyfile), 0o644)
	os.WriteFile(serveDir+"/Dockerfile", []byte("FROM caddy:2-alpine\nCOPY dist /srv\nCOPY Caddyfile /etc/caddy/Caddyfile\n"), 0o644)
	logf("building web image %s", image)
	if _, err := run(logf, "docker", "build", "-q", "-t", image, serveDir); err != nil {
		return "", err
	}
	return image, nil
}

// ServiceContainerPrefix is the docker name prefix for a project's service
// containers; DBContainerName the (stable) name for a database resource.
func ServiceContainerPrefix(project string) string { return "goku-svc-" + sanitize(project) + "-" }

func DBContainerName(project, resource string) string {
	return "goku-db-" + sanitize(project) + "-" + sanitize(resource)
}

// Run starts a service container (host networking; the app must honor PORT).
func Run(project, service, image string, port int, env map[string]string, hostMounts []string, logf Logf) (string, error) {
	name := fmt.Sprintf("goku-svc-%s-%s-%d", sanitize(project), sanitize(service), time.Now().Unix())
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

// StopPrevious stops and removes older service containers for the project
// (both current goku-svc-* and legacy goku-<project>-* names), keeping the
// containers from this deployment. Database containers are never touched.
func StopPrevious(project string, keep map[string]bool, logf Logf) {
	for _, prefix := range []string{"goku-svc-" + sanitize(project) + "-", "goku-" + sanitize(project) + "-"} {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}").Output()
		if err != nil {
			continue
		}
		for _, name := range strings.Fields(string(out)) {
			if keep[name] || strings.HasPrefix(name, "goku-db-") {
				continue
			}
			logf("stopping previous container %s", name)
			_ = exec.Command("docker", "rm", "-f", name).Run()
		}
	}
}

// WriteRoutes regenerates the Caddy site blocks for all healthy apps and
// reloads Caddy.
// SiteEntry is one routed backend on a host: path-matched or fallback.
type SiteEntry struct {
	Service string   `json:"service"`
	Paths   []string `json:"paths,omitempty"`
	Port    int      `json:"port"`
}

// WriteRoutes regenerates the Caddy site blocks: per host, path-matched
// entries first (manifest order), then the fallback service.
func WriteRoutes(t Target, sites map[string][]SiteEntry, logf Logf) error {
	if t.AppsCaddyFile == "" {
		logf("no apps caddy file configured — skipping routing")
		return nil
	}
	var b strings.Builder
	b.WriteString("# generated by gokud — one site block per healthy app deployment\n")
	hosts := make([]string, 0, len(sites))
	for h := range sites {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		entries := sites[h]
		// flush_interval -1: stream immediately (live log tails, SSE).
		if len(entries) == 1 && len(entries[0].Paths) == 0 {
			fmt.Fprintf(&b, "%s {\n\treverse_proxy localhost:%d {\n\t\tflush_interval -1\n\t}\n}\n", h, entries[0].Port)
			continue
		}
		fmt.Fprintf(&b, "%s {\n", h)
		for i, e := range entries {
			if len(e.Paths) > 0 {
				fmt.Fprintf(&b, "\t@m%d path %s\n\thandle @m%d {\n\t\treverse_proxy localhost:%d {\n\t\t\tflush_interval -1\n\t\t}\n\t}\n", i, strings.Join(e.Paths, " "), i, e.Port)
			}
		}
		for _, e := range entries {
			if len(e.Paths) == 0 {
				fmt.Fprintf(&b, "\thandle {\n\t\treverse_proxy localhost:%d {\n\t\t\tflush_interval -1\n\t\t}\n\t}\n", e.Port)
				break
			}
		}
		b.WriteString("}\n")
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
