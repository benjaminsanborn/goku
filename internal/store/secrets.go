package store

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

const secretsSchema = `
create table if not exists project_secrets (
	project_id uuid not null references projects(id),
	key text not null,
	value text not null,
	updated_at timestamptz not null default now(),
	primary key (project_id, key)
);
`

var secretKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

type SecretMeta struct {
	Key       string    `json:"key"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SetSecret upserts a project secret. Values are write-only through the API;
// they are stored for injection into deployed containers. (At-rest encryption
// arrives with the AWS/KMS phase — the DB lives on the same host that injects
// these into containers.)
func (s *Store) SetSecret(ctx context.Context, orgID string, p *Project, key, value, actor string) error {
	if !secretKeyRe.MatchString(key) {
		return fmt.Errorf("secret keys must look like env vars (A-Z, 0-9, _), got %q", key)
	}
	_, err := s.pool.Exec(ctx, `
		insert into project_secrets (project_id, key, value) values ($1, $2, $3)
		on conflict (project_id, key) do update set value = excluded.value, updated_at = now()`,
		p.ID, key, value)
	if err != nil {
		return err
	}
	s.audit(ctx, orgID, actor, "secret.set", "project/"+p.Name, map[string]any{"key": key})
	return nil
}

func (s *Store) DeleteSecret(ctx context.Context, orgID string, p *Project, key, actor string) error {
	tag, err := s.pool.Exec(ctx, `delete from project_secrets where project_id = $1 and key = $2`, p.ID, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("secret %q: %w", key, ErrNotFound)
	}
	s.audit(ctx, orgID, actor, "secret.delete", "project/"+p.Name, map[string]any{"key": key})
	return nil
}

// ListSecretMeta returns names and timestamps only — values never leave the
// deploy path.
func (s *Store) ListSecretMeta(ctx context.Context, projectID string) ([]SecretMeta, error) {
	rows, err := s.pool.Query(ctx, `select key, updated_at from project_secrets where project_id = $1 order by key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretMeta{}
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Key, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SecretValues is used only by the deploy engine.
func (s *Store) SecretValues(ctx context.Context, projectID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `select key, value from project_secrets where project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ProjectsByUpstream finds every project mirroring a GitHub repo, with its
// org — webhook fan-out is cross-org by design.
func (s *Store) ProjectsByUpstream(ctx context.Context, upstream string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		select p.id, p.org_id, p.name, p.region, p.status, p.upstream, p.created_at
		from projects p where lower(p.upstream) = lower($1)`, upstream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Status, &p.Upstream, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
