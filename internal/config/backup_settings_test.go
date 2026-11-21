package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const validTrustKey = "ed25519:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func backupDoc(body string) []byte {
	return fmt.Appendf(nil, `{"version":1,"settings":{"backup":%s},"connections":[],"scopes":[{"id":"org","name":"org"}]}`, body)
}

func TestBackupSettings(t *testing.T) {
	doc, err := Parse(backupDoc(fmt.Sprintf(`{"signing_key":"env:BACKUP_KEY","trust_keys":[%q]}`, validTrustKey)))
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Settings.Backup; b.SigningKey != "env:BACKUP_KEY" || !slices.Equal(b.TrustKeys, []string{validTrustKey}) {
		t.Fatalf("settings = %+v", b)
	}
	for name, body := range map[string]string{
		"a literal signing key": `{"signing_key":"ed25519-private:abc"}`,
		"a malformed trust key": `{"trust_keys":["ed25519:short"]}`,
		"an unknown field":      `{"signing":"env:X"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(backupDoc(body)); err == nil || !strings.Contains(err.Error(), "settings/backup") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
