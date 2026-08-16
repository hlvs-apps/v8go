#ifndef V8GO_GAV8_INTERNAL_H
#define V8GO_GAV8_INTERNAL_H

// C++-only machinery shared by the gav8_* value builders (gav8_values.cc, the
// per-node scope API, and gav8_build.cc, the single-crossing op-buffer one).
//
// Nothing declared here crosses the cgo boundary. The ABI headers stay plain C
// with opaque handles precisely so a consumer can compile against them without
// the V8 headers on its include path: the CXXFLAGS defines
// (V8_COMPRESS_POINTERS, V8_31BIT_SMIS_ON_64BIT_ARCH, V8_ENABLE_SANDBOX) change
// every V8 object's layout, and a consumer that misses one links clean and then
// corrupts memory.

#include <stdint.h>
#include <string.h>

#include <string>

#include "deps/include/v8-context.h"
#include "deps/include/v8-isolate.h"
#include "deps/include/v8-local-handle.h"
#include "deps/include/v8-locker.h"
#include "deps/include/v8-object.h"
#include "deps/include/v8-primitive.h"

#include "context.h"

// gav8_entry is the V8 entry state one builder needs: Locker, Isolate::Scope,
// HandleScope, Context::Scope, held for the builder's whole lifetime so that
// every intermediate value is a plain v8::Local. No m_value, no v8::Global, no
// ctx->vals entry, none of the ~200-500ns of bookkeeping the value.h/object.h
// API pays per node. Destroying the entry destroys the HandleScope, which frees
// the entire tree in one go.
//
// The isolate lock belongs to the thread that took it, so an entry must be
// created, used and destroyed on one thread. A single cgo call cannot migrate;
// an API that spans several calls (gav8_scope) has to pin its goroutine.
struct gav8_entry {
  explicit gav8_entry(m_ctx* c)
      : ctx(c),
        iso(c->iso),
        locker(c->iso),
        isolate_scope(c->iso),
        handle_scope(c->iso),
        local_ctx(c->ptr.Get(c->iso)),
        context_scope(local_ctx) {}

  m_ctx* ctx;
  v8::Isolate* iso;

  // Declaration order is load-bearing: members are constructed in this order
  // and destroyed in the reverse, which is exactly the order V8 requires for
  // these to nest. Anything a derived class adds is destroyed before all of
  // them, which is also what it needs.
  v8::Locker locker;
  v8::Isolate::Scope isolate_scope;
  v8::HandleScope handle_scope;
  v8::Local<v8::Context> local_ctx;
  v8::Context::Scope context_scope;

  // Object.prototype of local_ctx, resolved on first use.
  v8::Local<v8::Value> obj_proto;

  // First failure wins, and is what the entry point reports. Builders keep
  // going or bail as suits them; what must never happen is a half-built tree
  // reaching the caller as a value.
  std::string failure;

  // Objects get the context's Object.prototype so that a built tree is
  // indistinguishable from the JSON.parse of the same data to the JS that
  // consumes it: a null-prototype object has no hasOwnProperty, no toString,
  // and is not an instanceof Object.
  bool ensure_obj_proto() {
    if (!obj_proto.IsEmpty()) {
      return true;
    }
    obj_proto = v8::Object::New(iso)->GetPrototypeV2();
    return !obj_proto.IsEmpty();
  }
};

// True when every byte is ASCII, in which case the bytes are also valid Latin-1
// and String::NewFromOneByte can take them verbatim, skipping the UTF-8
// validation and transcoding pass.
inline bool gav8_all_ascii(const uint8_t* p, int len) {
  int i = 0;
  for (; i + 8 <= len; i += 8) {
    uint64_t word;
    memcpy(&word, p + i, sizeof(word));
    if ((word & 0x8080808080808080ull) != 0) {
      return false;
    }
  }
  for (; i < len; i++) {
    if ((p[i] & 0x80) != 0) {
      return false;
    }
  }
  return true;
}

// Builds a JS string from len UTF-8 bytes, taking the one-byte fast path when
// they are all ASCII. Length-delimited, so an interior NUL is a character.
inline v8::MaybeLocal<v8::String> gav8_new_string(v8::Isolate* iso,
                                                  const uint8_t* p,
                                                  int len) {
  if (gav8_all_ascii(p, len)) {
    return v8::String::NewFromOneByte(iso, p, v8::NewStringType::kNormal, len);
  }
  return v8::String::NewFromUtf8(iso, reinterpret_cast<const char*>(p),
                                 v8::NewStringType::kNormal, len);
}

#endif
