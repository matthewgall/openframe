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
	"github.com/matthewgall/openframe/internal/frame/imgutil"
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
	// fbID is created once alongside the dumb buffer and reused by
	// every commit: the old create-per-present churn cost two ioctls
	// and a kernel object per animation frame (30/s) for zero gain -
	// the FB is just a reference to the same dumb buffer memory.
	fbID uint32

	// half-resolution scanout pair for the crossfade transition:
	// blending at 1/4 the bytes against the HVS doing the upscale is
	// the only way past the Pi 3's DRAM ceiling. Zero when the second
	// buffer/FB could not be created (drivers fall back to full-size
	// transitions automatically).
	halfHandle uint32
	halfPitch  uint32
	halfBuf    []byte
	halfFB     uint32
	halfW      int
	halfH      int

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
	if dstPitch%4 != 0 {
		// Odd pitch (never on vc4): word aliasing impossible, fall
		// back to the byte loop.
		for y := 0; y < h; y++ {
			s := src[y*w*4 : (y+1)*w*4]
			t := dst[y*dstPitch : y*dstPitch+w*4]
			for x := 0; x < w*4; x += 4 {
				t[x] = s[x+2]
				t[x+1] = s[x+1]
				t[x+2] = s[x]
				t[x+3] = 0
			}
		}
		return
	}
	// Word path: NRGBA word [R,G,B,A] (LE) -> XRGB word [B,G,R,0]:
	// one shift pair per pixel instead of four byte moves, split
	// across cores. Rows are 4-byte aligned (src stride w*4, dst
	// pitch multiple of 4) so the uint32 aliasing is sound.
	imgutil.Chunked(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			srow := src[y*w*4:]
			trow := dst[y*dstPitch:]
			sw := unsafe.Slice((*uint32)(unsafe.Pointer(&srow[0])), w)
			tw := unsafe.Slice((*uint32)(unsafe.Pointer(&trow[0])), w)
			for x := 0; x < w; x++ {
				v := sw[x]
				tw[x] = ((v >> 16) & 0x000000FF) | (v & 0x0000FF00) | ((v << 16) & 0x00FF0000)
			}
		}
	})
}

// writeBlendXRGB8888 is the fused crossfade: blend base over top at
// fixed-point weight p/256 and convert to XRGB8888 in ONE pass,
// writing scanout memory directly. Skipping the canvas round-trip
// cuts the per-frame memory traffic roughly by a third versus
// BlendCrossfade + PresentFrame (bandwidth is the Pi 3's ceiling,
// not ALU).
func writeBlendXRGB8888(dst, base, top []byte, w, h, dstPitch int, p uint32) {
	if p > 256 {
		p = 256
	}
	ip := 256 - p
	imgutil.Chunked(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			brow := base[y*w*4 : (y+1)*w*4]
			trow := top[y*w*4 : (y+1)*w*4]
			drow := dst[y*dstPitch : y*dstPitch+w*4]
			for x := 0; x < w*4; x += 4 {
				drow[x] = uint8((uint32(brow[x+2])*ip + uint32(trow[x+2])*p + 128) >> 8)
				drow[x+1] = uint8((uint32(brow[x+1])*ip + uint32(trow[x+1])*p + 128) >> 8)
				drow[x+2] = uint8((uint32(brow[x])*ip + uint32(trow[x])*p + 128) >> 8)
				drow[x+3] = 0
			}
		}
	})
}

