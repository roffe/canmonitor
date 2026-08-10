package main

import (
	"testing"
	"time"
)

func TestParseTx(t *testing.T) {
	m, err := parseTx("0x1A0", "11 22 ff", "100", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.id != 0x1A0 || string(m.data) != "\x11\x22\xff" || m.period != 100*time.Millisecond || m.extended {
		t.Fatalf("got %+v", m)
	}
	if f := m.frame(); f.ID != 0x1A0 || f.Length != 3 || f.Extended {
		t.Fatalf("frame %+v", f)
	}
	if f, _ := parseTx("1FFFFFFF", "", "", true); !f.frame().Extended {
		t.Fatal("extended flag lost")
	}
	for _, bad := range [][3]string{{"zz", "00", "0"}, {"100", "0z", "0"}, {"100", "00112233445566778899", "0"}, {"100", "00", "-5"}} {
		if _, err := parseTx(bad[0], bad[1], bad[2], false); err == nil {
			t.Errorf("parseTx%v: want error", bad)
		}
	}
}
