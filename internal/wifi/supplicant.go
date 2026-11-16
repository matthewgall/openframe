package wifi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// supplicantBackend drives wpa_supplicant directly (Debian without
// NetworkManager). OpenFrame manages its own config file
// (defaultConfPath, 0600) and its own supplicant instance; it refuses
// to touch an interface managed by a different supplicant.
type supplicantBackend struct {
	ifname   string
	root     string // /sys/class/net (test seam)
	confPath string
	procRoot string // /proc (test seam)
}

func (s *supplicantBackend) Info(ctx context.Context) (Info, error) {
	info := Info{
		Available: true,
		Backend:   "wpa_supplicant",
		Interface: s.ifname,
		WiredUp:   wiredUp(s.root),
	}
	out, err := run(ctx, 5*time.Second, "wpa_cli", "-i", s.ifname, "status")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if ssid, ok := strings.CutPrefix(strings.TrimSpace(line), "ssid:"); ok && ssid != "" {
				info.ConnectedSSID = ssid
				break
			}
		}
	}
	return info, nil
}

// Scan uses `iw dev <if> scan dump`, which works whether or not a
// wpa_supplicant is running. (The wpa_cli scan_results output does
// not carry SSIDs, so it is not usable as a fallback.)
func (s *supplicantBackend) Scan(ctx context.Context) ([]Network, error) {
	if _, err := exec.LookPath("iw"); err != nil {
		return nil, fmt.Errorf("wifi scan: iw not installed (apt install iw, or use NetworkManager)")
	}
	out, err := run(ctx, 12*time.Second, "iw", "dev", s.ifname, "scan", "dump")
	if err != nil {
		return nil, fmt.Errorf("wifi scan: %w", err)
	}
	return parseIWScan(out), nil
}

func (s *supplicantBackend) Connect(ctx context.Context, ssid, psk string) error {
	if ssid == "" {
		return fmt.Errorf("wifi: no network selected")
	}
	// 1. Persist the network first (preferred = first in file), so a
	//    crash mid-association still leaves it saved for boot.
	if err := s.saveNetwork(ssid, psk); err != nil {
		return err
	}
	// 2. Make sure OUR supplicant owns the interface.
	if err := s.ensureSupplicant(ctx); err != nil {
		return err
	}
	// 3. Point it at the network: reload our config, then explicitly
	//    select it (RECONFIGURE alone may keep an existing link).
	if out, err := run(ctx, 10*time.Second, "wpa_cli", "-i", s.ifname, "RECONFIGURE", s.confPath); err != nil {
		return fmt.Errorf("wifi connect: %v: %s", err, tail(out))
	}
	if id, ok := s.supplicantNetworkID(ctx, ssid); ok {
		run(ctx, 10*time.Second, "wpa_cli", "-i", s.ifname, "SELECT_NETWORK", id)
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		out, err := run(ctx, 5*time.Second, "wpa_cli", "-i", s.ifname, "status")
		if err == nil {
			st := wifiState(out)
			lastState = st
			if st == "COMPLETED" && statusSSID(out) == ssid {
				return nil
			}
		}
		time.Sleep(1000 * time.Millisecond)
	}
	return fmt.Errorf("wifi connect: timed out (wpa state %q)", lastState)
}

func (s *supplicantBackend) Forget(ctx context.Context, ssid string) error {
	if err := s.removeNetwork(ssid); err != nil {
		return err
	}
	// Drop it from the running instance too (if one is ours).
	if out, err := run(ctx, 10*time.Second, "wpa_cli", "-i", s.ifname, "LIST_NETWORKS"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, ssid) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if _, err := run(ctx, 5*time.Second, "wpa_cli", "-i", s.ifname, "REMOVE_NETWORK", fields[0]); err != nil {
				return fmt.Errorf("wifi forget: %v: %s", err, tail(err.Error()))
			}
			break
		}
	}
	return nil
}

func (s *supplicantBackend) AutoConnect(ctx context.Context) error {
	info, err := s.Info(ctx)
	if err != nil {
		return err
	}
	if info.WiredUp || info.ConnectedSSID != "" {
		return nil
	}
	nets, err := s.loadNetworks()
	if err != nil || len(nets) == 0 {
		return nil // nothing saved: nothing to resume
	}
	return s.ensureSupplicant(ctx) // supplicant connects to the first reachable saved network
}