// writeSlideXRGB8888 is the fused slide: copy the visible bands of
// base (shifted left by off) and top (entering from the right)
// straight into scanout memory with the channel swap applied.
// Semantics match canvas.SlideFrame: top wins the overlap band,
// which is transparent black pre-blend and stays so in the gap.
// Sub-opaque pixels in the overlap use the same NRGBA-over fallback
// as SlideFrame (frame photos are server-derived JPEGs, so this is
// cold).
func writeSlideXRGB8888(dst, base, top []byte, w, h, dstPitch, off int) {
	overlapLo, overlapHi := off, w-off
	gapLo, gapHi := w-off, off
	baseOnly := min(overlapLo, overlapHi)
	topStart := max(gapHi, overlapHi)
	imgutil.Chunked(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			brow := base[y*w*4 : (y+1)*w*4]
			trow := top[y*w*4 : (y+1)*w*4]
			drow := dst[y*dstPitch : y*dstPitch+w*4]
			copySwapped(drow, 0, brow, off, baseOnly)
			for x := overlapLo; x < overlapHi; x++ {
				ti := (x - off) * 4
				switch trow[ti+3] {
				case 255:
					swap4(drow, x, trow[ti:ti+4], 0)
				case 0:
					swap4(drow, x, brow[(x+off)*4:(x+off)*4+4], 0)
				default:
					bi := (x + off) * 4
					at, ab := float64(trow[ti+3])/255, float64(brow[bi+3])/255
					oa := at + ab*(1-at)
					if oa == 0 {
						drow[x*4], drow[x*4+1], drow[x*4+2], drow[x*4+3] = 0, 0, 0, 0
						continue
					}
					for c := 0; c < 3; c++ {
						num := float64(trow[ti+c])*at + float64(brow[bi+c])*ab*(1-at)
						drow[x*4+2-c] = uint8(num/oa + 0.5) // R,G,B -> slots 2,1,0
					}
					drow[x*4+3] = 0
				}
			}
			for x := gapLo; x < gapHi; x++ {
				drow[x*4], drow[x*4+1], drow[x*4+2], drow[x*4+3] = 0, 0, 0, 0
			}
			copySwapped(drow, topStart, trow, topStart-off, w-topStart)
		}
	})
}

// copySwapped converts n pixels [srcS..] -> [dstD..] with the NRGBA->XRGB
// channel swap, via memmove-speed word reads rather than per-byte moves
// where the ranges are contiguous.
func copySwapped(dst []byte, d int, src []byte, s, n int) {
	for i := 0; i < n; i++ {
		swap4(dst, d+i, src, s*4+i*4)
	}
}

