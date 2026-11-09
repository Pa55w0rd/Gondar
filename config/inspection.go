package config

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

// InspectionPostgresDSN follows the normal DSN precedence without modifying the
// process environment or reading an unbounded/non-regular configuration file.
// Diagnostics handle missing/invalid configuration separately and never print it.
func InspectionPostgresDSN(filename string) string {
	if dsn := strings.TrimSpace(os.Getenv("GONDAR_PG_DSN")); dsn != "" {
		return dsn
	}
	st, err := os.Stat(filename)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return ""
	}
	f, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return ""
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	d := cfg.Database
	if dsn := strings.TrimSpace(d.DSN); dsn != "" {
		return dsn
	}
	if d.Host != "" || d.DBName != "" || d.User != "" {
		return d.buildDSN()
	}
	return ""
}
