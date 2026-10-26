package host

import (
	"bufio"
	"strings"
	"testing"
)

func TestParseLoadAvg(t *testing.T) {
	got, err := parseLoadAvg("0.52 1.25 2.00 3/812 12345\n")
	if err != nil || got != [3]float64{0.52, 1.25, 2} {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseLoadAvg("0.52"); err == nil {
		t.Error("a short line parsed")
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo(bufio.NewScanner(strings.NewReader("MemTotal:       16000 kB\nMemFree:  1000 kB\nMemAvailable:   4000 kB\nCached: 2000 kB\n")))
	if err != nil || total != 16000*1024 || avail != 4000*1024 {
		t.Fatalf("got %d %d %v", total, avail, err)
	}
	// Kernels before 3.14 have no MemAvailable.
	_, avail, _ = parseMeminfo(bufio.NewScanner(strings.NewReader("MemTotal: 16000 kB\nMemFree: 1000 kB\nBuffers: 500 kB\nCached: 2000 kB\n")))
	if avail != 3500*1024 {
		t.Errorf("without MemAvailable got %d", avail)
	}
	if _, _, err := parseMeminfo(bufio.NewScanner(strings.NewReader("nothing\n"))); err == nil {
		t.Error("no MemTotal parsed")
	}
}
