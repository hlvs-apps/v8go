#include "gav8_values.h"

#include <stdlib.h>
#include <string.h>

#include <string>
#include <unordered_set>
#include <utility>
#include <vector>

#include "deps/include/v8-array-buffer.h"
#include "deps/include/v8-container.h"
#include "deps/include/v8-context.h"
#include "deps/include/v8-isolate.h"
#include "deps/include/v8-local-handle.h"
#include "deps/include/v8-locker.h"
#include "deps/include/v8-object.h"
#include "deps/include/v8-primitive.h"
#include "deps/include/v8-typed-array.h"

#include "context.h"
#include "gav8_internal.h"
#include "utils.h"
#include "value.h"

using namespace v8;

/********** Batch value builder **********/

// A gav8_scope is one cgo crossing per node worth of value building. It holds
// the V8 entry state (see gav8_entry) for its lifetime, so every value it
// builds is a plain v8::Local rather than a tracked m_value.
//
// Because the isolate lock belongs to the thread that took it, the scope must
// be opened, used and closed on a single thread; the Go wrapper pins its
// goroutine for that reason.
struct gav8_scope : gav8_entry {
  explicit gav8_scope(m_ctx* c)
      : gav8_entry(c), vals(c->iso), scratch(c->iso) {
    vals.reserve(64);
  }

  // The LocalRef table. A LocalRef is an index into this, never a pointer, so
  // nothing crossing the boundary can dangle.
  LocalVector<Value> vals;

  // The ShapeRef table: one entry per shape, holding that shape's interned
  // keys in order.
  std::vector<LocalVector<Name>> shapes;

  // Gathers one composite's element handles for the bulk constructors. Reused
  // so that building N objects allocates once, not N times. gav8_obj and
  // gav8_arr never run concurrently or nested, so one buffer serves both.
  //
  // Neither this nor the base's cached Object.prototype is in vals, which is
  // what keeps gav8_scope_size equal to the caller's own node count.
  LocalVector<Value> scratch;
};

namespace {

// Records a failure (the first one only) and yields the sentinel, so it can be
// returned directly from either a LocalRef or a ShapeRef builder.
uint32_t fail(gav8_scope* s, const char* msg) {
  if (s->failure.empty()) {
    s->failure = msg;
  }
  return GAV8_INVALID;
}

uint32_t push(gav8_scope* s, Local<Value> val) {
  // Capping below the sentinel is what keeps GAV8_INVALID from ever being a
  // valid index.
  if (s->vals.size() >= GAV8_INVALID) {
    return fail(s, "gav8: too many values in one scope");
  }
  s->vals.push_back(val);
  return static_cast<uint32_t>(s->vals.size() - 1);
}

// Returns false for a sentinel or out-of-range ref. Every consumer of a
// LocalRef goes through here, which is how a failure upstream propagates as a
// failure instead of an out-of-bounds read.
bool resolve(gav8_scope* s, LocalRef ref, Local<Value>* out) {
  if (ref >= s->vals.size()) {
    return false;
  }
  *out = s->vals[ref];
  return true;
}

}  // namespace

ScopePtr gav8_scope_open(ContextPtr ctx) {
  if (ctx == nullptr) {
    return nullptr;
  }
  return new gav8_scope(ctx);
}

RtnValue gav8_scope_result(ScopePtr s, LocalRef root) {
  RtnValue rtn = {};
  if (s == nullptr) {
    rtn.error.msg = CopyString("gav8: nil scope");
    return rtn;
  }
  if (!s->failure.empty()) {
    rtn.error.msg = CopyString(s->failure);
    return rtn;
  }
  Local<Value> value;
  if (!resolve(s, root, &value)) {
    rtn.error.msg = CopyString("gav8: invalid root LocalRef");
    return rtn;
  }

  // The one and only tracked value this API creates.
  m_value* val = new m_value;
  val->id = 0;
  val->iso = s->iso;
  val->ctx = s->ctx;
  val->ptr = Global<Value>(s->iso, value);

  rtn.value = tracked_value(s->ctx, val);
  return rtn;
}

void gav8_scope_close(ScopePtr s) {
  delete s;
}

LocalRef gav8_null(ScopePtr s) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  return push(s, Null(s->iso));
}

LocalRef gav8_undefined(ScopePtr s) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  return push(s, Undefined(s->iso));
}

LocalRef gav8_bool(ScopePtr s, int v) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  return push(s, Boolean::New(s->iso, v != 0));
}

LocalRef gav8_i32(ScopePtr s, int32_t v) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  return push(s, Integer::New(s->iso, v));
}

LocalRef gav8_f64(ScopePtr s, double v) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  return push(s, Number::New(s->iso, v));
}

LocalRef gav8_str(ScopePtr s, const char* p, int len) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  if (len < 0) {
    return fail(s, "gav8: negative string length");
  }
  if (p == nullptr && len > 0) {
    return fail(s, "gav8: null string pointer with a non-zero length");
  }
  // Both factories reject a null data pointer even for an empty string.
  const char* data = p != nullptr ? p : "";
  const uint8_t* bytes = reinterpret_cast<const uint8_t*>(data);

  Local<String> str;
  if (!gav8_new_string(s->iso, bytes, len).ToLocal(&str)) {
    return fail(s, "gav8: could not create string");
  }
  return push(s, str);
}

LocalRef gav8_bytes(ScopePtr s, const void* p, int len) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  if (len < 0) {
    return fail(s, "gav8: negative byte length");
  }
  if (p == nullptr && len > 0) {
    return fail(s, "gav8: null byte pointer with a non-zero length");
  }

  // Uninitialized: the memcpy below writes every byte, so zeroing first would
  // be a second pass over the whole buffer.
  Local<ArrayBuffer> buf =
      ArrayBuffer::New(s->iso, static_cast<size_t>(len),
                       BackingStoreInitializationMode::kUninitialized);
  if (len > 0) {
    memcpy(buf->Data(), p, static_cast<size_t>(len));
  }
  return push(s, Uint8Array::New(buf, 0, static_cast<size_t>(len)));
}

