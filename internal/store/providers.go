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

const providersSchema = `
create table if not exists cloud_providers (
	id uuid primary key default gen_random_uuid(),
	org_id uuid not null references organizations(id),
	name text not null,
	kind text not null,
	credentials jsonb not null default '{}',
	region text not null default '',
	status text not null default 'verifying',
	account text not null default '',
	check_log text not null default '',
	created_at timestamptz not null default now(),
	last_checked_at timestamptz,
	unique (org_id, name)
);
`

// Provider is a cloud account goku can deploy into. Credentials are
// write-only through the API, like instance SSH keys.
type Provider struct {
	ID            string            `json:"id"`
	OrgID         string            `json:"org_id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"` // aws | azure | digitalocean
	Credentials   map[string]string `json:"-"`
	Region        string            `json:"region"`
	Status        string            `json:"status"` // verifying | ready | invalid
	Account       string            `json:"account"`
	CheckLog      string            `json:"check_log"`
	CreatedAt     time.Time         `json:"created_at"`
	LastCheckedAt *time.Time        `json:"last_checked_at"`
}

func (s *Store) CreateProvider(ctx context.Context, orgID, name, kind, region string, creds map[string]string, actor string) (*Provider, error) {
	if name == "" {
		return nil, errors.New("provider name is required")
	}
	credsJSON, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	p := &Provider{OrgID: orgID, Name: name, Kind: kind, Region: region, Status: "verifying"}
	err = s.pool.QueryRow(ctx, `
		insert into cloud_providers (org_id, name, kind, region, credentials) values ($1, $2, $3, $4, $5)
		returning id, created_at`, orgID, name, kind, region, credsJSON).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		if errAlready(err) {
			return nil, fmt.Errorf("provider %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, orgID, actor, "provider.add", "provider/"+name, map[string]any{"kind": kind, "region": region})
	return p, nil
}

func (s *Store) ListProviders(ctx context.Context, orgID string) ([]Provider, error) {
	rows, err := s.pool.Query(ctx, `
		select id, org_id, name, kind, region, status, account, check_log, created_at, last_checked_at
		from cloud_providers where org_id = $1 order by created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Kind, &p.Region, &p.Status, &p.Account,
			&p.CheckLog, &p.CreatedAt, &p.LastCheckedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProviderByKind returns the org's provider of a given kind (tailscale, for
// one) that has passed verification, or nil if there isn't one. Credentials
// are included — server-side use only.
func (s *Store) ProviderByKind(ctx context.Context, orgID, kind string) (*Provider, error) {
	p := &Provider{}
	var creds []byte
	err := s.pool.QueryRow(ctx, `
		select id, org_id, name, kind, region, credentials, status, account, check_log, created_at, last_checked_at
		from cloud_providers where org_id = $1 and kind = $2 and status = 'ready'
		order by created_at limit 1`, orgID, kind).
		Scan(&p.ID, &p.OrgID, &p.Name, &p.Kind, &p.Region, &creds, &p.Status, &p.Account,
			&p.CheckLog, &p.CreatedAt, &p.LastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creds, &p.Credentials)
	return p, nil
}

// GetProvider includes credentials — server-side use only.
func (s *Store) GetProvider(ctx context.Context, orgID, id string) (*Provider, error) {
	p := &Provider{}
	var creds []byte
	err := s.pool.QueryRow(ctx, `
		select id, org_id, name, kind, region, credentials, status, account, check_log, created_at, last_checked_at
		from cloud_providers where org_id = $1 and id::text = $2`, orgID, id).
		Scan(&p.ID, &p.OrgID, &p.Name, &p.Kind, &p.Region, &creds, &p.Status, &p.Account,
			&p.CheckLog, &p.CreatedAt, &p.LastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("provider: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creds, &p.Credentials)
	return p, nil
}

func (s *Store) DeleteProvider(ctx context.Context, orgID, id, actor string) error {
	var name string
	err := s.pool.QueryRow(ctx, `delete from cloud_providers where org_id = $1 and id::text = $2 returning name`, orgID, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("provider: %w", ErrNotFound)
	}
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return fmt.Errorf("provider still has instances in your fleet — remove those first")
	}
	if err == nil {
		s.audit(ctx, orgID, actor, "provider.remove", "provider/"+name, nil)
	}
	return err
}

// SetProviderCheck records a credential verification run's outcome.
func (s *Store) SetProviderCheck(ctx context.Context, id, status, account, checkLog string) {
	_, _ = s.pool.Exec(ctx, `
		update cloud_providers set status = $1, account = $2, check_log = $3, last_checked_at = now()
		where id = $4`, status, account, checkLog, id)
}
