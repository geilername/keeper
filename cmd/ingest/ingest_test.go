package main

import (
	"testing"
	"time"
)

func TestIncomingPath(t *testing.T) {
	t.Setenv("SPOOL_DIR", "/spool")
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"/spool/incoming/camera/DSC0001.JPG", true},
		{"/spool/incoming/../photos/x.jpg", false},
		{"/spool/incoming", false},
		{"/etc/passwd", false},
		{"", false},
	} {
		_, err := incomingPath(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("incomingPath(%q) err=%v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestSubSec(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"3": 300 * time.Millisecond, "35": 350 * time.Millisecond,
		"357": 357 * time.Millisecond, "": 0, "x": 0, "357\x00": 357 * time.Millisecond,
	} {
		if got := subSec(in); got != want {
			t.Errorf("subSec(%q) = %v, want %v", in, got, want)
		}
	}
}
