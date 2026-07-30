// Package store is the persistence layer for the goku control plane.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

const schema = `
create table if not exists organizations (
	id uuid primary key default gen_random_uuid(),
	name text not null unique,
	created_at timestamptz not null default now()
);

create table if not exists tokens (
	id uuid primary key default gen_random_uuid(),
	org_id uuid not null references organizations(id),
	label text not null default 'owner',
	token_hash bytea not null unique,
	created_at timestamptz not null default now(),
	last_used_at timestamptz
);

create table if not exists projects (
	id uuid primary key default gen_random_uuid(),
	org_id uuid not null references organizations(id),
	name text not null,
	region text not null default 'us-east-1',
	status text not null default 'ready_to_code',
	created_at timestamptz not null default now(),
	unique (org_id, name)
);

create table if not exists changesets (
	id uuid primary key default gen_random_uuid(),
	project_id uuid not null references projects(id),
	number int not null,
	title text not null,
	description text not null default '',
	branch text not null default '',
	status text not null default 'open',
	opened_by text not null,
	head_sha text not null default '',
	files jsonb not null default '[]',
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now(),
	unique (project_id, number)
);

alter table changesets add column if not exists head_sha text not null default '';

create table if not exists audit_events (
	seq bigserial primary key,
	org_id uuid not null references organizations(id),
	actor text not null,
	action text not null,
	subject text not null,
	detail jsonb not null default '{}',
	at timestamptz not null default now()
);
`

type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Project struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"org_id"`
	Name           string    `json:"name"`
	Region         string    `json:"region"`
	Status         string    `json:"status"`
	ChangesetCount int       `json:"changeset_count"`
	CreatedAt      time.Time `json:"created_at"`
}

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Changeset struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Branch      string    `json:"branch"`
	Status      string    `json:"status"`
	OpenedBy    string    `json:"opened_by"`
	HeadSHA     string    `json:"head_sha"`
	Files       []File    `json:"files"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AuditEvent struct {
	Seq     int64          `json:"seq"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"`
	Subject string         `json:"subject"`
	Detail  map[string]any `json:"detail"`
	At      time.Time      `json:"at"`
}

