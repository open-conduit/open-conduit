package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

type recordingDestination struct {
	mu     sync.Mutex
	stored map[string]string
	ended  chan error
}

func (d *recordingDestination) Put(_ context.Context, _ extension.BackupTarget, bundle string, f extension.BackupFile, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		d.ended <- err
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stored[bundle+"/"+f.Path] = string(data)
	d.ended <- nil
	return nil
}
func (d *recordingDestination) List(context.Context, extension.BackupTarget) ([]string, error) {
	return []string{"conduit-1"}, nil
}
func (d *recordingDestination) Files(context.Context, extension.BackupTarget, string) ([]extension.BackupFile, error) {
	return []extension.BackupFile{{Path: "conduit.db", Size: 6}}, nil
}
func (d *recordingDestination) Read(_ context.Context, _ extension.BackupTarget, _, _ string, offset int64, length int) ([]byte, bool, error) {
	return []byte("x"), offset+int64(length) >= 1, nil
}

func backupHost(t *testing.T) (*sdktest.Host, *recordingDestination) {
	t.Helper()
	d := &recordingDestination{stored: map[string]string{}, ended: make(chan error, 4)}
	ext := sdk.New("store", sdk.Version("1.0.0"), sdk.Command("store"))
	ext.Manifest.Provides.BackupDestinations = []protocol.BackupDestinationDecl{{Type: "store-bucket"}}
	ext.BackupDestinations = map[string]extension.BackupDestination{"store-bucket": d}
	return sdktest.New(t, ext), d
}

func write(h *sdktest.Host, upload string, size, offset int64, chunk string, final bool) error {
	return h.Call(context.Background(), "backup_destination.write", protocol.BackupWriteParams{
		Target: protocol.BackupTargetParams{Type: "store-bucket", Name: "offsite", Config: protocol.Payload{Schema: extension.BackupDestinationConfigSchema, Data: json.RawMessage(`{}`)}},
		Bundle: "conduit-1", Path: "conduit.db", Size: size, Upload: upload, Offset: offset, Chunk: []byte(chunk), Final: final}, nil)
}

func TestBackupDestinationCarriesAFileInChunks(t *testing.T) {
	h, d := backupHost(t)
	if err := write(h, "u1", 6, 0, "abc", false); err != nil {
		t.Fatal(err)
	}
	if err := write(h, "u1", 6, 3, "def", true); err != nil {
		t.Fatal(err)
	}
	if got := d.stored["conduit-1/conduit.db"]; got != "abcdef" {
		t.Fatalf("stored %q", got)
	}
	var files protocol.BackupFilesResult
	if err := h.Call(context.Background(), "backup_destination.files", protocol.BackupFilesParams{Target: protocol.BackupTargetParams{Type: "store-bucket"}, Bundle: "conduit-1"}, &files); err != nil || len(files.Files) != 1 || files.Files[0].Size != 6 {
		t.Fatalf("files = %+v, %v", files, err)
	}
}

// An upload that goes wrong reaches the Put as a failed read, so it can't
// store a partial file as though it were whole.
func TestBackupDestinationNeverCompletesAPartialFile(t *testing.T) {
	for name, send := range map[string]func(*sdktest.Host) error{
		"abort": func(h *sdktest.Host) error {
			return h.Call(context.Background(), "backup_destination.abort", protocol.BackupAbortParams{Upload: "u1"}, nil)
		},
		"a gap":          func(h *sdktest.Host) error { _ = write(h, "u1", 6, 5, "z", true); return nil },
		"past its size":  func(h *sdktest.Host) error { _ = write(h, "u1", 6, 3, "defghij", true); return nil },
		"ended too soon": func(h *sdktest.Host) error { _ = write(h, "u1", 6, 3, "d", true); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, d := backupHost(t)
			if err := write(h, "u1", 6, 0, "abc", false); err != nil {
				t.Fatal(err)
			}
			if err := send(h); err != nil {
				t.Fatal(err)
			}
			if err := <-d.ended; err == nil {
				t.Fatal("the Put read to a clean end")
			}
			if len(d.stored) != 0 {
				t.Fatalf("stored %v", d.stored)
			}
		})
	}
}

func TestBackupDestinationRefusesAnUnknownUploadAndType(t *testing.T) {
	h, _ := backupHost(t)
	if err := write(h, "never-started", 6, 3, "def", true); err == nil || !strings.Contains(err.Error(), "no backup upload") {
		t.Fatalf("err = %v", err)
	}
	err := h.Call(context.Background(), "backup_destination.list", protocol.BackupListParams{Target: protocol.BackupTargetParams{Type: "other"}}, nil)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
