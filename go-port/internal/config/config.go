// Package config: configuration, and the vault fetch that supplies this
// service's bearer. Same shape as the TS config.ts.
package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func env(key string, fallback ...string) string {
	value := os.Getenv(key)
	if value == "" {
		if len(fallback) > 0 {
			return fallback[0]
		}
		panic(fmt.Sprintf("FATAL: %s is not set", key))
	}
	return value
}

func envList(key string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(key), ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func oneOf(key string, allowed []string, fallback string) string {
	value := env(key, fallback)
	for _, a := range allowed {
		if value == a {
			return value
		}
	}
	panic(fmt.Sprintf("FATAL: %s must be one of %s, got %q", key, strings.Join(allowed, ", "), value))
}

type Config struct {
	Port      int
	AdminPort int

	// The policy database, on a named volume. Holds hashes only, no secrets.
	DBPath    string
	BackupDir string

	// Who schedules the daily audit prune and DB backup after the boot run:
	// "internal" (a 24 h interval here) or "external" (pipeline-runner calls
	// POST /internal/housekeeping).
	HousekeepingScheduler string

	// Extra exact source addresses allowed on the admin port.
	AdminAllowedSources []string
	// Extra Host header names the admin port answers to (default: loopback only).
	AdminAllowedHosts []string

	// Where broken-only pushes go. The topic itself comes from the vault.
	NtfyBase string

	// Expose only these upstream tools. Empty exposes NOTHING; see the gateway.
	ToolAllowlist []string

	Vault struct {
		URL    string
		Folder string
		Token  string
	}

	HTTPTimeout time.Duration // per vault request, not per boot
}

func Load() *Config {
	var c Config
	c.Port = atoi(env("PORT", "3014"))
	c.AdminPort = atoi(env("ADMIN_PORT", "3015"))
	c.DBPath = env("DB_PATH", "/state/gworkspace-v2.sqlite")
	c.BackupDir = env("BACKUP_DIR", "/state/backups")
	c.HousekeepingScheduler = oneOf("HOUSEKEEPING_SCHEDULER", []string{"internal", "external"}, "internal")
	c.AdminAllowedSources = envList("ADMIN_ALLOWED_SOURCES")
	c.AdminAllowedHosts = envList("ADMIN_ALLOWED_HOSTS")
	c.NtfyBase = env("NTFY_BASE", "https://ntfy.sh")
	c.ToolAllowlist = envList("TOOL_ALLOWLIST")
	c.Vault.URL = env("SECRETS_MANAGER_URL", "http://secret-manager:8000")
	c.Vault.Folder = env("SECRETS_MANAGER_FOLDER", "Root")
	c.Vault.Token = os.Getenv("SECRETS_MANAGER_TOKEN")
	c.HTTPTimeout = 15 * time.Second
	return &c
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// ParseVaultBody extracts the value from a vault response. A plain parse:
// fetch reads exactly one body per attempt, so the bash sibling's glued-body
// failure mode does not exist here. An empty string is a failure, not a value.
func parseVaultBody(text, label string) (string, error) {
	var parsed struct {
		Value *string `json:"value"`
	}
	if err := jsonUnmarshal([]byte(text), &parsed); err != nil {
		return "", fmt.Errorf("FATAL: vault returned unparseable JSON for %s", label)
	}
	if parsed.Value == nil || *parsed.Value == "" {
		return "", fmt.Errorf("FATAL: vault returned no usable value for %s", label)
	}
	return *parsed.Value, nil
}

// VaultFetch reads one secret. optional=true turns a 404 into "" rather than
// fatal. Four attempts, attempt*2s backoff; 401/403/404 never retried.
func (c *Config) VaultFetch(ctx context.Context, key string, optional bool) (string, error) {
	if c.Vault.Token == "" {
		return "", errors.New("FATAL: SECRETS_MANAGER_TOKEN is empty")
	}
	label := c.Vault.Folder + "/" + key
	const attempts = 4
	client := &http.Client{
		Timeout: c.HTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET",
			fmt.Sprintf("%s/api/secrets/%s/%s", c.Vault.URL, c.Vault.Folder, key), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.Vault.Token)
		res, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("FATAL: vault unreachable for %s: %v", label, err)
			if attempt == attempts {
				return "", lastErr
			}
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return "", fmt.Errorf("FATAL: vault unreadable for %s: %v", label, err)
		}
		switch {
		case res.StatusCode == 404 && optional:
			fmt.Printf("[vault] %s is not set\n", label)
			return "", nil
		case res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404:
			return "", fmt.Errorf("FATAL: vault returned HTTP %d for %s", res.StatusCode, label)
		case res.StatusCode < 200 || res.StatusCode >= 300:
			lastErr = fmt.Errorf("FATAL: vault returned HTTP %d for %s", res.StatusCode, label)
			if attempt == attempts {
				return "", lastErr
			}
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		return parseVaultBody(string(body), label)
	}
	return "", lastErr
}