type Store struct {
	pool *pgxpool.Pool
	// DefaultOrgID backs the root GOKU_TOKEN for single-org deployments.
	DefaultOrgID string
}

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.pool.QueryRow(ctx, `
		insert into organizations (name) values ('default')
		on conflict (name) do update set name = excluded.name
		returning id`).Scan(&s.DefaultOrgID); err != nil {
		return nil, fmt.Errorf("seed default org: %w", err)
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

// --- organizations & tokens ---

func (s *Store) CreateOrg(ctx context.Context, name string) (*Org, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, errors.New("organization name is required")
	}
	o := &Org{Name: name}
	err := s.pool.QueryRow(ctx, `insert into organizations (name) values ($1) returning id`, name).Scan(&o.ID)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("organization %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, o.ID, "user:owner", "org.create", "org/"+name, nil)
	return o, nil
}

func (s *Store) GetOrg(ctx context.Context, id string) (*Org, error) {
	o := &Org{ID: id}
	err := s.pool.QueryRow(ctx, `select name from organizations where id = $1`, id).Scan(&o.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("organization: %w", ErrNotFound)
	}
	return o, err
}

// CreateToken mints an org-scoped bearer token. Only the hash is stored; the
// plaintext is returned exactly once.
func (s *Store) CreateToken(ctx context.Context, orgID, label string) (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "gk_" + hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	if _, err := s.pool.Exec(ctx, `insert into tokens (org_id, label, token_hash) values ($1, $2, $3)`,
		orgID, label, hash[:]); err != nil {
		return "", err
	}
	s.audit(ctx, orgID, "user:owner", "token.create", "token/"+label, nil)
	return token, nil
}

// ResolveToken maps a bearer token to its organization.
func (s *Store) ResolveToken(ctx context.Context, token string) (orgID string, err error) {
	hash := sha256.Sum256([]byte(token))
	err = s.pool.QueryRow(ctx, `select org_id from tokens where token_hash = $1`, hash[:]).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err == nil {
		_, _ = s.pool.Exec(ctx, `update tokens set last_used_at = now() where token_hash = $1`, hash[:])
	}
	return orgID, err
}

// --- projects ---

func (s *Store) CreateProject(ctx context.Context, orgID, name, actor string) (*Project, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, errors.New("project name is required")
	}
	p := &Project{OrgID: orgID, Name: name}
	err := s.pool.QueryRow(ctx, `
		insert into projects (org_id, name) values ($1, $2)
		returning id, region, status, created_at`,
		orgID, name).Scan(&p.ID, &p.Region, &p.Status, &p.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("project %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, orgID, actor, "project.create", "project/"+p.Name, map[string]any{"project_id": p.ID})
	return p, nil
}

func (s *Store) ListProjects(ctx context.Context, orgID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		select p.id, p.org_id, p.name, p.region, p.status, p.created_at,
		       (select count(*) from changesets c where c.project_id = p.id)
		from projects p where p.org_id = $1 order by p.created_at desc`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Status, &p.CreatedAt, &p.ChangesetCount); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// GetProject resolves a project by UUID or by name within the organization.
func (s *Store) GetProject(ctx context.Context, orgID, ref string) (*Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `
		select p.id, p.org_id, p.name, p.region, p.status, p.created_at,
		       (select count(*) from changesets c where c.project_id = p.id)
		from projects p
		where p.org_id = $1 and (p.id::text = $2 or p.name = lower($2))`,
		orgID, ref).Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Status, &p.CreatedAt, &p.ChangesetCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q: %w", ref, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// --- changesets ---

func (s *Store) OpenChangeset(ctx context.Context, orgID, projectRef, title, description, branch, actor, headSHA string, files []File) (*Changeset, error) {
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("changeset title is required")
	}
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return nil, err
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	cs := &Changeset{ProjectID: p.ID, Title: title, Description: description, Branch: branch, OpenedBy: actor, HeadSHA: headSHA, Files: files}
	err = s.pool.QueryRow(ctx, `
		insert into changesets (project_id, number, title, description, branch, opened_by, head_sha, files)
		values ($1, (select coalesce(max(number), 0) + 1 from changesets where project_id = $1), $2, $3, $4, $5, $6, $7)
		returning id, number, status, created_at, updated_at`,
		p.ID, title, description, branch, actor, headSHA, filesJSON).
		Scan(&cs.ID, &cs.Number, &cs.Status, &cs.CreatedAt, &cs.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, orgID, actor, "changeset.open", fmt.Sprintf("project/%s/changeset/%d", p.Name, cs.Number),
		map[string]any{"title": title, "branch": branch, "head": short(headSHA), "files": len(files)})
	return cs, nil
}

// RefreshChangesetForBranch updates open changesets tracking a branch after a push.
func (s *Store) RefreshChangesetForBranch(ctx context.Context, orgID, projectRef, branch, headSHA string, files []File) {
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return
	}
	_, _ = s.pool.Exec(ctx, `
		update changesets set head_sha = $1, files = $2, updated_at = now()
		where project_id = $3 and branch = $4 and status = 'open'`,
		headSHA, filesJSON, p.ID, branch)
}

func (s *Store) RecordGitPush(ctx context.Context, orgID, projectRef, actor, branch, sha string) {
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return
	}
	s.audit(ctx, orgID, actor, "git.push", "project/"+p.Name, map[string]any{"branch": branch, "head": short(sha)})
}

// MarkMerged flips a changeset to merged after the repo fast-forward succeeded.
func (s *Store) MarkMerged(ctx context.Context, orgID string, cs *Changeset, projectName, actor, mainSHA string) error {
	tag, err := s.pool.Exec(ctx, `update changesets set status = 'merged', updated_at = now() where id = $1 and status = 'open'`, cs.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("changeset #%d is not open", cs.Number)
	}
	s.audit(ctx, orgID, actor, "changeset.merge", fmt.Sprintf("project/%s/changeset/%d", projectName, cs.Number),
		map[string]any{"branch": cs.Branch, "main": short(mainSHA)})
	return nil
}

func (s *Store) ListChangesets(ctx context.Context, orgID, projectRef string) ([]Changeset, error) {
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		select id, project_id, number, title, description, branch, status, opened_by, head_sha, files, created_at, updated_at
		from changesets where project_id = $1 order by number desc`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChangesets(rows)
}

func (s *Store) GetChangeset(ctx context.Context, orgID, id string) (*Changeset, error) {
	rows, err := s.pool.Query(ctx, `
		select c.id, c.project_id, c.number, c.title, c.description, c.branch, c.status, c.opened_by, c.head_sha, c.files, c.created_at, c.updated_at
		from changesets c join projects p on p.id = c.project_id
		where c.id::text = $1 and p.org_id = $2`, id, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanChangesets(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("changeset %q: %w", id, ErrNotFound)
	}
	return &list[0], nil
}

func scanChangesets(rows pgx.Rows) ([]Changeset, error) {
	out := []Changeset{}
	for rows.Next() {
		var cs Changeset
		var filesJSON []byte
		if err := rows.Scan(&cs.ID, &cs.ProjectID, &cs.Number, &cs.Title, &cs.Description, &cs.Branch,
			&cs.Status, &cs.OpenedBy, &cs.HeadSHA, &filesJSON, &cs.CreatedAt, &cs.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(filesJSON, &cs.Files); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// --- audit ---

func (s *Store) ListAuditEvents(ctx context.Context, orgID string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		select seq, actor, action, subject, detail, at
		from audit_events where org_id = $1 order by seq desc limit $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		var detail []byte
		if err := rows.Scan(&e.Seq, &e.Actor, &e.Action, &e.Subject, &detail, &e.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) audit(ctx context.Context, orgID, actor, action, subject string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, _ := json.Marshal(detail)
	// Audit failure must not fail the action in the dev slice; production hash-chains this in-transaction.
	_, _ = s.pool.Exec(ctx, `insert into audit_events (org_id, actor, action, subject, detail) values ($1, $2, $3, $4, $5)`,
		orgID, actor, action, subject, detailJSON)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
