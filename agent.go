package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// AgentConfig is the parsed topology config fetched from the config server.
type AgentConfig struct {
	Topology      string
	ProbeInterval time.Duration
	BurstSize     int
	BurstInterval time.Duration
	PacketTimeout time.Duration
	Padding       int
	ConfigRefresh time.Duration
	InfluxDB      InfluxConfig
	Hosts         []HostEntry
	HubSpoke      HubSpokeConfig
}

type HostEntry struct {
	Name    string
	Address string
	Site    string
}

type HubSpokeConfig struct {
	Enabled bool
	Hub     string
}

type InfluxConfig struct {
	URL    string
	Token  string
	Org    string
	Bucket string
}

// ProbeResult carries aggregated statistics for one burst to one target.
type ProbeResult struct {
	Source    string
	Target    string
	Site      string
	Topology  string
	RttMin    time.Duration
	RttAvg    time.Duration
	RttMax    time.Duration
	RttStddev time.Duration
	Jitter    time.Duration
	LossPct   float64
	SentAt    time.Time
	Sent      int
	Recv      int
}

// rawAgentConfig mirrors AgentConfig with string durations for JSON decoding.
type rawAgentConfig struct {
	Topology      string          `json:"topology"`
	ProbeInterval string          `json:"probe_interval"`
	BurstSize     int             `json:"burst_size"`
	BurstInterval string          `json:"burst_interval"`
	PacketTimeout string          `json:"packet_timeout"`
	Padding       int             `json:"padding"`
	ConfigRefresh string          `json:"config_refresh"`
	InfluxDB      rawInfluxConfig `json:"influxdb"`
	Hosts         []rawHostEntry  `json:"hosts"`
	HubSpoke      rawHubSpoke     `json:"hub_spoke"`
}

type rawInfluxConfig struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	Org    string `json:"org"`
	Bucket string `json:"bucket"`
}

type rawHostEntry struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Site    string `json:"site"`
}

type rawHubSpoke struct {
	Enabled bool   `json:"enabled"`
	Hub     string `json:"hub"`
}

func parseAgentConfig(data []byte) (AgentConfig, error) {
	var raw rawAgentConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgentConfig{}, fmt.Errorf("JSON parse error: %w", err)
	}
	if raw.Topology == "" {
		return AgentConfig{}, fmt.Errorf("config missing required field: topology")
	}

	parseDur := func(s, name, def string) (time.Duration, error) {
		if s == "" {
			s = def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration for %s %q: %w", name, s, err)
		}
		return d, nil
	}

	probeInterval, err := parseDur(raw.ProbeInterval, "probe_interval", "60s")
	if err != nil {
		return AgentConfig{}, err
	}
	burstInterval, err := parseDur(raw.BurstInterval, "burst_interval", "200ms")
	if err != nil {
		return AgentConfig{}, err
	}
	packetTimeout, err := parseDur(raw.PacketTimeout, "packet_timeout", "5s")
	if err != nil {
		return AgentConfig{}, err
	}
	configRefresh, err := parseDur(raw.ConfigRefresh, "config_refresh", "5m")
	if err != nil {
		return AgentConfig{}, err
	}

	burstSize := raw.BurstSize
	if burstSize <= 0 {
		burstSize = 5
	}

	hosts := make([]HostEntry, len(raw.Hosts))
	for i, h := range raw.Hosts {
		hosts[i] = HostEntry{Name: h.Name, Address: h.Address, Site: h.Site}
	}

	return AgentConfig{
		Topology:      raw.Topology,
		ProbeInterval: probeInterval,
		BurstSize:     burstSize,
		BurstInterval: burstInterval,
		PacketTimeout: packetTimeout,
		Padding:       raw.Padding,
		ConfigRefresh: configRefresh,
		InfluxDB: InfluxConfig{
			URL:    raw.InfluxDB.URL,
			Token:  raw.InfluxDB.Token,
			Org:    raw.InfluxDB.Org,
			Bucket: raw.InfluxDB.Bucket,
		},
		Hosts: hosts,
		HubSpoke: HubSpokeConfig{
			Enabled: raw.HubSpoke.Enabled,
			Hub:     raw.HubSpoke.Hub,
		},
	}, nil
}

// targetsFor returns the list of hosts this agent should probe given its hostname.
// Returns empty slice if hostname not found in Hosts (agent acts as reflector only).
func (c AgentConfig) targetsFor(hostname string) []HostEntry {
	found := false
	for _, h := range c.Hosts {
		if h.Name == hostname {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	if c.HubSpoke.Enabled {
		if hostname == c.HubSpoke.Hub {
			// Hub probes all spokes
			var spokes []HostEntry
			for _, h := range c.Hosts {
				if h.Name != c.HubSpoke.Hub {
					spokes = append(spokes, h)
				}
			}
			return spokes
		}
		// Spoke probes hub only
		for _, h := range c.Hosts {
			if h.Name == c.HubSpoke.Hub {
				return []HostEntry{h}
			}
		}
		return nil
	}

	// Mesh: probe everyone except self
	var targets []HostEntry
	for _, h := range c.Hosts {
		if h.Name != hostname {
			targets = append(targets, h)
		}
	}
	return targets
}
