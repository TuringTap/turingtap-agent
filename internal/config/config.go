// Package config loads turingtap-agent configuration from ~/.turingtap/agent.toml
// with per-field TT_AGENT_* environment overrides (env wins).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk agent configuration.
type Config struct {
	// APIKey is the TuringTap MCP API key (ttk_live_...).
	APIKey string `toml:"api_key"`

	// RelayURL is the outbound WSS endpoint on relay.turingtap.ai/agent.
	RelayURL string `toml:"relay_url"`

	// ProxyURL is the cloud MITM proxy Chromium is launched with
	// (--proxy-server value; scheme optional, e.g. http://host:port).
	ProxyURL string `toml:"proxy_url"`

	// ProxyAuth is the bearer/session token presented to the proxy on 407.
	// Sent as Basic auth (username=token, password="x") since Chromium's
	// --proxy-server doesn't carry credentials.
	ProxyAuth string `toml:"proxy_auth"`

	// CASPKI is the base64 SPKI hash of the TuringTap MITM CA, passed to
	// Chromium via --ignore-certificate-errors-spki-list.
	CASPKI string `toml:"ca_spki"`

	// MCPGatewayURL is the cloud MCP SSE endpoint the local server forwards to.
	MCPGatewayURL string `toml:"mcp_gateway_url"`

	// LANAllowCIDRs restricts which destinations the reverse-SOCKS handler
	// will dial on behalf of the cloud proxy. Defaults to RFC1918 + loopback.
	LANAllowCIDRs []string `toml:"lan_allow_cidrs"`

	// RecorderHotkey is reserved for the headed recorder mode.
	RecorderHotkey string `toml:"recorder_hotkey"`

	// LocalMCPAddr is the listen address for the local SSE MCP server.
	// Empty disables the server (see TT_AGENT_MCP_PORT=0).
	LocalMCPAddr string `toml:"local_mcp_addr"`

	// Headless launches Chromium without a window. Default true.
	Headless bool `toml:"headless"`

	// CI adds --no-sandbox --disable-dev-shm-usage to the Chromium launch
	// (containers / e2e). Set via TT_AGENT_CI=1.
	CI bool `toml:"ci"`
}

// DefaultLANCIDRs is RFC1918 + loopback (v4 and v6).
var DefaultLANCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"::1/128",
	"fc00::/7",
}

// DefaultPath returns the default config location: ~/.turingtap/agent.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".turingtap", "agent.toml"), nil
}

// Load reads the config file at path (or DefaultPath if empty), overlays
// TT_AGENT_* environment variables, and applies defaults for unset fields.
// A missing file is not an error as long as env supplies api_key.
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}

	c := Config{Headless: true}
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: decode %s: %w", path, err)
		}
	}

	applyEnv(&c)

	if c.RelayURL == "" {
		c.RelayURL = "wss://relay.turingtap.ai/agent"
	}
	if c.ProxyURL == "" {
		c.ProxyURL = "proxy.turingtap.ai:8443"
	}
	if c.MCPGatewayURL == "" {
		c.MCPGatewayURL = "https://mcp.turingtap.ai"
	}
	if c.LocalMCPAddr == "" && os.Getenv("TT_AGENT_MCP_PORT") == "" {
		c.LocalMCPAddr = "127.0.0.1:7847"
	}
	if len(c.LANAllowCIDRs) == 0 {
		c.LANAllowCIDRs = append([]string(nil), DefaultLANCIDRs...)
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("config: api_key is required (set TT_AGENT_API_KEY or agent.toml)")
	}

	return &c, nil
}

// applyEnv overlays TT_AGENT_<FIELD> env vars. Env wins over toml.
func applyEnv(c *Config) {
	if v := os.Getenv("TT_AGENT_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TT_AGENT_RELAY_URL"); v != "" {
		c.RelayURL = v
	}
	if v := os.Getenv("TT_AGENT_PROXY_URL"); v != "" {
		c.ProxyURL = v
	}
	if v := os.Getenv("TT_AGENT_PROXY_AUTH"); v != "" {
		c.ProxyAuth = v
	}
	if v := os.Getenv("TT_AGENT_CA_SPKI"); v != "" {
		c.CASPKI = v
	}
	if v := os.Getenv("TT_AGENT_MCP_GATEWAY_URL"); v != "" {
		c.MCPGatewayURL = v
	}
	if v := os.Getenv("TT_AGENT_LAN_ALLOW_CIDRS"); v != "" {
		c.LANAllowCIDRs = splitCSV(v)
	}
	if v := os.Getenv("TT_AGENT_RECORDER_HOTKEY"); v != "" {
		c.RecorderHotkey = v
	}
	if v := os.Getenv("TT_AGENT_LOCAL_MCP_ADDR"); v != "" {
		c.LocalMCPAddr = v
	}
	if v := os.Getenv("TT_AGENT_MCP_PORT"); v != "" {
		// Convenience override: "0" disables the local MCP server; any other
		// value sets the port on 127.0.0.1.
		if v == "0" {
			c.LocalMCPAddr = ""
		} else {
			c.LocalMCPAddr = "127.0.0.1:" + v
		}
	}
	if v := os.Getenv("TT_AGENT_HEADLESS"); v != "" {
		c.Headless = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv("TT_AGENT_CI"); v != "" {
		c.CI = v != "0" && !strings.EqualFold(v, "false")
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseCIDRs parses the configured allow-list into net.IPNet values.
func (c *Config) ParseCIDRs() ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(c.LANAllowCIDRs))
	for _, s := range c.LANAllowCIDRs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("config: bad lan_allow_cidrs entry %q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}
