package clusterscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	bolt "go.etcd.io/bbolt"
	"gopkg.in/yaml.v3"
)

// Provider is the interface each cloud must implement.
type Provider interface {
	Name() string
	Provision(ctx context.Context, tmpl NodeTemplate, joinCmd string) (instanceID string, err error)
	Terminate(ctx context.Context, instanceID string) error
}

// ClusterConfig is loaded from YAML.
type ClusterConfig struct {
	SwarmToken   string         `yaml:"swarm_token"`
	ManagerIP    string         `yaml:"manager_ip"`
	MinNodes     int            `yaml:"min_nodes"`
	MaxNodes     int            `yaml:"max_nodes"`
	ScaleUpCPU   float64        `yaml:"scale_up_cpu"`
	ScaleDownCPU float64        `yaml:"scale_down_cpu"`
	EvalInterval string         `yaml:"eval_interval"`
	CooldownUp   string         `yaml:"cooldown_up"`
	CooldownDown string         `yaml:"cooldown_down"`
	Prometheus   string         `yaml:"prometheus_url"`
	Providers    ProvidersBlock `yaml:"providers"`
}

type ProvidersBlock struct {
	AWS          *AWSConfig          `yaml:"aws,omitempty"`
	GCP          *GCPConfig          `yaml:"gcp,omitempty"`
	Azure        *AzureConfig        `yaml:"azure,omitempty"`
	DigitalOcean *DigitalOceanConfig `yaml:"digitalocean,omitempty"`
}

type NodeTemplate struct {
	InstanceType string `yaml:"instance_type"`
	Image        string `yaml:"image"`
	DiskGB       int    `yaml:"disk_gb"`
}

type AWSConfig struct {
	Region        string       `yaml:"region"`
	KeyName       string       `yaml:"key_name"`
	SecurityGroup string       `yaml:"security_group"`
	SubnetID      string       `yaml:"subnet_id"`
	Template      NodeTemplate `yaml:"template"`
}

type GCPConfig struct {
	Project  string       `yaml:"project"`
	Zone     string       `yaml:"zone"`
	Template NodeTemplate `yaml:"template"`
}

type AzureConfig struct {
	ResourceGroup string       `yaml:"resource_group"`
	Location      string       `yaml:"location"`
	VNet          string       `yaml:"vnet"`
	Subnet        string       `yaml:"subnet"`
	Template      NodeTemplate `yaml:"template"`
}

type DigitalOceanConfig struct {
	Region   string       `yaml:"region"`
	SSHKeyID string       `yaml:"ssh_key_id"`
	Template NodeTemplate `yaml:"template"`
}

type managedNode struct {
	InstanceID string
	Provider   string
	CreatedAt  time.Time
}

type Controller struct {
	docker      *client.Client
	config      ClusterConfig
	providers   []Provider
	logger      *slog.Logger
	db          *bolt.DB
	mu          sync.Mutex
	lastScaleUp time.Time
	lastScaleDn time.Time
	httpClient  *http.Client
	providerIdx int
}

var bucketName = []byte("managed_nodes")

func LoadConfig(path string) (ClusterConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ClusterConfig{}, err
	}
	var cfg ClusterConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ClusterConfig{}, err
	}
	if cfg.MinNodes == 0 { cfg.MinNodes = 2 }
	if cfg.MaxNodes == 0 { cfg.MaxNodes = 10 }
	if cfg.ScaleUpCPU == 0 { cfg.ScaleUpCPU = 80 }
	if cfg.ScaleDownCPU == 0 { cfg.ScaleDownCPU = 20 }
	if cfg.Prometheus == "" { cfg.Prometheus = "http://observability_prometheus:9090" }
	if cfg.EvalInterval == "" { cfg.EvalInterval = "30s" }
	if cfg.CooldownUp == "" { cfg.CooldownUp = "5m" }
	if cfg.CooldownDown == "" { cfg.CooldownDown = "10m" }
	return cfg, nil
}

func New(cli *client.Client, cfg ClusterConfig, providers []Provider, dbPath string, logger *slog.Logger) (*Controller, error) {
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.Update(func(tx *bolt.Tx) error { tx.CreateBucketIfNotExists(bucketName); return nil })
	c := &Controller{
		docker: cli, config: cfg, providers: providers, db: db, logger: logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	if n := c.managedCount(); n > 0 {
		logger.Info("restored managed nodes", "count", n)
	}
	return c, nil
}

func (c *Controller) Close() { c.db.Close() }

func (c *Controller) managedCount() int {
	n := 0
	c.db.View(func(tx *bolt.Tx) error { b := tx.Bucket(bucketName); if b != nil { n = b.Stats().KeyN }; return nil })
	return n
}

func (c *Controller) saveNode(id string, node *managedNode) {
	data, _ := json.Marshal(node)
	c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketName).Put([]byte(id), data) })
}

func (c *Controller) deleteNode(id string) {
	c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketName).Delete([]byte(id)) })
}

func (c *Controller) loadNodes() map[string]*managedNode {
	nodes := make(map[string]*managedNode)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil { return nil }
		return b.ForEach(func(k, v []byte) error {
			var n managedNode
			json.Unmarshal(v, &n)
			nodes[string(k)] = &n
			return nil
		})
	})
	return nodes
}

