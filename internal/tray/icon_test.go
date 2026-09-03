package tray

import (
	"bytes"
	"image/png"
	"testing"
)

func TestIconRenders(t *testing.T) {
	for _, size := range []int{22, 32, 36} {
		for _, st := range []State{StateStopped, StateConnecting, StateUp, StateReconnecting, StateFailed} {
			for _, template := range []bool{false, true} {
				img, err := png.Decode(bytes.NewReader(iconAt(size, st, template)))
				if err != nil {
					t.Fatalf("size %d state %d template %v: %v", size, st, template, err)
				}
				if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
					t.Fatalf("size %d state %d: got %v", size, st, b)
				}
			}
		}
		// The idle icon is the bare mark; every other state paints a badge, so
		// they must differ from it and from each other by colour or shape.
		idle := iconAt(size, StateStopped, false)
		if bytes.Equal(idle, iconAt(size, StateUp, false)) || bytes.Equal(iconAt(size, StateUp, false), iconAt(size, StateFailed, false)) {
			t.Fatalf("size %d: badge did not change the icon", size)
		}
		if bytes.Equal(iconAt(size, StateUp, true), iconAt(size, StateReconnecting, true)) {
			t.Fatalf("size %d: template icons must differ by shape", size)
		}
	}
	if len(Icon(StateUp, false)) == 0 {
		t.Fatal("Icon returned nothing for this platform's size")
	}
}
