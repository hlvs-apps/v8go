package v8go

// #include <stdlib.h>
// #include "snapshot.h"
import "C"

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// SnapshotScript runs in order in a creator-owned isolate, without Go callbacks.
// CaptureGlobals names own global properties to retain privately and delete before
// the next microtask checkpoint or script. Capture indexes follow script/name order.
type SnapshotScript struct {
	Source, Origin string
	CachedData     *CompilerCachedData
	CaptureGlobals []string
	// ValidateCaptured invokes previously captured functions after this script and
	// its microtask checkpoint. Validators must be synchronous: a thrown exception
	// or Promise return fails snapshot creation.
	ValidateCaptured []int
}

// Snapshot owns an immutable, trusted build artifact. Never parse snapshots from
// untrusted sources: integrity checks are not authentication or a V8 sandbox.
type (
	Snapshot struct {
		envelope snapshotEnvelope
		// native is shared by pointer so a copied Snapshot value still counts
		// the same isolates against the same C copy; see acquireNative.
		native *snapshotNative
	}
	// snapshotNative is the single C copy of a snapshot's blob shared by every
	// live isolate restored from it.
	snapshotNative struct {
		mu    sync.Mutex
		blob  C.SnapshotBlobPtr
		users int
	}
	snapshotEnvelope struct {
		Format     string
		Identity   string
		Digest     string
		Captures   int
		DataLength int
		Data       []byte `json:"-"`
	}
)

const (
	snapshotMagic       = "V8GOSNP2"
	snapshotPrefixSize  = len(snapshotMagic) + 4
	snapshotHeaderLimit = 64 << 10
	snapshotDataLimit   = 256 << 20
)

// SnapshotCompatibilityError reports rejection before native deserialization.
type SnapshotCompatibilityError struct{ Reason string }

func (e *SnapshotCompatibilityError) Error() string { return "v8go: snapshot rejected: " + e.Reason }

var snapshotFlags struct {
	sync.Mutex
	values []string
}

// snapshotUnmappedPlatform replaces the native archive identity on a GOOS/GOARCH
// that scripts/snapshot-identity.py has no archive for. Artifacts are refused
// there instead of silently carrying a weaker identity.
const snapshotUnmappedPlatform = "unmapped-platform"

func snapshotNativeArchiveIdentity() string {
	if id, ok := snapshotNativeIdentity[runtime.GOOS+"_"+runtime.GOARCH]; ok {
		return id
	}
	return snapshotUnmappedPlatform
}

func snapshotPlatformError() error {
	if snapshotNativeArchiveIdentity() == snapshotUnmappedPlatform {
		return &SnapshotCompatibilityError{"no native archive identity for " + runtime.GOOS + "/" + runtime.GOARCH}
	}
	return nil
}

// CachedDataVersionTag identifies V8's current code-cache version and flags.
func CachedDataVersionTag() uint32 {
	initializeIfNecessary()
	return uint32(C.SnapshotCacheVersionTag())
}

// StartupIdentity identifies the native archives, wrapper snapshot ABI, platform,
// V8 cache compatibility tag and configured flags for trusted startup artifacts.
func StartupIdentity() string {
	snapshotFlags.Lock()
	defer snapshotFlags.Unlock()
	return fmt.Sprintf("v8go-snapshot-2/%s/%s/%s/%s/%d/%s", Version(), runtime.GOOS, runtime.GOARCH, snapshotNativeArchiveIdentity(), CachedDataVersionTag(), strings.Join(snapshotFlags.values, " "))
}

// ContextIndex returns the additional application context index.
func (s *Snapshot) ContextIndex() int { return 0 }

// CaptureCount returns the number of privately retained context values.
func (s *Snapshot) CaptureCount() int { return s.envelope.Captures }

// Bytes returns an independent copy of the versioned snapshot envelope.
func (s *Snapshot) Bytes() []byte {
	header, _ := json.Marshal(s.envelope)
	data := make([]byte, snapshotPrefixSize+len(header)+len(s.envelope.Data))
	copy(data, snapshotMagic)
	binary.LittleEndian.PutUint32(data[len(snapshotMagic):snapshotPrefixSize], uint32(len(header)))
	copy(data[snapshotPrefixSize:], header)
	copy(data[snapshotPrefixSize+len(header):], s.envelope.Data)
	return data
}

