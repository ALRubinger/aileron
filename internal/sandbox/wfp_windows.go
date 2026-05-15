//go:build windows

// Package-level WFP (Windows Filtering Platform) glue for the
// Windows v2 spawn sandbox. WFP is the kernel-level packet
// filtering framework the runtime uses to deny outbound traffic
// from a wrapped CLI's process to any destination other than
// the daemon's per-spawn CONNECT proxy on loopback.
//
// `golang.org/x/sys/windows` v0.44.0 ships no WFP wrappers, so
// this file loads `fwpuclnt.dll` via [windows.NewLazySystemDLL]
// and calls the C-level entry points directly via Syscall. The
// struct layouts match the Win32 SDK headers (`fwpmtypes.h` and
// `fwptypes.h`); changes to those headers between SDK versions
// could in principle break alignment, but the relevant types
// are stable across the Windows 7+ era.
//
// Scope of this file (Windows v2 first cut):
//
//   - [openWFPEngine] / [closeWFPEngine] manage a session-scoped
//     engine handle. Filters added via the session are
//     automatically removed when the handle closes, so daemon
//     crashes don't leak persistent rules.
//   - Filter add / delete and condition construction land in
//     follow-up work once the engine-open path is validated on
//     the GHA `windows-latest` runner. The probe is the first
//     real signal that WFP is usable without elevated privileges
//     in CI.

package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modFwpuclnt                    = windows.NewLazySystemDLL("fwpuclnt.dll")
	procFwpmEngineOpen0            = modFwpuclnt.NewProc("FwpmEngineOpen0")
	procFwpmEngineClose0           = modFwpuclnt.NewProc("FwpmEngineClose0")
	procFwpmFilterAdd0             = modFwpuclnt.NewProc("FwpmFilterAdd0")
	procFwpmFilterDeleteById0      = modFwpuclnt.NewProc("FwpmFilterDeleteById0")
	procFwpmGetAppIdFromFileName0  = modFwpuclnt.NewProc("FwpmGetAppIdFromFileName0")
	procFwpmFreeMemory0            = modFwpuclnt.NewProc("FwpmFreeMemory0")
)

// guid is the Win32 GUID layout. Alignment 4 (matches Data1 size).
// Total size 16 bytes. Keep field names lowercase to mirror the
// SDK header for readability; Go doesn't care about the casing.
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// fwpByteBlob mirrors FWP_BYTE_BLOB. Variable-length byte data
// passed by pointer + size. Used for the ALE_APP_ID condition's
// value (the kernel-computed app identifier returned by
// FwpmGetAppIdFromFileName0). Total 16 bytes on x64.
type fwpByteBlob struct {
	Size uint32
	_    [4]byte // padding to align Data on 8 bytes
	Data *byte
}

// fwpmDisplayData0 mirrors FWPM_DISPLAY_DATA0. Both fields are
// UTF-16 strings; nil is allowed. 16 bytes on x64.
type fwpmDisplayData0 struct {
	Name        *uint16
	Description *uint16
}

// fwpValue0 mirrors FWP_VALUE0. The Type field is the FWP_DATA_TYPE
// enum (4 bytes); the Value field is a pointer-sized union holding
// one of many small inline values or pointers to larger ones. On
// x64 the union is 8 bytes, with 4 bytes of padding between Type
// and Value. Total 16 bytes.
//
// For the FWP_UINT16, FWP_UINT32, etc. cases the value is inline
// in the low bits of the union. For FWP_BYTE_BLOB_TYPE the value
// is a pointer to a FWP_BYTE_BLOB. For FWP_V4_ADDR_MASK the value
// is a pointer to a FWP_V4_ADDR_AND_MASK.
type fwpValue0 struct {
	Type  uint32
	_     [4]byte
	Value uintptr
}

// fwpV4AddrAndMask mirrors FWP_V4_ADDR_AND_MASK. Used by
// FWPM_CONDITION_IP_REMOTE_ADDRESS for IPv4 destinations. Both
// fields are stored in host byte order in the API (the kernel
// converts to network byte order internally).
type fwpV4AddrAndMask struct {
	Addr uint32
	Mask uint32
}

