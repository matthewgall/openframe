//go:build linux

package drm

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/matthewgall/openframe/internal/frame/display"
)

// Driver renders frames directly on the panel via DRM/KMS atomic
// commits, using dumb (CPU) framebuffers. One framebuffer object is
// created per present and swapped in atomically; the previous one is
// released after the commit lands. No X, no Wayland, no compositor.
type Driver struct {
	log  *slog.Logger
	card string
	// file is held (not just its Fd()) so the GC finalizer cannot close
	// the descriptor out from under in-flight ioctls — that produced
	// "bad file descriptor" mid-Init once probe() had succeeded.
	file   *os.File
	width  int
	height int

	// geometry chosen at probe time
	mode     modeInfo
	modeBlob uint32
	planeID  uint32
	crtcID   uint32
	connID   uint32

	// global property table: id -> name. The same name can exist as
	// several distinct global properties (vc4 registers one
	// "rotation"/"alpha"/... property per plane), so per-object maps
	// below are the only safe way to address an object's properties.
	propNames map[uint32]string
	// per-object property maps: name -> global id, for the objects
	// this driver commits.
	connProps  map[string]uint32
	crtcProps  map[string]uint32
	planeProps map[string]uint32

	// dumb buffer state
	dumbHandle uint32
	dumbPitch  uint32
	buf        []byte // mmap'd dumb buffer
	lastFB     uint32

	// display state
	brightness float64
	power      bool
	rotation   int  // degrees: 0, 90, 180, 270
	committed  bool // first commit needs ALLOW_MODESET
}

// New opens the DRM card and probes connectors/planes/properties.
// It does not touch the panel: call Init to apply the mode and bring
// the display up.
func New(log *slog.Logger, card string, width, height int) (*Driver, error) {
	fd, err := os.OpenFile(card, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", card, err)
	}
	d := &Driver{
		log:    log,
		card:   card,
		file:   fd,
		width:  width,
		height: height,
		power:  true,
	}
	if err := d.enableAtomic(); err != nil {
		d.file.Close()
		return nil, fmt.Errorf("enable atomic: %w", err)
	}
	if err := d.probe(); err != nil {
		d.file.Close()
		return nil, err
	}
	return d, nil
}

