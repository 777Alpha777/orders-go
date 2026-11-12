package config

import (
	"os"
	"strconv"
	"time"
)

// Config is the whole runtime configuration. Values come from the environment
// so the same binary runs locally, in Docker and in CI without rebuilds.
type Config struct {
	Addr            string
	DatabaseURL     string
	ShutdownTimeout time.Duration
	MaxPoolConns    int32
}

func Load() Config {
	return Config{
		Addr:            env("HTTP_ADDR", ":8080"),
		DatabaseURL:     env("DATABASE_URL", "postgres://postgres:postgres@localhost:5433/orders_go"),
		ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		MaxPoolConns:    int32(envInt("DB_MAX_CONNS", 10)),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}
