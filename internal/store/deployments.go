package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const deploymentsSchema = `
create table if not exists deployments (
	id uuid primary key default gen_random_uuid(),
	project_id uuid not null references projects(id),
	branch text not null,
	sha text not null,
	image text not null default '',
	port int not null default 0,
	status text not null default 'building',
	actor text not null,
	domain text not null default '',
	url text not null default '',
	log text not null default '',
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now()
);

alter table projects add column if not exists app_db_password text not null default '';
alter table deployments add column if not exists domain text not null default '';
`

type Deployment struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Branch    string    `json:"branch"`
	SHA       string    `json:"sha"`
	Image     string    `json:"image"`
	Port      int       `json:"port"`
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	Domain    string    `json:"domain"`
	URL       string    `json:"url"`
	Log       string    `json:"log"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) CreateDeployment(ctx context.Context, orgID string, p *Project, branch, sha, actor, domain string) (*Deployment, error) {
	d := &Deployment{ProjectID: p.ID, Branch: branch, SHA: sha, Actor: actor, Status: "building", Domain: domain}
	err := s.pool.QueryRow(ctx, `
		insert into deployments (project_id, branch, sha, actor, domain) values ($1, $2, $3, $4, $5)
		returning id, created_at, updated_at`,
		p.ID, branch, sha, actor, domain).Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, orgID, actor, "deploy.start", "project/"+p.Name,
		map[string]any{"branch": branch, "sha": short(sha)})
	return d, nil
}

// AppendDeployLog adds progress lines to the deployment's build/deploy log.
func (s *Store) AppendDeployLog(ctx context.Context, id, line string) {
	_, _ = s.pool.Exec(ctx, `update deployments set log = log || $1 || E'\n', updated_at = now() where id = $2`, line, id)
}

func (s *Store) SetDeploymentState(ctx context.Context, id, status string, fields map[string]any) {
	set := `status = $1, updated_at = now()`
	args := []any{status}
	i := 2
	for _, k := range []string{"image", "port", "url"} {
		if v, ok := fields[k]; ok {
			set += fmt.Sprintf(`, %s = $%d`, k, i)
			args = append(args, v)
			i++
		}
	}
	args = append(args, id)
	_, _ = s.pool.Exec(ctx, fmt.Sprintf(`update deployments set %s where id = $%d`, set, i), args...)
}

// SupersedePrevious marks earlier healthy deployments of a project as stopped.
func (s *Store) SupersedePrevious(ctx context.Context, projectID, exceptID string) {
	_, _ = s.pool.Exec(ctx, `
		update deployments set status = 'stopped', updated_at = now()
		where project_id = $1 and id <> $2 and status in ('healthy', 'starting')`, projectID, exceptID)
}

func (s *Store) ListDeployments(ctx context.Context, orgID, projectRef string, limit int) ([]Deployment, error) {
	p, err := s.GetProject(ctx, orgID, projectRef)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.pool.Query(ctx, `
		select id, project_id, branch, sha, image, port, status, actor, url, log, created_at, updated_at
		from deployments where project_id = $1 order by created_at desc limit $2`, p.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Branch, &d.SHA, &d.Image, &d.Port, &d.Status,
			&d.Actor, &d.URL, &d.Log, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ActiveDeployment returns the project's current healthy deployment, if any.
func (s *Store) ActiveDeployment(ctx context.Context, projectID string) (*Deployment, error) {
	var d Deployment
	err := s.pool.QueryRow(ctx, `
		select id, project_id, branch, sha, image, port, status, actor, url, log, created_at, updated_at
		from deployments where project_id = $1 and status = 'healthy'
		order by created_at desc limit 1`, projectID).
		Scan(&d.ID, &d.ProjectID, &d.Branch, &d.SHA, &d.Image, &d.Port, &d.Status,
			&d.Actor, &d.URL, &d.Log, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

// AppDBPassword returns the project's app-database password, minting it on
// first use.
func (s *Store) AppDBPassword(ctx context.Context, projectID string) (string, error) {
	var pw string
	if err := s.pool.QueryRow(ctx, `select app_db_password from projects where id = $1`, projectID).Scan(&pw); err != nil {
		return "", err
	}
	if pw != "" {
		return pw, nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	pw = hex.EncodeToString(raw)
	_, err := s.pool.Exec(ctx, `update projects set app_db_password = $1 where id = $2 and app_db_password = ''`, pw, projectID)
	if err != nil {
		return "", err
	}
	// Re-read in case of a concurrent mint.
	err = s.pool.QueryRow(ctx, `select app_db_password from projects where id = $1`, projectID).Scan(&pw)
	return pw, err
}

type HealthyRoute struct {
	Project string
	Domain  string // custom domain from the manifest, or "" for the default
	Port    int
}

// AllHealthyDeployments lists every healthy deployment across orgs — used to
// regenerate the proxy routes file.
func (s *Store) AllHealthyDeployments(ctx context.Context) ([]HealthyRoute, error) {
	rows, err := s.pool.Query(ctx, `
		select p.name, d.domain, d.port from deployments d join projects p on p.id = d.project_id
		where d.status = 'healthy'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []HealthyRoute{}
	for rows.Next() {
		var r HealthyRoute
		if err := rows.Scan(&r.Project, &r.Domain, &r.Port); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}