// enableAtomic sets DRM_CLIENT_CAP_ATOMIC on this file handle.
// value 2 also requests universal planes (supported by vc4); fall
// back to 1 if the kernel rejects it.
func (d *Driver) enableAtomic() error {
	for _, v := range []uint64{2, 1} {
		arg := setClientCap{capability: clientCapAtomic, value: v}
		err := ioctl(d.file.Fd(), reqSetClientCap, unsafe.Pointer(&arg))
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("kernel refused DRM_CLIENT_CAP_ATOMIC")
}

// probe walks the resource tree and selects connector, crtc, plane,
// mode and property names.
func (d *Driver) probe() error {
	ids, err := d.getResources()
	if err != nil {
		return err
	}
	if len(ids.connectors) == 0 {
		return fmt.Errorf("no connectors on %s", d.card)
	}

	// Find the first connected connector with usable modes.
	type candidate struct {
		connID uint32
		modes  []modeInfo
		encs   []uint32
	}
	var cands []candidate
	for _, cid := range ids.connectors {
		info, err := d.getConnector(cid)
		if err != nil {
			continue
		}
		if info.connection != connectorStatusConnected || len(info.modes) == 0 {
			continue
		}
		cands = append(cands, candidate{connID: cid, modes: info.modes, encs: info.encoders})
	}
	if len(cands) == 0 {
		return fmt.Errorf("no connected connector with modes on %s", d.card)
	}
	c := cands[0]
	d.connID = c.connID

	// Pick a mode: preferred, then highest resolution, then first.
	mode, ok := selectMode(c.modes)
	if !ok {
		return fmt.Errorf("connector has no usable mode")
	}
	d.mode = mode

	// The canvas must match the panel; a mismatch is a configuration
	// error, not something to paper over with scaling.
	if int(mode.hdisplay) != d.width || int(mode.vdisplay) != d.height {
		return fmt.Errorf("panel mode %dx%d does not match configured %dx%d",
			mode.hdisplay, mode.vdisplay, d.width, d.height)
	}

	// CRTC: first encoder that reports one.
	for _, eid := range c.encs {
		enc, err := d.getEncoder(eid)
		if err == nil && enc.crtcID != 0 {
			d.crtcID = enc.crtcID
			break
		}
	}
	if d.crtcID == 0 {
		// No encoder is currently bound; use the first CRTC on the card.
		if len(ids.crtcs) > 0 {
			d.crtcID = ids.crtcs[0]
		} else {
			return fmt.Errorf("no CRTC found")
		}
	}

	// Plane: prefer one already bound to our CRTC with a live
	// framebuffer, else the first bound, else the first.
	planes, err := d.getPlaneIDs()
	if err != nil {
		return err
	}
	bound := uint32(0)
	for _, pid := range planes {
		p, err := d.getPlane(pid)
		if err != nil {
			continue
		}
		if p.crtcID == d.crtcID {
			if p.fbID != 0 && bound == 0 {
				d.planeID = pid
				break // live scanout plane: best choice
			}
			if bound == 0 {
				bound = pid
			}
		}
	}
	if d.planeID == 0 {
		d.planeID = bound
	}
	if d.planeID == 0 && len(planes) > 0 {
		d.planeID = planes[0]
	}
	if d.planeID == 0 {
		return fmt.Errorf("no planes on %s", d.card)
	}

	// Global property table (id -> name). Property ids are allocated
	// from a per-device pool starting at 1 (id 0 is invalid) and are
	// not gap-free (vc4 on 6.18: 548 props spread over 1..669, with
	// per-object blocks leaving holes), so scan the range skipping
	// misses and stop after a long run of consecutive misses.
	d.propNames = make(map[uint32]string)
	misses := 0
	for i := 1; i <= 4096 && misses < 128; i++ {
		p, err := d.getProperty(uint32(i))
		if err != nil {
			misses++
			continue
		}
		misses = 0
		d.propNames[p.propID] = nameOf(p.name)
	}

	// Per-object property resolution. An atomic commit may only set
	// properties the object actually owns, and a duplicated name
	// (e.g. vc4's per-plane "rotation") belongs to a specific object;
	// resolving against the object's own list is the only correct way.
	if d.connProps, err = d.objectProps(d.connID, objConnector); err != nil {
		return fmt.Errorf("connector %d properties: %w", d.connID, err)
	}
	if d.crtcProps, err = d.objectProps(d.crtcID, objCrtc); err != nil {
		return fmt.Errorf("crtc %d properties: %w", d.crtcID, err)
	}
	if d.planeProps, err = d.objectProps(d.planeID, objPlane); err != nil {
		return fmt.Errorf("plane %d properties: %w", d.planeID, err)
	}
	for _, want := range []string{"MODE_ID", "ACTIVE"} {
		if _, ok := d.crtcProps[want]; !ok {
			return fmt.Errorf("required DRM property %q not found on CRTC %d", want, d.crtcID)
		}
	}
	for _, want := range []string{"FB_ID", "CRTC_ID", "CRTC_W", "CRTC_H", "SRC_W", "SRC_H"} {
		if _, ok := d.planeProps[want]; !ok {
			return fmt.Errorf("required DRM property %q not found on plane %d", want, d.planeID)
		}
	}
	if _, ok := d.connProps["CRTC_ID"]; !ok {
		return fmt.Errorf("required DRM property CRTC_ID not found on connector %d", d.connID)
	}
	return nil
}

// objectProps returns the object's own properties as name -> global
// property id, using the global id -> name table built in probe.
func (d *Driver) objectProps(objID, objType uint32) (map[string]uint32, error) {
	ids, err := d.objectPropIDs(objID, objType)
	if err != nil {
		return nil, err
	}
	m := make(map[string]uint32, len(ids))
	for _, id := range ids {
		if name, ok := d.propNames[id]; ok {
			m[name] = id
		}
	}
	return m, nil
}

// objectPropIDs lists the global property ids owned by an object.
// OBJ_GETPROPERTIES follows the same total-count/truncated-write
// protocol as the other list ioctls, and it fills BOTH the id and the
// value arrays (current values), so both buffers must be valid.
func (d *Driver) objectPropIDs(objID, objType uint32) ([]uint32, error) {
	probeIDs := make([]uint32, 1)
	probeVals := make([]uint64, 1)
	var arg objGetProps
	arg.propsPtr = u64Ptr(probeIDs)
	arg.propValuesPtr = u64Ptr(probeVals)
	arg.countProps = 1
	arg.objID = objID
	arg.objType = objType
	if err := ioctl(d.file.Fd(), reqModeObjGetProps, unsafe.Pointer(&arg)); err != nil {
		return nil, err
	}
	total := int(arg.countProps)
	if total == 0 {
		return nil, nil
	}
	if total > 4096 {
		return nil, fmt.Errorf("drm: implausible property count %d for object %d", total, objID)
	}
	ids := make([]uint32, total)
	vals := make([]uint64, total)
	arg = objGetProps{
		propsPtr:      u64Ptr(ids),
		propValuesPtr: u64Ptr(vals),
		countProps:    uint32(total),
		objID:         objID,
		objType:       objType,
	}
	if err := ioctl(d.file.Fd(), reqModeObjGetProps, unsafe.Pointer(&arg)); err != nil {
		return nil, err
	}
	n := int(arg.countProps)
	if n > total {
		return nil, fmt.Errorf("drm: property count grew to %d during probe", n)
	}
	return ids[:n], nil
}

// --- raw ioctl wrappers ------------------------------------------------

type resourceIDs struct {
	connectors []uint32
	encoders   []uint32
	crtcs      []uint32
}

func (d *Driver) getResources() (resourceIDs, error) {
	var out resourceIDs
	conn := make([]uint32, 64)
	enc := make([]uint32, 64)
	crtc := make([]uint32, 64)
	arg := cardRes{
		connectorIDPtr:  u64Ptr(conn),
		encoderIDPtr:    u64Ptr(enc),
		crtcIDPtr:       u64Ptr(crtc),
		countConnectors: uint32(len(conn)),
		countEncoders:   uint32(len(enc)),
		countCrtcs:      uint32(len(crtc)),
	}
	if err := ioctl(d.file.Fd(), reqModeGetResources, unsafe.Pointer(&arg)); err != nil {
		return out, err
	}
	// The kernel reports TOTAL counts but writes only what fits the
	// buffers; never slice with a raw kernel-reported count.
	if int(arg.countConnectors) > len(conn) || int(arg.countEncoders) > len(enc) || int(arg.countCrtcs) > len(crtc) {
		return out, fmt.Errorf("drm: resource counts (%d connectors, %d encoders, %d crtcs) exceed buffers", arg.countConnectors, arg.countEncoders, arg.countCrtcs)
	}
	out.connectors = conn[:arg.countConnectors]
	out.encoders = enc[:arg.countEncoders]
	out.crtcs = crtc[:arg.countCrtcs]
	return out, nil
}

type connectorInfo struct {
	connection uint32
	encoders   []uint32
	modes      []modeInfo
}

func (d *Driver) getConnector(id uint32) (connectorInfo, error) {
	var out connectorInfo
	modes := make([]modeInfo, 256)
	encs := make([]uint32, 16)
	props := make([]uint32, 32)
	values := make([]uint64, 32)
	arg := getConnector{
		connectorID:   id,
		modesPtr:      u64Ptr(modes),
		encodersPtr:   u64Ptr(encs),
		propsPtr:      u64Ptr(props),
		propValuesPtr: u64Ptr(values),
		countModes:    uint32(len(modes)),
		countEncoders: uint32(len(encs)),
		countProps:    uint32(len(props)),
	}
	if err := ioctl(d.file.Fd(), reqModeGetConnector, unsafe.Pointer(&arg)); err != nil {
		return out, err
	}
	// The kernel reports TOTAL counts but writes only what fits the
	// buffers; never slice with a raw kernel-reported count.
	if int(arg.countModes) > len(modes) || int(arg.countEncoders) > len(encs) {
		return out, fmt.Errorf("drm: connector %d reports %d modes / %d encoders, exceeding buffers", id, arg.countModes, arg.countEncoders)
	}
	out.connection = arg.connection
	out.modes = modes[:arg.countModes]
	out.encoders = encs[:arg.countEncoders]
	return out, nil
}

func (d *Driver) getEncoder(id uint32) (getEncoder, error) {
	var arg getEncoder
	arg.encoderID = id
	err := ioctl(d.file.Fd(), reqModeGetEncoder, unsafe.Pointer(&arg))
	return arg, err
}

// maxPlaneIDs sanity-bounds the plane count reported by the kernel before
// we allocate for it. Real hardware tops out far below this (56 on the
// vc4 panel with universal planes enabled).
const maxPlaneIDs = 1024

// getPlaneIDs returns the IDs of every plane the kernel advertises to this
// client. The kernel follows a documented two-pass protocol (see
// drm_mode_getplane_res in the kernel: "This ioctl is called twice, once to
// determine how much space is needed, and the 2nd time to fill it"): it
// always reports the TOTAL plane count in countPlanes, but writes only as
// many IDs as fit in the caller's buffer. A single call with a small fixed
// buffer therefore panics in ids[:countPlanes] on cards that advertise more
// planes than the buffer (e.g. 56 on vc4 with universal planes).
func (d *Driver) getPlaneIDs() ([]uint32, error) {
	// Pass 1: one-slot buffer. The kernel writes at most one ID into it
	// (guarded by count < countPlanes) and returns the total.
	probe := make([]uint32, 1)
	var arg getPlaneRes
	arg.planeIDPtr = u64Ptr(probe)
	arg.countPlanes = 1
	if err := ioctl(d.file.Fd(), reqModeGetPlaneRes, unsafe.Pointer(&arg)); err != nil {
		return nil, err
	}
	total := int(arg.countPlanes)
	if total == 0 {
		return nil, nil
	}
	if total > maxPlaneIDs {
		return nil, fmt.Errorf("drm: implausible plane count %d on %s", total, d.card)
	}
	// Pass 2: exact-size buffer, filled by the kernel.
	ids := make([]uint32, total)
	arg = getPlaneRes{
		planeIDPtr:  u64Ptr(ids),
		countPlanes: uint32(total),
	}
	if err := ioctl(d.file.Fd(), reqModeGetPlaneRes, unsafe.Pointer(&arg)); err != nil {
		return nil, err
	}
	n := int(arg.countPlanes)
	if n > total {
		return nil, fmt.Errorf("drm: plane count grew to %d during probe", n)
	}
	return ids[:n], nil
}

func (d *Driver) getPlane(id uint32) (getPlane, error) {
	var arg getPlane
	arg.planeID = id
	err := ioctl(d.file.Fd(), reqModeGetPlane, unsafe.Pointer(&arg))
	return arg, err
}

func (d *Driver) getProperty(id uint32) (getProperty, error) {
	var arg getProperty
	arg.propID = id
	err := ioctl(d.file.Fd(), reqModeGetProperty, unsafe.Pointer(&arg))
	return arg, err
}

// --- pure helpers (unit-tested) ----------------------------------------

// nameOf trims the NUL-padded property name.
func nameOf(b [32]byte) string {
	return strings.TrimRight(string(b[:]), "\x00")
}

// selectMode picks the PREFERRED-flagged mode (drivers list it
// first; that is the panel's native mode), else the highest
// resolution, else the highest refresh rate. User-defined modes are
// excluded (leftovers from other clients).
func selectMode(modes []modeInfo) (modeInfo, bool) {
	var fallback modeInfo
	fbScore := -1
	for _, m := range modes {
		if m.hdisplay == 0 || m.vdisplay == 0 || m.mtype&modeTypeUserDef != 0 {
			continue
		}
		if m.mtype&modeTypePreferred != 0 {
			return m, true
		}
		if score := int(m.hdisplay)*int(m.vdisplay)*1000 + int(m.vrefresh); score > fbScore {
			fallback, fbScore = m, score
		}
	}
	return fallback, fbScore >= 0
}

// rotationBit maps display degrees to the CRTC rotation property bit.
// Unsupported values (non-multiples of 90) return 0, meaning "keep
// normal".
func rotationBit(degrees int) uint64 {
	switch degrees % 360 {
	case 0:
		return rotateNormal
	case 90:
		return rotate90
	case 180:
		return rotate180
	case 270:
		return rotate270
	}
	return 0
}

// writeXRGB8888 converts an image.NRGBA pixel buffer into the dumb
// buffer's XRGB8888 layout. NRGBA Pix bytes are [R,G,B,A] (see the
// image.NRGBA docs; on Go >= 1.25 alpha is the FOURTH byte, not the
// first); XRGB8888 bytes on little-endian are [B,G,R,X]. So this is
// an R/B swap with G in place and alpha ignored. A raw copy swaps
// R and B (red cars render blue); assuming [A,R,G,B] puts alpha
// (255) into the blue slot (blue cast over everything). The unit
// test pins the byte order.
func writeXRGB8888(dst, src []byte, w, h, dstPitch int) {
	if len(src) < h*w*4 {
		panic(fmt.Sprintf("drm: source pixel buffer %d bytes, want %d", len(src), h*w*4))
	}
	if len(dst) < h*dstPitch {
		panic(fmt.Sprintf("drm: dumb buffer %d bytes, want %d", len(dst), h*dstPitch))
	}
	for y := 0; y < h; y++ {
		s := src[y*w*4 : (y+1)*w*4]
		t := dst[y*dstPitch : y*dstPitch+w*4]
		for x := 0; x < w*4; x += 4 {
			// src [R,G,B,A] -> dst [B,G,R,X]
			t[x] = s[x+2]
			t[x+1] = s[x+1]
			t[x+2] = s[x]
			t[x+3] = 0
		}
	}
}

// u64Ptr returns the address of a non-empty slice's backing array as a
// user pointer for a DRM "_ptr" field. The caller must keep the slice
// alive for the duration of the ioctl (it does: same goroutine).
func u64Ptr[T any](s []T) uint64 {
	if len(s) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(s))))
}

