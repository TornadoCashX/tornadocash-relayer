package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnvFileRequired loads path like loadDotEnvFile but fails when the
// file is missing or readable by group or others.
func loadDotEnvFileRequired(path string) error {
	if err := requirePrivateFile(path, "environment file"); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("environment file %q does not exist", path)
		}
		return fmt.Errorf("environment file %q is not secure: %w", path, err)
	}
	loaded, err := loadDotEnvFile(path)
	if err != nil {
		return err
	}
	if !loaded {
		return fmt.Errorf("environment file %q does not exist", path)
	}
	return nil
}

// loadDotEnvFile sets variables from path, reporting false if it does not
// exist. A variable already set to a non-empty value in the environment wins;
// this precedence is deliberate and deployment-sensitive.
func loadDotEnvFile(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, nil
}

func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	value = stripInlineComment(value)
	value = unquoteDotEnvValue(value)
	return key, value, true
}

// unquoteDotEnvValue strips one pair of matching surrounding quotes (" or ');
// mismatched or unbalanced quotes are kept verbatim.
func unquoteDotEnvValue(value string) string {
	if len(value) >= 2 {
		if first := value[0]; (first == '"' || first == '\'') && value[len(value)-1] == first {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func stripInlineComment(value string) string {
	var quote rune
	for i, r := range value {
		switch r {
		case '\'', '"':
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
		case '#':
			if quote == 0 && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return strings.TrimSpace(value[:i])
			}
		}
	}
	return value
}
