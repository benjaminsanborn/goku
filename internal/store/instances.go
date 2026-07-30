package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const instancesSchema = `
create table if not exists instances (
	id uuid primary key default gen_random_uuid(),
	org_id uuid not null references organizations(id),
	name text not null,
	driver text not null default 'ssh',
	address text not null default '',
	ssh_key text not null default '',
	host_key text not null default '',
	status text not null default 'verifying',
	facts jsonb not null default '{}',
	check_log text not null default '',
	created_at timestamptz not null default now(),
	last_checked_at timestamptz,
	unique (org_id, name)
);
`

type Instance struct {
	ID            string         `json:"id"`
	OrgID         string         `json:"org_id"`
	Name          string         `json:"name"`
	Driver        string         `json:"driver"`
	Address       string         `json:"address"`
	SSHKey        string         `json:"-"` // write-only, like secrets
	Status        string         `json:"status"`
	Facts         map[string]any `json:"facts"`
	CheckLog      string         `json:"check_log"`
	CreatedAt     time.Time      `json:"created_at"`
	LastCheckedAt *time.Time     `json:"last_checked_at"`
}

func (s *Store) CreateInstance(ctx context.Context, orgID, name, driver, address, sshKey, actor string) (*Instance, error) {
	if name == "" {
		return nil, errors.New("instance name is required")
	}
	inst := &Instance{OrgID: orgID, Name: name, Driver: driver, Address: address, Status: "verifying", Facts: map[string]any{}}
	err := s.pool.QueryRow(ctx, `
		insert into instances (org_id, name, driver, address, ssh_key) values ($1, $2, $3, $4, $5)
		returning id, created_at`, orgID, name, driver, address, sshKey).Scan(&inst.ID, &inst.CreatedAt)
	if err != nil {
		if errAlready(err) {
			return nil, fmt.Errorf("instance %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, orgID, actor, "instance.add", "instance/"+name, map[string]any{"driver": driver})
	return inst, nil
}

// EnsureLocalInstance registers the control-plane host itself as an ordinary
// fleet member of the operator's org (nothing special about it — see design
// doc 10).
func (s *Store) EnsureLocalInstance(ctx context.Context, name string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		insert into instances (org_id, name, driver, status)
		values ($1, $2, 'local', 'verifying')
		on conflict (org_id, name) do update set driver = 'local'
		returning id`, s.DefaultOrgID, name).Scan(&id)
	return id, err
}

func (s *Store) ListInstances(ctx context.Context, orgID string) ([]Instance, error) {
	rows, err := s.pool.Query(ctx, `
		select id, org_id, name, driver, address, status, facts, check_log, created_at, last_checked_at
		from instances where org_id = $1 order by created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		var i Instance
		var facts []byte
		if err := rows.Scan(&i.ID, &i.OrgID, &i.Name, &i.Driver, &i.Address, &i.Status, &facts,
			&i.CheckLog, &i.CreatedAt, &i.LastCheckedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(facts, &i.Facts)
		out = append(out, i)
	}
	return out, rows.Err()
}

// GetInstance includes the SSH key — server-side use only.
func (s *Store) GetInstance(ctx context.Context, orgID, id string) (*Instance, error) {
	i := &Instance{}
	var facts []byte
	err := s.pool.QueryRow(ctx, `
		select id, org_id, name, driver, address, ssh_key, status, facts, check_log, created_at, last_checked_at
		from instances where org_id = $1 and id::text = $2`, orgID, id).
		Scan(&i.ID, &i.OrgID, &i.Name, &i.Driver, &i.Address, &i.SSHKey, &i.Status, &facts,
			&i.CheckLog, &i.CreatedAt, &i.LastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("instance: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(facts, &i.Facts)
	return i, nil
}

// GetInstanceByName includes the SSH key — server-side use only.
func (s *Store) GetInstanceByName(ctx context.Context, orgID, name string) (*Instance, error) {
	i := &Instance{}
	var facts []byte
	err := s.pool.QueryRow(ctx, `
		select id, org_id, name, driver, address, ssh_key, status, facts, check_log, created_at, last_checked_at
		from instances where org_id = $1 and name = $2`, orgID, name).
		Scan(&i.ID, &i.OrgID, &i.Name, &i.Driver, &i.Address, &i.SSHKey, &i.Status, &facts,
			&i.CheckLog, &i.CreatedAt, &i.LastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("instance: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(facts, &i.Facts)
	return i, nil
}

func (s *Store) DeleteInstance(ctx context.Context, orgID, id, actor string) error {
	var name, driver string
	err := s.pool.QueryRow(ctx, `delete from instances where org_id = $1 and id::text = $2 returning name, driver`, orgID, id).Scan(&name, &driver)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("instance: %w", ErrNotFound)
	}
	if err == nil {
		s.audit(ctx, orgID, actor, "instance.remove", "instance/"+name, nil)
	}
	return err
}

// SetInstanceCheck records a verification run's outcome.
func (s *Store) SetInstanceCheck(ctx context.Context, id, status, checkLog string, facts map[string]any) {
	factsJSON, _ := json.Marshal(facts)
	_, _ = s.pool.Exec(ctx, `
		update instances set status = $1, check_log = $2, facts = $3, last_checked_at = now()
		where id = $4`, status, checkLog, factsJSON, id)
}

func errAlready(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}
