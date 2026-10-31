package config

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Example is the configuration written by `laker config create`.
//
//go:embed example.yaml
var Example []byte

// SearchPaths are tried in order when no file is given.
var SearchPaths = []string{"/etc/laker/config.yaml", "./config.yaml"}

// Locate returns path if set, otherwise the first of SearchPaths that exists.
func Locate(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	for _, p := range SearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("ERR700: no configuration file found in %s", strings.Join(SearchPaths, ", "))
}

// Load reads the file at path into a Config. Decoding is strict: a key the
// struct does not know is an error. ${VAR} in any string value is replaced
// with the environment variable VAR, which must be set.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ERR701: unable to read the configuration file: %w", err)
	}
	return Parse(b)
}

// Parse is Load on bytes.
func Parse(b []byte) (*Config, error) {
	cfg := &Config{}
	return cfg, parseInto(b, cfg)
}

// parseInto decodes b strictly into out (a pointer to a struct) and expands
// ${VAR} in its strings.
func parseInto(b []byte, out any) error {
	if err := decodeInto(b, out); err != nil {
		return err
	}
	return expandEnv(reflect.ValueOf(out).Elem())
}

// decodeInto decodes b strictly into out, leaving ${VAR} as written.
func decodeInto(b []byte, out any) error {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(b)); err != nil {
		return fmt.Errorf("ERR702: invalid yaml: %w", err)
	}
	err := v.UnmarshalExact(out, func(dc *mapstructure.DecoderConfig) {
		dc.TagName = "yaml"
	})
	if err != nil {
		return fmt.Errorf("ERR703: unable to decode the configuration: %w", err)
	}
	return nil
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} in every string reachable from v, adapter params
// included. Values are expanded after decoding, so a $ in a comment or in the
// expanded secret itself never reaches the yaml parser.
func expandEnv(v reflect.Value) error {
	var missing []string

	var walk func(v reflect.Value) reflect.Value
	expand := func(s string) string {
		return envRE.ReplaceAllStringFunc(s, func(m string) string {
			name := envRE.FindStringSubmatch(m)[1]
			val, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
	}
	walk = func(v reflect.Value) reflect.Value {
		switch v.Kind() {
		case reflect.String:
			if v.CanSet() {
				v.SetString(expand(v.String()))
				return v
			}
			return reflect.ValueOf(expand(v.String()))
		case reflect.Struct:
			// the params of a disabled adapter are left as written: a stub
			// needs no variable set until it is enabled
			disabled := v.Type() == reflect.TypeOf(Adapter{}) && !v.FieldByName("Enabled").Bool()
			for i := 0; i < v.NumField(); i++ {
				if disabled && v.Type().Field(i).Name == "Params" {
					continue
				}
				walk(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				e := v.Index(i)
				if r := walk(e); e.Kind() == reflect.Interface && r.IsValid() {
					e.Set(r)
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				if r := walk(v.MapIndex(k)); r.IsValid() {
					v.SetMapIndex(k, r)
				}
			}
		case reflect.Interface:
			if v.IsNil() {
				return reflect.Value{}
			}
			return walk(v.Elem())
		}
		return v
	}
	walk(v)

	if len(missing) > 0 {
		return fmt.Errorf("ERR704: environment variables not set: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Hash identifies a configuration: two files with the same content once
// decoded have the same hash.
func Hash(cfg *Config) string {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// errs collects validation failures so the user sees them all at once.
type errs []error

func (e *errs) add(err error) { *e = append(*e, err) }
func (e errs) join() error    { return errors.Join(e...) }
