// Package config loads Orbit's configuration from a properties file, with
// environment variables taking precedence.
//
// Every key can be overridden by an env var: prefix ORBIT_, upper-case, and
// dots become underscores, e.g. logger.level → ORBIT_LOGGER_LEVEL.
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"

	"github.com/aneesh-aparajit/jschd/internal/logger"
)

// DefaultPath is used when no -config flag is given.
const DefaultPath = "resources/properties.yaml"

type Config struct {
	Logger logger.Config `mapstructure:"logger"`
}

func Load(path string) (Config, error) {
	v := viper.New()

	v.SetEnvPrefix("Orbit - Scheduler")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
