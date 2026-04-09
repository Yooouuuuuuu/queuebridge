package config

import (
	"os"
)

// Config holds the application configuration
type Config struct {
	STT STTConfig
	TTS TTSConfig
}

// STTConfig holds STT service configuration
type STTConfig struct {
	Endpoint string
	Token    string
	UID      string
	Domain   string
}

// TTSConfig holds TTS service configuration
type TTSConfig struct {
	Endpoint    string
	Token       string
	UID         string
	ServiceName string
	Speaker     string
	Language    string
	OutFormat   string
	VBRQuality  int64
	Speed       float32
	Gain        float32
}

// DefaultConfig returns configuration with default values
func DefaultConfig() *Config {
	return &Config{
		STT: STTConfig{
			Endpoint: "ws://10.1.8.174:8890/SttProxy/recognition",
			Token:    getEnv("STT_TOKEN", "g9GzWBorWT9in7xIg79xhxWo9I3aNgczEPGsLNWFtX21sIoP7lFCOJ3JzZdiU0z7OV9Jt8mO-ZtVqfHH3N1RftmCKnehSkHdDur3-tz1hSo14JjGKrBIq2X8zbE69fHo"),
			UID:      getEnv("STT_UID", "go-bridge-user"),
			Domain:   "freeSTT-zh-TW",
		},
		TTS: TTSConfig{
			Endpoint:    "10.1.8.174:8088",
			Token:       getEnv("TTS_TOKEN", "qfGfDLk8LoH6BrBQymcnrIvgCv-qeZqIrmAuuqJWSoPo7DNr_gtIjRtKPt2eQcYcYqQtTy9Jws-fnCH9_Sb2Y_cUk9pQUbgc2iww07FULCfXTf7BciUSltAMqIZhk5pa"),
			UID:        getEnv("TTS_UID", "go-bridge-user"),
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

// Load loads configuration from environment variables with defaults
func Load() *Config {
	cfg := DefaultConfig()

	// Override with environment variables if set
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

	return cfg
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