// ParseSnapshot validates an envelope from a trusted build before native loading.
// Callers must authenticate externally supplied artifacts before calling this.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	reject := func(s string) (*Snapshot, error) { return nil, &SnapshotCompatibilityError{s} }
	if len(data) < snapshotPrefixSize || len(data) > snapshotPrefixSize+snapshotHeaderLimit+snapshotDataLimit {
		return reject("invalid envelope size")
	}
	if string(data[:len(snapshotMagic)]) != snapshotMagic {
		return reject("unsupported envelope format")
	}
	if err := snapshotPlatformError(); err != nil {
		return nil, err
	}
	// Bound the raw uint32 before converting: int(uint32) wraps negative on
	// 32-bit targets and would slip past every check below.
	rawHeaderLength := binary.LittleEndian.Uint32(data[len(snapshotMagic):snapshotPrefixSize])
	if rawHeaderLength == 0 || rawHeaderLength > snapshotHeaderLimit {
		return reject("invalid metadata size")
	}
	headerLength := int(rawHeaderLength)
	if headerLength > len(data)-snapshotPrefixSize {
		return reject("invalid metadata size")
	}
	bodyOffset := snapshotPrefixSize + headerLength
	var env snapshotEnvelope
	if err := json.Unmarshal(data[snapshotPrefixSize:bodyOffset], &env); err != nil {
		return reject("invalid envelope metadata")
	}
	if env.Format != "v8go-snapshot-2" || env.Identity != StartupIdentity() {
		return reject("build, platform, or flags differ")
	}
	if env.DataLength < 64 || env.DataLength > snapshotDataLimit || env.DataLength != len(data)-bodyOffset || env.Captures < 0 || env.Captures > 65536 {
		return reject("invalid snapshot shape")
	}
	sum := sha256.Sum256(data[bodyOffset:])
	if env.Digest != hex.EncodeToString(sum[:]) {
		return reject("digest differs")
	}
	// The caller may reuse or mutate its envelope immediately after parsing.
	env.Data = bytes.Clone(data[bodyOffset:])
	return &Snapshot{envelope: env, native: &snapshotNative{}}, nil
}

// CreateSnapshot runs synchronously in one native call and disposes the creator
// before returning. Run it in a deadline-bounded helper process when scripts may
// fail to terminate. It has no Go callbacks and does not inherit ordinary isolates.
func CreateSnapshot(scripts []SnapshotScript) (*Snapshot, error) {
	initializeIfNecessary()
	if err := snapshotPlatformError(); err != nil {
		return nil, err
	}
	if len(scripts) > 65536 {
		return nil, errors.New("v8go: too many snapshot scripts")
	}
	mem := C.calloc(C.size_t(len(scripts)+1), C.size_t(C.sizeof_SnapshotScriptInput))
	defer C.free(mem)
	inputs := unsafe.Slice((*C.SnapshotScriptInput)(mem), len(scripts))
	captures := 0
	for i, s := range scripts {
		if strings.ContainsRune(s.Source, 0) || strings.ContainsRune(s.Origin, 0) || len(s.Source) > 256<<20 {
			return nil, errors.New("v8go: invalid snapshot script")
		}
		inputs[i].source = C.CString(s.Source)
		defer C.free(unsafe.Pointer(inputs[i].source))
		inputs[i].origin = C.CString(s.Origin)
		defer C.free(unsafe.Pointer(inputs[i].origin))
		if s.CachedData != nil {
			if len(s.CachedData.Bytes) == 0 || len(s.CachedData.Bytes) > 256<<20 {
				return nil, errors.New("v8go: invalid snapshot code cache")
			}
			inputs[i].cache = (*C.uchar)(C.CBytes(s.CachedData.Bytes))
			defer C.free(unsafe.Pointer(inputs[i].cache))
			inputs[i].cache_len = C.int(len(s.CachedData.Bytes))
		}
		if len(s.ValidateCaptured) > 65536 {
			return nil, errors.New("v8go: too many snapshot validators")
		}
		validatorMem := C.calloc(C.size_t(len(s.ValidateCaptured)+1), C.size_t(unsafe.Sizeof(C.int(0))))
		defer C.free(validatorMem)
		inputs[i].validators = (*C.int)(validatorMem)
		inputs[i].validator_count = C.int(len(s.ValidateCaptured))
		validators := unsafe.Slice((*C.int)(validatorMem), len(s.ValidateCaptured))
		for j, index := range s.ValidateCaptured {
			if index < 0 || index >= captures+len(s.CaptureGlobals) {
				return nil, errors.New("v8go: invalid snapshot validator index")
			}
			validators[j] = C.int(index)
		}
		captures += len(s.CaptureGlobals)
		if captures > 65536 {
			return nil, errors.New("v8go: too many snapshot captures")
		}
		namesMem := C.calloc(C.size_t(len(s.CaptureGlobals)+1), C.size_t(unsafe.Sizeof(uintptr(0))))
		defer C.free(namesMem)
		inputs[i].captures = (**C.char)(namesMem)
		inputs[i].capture_count = C.int(len(s.CaptureGlobals))
		names := unsafe.Slice((**C.char)(namesMem), len(s.CaptureGlobals))
		for j, name := range s.CaptureGlobals {
			if strings.ContainsRune(name, 0) {
				return nil, errors.New("v8go: invalid snapshot capture name")
			}
			names[j] = C.CString(name)
			defer C.free(unsafe.Pointer(names[j]))
		}
	}
	identity := StartupIdentity()
	result := C.SnapshotCreate((*C.SnapshotScriptInput)(mem), C.int(len(scripts)))
	if result.data == nil {
		return nil, newJSError(result.error)
	}
	defer C.SnapshotDelete(result.data)
	if result.length < 64 || result.length > 256<<20 {
		return nil, errors.New("v8go: snapshot exceeds supported size")
	}
	data := C.GoBytes(unsafe.Pointer(result.data), result.length)
	sum := sha256.Sum256(data)
	env := snapshotEnvelope{Format: "v8go-snapshot-2", Identity: identity, Digest: hex.EncodeToString(sum[:]), Captures: int(result.captures), DataLength: len(data), Data: data}
	header, _ := json.Marshal(env)
	if len(header) > snapshotHeaderLimit {
		return nil, errors.New("v8go: snapshot metadata exceeds supported size")
	}
	return &Snapshot{envelope: env, native: &snapshotNative{}}, nil
}

