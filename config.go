package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is data/config.json. It is created with defaults on first start and
// can be edited by hand; unknown/missing keys fall back to defaults.
type Config struct {
	Ports struct {
		Dashboard  int `json:"dashboard"`  // QuickSpin dashboard + API (127.0.0.1)
		HTTP       int `json:"http"`       // FrankenPHP (default PHP); every dev domain is mapped here inside Chromium
		Editor     int `json:"editor"`     // VS Code (serve-web)
		CaddyAdmin int `json:"caddyAdmin"` // admin API of that FrankenPHP process
		MariaDB    int `json:"mariadb"`
		PHPBase    int `json:"phpBase"` // FrankenPHP per extra PHP version: phpBase+i (http), phpBase+50+i (admin)
	} `json:"ports"`

	DefaultPHP string `json:"defaultPhp"` // e.g. "8.5"
	// FrankenPHP + Xdebug builds (release download URL holding manifest.json).
	FrankenPHPBuilds string `json:"frankenphpBuilds"`
	PHPWorkers       int    `json:"phpWorkers"`    // PHP threads per FrankenPHP process
	MariaDBSeries    string `json:"mariadbSeries"` // e.g. "11.8" (LTS)

	Browser struct {
		Disabled        bool     `json:"disabled"`
		Source          string   `json:"source"`          // "chromium" (snapshot build) or "chrome-for-testing"
		ExitWithBrowser bool     `json:"exitWithBrowser"` // closing the browser stops QuickSpin
		ProxyServer     string   `json:"proxyServer"`     // optional upstream proxy, dev domains always bypass it
		ExtraArgs       []string `json:"extraArgs"`
	} `json:"browser"`

	Editor struct {
		Disabled bool `json:"disabled"`
	} `json:"editor"`

	Git struct {
		PreferSystem bool `json:"preferSystem"` // use an installed Git (and its credentials) when found
	} `json:"git"`

	// Paths overrides tool locations, e.g. {"chromium": "D:\\chrome\\chrome.exe",
	// "php-8.5": "D:\\frankenphp", "mariadb": "D:\\mariadb\\bin", "caddy": "...", "code": "...", "git": "..."}.
	Paths map[string]string `json:"paths"`

	path string
}

func defaultConfig() *Config {
	c := &Config{}
	c.Ports.Dashboard = 47080
	c.Ports.HTTP = 47081
	c.Ports.Editor = 47082
	c.Ports.CaddyAdmin = 47083
	c.Ports.MariaDB = 47306
	c.Ports.PHPBase = 47100
	c.DefaultPHP = "8.5"
	c.PHPWorkers = 8
	c.FrankenPHPBuilds = defaultBuildsURL
	c.MariaDBSeries = "11.8"
	c.Browser.Source = "chromium"
	c.Browser.ExitWithBrowser = true
	c.Git.PreferSystem = true
	c.Paths = map[string]string{}
	return c
}

func LoadConfig(path string) (*Config, error) {
	c := defaultConfig()
	c.path = path
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if c.Paths == nil {
		c.Paths = map[string]string{}
	}
	if c.PHPWorkers < 1 {
		c.PHPWorkers = 1
	}
	if c.PHPWorkers > 64 {
		c.PHPWorkers = 64
	}
	if phpIndex(c.DefaultPHP) < 0 {
		c.DefaultPHP = phpCatalog[len(phpCatalog)-1] // e.g. a version QuickSpin no longer offers
	}
	return c, c.Save() // writes defaults for any missing keys
}

func (c *Config) Save() error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(c.path, b)
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
