package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/benjaminsanborn/goku/internal/cloud"
	"github.com/benjaminsanborn/goku/internal/store"
)

// Cloud providers: the accounts goku is allowed to deploy into. Adding one
// stores its credentials write-only and verifies them against the provider's
// API, the same enroll-then-check shape the fleet uses (design doc 10).
//
// AWS is the implemented deploy path: provisioning launches an EC2 instance,
// which joins the fleet as an ordinary ssh instance and is then deployed to by
// the existing engine. Azure and DigitalOcean verify credentials and settle at
// status "pending" — provisioning for them is not built yet.
//
// Tailscale is a different sort of provider: it supplies no machines, it
// supplies the network they are reached over. Connect one and provisioned
// instances join the tailnet at boot and take no public ingress at all.

// providerRole distinguishes accounts that supply machines from the one that
// supplies the network between them.
type providerRole string

const (
	roleCompute providerRole = "compute"
	roleNetwork providerRole = "network"
)

// providerKinds describes each supported provider: which credential fields the
// UI collects, which are required, and whether deployments are implemented.
var providerKinds = map[string]struct {
	role       providerRole
	fields     []string
	required   []string
	deployable bool
}{
	"aws": {
		role:       roleCompute,
		fields:     []string{"access_key_id", "secret_access_key", "session_token"},
		required:   []string{"access_key_id", "secret_access_key"},
		deployable: true,
	},
	"azure": {
		role:     roleCompute,
		fields:   []string{"tenant_id", "client_id", "client_secret", "subscription_id"},
		required: []string{"tenant_id", "client_id", "client_secret"},
	},
	"digitalocean": {
		role:     roleCompute,
		fields:   []string{"api_token"},
		required: []string{"api_token"},
	},
	"tailscale": {
		role:     roleNetwork,
		fields:   []string{"client_id", "client_secret", "tailnet", "tag"},
		required: []string{"client_id", "client_secret"},
	},
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.Store.ListProviders(r.Context(), orgFrom(r.Context()))
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := []map[string]any{}
	for _, p := range providers {
		spec := providerKinds[p.Kind]
		out = append(out, map[string]any{"provider": p, "deployable": spec.deployable, "role": string(spec.role)})
	}
	respond(w, map[string]any{"providers": out}, nil)
}

func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string            `json:"name"`
		Kind        string            `json:"kind"`
		Region      string            `json:"region"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	spec, ok := providerKinds[kind]
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "kind must be one of aws, azure, digitalocean, tailscale")
		return
	}
	creds := map[string]string{}
	for _, f := range spec.fields {
		if v := strings.TrimSpace(in.Credentials[f]); v != "" {
			creds[f] = v
		}
	}
	var missing []string
	for _, f := range spec.required {
		if creds[f] == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		httpError(w, http.StatusUnprocessableEntity, "missing credential fields: "+strings.Join(missing, ", "))
		return
	}
	org := orgFrom(r.Context())
	p, err := s.Store.CreateProvider(r.Context(), org, strings.ToLower(strings.TrimSpace(in.Name)), kind,
		strings.TrimSpace(in.Region), creds, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go s.verifyProvider(org, p.ID)
	respond(w, map[string]any{"provider": p}, nil)
}

func (s *Server) handleVerifyProvider(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProvider(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go s.verifyProvider(org, p.ID)
	respond(w, map[string]any{"verifying": p.Name}, nil)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	err := s.Store.DeleteProvider(r.Context(), orgFrom(r.Context()), r.PathValue("id"), s.actorFrom(r))
	respond(w, map[string]any{"deleted": true}, err)
}

// handleProvisionInstance launches a machine in the provider's account and
// enrolls it in the fleet. The instance is created up front in the
// "provisioning" state so the Fleet tab shows the work in progress.
func (s *Server) handleProvisionInstance(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProvider(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if !providerKinds[p.Kind].deployable {
		httpError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("%s deployments are not supported yet — this provider's credentials are stored and verified, but goku can only provision instances on AWS today", p.Kind))
		return
	}
	if p.Status != "ready" {
		httpError(w, http.StatusUnprocessableEntity, "provider credentials are not verified (status "+p.Status+")")
		return
	}
	var in struct {
		Name string `json:"name"`
		Size string `json:"size"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if name == "" {
		httpError(w, http.StatusUnprocessableEntity, "instance name is required")
		return
	}

	inst, err := s.Store.CreateInstance(r.Context(), org, store.NewInstance{
		Name: name, Driver: "ssh", ProviderID: p.ID,
	}, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.Store.SetInstanceCheck(context.Background(), inst.ID, "provisioning",
		fmt.Sprintf("… provisioning on %s (%s)\n", p.Name, p.Kind), map[string]any{})

	go s.provisionInstance(org, p.ID, inst.ID, strings.TrimSpace(in.Size))
	respond(w, map[string]any{"instance": inst}, nil)
}

