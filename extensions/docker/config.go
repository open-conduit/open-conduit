package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/open-conduit/open-conduit/extension"
)

// Settings are the provider's own part of a binding's container config
// (ContainerConfig.Provider): the Docker context every call uses.
type Settings struct {
	// Context names the Docker CLI context (`docker context ls`), so the
	// daemon a binding runs on never changes with the CLI's current one.
	Context string `json:"context"`
}

var contextPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]*$`)

// ParseConfig strictly reads a binding's container config and the
// provider's settings in it.
func ParseConfig(raw json.RawMessage) (extension.ContainerConfig, Settings, error) {
	cfg, err := extension.ParseContainerConfig(raw)
	if err != nil {
		return extension.ContainerConfig{}, Settings{}, err
	}
	var s Settings
	if len(bytes.TrimSpace(cfg.Provider)) == 0 {
		return extension.ContainerConfig{}, Settings{}, errors.New(`the docker provider needs "provider": {"context": "<docker context>"}`)
	}
	dec := json.NewDecoder(bytes.NewReader(cfg.Provider))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return extension.ContainerConfig{}, Settings{}, errors.New(`the docker provider's settings are {"context": "<docker context>"}`)
	}
	if !contextPattern.MatchString(s.Context) {
		return extension.ContainerConfig{}, Settings{}, fmt.Errorf("docker context %q is not a context name", s.Context)
	}
	return cfg, s, nil
}

// ValidateConfig is ParseConfig for configuration validation.
func ValidateConfig(raw json.RawMessage) error {
	_, _, err := ParseConfig(raw)
	return err
}
