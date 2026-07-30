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

alter table projects add column if not exists upstream text not null default '';

drop table if exists changesets;

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
	ID     string `json:"id"`
	OrgID  string `json:"org_id"`
	Name   string `json:"name"`
	Region string `json:"region"`
	Status string `json:"status"`
	// Upstream is "owner/repo" for GitHub-linked (imported) projects: GitHub
	// is the source of truth and goku mirrors it.
	Upstream  string    `json:"upstream"`
	CreatedAt time.Time `json:"created_at"`
}

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
	if _, err := pool.Exec(ctx, schema+usersSchema+deploymentsSchema+secretsSchema+instancesSchema); err != nil {
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
		select p.id, p.org_id, p.name, p.region, p.status, p.upstream, p.created_at
		from projects p where p.org_id = $1 order by p.created_at desc`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Status, &p.Upstream, &p.CreatedAt); err != nil {
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
		select p.id, p.org_id, p.name, p.region, p.status, p.upstream, p.created_at
		from projects p
		where p.org_id = $1 and (p.id::text = $2 or p.name = lower($2))`,
		orgID, ref).Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Status, &p.Upstream, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q: %w", ref, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetProjectUpstream links a project to its GitHub source of truth.
func (s *Store) SetProjectUpstream(ctx context.Context, projectID, upstream string) error {
	_, err := s.pool.Exec(ctx, `update projects set upstream = $1 where id = $2`, upstream, projectID)
	return err
}

func (s *Store) RecordGitPush(ctx context.Context, orgID, projectRef, actor, branch, sha string) {
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return
	}
	s.audit(ctx, orgID, actor, "git.push", "project/"+p.Name, map[string]any{"branch": branch, "head": short(sha)})
}

// RecordMerge audits a branch merge into main.
func (s *Store) RecordMerge(ctx context.Context, orgID, projectName, actor, branch, mainSHA string) {
	s.audit(ctx, orgID, actor, "branch.merge", "project/"+projectName,
		map[string]any{"branch": branch, "main": short(mainSHA)})
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