// --- saved networks (the 0600 config file) --------------------------

type savedNetwork struct {
	SSID string
	PSK  string // "" for open networks
}

func (s *supplicantBackend) loadNetworks() ([]savedNetwork, error) {
	b, err := os.ReadFile(s.confPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wifi config: %w", err)
	}
	var out []savedNetwork
	var cur *savedNetwork
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "network={":
			cur = &savedNetwork{}
		case cur != nil && line == "}":
			if cur.SSID != "" {
				out = append(out, *cur)
			}
			cur = nil
		case cur != nil:
			if v, ok := strings.CutPrefix(line, "ssid="); ok {
				cur.SSID = unescapeWPAStr(v)
			} else if v, ok := strings.CutPrefix(line, "psk="); ok {
				cur.PSK = unescapeWPAStr(v)
			}
		}
	}
	return out, nil
}

// saveNetwork appends or updates ssid at the TOP of the file (the
// supplicant tries networks in file order, so first = preferred).
func (s *supplicantBackend) saveNetwork(ssid, psk string) error {
	nets, err := s.loadNetworks()
	if err != nil {
		return err
	}
	updated := false
	for i := range nets {
		if nets[i].SSID == ssid {
			nets[i].PSK = psk
			updated = true
			break
		}
	}
	if !updated {
		nets = append([]savedNetwork{{SSID: ssid, PSK: psk}}, nets...)
	}
	return s.writeNetworks(nets)
}

func (s *supplicantBackend) removeNetwork(ssid string) error {
	nets, err := s.loadNetworks()
	if err != nil {
		return err
	}
	kept := nets[:0]
	for _, n := range nets {
		if n.SSID != ssid {
			kept = append(kept, n)
		}
	}
	return s.writeNetworks(kept)
}

