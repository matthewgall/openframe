package wifi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func sysTree(t *testing.T, ifaces ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "net")
	for _, ifc := range ifaces {
		d := filepath.Join(root, ifc)
		if err := os.MkdirAll(filepath.Join(d, "wireless"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "operstate"), []byte("up\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// wiredIface adds a non-wireless interface.
func addWired(t *testing.T, root, ifc, state string) {
	t.Helper()
	d := filepath.Join(root, ifc)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "operstate"), []byte(state+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindWifiIfAndWiredUp(t *testing.T) {
	root := sysTree(t, "wlan0", "wlan1")
	ifname, ok := findWifiIf(root)
	if !ok || ifname != "wlan0" {
		t.Fatalf("findWifiIf = %q, %v; want wlan0", ifname, ok)
	}
	if wiredUp(root) {
		t.Fatal("wiredUp with only wireless interfaces")
	}
	addWired(t, root, "eth0", "up")
	if !wiredUp(root) {
		t.Fatal("wiredUp with eth0 up")
	}
	addWired(t, root, "eth1", "down")
	if _, ok := findWifiIf("/nonexistent/class/net"); ok {
		t.Fatal("findWifiIf must fail for a missing root")
	}
}

func TestDetectWithFakeTools(t *testing.T) {
	root := sysTree(t, "wlan0")

	// No tools at all: limited backend.
	none := func(string) (string, error) { return "", os.ErrNotExist }
	b := detect(root, none)
	info, _ := b.Info(context.Background())
	if info.Available != true || info.Backend != "none" {
		t.Fatalf("limited: %+v", info)
	}
	if _, err := b.Scan(context.Background()); err == nil {
		t.Fatal("limited Scan must explain the missing tooling")
	}

	// No wireless interface: unavailable.
	empty := sysTree(t) // no interfaces at all
	b = detect(empty, none)
	info, _ = b.Info(context.Background())
	if info.Available {
		t.Fatalf("expected unavailable, got %+v", info)
	}
	if _, err := b.Scan(context.Background()); err == nil {
		t.Fatal("unavailable Scan must error")
	}
}

func TestDbmToPercent(t *testing.T) {
	for dbm, want := range map[int]int{-30: 100, -60: 50, -90: 0, -100: 0, -20: 100} {
		if got := dbmToPercent(dbm); got != want {
			t.Errorf("dbmToPercent(%d) = %d, want %d", dbm, got, want)
		}
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	for _, s := range []string{
		"plain",
		`with "quotes"`,
		`back\slash`,
		`both " and \ together`,
	} {
		if got := unescapeWPAStr(escapeWPAStr(s)); got != s {
			t.Errorf("round trip %q = %q", s, got)
		}
	}
}

func TestSavedNetworksRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &supplicantBackend{
		ifname:   "wlan0",
		root:     sysTree(t, "wlan0"),
		confPath: filepath.Join(dir, "openframe.conf"),
	}

	// Save two networks; the newest must land first (preferred).
	if err := s.saveNetwork("Old", "old-psk"); err != nil {
		t.Fatal(err)
	}
	if err := s.saveNetwork("New", "new-psk"); err != nil {
		t.Fatal(err)
	}
	nets, err := s.loadNetworks()
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 || nets[0].SSID != "New" || nets[1].SSID != "Old" {
		t.Fatalf("saved order: %+v", nets)
	}
	// File must be 0600 (passphrases inside).
	st, err := os.Stat(s.confPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("conf perms = %o, want 600", perm)
	}

	// Update in place: same order, new PSK.
	if err := s.saveNetwork("Old", "changed"); err != nil {
		t.Fatal(err)
	}
	nets, _ = s.loadNetworks()
	if nets[1].PSK != "changed" || len(nets) != 2 {
		t.Fatalf("update: %+v", nets)
	}

	// Forget removes it.
	if err := s.Forget(context.Background(), "Old"); err != nil {
		t.Fatal(err)
	}
	nets, _ = s.loadNetworks()
	if len(nets) != 1 || nets[0].SSID != "New" {
		t.Fatalf("after forget: %+v", nets)
	}

	// Open network: no psk line.
	if err := s.saveNetwork("CoffeeShop", ""); err != nil {
		t.Fatal(err)
	}
	nets, _ = s.loadNetworks()
	if len(nets) != 2 || nets[0].SSID != "CoffeeShop" || nets[0].PSK != "" {
		t.Fatalf("open network: %+v", nets)
	}
}

func TestParseIWScan(t *testing.T) {
	const fixture = `
BSSID 00:11:22:33:44:55
	SSID: HomeNet
	freq 2437
	signal: -52.00 dBm
	WPA:
	 * WPA2 (PSK)
RSN:
	 * (auth 0x0)
BSSID 66:77:88:99:aa:bb
	SSID: CoffeeShop
	freq 5180
	signal: -68.00 dBm
BSSID cc:cc:cc:cc:cc:cc
	SSID: HomeNet
	freq 5180
	signal: -40.00 dBm
	WPA:
	 * WPA2 (PSK)
`
	nets := parseIWScan(fixture)
	if len(nets) != 2 {
		t.Fatalf("nets = %+v, want 2 (HomeNet deduped)", nets)
	}
	bySSID := map[string]Network{}
	for _, n := range nets {
		bySSID[n.SSID] = n
	}
	home := bySSID["HomeNet"]
	if !home.Encrypted || home.Signal != dbmToPercent(-40) {
		t.Fatalf("HomeNet = %+v, want encrypted + strongest (-40)", home)
	}
	cafe := bySSID["CoffeeShop"]
	if cafe.Encrypted {
		t.Fatalf("CoffeeShop = %+v, want open", cafe)
	}
}

func TestParseStatus(t *testing.T) {
	out := "state: ASSOCIATED\nwpa_state:COMPLETED\nssid:HomeNet\nid:1\n"
	if wifiState(out) != "COMPLETED" {
		t.Fatalf("wifiState = %q", wifiState(out))
	}
	if statusSSID(out) != "HomeNet" {
		t.Fatalf("statusSSID = %q", statusSSID(out))
	}
}

func TestSupplicantProcessesFakeProc(t *testing.T) {
	proc := t.TempDir()
	mk := func(pid string, cmdline string) {
		d := filepath.Join(proc, pid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		// argv is NUL-separated; strings.Fields splits on any
		// whitespace, which includes NUL.
		if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("1", "init")
	mk("100", "/usr/sbin/wpa_supplicant\x00-B\x00-i\x00wlan0\x00-c\x00/etc/wpa_supplicant/openframe.conf")
	mk("200", "/usr/sbin/wpa_supplicant\x00-i\x00wlan1\x00-c\x00/etc/other.conf")

	s := &supplicantBackend{ifname: "wlan0", procRoot: proc,
		confPath: "/etc/wpa_supplicant/openframe.conf"}
	pid, ours := s.findOurSupplicant()
	if !ours || pid != 100 {
		t.Fatalf("findOurSupplicant = %d, %v", pid, ours)
	}
	other := s.findAnySupplicant()
	if other != 100 && other != 200 {
		t.Fatalf("findAnySupplicant = %d", other)
	}

	// A supplicant on our interface but NOT ours must be detected as
	// foreign.
	os.Remove(filepath.Join(proc, "100", "cmdline"))
	_ = os.WriteFile(filepath.Join(proc, "100", "cmdline"),
		[]byte("/usr/sbin/wpa_supplicant\x00-B\x00-i\x00wlan0\x00-c\x00/etc/other.conf"), 0o644)
	if _, ours := s.findOurSupplicant(); ours {
		t.Fatal("foreign config must not be detected as ours")
	}
}

func TestParseNmcliScan(t *testing.T) {
	const fixture = "HomeNet:82:WPA2\nCoffeeShop:55:\n:70:WPA2\nHidden:60:WPA2\n"
	nets := parseNmcliScan(fixture)
	if len(nets) != 3 {
		t.Fatalf("nets = %+v, want 3 (hidden skipped)", nets)
	}
	if nets[0].SSID != "HomeNet" || nets[0].Signal != 82 || !nets[0].Encrypted {
		t.Fatalf("nets[0] = %+v", nets[0])
	}
	if nets[1].Encrypted {
		t.Fatalf("CoffeeShop should be open: %+v", nets[1])
	}
}

func TestNmcliActiveSSID(t *testing.T) {
	if got := nmcliActiveSSID("no:\nyes:HomeNet\nno:"); got != "HomeNet" {
		t.Fatalf("nmcliActiveSSID = %q", got)
	}
	if got := nmcliActiveSSID("no:\nno:"); got != "" {
		t.Fatalf("expected no active, got %q", got)
	}
}
