package srt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Policy is the operator's policy, the binding's sandbox config. It is
// exactly what a sandboxed process is granted: an executor's or a
// resource's needs are checked against it and never widen it. Paths may
// start with ~ (the home directory) or a placeholder for this attempt's
// paths: {workspace}, {run} or {toolbelt}.
type Policy struct {
	// Write are the paths the process may write (and read). The attempt's
	// own run directory is always writable.
	Write []string `json:"write,omitempty"`
	Read  struct {
		// Deny are paths the process may not read, besides the provider's
		// default denies (credentials, and on Linux the escape sockets).
		// Everything else is readable, as srt reads broadly by default.
		Deny []string `json:"deny,omitempty"`
		// Allow are exceptions beneath Deny; one naming a default deny
		// exactly lifts it.
		Allow []string `json:"allow,omitempty"`
	} `json:"read"`
	Network struct {
		// Allow are the hosts the process may reach, through srt's proxy:
		// a name or *.domain. Everything else is denied.
		Allow []string `json:"allow,omitempty"`
	} `json:"network"`
	// Sockets are the unix sockets the process may connect to; one the
	// provider hides by default is lifted by listing it.
	Sockets []string `json:"sockets,omitempty"`
	// UnixSockets "all" accepts that on Linux, where seccomp can't allow a
	// unix socket by path, any socket the filesystem policy leaves visible
	// is reachable. Without it a socket on Linux can't be enforced.
	UnixSockets string `json:"unix_sockets,omitempty"`
}

var (
	hostPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	errConfig   = errors.New("invalid srt sandbox config")
)

// ParseConfig strictly decodes and checks a binding's sandbox config.
func ParseConfig(raw json.RawMessage) (Policy, error) {
	var p Policy
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return p, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || dec.More() {
		return Policy{}, fmt.Errorf("%w: it must be an object of write, read, network, sockets and unix_sockets", errConfig)
	}
	for _, list := range [][]string{p.Write, p.Read.Deny, p.Read.Allow, p.Sockets} {
		for _, path := range list {
			if err := checkPath(path); err != nil {
				return Policy{}, err
			}
		}
	}
	for _, h := range p.Network.Allow {
		if !hostPattern.MatchString(h) {
			return Policy{}, fmt.Errorf("%w: network.allow takes host names or *.domain", errConfig)
		}
	}
	if p.UnixSockets != "" && p.UnixSockets != "all" {
		return Policy{}, fmt.Errorf(`%w: unix_sockets is "all" or absent`, errConfig)
	}
	return p, nil
}

// ValidateConfig is ParseConfig for configuration validation.
func ValidateConfig(raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

func checkPath(p string) error {
	switch {
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: a path holds a NUL", errConfig)
	case strings.ContainsAny(p, "*?["):
		return fmt.Errorf("%w: paths are literal; patterns aren't supported", errConfig)
	case p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/"):
		return nil
	}
	for _, ph := range []string{"{workspace}", "{run}", "{toolbelt}"} {
		if p == ph || strings.HasPrefix(p, ph+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: a path is absolute, ~/… or starts with {workspace}, {run} or {toolbelt}", errConfig)
}