// fwpmFilterCondition0 mirrors FWPM_FILTER_CONDITION0.
//
//	GUID fieldKey;       // 16 bytes, align 4
//	FWP_MATCH_TYPE type; // 4 bytes
//	FWP_CONDITION_VALUE0 value; // same as FWP_VALUE0, align 8
//
// Go inserts 4 bytes of padding before Value to satisfy fwpValue0's
// 8-byte alignment. Total 40 bytes.
type fwpmFilterCondition0 struct {
	FieldKey  guid
	MatchType uint32
	_         [4]byte
	Value     fwpValue0
}

// fwpmAction0 mirrors FWPM_ACTION0:
//
//	FWP_ACTION_TYPE type;   // 4 bytes
//	union {
//	    GUID filterType;     // 16 bytes
//	    GUID calloutKey;
//	};
//
// For block/permit actions we set Type to FWP_ACTION_BLOCK/PERMIT
// and leave the GUID zero. Total 20 bytes, alignment 4.
type fwpmAction0 struct {
	Type uint32
	GUID guid
}

// fwpmFilter0 mirrors FWPM_FILTER0. This is the big one. Field
// offsets on x64 are:
//
//	  0  filterKey         GUID                 (16 bytes, align 4)
//	 16  displayData       FWPM_DISPLAY_DATA0   (16 bytes, align 8)
//	 32  flags             UINT32               (4 bytes)
//	 36  (padding)                              (4 bytes)
//	 40  providerKey       *GUID                (8 bytes)
//	 48  providerData      FWP_BYTE_BLOB        (16 bytes)
//	 64  layerKey          GUID                 (16 bytes)
//	 80  subLayerKey       GUID                 (16 bytes)
//	 96  weight            FWP_VALUE0           (16 bytes)
//	112  numFilterConditions UINT32             (4 bytes)
//	116  (padding)                              (4 bytes)
//	120  filterCondition   *FWPM_FILTER_CONDITION0 (8 bytes)
//	128  action            FWPM_ACTION0         (20 bytes)
//	148  (padding)                              (4 bytes)
//	152  context union     UINT64 / *GUID       (16 bytes; size of the larger union member, GUID)
//	168  reserved          *GUID                (8 bytes)
//	176  filterId          UINT64               (8 bytes)
//	184  effectiveWeight   FWP_VALUE0           (16 bytes)
//	-----
//	200 total
//
// The context union is interesting: the C definition is
//
//	union {
//	    UINT64 rawContext;
//	    GUID providerContextKey;
//	};
//
// Size = sizeof(GUID) = 16. We only use rawContext = 0, but the
// field has to occupy 16 bytes so the layout matches.
type fwpmFilter0 struct {
	FilterKey           guid
	DisplayData         fwpmDisplayData0
	Flags               uint32
	_                   [4]byte
	ProviderKey         *guid
	ProviderData        fwpByteBlob
	LayerKey            guid
	SubLayerKey         guid
	Weight              fwpValue0
	NumFilterConditions uint32
	_                   [4]byte
	FilterCondition     *fwpmFilterCondition0
	Action              fwpmAction0
	_                   [4]byte
	ContextUnion        [16]byte // raw bytes for the rawContext/providerContextKey union
	Reserved            *guid
	FilterID            uint64
	EffectiveWeight     fwpValue0
}

// FWP_DATA_TYPE values used by the runtime. The full enum has
// ~30 members; the runtime only needs these. Values from
// `fwptypes.h` in the Windows SDK.
//
// Critically, FWP_BYTE_BLOB_TYPE is 12 (not 9 — 9 is FWP_FLOAT),
// and FWP_V4_ADDR_MASK is 256 (FWP_SINGLE_DATA_TYPE_MAX + 1, the
// enum jumps over a 0xff reserved range for "composite" types).
// Getting these wrong yields FWP_E_TYPE_MISMATCH (0x80320027) at
// filter-add time, which is how the runtime caught the original
// values in CI.
const (
	fwpEmpty        uint32 = 0
	fwpUint16       uint32 = 2
	fwpUint32       uint32 = 3
	fwpUint64       uint32 = 4
	fwpByteBlobType uint32 = 12
	fwpV4AddrMask   uint32 = 256
)

