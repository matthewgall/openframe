package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/platform"
	"github.com/matthewgall/openframe/internal/version"
)

type checkStatus string

const (
	checkOK   checkStatus = "ok"
	checkWarn checkStatus = "warn"
	checkFail checkStatus = "fail"
)

type checkResult struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type doctorReport struct {
	Version string            `json:"version"`
	Host    platform.HostInfo `json:"host"`
	Checks  []checkResult     `json:"checks"`
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	cfg, _, cfgErr := configTolerant(&cf)

	sigCtx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, 60*time.Second)
	defer cancel()

	rep := doctorReport{Version: version.Version, Host: platform.Host()}
	add := func(name string, st checkStatus, detail string) {
		rep.Checks = append(rep.Checks, checkResult{Name: name, Status: st, Detail: detail})
	}

	// 1. OS / architecture.
	host := platform.Host()
	osArch := fmt.Sprintf("%s/%s (go %s)", host.OS, host.Arch, host.Go)
	switch host.Arch {
	case "amd64", "arm64":
		add("os", checkOK, osArch)
	case "arm":
		add("os", checkWarn, osArch+" — 32-bit arm is not a supported target; use 64-bit Raspberry Pi OS")
	default:
		add("os", checkWarn, osArch+" — untested architecture")
	}

	// 2. Memory.
	mem := platform.Memory()
	if mem.TotalBytes == 0 {
		add("memory", checkWarn, "could not read /proc/meminfo")
	} else {
		need := int64(1 << 30) // 1 GiB target for Pi 3B+
		st := checkOK
		detail := fmt.Sprintf("%d MB total, %d MB available", mem.TotalBytes>>20, mem.Available>>20)
		if mem.TotalBytes < need {
			st = checkWarn
			detail += " — below the 1 GiB frame target"
		}
		add("memory", st, detail)
	}

	// 3. Disk space for the data directory.
	_, frameDir, _ := cfg.Paths()
	disk, err := platform.Disk(frameDir)
	if err != nil {
		add("disk", checkWarn, fmt.Sprintf("could not stat %s: %v", frameDir, err))
	} else {
		st := checkOK
		detail := fmt.Sprintf("%.1f GB free of %.1f GB (%s)", disk.AvailableGB, float64(disk.TotalBytes)/(1<<30), frameDir)
		if disk.FreeBytes < 1<<30 {
			st = checkWarn
			detail += " — less than 1 GB free; downloads and updates will fail"
		}
		add("disk", st, detail)
	}

	// 4. Configuration file.
	if cfgErr != nil {
		add("config", checkWarn, "using defaults — "+cfgErr.Error())
	} else {
		detail := "defaults"
		if cf.configPath != "" {
			detail = cf.configPath
		}
		add("config", checkOK, "valid ("+detail+")")
	}

	// 5. Server database integrity (if present).
	dbCheck(ctx, "server database", filepath.Join(cfg.DataDir, "server", "server.db"), add)
	// 6. Frame database integrity (if present).
	dbCheck(ctx, "frame database", filepath.Join(frameDir, "frame.db"), add)

	// 7. Display detection.
	disp := platform.DetectDisplay()
	if disp.Present {
		add("display", checkOK, fmt.Sprintf("%v", disp.Backends))
	} else {
		add("display", checkWarn, "no display backend found; the headless dev renderer will be used")
	}

	// 8. Touch input detection.
	touch := platform.DetectTouch()
	if touch.Present {
		add("touch", checkOK, fmt.Sprintf("%v", touch.Devices))
	} else {
		add("touch", checkWarn, "no touchscreen detected (dev renderer works without one)")
	}

	// 9. Network / server reachability (informational: offline is a
	//    supported operating mode, so unreachable is a warning).
	fsURL := cfg.Frame.ServerURL
	if fsURL == "" {
		add("server", checkWarn, "frame.server_url not configured")
	} else {
		url := fsURL
		if len(url) > 0 && url[len(url)-1] == '/' {
			url = url[:len(url)-1]
		}
		url += "/healthz"
		if err := platform.Reachable(ctx, url, 5*time.Second); err != nil {
			add("server", checkWarn, "unreachable: "+err.Error()+" (frames operate offline; this only blocks sync)")
		} else {
			add("server", checkOK, "reachable")
		}
	}

	// 10. Pairing / identity.
	st := readFrameState(ctx, cfg)
	switch {
	case !st.Available:
		add("pairing", checkWarn, "no frame state yet (run `openframe frame` or `openframe pair`)")
	case st.DeviceID == "":
		add("pairing", checkWarn, "frame database exists but has no identity")
	default:
		add("pairing", checkOK, fmt.Sprintf("device %s (state: %s)", st.DeviceID, st.PairingStatus))
	}

	// 11. Local cache health.
	photosDir := filepath.Join(frameDir, "photos")
	if fi, err := os.Stat(photosDir); err == nil && fi.IsDir() {
		add("cache", checkOK, photosDir)
	} else {
		add("cache", checkWarn, "photo cache not initialised yet (created on first sync)")
	}

	// 12. Updater / A-B slot layout.
	slotsDir := "/opt/openframe/slots"
	if _, err := os.Stat(filepath.Join(slotsDir, "a", "openframe")); err == nil {
		add("updater", checkOK, "A/B slot layout present at /opt/openframe")
	} else {
		add("updater", checkWarn, "A/B slot layout not installed yet (arrives with the deployment package, milestone 6)")
	}

	// Render.
	fails := 0
	for _, c := range rep.Checks {
		if c.Status == checkFail {
			fails++
		}
	}

	if cf.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return ExitError
		}
	} else {
		fmt.Println("OpenFrame doctor")
		fmt.Println("================")
		fmt.Printf("%s  %s/%s\n", rep.Version, host.OS, host.Arch)
		fmt.Println()
		for _, c := range rep.Checks {
			mark := "[ok]  "
			switch c.Status {
			case checkWarn:
				mark = "[warn]"
			case checkFail:
				mark = "[FAIL]"
			}
			fmt.Printf("  %s %-16s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Println()
		if fails > 0 {
			fmt.Printf("%d check(s) failed.\n", fails)
		} else {
			fmt.Println("No failing checks.")
		}
	}
	if fails > 0 {
		return ExitError
	}
	return ExitOK
}

// dbCheck reports database presence and integrity. Absence is a warning
// (a fresh machine is not broken); corruption is a hard fail.
func dbCheck(ctx context.Context, name, path string, add func(string, checkStatus, string)) {
	if _, err := os.Stat(path); err != nil {
		add(name, checkWarn, "not initialised yet ("+path+")")
		return
	}
	dbh, err := db.Open(ctx, path, true)
	if err != nil {
		add(name, checkFail, "cannot open: "+err.Error())
		return
	}
	defer dbh.Close()
	result, err := db.IntegrityCheck(ctx, dbh)
	if err != nil {
		add(name, checkFail, "integrity check error: "+err.Error())
		return
	}
	switch result {
	case "ok":
		add(name, checkOK, path)
	default:
		add(name, checkFail, "integrity_check: "+result)
	}
}
