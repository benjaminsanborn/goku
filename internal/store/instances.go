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

alter table instances add column if not exists provider_id uuid references cloud_providers(id);
alter table instances add column if not exists external_id text not null default '';
alter table instances add column if not exists key_name text not null default '';
`

type Instance struct {
	ID       string         `json:"id"`
	OrgID    string         `json:"org_id"`
	Name     string         `json:"name"`
	Driver   string         `json:"driver"`
	Address  string         `json:"address"`
	SSHKey   string         `json:"-"` // write-only, like secrets
	Status   string         `json:"status"`
	Facts    map[string]any `json:"facts"`
	CheckLog string         `json:"check_log"`
	// ProviderID, ExternalID and KeyName are set for instances goku
	// provisioned itself: which cloud account, and what to tear down.
	ProviderID    string     `json:"provider_id"`
	ExternalID    string     `json:"external_id"`
	KeyName       string     `json:"key_name"`
	CreatedAt     time.Time  `json:"created_at"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
}

// NewInstance describes a fleet member being registered, whether an operator
// attached it by hand or goku provisioned it from a cloud provider.
type NewInstance struct {
	Name       string
	Driver     string
	Address    string
	SSHKey     string
	ProviderID string // empty for hand-attached machines
	ExternalID string // e.g. an EC2 instance id
	KeyName    string // provider-side key pair to clean up on removal
}

func (s *Store) CreateInstance(ctx context.Context, orgID string, in NewInstance, actor string) (*Instance, error) {
	if in.Name == "" {
		return nil, errors.New("instance name is required")
	}
	inst := &Instance{
		OrgID: orgID, Name: in.Name, Driver: in.Driver, Address: in.Address, Status: "verifying",
		Facts: map[string]any{}, ProviderID: in.ProviderID, ExternalID: in.ExternalID, KeyName: in.KeyName,
	}
	err := s.pool.QueryRow(ctx, `
		insert into instances (org_id, name, driver, address, ssh_key, provider_id, external_id, key_name)
		values ($1, $2, $3, $4, $5, nullif($6, '')::uuid, $7, $8)
		returning id, created_at`,
		orgID, in.Name, in.Driver, in.Address, in.SSHKey, in.ProviderID, in.ExternalID, in.KeyName).
		Scan(&inst.ID, &inst.CreatedAt)
	if err != nil {
		if errAlready(err) {
			return nil, fmt.Errorf("instance %q already exists", in.Name)
		}
		return nil, err
	}
	s.audit(ctx, orgID, actor, "instance.add", "instance/"+in.Name, map[string]any{"driver": in.Driver})
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
		select id, org_id, name, driver, address, status, facts, check_log,
		       coalesce(provider_id::text, ''), external_id, key_name, created_at, last_checked_at
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
			&i.CheckLog, &i.ProviderID, &i.ExternalID, &i.KeyName, &i.CreatedAt, &i.LastCheckedAt); err != nil {
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
		select id, org_id, name, driver, address, ssh_key, status, facts, check_log,
		       coalesce(provider_id::text, ''), external_id, key_name, created_at, last_checked_at
		from instances where org_id = $1 and id::text = $2`, orgID, id).
		Scan(&i.ID, &i.OrgID, &i.Name, &i.Driver, &i.Address, &i.SSHKey, &i.Status, &facts,
			&i.CheckLog, &i.ProviderID, &i.ExternalID, &i.KeyName, &i.CreatedAt, &i.LastCheckedAt)
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
		select id, org_id, name, driver, address, ssh_key, status, facts, check_log,
		       coalesce(provider_id::text, ''), external_id, key_name, created_at, last_checked_at
		from instances where org_id = $1 and name = $2`, orgID, name).
		Scan(&i.ID, &i.OrgID, &i.Name, &i.Driver, &i.Address, &i.SSHKey, &i.Status, &facts,
			&i.CheckLog, &i.ProviderID, &i.ExternalID, &i.KeyName, &i.CreatedAt, &i.LastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("instance: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(facts, &i.Facts)
	return i, nil
}

// DeleteInstance removes the record and reports what it was, so a caller can
// tear down the cloud resources behind a provisioned instance.
func (s *Store) DeleteInstance(ctx context.Context, orgID, id, actor string) (*Instance, error) {
	i := &Instance{OrgID: orgID}
	err := s.pool.QueryRow(ctx, `
		delete from instances where org_id = $1 and id::text = $2
		returning id, name, driver, coalesce(provider_id::text, ''), external_id, key_name`, orgID, id).
		Scan(&i.ID, &i.Name, &i.Driver, &i.ProviderID, &i.ExternalID, &i.KeyName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("instance: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	s.audit(ctx, orgID, actor, "instance.remove", "instance/"+i.Name, nil)
	return i, nil
}

// AttachProvisioned records the address and key of a machine goku just
// launched, so the ssh driver can reach it.
func (s *Store) AttachProvisioned(ctx context.Context, id, address, sshKey, externalID, keyName string) error {
	_, err := s.pool.Exec(ctx, `
		update instances set address = $1, ssh_key = $2, external_id = $3, key_name = $4
		where id = $5`, address, sshKey, externalID, keyName, id)
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
