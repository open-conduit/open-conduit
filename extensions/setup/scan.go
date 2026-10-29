package setup

import (
	"encoding/json"
	"regexp"
)

// secretShapes are the token shapes Open Conduit knows: GitHub, Slack,
// Linear and Anthropic tokens and private-key blocks. Scanning is a
// backstop for mistakes, not the guard: no probe reads a secret and a
// proposal takes only references.
var secretShapes = regexp.MustCompile(`(?:\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bxox[abposr]-[A-Za-z0-9-]{10,}|\bxapp-[A-Za-z0-9-]{10,}|\blin_(?:api|oauth)_[A-Za-z0-9]{20,}|\bsk-ant-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)

// looksLikeSecret reports whether any string in v has a token's shape.
func looksLikeSecret(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	return secretShapes.Match(b)
}

// scanText refuses text the agent wrote that holds a token's shape.
func scanText(texts ...string) error {
	for _, s := range texts {
		if secretShapes.MatchString(s) {
			return refuse("looks_like_secret", "that text looks like it holds a secret; never repeat or propose a secret value, only its reference")
		}
	}
	return nil
}
