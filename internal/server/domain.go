package server

import (
	"context"
	"fmt"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"github.com/benjaminsanborn/goku/internal/store"
)

// createProject creates the project row and its bare git repository together.
func (s *Server) createProject(ctx context.Context, orgID, name, actor string) (*store.Project, error) {
	p, err := s.Store.CreateProject(ctx, orgID, name, actor)
	if err != nil {
		return nil, err
	}
	if err := gitrepo.EnsureBareRepo(s.RepoPath(orgID, p.Name)); err != nil {
		return nil, fmt.Errorf("project created but repo init failed: %w", err)
	}
	return p, nil
}

// mergeBranch fast-forwards main to a branch and deletes the branch —
// the approval action in the branch-based flow.
func (s *Server) mergeBranch(ctx context.Context, orgID, projectRef, branch, actor string) (string, error) {
	if branch == "" || branch == "main" {
		return "", fmt.Errorf("a non-main branch is required")
	}
	p, err := s.Store.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return "", err
	}
	if p.Upstream != "" {
		return "", fmt.Errorf("this project is linked to github.com/%s — merge there; goku syncs automatically", p.Upstream)
	}
	repo := s.RepoPath(orgID, p.Name)
	mainSHA, err := gitrepo.MergeFF(repo, branch)
	if err != nil {
		return "", err
	}
	_ = gitrepo.DeleteBranch(repo, branch)
	s.Store.RecordMerge(ctx, orgID, p.Name, actor, branch, mainSHA)
	return mainSHA, nil
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
