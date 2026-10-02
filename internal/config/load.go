// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvPrefix starts every environment override: LLM_SHAPE_<SECTION>_<KEY>.
const EnvPrefix = "LLM_SHAPE_"

// Load reads the YAML file at path over the defaults, applies environment
// overrides, fills derived defaults and validates the result.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data, os.LookupEnv)
}

// Parse is Load without file access; lookup supplies environment variables.
func Parse(data []byte, lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a misspelled key is an error, not a silent default
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := applyEnv(reflect.ValueOf(&cfg).Elem(), EnvPrefix, lookup); err != nil {
		return Config{}, err
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].StreamUsage == "" {
			cfg.Routes[i].StreamUsage = StreamUsagePassthrough
		}
	}
	if cfg.Instance.Name == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Instance.Name = h
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv walks the struct by yaml tags and overrides every leaf whose
// environment variable is set. Values are decoded as YAML, so every field type
// (sizes, durations, lists such as "[a, b]") parses exactly as in the file.
// Lists of structs (routes) are file-only.
func applyEnv(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		key := prefix + strings.ToUpper(tag)
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Struct:
			if err := applyEnv(f, key+"_", lookup); err != nil {
				return err
			}
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Struct:
			continue
		default:
			s, ok := lookup(key)
			if !ok {
				continue
			}
			if err := yaml.Unmarshal([]byte(s), f.Addr().Interface()); err != nil {
				return fmt.Errorf("config: %s: %w", key, err)
			}
		}
	}
	return nil
}