func (s *supplicantBackend) writeNetworks(nets []savedNetwork) error {
	var b strings.Builder
	b.WriteString("# Managed by OpenFrame (frame Wi-Fi setup).\n")
	b.WriteString("# DO NOT EDIT BY HAND.\n")
	b.WriteString("ctrl_interface=/var/run/wpa_supplicant\n")
	for _, n := range nets {
		// escapeWPAStr returns the fully quoted, escaped value.
		b.WriteString("network={\n")
		b.WriteString("\t" + "ssid=" + escapeWPAStr(n.SSID) + "\n")
		if n.PSK != "" {
			b.WriteString("\t" + "psk=" + escapeWPAStr(n.PSK) + "\n")
		}
		b.WriteString("}\n")
	}
	dir := filepath.Dir(s.confPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("wifi config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".openframe-wifi-*.conf")
	if err != nil {
		return fmt.Errorf("wifi config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("wifi config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("wifi config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wifi config: %w", err)
	}
	if err := os.Rename(tmpName, s.confPath); err != nil {
		return fmt.Errorf("wifi config: %w", err)
	}
	return nil
}

// escapeWPAStr / unescapeWPAStr: the wpa_supplicant config quotes a
// string and escapes backslash and double-quote.
func escapeWPAStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func unescapeWPAStr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '"') {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// --- process management ---------------------------------------------

// ensureSupplicant makes sure OUR wpa_supplicant instance owns the
// interface. It refuses to interfere with one started by something
// else (e.g. a different distribution tool).
func (s *supplicantBackend) ensureSupplicant(ctx context.Context) error {
	_, ours := s.findOurSupplicant()
	otherPid := s.findAnySupplicant()
	switch {
	case ours:
		return nil // already running with our config
	case otherPid > 0:
		return fmt.Errorf("another wpa_supplicant (pid %d) is managing %s; stop it or use NetworkManager", otherPid, s.ifname)
	}
	out, err := run(ctx, 10*time.Second, "wpa_supplicant", "-B", "-i", s.ifname, "-c", s.confPath)
	if err != nil {
		return fmt.Errorf("start wpa_supplicant: %v: %s", err, tail(out))
	}
	// Wait for the control interface.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := run(ctx, 3*time.Second, "wpa_cli", "-i", s.ifname, "status"); err == nil {
			return s.dhcp(ctx)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("start wpa_supplicant: control interface never appeared")
}

// findOurSupplicant scans /proc for a wpa_supplicant started with our
// config file.
func (s *supplicantBackend) findOurSupplicant() (int, bool) {
	for pid, args := range s.supplicantProcesses() {
		if containsArg(args, "-i", s.ifname) && containsArg(args, "-c", s.confPath) {
			return pid, true
		}
	}
	return 0, false
}

// findAnySupplicant returns the pid of ANY wpa_supplicant on our
// interface (0 = none).
// supplicantNetworkID finds the running supplicant's network id for
// an SSID ("" if not present).
func (s *supplicantBackend) supplicantNetworkID(ctx context.Context, ssid string) (string, bool) {
	out, err := run(ctx, 10*time.Second, "wpa_cli", "-i", s.ifname, "LIST_NETWORKS")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ssid {
			return fields[0], true
		}
	}
	return "", false
}

func (s *supplicantBackend) findAnySupplicant() int {
	for pid, args := range s.supplicantProcesses() {
		if containsArg(args, "-i", s.ifname) {
			return pid
		}
	}
	return 0
}

func containsArg(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// dhcp runs the system's DHCP client (dhclient or dhcpcd) for the
// interface, renewing first.
func (s *supplicantBackend) dhcp(ctx context.Context) error {
	clients := []struct{ bin, flags string }{
		{"dhclient", "-r"},
		{"dhcpcd", "-r"},
	}
	for _, c := range clients {
		if _, err := exec.LookPath(c.bin); err != nil {
			continue
		}
		run(ctx, 15*time.Second, c.bin, c.flags, s.ifname)
		_, err := run(ctx, 30*time.Second, c.bin, s.ifname)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("no DHCP client found (need dhclient or dhcpcd)")
}

// supplicantProcesses maps pid -> argv for every wpa_supplicant
// process (proc root is a test seam defaulting to /proc).
func (s *supplicantBackend) supplicantProcesses() map[int][]string {
	root := s.procRoot
	if root == "" {
		root = "/proc"
	}
	out := map[int][]string{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(root + "/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		// argv is NUL-separated; NUL is not a unicode.IsSpace rune, so
		// Fields would not split it.
		args := []string{}
		for _, part := range strings.Split(string(b), "\x00") {
			if part != "" {
				args = append(args, part)
			}
		}
		if len(args) > 0 && strings.HasSuffix(args[0], "wpa_supplicant") {
			out[pid] = args
		}
	}
	return out
}

// --- status / scan parsing -------------------------------------------

func wifiState(statusOut string) string {
	for _, line := range strings.Split(statusOut, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "wpa_state:"); ok {
			return v
		}
	}
	return ""
}

func statusSSID(statusOut string) string {
	for _, line := range strings.Split(statusOut, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ssid:"); ok {
			return v
		}
	}
	return ""
}

// parseIWScan parses `iw dev <if> scan dump` output: blocks start at a
// "BSSID " line. Duplicate SSIDs keep the strongest signal.
func parseIWScan(out string) []Network {
	var nets []Network
	seen := map[string]int{} // ssid -> index in nets

	var cur *struct {
		ssid string
		sig  int
		wpa  bool
		ok   bool
	}
	flush := func() {
		if cur != nil && cur.ok && cur.ssid != "" {
			if i, dup := seen[cur.ssid]; dup {
				if cur.sig > nets[i].Signal {
					nets[i] = Network{SSID: cur.ssid, Signal: cur.sig, Encrypted: cur.wpa}
				}
				return
			}
			seen[cur.ssid] = len(nets)
			nets = append(nets, Network{SSID: cur.ssid, Signal: cur.sig, Encrypted: cur.wpa})
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "BSSID ") {
			flush()
			cur = &struct {
				ssid string
				sig  int
				wpa  bool
				ok   bool
			}{}
			continue
		}
		if cur == nil {
			continue
		}
		cur.ok = true
		if v, ok := strings.CutPrefix(line, "SSID: "); ok {
			cur.ssid = v
		} else if v, ok := strings.CutPrefix(line, "signal: "); ok {
			if f, err := strconv.ParseFloat(strings.SplitN(v, " ", 2)[0], 64); err == nil {
				cur.sig = dbmToPercent(int(f))
			}
		} else if strings.HasPrefix(line, "WPA:") || strings.HasPrefix(line, "RSN:") {
			cur.wpa = true
		}
	}
	flush()
	return nets
}
