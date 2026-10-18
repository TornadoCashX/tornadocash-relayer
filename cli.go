package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const startupUsage = "go-withdraw-relayer --config <path> --env <path>"

type startupOptions struct {
	configPath string
	envPath    string
}

func parseStartupOptions(args []string) (startupOptions, error) {
	var opts startupOptions
	var configCount, envCount int
	flags := flag.NewFlagSet("go-withdraw-relayer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Func("config", "path to relayer config JSON", func(value string) error {
		configCount++
		opts.configPath = value
		return nil
	})
	flags.Func("env", "path to environment file", func(value string) error {
		envCount++
		opts.envPath = value
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return startupOptions{}, fmt.Errorf("parse startup arguments: %w", err)
	}
	if configCount > 1 || envCount > 1 {
		return startupOptions{}, errors.New("--config and --env may each be specified only once")
	}
	if flags.NArg() != 0 {
		return startupOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(opts.configPath) == "" {
		return startupOptions{}, errors.New("--config is required")
	}
	if strings.TrimSpace(opts.envPath) == "" {
		return startupOptions{}, errors.New("--env is required")
	}
	return opts, nil
}
