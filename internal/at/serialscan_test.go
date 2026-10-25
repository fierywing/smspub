package at

import (
	"os"
	"path/filepath"
	"testing"
)

func TestML307ANetworkAndDiagnosticAreNeverATProbeCandidates(t *testing.T) {
	root := t.TempDir()
	usb := filepath.Join(root, "usb")
	if err := os.MkdirAll(usb, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"idVendor": "2ecc", "idProduct": "3012"} {
		if err := os.WriteFile(filepath.Join(usb, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		number, class string
		skip          bool
	}{
		{"00", "02", true}, {"01", "0a", true}, {"02", "ff", false}, {"03", "ff", false}, {"04", "ff", true},
	} {
		t.Run(tc.number, func(t *testing.T) {
			dir := filepath.Join(usb, "if"+tc.number)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"bInterfaceNumber": tc.number, "bInterfaceClass": tc.class} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tty := filepath.Join(root, "ttyUSB"+tc.number)
			if err := os.WriteFile(tty, nil, 0600); err != nil {
				t.Fatal(err)
			}
			class := filepath.Join(root, "class", filepath.Base(tty))
			if err := os.MkdirAll(class, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir, filepath.Join(class, "device")); err != nil {
				t.Fatal(err)
			}
			if got := dataInterface(tty, filepath.Join(root, "class")); got != tc.skip {
				t.Fatalf("interface %s skip=%v, want %v", tc.number, got, tc.skip)
			}
		})
	}
}
