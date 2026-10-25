// Package config loads and validates process configuration from environment
// variables prefixed with CA_ (12-factor, see docs/07-deployment.md §8 for the
// authoritative variable reference).
//
// Loading is a pure function of a lookup callback so tests never touch the real
// environment. A deployment profile (BU_PROFILE) supplies safe defaults; any
// explicitly set variable overrides the profile default.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Profile is a named bundle of defaults for one deployment shape.
type Profile string

// Supported deployment profiles (docs/07-deployment.md §1).
const (
	ProfileLocal              Profile = "local"
	ProfileLocalFull          Profile = "local-full"
	ProfileCloudFree          Profile = "cloud-free"
	ProfileCloudFreeContainer Profile = "cloud-free-container"
)

// Mode selects the tenancy model.
type Mode string

const (
	// ModeLocal runs with a single implicit tenant and user (FR-ADM-001).
	ModeLocal Mode = "local"
	// ModeHosted runs multi-tenant with authentication (FR-ADM-002, MT-001).
	ModeHosted Mode = "hosted"
)

// DefaultPort is the port used when neither BU_HTTP_ADDR nor PORT is set.
// 7420 is unusual enough to avoid clashing with common dev servers.
const DefaultPort = "7420"

// Config is the fully resolved, validated configuration.
type Config struct {
	Profile Profile
	Mode    Mode
	// Env is the deployment environment name (local, staging, production),
	// reported as the telemetry attribute deployment.environment.name.
	Env string

	// HTTPAddr is the listen address for `betweenus serve`.
	HTTPAddr string
	// PublicBaseURL is the externally reachable URL used in links.
	PublicBaseURL string

	LogLevel  slog.Level
	LogFormat LogFormat

	// DataDir holds local state: the SQLite database and, if enabled, kept
	// exports. Created with 0700 permissions by EnsureDataDir.
	DataDir string

	DB DBConfig
	// MigrateOnStart applies migrations when `serve` starts. Local default;
	// hosted deployments migrate from CI instead (docs/07-deployment.md §10).
	MigrateOnStart bool
}

// DBConfig holds database connection settings. The store itself arrives in M1;
// the settings are validated now so misconfiguration fails fast.
type DBConfig struct {
	Driver   string // "sqlite" or "postgres"
	DSN      string
	MaxConns int
}

// LogFormat selects the slog handler.
type LogFormat string

// Supported log formats.
const (
	LogText LogFormat = "text"
	LogJSON LogFormat = "json"
)

// IsHosted reports whether the process serves multiple tenants.
func (c Config) IsHosted() bool { return c.Mode == ModeHosted }

// Lookup mirrors os.LookupEnv so callers can inject a fake environment.
type Lookup func(key string) (string, bool)

// Load resolves configuration from lookup and validates it. All problems are
// reported together (errors.Join) so an operator can fix them in one pass.
func Load(lookup Lookup) (Config, error) {
	get := func(key string) string {
		v, _ := lookup(key)
		return strings.TrimSpace(v)
	}

	profile := Profile(orDefault(get("BU_PROFILE"), string(ProfileLocal)))
	d, ok := profileDefaults[profile]
	if !ok {
		return Config{}, fmt.Errorf("BU_PROFILE=%q: must be one of local, local-full, cloud-free, cloud-free-container", profile)
	}

	var errs []error
	cfg := Config{
		Profile:       profile,
		Mode:          Mode(orDefault(get("BU_MODE"), string(d.mode))),
		Env:           orDefault(get("BU_ENV"), d.env),
		HTTPAddr:      resolveAddr(get("BU_HTTP_ADDR"), get("PORT"), d.host),
		PublicBaseURL: get("BU_PUBLIC_BASE_URL"),
		LogFormat:     LogFormat(orDefault(get("BU_LOG_FORMAT"), string(d.logFormat))),
		DB: DBConfig{
			Driver: orDefault(get("BU_DB_DRIVER"), d.dbDriver),
			DSN:    orDefault(get("BU_DB_DSN"), d.dbDSN),
		},
	}

	cfg.DataDir = get("BU_DATA_DIR")
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	// The local SQLite file lives in the data directory unless a DSN is given.
	if cfg.DB.Driver == "sqlite" && cfg.DB.DSN == "" {
		cfg.DB.DSN = "file:" + filepath.Join(cfg.DataDir, "betweenus.db")
	}

	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = "http://" + loopbackFor(cfg.HTTPAddr)
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(orDefault(get("BU_LOG_LEVEL"), "info"))); err != nil {
		errs = append(errs, fmt.Errorf("BU_LOG_LEVEL: %w", err))
	}

	cfg.MigrateOnStart = cfg.Mode == ModeLocal
	if v := get("BU_MIGRATE_ON_START"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("BU_MIGRATE_ON_START=%q: must be true or false", v))
		}
		cfg.MigrateOnStart = b
	}

	cfg.DB.MaxConns = d.dbMaxConns
	if v := get("BU_DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("BU_DB_MAX_CONNS=%q: must be a positive integer", v))
		}
		cfg.DB.MaxConns = n
	}

	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// validate checks cross-field rules. It returns every violation it finds.