// FWP_ACTION_TYPE values.
const (
	fwpActionBlock  uint32 = 0x00001001
	fwpActionPermit uint32 = 0x00001000
)

// FWP_MATCH_TYPE values used by the runtime.
const (
	fwpMatchEqual uint32 = 0
)

// Filter weight constants. FWP_EMPTY in a weight field tells the
// kernel "auto-assign weight"; explicit weights are used when the
// runtime needs to order filters deterministically (permit ahead
// of block at the same layer).
const fwpWeightAutoAssign uint32 = 0

// Well-known WFP layer and condition GUIDs. Values come from
// fwpmu.h in the Windows SDK. The runtime uses only the ALE
// connect layers (V4 and V6) — these are evaluated when a process
// calls connect() on an outbound socket, which is the relevant
// hook for HTTPS_PROXY-mediated traffic.
var (
	// FWPM_LAYER_ALE_AUTH_CONNECT_V4 = c38d57d1-05a7-4c33-904f-7fbceee60e82
	fwpmLayerALEAuthConnectV4 = guid{
		Data1: 0xc38d57d1, Data2: 0x05a7, Data3: 0x4c33,
		Data4: [8]byte{0x90, 0x4f, 0x7f, 0xbc, 0xee, 0xe6, 0x0e, 0x82},
	}
	// FWPM_LAYER_ALE_AUTH_CONNECT_V6 = 4a72393b-319f-44bc-84c3-ba54dcb3b6b4
	fwpmLayerALEAuthConnectV6 = guid{
		Data1: 0x4a72393b, Data2: 0x319f, Data3: 0x44bc,
		Data4: [8]byte{0x84, 0xc3, 0xba, 0x54, 0xdc, 0xb3, 0xb6, 0xb4},
	}
	// FWPM_CONDITION_ALE_APP_ID = d78e1e87-8644-4ea5-9437-d809ecefc971
	fwpmConditionALEAppID = guid{
		Data1: 0xd78e1e87, Data2: 0x8644, Data3: 0x4ea5,
		Data4: [8]byte{0x94, 0x37, 0xd8, 0x09, 0xec, 0xef, 0xc9, 0x71},
	}
	// FWPM_CONDITION_IP_REMOTE_ADDRESS = b235ae9a-1d64-49b8-a44c-5ff3d9095045
	fwpmConditionIPRemoteAddress = guid{
		Data1: 0xb235ae9a, Data2: 0x1d64, Data3: 0x49b8,
		Data4: [8]byte{0xa4, 0x4c, 0x5f, 0xf3, 0xd9, 0x09, 0x50, 0x45},
	}
	// FWPM_CONDITION_IP_REMOTE_PORT = c35a604d-d22b-4e1a-91b4-68f674ee674b
	fwpmConditionIPRemotePort = guid{
		Data1: 0xc35a604d, Data2: 0xd22b, Data3: 0x4e1a,
		Data4: [8]byte{0x91, 0xb4, 0x68, 0xf6, 0x74, 0xee, 0x67, 0x4b},
	}
)

// RPC authentication service constants from `rpcdce.h`. WFP's
// engine-open accepts these for the RPC channel it sets up to
// the local filter engine; `RPC_C_AUTHN_WINNT` is the documented
// default for local connections.
const (
	rpcCAuthnDefault uint32 = 0xFFFFFFFF
	rpcCAuthnWinNT   uint32 = 10
)

// openWFPEngine opens a default-session handle to the local
// Windows Filter Engine. Returns the engine handle (caller must
// close via [closeWFPEngine]) or an error if the open failed.
//
// Failure modes worth distinguishing in the caller:
//
//   - `ERROR_ACCESS_DENIED` (0x5): the daemon process lacks
//     `SE_DEBUG_NAME` or membership in the BUILTIN\Administrators
//     group required for filter-engine access on most Windows
//     SKUs. Surface as wrapped `ErrSpawnUnavailable` so the
//     audit row records the structured class.
//   - `RPC_S_SERVER_UNAVAILABLE` (0x6BA): Base Filtering Engine
//     service is not running. Same surface.
//
// Any other error indicates a misconfiguration we want loud.
func openWFPEngine() (windows.Handle, error) {
	var engine windows.Handle
	r1, _, _ := procFwpmEngineOpen0.Call(
		0, // serverName: NULL for local
		uintptr(rpcCAuthnWinNT),
		0, // authIdentity: NULL
		0, // session: NULL (default)
		uintptr(unsafe.Pointer(&engine)),
	)
	if r1 != 0 {
		return 0, fmt.Errorf("FwpmEngineOpen0: error code 0x%x", r1)
	}
	return engine, nil
}