// provisionInstance runs the launch, records the resulting address and key,
// then hands off to the ordinary fleet verification.
func (s *Server) provisionInstance(org, providerID, instanceID, size string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	p, err := s.Store.GetProvider(ctx, org, providerID)
	if err != nil {
		return
	}
	inst, err := s.Store.GetInstance(ctx, org, instanceID)
	if err != nil {
		return
	}

	var log strings.Builder
	logf := func(format string, args ...any) {
		fmt.Fprintf(&log, "… "+format+"\n", args...)
		s.Store.SetInstanceCheck(ctx, instanceID, "provisioning", log.String(), map[string]any{})
	}

	fail := func(err error) {
		fmt.Fprintf(&log, "✗ provisioning failed: %v\n", err)
		s.Store.SetInstanceCheck(ctx, instanceID, "failed", log.String(), map[string]any{})
	}

	opts := cloud.Options{Name: inst.Name, Type: size}
	// A connected tailnet is what decides the instance's ingress model: on the
	// mesh it takes no public inbound, otherwise the security group is pinned
	// to this control plane's egress address.
	netProvider, err := s.Store.ProviderByKind(ctx, org, "tailscale")
	if err != nil {
		fail(err)
		return
	}
	var ts cloud.Tailscale
	if netProvider != nil {
		ts = cloud.TailscaleFrom(netProvider.Credentials)
		logf("minting a tailnet auth key from %s", netProvider.Name)
		key, err := ts.MintAuthKey(ctx)
		if err != nil {
			fail(err)
			return
		}
		opts.TailscaleAuthKey = key
	} else {
		logf("no tailscale provider connected — locking ingress to this control plane")
		cidr, err := cloud.EgressIP(ctx)
		if err != nil {
			fail(fmt.Errorf("%w — connect a Tailscale provider to avoid needing a public address", err))
			return
		}
		opts.AllowCIDR = cidr + "/32"
	}

	aws := cloud.AWSFrom(p.Credentials, p.Region)
	machine, err := aws.Provision(ctx, opts, logf)
	if err != nil {
		fail(err)
		return
	}
	fmt.Fprintf(&log, "✓ launched %s (%s) at %s\n", machine.InstanceID, machine.Type, machine.PublicIP)

	// On a tailnet the public address is incidental — the control plane dials
	// the machine's tailnet address for both ssh and routed app traffic.
	host := machine.PublicIP
	if opts.TailscaleAuthKey != "" {
		logf("waiting for %s to join the tailnet", opts.Hostname())
		ip, err := ts.WaitForDevice(ctx, opts.Hostname(), logf)
		if err != nil {
			fail(err)
			return
		}
		fmt.Fprintf(&log, "✓ joined tailnet as %s (%s)\n", opts.Hostname(), ip)
		host = ip
	}
	address := machine.User + "@" + host
	if err := s.Store.AttachProvisioned(ctx, instanceID, address, machine.PrivateKey, machine.InstanceID, machine.KeyName); err != nil {
		fail(fmt.Errorf("could not record instance details: %w", err))
		return
	}

	// cloud-init is still installing docker; verification is what proves the
	// machine is actually deployable, so retry it until it passes.
	fmt.Fprintf(&log, "… waiting for docker (cloud-init)\n")
	s.Store.SetInstanceCheck(ctx, instanceID, "provisioning", log.String(), map[string]any{})
	for attempt := 0; attempt < 20; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
		s.verifyInstance(org, instanceID)
		if cur, err := s.Store.GetInstance(ctx, org, instanceID); err == nil && cur.Status == "ready" {
			return
		}
	}
}

// verifyProvider calls the provider's own identity endpoint — the cheapest
// read that proves the credentials are live. Kinds goku cannot deploy to yet
// settle at "pending" rather than "ready".
func (s *Server) verifyProvider(org, id string) {
	ctx := context.Background()
	p, err := s.Store.GetProvider(ctx, org, id)
	if err != nil {
		return
	}
	spec, ok := providerKinds[p.Kind]
	if !ok {
		s.Store.SetProviderCheck(ctx, id, "invalid", "", "✗ unknown provider kind "+p.Kind)
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var account string
	switch p.Kind {
	case "aws":
		account, err = cloud.AWSFrom(p.Credentials, p.Region).Identity(runCtx)
	case "tailscale":
		account, err = cloud.TailscaleFrom(p.Credentials).Identity(runCtx)
	case "azure":
		account, err = cloud.VerifyAzure(runCtx, p.Credentials)
	case "digitalocean":
		account, err = cloud.VerifyDigitalOcean(runCtx, p.Credentials)
	}
	if err != nil {
		s.Store.SetProviderCheck(ctx, id, "invalid", "", fmt.Sprintf("✗ credentials rejected: %v\n", err))
		return
	}

	log := fmt.Sprintf("✓ credentials valid\n✓ identity: %s\n", account)
	status := "ready"
	if spec.role == roleNetwork {
		log += "… provisioned instances will join this tailnet and take no public ingress.\n"
	} else if !spec.deployable {
		status = "pending"
		log += fmt.Sprintf("… %s deployments are not implemented yet — this account is stored and verified,\n  but instances can only be provisioned on AWS today.\n", p.Kind)
	}
	s.Store.SetProviderCheck(ctx, id, status, account, log)
}
