package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseStartupOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want startupOptions
		err  string
	}{
		{name: "separate values", args: []string{"--config", "/a/config.json", "--env", "/a/app.env"}, want: startupOptions{configPath: "/a/config.json", envPath: "/a/app.env"}},
		{name: "equals and reversed", args: []string{"--env=/a/app.env", "--config=/a/config.json"}, want: startupOptions{configPath: "/a/config.json", envPath: "/a/app.env"}},
		{name: "missing config", args: []string{"--env", "x"}, err: "--config is required"},
		{name: "missing env", args: []string{"--config", "x"}, err: "--env is required"},
		{name: "blank path", args: []string{"--config", " ", "--env", "x"}, err: "--config is required"},
		{name: "unknown", args: []string{"--config", "x", "--env", "y", "--other"}, err: "flag provided but not defined"},
		{name: "positional", args: []string{"--config", "x", "--env", "y", "extra"}, err: "unexpected positional arguments"},
		{name: "duplicate", args: []string{"--config", "x", "--config", "z", "--env", "y"}, err: "specified only once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStartupOptions(tt.args)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error=%v, want containing %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("options=%+v error=%v, want %+v", got, err, tt.want)
			}
		})
	}
}

func TestParseStartupOptionsHelp(t *testing.T) {
	if _, err := parseStartupOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error=%v, want flag.ErrHelp", err)
	}
}

func TestRequiredDotEnvFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	if err := loadDotEnvFileRequired(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing env error=%v", err)
	}

	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte("EXPLICIT_ENV_TEST=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXPLICIT_ENV_TEST", "")
	if err := loadDotEnvFileRequired(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("EXPLICIT_ENV_TEST"); got != "loaded" {
		t.Fatalf("EXPLICIT_ENV_TEST=%q", got)
	}
}

func TestRequiredDotEnvRejectsBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not available")
	}
	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte("SECRET=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnvFileRequired(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("broad environment permissions error=%v", err)
	}
}

func TestDotEnvPreservesExistingEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte("ENV_PRECEDENCE_TEST=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_PRECEDENCE_TEST", "process")
	if err := loadDotEnvFileRequired(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ENV_PRECEDENCE_TEST"); got != "process" {
		t.Fatalf("ENV_PRECEDENCE_TEST=%q, want process", got)
	}
}
