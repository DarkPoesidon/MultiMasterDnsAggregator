package appcontrol

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/multipath"
)

type TunnelConfig struct {
	Label      string `json:"label"`
	SOCKS5Addr string `json:"socks5_addr"`
	Weight     int    `json:"weight"`
}

type AppConfig struct {
	ListenAddr      string         `json:"listen_addr"`
	AggregatorAddr  string         `json:"aggregator_addr"`
	ChunkSize       int            `json:"chunk_size"`
	DialTimeoutSec  int            `json:"dial_timeout_sec"`
	ReconnectSec    int            `json:"reconnect_sec"`
	ReadBufferSize  int            `json:"read_buffer_size"`
	InboundDepth    int            `json:"inbound_depth"`
	DispatchRetries int            `json:"dispatch_retries"`
	Tunnels         []TunnelConfig `json:"tunnels"`
}

func DefaultAppConfig() AppConfig {
	base := multipath.DefaultConfig()
	return AppConfig{
		ListenAddr:      base.ListenAddr,
		AggregatorAddr:  base.AggregatorAddr,
		ChunkSize:       base.ChunkSize,
		DialTimeoutSec:  int(base.DialTimeout.Seconds()),
		ReconnectSec:    int(base.ReconnectDelay.Seconds()),
		ReadBufferSize:  base.ReadBufferSize,
		InboundDepth:    base.InboundChannelDepth,
		DispatchRetries: base.DispatchRetries,
		Tunnels: []TunnelConfig{
			{Label: "tunnel-1", SOCKS5Addr: "127.0.0.1:18001", Weight: 1},
			{Label: "tunnel-2", SOCKS5Addr: "127.0.0.1:18002", Weight: 1},
			{Label: "tunnel-3", SOCKS5Addr: "127.0.0.1:18003", Weight: 1},
			{Label: "tunnel-4", SOCKS5Addr: "127.0.0.1:18004", Weight: 1},
			{Label: "tunnel-5", SOCKS5Addr: "127.0.0.1:18005", Weight: 1},
		},
	}
}

func (c AppConfig) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.AggregatorAddr == "" {
		return errors.New("aggregator_addr is required")
	}
	if c.ChunkSize <= 0 {
		return errors.New("chunk_size must be > 0")
	}
	if c.DialTimeoutSec <= 0 {
		return errors.New("dial_timeout_sec must be > 0")
	}
	if c.ReconnectSec <= 0 {
		return errors.New("reconnect_sec must be > 0")
	}
	if len(c.Tunnels) == 0 {
		return errors.New("at least one tunnel is required")
	}
	for _, t := range c.Tunnels {
		if t.SOCKS5Addr == "" {
			return errors.New("tunnel socks5_addr cannot be empty")
		}
	}
	return nil
}

func (c AppConfig) ToMultipathConfig() multipath.MultipathConfig {
	tunnels := make([]multipath.TunnelEndpoint, 0, len(c.Tunnels))
	for _, t := range c.Tunnels {
		tunnels = append(tunnels, multipath.TunnelEndpoint{
			Label:      t.Label,
			SOCKS5Addr: t.SOCKS5Addr,
			Weight:     t.Weight,
		})
	}
	return multipath.MultipathConfig{
		ListenAddr:          c.ListenAddr,
		AggregatorAddr:      c.AggregatorAddr,
		ChunkSize:           c.ChunkSize,
		DialTimeout:         time.Duration(c.DialTimeoutSec) * time.Second,
		ReconnectDelay:      time.Duration(c.ReconnectSec) * time.Second,
		ReadBufferSize:      c.ReadBufferSize,
		InboundChannelDepth: c.InboundDepth,
		DispatchRetries:     c.DispatchRetries,
		Tunnels:             tunnels,
	}
}

func LoadConfig(path string) (AppConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, err
	}
	cfg := DefaultAppConfig()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return AppConfig{}, err
	}
	return cfg, cfg.Validate()
}

func SaveConfig(path string, cfg AppConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
