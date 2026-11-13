//go:build linux

// DRM/KMS ioctls for the frame display driver, in pure Go (ADR-0006,
// ADR-0002).
//
// Every ioctl number and struct layout in this file was verified
// against the kernel uapi headers (v5.10, v6.6 and master — the layout
// is stable across all three): include/uapi/drm/drm.h and
// include/uapi/drm/drm_mode.h. layout_test.go asserts the mirrored Go
// struct sizes at build time so any future drift fails CI instead of a
// panel.
package drm

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl request encodings: _IOC(dir, 'd', nr, size) per asm-generic
// ioctl.h. Values are the exact decimal numbers the kernel expects
// (derived with a C probe over the v5.10 uapi headers).
const (
	reqSetClientCap       = 0x4010640d // IOW  ('d', 0x0d, 16)  DRM_IOCTL_SET_CLIENT_CAP
	reqModeGetResources   = 0xc04064a0 // IOWR ('d', 0xa0, 64)
	reqModeGetEncoder     = 0xc01464a6 // IOWR ('d', 0xa6, 20)
	reqModeGetConnector   = 0xc05064a7 // IOWR ('d', 0xa7, 80)
	reqModeGetProperty    = 0xc04064aa // IOWR ('d', 0xaa, 64)
	reqModeGetPropBlob    = 0xc01064ac // IOWR ('d', 0xac, 16)
	reqModeRMFB           = 0xc00464af // IOWR ('d', 0xaf, 4)
	reqModeCreateDumb     = 0xc02064b2 // IOWR ('d', 0xb2, 32)
	reqModeMapDumb        = 0xc01064b3 // IOWR ('d', 0xb3, 16)
	reqModeDestroyDumb    = 0xc00464b4 // IOWR ('d', 0xb4, 4)
	reqModeGetPlaneRes    = 0xc01064b5 // IOWR ('d', 0xb5, 16)
	reqModeGetPlane       = 0xc02064b6 // IOWR ('d', 0xb6, 32)
	reqModeAddFB2         = 0xc06864b8 // IOWR ('d', 0xb8, 104)
	reqModeAtomic         = 0xc03864bc // IOWR ('d', 0xbc, 56)
	reqModeCreatePropBlob = 0xc01064bd // IOWR ('d', 0xbd, 16)
)

// Client capabilities (include/uapi/drm/drm.h).
const clientCapAtomic = 3

// Connector status (enum drm_connector_status).
const (
	connectorStatusUnknown      = 0
	connectorStatusConnected    = 1
	connectorStatusDisconnected = 2
)

// Mode type flags (include/uapi/drm/drm_mode.h).
const (
	modeTypePreferred = 1 << 3
	modeTypeUserDef   = 1 << 5
)

// Atomic commit flags.
const (
	atomicTestOnly     = 0x0100
	atomicNonBlock     = 0x0200
	atomicAllowModeSet = 0x0400
)

// CRTC rotation bits (DRM_MODE_ROTATE_*).
const (
	rotateNormal = 1 << 0
	rotate90     = 1 << 1
	rotate180    = 1 << 2
	rotate270    = 1 << 3
)

// FourCC XRGB8888 = fourcc('X','R','2','4') little-endian packed, as
// defined in drm_fourcc.h.
const formatXRGB8888 = 0x34325258

// Object type markers (DRM_MODE_OBJECT_*); only needed for diagnostics
// since the atomic ioctl resolves objects of ANY type by id.
const (
	objCrtc      = 0xcccccccc
	objConnector = 0xc0c0c0c0
	objPlane     = 0xeeeeeeee
)

// --- struct mirrors ---------------------------------------------------
// Field order and types mirror the kernel uapi structs exactly.
// layout_test.go pins their sizes.

// struct drm_set_client_cap (16 bytes)
type setClientCap struct {
	capability uint64
	value      uint64
}

