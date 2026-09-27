package main

import (
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/aneesh-aparajit/jschd/internal/config"
	"github.com/aneesh-aparajit/jschd/internal/logger"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "path to the properties file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orbit:", err)
		os.Exit(1)
	}

	log, err := logger.New(cfg.Logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orbit:", err)
		os.Exit(1)
	}
	defer logger.Sync(log)
	zap.ReplaceGlobals(log)

	log.Info("orbit starting", zap.String("config", *configPath))
}