func (c Config) validate() []error {
	var errs []error
	if c.Mode != ModeLocal && c.Mode != ModeHosted {
		errs = append(errs, fmt.Errorf("BU_MODE=%q: must be local or hosted", c.Mode))
	}
	if c.LogFormat != LogText && c.LogFormat != LogJSON {
		errs = append(errs, fmt.Errorf("BU_LOG_FORMAT=%q: must be text or json", c.LogFormat))
	}
	if c.DB.Driver != "sqlite" && c.DB.Driver != "postgres" {
		errs = append(errs, fmt.Errorf("BU_DB_DRIVER=%q: must be sqlite or postgres", c.DB.Driver))
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("BU_HTTP_ADDR=%q: %w", c.HTTPAddr, err))
	}
	if u, err := url.Parse(c.PublicBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("BU_PUBLIC_BASE_URL=%q: must be an absolute http(s) URL", c.PublicBaseURL))
	}
	// Cloud profiles have ephemeral filesystems, so a SQLite file would be lost
	// on every deploy or scale-to-zero (docs/07-deployment.md §5.1).
	if (c.Profile == ProfileCloudFree || c.Profile == ProfileCloudFreeContainer) && c.DB.Driver != "postgres" {
		errs = append(errs, fmt.Errorf("BU_DB_DRIVER=%q: profile %s requires postgres", c.DB.Driver, c.Profile))
	}
	if c.DB.Driver == "postgres" && c.DB.DSN == "" {
		errs = append(errs, errors.New("BU_DB_DSN: required when BU_DB_DRIVER=postgres"))
	}
	return errs
}

// defaults are the per-profile baseline values.
type defaults struct {
	mode       Mode
	env        string
	host       string // listen host; port comes from PORT/DefaultPort
	logFormat  LogFormat
	dbDriver   string
	dbDSN      string
	dbMaxConns int
}

var profileDefaults = map[Profile]defaults{
	// Single binary on a laptop: loopback only, human-readable logs, SQLite.
	ProfileLocal: {ModeLocal, "local", "127.0.0.1", LogText, "sqlite", "", 1},
	// Docker Compose: bind all interfaces inside the container, Postgres service.
	ProfileLocalFull: {ModeHosted, "local", "0.0.0.0", LogJSON, "postgres", "", 10},
	// Serverless functions: tiny pools because every instance has its own.
	ProfileCloudFree: {ModeHosted, "production", "0.0.0.0", LogJSON, "postgres", "", 5},
	// Free container host (Render/Koyeb): one long-lived process.
	ProfileCloudFreeContainer: {ModeHosted, "production", "0.0.0.0", LogJSON, "postgres", "", 10},
}

// resolveAddr applies the precedence BU_HTTP_ADDR > PORT > profile default.
// PORT is set by Vercel, Render and Koyeb and only replaces the port.
func resolveAddr(explicit, port, host string) string {
	if explicit != "" {
		return explicit
	}
	return net.JoinHostPort(host, orDefault(port, DefaultPort))
}

// loopbackFor turns a wildcard listen address into one a browser can open.
func loopbackFor(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // validate() reports the real problem
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// defaultDataDir follows each OS's convention for per-user application data:
// ~/Library/Application Support on macOS, %AppData% on Windows, and
// $XDG_DATA_HOME (default ~/.local/share) on Linux and other Unixes.
func defaultDataDir() string {
	const app = "betweenus"
	switch runtime.GOOS {
	case "darwin", "windows":
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, app)
		}
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, app)
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", app)
		}
	}
	return app // last resort: relative to the working directory
}

// EnsureDataDir creates the data directory, readable only by the current
// user because it holds private chat data.
func (c Config) EnsureDataDir() error {
	return os.MkdirAll(c.DataDir, 0o700)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