func (c *Controller) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evaluate(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) evaluate(ctx context.Context) {
	nodes, err := c.docker.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return
	}
	activeWorkers := 0
	for _, n := range nodes {
		if n.Spec.Role == swarm.NodeRoleWorker && n.Spec.Availability == swarm.NodeAvailabilityActive && n.Status.State == swarm.NodeStateReady {
			activeWorkers++
		}
	}
	cpu := c.queryPrometheus(ctx, `avg(1 - rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100`)
	if math.IsNaN(cpu) {
		return
	}
	c.logger.Info("cluster eval", "workers", activeWorkers, "cpu", fmt.Sprintf("%.1f%%", cpu), "managed", c.managedCount())

	coolUp, _ := time.ParseDuration(c.config.CooldownUp)
	coolDn, _ := time.ParseDuration(c.config.CooldownDown)

	if cpu > c.config.ScaleUpCPU && len(nodes) < c.config.MaxNodes && time.Since(c.lastScaleUp) >= coolUp {
		c.scaleUp(ctx)
	} else if cpu < c.config.ScaleDownCPU && activeWorkers > c.config.MinNodes && time.Since(c.lastScaleDn) >= coolDn {
		c.scaleDown(ctx, nodes)
	}
}

func (c *Controller) scaleUp(ctx context.Context) {
	if len(c.providers) == 0 {
		c.logger.Error("no providers configured")
		return
	}
	// Round-robin provider selection
	provider := c.providers[c.providerIdx%len(c.providers)]
	c.providerIdx++

	joinCmd := fmt.Sprintf("docker swarm join --token %s %s:2377", c.config.SwarmToken, c.config.ManagerIP)
	tmpl := c.templateFor(provider.Name())

	c.logger.Info("provisioning node", "provider", provider.Name(), "type", tmpl.InstanceType)
	instanceID, err := provider.Provision(ctx, tmpl, joinCmd)
	if err != nil {
		c.logger.Error("provision failed", "provider", provider.Name(), "error", err)
		return
	}
	c.saveNode(instanceID, &managedNode{InstanceID: instanceID, Provider: provider.Name(), CreatedAt: time.Now()})
	c.mu.Lock()
	c.lastScaleUp = time.Now()
	c.mu.Unlock()
	c.logger.Info("node provisioned", "provider", provider.Name(), "instance", instanceID)
}

func (c *Controller) scaleDown(ctx context.Context, nodes []swarm.Node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for instanceID, managed := range c.loadNodes() {
		if time.Since(managed.CreatedAt) < 10*time.Minute {
			continue
		}
		nodeID := c.findNode(nodes, instanceID)
		if nodeID == "" {
			continue
		}
		node, _, err := c.docker.NodeInspectWithRaw(ctx, nodeID)
		if err != nil {
			continue
		}
		node.Spec.Availability = swarm.NodeAvailabilityDrain
		c.docker.NodeUpdate(ctx, nodeID, node.Version, node.Spec)
		c.logger.Info("node drained", "instance", instanceID)
		time.Sleep(30 * time.Second)
		c.docker.NodeRemove(ctx, nodeID, types.NodeRemoveOptions{Force: true})

		for _, p := range c.providers {
			if p.Name() == managed.Provider {
				p.Terminate(ctx, instanceID)
				break
			}
		}
		c.deleteNode(instanceID)
		c.lastScaleDn = time.Now()
		c.logger.Info("node terminated", "provider", managed.Provider, "instance", instanceID)
		return
	}
}

func (c *Controller) templateFor(provider string) NodeTemplate {
	switch provider {
	case "aws":
		if c.config.Providers.AWS != nil { return c.config.Providers.AWS.Template }
	case "gcp":
		if c.config.Providers.GCP != nil { return c.config.Providers.GCP.Template }
	case "azure":
		if c.config.Providers.Azure != nil { return c.config.Providers.Azure.Template }
	case "digitalocean":
		if c.config.Providers.DigitalOcean != nil { return c.config.Providers.DigitalOcean.Template }
	}
	return NodeTemplate{InstanceType: "t3.large", Image: "ubuntu-24.04", DiskGB: 20}
}

func (c *Controller) findNode(nodes []swarm.Node, instanceID string) string {
	for _, n := range nodes {
		if n.Spec.Labels["swarmex.instance-id"] == instanceID {
			return n.ID
		}
	}
	// Fallback: resolve instance IP via aws/gcloud and match hostname
	for _, p := range c.providers {
		if ip := c.resolveIP(p, instanceID); ip != "" {
			for _, n := range nodes {
				if strings.Contains(n.Description.Hostname, strings.ReplaceAll(ip, ".", "-")) {
					return n.ID
				}
			}
		}
	}
	return ""
}

func (c *Controller) resolveIP(p Provider, instanceID string) string {
	switch p.Name() {
	case "aws":
		out, err := exec.Command("aws", "ec2", "describe-instances",
			"--instance-ids", instanceID, "--query", "Reservations[0].Instances[0].PrivateIpAddress",
			"--output", "text").CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

func (c *Controller) queryPrometheus(ctx context.Context, query string) float64 {
	u := fmt.Sprintf("%s/api/v1/query?query=%s", c.config.Prometheus, url.QueryEscape(query))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil { return math.NaN() }
	defer resp.Body.Close()
	var result struct { Data struct { Result []struct { Value []json.RawMessage `json:"value"` } `json:"result"` } `json:"data"` }
	if json.NewDecoder(resp.Body).Decode(&result) != nil || len(result.Data.Result) == 0 { return math.NaN() }
	if len(result.Data.Result[0].Value) < 2 { return math.NaN() }
	var s string
	json.Unmarshal(result.Data.Result[0].Value[1], &s)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
