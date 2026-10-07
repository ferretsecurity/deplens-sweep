package sweep

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Selection struct {
	Repositories []string `yaml:"repositories" json:"repositories,omitempty"`
}
type Connection struct {
	Name            string    `yaml:"name" json:"name"`
	Provider        string    `yaml:"provider" json:"provider"`
	ServerURL       string    `yaml:"server_url" json:"server_url"`
	TokenEnv        string    `yaml:"token_env" json:"-"`
	Include         Selection `yaml:"include" json:"include"`
	Exclude         Selection `yaml:"exclude" json:"exclude"`
	IncludeArchived bool      `yaml:"include_archived" json:"include_archived"`
	IncludeForks    bool      `yaml:"include_forks" json:"include_forks"`
	token           string
}
type ScanConfig struct {
	Executable     string   `yaml:"executable" json:"-"`
	Rules          string   `yaml:"rules" json:"rules,omitempty"`
	ExtendRules    []string `yaml:"extend_rules" json:"extend_rules,omitempty"`
	Ignore         []string `yaml:"ignore" json:"ignore"`
	DisableRules   []string `yaml:"disable_rules" json:"disable_rules,omitempty"`
	DisableChecks  []string `yaml:"disable_checks" json:"disable_checks,omitempty"`
	ExcludePresets []string `yaml:"exclude_presets" json:"exclude_presets,omitempty"`
}
type Config struct {
	Workers     int           `yaml:"workers"`
	Timeout     time.Duration `yaml:"timeout"`
	ResultsDir  string        `yaml:"results_dir"`
	TempDir     string        `yaml:"temp_dir"`
	Connections []Connection  `yaml:"connections"`
	Deplens     ScanConfig    `yaml:"deplens"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{Workers: 3, Timeout: 60 * time.Minute}
	data, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open configuration: %w", err)
	}
	defer data.Close()
	dec := yaml.NewDecoder(data)
	dec.KnownFields(true)
	if err = dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid or unsupported configuration: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, errors.New("configuration must contain one YAML document")
	}
	if cfg.Workers <= 0 {
		return cfg, errors.New("workers must be positive")
	}
	if cfg.Timeout <= 0 {
		return cfg, errors.New("timeout must be a positive duration")
	}
	if cfg.ResultsDir == "" {
		return cfg, errors.New("results_dir is required")
	}
	if len(cfg.Connections) != 1 {
		return cfg, errors.New("this version requires exactly one connection")
	}
	names := map[string]bool{}
	for i := range cfg.Connections {
		c := &cfg.Connections[i]
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(c.Name) || names[c.Name] {
			return cfg, errors.New("connection names must be unique and contain only letters, numbers, underscores or hyphens")
		}
		names[c.Name] = true
		if c.Provider != "gitlab" && c.Provider != "github" {
			return cfg, errors.New("provider must be gitlab or github")
		}
		u, e := url.Parse(c.ServerURL)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return cfg, errors.New("server_url must be an HTTP(S) server URL without credentials, query or fragment")
		}
		c.ServerURL = strings.TrimRight(c.ServerURL, "/")
		if c.Provider == "github" && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "github.com") && (u.Port() == "" || u.Port() == "443") && strings.Trim(u.Path, "/") == "" {
			c.ServerURL = "https://github.com"
		}
		if len(c.Include.Repositories) == 0 {
			return cfg, errors.New("explicit repository selection is required")
		}
		for _, paths := range [][]string{c.Include.Repositories, c.Exclude.Repositories} {
			for _, p := range paths {
				if p == "" || strings.Trim(p, "/") != p || strings.ContainsAny(p, "*?\\\n\r") || strings.Contains(p, "..") {
					return cfg, errors.New("repository selections must be exact nonempty project paths")
				}
			}
		}
		c.token = os.Getenv(c.TokenEnv)
		if c.TokenEnv == "" || c.token == "" {
			return cfg, errors.New("connection token environment variable is missing or empty")
		}
	}
	base, e := filepath.Abs(filepath.Dir(path))
	if e != nil {
		return cfg, e
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	cfg.ResultsDir = resolve(cfg.ResultsDir)
	cfg.TempDir = resolve(cfg.TempDir)
	if cfg.Deplens.Executable == "" {
		cfg.Deplens.Executable = "deplens"
	} else if strings.ContainsAny(cfg.Deplens.Executable, "/\\") {
		cfg.Deplens.Executable = resolve(cfg.Deplens.Executable)
	}
	cfg.Deplens.Rules = resolve(cfg.Deplens.Rules)
	for i, p := range cfg.Deplens.ExtendRules {
		cfg.Deplens.ExtendRules[i] = resolve(p)
	}
	if cfg.Deplens.Ignore == nil {
		cfg.Deplens.Ignore = []string{".git", "node_modules", ".venv", "venv", "vendor", ".tox", ".mypy_cache", ".pytest_cache"}
	}
	return cfg, nil
}