// getAppID returns the kernel-computed APP_ID for `binaryPath`,
// the opaque blob the WFP `ALE_APP_ID` condition matches against.
// The returned blob is allocated by `FwpmGetAppIdFromFileName0`
// and must be released via [freeWFPMemory] when no longer needed.
//
// The blob's contents are an internal kernel hash; the runtime
// passes it straight back into a filter condition without
// inspecting the bytes.
func getAppID(binaryPath string) (*fwpByteBlob, error) {
	pathPtr, err := windows.UTF16PtrFromString(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("UTF16PtrFromString(%s): %w", binaryPath, err)
	}
	var blob *fwpByteBlob
	r1, _, _ := procFwpmGetAppIdFromFileName0.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&blob)),
	)
	if r1 != 0 {
		return nil, fmt.Errorf("FwpmGetAppIdFromFileName0(%s): error code 0x%x", binaryPath, r1)
	}
	return blob, nil
}

// freeWFPMemory releases memory allocated by the WFP API (e.g.,
// the app-ID blob from [getAppID]). The WFP allocator is
// separate from Go's heap; the kernel manages it via
// FwpmFreeMemory0.
func freeWFPMemory(p unsafe.Pointer) {
	if p == nil {
		return
	}
	_, _, _ = procFwpmFreeMemory0.Call(uintptr(unsafe.Pointer(&p)))
}

