package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "properties.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFromFile(t *testing.T) {
	path := writeFile(t, "logger:\n  level: debug\n  format: json\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logger.Level != "debug" || cfg.Logger.Format != "json" {
		t.Errorf("got %+v, want level=debug format=json", cfg.Logger)
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeFile(t, "{}\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logger.Level != "info" || cfg.Logger.Format != "console" {
		t.Errorf("got %+v, want defaults level=info format=console", cfg.Logger)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeFile(t, "logger:\n  level: debug\n")
	t.Setenv("ORBIT_LOGGER_LEVEL", "warn")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logger.Level != "warn" {
		t.Errorf("level = %q, want env override warn", cfg.Logger.Level)
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	path := writeFile(t, "logger:\n  levle: debug\n")

	if _, err := Load(path); err == nil {
		t.Error("want error for misspelled key logger.levle")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("want error for missing file")
	}
}

func TestShippedFileLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", DefaultPath)); err != nil {
		t.Errorf("%s: %v", DefaultPath, err)
	}
}
