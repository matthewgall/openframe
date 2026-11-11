package config

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the prefix for all configuration environment variables.
const EnvPrefix = "OPENFRAME_"

// envKey maps a YAML path to its environment variable name:
// data_dir -> OPENFRAME_DATA_DIR, frame.sleep.tz -> OPENFRAME_FRAME_SLEEP_TZ.
func envKey(parts ...string) string {
	j := strings.Join(parts, "_")
	return EnvPrefix + strings.ToUpper(j)
}

// applyEnv walks the config struct and overrides fields from environment
// variables. Only fields that are actually present in the environment are
// touched; unset variables leave defaults/file values alone.
func applyEnv(cfg *Config) error {
	v := reflect.ValueOf(cfg).Elem()
	return bindEnv(v, []string{})
}

func bindEnv(v reflect.Value, path []string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		fieldPath := append(append([]string{}, path...), name)

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			// time.Duration is handled below; other structs recurse.
			if fv.Type() == reflect.TypeOf(time.Duration(0)) {
				applyScalar(fv, fieldPath)
				continue
			}
			if err := bindEnv(fv, fieldPath); err != nil {
				return err
			}
		case reflect.String, reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64:
			applyScalar(fv, fieldPath)
		case reflect.Slice:
			if fv.Type().Elem().Kind() == reflect.String {
				applyStringSlice(fv, fieldPath)
			}
		}
	}
	return nil
}

func applyScalar(fv reflect.Value, path []string) {
	raw, ok := os.LookupEnv(envKey(path...))
	if !ok {
		return
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		if b, err := strconv.ParseBool(raw); err == nil {
			fv.SetBool(b)
		}
	case reflect.Int:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			fv.SetInt(n)
		}
	case reflect.Int64:
		switch fv.Type() {
		case reflect.TypeOf(time.Duration(0)):
			if d, err := time.ParseDuration(raw); err == nil {
				fv.SetInt(int64(d))
			}
		default:
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				fv.SetInt(n)
			}
		}
	case reflect.Float64:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			fv.SetFloat(f)
		}
	}
}

func applyStringSlice(fv reflect.Value, path []string) {
	raw, ok := os.LookupEnv(envKey(path...))
	if !ok {
		return
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	fv.Set(reflect.ValueOf(out))
}
