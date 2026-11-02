package ssh

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Settings are the provider's own part of a binding's container config
// (ContainerConfig.Provider): the machine to run on and how to reach it.
type Settings struct {
	// Host is the remote's address (a name or an IP address).
	Host string `json:"host"`
	// User is the account the executor runs as there ("" for ssh's own
	// default).
	User string `json:"user,omitempty"`
	// Port is the SSH port (22 when zero).
	Port int `json:"port,omitempty"`
	// HostKey is the remote's public host key, as a known_hosts key:
	// "<type> <base64>". Every connection checks it and no other.
	HostKey string `json:"host_key"`
	// Identity is the private key ssh signs in with (ssh's own choice,
	// its agent or config, when empty). ~ expands.
	Identity string `json:"identity,omitempty"`
	// Network acknowledges what the provider can't hold: the remote's own
	// network. It must be "host": network.allow is checked against the
	// executor's needs, but the remote reaches whatever its own firewall
	// lets it.
	Network string `json:"network"`
}

// NetworkHost is the one Settings.Network: the remote host's own network.
const NetworkHost = "host"

var (
	hostPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
	userPattern = regexp.MustCompile(`^[a-z_][a-z0-9_.-]*$`)
	keyPattern  = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) ([A-Za-z0-9+/]+={0,2})$`)
)

// ParseConfig strictly reads a binding's container config and the
// provider's settings in it. The config's image is the remote's pin:
// "<host>@sha256:<hex>", the SHA-256 of its host key, so a binding names
// the machine it trusts as a container binding names the image it runs.
func ParseConfig(raw json.RawMessage) (extension.ContainerConfig, Settings, error) {
	cfg, err := extension.ParseContainerConfig(raw)
	if err != nil {
		return extension.ContainerConfig{}, Settings{}, err
	}
	const shape = `the ssh provider's settings are {"host": "<host>", "user": "<user>", "port": 22, "host_key": "<type> <base64>", "identity": "<key file>", "network": "host"}`
	if len(bytes.TrimSpace(cfg.Provider)) == 0 {
		return extension.ContainerConfig{}, Settings{}, errors.New(shape)
	}
	var s Settings
	dec := json.NewDecoder(bytes.NewReader(cfg.Provider))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return extension.ContainerConfig{}, Settings{}, errors.New(shape)
	}
	switch {
	case !hostPattern.MatchString(s.Host):
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("ssh host %q is not a host name or address", s.Host)
	case s.User != "" && !userPattern.MatchString(s.User):
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("ssh user %q is not a user name", s.User)
	case s.Port < 0 || s.Port > 65535:
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("ssh port %d is out of range", s.Port)
	case s.Identity != "" && !strings.HasPrefix(s.Identity, "/") && !strings.HasPrefix(s.Identity, "~/"):
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("ssh identity %q must be absolute or under ~", s.Identity)
	case s.Network != NetworkHost:
		return extension.ContainerConfig{}, Settings{}, errors.New(`the ssh provider can't limit the remote's network: set "network": "host" to run with the remote's own (network.allow is still checked against what the executor declares)`)
	case cfg.Limits.CPUs != 0 || cfg.Limits.Memory != "" || cfg.Limits.PIDs != 0:
		return extension.ContainerConfig{}, Settings{}, errors.New("the ssh provider can't enforce limits: leave them out")
	}
	digest, err := HostKeyDigest(s.HostKey)
	if err != nil {
		return extension.ContainerConfig{}, Settings{}, err
	}
	if want := s.Host + "@sha256:" + digest; cfg.Image != want {
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("the image of an ssh binding pins its host key: set it to %q", want)
	}
	if s.Port == 0 {
		s.Port = 22
	}
	return cfg, s, nil
}

// ValidateConfig is ParseConfig for configuration validation.
func ValidateConfig(raw json.RawMessage) error {
	_, _, err := ParseConfig(raw)
	return err
}

// HostKeyDigest is the hex SHA-256 of a known_hosts key's blob ("<type>
// <base64>"), the digest a binding's image pins.
func HostKeyDigest(key string) (string, error) {
	m := keyPattern.FindStringSubmatch(strings.TrimSpace(key))
	if m == nil {
		return "", errors.New(`ssh host_key must be "<type> <base64>", as in known_hosts`)
	}
	blob, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil || len(blob) == 0 {
		return "", errors.New("ssh host_key's key isn't base64")
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// Endpoint names the remote a binding runs on: user@host:port. A
// container's reference is "<endpoint>/<id>".
func (s Settings) Endpoint() string {
	port := s.Port
	if port == 0 {
		port = 22
	}
	return s.User + "@" + s.Host + ":" + strconv.Itoa(port)
}

// Daemon is the remote's identity, as Inspect reports it: the host and its
// pinned key's digest, the binding's image.
func (s Settings) Daemon() string {
	d, _ := HostKeyDigest(s.HostKey)
	return s.Host + "@sha256:" + d
}