// struct drm_mode_card_res (64 bytes). The *_ptr fields point at
// user-space arrays whose capacities are the count_* fields; the
// kernel fills the arrays and returns actual counts.
type cardRes struct {
	fbIDPtr         uint64
	crtcIDPtr       uint64
	connectorIDPtr  uint64
	encoderIDPtr    uint64
	countFBs        uint32
	countCrtcs      uint32
	countConnectors uint32
	countEncoders   uint32
	minWidth        uint32
	maxWidth        uint32
	minHeight       uint32
	maxHeight       uint32
}

// struct drm_mode_get_encoder (20 bytes)
type getEncoder struct {
	encoderID      uint32
	encoderType    uint32
	crtcID         uint32
	possibleCrtcs  uint32
	possibleClones uint32
}

// struct drm_mode_get_connector (80 bytes)
type getConnector struct {
	encodersPtr     uint64
	modesPtr        uint64
	propsPtr        uint64
	propValuesPtr   uint64
	countModes      uint32
	countProps      uint32
	countEncoders   uint32
	encoderID       uint32
	connectorID     uint32
	connectorType   uint32
	connectorTypeID uint32
	connection      uint32
	mmWidth         uint32
	mmHeight        uint32
	subpixel        uint32
	pad             uint32
}

// struct drm_mode_modeinfo (68 bytes). The layout is version-stable:
// the MODE_ID blob must contain exactly this struct, and the kernel
// validates its length against its own copy.
type modeInfo struct {
	clock      uint32
	hdisplay   uint16
	hsyncStart uint16
	hsyncEnd   uint16
	htotal     uint16
	hskew      uint16
	vdisplay   uint16
	vsyncStart uint16
	vsyncEnd   uint16
	vtotal     uint16
	vscan      uint16
	vrefresh   uint32
	flags      uint32
	mtype      uint32
	name       [32]byte
}

// struct drm_mode_get_plane_res (16 bytes)
type getPlaneRes struct {
	planeIDPtr  uint64
	countPlanes uint32
	_pad        uint32
}

// struct drm_mode_get_plane (32 bytes)
type getPlane struct {
	planeID          uint32
	crtcID           uint32
	fbID             uint32
	possibleCrtcs    uint32
	gammaSize        uint32
	countFormatTypes uint32
	formatTypePtr    uint64
}

// struct drm_mode_get_property (64 bytes)
type getProperty struct {
	valuesPtr      uint64
	enumBlobPtr    uint64
	propID         uint32
	flags          uint32
	name           [32]byte
	countValues    uint32
	countEnumBlobs uint32
}

// struct drm_mode_create_blob (16 bytes)
type createBlob struct {
	data   uint64
	length uint32
	blobID uint32
}

// struct drm_mode_fb_cmd2 (104 bytes)
type fbCmd2 struct {
	fbID        uint32
	width       uint32
	height      uint32
	pixelFormat uint32
	flags       uint32
	handles     [4]uint32
	pitches     [4]uint32
	offsets     [4]uint32
	_pad        uint32
	modifier    [4]uint64
}

// struct drm_mode_atomic (56 bytes). Objects and their property
// counts/values live in separate user-space arrays: objs (u32 per
// object), counts (u32 per object), props (u32 flattened), values
// (u64 flattened; blob properties carry the BLOB ID as the value).
type atomicCmd struct {
	flags         uint32
	countObjs     uint32
	objsPtr       uint64
	countPropsPtr uint64
	propsPtr      uint64
	propValuesPtr uint64
	reserved      uint64
	userData      uint64
}

// struct drm_mode_create_dumb (32 bytes)
type createDumb struct {
	height uint32
	width  uint32
	bpp    uint32
	flags  uint32
	handle uint32
	pitch  uint32
	size   uint64
}

// struct drm_mode_map_dumb (16 bytes)
type mapDumb struct {
	handle uint32
	_pad   uint32
	offset uint64
}

// struct drm_mode_destroy_dumb (4 bytes)
type destroyDumb struct {
	handle uint32
}

// ioctl issues a single DRM ioctl.
func ioctl(fd int, req uint, p unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(p))
	if errno != 0 {
		return errno
	}
	return nil
}
