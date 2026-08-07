package config

import (
	"testing"
	"time"
)

func TestLoadRequiresDBPath(t *testing.T) {
	t.Setenv("PIUMY_DB_PATH", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() with no PIUMY_DB_PATH: want an error, got nil")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PIUMY_DB_PATH", "piumy.db")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RouterPath != "router.json" {
		t.Errorf("RouterPath = %q, want router.json", cfg.RouterPath)
	}
	if cfg.StatusPath != "status.json" {
		t.Errorf("StatusPath = %q, want status.json", cfg.StatusPath)
	}
	if cfg.SwampedAt != 8 {
		t.Errorf("SwampedAt = %d, want 8", cfg.SwampedAt)
	}
	if cfg.RateLimitPerMin != 10 || cfg.RateLimitPerDay != 500 {
		t.Errorf("RateLimitPerMin/Day = %d/%d, want 10/500", cfg.RateLimitPerMin, cfg.RateLimitPerDay)
	}
	if cfg.WifiIface != "wlan0" {
		t.Errorf("WifiIface = %q, want wlan0", cfg.WifiIface)
	}
	if cfg.MCPAddr != ":8091" || cfg.RESTAddr != ":8092" {
		t.Errorf("MCPAddr/RESTAddr = %q/%q, want :8091/:8092", cfg.MCPAddr, cfg.RESTAddr)
	}
	if cfg.RESTKey != "" {
		t.Errorf("RESTKey = %q, want empty (open dev/LAN default)", cfg.RESTKey)
	}
	if cfg.PolicyPath != "" {
		t.Errorf("PolicyPath = %q, want empty (falls back to each package's embedded default)", cfg.PolicyPath)
	}
}

// TestF5AddrEnvOverrides covers PIUMY_MCP_ADDR/REST_ADDR/REST_KEY/POLICY_PATH
// (F5-wire) — env overrides the default listen addrs and the open-by-default
// REST key.
func TestF5AddrEnvOverrides(t *testing.T) {
	t.Setenv("PIUMY_DB_PATH", "piumy.db")
	t.Setenv("PIUMY_MCP_ADDR", ":9001")
	t.Setenv("PIUMY_REST_ADDR", ":9002")
	t.Setenv("PIUMY_REST_KEY", "secret")
	t.Setenv("PIUMY_POLICY_PATH", "/etc/piumy/policy.md")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPAddr != ":9001" || cfg.RESTAddr != ":9002" {
		t.Errorf("MCPAddr/RESTAddr = %q/%q, want :9001/:9002", cfg.MCPAddr, cfg.RESTAddr)
	}
	if cfg.RESTKey != "secret" {
		t.Errorf("RESTKey = %q, want secret", cfg.RESTKey)
	}
	if cfg.PolicyPath != "/etc/piumy/policy.md" {
		t.Errorf("PolicyPath = %q, want /etc/piumy/policy.md", cfg.PolicyPath)
	}
}

// TestDispatchDelayEnv covers env override and the anti-ban invariant: a
// non-positive value must never be honored (it would mean instant sends).
func TestDispatchDelayEnv(t *testing.T) {
	t.Setenv("PIUMY_DB_PATH", "piumy.db")

	t.Run("defaults when unset", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DispatchDelayMin != 1*time.Second || cfg.DispatchDelayMax != 5*time.Second {
			t.Errorf("DispatchDelayMin/Max = %v/%v, want 1s/5s", cfg.DispatchDelayMin, cfg.DispatchDelayMax)
		}
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("PIUMY_DELAY_DISPATCH_MIN", "2s")
		t.Setenv("PIUMY_DELAY_DISPATCH_MAX", "9s")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DispatchDelayMin != 2*time.Second || cfg.DispatchDelayMax != 9*time.Second {
			t.Errorf("DispatchDelayMin/Max = %v/%v, want 2s/9s", cfg.DispatchDelayMin, cfg.DispatchDelayMax)
		}
	})

	t.Run("non-positive falls back to default (never instant)", func(t *testing.T) {
		t.Setenv("PIUMY_DELAY_DISPATCH_MIN", "0s")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DispatchDelayMin != 1*time.Second {
			t.Errorf("DispatchDelayMin = %v, want default 1s (0 must not be honored)", cfg.DispatchDelayMin)
		}
	})
}
