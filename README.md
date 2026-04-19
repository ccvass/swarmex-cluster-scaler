<p align="center"><img src="https://raw.githubusercontent.com/ccvass/swarmex/main/docs/assets/logo.svg" alt="Swarmex" width="400"></p>

[![Test, Build & Deploy](https://github.com/ccvass/swarmex-cluster-scaler/actions/workflows/publish.yml/badge.svg)](https://github.com/ccvass/swarmex-cluster-scaler/actions/workflows/publish.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

# Swarmex Cluster Scaler

Auto-provision and deprovision cloud nodes based on cluster CPU usage.

Part of [Swarmex](https://github.com/ccvass/swarmex) — enterprise-grade orchestration for Docker Swarm.

## What It Does

Monitors overall cluster CPU utilization and automatically provisions or deprovisions cloud VMs to match demand. Supports AWS, GCP, Azure, and DigitalOcean. Tracks managed nodes in bbolt for reliable state across restarts.

## Labels

No service labels. Configuration via YAML:

```yaml
swarm_token: "SWMTKN-..."
manager_ip: "10.0.0.1"
min_nodes: 3
max_nodes: 10
scale_up_cpu: 80       # % CPU to trigger scale-up
scale_down_cpu: 30     # % CPU to trigger scale-down
providers:
  aws:
    region: us-east-1
    instance_type: t3.medium
    ami: ami-0abcdef1234567890
```

## How It Works

1. Queries Prometheus for cluster-wide CPU utilization.
2. If CPU exceeds `scale_up_cpu`, provisions a new node via the configured cloud provider.
3. Joins the new node to the Swarm using the configured token and manager IP.
4. If CPU drops below `scale_down_cpu`, drains and terminates the newest managed node.
5. Persists managed node state in bbolt to track provisioned instances.

## Quick Start

```bash
docker service create \
  --name swarmex-cluster-scaler \
  --mount type=bind,src=/etc/swarmex/cluster-scaler.yaml,dst=/config.yaml \
  --mount type=volume,src=cluster-scaler-data,dst=/data \
  ghcr.io/ccvass/swarmex-cluster-scaler:latest
```

## Verified

AWS full cycle: 3→5 nodes on high CPU, then 5→3 on low CPU. Nodes joined and drained correctly.

## License

Apache-2.0