// --- driver boundary ----------------------------------------------------

// Name implements display.Driver.
func (d *Driver) Name() string { return "drm" }

// Info implements display.Driver.
func (d *Driver) Info() display.Info {
	return display.Info{
		Name:               "drm",
		ResolutionWidth:    d.width,
		ResolutionHeight:   d.height,
		SupportsBrightness: true,
		SupportsPower:      true,
		Extra: fmt.Sprintf("card=%s mode=%s %dx%d@%dHz plane=%d crtc=%d",
			d.card, d.modeName(), d.width, d.height, d.mode.vrefresh, d.planeID, d.crtcID),
	}
}

func (d *Driver) modeName() string {
	return nameOf(d.mode.name)
}

// FrameSize implements display.Driver.
func (d *Driver) FrameSize() (int, int) { return d.width, d.height }

// Init brings the display up: allocates the dumb buffer, applies the
// mode and commits the initial (black) frame. It is safe to call
// exactly once; the UI engine presents afterwards.
func (d *Driver) Init(_ context.Context) error {
	return d.setupDumbBuffer()
}

func (d *Driver) setupDumbBuffer() error {
	var create createDumb
	create.width, create.height, create.bpp = uint32(d.width), uint32(d.height), 32
	if err := ioctl(d.file.Fd(), reqModeCreateDumb, unsafe.Pointer(&create)); err != nil {
		return fmt.Errorf("create dumb buffer: %w", err)
	}
	d.dumbHandle = create.handle
	d.dumbPitch = create.pitch

	var mapArg mapDumb
	mapArg.handle = create.handle
	if err := ioctl(d.file.Fd(), reqModeMapDumb, unsafe.Pointer(&mapArg)); err != nil {
		return fmt.Errorf("map dumb buffer: %w", err)
	}
	mmapLen := int(create.size)
	if mmapLen <= 0 {
		mmapLen = int(d.dumbPitch) * d.height
	}
	// The dumb buffer is mapped through the DRM file descriptor (the
	// offset from MAP_DUMB is an offset into that fd, not into fd -1).
	ptr, err := syscall.Mmap(int(d.file.Fd()), int64(mapArg.offset), mmapLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap dumb buffer: %w", err)
	}
	d.buf = unsafe.Slice((*byte)(unsafe.Pointer(&ptr[0])), len(ptr))

	// The mode blob is fixed for the life of the driver: create it
	// once, reuse it in every commit. It must exist before the first
	// commit, which references it as MODE_ID.
	if d.modeBlob, err = d.createModeBlob(); err != nil {
		return err
	}

	// First frame: black, with the mode applied.
	if err := d.presentBlack(); err != nil {
		return err
	}
	d.brightness = 1.0
	if err := d.applyBrightness(d.brightness); err != nil {
		d.log.Warn("drm: backlight init failed", "err", err)
	}
	return nil
}