// swap4 converts src pixel bytes [R,G,B,A] at byte offset s into dst
// word [B,G,R,0] at byte offset d (both scaled by 4).
func swap4(dst []byte, d int, src []byte, s int) {
	dst[d*4] = src[s+2]
	dst[d*4+1] = src[s+1]
	dst[d*4+2] = src[s]
	dst[d*4+3] = 0
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
	// The framebuffer likewise: same dumb buffer, same FB, forever.
	if d.fbID, err = d.addFB(); err != nil {
		return err
	}

	// Half-resolution scanout buffer for crossfades (see struct
	// comment). Best effort: absence costs speed, never correctness.
	if hw, hh := d.width/2, d.height/2; hw >= 2 && hh >= 2 {
		if err := d.setupHalfBuffer(hw, hh); err != nil {
			d.log.Info("drm: half-res transition buffer unavailable, full-res fallback", "err", err)
		} else {
			d.halfW, d.halfH = hw, hh
		}
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
	return d.commit()
}

// PresentFrame implements display.Driver. It copies the frame into
// the dumb buffer and commits it atomically through the persistent
// framebuffer.
func (d *Driver) PresentFrame(_ context.Context, img *image.NRGBA) error {
	if img.Bounds() != (image.Rectangle{Max: image.Point{d.width, d.height}}) {
		return fmt.Errorf("drm: frame is %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), d.width, d.height)
	}
	pitch := int(d.dumbPitch)
	writeXRGB8888(d.buf, img.Pix, d.width, d.height, pitch)
	return d.commit()
}

func (d *Driver) setupHalfBuffer(hw, hh int) error {
	var create createDumb
	create.width, create.height, create.bpp = uint32(hw), uint32(hh), 32
	if err := ioctl(d.file.Fd(), reqModeCreateDumb, unsafe.Pointer(&create)); err != nil {
		return err
	}
	d.halfHandle = create.handle
	d.halfPitch = create.pitch
	var mapArg mapDumb
	mapArg.handle = create.handle
	if err := ioctl(d.file.Fd(), reqModeMapDumb, unsafe.Pointer(&mapArg)); err != nil {
		return err
	}
	mmapLen := int(create.size)
	if mmapLen <= 0 {
		mmapLen = int(d.halfPitch) * hh
	}
	ptr, err := syscall.Mmap(int(d.file.Fd()), int64(mapArg.offset), mmapLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	d.halfBuf = unsafe.Slice((*byte)(unsafe.Pointer(&ptr[0])), len(ptr))
	var fb fbCmd2
	fb.width, fb.height = uint32(hw), uint32(hh)
	fb.pixelFormat = formatXRGB8888
	fb.handles[0] = d.halfHandle
	fb.pitches[0] = d.halfPitch
	if err := ioctl(d.file.Fd(), reqModeAddFB2, unsafe.Pointer(&fb)); err != nil {
		return err
	}
	d.halfFB = fb.fbID
	return nil
}

// PresentTransition implements display.TransitionDriver: fused
// blend/slide + XRGB8888 conversion written straight into scanout
// memory, skipping the canvas round-trip. base/top may be full panel
// size or (crossfade only) the half-size pair the engine box-filters
// at transition start: when the half buffer exists, the dissolve runs
// on it and the HVS upscales, cutting per-frame bytes ~4x.
func (d *Driver) PresentTransition(_ context.Context, base, top *image.NRGBA, mode string, t float64) error {
	want := image.Rectangle{Max: image.Point{d.width, d.height}}
	half := image.Rectangle{Max: image.Point{d.halfW, d.halfH}}
	switch mode {
	case "crossfade":
		p := uint32(t*256 + 0.5)
		if p < 1 {
			p = 1
		}
		if p > 255 {
			p = 255
		}
		if d.halfFB != 0 && base.Bounds() == half && top.Bounds() == half {
			writeBlendXRGB8888(d.halfBuf, base.Pix, top.Pix, d.halfW, d.halfH, int(d.halfPitch), p)
			return d.commitSized(d.halfFB, d.halfW, d.halfH)
		}
		if base.Bounds() != want || top.Bounds() != want {
			return fmt.Errorf("drm: transition images %v/%v, want %v or %v", base.Bounds().Size(), top.Bounds().Size(), want.Size(), half.Size())
		}
		writeBlendXRGB8888(d.buf, base.Pix, top.Pix, d.width, d.height, int(d.dumbPitch), p)
		return d.commitSized(d.fbID, d.width, d.height)
	case "slide":
		if base.Bounds() != want || top.Bounds() != want {
			return fmt.Errorf("drm: transition images %v/%v, want %v", base.Bounds().Size(), top.Bounds().Size(), want.Size())
		}
		writeSlideXRGB8888(d.buf, base.Pix, top.Pix, d.width, d.height, int(d.dumbPitch), int(float64(d.width)*t))
		return d.commitSized(d.fbID, d.width, d.height)
	default:
		return fmt.Errorf("drm: no fused transition for %q", mode)
	}
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

// commit performs the atomic commit at full panel size.
func (d *Driver) commit() error {
	return d.commitSized(d.fbID, d.width, d.height)
}

// commitSized commits fbID filling the panel, with the source
// rectangle sized srcW x srcH pixels (half for the upscaled
// crossfade path, panel size otherwise).
func (d *Driver) commitSized(fbID uint32, srcW, srcH int) error {

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

	// Plane -> framebuffer, panel-sized destination; source is the
	// (possibly half-res) buffer the caller filled.
	w, h := uint64(d.width), uint64(d.height)
	addProp(plane["FB_ID"], uint64(fbID))
	addProp(plane["CRTC_ID"], uint64(d.crtcID))
	if _, ok := plane["CRTC_X"]; ok {
		addProp(plane["CRTC_X"], 0)
		addProp(plane["CRTC_Y"], 0)
	}
	addProp(plane["CRTC_W"], w)
	addProp(plane["CRTC_H"], h)
	addProp(plane["SRC_W"], uint64(srcW)<<16)
	addProp(plane["SRC_H"], uint64(srcH)<<16)

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

// Close releases the dumb buffers, framebuffers and card handle.
func (d *Driver) Close() error {
	if d.halfFB != 0 {
		d.rmFB(d.halfFB)
		d.halfFB = 0
	}
	if len(d.halfBuf) > 0 {
		syscall.Munmap(d.halfBuf)
		d.halfBuf = nil
	}
	if d.halfHandle != 0 {
		var dd destroyDumb
		dd.handle = d.halfHandle
		ioctl(d.file.Fd(), reqModeDestroyDumb, unsafe.Pointer(&dd))
		d.halfHandle = 0
	}
	if d.fbID != 0 {
		d.rmFB(d.fbID)
		d.fbID = 0
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
