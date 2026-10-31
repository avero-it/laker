package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The secrets of the adapters (passwords, DSNs) can live in laker-agent.env
// next to the configuration file: KEY=value lines, readable by the owner
// only. laker-agent reads it itself before the configuration; variables
// already set in the environment win. The values never leave this machine.

const envFileName = "laker-agent.env"

func envPath(configPath string) string { return filepath.Join(filepath.Dir(configPath), envFileName) }

func readEnvFile(path string) (map[string]string, []string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ERR80D: %w", err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			return nil, nil, fmt.Errorf("ERR80E: %s holds secrets but others can read it: chmod 600 %s", path, path)
		}
	}
	vars, order := map[string]string{}, []string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := vars[k]; !seen {
			order = append(order, k)
		}
		vars[k] = v
	}
	return vars, order, nil
}

// loadEnvFile sets the variables of the file that the environment lacks.
func loadEnvFile(path string) error {
	vars, _, err := readEnvFile(path)
	if err != nil {
		return err
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}

// saveSecrets adds or replaces variables in the file, written atomically
// with mode 0600.
func saveSecrets(path string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	vars, order, err := readEnvFile(path)
	if err != nil {
		return err
	}
	var added []string
	for k, v := range secrets {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("ERR80F: the value of %s spans several lines", k)
		}
		if _, ok := vars[k]; !ok {
			added = append(added, k)
		}
		vars[k] = v
	}
	sort.Strings(added)
	var buf bytes.Buffer
	buf.WriteString("# secrets of laker-agent: read by laker-agent only, never sent to laker\n")
	for _, k := range append(order, added...) {
		fmt.Fprintf(&buf, "%s=%s\n", k, vars[k])
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".laker-agent-env-")
	if err != nil {
		return fmt.Errorf("ERR810: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("ERR811: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("ERR812: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ERR813: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("ERR814: %w", err)
	}
	return nil
}