func (d *Driver) presentBlack() error {
	for i := range d.buf {
		d.buf[i] = 0
	}
	newFB, err := d.addFB()
	if err != nil {
		return err
	}
	if err := d.commit(newFB); err != nil {
		d.rmFB(newFB)
		return err
	}
	d.lastFB = newFB
	return nil
}

// PresentFrame implements display.Driver. It copies the frame into
// the dumb buffer, builds a new framebuffer object and commits it
// atomically. The previously committed framebuffer is released
// afterwards.
func (d *Driver) PresentFrame(_ context.Context, img *image.NRGBA) error {
	if img.Bounds() != (image.Rectangle{Max: image.Point{d.width, d.height}}) {
		return fmt.Errorf("drm: frame is %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), d.width, d.height)
	}
	pitch := int(d.dumbPitch)
	writeXRGB8888(d.buf, img.Pix, d.width, d.height, pitch)

	newFB, err := d.addFB()
	if err != nil {
		return err
	}
	if err := d.commit(newFB); err != nil {
		// The new fb exists but was never scanned out: drop it.
		d.rmFB(newFB)
		return err
	}
	old := d.lastFB
	d.lastFB = newFB
	if old != 0 {
		d.rmFB(old)
	}
	return nil
}

// addFB wraps the current dumb-buffer contents in a new framebuffer
// object and returns its id.
func (d *Driver) addFB() (uint32, error) {
	var fb fbCmd2
	fb.width, fb.height = uint32(d.width), uint32(d.height)
	fb.pixelFormat = formatXRGB8888
	fb.handles[0] = d.dumbHandle
	fb.pitches[0] = d.dumbPitch
	if err := ioctl(d.file.Fd(), reqModeAddFB2, unsafe.Pointer(&fb)); err != nil {
		return 0, fmt.Errorf("addfb2: %w", err)
	}
	return fb.fbID, nil
}

func (d *Driver) rmFB(id uint32) {
	ioctl(d.file.Fd(), reqModeRMFB, unsafe.Pointer(&id)) // best effort
}

// commit performs the atomic commit: connector -> crtc, crtc mode
// active, plane -> framebuffer at full size. rotation is applied
// from the current setting. The first commit requests a mode set.
func (d *Driver) commit(fbID uint32) error {
	if fbID == 0 {
		id, err := d.addFB()
		if err != nil {
			return err
		}
		fbID = id
	}

	// Properties are resolved per object: an atomic commit may only set
	// properties the object owns, and duplicated names (vc4's per-plane
	// "rotation"/"alpha"/...) belong to a specific object.
	crtc := d.crtcProps
	plane := d.planeProps
	conn := d.connProps

	// Objects and their property counts.
	crtcN := 2 // MODE_ID, ACTIVE
	if _, ok := crtc["rotation"]; ok && d.rotation != 0 {
		crtcN++
	}
	planeN := 8 // FB_ID, CRTC_ID, CRTC_X, CRTC_Y, CRTC_W, CRTC_H, SRC_W, SRC_H
	connN := 1  // CRTC_ID

	counts := []uint32{uint32(connN), uint32(crtcN), uint32(planeN)}
	total := connN + crtcN + planeN
	props := make([]uint32, 0, total)
	values := make([]uint64, 0, total)

	addProp := func(id uint32, val uint64) {
		props = append(props, id)
		values = append(values, val)
	}

	// Connector -> CRTC
	addProp(conn["CRTC_ID"], uint64(d.crtcID))

	// CRTC: mode + active (+ rotation, where the hardware exposes a
	// CRTC rotation property; vc4 does not).
	addProp(crtc["MODE_ID"], uint64(d.modeBlob))
	addProp(crtc["ACTIVE"], 1)
	if rid, ok := crtc["rotation"]; ok && d.rotation != 0 {
		addProp(rid, rotationBit(d.rotation))
	}

	// Plane -> framebuffer, full size, no crop.
	w, h := uint64(d.width), uint64(d.height)
	addProp(plane["FB_ID"], uint64(fbID))
	addProp(plane["CRTC_ID"], uint64(d.crtcID))
	if _, ok := plane["CRTC_X"]; ok {
		addProp(plane["CRTC_X"], 0)
		addProp(plane["CRTC_Y"], 0)
	}
	addProp(plane["CRTC_W"], w)
	addProp(plane["CRTC_H"], h)
	addProp(plane["SRC_W"], w<<16)
	addProp(plane["SRC_H"], h<<16)

	objs := []uint32{d.connID, d.crtcID, d.planeID}

	var cmd atomicCmd
	cmd.flags = atomicNonBlock
	if !d.committed {
		cmd.flags |= atomicAllowModeSet
	}
	cmd.countObjs = uint32(len(objs))
	cmd.objsPtr = u64Ptr(objs)
	cmd.countPropsPtr = u64Ptr(counts)
	cmd.propsPtr = u64Ptr(props)
	cmd.propValuesPtr = u64Ptr(values)

	if err := ioctl(d.file.Fd(), reqModeAtomic, unsafe.Pointer(&cmd)); err != nil {
		return fmt.Errorf("atomic commit: %w", err)
	}
	d.committed = true
	return nil
}

// createModeBlob uploads the selected mode as a MODE_ID property
// blob. vc4 (and the core) validate the blob length against its own
// drm_mode_modeinfo, so the struct mirror must be byte-exact.
func (d *Driver) createModeBlob() (uint32, error) {
	var arg createBlob
	arg.data = u64Ptr([]byte(unsafe.Slice((*byte)(unsafe.Pointer(&d.mode)), unsafe.Sizeof(d.mode))))
	arg.length = uint32(unsafe.Sizeof(d.mode))
	if err := ioctl(d.file.Fd(), reqModeCreatePropBlob, unsafe.Pointer(&arg)); err != nil {
		return 0, fmt.Errorf("create mode blob: %w", err)
	}
	return arg.blobID, nil
}

// SetBrightness implements display.Driver via the sysfs backlight
// class. Frames without a backlight expose nothing to write and the
// call is a documented no-op.
func (d *Driver) SetBrightness(_ context.Context, fraction float64) error {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	d.brightness = fraction
	if !d.power {
		return nil
	}
	return d.applyBrightness(fraction)
}

func (d *Driver) applyBrightness(fraction float64) error {
	bl, err := findBacklight()
	if err != nil {
		return nil // no backlight: brightness is not applicable
	}
	max, err := readBacklightMax(bl)
	if err != nil {
		return err
	}
	val := int(fraction * float64(max))
	if err := os.WriteFile(filepath.Join(bl, "brightness"), []byte(strconv.Itoa(val)), 0o644); err != nil {
		return err
	}
	return nil
}

// SetPower implements display.Driver. Off: backlight to zero (and
// the UI presents black). On: brightness restored.
func (d *Driver) SetPower(_ context.Context, on bool) error {
	d.power = on
	if on {
		return d.applyBrightness(d.brightness)
	}
	bl, err := findBacklight()
	if err != nil {
		return nil
	}
	return os.WriteFile(filepath.Join(bl, "brightness"), []byte("0"), 0o644)
}

// SetRotation implements display.Driver. The new rotation takes
// effect on the next commit.
func (d *Driver) SetRotation(_ context.Context, degrees int) error {
	switch degrees % 360 {
	case 0, 90, 180, 270:
		d.rotation = degrees % 360
		return nil
	default:
		return fmt.Errorf("drm: rotation %d not supported (want 0/90/180/270)", degrees)
	}
}

// Close releases the dumb buffer, framebuffer and card handle.
func (d *Driver) Close() error {
	if d.lastFB != 0 {
		d.rmFB(d.lastFB)
		d.lastFB = 0
	}
	if len(d.buf) > 0 {
		syscall.Munmap(d.buf)
		d.buf = nil
	}
	if d.dumbHandle != 0 {
		var dd destroyDumb
		dd.handle = d.dumbHandle
		ioctl(d.file.Fd(), reqModeDestroyDumb, unsafe.Pointer(&dd))
		d.dumbHandle = 0
	}
	if d.file != nil {
		err := d.file.Close()
		d.file = nil
		return err
	}
	return nil
}

// --- sysfs backlight helpers -------------------------------------------

// findBacklight returns the first /sys/class/backlight/<name> entry
// that has a writable brightness file.
func findBacklight() (string, error) {
	dir := "/sys/class/backlight"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(path, "brightness")); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no backlight found")
}

