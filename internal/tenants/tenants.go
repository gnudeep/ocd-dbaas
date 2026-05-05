package tenants

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level structure of tenants.yaml.
type Config struct {
	Tenants []Tenant `yaml:"tenants"`
}

// Tenant maps one engineering team to their Kubernetes namespace,
// VLAN network attachment, and resource quotas.
type Tenant struct {
	// Group is the Asgardeo group name for this team (format: <team>-dbs).
	// The OIDC middleware (Task 4) resolves the JWT group claim against this.
	Group string `yaml:"group"`

	// Namespace is the Kubernetes namespace where this tenant's DBInstances live.
	Namespace string `yaml:"namespace"`

	// NetworkRef is the fully-qualified Harvester NAD (<nad-ns>/<nad-name>).
	// Auto-filled on every CreateInstance call so engineers never specify it.
	NetworkRef string `yaml:"networkRef"`

	Quotas Quotas `yaml:"quotas"`
}

// Quotas defines per-tenant resource limits enforced by the gateway
// before any DBInstance is created in Kubernetes.
type Quotas struct {
	// MaxInstances is the maximum number of DBInstances allowed concurrently.
	MaxInstances int `yaml:"maxInstances"`

	// MaxStorageGiB is the total allocated storage across all instances, in GiB.
	MaxStorageGiB int `yaml:"maxStorageGiB"`

	// AllowedInstanceClasses restricts which dbInstanceClass values this tenant
	// may use. Empty slice means all classes are permitted.
	AllowedInstanceClasses []string `yaml:"allowedInstanceClasses"`
}

// Load reads and parses the tenants.yaml file at path.
// The gateway calls this once at startup and holds the result in memory.
// Returns an error if the file is missing, unparseable, or fails validation —
// the gateway refuses to start without a valid tenants config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tenants config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse tenants config %q: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid tenants config: %w", err)
	}

	return &cfg, nil
}

// Lookup finds a tenant by Asgardeo group name.
// Returns nil if no tenant is registered for that group.
func (c *Config) Lookup(group string) *Tenant {
	for i := range c.Tenants {
		if c.Tenants[i].Group == group {
			return &c.Tenants[i]
		}
	}
	return nil
}

// LookupByNamespace finds a tenant by Kubernetes namespace.
// Returns nil if the namespace is not a registered tenant.
func (c *Config) LookupByNamespace(namespace string) *Tenant {
	for i := range c.Tenants {
		if c.Tenants[i].Namespace == namespace {
			return &c.Tenants[i]
		}
	}
	return nil
}

func validate(cfg *Config) error {
	seen := map[string]bool{}
	for i, t := range cfg.Tenants {
		if t.Group == "" {
			return fmt.Errorf("tenant[%d]: group is required", i)
		}
		if t.Namespace == "" {
			return fmt.Errorf("tenant[%d] %q: namespace is required", i, t.Group)
		}
		if t.NetworkRef == "" {
			return fmt.Errorf("tenant[%d] %q: networkRef is required", i, t.Group)
		}
		if seen[t.Group] {
			return fmt.Errorf("duplicate group %q", t.Group)
		}
		seen[t.Group] = true
	}
	return nil
}
