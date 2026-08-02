package cloud

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Machine is a freshly provisioned EC2 instance, described in the terms the
// fleet cares about: how to reach it and the key that opens it.
type Machine struct {
	InstanceID string
	PublicIP   string
	User       string // login the AMI ships with
	PrivateKey string // PEM, generated for this machine alone
	KeyName    string
	Type       string
	AMI        string
}

// Logf receives provisioning progress.
type Logf func(format string, args ...any)

// Two security groups, by ingress model: instances on a tailnet need no
// public inbound at all, while unmeshed instances get SSH and the app port
// range opened to the control plane's egress address only.
const (
	privateGroupName = "goku-%s-private"
	directGroupName  = "goku-%s-direct"
)

// appPortLow/appPortHigh mirror deploy.Port's range — the ports the central
// proxy dials on an instance. Database ports stay bound to the instance's
// own loopback and are never exposed.
const (
	appPortLow  = 30000
	appPortHigh = 39999
)

// PortRange is one TCP ingress rule on an unmeshed instance.
type PortRange struct {
	From int
	To   int
	Note string
}

// Options configure one launch.
type Options struct {
	Name string
	// Type is the EC2 instance type; empty means t3.small.
	Type string
	// Purpose separates security groups and tags by what the machine is for
	// ("fleet", "db"): a database should never inherit the app port range.
	Purpose string
	// DataVolumeGB attaches a second EBS volume, left unformatted here — the
	// caller's setup script owns the filesystem. Stateful workloads keep
	// their data off the root disk so the instance can be replaced.
	DataVolumeGB int
	// Ingress overrides the ports opened to AllowCIDR when there is no
	// tailnet. Empty means ssh plus the app port range.
	Ingress []PortRange
	// Setup is appended to cloud-init after docker and tailscale are up.
	Setup string
	// TailscaleAuthKey, when set, joins the instance to the tailnet at boot
	// and leaves its security group with no ingress rules at all.
	TailscaleAuthKey string
	// AllowCIDR is the only source allowed to reach an unmeshed instance.
	// Required when TailscaleAuthKey is empty.
	AllowCIDR string
}

