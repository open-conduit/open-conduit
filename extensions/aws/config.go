package aws

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Connection secrets: a base key pair. Without them the daemon's own
// environment gives the base identity.
const (
	SecretAccessKeyID     = "access_key_id"
	SecretSecretAccessKey = "secret_access_key"
)

// Role durations. STS issues no session shorter than 15 minutes, and none
// longer than 12 hours (the role's own maximum session duration may be
// shorter; STS refuses a longer ask).
const (
	DefaultDuration = 15 * time.Minute
	MinDuration     = 15 * time.Minute
	MaxDuration     = 12 * time.Hour
)

// Connection is an aws connection's config.
type Connection struct {
	// Region is the STS region, which every profile the run gets uses
	// too.
	Region string `json:"region"`
	// Endpoint replaces the STS endpoint (an emulator such as LocalStack,
	// a VPC endpoint). A run's profiles send every call to it as well.
	Endpoint string `json:"endpoint,omitempty"`
	// Profile is the AWS CLI profile that gives the daemon's ambient
	// identity when the connection has no key pair ("" for the default
	// chain).
	Profile string `json:"profile,omitempty"`
}

// Settings is an aws entry's config.
type Settings struct {
	// Accounts are the AWS account IDs a role may be in.
	Accounts []string `json:"accounts"`
	// Roles are the roles a run may get credentials for.
	Roles []Role `json:"roles"`
}

// Role is one role of an entry.
type Role struct {
	// Name is what a run asks for and its profile's name.
	Name string `json:"name"`
	// ARN is the IAM role's ARN.
	ARN string `json:"arn"`
	// ForBindings are the bindings that may ask for the role.
	ForBindings []string `json:"for_bindings"`
	// Duration is how long its credentials last, a Go duration from 15m
	// to 12h. Default 15m.
	Duration string `json:"duration,omitempty"`
	// TagSession sends the scope, binding and Work as session tags; the
	// role's trust policy must allow sts:TagSession.
	TagSession bool `json:"tag_session,omitempty"`
	// ExternalID is sent as sts:AssumeRole's ExternalId, when the role's
	// trust policy asks for one.
	ExternalID string `json:"external_id,omitempty"`

	duration time.Duration
	account  string
}

// Account is the role's account ID.
func (r Role) Account() string { return r.account }

// Lifetime is the role's credential lifetime.
func (r Role) Lifetime() time.Duration { return r.duration }

var (
	accountRE  = regexp.MustCompile(`^\d{12}$`)
	roleARNRE  = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):role/[\w+=,.@/-]{1,512}$`)
	roleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	regionRE   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	externalRE = regexp.MustCompile(`^[\w+=,.@:/-]+$`)
)

// AccountOf is the account ID in a role ARN, or "" when it isn't one.
func AccountOf(arn string) string {
	m := roleARNRE.FindStringSubmatch(arn)
	if m == nil {
		return ""
	}
	return m[1]
}

// ParseConnection parses and checks a connection's config.
func ParseConnection(raw json.RawMessage) (Connection, error) {
	var c Connection
	if err := strictJSON(raw, &c); err != nil {
		return c, err
	}
	if !regionRE.MatchString(c.Region) {
		return c, errors.New("an aws connection needs its region, such as eu-west-2")
	}
	if c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://") {
		return c, errors.New("endpoint must be an http or https URL")
	}
	return c, nil
}

// ParseSettings parses and checks an entry's config: every role is in an
// allowed account, named once, and lasts from 15 minutes to 12 hours.
func ParseSettings(raw json.RawMessage) (Settings, error) {
	var s Settings
	if err := strictJSON(raw, &s); err != nil {
		return s, err
	}
	if len(s.Accounts) == 0 {
		return s, errors.New("an aws entry needs the accounts its roles may be in")
	}
	for _, a := range s.Accounts {
		if !accountRE.MatchString(a) {
			return s, fmt.Errorf("account %q is not a 12-digit AWS account ID", a)
		}
	}
	if len(s.Roles) == 0 {
		return s, errors.New("an aws entry needs at least one role")
	}
	seen := map[string]bool{}
	for i := range s.Roles {
		r := &s.Roles[i]
		if !roleNameRE.MatchString(r.Name) {
			return s, fmt.Errorf("role name %q must be a lower-case letter then up to 31 lower-case letters, digits, _ or -", r.Name)
		}
		if seen[r.Name] {
			return s, fmt.Errorf("role %q is named twice", r.Name)
		}
		seen[r.Name] = true
		r.account = AccountOf(r.ARN)
		if r.account == "" {
			return s, fmt.Errorf("role %q needs an IAM role ARN", r.Name)
		}
		if !slices.Contains(s.Accounts, r.account) {
			return s, fmt.Errorf("role %q is in account %s, which is not in accounts", r.Name, r.account)
		}
		if len(r.ForBindings) == 0 {
			return s, fmt.Errorf("role %q needs for_bindings, the bindings that may ask for it", r.Name)
		}
		r.duration = DefaultDuration
		if r.Duration != "" {
			d, err := time.ParseDuration(r.Duration)
			if err != nil || d < MinDuration || d > MaxDuration {
				return s, fmt.Errorf("role %q: duration must be from 15m to 12h", r.Name)
			}
			r.duration = d
		}
		if r.ExternalID != "" && (len(r.ExternalID) < 2 || len(r.ExternalID) > 1224 || !externalRE.MatchString(r.ExternalID)) {
			return s, fmt.Errorf("role %q: external_id has characters STS refuses", r.Name)
		}
	}
	return s, nil
}

// Role is the named role, if the entry has it.
func (s Settings) Role(name string) (Role, bool) {
	for _, r := range s.Roles {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}

// RolesFor are the roles a binding may ask for, in config order.
func (s Settings) RolesFor(binding string) []Role {
	var out []Role
	for _, r := range s.Roles {
		if slices.Contains(r.ForBindings, binding) {
			out = append(out, r)
		}
	}
	return out
}

// Bindings are the bindings the entry's roles name, each once, for config
// check to find in the scope.
func (s Settings) Bindings() []string {
	var out []string
	for _, r := range s.Roles {
		for _, b := range r.ForBindings {
			if !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
	}
	return out
}

func strictJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
