// Package config reads process settings from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting of the proxy process.
type Config struct {
	ParamsDir          string        // DBPROXY_PARAMS_DIR
	PgSQLListen        string        // DBPROXY_PGSQL_LISTEN
	MySQLListen        string        // DBPROXY_MYSQL_LISTEN
	MySQLServerVersion string        // DBPROXY_MYSQL_SERVER_VERSION
	ConnectTimeout     time.Duration // DBPROXY_CONNECT_TIMEOUT
	ShutdownTimeout    time.Duration // DBPROXY_SHUTDOWN_TIMEOUT
	LogLevel           slog.Level    // DBPROXY_LOG_LEVEL
	LogSQLMaxLen       int           // DBPROXY_LOG_SQL_MAX_LEN
}

// FromEnv builds the configuration, applying defaults for unset variables.
func FromEnv() (Config, error) {
	c := Config{
		ParamsDir:          env("DBPROXY_PARAMS_DIR", "params"),
		PgSQLListen:        env("DBPROXY_PGSQL_LISTEN", "127.0.0.1:25432"),
		MySQLListen:        env("DBPROXY_MYSQL_LISTEN", "127.0.0.1:23306"),
		MySQLServerVersion: env("DBPROXY_MYSQL_SERVER_VERSION", "8.0.36-db-proxy-ro"),
	}
	var err error
	if c.ConnectTimeout, err = duration("DBPROXY_CONNECT_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = duration("DBPROXY_SHUTDOWN_TIMEOUT", 5*time.Second); err != nil {
		return c, err
	}
	if err = c.LogLevel.UnmarshalText([]byte(env("DBPROXY_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("DBPROXY_LOG_LEVEL: %w", err)
	}
	raw := env("DBPROXY_LOG_SQL_MAX_LEN", "8192")
	if c.LogSQLMaxLen, err = strconv.Atoi(raw); err != nil || c.LogSQLMaxLen < 0 {
		return c, fmt.Errorf("DBPROXY_LOG_SQL_MAX_LEN: %q is not a non-negative integer", raw)
	}
	return c, nil
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func duration(name string, def time.Duration) (time.Duration, error) {
	raw := env(name, "")
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive duration (example: 10s)", name, raw)
	}
	return d, nil
}
