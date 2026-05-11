package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetSecretEnvUsesDirectEnvFirst(t *testing.T) {
	clearSecretEnv(t, "TAILSCALE_OAUTH_CLIENT_SECRET")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "from-env")
	t.Setenv("FILE__TAILSCALE_OAUTH_CLIENT_SECRET", writeTestSecretFile(t, "from-file"))

	value := getSecretEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	if value != "from-env" {
		t.Fatalf("expected direct env value, got %q", value)
	}
}

func TestGetSecretEnvSupportsFilePrefix(t *testing.T) {
	clearSecretEnv(t, "TAILSCALE_OAUTH_CLIENT_SECRET")
	t.Setenv("FILE__TAILSCALE_OAUTH_CLIENT_SECRET", writeTestSecretFile(t, "from-file\n"))

	value := getSecretEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	if value != "from-file" {
		t.Fatalf("expected FILE__ value with newline trimmed, got %q", value)
	}
}

func TestGetSecretEnvSupportsFileSuffix(t *testing.T) {
	clearSecretEnv(t, "TAILSCALE_OAUTH_CLIENT_SECRET")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET_FILE", writeTestSecretFile(t, "from-suffix\r\n"))

	value := getSecretEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	if value != "from-suffix" {
		t.Fatalf("expected _FILE value with newline trimmed, got %q", value)
	}
}

func TestGetSecretEnvUsesDefaultWhenUnset(t *testing.T) {
	clearSecretEnv(t, "TAILSCALE_OAUTH_CLIENT_SECRET")

	value := getSecretEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "default")
	if value != "default" {
		t.Fatalf("expected default value, got %q", value)
	}
}

func clearSecretEnv(t *testing.T, key string) {
	t.Helper()

	t.Setenv(key, "")
	t.Setenv("FILE__"+key, "")
	t.Setenv(key+"_FILE", "")
}

func writeTestSecretFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test secret: %v", err)
	}

	return path
}