// addBlockOutboundForApp installs two WFP filters (V4 and V6) at
// the ALE_AUTH_CONNECT layer that deny outbound TCP connect calls
// made by any process whose binary path matches `appID`. Returns
// the filter IDs the caller will pass to [deleteFilter] on
// cleanup.
//
// The runtime pairs this with [addPermitLoopbackForApp] at a
// higher weight to carve out the proxy endpoint. WFP's filter
// arbitration picks the highest-weight matching filter, so the
// permit beats the deny when the destination is the proxy.
func addBlockOutboundForApp(engine windows.Handle, appID *fwpByteBlob, displayName string) ([]uint64, error) {
	nameUTF16, _ := windows.UTF16PtrFromString(displayName)
	descUTF16, _ := windows.UTF16PtrFromString("aileron spawn-sandbox deny outbound")

	cond := fwpmFilterCondition0{
		FieldKey:  fwpmConditionALEAppID,
		MatchType: fwpMatchEqual,
		Value: fwpValue0{
			Type:  fwpByteBlobType,
			Value: uintptr(unsafe.Pointer(appID)),
		},
	}
	ids := make([]uint64, 0, 2)
	for _, layer := range []guid{fwpmLayerALEAuthConnectV4, fwpmLayerALEAuthConnectV6} {
		f := fwpmFilter0{
			DisplayData: fwpmDisplayData0{Name: nameUTF16, Description: descUTF16},
			LayerKey:    layer,
			Weight:      fwpValue0{Type: fwpEmpty}, // auto-assign
			Action:      fwpmAction0{Type: fwpActionBlock},
			NumFilterConditions: 1,
			FilterCondition:     &cond,
		}
		var id uint64
		r1, _, _ := procFwpmFilterAdd0.Call(
			uintptr(engine),
			uintptr(unsafe.Pointer(&f)),
			0, // security descriptor: NULL
			uintptr(unsafe.Pointer(&id)),
		)
		if r1 != 0 {
			// Roll back filters already added so we don't leak.
			for _, prev := range ids {
				_ = deleteFilter(engine, prev)
			}
			return nil, fmt.Errorf("FwpmFilterAdd0 block layer %x: error code 0x%x", layer.Data1, r1)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// addPermitLoopbackForApp installs a V4 filter at higher weight
// than the deny filter from [addBlockOutboundForApp]: when the
// wrapped binary connects to `127.0.0.1:port`, this filter
// permits it. All other destinations fall through to the deny.
//
// The runtime only adds a V4 permit (loopback proxy is bound on
// 127.0.0.1); the V6 deny continues to apply, blocking any
// attempted IPv6 egress.
func addPermitLoopbackForApp(engine windows.Handle, appID *fwpByteBlob, port uint16, displayName string) (uint64, error) {
	nameUTF16, _ := windows.UTF16PtrFromString(displayName)
	descUTF16, _ := windows.UTF16PtrFromString("aileron spawn-sandbox permit loopback to proxy")

	// 127.0.0.1/32 in host byte order.
	addrMask := fwpV4AddrAndMask{
		Addr: 0x7F000001, // 127.0.0.1
		Mask: 0xFFFFFFFF,
	}
	conds := [3]fwpmFilterCondition0{
		{
			FieldKey:  fwpmConditionALEAppID,
			MatchType: fwpMatchEqual,
			Value:     fwpValue0{Type: fwpByteBlobType, Value: uintptr(unsafe.Pointer(appID))},
		},
		{
			FieldKey:  fwpmConditionIPRemoteAddress,
			MatchType: fwpMatchEqual,
			Value:     fwpValue0{Type: fwpV4AddrMask, Value: uintptr(unsafe.Pointer(&addrMask))},
		},
		{
			FieldKey:  fwpmConditionIPRemotePort,
			MatchType: fwpMatchEqual,
			Value:     fwpValue0{Type: fwpUint16, Value: uintptr(port)},
		},
	}
	// Higher weight ensures the permit wins arbitration against
	// the auto-weighted deny. FWP filter weights MUST be tagged
	// as FWP_UINT64 (not UINT32); the kernel rejects other
	// numeric types with FWP_E_INVALID_WEIGHT (0x80320025).
	// The runtime uses 0xFFFFFFFFFFFFFFFE so user-added rules
	// (if any) at MAX can still preempt.
	weight := uint64(0xFFFFFFFFFFFFFFFE)
	f := fwpmFilter0{
		DisplayData:         fwpmDisplayData0{Name: nameUTF16, Description: descUTF16},
		LayerKey:            fwpmLayerALEAuthConnectV4,
		Weight:              fwpValue0{Type: fwpUint64, Value: uintptr(unsafe.Pointer(&weight))},
		Action:              fwpmAction0{Type: fwpActionPermit},
		NumFilterConditions: 3,
		FilterCondition:     &conds[0],
	}
	var id uint64
	r1, _, _ := procFwpmFilterAdd0.Call(
		uintptr(engine),
		uintptr(unsafe.Pointer(&f)),
		0,
		uintptr(unsafe.Pointer(&id)),
	)
	if r1 != 0 {
		return 0, fmt.Errorf("FwpmFilterAdd0 permit: error code 0x%x", r1)
	}
	return id, nil
}

// deleteFilter removes a filter installed via [addBlockOutboundForApp]
// or [addPermitLoopbackForApp]. Idempotent: deleting an already-
// removed filter returns a non-fatal error code the caller logs
// and ignores. Cleanup paths invoke this best-effort; closing the
// engine handle also removes any filters scoped to the session.
func deleteFilter(engine windows.Handle, id uint64) error {
	r1, _, _ := procFwpmFilterDeleteById0.Call(
		uintptr(engine),
		uintptr(id),
	)
	if r1 != 0 {
		return fmt.Errorf("FwpmFilterDeleteById0(%d): error code 0x%x", id, r1)
	}
	return nil
}

// closeWFPEngine releases a handle returned by [openWFPEngine].
// Must be called exactly once per opened handle: the Win32 API
// does not validate that the handle is still live, and a
// double-close typically crashes the host process with an
// access violation rather than returning an error. Production
// callers pair Open/Close in a defer.
func closeWFPEngine(engine windows.Handle) error {
	r1, _, _ := procFwpmEngineClose0.Call(uintptr(engine))
	if r1 != 0 {
		return fmt.Errorf("FwpmEngineClose0: error code 0x%x", r1)
	}
	return nil
}
