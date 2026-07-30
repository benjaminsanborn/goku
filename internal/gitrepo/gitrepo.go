// Package gitrepo manages the platform's bare git repositories and the
// plumbing operations the changeset model needs (diffs, commits, ff merges).
package gitrepo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// emptyTree is git's well-known empty tree object, used as the diff base for
// repos whose main branch has no commits yet.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

const preReceiveHook = `#!/bin/sh
# main only moves via changeset merge; the first push (repo bootstrap) is allowed.
zero=0000000000000000000000000000000000000000
while read old new ref; do
  if [ "$ref" = "refs/heads/main" ] && [ "$old" != "$zero" ]; then
    echo "error: main is protected — open a changeset and merge it instead" >&2
    exit 1
  fi
done
exit 0
`

type File struct {
	Path    string
	Content string
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// EnsureBareRepo initializes a bare repo with hooks if it does not exist.
func EnsureBareRepo(path string) error {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if _, err := git("", "init", "--bare", "--initial-branch=main", path); err != nil {
		return err
	}
	return InstallHooks(path)
}

// InstallHooks configures a bare repo for platform serving: push enabled over
// smart HTTP and main protected by the pre-receive hook.
func InstallHooks(path string) error {
	if _, err := git(path, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "hooks", "pre-receive"), []byte(preReceiveHook), 0o755)
}

// CloneBareFrom imports an external repository (full history, branches, tags)
// as a platform bare repo, normalizes main as the default branch, and
// installs hooks. The path must not already contain a repo.
func CloneBareFrom(url, path string) error {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return fmt.Errorf("repo already exists")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := git("", "clone", "--bare", "--quiet", url, path); err != nil {
		return err
	}
	// Normalize: main must exist and be HEAD (the platform's protected branch).
	if _, err := Head(path, "main"); err != nil {
		defaultRef, err := git(path, "symbolic-ref", "HEAD")
		if err != nil {
			return err
		}
		defaultSHA, err := git(path, "rev-parse", defaultRef)
		if err != nil {
			return fmt.Errorf("imported repo has no commits")
		}
		if _, err := git(path, "update-ref", "refs/heads/main", defaultSHA); err != nil {
			return err
		}
	}
	if _, err := git(path, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	return InstallHooks(path)
}

// HasFile reports whether a path exists at the tip of a branch.
func HasFile(path, branch, file string) bool {
	_, err := git(path, "cat-file", "-e", branch+":"+file)
	return err == nil
}

// FileAt returns a file's content at the tip of a branch.
func FileAt(path, branch, file string) (string, error) {
	return git(path, "show", branch+":"+file)
}

type Branch struct {
	Name        string    `json:"name"`
	SHA         string    `json:"sha"`
	Subject     string    `json:"subject"`
	CommittedAt time.Time `json:"committed_at"`
}

// Branches lists heads, main first, then most recently committed.
func Branches(path string) ([]Branch, error) {
	out, err := git(path, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)%00%(objectname)%00%(committerdate:iso8601-strict)%00%(subject)", "refs/heads")
	if err != nil {
		return nil, err
	}
	branches := []Branch{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x00", 4)
		if len(parts) < 4 {
			continue
		}
		t, _ := time.Parse(time.RFC3339, parts[2])
		b := Branch{Name: parts[0], SHA: parts[1], CommittedAt: t, Subject: parts[3]}
		if b.Name == "main" {
			branches = append([]Branch{b}, branches...)
		} else {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// Refs returns branch name → sha for all heads.
func Refs(path string) (map[string]string, error) {
	out, err := git(path, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if parts := strings.Fields(line); len(parts) == 2 {
			refs[parts[0]] = parts[1]
		}
	}
	return refs, nil
}

func Head(path, branch string) (string, error) {
	return git(path, "rev-parse", "refs/heads/"+branch)
}

// DiffFiles returns the files changed between main (or the empty tree) and
// branch, with new content. Large and binary files are elided, not omitted.
func DiffFiles(path, branch string) ([]File, error) {
	head, err := Head(path, branch)
	if err != nil {
		return nil, fmt.Errorf("branch %q not found", branch)
	}
	base := emptyTree
	if mainSHA, err := Head(path, "main"); err == nil {
		base = mainSHA
	}
	out, err := git(path, "diff", "--name-status", base, head)
	if err != nil {
		return nil, err
	}
	files := []File{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		status, p := parts[0], parts[len(parts)-1]
		if len(files) >= 100 {
			files = append(files, File{Path: "…", Content: "(more files elided)"})
			break
		}
		if strings.HasPrefix(status, "D") {
			files = append(files, File{Path: p, Content: "(deleted)"})
			continue
		}
		content, err := git(path, "show", head+":"+p)
		switch {
		case err != nil:
			content = "(unreadable)"
		case strings.ContainsRune(content, 0):
			content = "(binary)"
		case len(content) > 64*1024:
			content = content[:64*1024] + "\n… (truncated)"
		}
		files = append(files, File{Path: p, Content: content})
	}
	return files, nil
}

// CommitFiles creates branch from main (or an unborn root) containing the
// given files, committed as the actor, via a temporary local clone. Used when
// an agent proposes a changeset without a local workspace.
func CommitFiles(path, branch, message, actor string, files []File) (string, error) {
	tmp, err := os.MkdirTemp("", "goku-commit-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "work")
	if _, err := git("", "clone", "--quiet", path, work); err != nil {
		return "", err
	}
	if _, err := git(work, "checkout", "-b", branch); err != nil {
		return "", err
	}
	for _, f := range files {
		p := filepath.Join(work, filepath.Clean("/"+f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			return "", err
		}
	}
	if _, err := git(work, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git(work, "-c", "user.name="+actor, "-c", "user.email=agent@goku.host",
		"commit", "-m", message); err != nil {
		return "", err
	}
	if _, err := git(work, "push", "--quiet", "origin", branch); err != nil {
		return "", err
	}
	return Head(path, branch)
}

// MergeFF fast-forwards main to branch. Returns the new main sha.
func MergeFF(path, branch string) (string, error) {
	head, err := Head(path, branch)
	if err != nil {
		return "", fmt.Errorf("branch %q not found", branch)
	}
	if mainSHA, err := Head(path, "main"); err == nil {
		if _, err := git(path, "merge-base", "--is-ancestor", mainSHA, head); err != nil {
			return "", fmt.Errorf("branch %q is not fast-forward from main — rebase it on main and push again", branch)
		}
	}
	if _, err := git(path, "update-ref", "refs/heads/main", head); err != nil {
		return "", err
	}
	return head, nil
}

// ExecPath returns git's exec-path (where git-http-backend lives).
func ExecPath() (string, error) {
	return git("", "--exec-path")
}
