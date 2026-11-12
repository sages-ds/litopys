package ops

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
)

// Manifest is litopys-release.yaml, the release artifact next to the image: images by
// digest, so "what really runs" is the container digest against this file, not a tag.
type Manifest struct {
	Version     string            `yaml:"version" json:"version"`
	Commit      string            `yaml:"commit" json:"commit"`
	Images      map[string]string `yaml:"images" json:"images"` // service role (core, postgres, keycloak) -> repo@sha256:…
	Schema      ManifestSchema    `yaml:"schema" json:"schema"`
	UpgradeFrom string            `yaml:"upgrade_from,omitempty" json:"upgrade_from,omitempty"`
}

// ManifestSchema is what the release expects of the database.
type ManifestSchema struct {
	MigrationHead    string `yaml:"migration_head" json:"migration_head"` // file name of the last migration
	ProjectorVersion string `yaml:"projector_version" json:"projector_version"`
}

// LoadManifest reads a release manifest.
func LoadManifest(path string) (*Manifest, error) {
	if path == "" {
		return nil, errors.New("no manifest in install config")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	if m.Schema.MigrationHead == "" || m.Schema.ProjectorVersion == "" {
		return nil, fmt.Errorf("manifest %s: schema.migration_head and schema.projector_version are required", path)
	}
	return &m, nil
}

// BuildManifest describes this binary as a release: the schema comes from the embedded migrations and
// the projector of this code, so the manifest cannot disagree with the image built from the same commit.
// coreImage is repo@sha256:… as the registry reported it after the push.
func BuildManifest(version, commit, coreImage string) (*Manifest, error) {
	if !strings.Contains(coreImage, "@sha256:") {
		return nil, fmt.Errorf("core image %q: want repo@sha256:<digest>", coreImage)
	}
	head, err := migrationHead()
	if err != nil {
		return nil, err
	}
	return &Manifest{Version: version, Commit: commit, Images: map[string]string{"core": coreImage},
		Schema: ManifestSchema{MigrationHead: head, ProjectorVersion: projector.Version}}, nil
}

// Marshal renders the manifest as YAML.
func (m *Manifest) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func migrationHead() (string, error) {
	all, err := migrate.List()
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "", errors.New("no embedded migrations")
	}
	return all[len(all)-1].Name, nil
}
