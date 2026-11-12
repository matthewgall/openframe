package drm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"unsafe"

	"github.com/matthewgall/openframe/internal/frame/display"
)

// The kernel matches ioctl numbers AND argument struct sizes. A Go
// mirror with the wrong size produces a different ioctl number and
// the kernel rejects the call with EINVAL — a confusing failure on a
// 1280x800 panel at 3am. These tests pin the sizes to the values
// verified against the Linux uapi headers (v5.10, v6.6, master all
// agree) with a C probe, so layout drift fails CI instead.
func TestStructLayoutsMatchKernelUAPI(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"setClientCap", unsafe.Sizeof(setClientCap{}), 16},
		{"cardRes", unsafe.Sizeof(cardRes{}), 64},
		{"getEncoder", unsafe.Sizeof(getEncoder{}), 20},
		{"getConnector", unsafe.Sizeof(getConnector{}), 80},
		{"modeInfo", unsafe.Sizeof(modeInfo{}), 68},
		{"getPlaneRes", unsafe.Sizeof(getPlaneRes{}), 16},
		{"getPlane", unsafe.Sizeof(getPlane{}), 32},
		{"getProperty", unsafe.Sizeof(getProperty{}), 64},
		{"createBlob", unsafe.Sizeof(createBlob{}), 16},
		{"fbCmd2", unsafe.Sizeof(fbCmd2{}), 104},
		{"atomicCmd", unsafe.Sizeof(atomicCmd{}), 56},
		{"createDumb", unsafe.Sizeof(createDumb{}), 32},
		{"mapDumb", unsafe.Sizeof(mapDumb{}), 16},
		{"destroyDumb", unsafe.Sizeof(destroyDumb{}), 4},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d bytes, kernel expects %d", c.name, c.got, c.want)
		}
	}
}

// The exact request words, derived from the v5.10 uapi with a C
// probe: _IOC(dir, 'd', nr, size).
func TestIoctlRequestWords(t *testing.T) {
	cases := []struct {
		name string
		got  uint
		want uint
	}{
		{"setClientCap", reqSetClientCap, 0x4010640d},
		{"getResources", reqModeGetResources, 0xc04064a0},
		{"getEncoder", reqModeGetEncoder, 0xc01464a6},
		{"getConnector", reqModeGetConnector, 0xc05064a7},
		{"getProperty", reqModeGetProperty, 0xc04064aa},
		{"rmFB", reqModeRMFB, 0xc00464af},
		{"createDumb", reqModeCreateDumb, 0xc02064b2},
		{"mapDumb", reqModeMapDumb, 0xc01064b3},
		{"destroyDumb", reqModeDestroyDumb, 0xc00464b4},
		{"getPlaneRes", reqModeGetPlaneRes, 0xc01064b5},
		{"getPlane", reqModeGetPlane, 0xc02064b6},
		{"addFB2", reqModeAddFB2, 0xc06864b8},
		{"atomic", reqModeAtomic, 0xc03864bc},
		{"createPropBlob", reqModeCreatePropBlob, 0xc01064bd},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = 0x%x, want 0x%x", c.name, c.got, c.want)
		}
	}
}

func TestModeInfoFieldOffsets(t *testing.T) {
	// The MODE_ID blob is length-validated by the kernel against its
	// own struct; a field-order slip would corrupt the mode.
	var m modeInfo
	wantOffsets := map[string][2]uintptr{
		"clock":    {0, unsafe.Offsetof(m.clock)},
		"hdisplay": {4, unsafe.Offsetof(m.hdisplay)},
		"vdisplay": {14, unsafe.Offsetof(m.vdisplay)},
		"vrefresh": {24, unsafe.Offsetof(m.vrefresh)},
		"type":     {32, unsafe.Offsetof(m.mtype)},
		"name":     {36, unsafe.Offsetof(m.name)},
	}
	for name, pair := range wantOffsets {
		if pair[1] != pair[0] {
			t.Errorf("modeInfo.%s at %d, kernel has %d", name, pair[1], pair[0])
		}
	}
}

func TestSelectMode(t *testing.T) {
	mk := func(name string, w, h, refresh int, mtype uint32) modeInfo {
		var m modeInfo
		m.hdisplay, m.vdisplay, m.vrefresh = uint16(w), uint16(h), uint32(refresh)
		m.mtype = mtype
		copy(m.name[:], name)
		return m
	}
	modes := []modeInfo{
		mk("1024x768", 1024, 768, 60, 0),
		mk("1280x800", 1280, 800, 60, modeTypePreferred),
		mk("1920x1080", 1920, 1080, 60, 0),
		mk("leftover", 800, 480, 30, modeTypeUserDef),
	}
	m, ok := selectMode(modes)
	if !ok {
		t.Fatal("no mode selected")
	}
	if m.hdisplay != 1280 || m.vdisplay != 800 {
		t.Errorf("selected %dx%d, want preferred 1280x800", m.hdisplay, m.vdisplay)
	}
	// Without a preferred flag the highest resolution wins.
	m, ok = selectMode([]modeInfo{modes[0], modes[2]})
	if !ok || m.hdisplay != 1920 {
		t.Errorf("selected %+v, want 1920x1080", m)
	}
	// Only user-defined modes: nothing selectable.
	if _, ok := selectMode([]modeInfo{modes[3]}); ok {
		t.Error("user-defined-only modes must be rejected")
	}
	// Empty: nothing.
	if _, ok := selectMode(nil); ok {
		t.Error("empty mode list must be rejected")
	}
}

func TestRotationBit(t *testing.T) {
	cases := []struct {
		degrees int
		want    uint64
	}{
		{0, rotateNormal},
		{90, rotate90},
		{180, rotate180},
		{270, rotate270},
		{360, rotateNormal},
		{450, rotate90},
		{45, 0},
		{-90, 0},
	}
	for _, c := range cases {
		if got := rotationBit(c.degrees); got != c.want {
			t.Errorf("rotationBit(%d) = %d, want %d", c.degrees, got, c.want)
		}
	}
}

func TestNameOf(t *testing.T) {
	var b [32]byte
	copy(b[:], "CRTC_ID")
	if got := nameOf(b); got != "CRTC_ID" {
		t.Errorf("nameOf = %q, want CRTC_ID", got)
	}
	var empty [32]byte
	if got := nameOf(empty); got != "" {
		t.Errorf("nameOf(empty) = %q, want empty", got)
	}
}

func TestU64Ptr(t *testing.T) {
	s := []uint32{1, 2, 3}
	if p := u64Ptr(s); p == 0 {
		t.Error("non-empty slice must produce a non-zero pointer")
	}
	var empty []uint32
	if p := u64Ptr(empty); p != 0 {
		t.Errorf("empty slice = %d, want 0", p)
	}
}

func TestFactoryRegistration(t *testing.T) {
	// The init() registration must be live: asking the factory for
	// the drm backend should reach the constructor (and fail cleanly
	// because this machine has no /dev/dri/card0), NOT "unknown
	// driver".
	d, err := display.New(context.Background(), display.Options{Name: "drm", Width: 1280, Height: 800, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if d != nil {
		d.Close()
		t.Fatal("expected error on a machine without /dev/dri/card0")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, display.ErrUnsupportedDriver) {
		t.Errorf("backend not registered: %v", err)
	}
}