func readBacklightMax(bl string) (int, error) {
	b, err := os.ReadFile(filepath.Join(bl, "max_brightness"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// --- factory helper ------------------------------------------------------

// probeCard opens the card read-mostly and reports the preferred
// mode of the first connected connector. It never commits anything.
func probeCard(card string) (desc string, w, h int, err error) {
	fd, err := os.OpenFile(card, os.O_RDWR, 0)
	if err != nil {
		return "", 0, 0, err
	}
	defer fd.Close()
	d := &Driver{card: card, file: fd}
	for _, v := range []uint64{2, 1} {
		arg := setClientCap{capability: clientCapAtomic, value: v}
		if err := ioctl(d.file.Fd(), reqSetClientCap, unsafe.Pointer(&arg)); err == nil {
			break
		}
	}
	ids, err := d.getResources()
	if err != nil {
		return "", 0, 0, fmt.Errorf("probe %s: %w (another DRM master may be holding it)", card, err)
	}
	for _, cid := range ids.connectors {
		info, err := d.getConnector(cid)
		if err == nil && info.connection == connectorStatusConnected && len(info.modes) > 0 {
			m, _ := selectMode(info.modes)
			return fmt.Sprintf("%s %dx%d @ %dHz", nameOf(m.name), m.hdisplay, m.vdisplay, m.vrefresh), int(m.hdisplay), int(m.vdisplay), nil
		}
	}
	return "", 0, 0, fmt.Errorf("no connected display on %s", card)
}

// Available reports the preferred mode of the given card (used by the
// doctor and by auto-detection). It never mutates display state.
func Available(card string) (string, error) {
	desc, _, _, err := probeCard(card)
	return desc, err
}

// PreferredModeSize returns the preferred-mode resolution of the
// first connected connector on the card.
func PreferredModeSize(card string) (int, int, error) {
	_, w, h, err := probeCard(card)
	return w, h, err
}

// ScanCards lists candidate /dev/dri/card* devices.
func ScanCards() []string {
	var out []string
	for i := 0; i < 8; i++ {
		card := fmt.Sprintf("/dev/dri/card%d", i)
		if _, err := os.Stat(card); err == nil {
			out = append(out, card)
		}
	}
	return out
}

var _ display.Driver = (*Driver)(nil)
