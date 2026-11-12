// Package ops holds the operations commands of the litopys binary that run on the host of an
// installation, not in its containers: status and doctor now, backup / update later.
//
// Everything here only reads. The installation is described by install.yaml (no secrets); the
// database credentials come from the environment as for every other command (internal/pg).
package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfig is where an installation keeps its description.
const DefaultConfig = "/etc/litopys/install.yaml"

// Config is install.yaml: what the installation consists of and where to look. No secrets.
type Config struct {
	Target   string `yaml:"target"`  // truenas | incus
	Release  string `yaml:"release"` // the release the installation is meant to run (informational)
	Manifest string `yaml:"manifest"`

	Services struct {
		Core string `yaml:"core"`
		DB   string `yaml:"db"`
		IdP  string `yaml:"idp"`
	} `yaml:"services"`

	// Endpoint is the core service as the host sees it (/healthz, /version); PublicURL is the address
	// clients use — its certificate is checked; IdPIssuer is the OpenID issuer of the identity provider.
	Endpoint  string `yaml:"endpoint"`
	PublicURL string `yaml:"public_url"`
	IdPIssuer string `yaml:"idp_issuer"`

	Backup struct {
		Dir      string `yaml:"dir"`
		KeepDays int    `yaml:"keep_days"`
	} `yaml:"backup"`

	DataPool string `yaml:"data_pool"`
	// DBDataset is the dataset of the database's data (PGDATA with its WAL): the update snapshots it.
	DBDataset string `yaml:"db_dataset"`
	// AcceptedFindings are verify findings this installation lives with by the owner's decision, each written out in
	// full, its count included: the verify of an update (the rehearsal on a copy and the live upgrade) passes a finding
	// only when it is word for word one of these, so any change of it (another count) stops it again. The pilot: imported records numbered after native ones
	// (#275). A production installation has none.
	AcceptedFindings []string `yaml:"accepted_findings"`
	// CoreCompose is the compose template of the core service (placeholders, no values) the update renders.
	CoreCompose string `yaml:"core_compose"`

	TrueNAS struct {
		SSH    string `yaml:"ssh"`     // user@host of a login allowed to run midclt
		SSHKey string `yaml:"ssh_key"` // private key path; ~ is the home directory
	} `yaml:"truenas"`
}

// LoadConfig reads install.yaml; path "" means LITOPYS_INSTALL_CONFIG, then DefaultConfig.
// Relative paths inside (manifest, backup dir, ssh key) are taken from the file's directory.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("LITOPYS_INSTALL_CONFIG")
	}
	if path == "" {
		path = DefaultConfig
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("install config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("install config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("install config %s: %w", path, err)
	}
	base := filepath.Dir(path)
	c.Manifest = resolve(base, c.Manifest)
	c.CoreCompose = resolve(base, c.CoreCompose)
	c.Backup.Dir = resolve(base, c.Backup.Dir)
	c.TrueNAS.SSHKey = resolve(base, c.TrueNAS.SSHKey)
	return &c, nil
}

func (c *Config) validate() error {
	switch c.Target {
	case "truenas":
		if c.TrueNAS.SSH == "" {
			return errors.New("target truenas needs truenas.ssh")
		}
	case "incus":
		return errors.New("target incus is not implemented yet")
	default:
		return fmt.Errorf("target %q: want truenas | incus", c.Target)
	}
	if c.Services.Core == "" || c.Endpoint == "" {
		return errors.New("services.core and endpoint are required")
	}
	return nil
}

func resolve(base, p string) string {
	switch {
	case p == "":
		return ""
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		return p
	case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		return p
	default:
		return filepath.Join(base, p)
	}
}
