//go:build linux

package voice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesOnlySameModemInterface04(t *testing.T) {
	root := t.TempDir()
	sysroot := filepath.Join(root, "class")
	devroot := filepath.Join(root, "dev")
	for _, path := range []string{sysroot, devroot} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	add := func(usb, iface, tty string) {
		parent := filepath.Join(root, usb)
		inter := filepath.Join(parent, usb+":1."+iface)
		if err := os.MkdirAll(inter, 0700); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"idVendor": "2ecc", "idProduct": "3012", "devnum": "73"} {
			if err := os.WriteFile(filepath.Join(parent, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(inter, "bInterfaceNumber"), []byte(iface), 0600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(sysroot, tty)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(inter, filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(devroot, tty), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	add("9-2", "02", "ttyUSB24")
	add("9-2", "00", "ttyUSB21")
	add("9-3", "04", "ttyUSB7")
	at := filepath.Join(devroot, "ttyUSB24")
	if _, err := resolve(at, sysroot, devroot); err == nil {
		t.Fatal("selected another modem's diagnostic port")
	}
	add("9-2", "04", "ttyUSB22")
	d, err := resolve(at, sysroot, devroot)
	if err != nil || d.Path != filepath.Join(devroot, "ttyUSB22") || d.Generation != "73" {
		t.Fatalf("resolve: %+v %v", d, err)
	}
	if _, err := resolve(filepath.Join(devroot, "ttyUSB21"), sysroot, devroot); err == nil {
		t.Fatal("accepted network interface as AT")
	}
	if err := os.WriteFile(filepath.Join(root, "9-2", "idVendor"), []byte("ffff"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(at, sysroot, devroot); err == nil {
		t.Fatal("accepted another USB product")
	}
}
