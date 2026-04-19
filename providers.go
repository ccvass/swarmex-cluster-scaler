package clusterscaler

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// AWS Provider — uses aws CLI
type AWSProvider struct{ Config *AWSConfig }

func (p *AWSProvider) Name() string { return "aws" }

func (p *AWSProvider) Provision(ctx context.Context, tmpl NodeTemplate, joinCmd string) (string, error) {
	userdata := fmt.Sprintf("#!/bin/bash\ncurl -fsSL https://get.docker.com | sh\n%s\n", joinCmd)
	out, err := exec.CommandContext(ctx, "aws", "ec2", "run-instances",
		"--region", p.Config.Region,
		"--instance-type", tmpl.InstanceType,
		"--image-id", tmpl.Image,
		"--key-name", p.Config.KeyName,
		"--security-group-ids", p.Config.SecurityGroup,
		"--subnet-id", p.Config.SubnetID,
		"--block-device-mappings", fmt.Sprintf("DeviceName=/dev/sda1,Ebs={VolumeSize=%d}", tmpl.DiskGB),
		"--user-data", userdata,
		"--tag-specifications", "ResourceType=instance,Tags=[{Key=Name,Value=swarmex-auto},{Key=swarmex.managed,Value=true}]",
		"--query", "Instances[0].InstanceId", "--output", "text",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *AWSProvider) Terminate(ctx context.Context, instanceID string) error {
	_, err := exec.CommandContext(ctx, "aws", "ec2", "terminate-instances",
		"--region", p.Config.Region, "--instance-ids", instanceID).CombinedOutput()
	return err
}

// GCP Provider — uses gcloud CLI
type GCPProvider struct{ Config *GCPConfig }

func (p *GCPProvider) Name() string { return "gcp" }

func (p *GCPProvider) Provision(ctx context.Context, tmpl NodeTemplate, joinCmd string) (string, error) {
	name := fmt.Sprintf("swarmex-auto-%d", time.Now().Unix())
	startup := fmt.Sprintf("#!/bin/bash\ncurl -fsSL https://get.docker.com | sh\n%s\n", joinCmd)
	out, err := exec.CommandContext(ctx, "gcloud", "compute", "instances", "create", name,
		"--project", p.Config.Project,
		"--zone", p.Config.Zone,
		"--machine-type", tmpl.InstanceType,
		"--image-family", tmpl.Image,
		"--image-project", "ubuntu-os-cloud",
		"--boot-disk-size", fmt.Sprintf("%dGB", tmpl.DiskGB),
		"--metadata", fmt.Sprintf("startup-script=%s", startup),
		"--tags", "swarmex",
		"--format", "value(name)",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *GCPProvider) Terminate(ctx context.Context, instanceID string) error {
	_, err := exec.CommandContext(ctx, "gcloud", "compute", "instances", "delete", instanceID,
		"--project", p.Config.Project, "--zone", p.Config.Zone, "--quiet").CombinedOutput()
	return err
}

// Azure Provider — uses az CLI
type AzureProvider struct{ Config *AzureConfig }

func (p *AzureProvider) Name() string { return "azure" }

func (p *AzureProvider) Provision(ctx context.Context, tmpl NodeTemplate, joinCmd string) (string, error) {
	name := fmt.Sprintf("swarmex-auto-%d", time.Now().Unix())
	customData := fmt.Sprintf("#!/bin/bash\ncurl -fsSL https://get.docker.com | sh\n%s\n", joinCmd)
	out, err := exec.CommandContext(ctx, "az", "vm", "create",
		"--resource-group", p.Config.ResourceGroup,
		"--name", name,
		"--location", p.Config.Location,
		"--image", tmpl.Image,
		"--size", tmpl.InstanceType,
		"--os-disk-size-gb", fmt.Sprintf("%d", tmpl.DiskGB),
		"--vnet-name", p.Config.VNet,
		"--subnet", p.Config.Subnet,
		"--custom-data", customData,
		"--tags", "swarmex.managed=true",
		"--query", "id", "--output", "tsv",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return name, nil
}

func (p *AzureProvider) Terminate(ctx context.Context, instanceID string) error {
	_, err := exec.CommandContext(ctx, "az", "vm", "delete",
		"--resource-group", p.Config.ResourceGroup,
		"--name", instanceID, "--yes").CombinedOutput()
	return err
}

// DigitalOcean Provider — uses doctl CLI
type DOProvider struct{ Config *DigitalOceanConfig }

func (p *DOProvider) Name() string { return "digitalocean" }

func (p *DOProvider) Provision(ctx context.Context, tmpl NodeTemplate, joinCmd string) (string, error) {
	name := fmt.Sprintf("swarmex-auto-%d", time.Now().Unix())
	userData := fmt.Sprintf("#!/bin/bash\ncurl -fsSL https://get.docker.com | sh\n%s\n", joinCmd)
	out, err := exec.CommandContext(ctx, "doctl", "compute", "droplet", "create", name,
		"--region", p.Config.Region,
		"--size", tmpl.InstanceType,
		"--image", tmpl.Image,
		"--ssh-keys", p.Config.SSHKeyID,
		"--user-data", userData,
		"--tag-name", "swarmex-managed",
		"--format", "ID", "--no-header",
		"--wait",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *DOProvider) Terminate(ctx context.Context, instanceID string) error {
	_, err := exec.CommandContext(ctx, "doctl", "compute", "droplet", "delete",
		instanceID, "--force").CombinedOutput()
	return err
}
