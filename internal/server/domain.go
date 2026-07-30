package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"github.com/benjaminsanborn/goku/internal/store"
)

// createProject creates the project row and its bare git repository together.
func (s *Server) createProject(ctx context.Context, name, actor string) (*store.Project, error) {
	p, err := s.Store.CreateProject(ctx, name, actor)
	if err != nil {
		return nil, err
	}
	if err := gitrepo.EnsureBareRepo(s.RepoPath(p.Name)); err != nil {
		return nil, fmt.Errorf("project created but repo init failed: %w", err)
	}
	return p, nil
}

// openChangeset opens a changeset from either a branch the actor already
// pushed, or a set of files the platform commits onto a new branch on their
// behalf (for agents without a local workspace).
func (s *Server) openChangeset(ctx context.Context, projectRef, title, description, branch, actor string, files []store.File) (*store.Changeset, error) {
	p, err := s.Store.GetProject(ctx, projectRef)
	if err != nil {
		return nil, err
	}
	repo := s.RepoPath(p.Name)
	if err := gitrepo.EnsureBareRepo(repo); err != nil {
		return nil, err
	}
	if branch == "" {
		branch = "claude/" + slugify(title)
	}
	if branch == "main" {
		return nil, errors.New("changesets cannot target main directly")
	}

	var head string
	if len(files) > 0 {
		head, err = gitrepo.CommitFiles(repo, branch, title, actor, gitFiles(files))
		if err != nil {
			return nil, fmt.Errorf("commit files: %w", err)
		}
	} else {
		head, err = gitrepo.Head(repo, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %q has not been pushed — push it first or provide files", branch)
		}
	}

	diff, err := gitrepo.DiffFiles(repo, branch)
	if err != nil {
		return nil, err
	}
	return s.Store.OpenChangeset(ctx, p.ID, title, description, branch, actor, head, storeFiles(diff))
}

// mergeChangeset fast-forwards main to the changeset branch and marks it merged.
func (s *Server) mergeChangeset(ctx context.Context, id, actor string) (*store.Changeset, error) {
	cs, err := s.Store.GetChangeset(ctx, id)
	if err != nil {
		return nil, err
	}
	if cs.Status != "open" {
		return nil, fmt.Errorf("changeset #%d is %s, not open", cs.Number, cs.Status)
	}
	p, err := s.Store.GetProject(ctx, cs.ProjectID)
	if err != nil {
		return nil, err
	}
	mainSHA, err := gitrepo.MergeFF(s.RepoPath(p.Name), cs.Branch)
	if err != nil {
		return nil, err
	}
	if err := s.Store.MarkMerged(ctx, cs, p.Name, actor, mainSHA); err != nil {
		return nil, err
	}
	cs.Status = "merged"
	return cs, nil
}

func (s *Server) gitRemoteURL(project string) string {
	return s.BaseURL + "/git/" + project + ".git"
}

func storeFiles(fs []gitrepo.File) []store.File {
	out := make([]store.File, len(fs))
	for i, f := range fs {
		out[i] = store.File{Path: f.Path, Content: f.Content}
	}
	return out
}

func gitFiles(fs []store.File) []gitrepo.File {
	out := make([]gitrepo.File, len(fs))
	for i, f := range fs {
		out[i] = gitrepo.File{Path: f.Path, Content: f.Content}
	}
	return out
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