ShapeRef gav8_shape(ScopePtr s,
                    const char* const* keys,
                    const int* keylens,
                    int n) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  if (n < 0 || (n > 0 && (keys == nullptr || keylens == nullptr))) {
    return fail(s, "gav8: invalid shape arguments");
  }
  if (s->shapes.size() >= GAV8_INVALID) {
    return fail(s, "gav8: too many shapes in one scope");
  }

  // Duplicate keys would leave V8 building a map with a repeated property.
  // Checking here costs once per shape rather than once per object.
  std::unordered_set<std::string> seen;
  seen.reserve(static_cast<size_t>(n));

  LocalVector<Name> names(s->iso);
  names.reserve(static_cast<size_t>(n));

  for (int i = 0; i < n; i++) {
    const int len = keylens[i];
    if (len < 0) {
      return fail(s, "gav8: negative shape key length");
    }
    if (keys[i] == nullptr && len > 0) {
      return fail(s, "gav8: null shape key with a non-zero length");
    }
    const char* key = keys[i] != nullptr ? keys[i] : "";
    if (!seen.emplace(key, static_cast<size_t>(len)).second) {
      return fail(s, "gav8: duplicate key in shape");
    }

    // kInternalized: the key is interned exactly once, here, and every object
    // of this shape reuses the same handle, so building N objects does no
    // string work at all after the first. (It does not, on its own, buy a
    // shared hidden class — see the comment in gav8_obj for what the bulk
    // constructor actually produces.)
    Local<String> name;
    if (!String::NewFromUtf8(s->iso, key, NewStringType::kInternalized, len)
             .ToLocal(&name)) {
      return fail(s, "gav8: could not intern shape key");
    }
    names.push_back(name);
  }

  s->shapes.push_back(std::move(names));
  return static_cast<ShapeRef>(s->shapes.size() - 1);
}

LocalRef gav8_obj(ScopePtr s, ShapeRef shape, const LocalRef* vals) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  if (shape >= s->shapes.size()) {
    return fail(s, "gav8: invalid ShapeRef");
  }
  LocalVector<Name>& names = s->shapes[shape];
  const size_t n = names.size();

  if (n == 0) {
    // Object::New(iso) already yields {} with Object.prototype, and avoids
    // handing the bulk constructor a pair of empty-vector data pointers.
    return push(s, Object::New(s->iso));
  }
  if (vals == nullptr) {
    return fail(s, "gav8: null values for a non-empty shape");
  }
  if (!s->ensure_obj_proto()) {
    return fail(s, "gav8: could not resolve Object.prototype");
  }

  s->scratch.resize(n);
  for (size_t i = 0; i < n; i++) {
    if (!resolve(s, vals[i], &s->scratch[i])) {
      return fail(s, "gav8: invalid LocalRef in object values");
    }
  }

  // One call per object with the shape's already-interned keys, instead of n
  // property stores.
  //
  // Read this before changing it, and before assuming what it buys. Measured
  // on darwin/amd64, 20 keys x 1000 objects: this constructor does NOT produce
  // hidden-class objects. It produces DICTIONARY-MODE ones —
  // %HasFastProperties(o) is false here and true for the same data out of
  // JSON.parse. Objects of one shape do share a map, but it is the slow map,
  // so the sharing buys nothing on the read side, and JS reads from these
  // about 11x slower than from parsed objects (10.5-14.0ms vs 0.9-1.3ms for
  // 500 scans of 1000 rows, 3 property loads each).
  //
  // The alternative — Object::New(iso) then n CreateDataProperty calls — does
  // produce fast-property objects sharing a hidden class, and reads at parity
  // with JSON.parse, but costs ~2.2us per object against ~0.68us here
  // (20x1000: 4.10ms vs 2.50ms to build). Break-even is around 70 full passes
  // over the data; below that the constructor here wins, above it the other
  // one does.
  //
  // The spec mandates this one and it is the build-optimal one, so it stays.
  // The swap, if a caller turns out to read these in a hot loop, is:
  //
  //   Local<Object> obj = Object::New(s->iso);
  //   for (size_t i = 0; i < n; i++) {
  //     if (obj->CreateDataProperty(s->local_ctx, names[i], s->scratch[i])
  //             .IsNothing()) {
  //       return fail(s, "gav8: could not define object property");
  //     }
  //   }
  //   return push(s, obj);
  return push(s, Object::New(s->iso, s->obj_proto, names.data(),
                             s->scratch.data(), n));
}

LocalRef gav8_arr(ScopePtr s, const LocalRef* elems, int n) {
  if (s == nullptr) {
    return GAV8_INVALID;
  }
  if (n < 0) {
    return fail(s, "gav8: negative array length");
  }
  if (n == 0) {
    return push(s, Array::New(s->iso, 0));
  }
  if (elems == nullptr) {
    return fail(s, "gav8: null elements for a non-empty array");
  }

  const size_t count = static_cast<size_t>(n);
  s->scratch.resize(count);
  for (size_t i = 0; i < count; i++) {
    if (!resolve(s, elems[i], &s->scratch[i])) {
      return fail(s, "gav8: invalid LocalRef in array elements");
    }
  }
  return push(s, Array::New(s->iso, s->scratch.data(), count));
}

uint32_t gav8_scope_size(ScopePtr s) {
  if (s == nullptr) {
    return 0;
  }
  return static_cast<uint32_t>(s->vals.size());
}
