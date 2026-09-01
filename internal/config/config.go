// Package config reads the labeler's configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Config is everything the labeler needs at start-up.
type Config struct {
	// NetBoxURL is the NetBox base URL, normally the tbot tunnel on loopback.
	NetBoxURL string
	// NetBoxToken authenticates against NetBox.
	NetBoxToken string
	// NetBoxTimeout bounds one NetBox HTTP request.
	NetBoxTimeout time.Duration
	// NetBoxWait bounds the start-up wait for NetBox to answer; on expiry the
	// labeler starts anyway and retries lookups with backoff.
	NetBoxWait time.Duration
	// ExcludeRoles lists node roles (node-role.kubernetes.io/<role>) to skip.
	ExcludeRoles []string
	// ReconcilePeriod is how often every node is re-checked against NetBox.
	ReconcilePeriod time.Duration
	// ListenAddr serves /healthz, /readyz and /metrics.
	ListenAddr string
	// LogLevel is the slog level.
	LogLevel slog.Level
	// DryRun logs the patches instead of applying them.
	DryRun bool
	// KubeContext selects a kubeconfig context for runs outside the cluster.
	KubeContext string
}

// FromEnv builds a Config from environment variables. lookup is normally
// os.LookupEnv.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	env := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		NetBoxURL:   env("NETBOX_URL", ""),
		NetBoxToken: env("NETBOX_TOKEN", ""),
		ListenAddr:  env("LISTEN_ADDR", ":8081"),
		KubeContext: env("KUBECONTEXT", ""),
	}
	var errs []error
	if cfg.NetBoxURL == "" {
		errs = append(errs, errors.New("NETBOX_URL is required"))
	}
	if cfg.NetBoxToken == "" {
		errs = append(errs, errors.New("NETBOX_TOKEN is required"))
	}
	for _, r := range strings.Split(env("EXCLUDE_NODE_ROLES", ""), ",") {
		if r = strings.TrimSpace(r); r != "" {
			cfg.ExcludeRoles = append(cfg.ExcludeRoles, r)
		}
	}

	durations := []struct {
		key string
		dst *time.Duration
		def string
	}{
		{"NETBOX_TIMEOUT", &cfg.NetBoxTimeout, "10s"},
		{"NETBOX_WAIT", &cfg.NetBoxWait, "2m"},
		{"RECONCILE_PERIOD", &cfg.ReconcilePeriod, "30m"},
	}
	for _, d := range durations {
		v, err := time.ParseDuration(env(d.key, d.def))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.key, err))
			continue
		}
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", d.key, v))
		}
		*d.dst = v
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	dryRun, err := strconv.ParseBool(env("DRY_RUN", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("DRY_RUN: %w", err))
	}
	cfg.DryRun = dryRun

	return cfg, errors.Join(errs...)
}
