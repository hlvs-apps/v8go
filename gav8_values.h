#ifndef V8GO_GAV8_VALUES_H
#define V8GO_GAV8_VALUES_H

#include <stdint.h>

#include "errors.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct m_ctx m_ctx;
typedef m_ctx* ContextPtr;

typedef struct gav8_scope gav8_scope;
typedef gav8_scope* ScopePtr;

// Index into the scope's LocalVector. GAV8_INVALID (0xFFFFFFFF) signals
// failure.
typedef uint32_t LocalRef;
typedef uint32_t ShapeRef;
#define GAV8_INVALID 0xFFFFFFFFu

// --- lifecycle ---
// Opens a HandleScope on ctx's isolate and enters ctx. Every LocalRef returned
// by this scope is valid until gav8_scope_close.
//
// The scope holds the isolate's lock for its whole lifetime, and that lock
// belongs to the thread that took it: open, build and close must all happen on
// one thread.
extern ScopePtr gav8_scope_open(ContextPtr ctx);

// Wraps ONE LocalRef as a tracked ValuePtr (the only m_value/v8::Global this
// API creates) and returns it. Does not close the scope. Fails, with the first
// error the scope recorded, if any builder call failed: a tree with a hole in
// it must never reach the caller as a value.
extern RtnValue gav8_scope_result(ScopePtr s, LocalRef root);

// Closes the HandleScope and frees the scope. All LocalRefs become invalid.
extern void gav8_scope_close(ScopePtr s);

// --- leaves ---
extern LocalRef gav8_null(ScopePtr s);
extern LocalRef gav8_undefined(ScopePtr s);
extern LocalRef gav8_bool(ScopePtr s, int v);
extern LocalRef gav8_i32(ScopePtr s, int32_t v);
extern LocalRef gav8_f64(ScopePtr s, double v);

// UTF-8 bytes, length-delimited (NOT NUL-terminated). Implementations SHOULD
// use NewFromOneByte when every byte is < 0x80, which is the overwhelmingly
// common case and avoids a UTF-8 validation pass.
extern LocalRef gav8_str(ScopePtr s, const char* p, int len);

// Copies len bytes into a new Uint8Array. This is what removes base64 from the
// binary paths (crypto, zlib, fs, fetch bodies, SQL blob cells).
extern LocalRef gav8_bytes(ScopePtr s, const void* p, int len);

// --- composites ---
// Interns n keys and registers them as a reusable shape. keys[i] has length
// keylens[i]. Call ONCE per object shape, then reuse the ShapeRef for every
// object of that shape — that is the whole point. Keys must be unique.
extern ShapeRef gav8_shape(ScopePtr s,
                           const char* const* keys,
                           const int* keylens,
                           int n);

// Builds an object with the shape's keys and the given values, in shape order.
// vals must have exactly the shape's n entries.
extern LocalRef gav8_obj(ScopePtr s, ShapeRef shape, const LocalRef* vals);

// Builds an array from n elements.
extern LocalRef gav8_arr(ScopePtr s, const LocalRef* elems, int n);

// --- diagnostics ---
// Number of Locals the scope has created. Tests assert this against the
// expected node count; it is how a stray per-node allocation is caught.
extern uint32_t gav8_scope_size(ScopePtr s);

#ifdef __cplusplus
}
#endif
#endif
