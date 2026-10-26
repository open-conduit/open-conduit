package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShares(t *testing.T) {
	s := Sample{CPUs: 4, Load: [3]float64{6, 1, 1}, HasLoad: true, MemTotal: 1000, MemAvailable: 250}
	if got := s.LoadPerCPU(); got != 1.5 {
		t.Errorf("load per CPU %v, want 1.5", got)
	}
	if got := s.MemoryUsedPct(); got != 75 {
		t.Errorf("memory %v%%, want 75", got)
	}
	if got := (Disk{Total: 200, Free: 20}).UsedPct(); got != 90 {
		t.Errorf("disk %v%%, want 90", got)
	}
	if got := (Sample{}).LoadPerCPU(); got != 0 {
		t.Errorf("no load read gave %v", got)
	}
	if got := (Sample{MemTotal: 10, MemAvailable: 20}).MemoryUsedPct(); got != 0 {
		t.Errorf("more available than total gave %v%%", got)
	}
}

func TestReadMeasuresAMissingPathAtItsParentOnce(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not", "yet")
	s, err := Read([]string{dir, missing})
	if err == ErrUnsupported {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Disks) != 1 || s.Disks[0].Path != dir {
		t.Fatalf("disks %+v, want just %s: both are on one filesystem", s.Disks, dir)
	}
	if d := s.Disks[0]; d.Total == 0 || d.Free > d.Total {
		t.Errorf("disk %+v", d)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("reading created the path")
	}
}
