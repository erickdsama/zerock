package tray

import (
	"bytes"
	"image/png"
	"testing"
)

func TestIconRenders(t *testing.T) {
	for _, st := range []State{StateStopped, StateConnecting, StateUp, StateReconnecting, StateFailed} {
		for _, template := range []bool{false, true} {
			img, err := png.Decode(bytes.NewReader(Icon(st, template)))
			if err != nil {
				t.Fatalf("state %d template %v: %v", st, template, err)
			}
			if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
				t.Fatalf("state %d: size %v", st, b)
			}
		}
	}
	// The idle icon is the bare mark; every other state paints a badge, so
	// they must differ from it and from each other's shape or colour.
	idle := Icon(StateStopped, false)
	if bytes.Equal(idle, Icon(StateUp, false)) || bytes.Equal(Icon(StateUp, false), Icon(StateFailed, false)) {
		t.Fatal("badge did not change the icon")
	}
	if bytes.Equal(Icon(StateUp, true), Icon(StateReconnecting, true)) {
		t.Fatal("template icons must differ by shape")
	}
}