// cloudInit installs docker (and optionally tailscale) and leaves both usable
// by the default login, which is what fleet verification checks for.
func cloudInit(o Options) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -eux\n")
	b.WriteString("curl -fsSL https://get.docker.com | sh\n")
	b.WriteString("usermod -aG docker ubuntu\n")
	b.WriteString("systemctl enable --now docker\n")
	if o.TailscaleAuthKey != "" {
		b.WriteString("curl -fsSL https://tailscale.com/install.sh | sh\n")
		fmt.Fprintf(&b, "tailscale up --authkey=%s --hostname=%s --accept-dns=false\n",
			shellQuote(o.TailscaleAuthKey), shellQuote(o.Hostname()))
	}
	if o.Setup != "" {
		b.WriteString(o.Setup)
		if !strings.HasSuffix(o.Setup, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Hostname is the name the instance reports to the tailnet. Databases are
// namespaced so a database and a fleet instance can share a name.
func (o Options) Hostname() string {
	if o.Purpose == "db" {
		return "goku-db-" + o.Name
	}
	return "goku-" + o.Name
}

// ingress is the rule set for an unmeshed instance of this purpose.
func (o Options) ingress() []PortRange {
	if len(o.Ingress) > 0 {
		return o.Ingress
	}
	return []PortRange{
		{22, 22, "goku control plane ssh"},
		{appPortLow, appPortHigh, "goku control plane app routing"},
	}
}

// shellQuote makes a value safe to interpolate into the boot script.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Provision launches an instance and waits for it to reach running with a
// public address. The caller still runs fleet verification against it — this
// only gets the machine to the point where SSH can succeed.
func (a AWS) Provision(ctx context.Context, o Options, logf Logf) (*Machine, error) {
	instanceType := o.Type
	if instanceType == "" {
		instanceType = "t3.small"
	}
	if o.TailscaleAuthKey == "" && o.AllowCIDR == "" {
		return nil, fmt.Errorf("ec2: refusing to launch with neither a tailnet nor an allowed source address")
	}
	logf("resolving latest Ubuntu 24.04 AMI in %s", a.Region)
	ami, err := a.ubuntuAMI(ctx)
	if err != nil {
		return nil, err
	}
	logf("ami %s", ami)

	vpc, err := a.defaultVPC(ctx)
	if err != nil {
		return nil, err
	}
	group, err := a.ensureSecurityGroup(ctx, vpc, o, logf)
	if err != nil {
		return nil, err
	}

	keyName := "goku-" + o.Name
	logf("creating key pair %s", keyName)
	// A stale key pair from a previous attempt would make RunInstances succeed
	// with a private key we no longer hold.
	_ = a.query(ctx, "ec2", url.Values{
		"Action": {"DeleteKeyPair"}, "Version": {ec2Version}, "KeyName": {keyName},
	}, nil)
	var keyOut struct {
		Material string `xml:"keyMaterial"`
	}
	if err := a.query(ctx, "ec2", url.Values{
		"Action": {"CreateKeyPair"}, "Version": {ec2Version}, "KeyName": {keyName}, "KeyType": {"ed25519"},
	}, &keyOut); err != nil {
		return nil, err
	}
	if keyOut.Material == "" {
		return nil, fmt.Errorf("ec2: CreateKeyPair returned no key material")
	}

	logf("launching %s instance", instanceType)
	var runOut struct {
		InstanceID string `xml:"instancesSet>item>instanceId"`
	}
	form := url.Values{
		"Action":                              {"RunInstances"},
		"Version":                             {ec2Version},
		"ImageId":                             {ami},
		"InstanceType":                        {instanceType},
		"MinCount":                            {"1"},
		"MaxCount":                            {"1"},
		"KeyName":                             {keyName},
		"SecurityGroupId.1":                   {group},
		"UserData":                            {b64(cloudInit(o))},
		"TagSpecification.1.ResourceType":     {"instance"},
		"TagSpecification.1.Tag.1.Key":        {"Name"},
		"TagSpecification.1.Tag.1.Value":      {o.Hostname()},
		"TagSpecification.1.Tag.2.Key":        {"managed-by"},
		"TagSpecification.1.Tag.2.Value":      {"goku"},
		"MetadataOptions.HttpTokens":          {"required"},
		"BlockDeviceMapping.1.DeviceName":     {"/dev/sda1"},
		"BlockDeviceMapping.1.Ebs.VolumeSize": {"30"},
		"BlockDeviceMapping.1.Ebs.VolumeType": {"gp3"},
	}
	if o.DataVolumeGB > 0 {
		// A separate volume for state: the instance can be replaced or
		// resized without touching the data on it.
		form.Set("BlockDeviceMapping.2.DeviceName", "/dev/sdf")
		form.Set("BlockDeviceMapping.2.Ebs.VolumeSize", fmt.Sprint(o.DataVolumeGB))
		form.Set("BlockDeviceMapping.2.Ebs.VolumeType", "gp3")
		form.Set("BlockDeviceMapping.2.Ebs.DeleteOnTermination", "true")
	}
	err = a.query(ctx, "ec2", form, &runOut)
	if err != nil {
		_ = a.query(ctx, "ec2", url.Values{"Action": {"DeleteKeyPair"}, "Version": {ec2Version}, "KeyName": {keyName}}, nil)
		return nil, err
	}
	logf("instance %s launched, waiting for a public address", runOut.InstanceID)

	ip, err := a.waitForRunning(ctx, runOut.InstanceID, logf)
	if err != nil {
		return nil, err
	}
	logf("instance running at %s", ip)

	return &Machine{
		InstanceID: runOut.InstanceID,
		PublicIP:   ip,
		User:       "ubuntu",
		PrivateKey: keyOut.Material,
		KeyName:    keyName,
		Type:       instanceType,
		AMI:        ami,
	}, nil
}

const ec2Version = "2016-11-15"

// ubuntuAMI reads Canonical's public SSM parameter for the current 24.04
// image, which is always right for the region without hardcoding IDs.
func (a AWS) ubuntuAMI(ctx context.Context) (string, error) {
	var out struct {
		Parameter struct {
			Value string `json:"Value"`
		} `json:"Parameter"`
	}
	in := map[string]string{"Name": "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"}
	if err := a.jsonAPI(ctx, "ssm", "AmazonSSM.GetParameter", in, &out); err != nil {
		return "", err
	}
	if out.Parameter.Value == "" {
		return "", fmt.Errorf("ssm: no AMI id returned")
	}
	return out.Parameter.Value, nil
}

func (a AWS) defaultVPC(ctx context.Context) (string, error) {
	var out struct {
		VpcID string `xml:"vpcSet>item>vpcId"`
	}
	err := a.query(ctx, "ec2", url.Values{
		"Action": {"DescribeVpcs"}, "Version": {ec2Version},
		"Filter.1.Name": {"isDefault"}, "Filter.1.Value.1": {"true"},
	}, &out)
	if err != nil {
		return "", err
	}
	if out.VpcID == "" {
		return "", fmt.Errorf("ec2: no default VPC in %s — create one, or attach an existing instance from the Fleet tab", a.Region)
	}
	return out.VpcID, nil
}

// ensureSecurityGroup finds or creates the group for this ingress model.
//
// Tailnet instances get a group with no ingress rules whatsoever — the only
// way in is the mesh. Unmeshed instances get SSH and the app port range
// opened to the control plane's egress address, and nothing else; those rules
// are re-authorized on every launch so a control plane whose public address
// has changed re-adds itself instead of locking the fleet out.
func (a AWS) ensureSecurityGroup(ctx context.Context, vpc string, o Options, logf Logf) (string, error) {
	purpose := o.Purpose
	if purpose == "" {
		purpose = "fleet"
	}
	name := fmt.Sprintf(privateGroupName, purpose)
	description := "goku " + purpose + ": tailnet only, no public ingress"
	if o.TailscaleAuthKey == "" {
		name = fmt.Sprintf(directGroupName, purpose)
		description = "goku " + purpose + ": control plane ingress only"
	}

	var found struct {
		GroupID string `xml:"securityGroupInfo>item>groupId"`
	}
	err := a.query(ctx, "ec2", url.Values{
		"Action": {"DescribeSecurityGroups"}, "Version": {ec2Version},
		"Filter.1.Name": {"group-name"}, "Filter.1.Value.1": {name},
		"Filter.2.Name": {"vpc-id"}, "Filter.2.Value.1": {vpc},
	}, &found)
	if err != nil {
		return "", err
	}
	group := found.GroupID
	if group == "" {
		logf("creating security group %s in %s", name, vpc)
		var created struct {
			GroupID string `xml:"groupId"`
		}
		if err := a.query(ctx, "ec2", url.Values{
			"Action": {"CreateSecurityGroup"}, "Version": {ec2Version},
			"GroupName": {name}, "VpcId": {vpc}, "GroupDescription": {description},
		}, &created); err != nil {
			return "", err
		}
		group = created.GroupID
	}
	if o.TailscaleAuthKey != "" {
		logf("security group %s: no ingress (tailnet only)", name)
		return group, nil
	}

	logf("security group %s: allowing %s from %s", name, portSummary(o.ingress()), o.AllowCIDR)
	for i, rule := range o.ingress() {
		err := a.query(ctx, "ec2", url.Values{
			"Action": {"AuthorizeSecurityGroupIngress"}, "Version": {ec2Version},
			"GroupId":                                {group},
			"IpPermissions.1.IpProtocol":             {"tcp"},
			"IpPermissions.1.FromPort":               {fmt.Sprint(rule.From)},
			"IpPermissions.1.ToPort":                 {fmt.Sprint(rule.To)},
			"IpPermissions.1.IpRanges.1.CidrIp":      {o.AllowCIDR},
			"IpPermissions.1.IpRanges.1.Description": {rule.Note},
		}, nil)
		if err != nil && !strings.Contains(err.Error(), "Duplicate") {
			return "", fmt.Errorf("authorize rule %d: %w", i+1, err)
		}
	}
	return group, nil
}

func portSummary(rules []PortRange) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.From == r.To {
			parts = append(parts, fmt.Sprint(r.From))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", r.From, r.To))
	}
	return strings.Join(parts, ", ")
}

func (a AWS) waitForRunning(ctx context.Context, instanceID string, logf Logf) (string, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(6 * time.Second):
		}
		var out struct {
			State string `xml:"reservationSet>item>instancesSet>item>instanceState>name"`
			IP    string `xml:"reservationSet>item>instancesSet>item>ipAddress"`
		}
		if err := a.query(ctx, "ec2", url.Values{
			"Action": {"DescribeInstances"}, "Version": {ec2Version}, "InstanceId.1": {instanceID},
		}, &out); err != nil {
			return "", err
		}
		switch out.State {
		case "running":
			if out.IP != "" {
				return out.IP, nil
			}
		case "terminated", "shutting-down":
			return "", fmt.Errorf("ec2: instance %s went %s during launch", instanceID, out.State)
		}
	}
	return "", fmt.Errorf("ec2: instance %s did not reach running within 4m", instanceID)
}

// Terminate destroys an instance and its dedicated key pair.
func (a AWS) Terminate(ctx context.Context, instanceID, keyName string) error {
	err := a.query(ctx, "ec2", url.Values{
		"Action": {"TerminateInstances"}, "Version": {ec2Version}, "InstanceId.1": {instanceID},
	}, nil)
	if keyName != "" {
		_ = a.query(ctx, "ec2", url.Values{"Action": {"DeleteKeyPair"}, "Version": {ec2Version}, "KeyName": {keyName}}, nil)
	}
	return err
}
