#ifndef V8GO_GAV8_BUILD_H
#define V8GO_GAV8_BUILD_H

#include <stdint.h>

#include "errors.h"
#include "gav8_values.h"

#ifdef __cplusplus
extern "C" {
#endif

// gav8_build constructs a whole JavaScript value tree in ONE cgo crossing.
//
// It exists because gav8_values.h did not. That API removed the per-node
// m_value/v8::Global/hash-insert cost, then spent it again in a different
// currency: every leaf was its own cgo call at ~64-84ns, so a 20x1000 result
// set paid ~21 000 crossings — 1.3-1.5ms, the whole of JSON.parse's budget,
// on boundary transitions alone. Dispatching the same node from a C-side
// switch costs ~2ns, about 30x less.
//
// The op buffer encodes the PROGRAM, not the data. A generated marshaller
// knows the type, so it emits a loop as a loop: []User{ID int64, Name, Email
// string} is 7 ops for any number of users, not 7 ops per user. The buffer is
// O(type complexity), not O(data size). Cursors into the payload arrays
// advance implicitly, in the order the marshaller filled them.

// --- span kinds ---
// A leaf's bytes are either staged into buf (off is a byte offset) or left in
// place and pinned by the caller (off is an index into ptrs). A producer picks
// per value against a size threshold, so both kinds occur in one payload.
#define GAV8_SPAN_STAGED 0u
#define GAV8_SPAN_PINNED 1u

// gav8_span locates one string or byte-slice leaf. The layout is part of the
// ABI — 16 bytes, fields at 0/4/8 — because the producer writes these as a
// flat array from another language.
typedef struct {
  uint32_t off;  // byte offset into buf, or index into ptrs when kind == 1
  uint32_t len;
  uint32_t kind;  // GAV8_SPAN_STAGED or GAV8_SPAN_PINNED
  uint32_t _rsv;  // reserved; keeps the struct 16 bytes
} gav8_span;

// gav8_shapedef names one object shape as a run of key spans. Its keys are
// spans[first .. first+n), which must lie inside the key region.
typedef struct {
  uint32_t first;  // index of the first key span
  uint32_t n;      // number of keys
} gav8_shapedef;

// gav8_payload is the data half: flat, pointer-free arrays (except ptrs, whose
// entries the caller pins), all owned by the caller for the duration of the
// single call. gav8_build copies everything it needs into V8 and retains none
// of it.
//
// spans[0 .. nkeyspans) are shape keys and spans[nkeyspans ..) are values, so
// the value cursor starts at nkeyspans. Keys live in the same arrays to avoid
// a second buffer.
typedef struct {
  const uint32_t* ops;
  int nops;
  const gav8_shapedef* shapes;
  int nshapes;
  const char* buf;
  int buflen;
  const gav8_span* spans;
  int nspans;
  int nkeyspans;  // spans[0..nkeyspans) are shape keys
  const void* const* ptrs;
  int nptrs;
  const int64_t* nums;
  int nnums;
  const double* floats;
  int nfloats;
  const int32_t* counts;
  int ncounts;
} gav8_payload;

// Opcodes, encoded as uint32 words. An op is its code, optionally followed by
// operand words. These numbers are part of the ABI: a producer emits them.
typedef enum {
  // Terminates. Exactly one value must remain on the stack; it is the root.
  GAV8_OP_END = 0,
  GAV8_OP_NULL = 1,
  GAV8_OP_UNDEF = 2,
  GAV8_OP_TRUE = 3,
  GAV8_OP_FALSE = 4,
  GAV8_OP_BOOL = 5,   // push nums[numCursor++] != 0
  GAV8_OP_INT = 6,    // push nums[numCursor++] as a Number
  GAV8_OP_F64 = 7,    // push floats[fltCursor++]
  GAV8_OP_STR = 8,    // push a String from spans[spanCursor++]
  GAV8_OP_BYTES = 9,  // push a Uint8Array from spans[spanCursor++]
  // operand: shapeId. Pops shapes[shapeId].n values into an object.
  GAV8_OP_OBJ = 10,
  GAV8_OP_MARK = 11,           // remember the current value-stack depth
  GAV8_OP_ARR_FROM_MARK = 12,  // pop everything above the mark into an Array
  // operand: bodyLen. n = counts[cntCursor++]; runs the next bodyLen words n
  // times, then continues after them. Bodies may nest. n == 0 skips the body
  // and leaves every other cursor untouched.
  GAV8_OP_REPEAT = 13,
  // operand: bodyLen. flag = counts[cntCursor++]. Zero pushes null and skips
  // the body outright, leaving every other cursor untouched; anything non-zero
  // runs the body, which must push EXACTLY one value. Bodies may nest.
  //
  // This is the op a nullable field needs — a *T is null or a value — and no
  // combination of the others expresses it: OP_REPEAT with a count of 0 or 1
  // gives "the value or nothing", but OP_OBJ pops a fixed arity from its
  // shape, so a skipped push does not yield null, it steals the previous
  // field's value and shifts every field after it.
  //
  // The flag rides in counts because it is control flow, like a loop bound,
  // and reading it there keeps the payload to the same arrays. Only the flag
  // is consumed on the null path: a producer stages no nums/floats/spans entry
  // for a value it is not sending, so touching one would desynchronise every
  // leaf that follows.
  GAV8_OP_NULLABLE = 14,
} gav8_op;

// Builds the entire value tree and returns the root as a tracked ValuePtr.
// Exactly one m_value is created, for the root; every interior node is a
// v8::Local freed when the call returns.
//
// Fails closed. The op buffer is generated code, but it is also the only thing
// between a producer bug and an out-of-bounds read of process memory, so every
// index is checked against its array's length — shape ids, all four cursors,
// span off+len, ptrs indices, stack depth on each pop, mark balance, a repeat
// or nullable body running past the end of the buffer, and a nullable body
// that does not leave exactly one value. A malformed payload returns an
// RtnError; it never yields a partially built tree.
extern RtnValue gav8_build(ContextPtr ctx, const gav8_payload* p);

// gav8_build_args is gav8_build with the payload spread across arguments. It
// exists for cgo: a Go caller cannot hand C a Go-allocated gav8_payload
// because that struct holds pointers into Go memory, which the cgo pointer
// rules forbid without pinning every field. Passing the arrays as separate
// arguments is checked and allowed. It assembles the struct on the C stack and
// calls gav8_build once, so the crossing count is unaffected.
//
// ptrs is uintptr_t rather than void* const* deliberately, and the reason is
// not style. Handed an array of pointers, cgo's pointer check scans the whole
// backing ALLOCATION — not the nptrs entries this call reads — and rejects any
// slot holding an unpinned Go pointer. A producer that pools its staging
// buffers leaves exactly that behind: pointers from a longer earlier call,
// unpinned when it finished. There is nothing a caller can do about memory the
// call never touches, and the check does not fail gracefully — in the caller
// this exists for, it panics inside a v8::FunctionTemplate callback, unwinds
// through V8's C++ frames without running their destructors, and aborts the
// process at the next Isolate::Dispose. Addresses cross as addresses; keeping
// them valid for the call is the caller's documented job either way.
extern RtnValue gav8_build_args(ContextPtr ctx,
                                const uint32_t* ops,
                                int nops,
                                const gav8_shapedef* shapes,
                                int nshapes,
                                const char* buf,
                                int buflen,
                                const gav8_span* spans,
                                int nspans,
                                int nkeyspans,
                                const uintptr_t* ptrs,
                                int nptrs,
                                const int64_t* nums,
                                int nnums,
                                const double* floats,
                                int nfloats,
                                const int32_t* counts,
                                int ncounts);

// --- diagnostics ---
// How many times gav8_build has been entered, successfully or not. One
// crossing per tree is the entire point of this ABI and the defect it exists
// to fix, so it is measurable rather than merely asserted.
extern uint64_t gav8_build_call_count(void);
extern void gav8_build_reset_call_count(void);

#ifdef __cplusplus
}
#endif
#endif
