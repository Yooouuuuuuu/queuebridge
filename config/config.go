package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
type Config struct {
	AppName    string       `yaml:"app_name"`   // used as Prometheus metric prefix; default "app"
	Listen     string       `yaml:"listen"`
	GRPCListen string       `yaml:"grpc_listen"`
	STT        STTConfig    `yaml:"stt"`
	TTS        TTSConfig    `yaml:"tts"`
	Pools      []PoolConfig `yaml:"pools"`
}

// PoolConfig describes one named pool of backend workers.
// Defined here (not in broker) so config files and the broker share one type.
type PoolConfig struct {
	Name     string `yaml:"name"`
	Service  string `yaml:"service"`   // "stt" | "tts"
	Protocol string `yaml:"protocol"`  // "ws"  | "grpc"
	Endpoint string `yaml:"endpoint"`  // overrides service-level endpoint when non-empty
	Conns    int    `yaml:"conns"`
}

// STTConfig holds STT service configuration.
type STTConfig struct {
	Endpoint string `yaml:"endpoint"`
	Token    string `yaml:"token"`
	UID      string `yaml:"uid"`
	Domain   string `yaml:"domain"`
}

// TTSConfig holds TTS service configuration.
type TTSConfig struct {
	Endpoint    string  `yaml:"endpoint"`
	Token       string  `yaml:"token"`
	UID         string  `yaml:"uid"`
	ServiceName string  `yaml:"service_name"`
	Speaker     string  `yaml:"speaker"`
	Language    string  `yaml:"language"`
	OutFormat   string  `yaml:"out_format"`
	VBRQuality  int64   `yaml:"vbr_quality"`
	Speed       float32 `yaml:"speed"`
	Gain        float32 `yaml:"gain"`
}

// defaults returns a Config populated with built-in defaults.
// Endpoints and tokens are intentionally empty — they are environment-specific
// and must be supplied via a config file or environment variables.
func defaults() *Config {
	return &Config{
		AppName:    "app",
		Listen:     ":8080",
		GRPCListen: ":9090",
		STT: STTConfig{
			UID:    "go-bridge-user",
			Domain: "freeSTT-zh-TW",
		},
		TTS: TTSConfig{
			UID:         "go-bridge-user",
			ServiceName: "e2e",
			Speaker:     "Sharon",
			Language:    "zh-TW",
			OutFormat:   "wav",
			VBRQuality:  4,
			Speed:       1.0,
			Gain:        1.0,
		},
	}
}

// Load returns configuration built from built-in defaults overlaid with
// environment variables.
func Load() *Config {
	cfg := defaults()
	applyEnv(cfg)
	return cfg
}

// LoadFile reads a YAML config file and returns the merged result:
// built-in defaults → file values → environment variable overrides.
func LoadFile(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	applyEnv(cfg)
	return cfg, nil
}

// DefaultConfig returns a Config with built-in defaults.
// Deprecated: use Load.
func DefaultConfig() *Config { return defaults() }

// applyEnv overlays non-empty environment variables onto cfg.
func applyEnv(cfg *Config) {
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.Listen = v
	}

	if v := os.Getenv("STT_ENDPOINT"); v != "" {
		cfg.STT.Endpoint = v
	}
	if v := os.Getenv("STT_TOKEN"); v != "" {
		cfg.STT.Token = v
	}
	if v := os.Getenv("STT_UID"); v != "" {
		cfg.STT.UID = v
	}
	if v := os.Getenv("STT_DOMAIN"); v != "" {
		cfg.STT.Domain = v
	}

	if v := os.Getenv("TTS_ENDPOINT"); v != "" {
		cfg.TTS.Endpoint = v
	}
	if v := os.Getenv("TTS_TOKEN"); v != "" {
		cfg.TTS.Token = v
	}
	if v := os.Getenv("TTS_UID"); v != "" {
		cfg.TTS.UID = v
	}
	if v := os.Getenv("TTS_SPEAKER"); v != "" {
		cfg.TTS.Speaker = v
	}
	if v := os.Getenv("TTS_LANGUAGE"); v != "" {
		cfg.TTS.Language = v
	}
}

