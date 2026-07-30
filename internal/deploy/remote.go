package deploy

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Remote is the ssh driver's connection to a fleet instance.
type Remote struct {
	Target     string // user@host
	Port       string
	KeyFile    string
	KnownHosts string
}

func (r *Remote) sshArgs(cmd string) []string {
	return []string{
		"-i", r.KeyFile, "-p", r.Port,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + r.KnownHosts,
		r.Target, cmd,
	}
}

func (r *Remote) run(logf Logf, cmd string) (string, error) {
	out, err := exec.Command("ssh", r.sshArgs(cmd)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			logf("%s", tail(text, 20))
		}
		return text, fmt.Errorf("ssh: %w", err)
	}
	return text, nil
}

// RunCommand executes one command on the instance (no logging).
func (r *Remote) RunCommand(cmd string) (string, error) {
	return r.run(func(string, ...any) {}, cmd)
}

// Host is the instance's address without user or port — what the central
// proxy dials for routed traffic.
func (r *Remote) Host() string {
	host := r.Target
	if _, h, ok := strings.Cut(r.Target, "@"); ok {
		host = h
	}
	return host
}

// shq single-quotes a string for a remote shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// RemoteBuild ships the repo at sha to the instance as a tar stream and
// builds there: git archive | ssh docker build - . No registry, no image
// transfer, and the instance's own arch builds its own image.
func RemoteBuild(repoPath, project, sha string, r *Remote, logf Logf) (string, error) {
	image := fmt.Sprintf("goku-app/%s:%s", project, sha[:12])
	logf("remote build %s on %s", image, r.Target)
	archive := exec.Command("git", "--git-dir", repoPath, "archive", sha)
	build := exec.Command("ssh", r.sshArgs("docker build -q -t "+image+" -")...)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	build.Stdin = pipe
	var out strings.Builder
	build.Stdout = &out
	build.Stderr = &out
	if err := build.Start(); err != nil {
		return "", err
	}
	if err := archive.Run(); err != nil {
		return "", fmt.Errorf("git archive: %w", err)
	}
	if err := build.Wait(); err != nil {
		logf("%s", tail(out.String(), 25))
		return "", fmt.Errorf("remote docker build: %w", err)
	}
	return image, nil
}

// RemoteEnsureDatabaseContainers mirrors EnsureDatabaseContainers over ssh;
// apps reach their databases on the instance's own loopback.
func RemoteEnsureDatabaseContainers(project, branch, password string, m *Manifest, r *Remote, logf Logf) (map[string]string, error) {
	env := map[string]string{}
	names := []string{}
	for name, res := range m.Resources {
		if res.Type == "database" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return env, nil
	}
	role := "goku_app_" + sanitize(project)
	for i, name := range names {
		cname := DBContainerName(project, branch, name)
		port := DBPort(project+"/"+branch, name)
		if branch == "main" {
			port = DBPort(project, name)
		}
		running, _ := r.run(logf, "docker inspect -f '{{.State.Running}}' "+cname+" 2>/dev/null || true")
		if strings.TrimSpace(running) != "true" {
			r.run(logf, "docker rm -f "+cname+" 2>/dev/null || true")
			logf("starting database container %s (postgres:18) on %s:%d", cname, r.Host(), port)
			if _, err := r.run(logf, fmt.Sprintf(
				"docker run -d --name %s --restart unless-stopped -p 127.0.0.1:%d:5432 -v %s:/var/lib/postgresql -e POSTGRES_USER=%s -e POSTGRES_PASSWORD=%s -e POSTGRES_DB=%s postgres:18",
				cname, port, cname, role, shq(password), sanitize(name))); err != nil {
				return nil, err
			}
		}
		ready := false
		for t := 0; t < 30; t++ {
			if _, err := r.run(func(string, ...any) {}, "docker exec "+cname+" pg_isready -U "+role+" -d "+sanitize(name)); err == nil {
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

// RemoteRun starts a service container on the instance.
func RemoteRun(project, branch, service, image string, port int, env map[string]string, r *Remote, logf Logf) (string, error) {
	name := fmt.Sprintf("%s%s-%d", ServiceContainerPrefix(project, branch), sanitize(service), time.Now().Unix())
	var b strings.Builder
	fmt.Fprintf(&b, "docker run -d --name %s --network host --restart unless-stopped", name)
	fmt.Fprintf(&b, " --volume goku-app-%s:/data --env PORT=%d --env DATA_DIR=/data", sanitize(project), port)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	for _, k := range keys {
		fmt.Fprintf(&b, " --env %s=%s", k, shq(env[k]))
	}
	b.WriteString(" " + image)
	logf("starting container %s on %s:%d", name, r.Host(), port)
	if _, err := r.run(logf, b.String()); err != nil {
		return "", err
	}
	return name, nil
}

// RemoteHealthCheck polls the app on the instance's loopback via ssh.
func RemoteHealthCheck(r *Remote, port int, path string, timeout time.Duration, logf Logf) error {
	if path == "" {
		path = "/"
	}
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	logf("health check %s (via ssh)", url)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := r.run(func(string, ...any) {}, "curl -fsS -o /dev/null --max-time 5 "+shq(url)+" || wget -q -O /dev/null "+shq(url)); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("no healthy response from %s within %s", url, timeout)
}

// RemoteStopPrevious removes the environment's older service containers.
func RemoteStopPrevious(project, branch string, keep map[string]bool, r *Remote, logf Logf) {
	prefix := ServiceContainerPrefix(project, branch)
	out, err := r.run(logf, "docker ps -a --filter name="+prefix+" --format '{{.Names}}'")
	if err != nil {
		return
	}
	for _, name := range strings.Fields(out) {
		if keep[name] || strings.HasPrefix(name, "goku-db-") {
			continue
		}
		logf("stopping previous container %s", name)
		r.run(func(string, ...any) {}, "docker rm -f "+name)
	}
}

// RemoteStopEnvironment tears down a branch environment on the instance.
func RemoteStopEnvironment(project, branch string, r *Remote, logf Logf) {
	for _, prefix := range []string{ServiceContainerPrefix(project, branch), "goku-db-" + sanitize(project) + "--" + EnvSlug(branch) + "--"} {
		out, err := r.run(logf, "docker ps -a --filter name="+prefix+" --format '{{.Names}}'")
		if err != nil {
			continue
		}
		for _, name := range strings.Fields(out) {
			logf("stopping %s", name)
			r.run(func(string, ...any) {}, "docker rm -f "+name)
		}
	}
}
