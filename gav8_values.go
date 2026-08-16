// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

/*
#include <stdlib.h>
#include "gav8_values.h"

// None of these call back into Go, and none of them keep a pointer they were
// given past the call: gav8_str/gav8_bytes/gav8_shape/gav8_obj/gav8_arr copy
// what they need into V8 before returning. Declaring both facts lets the
// compiler keep the arguments on the goroutine stack instead of heap-allocating
// them, which matters because these are the calls a hot bridge makes per value.
// (gav8_scope_open does retain its ContextPtr, but that is C-owned memory, not
// a Go pointer, so noescape stays sound.)

#cgo noescape gav8_scope_open
#cgo nocallback gav8_scope_open
#cgo noescape gav8_scope_result
#cgo nocallback gav8_scope_result
#cgo noescape gav8_scope_close
#cgo nocallback gav8_scope_close
#cgo noescape gav8_null
#cgo nocallback gav8_null
#cgo noescape gav8_undefined
#cgo nocallback gav8_undefined
#cgo noescape gav8_bool
#cgo nocallback gav8_bool
#cgo noescape gav8_i32
#cgo nocallback gav8_i32
#cgo noescape gav8_f64
#cgo nocallback gav8_f64
#cgo noescape gav8_str
#cgo nocallback gav8_str
#cgo noescape gav8_bytes
#cgo nocallback gav8_bytes
#cgo noescape gav8_shape
#cgo nocallback gav8_shape
#cgo noescape gav8_obj
#cgo nocallback gav8_obj
#cgo noescape gav8_arr
#cgo nocallback gav8_arr
#cgo noescape gav8_scope_size
#cgo nocallback gav8_scope_size
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// LocalRef identifies one value inside a BatchScope. It is an index into the
// scope's handle table, not a pointer, so it cannot dangle: closing the scope
// invalidates every ref at once. Refs from one scope are meaningless in
// another.
//
// A builder that fails returns InvalidLocalRef instead of an error, and every
// method that consumes a ref propagates an invalid one rather than panicking.
// That lets a caller build an entire tree unchecked and find out at
// [BatchScope.Result], which reports what actually went wrong.
type LocalRef uint32

// ShapeRef identifies one interned key set inside a BatchScope. See
// [BatchScope.Shape].
type ShapeRef uint32

const (
	// InvalidLocalRef is returned by every LocalRef builder that fails.
	InvalidLocalRef LocalRef = C.GAV8_INVALID
	// InvalidShapeRef is returned by [BatchScope.Shape] when it fails.
	InvalidShapeRef ShapeRef = C.GAV8_INVALID
)

// BatchScope builds a whole JavaScript value tree in a single cgo crossing per
// node, without a tracked value per node.
//
// The Object/Value API creates a v8::Global for every value it makes, and
// registers it with the Context so it can be released later: a malloc, a
// GC-root registration and a hash-map insert, before any V8 work happens. That
// is affordable for a handful of values and roughly an order of magnitude
// slower than JSON.parse for a tree of a few thousand. A BatchScope instead
// holds one v8::HandleScope open for its lifetime and hands back uint32 indices
// into a handle table, so a node costs a v8::Local and nothing else. Exactly
// one tracked value is ever created: the root that [BatchScope.Result] returns.
//
// A scope holds the isolate's lock the whole time it is open, so nothing else
// can enter that isolate meanwhile — build and close promptly. It also pins its
// goroutine to the OS thread it started on, because the lock belongs to the
// thread that took it; [BatchScope.Close] unpins.
//
// A BatchScope is not safe for concurrent use, and must be closed on the
// goroutine that opened it.
type BatchScope struct {
	ptr C.ScopePtr
	ctx *Context

	// Key counts of the shapes registered so far, indexed by ShapeRef. The C
	// side reads exactly this many entries out of the slice given to Object,
	// so checking the length here is what keeps a short slice from becoming an
	// out-of-bounds read.
	shapeLens []int
}

// NewBatchScope opens a value-building scope on ctx. The caller must Close it,
// normally with defer:
//
//	s := v8.NewBatchScope(ctx)
//	defer s.Close()
//
// It panics if ctx is nil or already closed.
func NewBatchScope(ctx *Context) *BatchScope {
	if ctx == nil || ctx.ptr == nil {
		panic("v8go: NewBatchScope requires an open Context")
	}
	// The scope's isolate lock is owned by the thread that took it, so the
	// goroutine must not migrate between the calls that build the tree.
	runtime.LockOSThread()
	return &BatchScope{ptr: C.gav8_scope_open(ctx.ptr), ctx: ctx}
}

// Result wraps root as a *Value owned by the scope's Context, the way any other
// v8go value is owned, and returns it. The value outlives the scope; every
// other node in the tree is freed by Close.
//
// Result does not close the scope. It returns an error if root is invalid, or
// if any builder call on this scope failed — a tree with a hole in it never
// reaches the caller as a value.
func (s *BatchScope) Result(root LocalRef) (*Value, error) {
	rtn := C.gav8_scope_result(s.ptr, C.LocalRef(root))
	return valueResult(s.ctx, rtn)
}

// Close destroys the scope's HandleScope, which frees every value it built at
// once, and unpins the goroutine. All LocalRefs and ShapeRefs from this scope
// become invalid; values already handed out by Result are unaffected.
//
// Close is idempotent and must run on the goroutine that opened the scope.
func (s *BatchScope) Close() {
	if s.ptr == nil {
		return
	}
	C.gav8_scope_close(s.ptr)
	s.ptr = nil
	s.shapeLens = nil
	runtime.UnlockOSThread()
	runtime.KeepAlive(s.ctx)
}

// Null adds the JS null value to the scope.
func (s *BatchScope) Null() LocalRef {
	return LocalRef(C.gav8_null(s.ptr))
}

// Undefined adds the JS undefined value to the scope.
func (s *BatchScope) Undefined() LocalRef {
	return LocalRef(C.gav8_undefined(s.ptr))
}

// Bool adds a JS boolean to the scope.
func (s *BatchScope) Bool(v bool) LocalRef {
	var i C.int
	if v {
		i = 1
	}
	return LocalRef(C.gav8_bool(s.ptr, i))
}

// Int32 adds a JS number to the scope, as a V8 integer.
func (s *BatchScope) Int32(v int32) LocalRef {
	return LocalRef(C.gav8_i32(s.ptr, C.int32_t(v)))
}

// Float64 adds a JS number to the scope.
func (s *BatchScope) Float64(v float64) LocalRef {
	return LocalRef(C.gav8_f64(s.ptr, C.double(v)))
}

// String adds a JS string to the scope. The bytes are UTF-8 and
// length-delimited, so an interior NUL is a character like any other rather
// than a terminator.
//
// The string's bytes are read, and copied into V8, during the call; V8 never
// retains a pointer into Go memory.
func (s *BatchScope) String(v string) LocalRef {
	if len(v) == 0 {
		return LocalRef(C.gav8_str(s.ptr, nil, 0))
	}
	return LocalRef(C.gav8_str(s.ptr,
		(*C.char)(unsafe.Pointer(unsafe.StringData(v))), C.int(len(v))))
}

// Bytes adds a JS Uint8Array holding a copy of v to the scope. This is what
// lets binary payloads — crypto output, compressed blocks, file and response
// bodies, SQL blob cells — reach JS without a base64 round trip.
//
// The slice is copied into V8 during the call; V8 never retains a pointer into
// Go memory.
func (s *BatchScope) Bytes(v []byte) LocalRef {
	if len(v) == 0 {
		return LocalRef(C.gav8_bytes(s.ptr, nil, 0))
	}
	return LocalRef(C.gav8_bytes(s.ptr, unsafe.Pointer(&v[0]), C.int(len(v))))
}

// Shape interns keys once and returns a handle to be reused for every object
// that has those keys in that order. Interning once per shape instead of once
// per object is what lets V8 build the hidden class once and share it across
// them, which is the whole reason this is faster than setting properties one by
// one.
//
// Keys must be unique; Shape returns InvalidShapeRef if they are not.
func (s *BatchScope) Shape(keys []string) ShapeRef {
	n := len(keys)
	if n == 0 {
		return s.recordShape(0, ShapeRef(C.gav8_shape(s.ptr, nil, nil, 0)))
	}

	// The key array holds pointers, so it cannot live in Go memory: cgo
	// forbids passing a Go pointer to memory that itself contains Go pointers.
	// Shape runs once per shape, so the copy is not on any hot path.
	cKeys := (**C.char)(C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof((*C.char)(nil)))))
	defer C.free(unsafe.Pointer(cKeys))
	cLens := (*C.int)(C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.int(0)))))
	defer C.free(unsafe.Pointer(cLens))

	keySlice := unsafe.Slice(cKeys, n)
	lenSlice := unsafe.Slice(cLens, n)
	for i, key := range keys {
		keySlice[i] = C.CString(key)
		lenSlice[i] = C.int(len(key))
	}
	defer func() {
		for _, key := range keySlice {
			C.free(unsafe.Pointer(key))
		}
	}()

	return s.recordShape(n, ShapeRef(C.gav8_shape(s.ptr, cKeys, cLens, C.int(n))))
}

// recordShape remembers a successful shape's key count so Object can check the
// length of the values it is handed.
func (s *BatchScope) recordShape(n int, ref ShapeRef) ShapeRef {
	if ref == InvalidShapeRef {
		return ref
	}
	// The C side allocates refs densely from zero, in call order.
	if int(ref) == len(s.shapeLens) {
		s.shapeLens = append(s.shapeLens, n)
	}
	return ref
}

// Object builds a JS object with the shape's keys and the given values, in
// shape order. len(vals) must equal the number of keys in the shape; Object
// returns InvalidLocalRef otherwise.
//
// The object gets the context's Object.prototype, so it is structurally
// indistinguishable from the same data parsed out of JSON.
//
// It is not, however, indistinguishable in performance. V8's bulk object
// constructor produces dictionary-mode ("slow properties") objects, where
// JSON.parse produces hidden-class ones, and JS reads from these roughly 11x
// slower — a difference that only starts to matter after about 70 full passes
// over the data, which is why it is still the right trade for a result set
// rendered once. See the comment on gav8_obj in gav8_values.cc for the
// measurement and for the alternative.
func (s *BatchScope) Object(shape ShapeRef, vals []LocalRef) LocalRef {
	if int(shape) >= len(s.shapeLens) || s.shapeLens[shape] != len(vals) {
		return InvalidLocalRef
	}
	if len(vals) == 0 {
		return LocalRef(C.gav8_obj(s.ptr, C.ShapeRef(shape), nil))
	}
	return LocalRef(C.gav8_obj(s.ptr, C.ShapeRef(shape),
		(*C.LocalRef)(unsafe.Pointer(&vals[0]))))
}

// Array builds a JS array from elems.
func (s *BatchScope) Array(elems []LocalRef) LocalRef {
	if len(elems) == 0 {
		return LocalRef(C.gav8_arr(s.ptr, nil, 0))
	}
	return LocalRef(C.gav8_arr(s.ptr,
		(*C.LocalRef)(unsafe.Pointer(&elems[0])), C.int(len(elems))))
}

// Size reports how many values the scope has created. A test that knows how
// many nodes its tree has can assert on this: a number larger than the node
// count means something is allocating per node behind the caller's back.
func (s *BatchScope) Size() uint32 {
	return uint32(C.gav8_scope_size(s.ptr))
}