// acquireNative returns the native blob copy for one more isolate, creating it
// when no isolate currently uses it. V8 reads the blob for the isolate's whole
// lifetime, so every acquire is paired with releaseNative after IsolateDispose.
func (s *Snapshot) acquireNative() C.SnapshotBlobPtr {
	n := s.native
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.users == 0 {
		n.blob = C.SnapshotBlobNew((*C.char)(unsafe.Pointer(&s.envelope.Data[0])), C.int(len(s.envelope.Data)))
	}
	n.users++
	return n.blob
}

func (s *Snapshot) releaseNative() {
	n := s.native
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.users--; n.users == 0 {
		C.SnapshotBlobDelete(n.blob)
		n.blob = nil
	}
}

func snapshotNativeBlobCount() int { return int(C.SnapshotBlobLiveCount()) }

// NewIsolateWithSnapshot restores an isolate from s. Live isolates restored from
// the same Snapshot share one native copy of the blob, freed when the last of
// them is disposed.
func NewIsolateWithSnapshot(s *Snapshot, opts ...IsolateOption) (*Isolate, error) {
	if s == nil || s.native == nil || len(s.envelope.Data) < 64 {
		return nil, &SnapshotCompatibilityError{"nil snapshot"}
	}
	if err := snapshotPlatformError(); err != nil {
		return nil, err
	}
	if s.envelope.Identity != StartupIdentity() {
		return nil, &SnapshotCompatibilityError{"build, platform, or flags differ"}
	}
	config := &isolateConfig{}
	for _, opt := range opts {
		opt(config)
	}
	var constraints C.IsolateConstraintsPtr
	if c := config.resourceConstraints; c != nil {
		constraints = &C.IsolateConstraints{initial_heap_size_in_bytes: C.size_t(c.InitialHeapSizeInBytes), maximum_heap_size_in_bytes: C.size_t(c.MaxHeapSizeInBytes)}
	}
	ptr := C.SnapshotNewIsolate(s.acquireNative(), constraints)
	if ptr == nil {
		s.releaseNative()
		return nil, errors.New("v8go: snapshot isolate creation failed")
	}
	iso := &Isolate{ptr: ptr, cbs: make(map[int]FunctionCallbackWithError), snapshot: s}
	iso.null = newValueNull(iso)
	iso.undefined = newValueUndefined(iso)
	return iso, nil
}

// NewContextFromSnapshot restores the application context with a fresh Go
// registry reference. Close the context before disposing its isolate.
func NewContextFromSnapshot(iso *Isolate, index int) (*Context, error) {
	if iso == nil || iso.ptr == nil || iso.snapshot == nil || index != 0 {
		return nil, errors.New("v8go: invalid snapshot context")
	}
	ctxMutex.Lock()
	ctxSeq++
	ref := ctxSeq
	ctxMutex.Unlock()
	ptr := C.SnapshotNewContext(iso.ptr, C.int(index), C.int(ref))
	if ptr == nil {
		return nil, errors.New("v8go: context restoration failed")
	}
	ctx := &Context{ptr: ptr, ref: ref, iso: iso}
	ctx.register()
	return ctx, nil
}

// SnapshotData consumes one captured value. Values live until Context.Close.
func (c *Context) SnapshotData(index int) (*Value, error) {
	if c == nil || c.ptr == nil || c.iso.snapshot == nil || index < 0 || index >= c.iso.snapshot.CaptureCount() {
		return nil, errors.New("v8go: invalid snapshot data index")
	}
	ptr := C.SnapshotContextData(c.ptr, C.int(index))
	if ptr == nil {
		return nil, errors.New("v8go: snapshot data already consumed or absent")
	}
	return &Value{ptr, c}, nil
}
