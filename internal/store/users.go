package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const usersSchema = `
create table if not exists users (
	id uuid primary key default gen_random_uuid(),
	provider text not null,
	provider_id text not null,
	email text not null,
	name text not null default '',
	avatar_url text not null default '',
	github_token text not null default '',
	created_at timestamptz not null default now(),
	last_login_at timestamptz not null default now(),
	unique (provider, provider_id)
);

create table if not exists memberships (
	user_id uuid not null references users(id),
	org_id uuid not null references organizations(id),
	role text not null default 'member',
	created_at timestamptz not null default now(),
	primary key (user_id, org_id)
);

create table if not exists sessions (
	token_hash bytea primary key,
	user_id uuid not null references users(id),
	expires_at timestamptz not null
);
`

type User struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	AvatarURL   string `json:"avatar_url"`
	GitHubToken string `json:"-"`
}

// UpsertUser records an OAuth login, updating profile fields on every visit.
func (s *Store) UpsertUser(ctx context.Context, provider, providerID, email, name, avatar, githubToken string) (*User, error) {
	u := &User{Provider: provider, Email: email, Name: name, AvatarURL: avatar, GitHubToken: githubToken}
	err := s.pool.QueryRow(ctx, `
		insert into users (provider, provider_id, email, name, avatar_url, github_token)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (provider, provider_id) do update set
			email = excluded.email, name = excluded.name, avatar_url = excluded.avatar_url,
			github_token = case when excluded.github_token <> '' then excluded.github_token else users.github_token end,
			last_login_at = now()
		returning id`,
		provider, providerID, email, name, avatar, githubToken).Scan(&u.ID)
	return u, err
}

// CreateSession mints a browser session for a user; returns the cookie value.
func (s *Store) CreateSession(ctx context.Context, userID string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "gks_" + hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	_, err := s.pool.Exec(ctx, `insert into sessions (token_hash, user_id, expires_at) values ($1, $2, $3)`,
		hash[:], userID, time.Now().Add(30*24*time.Hour))
	return token, err
}

// ResolveSession returns the session's user and their org (first membership;
// orgID is empty when the user hasn't joined an organization yet).
func (s *Store) ResolveSession(ctx context.Context, token string) (*User, string, error) {
	hash := sha256.Sum256([]byte(token))
	u := &User{}
	err := s.pool.QueryRow(ctx, `
		select u.id, u.provider, u.email, u.name, u.avatar_url, u.github_token
		from sessions s join users u on u.id = s.user_id
		where s.token_hash = $1 and s.expires_at > now()`, hash[:]).
		Scan(&u.ID, &u.Provider, &u.Email, &u.Name, &u.AvatarURL, &u.GitHubToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var orgID string
	err = s.pool.QueryRow(ctx, `select org_id from memberships where user_id = $1 order by created_at limit 1`, u.ID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, "", nil
	}
	return u, orgID, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) {
	hash := sha256.Sum256([]byte(token))
	_, _ = s.pool.Exec(ctx, `delete from sessions where token_hash = $1`, hash[:])
}

// JoinOrg adds a user to an organization (idempotent).
func (s *Store) JoinOrg(ctx context.Context, userID, orgID, email string) error {
	_, err := s.pool.Exec(ctx, `
		insert into memberships (user_id, org_id) values ($1, $2)
		on conflict do nothing`, userID, orgID)
	if err != nil {
		return err
	}
	s.audit(ctx, orgID, "user:"+email, "member.join", "org", map[string]any{"user_id": userID})
	return nil
}

func (s *Store) UserOrgs(ctx context.Context, userID string) ([]Org, error) {
	rows, err := s.pool.Query(ctx, `
		select o.id, o.name from memberships m join organizations o on o.id = m.org_id
		where m.user_id = $1 order by m.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orgs := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// GitHubTokenForOrgImport returns a GitHub token usable for importing into
// the org: the acting user's if present, else any member's (so token-authed
// agents can import private repos once a human with GitHub SSO has joined).
func (s *Store) GitHubTokenForOrg(ctx context.Context, orgID string) string {
	var t string
	_ = s.pool.QueryRow(ctx, `
		select u.github_token from memberships m join users u on u.id = m.user_id
		where m.org_id = $1 and u.github_token <> '' order by m.created_at limit 1`, orgID).Scan(&t)
	return t
}
