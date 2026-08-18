// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

/*
#include <stdlib.h>
#include "gav8_build.h"

// gav8_build_args never calls back into Go, and never keeps a pointer it was
// given past the call: everything it reads is copied into V8 before it
// returns. Declaring both facts lets the compiler keep the arguments on the
// goroutine stack instead of heap-allocating them.

#cgo noescape gav8_build_args
#cgo nocallback gav8_build_args
#cgo noescape gav8_build_call_count
#cgo nocallback gav8_build_call_count
#cgo noescape gav8_build_reset_call_count
#cgo nocallback gav8_build_reset_call_count
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// Span kinds. A leaf's bytes are either staged into [Payload.Buf] — Off is a
// byte offset into it — or left where they are and pinned, in which case Off
// indexes [Payload.Ptrs]. A producer picks per value against a size threshold,
// so a single payload normally carries both.
const (
	SpanStaged uint32 = C.GAV8_SPAN_STAGED
	SpanPinned uint32 = C.GAV8_SPAN_PINNED
)

// Opcodes for a build program. See [BuildValue] for what a program is and why
// it is shaped this way.
//
// These values are the ABI: a producer in another package (or another
// language) emits them as plain uint32 words, so they may be added to but
// never renumbered.
const (
	// OpEnd terminates the program. Exactly one value must remain on the
	// stack, and it is the root.
	OpEnd uint32 = C.GAV8_OP_END
	// OpNull pushes null.
	OpNull uint32 = C.GAV8_OP_NULL
	// OpUndef pushes undefined.
	OpUndef uint32 = C.GAV8_OP_UNDEF
	// OpTrue pushes true.
	OpTrue uint32 = C.GAV8_OP_TRUE
	// OpFalse pushes false.
	OpFalse uint32 = C.GAV8_OP_FALSE
	// OpBool pushes the next Nums entry as a boolean (0 is false).
	OpBool uint32 = C.GAV8_OP_BOOL
	// OpInt pushes the next Nums entry as a number.
	OpInt uint32 = C.GAV8_OP_INT
	// OpF64 pushes the next Floats entry.
	OpF64 uint32 = C.GAV8_OP_F64
	// OpStr pushes a string from the next value Span.
	OpStr uint32 = C.GAV8_OP_STR
	// OpBytes pushes a Uint8Array from the next value Span.
	OpBytes uint32 = C.GAV8_OP_BYTES
	// OpObj is followed by a shape id. It pops that shape's key count off the
	// stack, in shape order, and pushes the object. Every key in the shape
	// gets a property, whatever its value; only [OpObjOmit] reads anything
	// into an undefined.
	OpObj uint32 = C.GAV8_OP_OBJ
	// OpMark remembers the current stack depth.
	OpMark uint32 = C.GAV8_OP_MARK
	// OpArrFromMark pops everything pushed since the matching OpMark into an
	// array.
	OpArrFromMark uint32 = C.GAV8_OP_ARR_FROM_MARK
	// OpRepeat is followed by a body length in words. It takes n from the next
	// Counts entry and runs that many words n times before continuing after
	// them. Bodies may nest. n == 0 skips the body and leaves every other
	// cursor untouched, so an empty slice costs nothing. The body may not be
	// empty.
	OpRepeat uint32 = C.GAV8_OP_REPEAT
	// OpNullable is followed by a body length in words. It takes a flag from
	// the next Counts entry — only zero is null, any other value is present —
	// and either pushes null and skips the body, or runs the body, which must
	// push exactly one value.
	//
	// This is how a *T is encoded, and nothing else expresses it: OpRepeat with
	// a count of 0 or 1 gives "the value or nothing", but OpObj pops a fixed
	// arity from its shape, so a skipped push does not yield null — it takes
	// the previous field's value and shifts every field after it.
	//
	// On the null path only the flag is consumed. Nums, Floats, Spans and the
	// rest of Counts stay where they were, because a producer stages no payload
	// for a value it is not sending; consuming one would desynchronise every
	// leaf that follows. The body must also be self-contained: it may nest
	// anything, including OpRepeat, OpObj and further OpNullable, but it may
	// not pop values pushed before it or close an OpMark from outside it. Both
	// are errors, since either would make the tree depend on the flag in a way
	// the generator did not write.
	OpNullable uint32 = C.GAV8_OP_NULLABLE
	// OpOptional is [OpNullable] with a different absent value: the flag comes
	// from the next Counts entry, zero pushes undefined instead of null, and
	// everything else — the skipped body, the untouched cursors, the
	// exactly-one-value contract, the self-containment rules — is identical.
	//
	// It is how an ABSENT object key is encoded, and null cannot stand in for
	// it: {"note":null} and {} are different values, and a producer whose
	// field is conditionally emitted is asking for exactly that distinction.
	// Pair it with [OpObjOmit], which reads the sentinel.
	OpOptional uint32 = C.GAV8_OP_OPTIONAL
	// OpObjOmit is followed by a shape id. It pops the same fixed arity as
	// [OpObj] and builds an object from only the pairs whose value is not
	// undefined, in shape order; all of them undefined yields {}.
	//
	// Undefined is the omit sentinel because it cannot occur as a legitimate
	// value: null, booleans, numbers, strings, Uint8Array, arrays and objects
	// are the whole of what a leaf can be, so nothing but [OpUndef] and an
	// absent [OpOptional] produces one. A presence bitmask would instead be a
	// second source of truth, and a producer whose bit and whose staged value
	// disagreed would build a valid object with its fields shifted.
	OpObjOmit uint32 = C.GAV8_OP_OBJ_OMIT
)

// Span locates one string or byte-slice leaf, either in [Payload.Buf] or
// through [Payload.Ptrs]. See [SpanStaged] and [SpanPinned].
//
// The layout is part of the ABI — 16 bytes, fields at offsets 0, 4 and 8 —
// because a producer may write these as a flat array without going through
// this type.
type Span struct {
	Off  uint32
	Len  uint32
	Kind uint32
	_    uint32
}

// ShapeDef names one object shape as a run of key spans: its keys are
// Payload.Spans[First : First+N], which must lie inside the key region.
type ShapeDef struct {
	First uint32
	N     uint32
}

// Payload is the data half of a build: flat arrays the program indexes with
// implicitly advancing cursors, in the order a producer filled them.
//
// Spans is one array with two regions. Spans[:KeySpans] are the shape keys and
// Spans[KeySpans:] are the values, so the value cursor starts at KeySpans;
// keys live in the same array to avoid a second buffer.
//
// Nothing here is retained past the call: every leaf is copied into V8 while
// the call runs. See [BuildValue] for the pinning rule that applies to Ptrs.
type Payload struct {
	// Ops is the program. See [BuildValue].
	Ops []uint32
	// Shapes are the object shapes the program's OpObj operands index.
	Shapes []ShapeDef
	// Buf holds the bytes of every staged string and byte slice, concatenated.
	Buf []byte
	// Spans locates each string and byte-slice leaf.
	Spans []Span
	// KeySpans is how many leading entries of Spans are shape keys.
	KeySpans int
	// Ptrs holds the backing pointers of leaves that were pinned rather than
	// staged, indexed by a pinned Span's Off. Only the first len(Ptrs) entries
	// are read; a pooled backing array may hold anything past that. See
	// [BuildValue] for the pinning rule.
	Ptrs []unsafe.Pointer
	// Nums holds the int64 scalars, positionally. Booleans ride here as 0/1.
	Nums []int64
	// Floats holds the float64 scalars, positionally.
	Floats []float64
	// Counts holds the program's control-flow values in execution order: one
	// entry per OpRepeat executed, holding the length of that region, and one
	// per OpNullable or OpOptional executed, holding its present/absent flag.
	Counts []int32
}

// BuildValue constructs a whole JavaScript value tree in a single cgo crossing
// and returns its root, owned by ctx the way any other v8go value is.
//
// A program, not a serialization. p.Ops is a stack machine's instruction
// stream, and it encodes the shape of the data rather than the data itself:
// OpRepeat runs a body n times, so a marshaller that knows the Go type emits
// the loop as a loop. A []struct{ID int64; Name, Email string} is seven ops
// whether it holds one row or a million:
//
//	OpMark
//	OpRepeat, 4     // n comes from Counts
//	  OpInt         // from Nums
//	  OpStr         // from Spans
//	  OpStr
//	  OpObj, shape  // pops 3, pushes the object
//	OpArrFromMark
//	OpEnd
//
// That is what makes this cheap. The alternative — a cgo call per node — costs
// ~64-84ns a time, so a 20-column, 1000-row result set spends over a
// millisecond on boundary crossings alone, more than JSON.parse needs to build
// the same tree from scratch. Here the per-node cost is a C switch dispatch,
// about 2ns.
//
// Exactly one tracked value is created, for the root; every interior node
// lives and dies as a v8::Local inside the call.
//
// It is safe to call from inside a [FunctionTemplate] callback, which is the
// shape the builder exists for: the host native receives a call from JS and
// answers it with a value rather than with JSON. V8 is already entered there —
// locked, in a HandleScope, in a Context::Scope, with a call in flight — and
// nesting another set of those is fine. What is NOT fine there is panicking:
// a Go panic unwinds out through V8's C++ frames without running their
// destructors, so the isolate is left entered and the process aborts at the
// next Isolate::Dispose, reported as a teardown fault far from its cause.
// BuildValue therefore reports everything, including a recovered panic, as an
// error.
//
// Pinning. Everything in p is read, and copied into V8, during the call, and
// none of it is retained. The one caller obligation is p.Ptrs: each entry that
// the spans actually reference must point either to non-Go memory or to a Go
// object the caller has pinned with a [runtime.Pinner] that outlives the call.
// Only the first len(p.Ptrs) entries are read, and the addresses cross as
// addresses — a pooled backing array may hold whatever it likes in the slots
// past len.
//
// A malformed program is an error, never a crash: every index is checked
// against its array before it is used, and a failure builds nothing.
func BuildValue(ctx *Context, p *Payload) (val *Value, err error) {
	if ctx == nil || ctx.ptr == nil {
		return nil, errors.New("v8go: BuildValue requires an open Context")
	}
	if p == nil {
		return nil, errors.New("v8go: BuildValue requires a Payload")
	}
	if p.KeySpans < 0 || p.KeySpans > len(p.Spans) {
		return nil, errors.New("v8go: Payload.KeySpans is outside Payload.Spans")
	}

	// Nothing below is expected to panic. The guard is here because of where
	// this runs: inside a V8 callback a panic does not fail the call, it
	// corrupts the isolate (see above). An error the caller can fall back
	// from is strictly better, and easier to attribute.
	defer func() {
		if r := recover(); r != nil {
			val, err = nil, fmt.Errorf("v8go: BuildValue: %v", r)
		}
	}()

	// No runtime.LockOSThread here, unlike a BatchScope. That one spans many
	// cgo calls and the isolate lock must be released by the thread that took
	// it, so its goroutine cannot migrate in between. This is one call: the
	// Locker is taken and released inside it, and a goroutine cannot move off
	// its thread part-way through a cgo call.
	rtn := C.gav8_build_args(ctx.ptr,
		(*C.uint32_t)(sliceData(p.Ops)), C.int(len(p.Ops)),
		(*C.gav8_shapedef)(sliceData(p.Shapes)), C.int(len(p.Shapes)),
		(*C.char)(sliceData(p.Buf)), C.int(len(p.Buf)),
		(*C.gav8_span)(sliceData(p.Spans)), C.int(len(p.Spans)),
		C.int(p.KeySpans),
		(*C.uintptr_t)(sliceData(p.Ptrs)), C.int(len(p.Ptrs)),
		(*C.int64_t)(sliceData(p.Nums)), C.int(len(p.Nums)),
		(*C.double)(sliceData(p.Floats)), C.int(len(p.Floats)),
		(*C.int32_t)(sliceData(p.Counts)), C.int(len(p.Counts)))
	runtime.KeepAlive(p)

	return valueResult(ctx, rtn)
}

// sliceData is the address of a slice's first element, or nil when it is
// empty. C reads the length from its own argument, so an empty slice must
// arrive as a null pointer rather than as whatever &s[0] would panic on.
func sliceData[T any](s []T) unsafe.Pointer {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Pointer(&s[0])
}

// BuildCallCount reports how many times the C entry point behind [BuildValue]
// has been entered, successfully or not, since the process started or since
// [ResetBuildCallCount].
//
// It is a diagnostic, and the one that matters: building a tree in a single
// crossing is the whole reason this API exists, so a test can measure it
// rather than assume it. The counter is process-wide, so a test that asserts
// an exact value must not run in parallel with another that builds.
func BuildCallCount() uint64 {
	return uint64(C.gav8_build_call_count())
}

// ResetBuildCallCount zeroes the counter [BuildCallCount] reports.
func ResetBuildCallCount() {
	C.gav8_build_reset_call_count()
}

// Compile-time proof that the Go structs above are laid out exactly like the C
// ones they are cast to. A negative constant here fails to convert to uint,
// which is the error a reader should see rather than a corrupt payload at
// runtime. The literal offsets are the ABI: a producer writes these arrays
// without necessarily going through these types.
const (
	_ = uint(unsafe.Sizeof(Span{}) - C.sizeof_gav8_span)
	_ = uint(C.sizeof_gav8_span - unsafe.Sizeof(Span{}))
	_ = uint(unsafe.Sizeof(ShapeDef{}) - C.sizeof_gav8_shapedef)
	_ = uint(C.sizeof_gav8_shapedef - unsafe.Sizeof(ShapeDef{}))

	_ = uint(unsafe.Offsetof(Span{}.Off) - 0)
	_ = uint(unsafe.Offsetof(Span{}.Len) - 4)
	_ = uint(4 - unsafe.Offsetof(Span{}.Len))
	_ = uint(unsafe.Offsetof(Span{}.Kind) - 8)
	_ = uint(8 - unsafe.Offsetof(Span{}.Kind))
	_ = uint(unsafe.Offsetof(ShapeDef{}.N) - 4)
	_ = uint(4 - unsafe.Offsetof(ShapeDef{}.N))
)
