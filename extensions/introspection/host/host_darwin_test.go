package host

import (
	"encoding/binary"
	"testing"
)

func TestParseLoadAvg(t *testing.T) {
	raw := make([]byte, 24)
	for i, v := range []uint32{2048, 1024, 512} {
		binary.LittleEndian.PutUint32(raw[i*4:], v)
	}
	binary.LittleEndian.PutUint64(raw[16:], 2048)
	got, err := parseLoadAvg(raw)
	if err != nil || got != [3]float64{1, 0.5, 0.25} {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseLoadAvg(raw[:12]); err == nil {
		t.Error("a short struct parsed")
	}
}

func TestDecodeUint(t *testing.T) {
	four, eight := make([]byte, 4), make([]byte, 8)
	binary.LittleEndian.PutUint32(four, 7)
	binary.LittleEndian.PutUint64(eight, 1<<40)
	if v, err := decodeUint("x", four); err != nil || v != 7 {
		t.Errorf("4 bytes: %d %v", v, err)
	}
	if v, err := decodeUint("x", eight); err != nil || v != 1<<40 {
		t.Errorf("8 bytes: %d %v", v, err)
	}
	if _, err := decodeUint("x", []byte{1}); err == nil {
		t.Error("1 byte decoded")
	}
}

func TestReadsThisMac(t *testing.T) {
	s, err := Read(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasLoad || s.MemTotal == 0 || s.MemAvailable > s.MemTotal {
		t.Errorf("sample %+v", s)
	}
}
