package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestFromEnv_Defaults(t *testing.T) {
	cfg, err := FromEnv(lookupFrom(map[string]string{"NETBOX_URL": "http://127.0.0.1:8080", "NETBOX_TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReconcilePeriod != 30*time.Minute || cfg.NetBoxTimeout != 10*time.Second || cfg.NetBoxWait != 2*time.Minute {
		t.Errorf("durations = %+v", cfg)
	}
	if cfg.ListenAddr != ":8081" || cfg.LogLevel != slog.LevelInfo || cfg.DryRun || len(cfg.ExcludeRoles) != 0 {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestFromEnv_Overrides(t *testing.T) {
	cfg, err := FromEnv(lookupFrom(map[string]string{
		"NETBOX_URL": "u", "NETBOX_TOKEN": "t",
		"EXCLUDE_NODE_ROLES": " master , control-plane,,",
		"RECONCILE_PERIOD":   "5m", "LOG_LEVEL": "debug", "DRY_RUN": "true", "LISTEN_ADDR": ":9999",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExcludeRoles) != 2 || cfg.ExcludeRoles[0] != "master" || cfg.ExcludeRoles[1] != "control-plane" {
		t.Errorf("ExcludeRoles = %v", cfg.ExcludeRoles)
	}
	if cfg.ReconcilePeriod != 5*time.Minute || cfg.LogLevel != slog.LevelDebug || !cfg.DryRun || cfg.ListenAddr != ":9999" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestFromEnv_Errors(t *testing.T) {
	_, err := FromEnv(lookupFrom(map[string]string{"RECONCILE_PERIOD": "soon", "LOG_LEVEL": "loud", "DRY_RUN": "maybe", "NETBOX_WAIT": "-1s"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"NETBOX_URL", "NETBOX_TOKEN", "RECONCILE_PERIOD", "LOG_LEVEL", "DRY_RUN", "NETBOX_WAIT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
