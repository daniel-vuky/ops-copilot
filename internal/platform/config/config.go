// Package config loads and validates the process configuration from
// environment variables (ADR 0001 §2). It reports every bad variable at once
// and never echoes a raw value in an error.
package config

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	// Embed the IANA time zone database: the distroless image has no
	// /usr/share/zoneinfo, so time.LoadLocation would otherwise fail there.
	_ "time/tzdata"
)

// Default values for the optional variables.
const (
	defaultDisplayTZ  = "Asia/Ho_Chi_Minh"
	defaultListenAddr = ":8080"
	defaultLogLevel   = "info"
)

// hostnamePattern is the character set allowed in a PUBLIC_URL host name:
// lower-case ASCII letters, digits, dots and hyphens, starting and ending
// with a letter or digit. Internationalised names must be given in punycode.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Fixed error reasons. They never contain the raw value, which may hold a
// secret (for example user info in PUBLIC_URL).
const (
	reasonRequired   = "is required"
	reasonPublicURL  = "must be an https origin: https://host[:port] with no path, query, fragment or user info"
	reasonTimeZone   = "unknown time zone"
	reasonLocalZone  = "must name a zone, not Local"
	reasonListenAddr = "must be host:port with a port from 1 to 65535"
	reasonLogLevel   = "must be one of debug, info, warn, error"
)

// Config is the validated process configuration.
type Config struct {
	// PublicURL is the external https origin and OAuth issuer, normalised to
	// "https://" plus the lower-cased host and optional port, with no
	// trailing slash.
	PublicURL string
	// DisplayTZ is the IANA name of the zone times are shown in.
	DisplayTZ string
	// DisplayLocation is DisplayTZ loaded as a location.
	DisplayLocation *time.Location
	// ListenAddr is the host:port the HTTP server binds to.
	ListenAddr string
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
}

// VarError reports one environment variable with a bad value.
type VarError struct {
	// Name is the environment variable name.
	Name string
	// Reason is a fixed phrase saying what is wrong. It never contains the
	// variable's value.
	Reason string
}

// Error returns "<Name>: <Reason>".
func (e *VarError) Error() string {
	return e.Name + ": " + e.Reason
}

// Load reads the configuration through getenv and validates it. On failure
// the error joins one *VarError per bad variable, in the order PUBLIC_URL,
// DISPLAY_TZ, LISTEN_ADDR, LOG_LEVEL.
func Load(getenv func(string) string) (Config, error) {
	var (
		cfg  Config
		errs []error
		err  error
	)

	if cfg.PublicURL, err = parsePublicURL(getenv("PUBLIC_URL")); err != nil {
		errs = append(errs, err)
	}
	if cfg.DisplayTZ, cfg.DisplayLocation, err = parseDisplayTZ(getenv("DISPLAY_TZ")); err != nil {
		errs = append(errs, err)
	}
	if cfg.ListenAddr, err = parseListenAddr(getenv("LISTEN_ADDR")); err != nil {
		errs = append(errs, err)
	}
	if cfg.LogLevel, err = parseLogLevel(getenv("LOG_LEVEL")); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// LogAttrs returns the configuration as log attributes. Config holds no
// secret today; a future secret field must be a Secret.
func (c Config) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("public_url", c.PublicURL),
		slog.String("display_tz", c.DisplayTZ),
		slog.String("listen_addr", c.ListenAddr),
		slog.String("log_level", c.LogLevel.String()),
	}
}

func parsePublicURL(raw string) (string, error) {
	const name = "PUBLIC_URL"
	if raw == "" {
		return "", &VarError{Name: name, Reason: reasonRequired}
	}
	bad := &VarError{Name: name, Reason: reasonPublicURL}

	// url.Parse drops an empty query or fragment ("?" or "#"), so check the
	// raw value first.
	if strings.ContainsAny(raw, "?#") {
		return "", bad
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error quotes the raw value; it is deliberately dropped.
		return "", bad
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" || u.User != nil ||
		u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
		return "", bad
	}
	port := u.Port()
	if port == "" {
		// Port() is also empty for "host:", which is not a valid origin.
		if strings.HasSuffix(u.Host, ":") {
			return "", bad
		}
	} else if !validPort(port) || port[0] == '0' {
		// A leading zero ("0443") would defeat the canonical form below.
		return "", bad
	}
	if !validHost(u) {
		return "", bad
	}

	host := strings.ToLower(u.Host)
	if port == "443" {
		// 443 is the https default; store the origin without it.
		host = strings.TrimSuffix(host, ":443")
	}
	return "https://" + host, nil
}

// validHost reports whether u's host is a bracketed IPv6 literal with no
// zone, or a name (or IPv4 address) made only of hostnamePattern characters.
func validHost(u *url.URL) bool {
	name := strings.ToLower(u.Hostname())
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(name)
		return err == nil && addr.Is6() && addr.Zone() == ""
	}
	return hostnamePattern.MatchString(name)
}

func parseDisplayTZ(raw string) (string, *time.Location, error) {
	const name = "DISPLAY_TZ"
	if raw == "" {
		raw = defaultDisplayTZ
	}
	if raw == "Local" {
		return "", nil, &VarError{Name: name, Reason: reasonLocalZone}
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return "", nil, &VarError{Name: name, Reason: reasonTimeZone}
	}
	return raw, loc, nil
}

func parseListenAddr(raw string) (string, error) {
	if raw == "" {
		raw = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(raw)
	if err != nil || !validPort(port) {
		return "", &VarError{Name: "LISTEN_ADDR", Reason: reasonListenAddr}
	}
	return raw, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	if raw == "" {
		raw = defaultLogLevel
	}
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, &VarError{Name: "LOG_LEVEL", Reason: reasonLogLevel}
	}
}

// validPort reports whether s is a decimal port from 1 to 65535. Only ASCII
// digits are allowed, so "+80" and service names such as "http" fail.
func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}
