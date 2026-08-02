package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const databasesSchema = `
create table if not exists databases (
	id uuid primary key default gen_random_uuid(),
	org_id uuid not null references organizations(id),
	provider_id uuid not null references cloud_providers(id),
	name text not null,
	engine_version text not null default '18',
	instance_type text not null,
	storage_gb int not null default 50,
	status text not null default 'creating',
	endpoint text not null default '',
	external_id text not null default '',
	key_name text not null default '',
	ssh_key text not null default '',
	superuser text not null default 'goku',
	password text not null default '',
	config_overrides jsonb not null default '{}',
	event_log text not null default '',
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now(),
	unique (org_id, name)
);
`

// Database is a managed Postgres server on its own EC2 instance. Like
// instances and providers, its key material is write-only through the API.
type Database struct {
	ID         string `json:"id"`
	OrgID      string `json:"org_id"`
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Version    string `json:"engine_version"`
	Type       string `json:"instance_type"`
	StorageGB  int    `json:"storage_gb"`
	// Status is the lifecycle: creating, available, modifying, rebooting,
	// failed, deleting.
	Status     string            `json:"status"`
	Endpoint   string            `json:"endpoint"`
	ExternalID string            `json:"external_id"`
	KeyName    string            `json:"key_name"`
	SSHKey     string            `json:"-"`
	Superuser  string            `json:"superuser"`
	Password   string            `json:"-"`
	Overrides  map[string]string `json:"config_overrides"`
	EventLog   string            `json:"event_log"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type NewDatabase struct {
	Name       string
	ProviderID string
	Type       string
	StorageGB  int
	Superuser  string
	Password   string
	Overrides  map[string]string
}

func (s *Store) CreateDatabase(ctx context.Context, orgID string, in NewDatabase, actor string) (*Database, error) {
	if in.Name == "" {
		return nil, errors.New("database name is required")
	}
	overrides, err := json.Marshal(in.Overrides)
	if err != nil {
		return nil, err
	}
	d := &Database{
		OrgID: orgID, ProviderID: in.ProviderID, Name: in.Name, Version: "18",
		Type: in.Type, StorageGB: in.StorageGB, Status: "creating",
		Superuser: in.Superuser, Overrides: in.Overrides,
	}
	err = s.pool.QueryRow(ctx, `
		insert into databases (org_id, provider_id, name, instance_type, storage_gb, superuser, password, config_overrides)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning id, created_at, updated_at`,
		orgID, in.ProviderID, in.Name, in.Type, in.StorageGB, in.Superuser, in.Password, overrides).
		Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errAlready(err) {
			return nil, fmt.Errorf("database %q already exists", in.Name)
		}
		return nil, err
	}
	s.audit(ctx, orgID, actor, "database.create", "database/"+in.Name,
		map[string]any{"instance_type": in.Type, "storage_gb": in.StorageGB})
	return d, nil
}

const dbColumns = `id, org_id, provider_id, name, engine_version, instance_type, storage_gb,
	status, endpoint, external_id, key_name, superuser, config_overrides, event_log, created_at, updated_at`

func scanDatabase(row pgx.Row) (*Database, error) {
	d := &Database{}
	var overrides []byte
	err := row.Scan(&d.ID, &d.OrgID, &d.ProviderID, &d.Name, &d.Version, &d.Type, &d.StorageGB,
		&d.Status, &d.Endpoint, &d.ExternalID, &d.KeyName, &d.Superuser, &overrides, &d.EventLog,
		&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(overrides, &d.Overrides)
	return d, nil
}

func (s *Store) ListDatabases(ctx context.Context, orgID string) ([]Database, error) {
	rows, err := s.pool.Query(ctx, `select `+dbColumns+` from databases where org_id = $1 order by created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Database{}
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// GetDatabase omits the credentials; use GetDatabaseSecrets server-side.
func (s *Store) GetDatabase(ctx context.Context, orgID, id string) (*Database, error) {
	d, err := scanDatabase(s.pool.QueryRow(ctx,
		`select `+dbColumns+` from databases where org_id = $1 and id::text = $2`, orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("database: %w", ErrNotFound)
	}
	return d, err
}

// GetDatabaseSecrets includes the ssh key and superuser password — the deploy
// path and the credentials reveal, nothing else.
func (s *Store) GetDatabaseSecrets(ctx context.Context, orgID, id string) (*Database, error) {
	d, err := s.GetDatabase(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `select ssh_key, password from databases where id = $1`, d.ID).
		Scan(&d.SSHKey, &d.Password)
	return d, err
}

// SetDatabaseStatus records lifecycle changes and the event feed shown on the
// database page.
func (s *Store) SetDatabaseStatus(ctx context.Context, id, status, eventLog string) {
	_, _ = s.pool.Exec(ctx, `
		update databases set status = $1, event_log = $2, updated_at = now() where id = $3`,
		status, eventLog, id)
}

// AttachDatabaseMachine records the EC2 instance behind a database once it
// has been launched.
func (s *Store) AttachDatabaseMachine(ctx context.Context, id, endpoint, sshKey, externalID, keyName string) error {
	_, err := s.pool.Exec(ctx, `
		update databases set endpoint = $1, ssh_key = $2, external_id = $3, key_name = $4, updated_at = now()
		where id = $5`, endpoint, sshKey, externalID, keyName, id)
	return err
}

func (s *Store) SetDatabaseOverrides(ctx context.Context, orgID, id string, overrides map[string]string, actor string) error {
	raw, err := json.Marshal(overrides)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`update databases set config_overrides = $1, updated_at = now() where id = $2`, raw, id); err != nil {
		return err
	}
	s.audit(ctx, orgID, actor, "database.configure", "database/"+id, map[string]any{"parameters": len(overrides)})
	return nil
}

func (s *Store) DeleteDatabase(ctx context.Context, orgID, id, actor string) (*Database, error) {
	d, err := s.GetDatabaseSecrets(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `delete from databases where org_id = $1 and id::text = $2`, orgID, id); err != nil {
		return nil, err
	}
	s.audit(ctx, orgID, actor, "database.delete", "database/"+d.Name, map[string]any{"instance": d.ExternalID})
	return d, nil
}
