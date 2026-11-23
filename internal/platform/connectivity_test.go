package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func mkiface(t *testing.T, root, name, carrier, operstate string) {
	t.Helper()
	d := filepath.Join(root, "class", "net", name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if carrier != "" {
		if err := os.WriteFile(filepath.Join(d, "carrier"), []byte(carrier+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if operstate != "" {
		if err := os.WriteFile(filepath.Join(d, "operstate"), []byte(operstate+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectivityIn(t *testing.T) {
	cases := []struct {
		name  string
		build func(root string)
		want  Link
	}{
		{"empty tree", func(root string) {}, LinkNone},
		{"lo only", func(r string) { mkiface(t, r, "lo", "1", "up") }, LinkNone},
		{"wlan up", func(r string) { mkiface(t, r, "wlan0", "0", "up") }, LinkWiFi},
		{"wlan carrier", func(r string) { mkiface(t, r, "wlan0", "1", "down") }, LinkWiFi},
		{"eth down", func(r string) { mkiface(t, r, "eth0", "0", "down") }, LinkNone},
		{"eth up wins over wlan", func(r string) {
			mkiface(t, r, "wlan0", "1", "up")
			mkiface(t, r, "eth0", "1", "up")
		}, LinkWired},
		{"enp prefix (systemd naming)", func(r string) { mkiface(t, r, "enp3s0", "1", "up") }, LinkWired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			c.build(root)
			if got := ConnectivityIn(root); got != c.want {
				t.Errorf("ConnectivityIn = %v, want %v", got, c.want)
			}
		})
	}
}
