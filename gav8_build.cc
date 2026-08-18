#include "gav8_build.h"

#include <limits.h>
#include <stdint.h>
#include <string.h>

#include <atomic>
#include <string>
#include <unordered_set>
#include <vector>

#include "deps/include/v8-array-buffer.h"
#include "deps/include/v8-container.h"
#include "deps/include/v8-context.h"
#include "deps/include/v8-isolate.h"
#include "deps/include/v8-local-handle.h"
#include "deps/include/v8-object.h"
#include "deps/include/v8-primitive.h"
#include "deps/include/v8-typed-array.h"

#include "context.h"
#include "gav8_internal.h"
#include "utils.h"
#include "value.h"

using namespace v8;

/********** Single-crossing op-buffer builder **********/

namespace {

// One crossing per tree is the property this ABI exists to have, so it is
// counted rather than assumed. Relaxed is enough: nothing is ordered against
// it, and a test reads it after the build it is measuring has returned.
std::atomic<uint64_t> g_build_calls{0};

// Checks everything about the payload that does not depend on the program:
// no negative length, no null array with a non-zero length, no key region
// larger than the span array. Returns nullptr when the payload is usable, and
// the reason when it is not.
//
// This runs before any V8 state is entered, so a malformed payload costs
// nothing and touches nothing.
const char* validate_payload(const gav8_payload* p) {
  if (p == nullptr) {
    return "gav8: nil payload";
  }
  if (p->nops < 0 || p->nshapes < 0 || p->buflen < 0 || p->nspans < 0 ||
      p->nkeyspans < 0 || p->nptrs < 0 || p->nnums < 0 || p->nfloats < 0 ||
      p->ncounts < 0) {
    return "gav8: negative array length in payload";
  }
  if (p->nkeyspans > p->nspans) {
    return "gav8: nkeyspans exceeds nspans";
  }
  if ((p->nops > 0 && p->ops == nullptr) ||
      (p->nshapes > 0 && p->shapes == nullptr) ||
      (p->buflen > 0 && p->buf == nullptr) ||
      (p->nspans > 0 && p->spans == nullptr) ||
      (p->nptrs > 0 && p->ptrs == nullptr) ||
      (p->nnums > 0 && p->nums == nullptr) ||
      (p->nfloats > 0 && p->floats == nullptr) ||
      (p->ncounts > 0 && p->counts == nullptr)) {
    return "gav8: null array with a non-zero length in payload";
  }
  if (p->nops == 0) {
    return "gav8: empty op buffer";
  }
  return nullptr;
}

// Which op opened a frame. The two conditional kinds are one mechanism: they
// share every check below and differ only in the value their absent branch
// pushes, so they are told apart here to name the right op in an error.
enum frame_kind : uint8_t {
  FRAME_REPEAT,
  FRAME_NULLABLE,
  FRAME_OPTIONAL,
};

// The op that opened a frame, for error messages.
const char* frame_op_name(frame_kind k) {
  switch (k) {
    case FRAME_NULLABLE:
      return "OP_NULLABLE";
    case FRAME_OPTIONAL:
      return "OP_OPTIONAL";
    default:
      return "OP_REPEAT";
  }
}

// A conditional body's floor is installed by either op, so a message about
// escaping one has to name both.
const char* const kConditionalBody = "an OP_NULLABLE/OP_OPTIONAL body";

// One body in flight, any kind. An OP_REPEAT frame carries the body to re-run
// and how many runs are left; an OP_NULLABLE or OP_OPTIONAL frame carries what
// the body must have done to the stack by the time it ends.
//
// All kinds share ONE stack because two frames can end on the same word — a
// conditional whose body is the tail of an OP_REPEAT body does, and so does a
// nullable body ending where an enclosing optional body ends — and the inner
// one has to close first. Separate stacks cannot express that ordering.
struct frame {
  uint32_t start;  // first word of the body; where a repeat jumps back to
  uint32_t end;    // one word past the body
  frame_kind kind;

  int64_t remaining;  // OP_REPEAT: iterations still to run

  // The conditional kinds: the state the body must restore, and the floor it
  // installs.
  size_t depth;        // value-stack depth on entry; +1 is required on exit
  size_t nmarks;       // marks.size() on entry; must be equal on exit
  size_t saved_floor;  // the enclosing floor, reinstated on exit
};

// builder interprets one op buffer against one payload. It holds the V8 entry
// state (see gav8_entry), so every node it makes is a v8::Local and the whole
// tree is freed when it goes out of scope.
struct builder : gav8_entry {
  builder(m_ctx* c, const gav8_payload* payload)
      : gav8_entry(c),
        p(payload),
        stack(c->iso),
        shape_keys(static_cast<size_t>(payload->nshapes),
                   LocalVector<Name>(c->iso)),
        shape_interned(static_cast<size_t>(payload->nshapes), 0),
        kept_keys(c->iso),
        span_cur(static_cast<uint32_t>(payload->nkeyspans)) {
    stack.reserve(64);
  }

  const gav8_payload* p;

  // The value stack. Leaves push; composites pop their operands and push the
  // result in place.
  LocalVector<Value> stack;

  // Depths remembered by OP_MARK, innermost last.
  std::vector<size_t> marks;

  // The lowest stack index the current op may pop down to: the entry depth of
  // the innermost conditional body (OP_NULLABLE or OP_OPTIONAL), or 0 outside
  // one. It is what makes such a body self-contained, which is the property
  // that lets the absent path skip it: a body that reached under its own entry
  // depth — popping an enclosing OP_MARK, or feeding an OP_OBJ with a value
  // pushed before the flag was read — would build a different tree depending
  // on the flag, and the exit depth check alone cannot see it (steal one
  // value, push one more).
  size_t floor = 0;

  // Interned keys per shape, filled on first use of that shape. Interning
  // lazily means a shape only reachable through a repeat that runs zero times
  // costs nothing.
  std::vector<LocalVector<Name>> shape_keys;
  std::vector<uint8_t> shape_interned;

  // Scratch for OP_OBJ_OMIT: the subset of a shape's keys whose values
  // survived. Kept on the builder rather than in the case so that a repeat
  // building N objects reuses one allocation. The values are compacted in
  // place on the value stack, so only the keys need somewhere to go — the
  // shape's own interned vector is shared by every object of that shape and
  // must not be disturbed.
  LocalVector<Name> kept_keys;

  // Cursors into the payload arrays. They advance implicitly, in the order the
  // producer filled the arrays; the span cursor starts past the key region.
  uint32_t span_cur;
  uint32_t num_cur = 0;
  uint32_t flt_cur = 0;
  uint32_t cnt_cur = 0;

  bool fail(const std::string& msg) {
    if (failure.empty()) {
      failure = msg;
    }
    return false;
  }

  bool span_bytes(uint32_t idx, const uint8_t** data, int* len);
  bool intern_shape(uint32_t id);
  bool run();
};

// Resolves one span to bytes the caller owns, checking it against whichever
// array it indexes. Every read of buf or ptrs goes through here.
bool builder::span_bytes(uint32_t idx, const uint8_t** data, int* len) {
  if (idx >= static_cast<uint32_t>(p->nspans)) {
    return fail("gav8: span cursor out of range");
  }
  const gav8_span s = p->spans[idx];
  if (s.len > static_cast<uint32_t>(INT32_MAX)) {
    return fail("gav8: span longer than INT32_MAX");
  }

  const void* base = nullptr;
  if (s.kind == GAV8_SPAN_STAGED) {
    // 64-bit arithmetic: off + len must not be able to wrap past the check.
    if (static_cast<uint64_t>(s.off) + s.len >
        static_cast<uint64_t>(p->buflen)) {
      return fail("gav8: staged span runs past the end of buf");
    }
    base = p->buf + s.off;
  } else if (s.kind == GAV8_SPAN_PINNED) {
    if (s.off >= static_cast<uint32_t>(p->nptrs)) {
      return fail("gav8: pinned span pointer index out of range");
    }
    base = p->ptrs[s.off];
    if (base == nullptr && s.len > 0) {
      return fail("gav8: null pinned span pointer with a non-zero length");
    }
  } else {
    return fail("gav8: unknown span kind");
  }

  // V8's factories reject a null data pointer even for an empty string, and a
  // zero-length span legitimately has one.
  *data = base != nullptr ? static_cast<const uint8_t*>(base)
                          : reinterpret_cast<const uint8_t*>("");
  *len = static_cast<int>(s.len);
  return true;
}

// Interns a shape's keys once, on first use. Every object of that shape then
// reuses the same handles, so building N objects does no string work after the
// first.
bool builder::intern_shape(uint32_t id) {
  if (id >= static_cast<uint32_t>(p->nshapes)) {
    return fail("gav8: shape id out of range");
  }
  if (shape_interned[id]) {
    return true;
  }
  const gav8_shapedef def = p->shapes[id];
  if (static_cast<uint64_t>(def.first) + def.n >
      static_cast<uint64_t>(p->nkeyspans)) {
    return fail("gav8: shape keys fall outside the key-span region");
  }

  LocalVector<Name>& names = shape_keys[id];
  names.reserve(def.n);

  // Duplicate keys would leave V8 building a map with a repeated property.
  // Checking here costs once per shape rather than once per object.
  std::unordered_set<std::string> seen;
  seen.reserve(def.n);

  for (uint32_t i = 0; i < def.n; i++) {
    const uint8_t* data;
    int len;
    if (!span_bytes(def.first + i, &data, &len)) {
      return false;
    }
    if (!seen.emplace(reinterpret_cast<const char*>(data),
                      static_cast<size_t>(len))
             .second) {
      return fail("gav8: duplicate key in shape");
    }
    Local<String> name;
    if (!String::NewFromUtf8(iso, reinterpret_cast<const char*>(data),
                             NewStringType::kInternalized, len)
             .ToLocal(&name)) {
      return fail("gav8: could not intern shape key");
    }
    names.push_back(name);
  }

  shape_interned[id] = 1;
  return true;
}

// Runs the program. On success the root is the single value left on the stack.
bool builder::run() {
  const uint32_t nops = static_cast<uint32_t>(p->nops);
  std::vector<frame> frames;
  uint32_t pc = 0;

  for (;;) {
    // Close out any bodies that end here, innermost first. A pop leaves pc at
    // the body's end, which is the op after it, so the outer frame's own end
    // may land on the same word — hence the loop. A repeat that jumps back
    // ends it: its start is below every open frame's end.
    while (!frames.empty() && pc == frames.back().end) {
      if (frames.back().kind != FRAME_REPEAT) {
        const frame& f = frames.back();
        const char* name = frame_op_name(f.kind);
        // The one thing a conditional body owes its caller. Zero or two pushes
        // would shift the enclosing OP_OBJ's operands by one and build a
        // plausible, wrong tree; the flag path would build the right one.
        if (stack.size() != f.depth + 1) {
          return fail(std::string("gav8: ") + name + " body left " +
                      std::to_string(static_cast<int64_t>(stack.size()) -
                                     static_cast<int64_t>(f.depth)) +
                      " value(s) on the stack, want exactly 1");
        }
        if (marks.size() != f.nmarks) {
          return fail(std::string("gav8: ") + name +
                      " body did not balance its OP_MARK(s)");
        }
        floor = f.saved_floor;
        frames.pop_back();
        continue;
      }
      if (--frames.back().remaining > 0) {
        pc = frames.back().start;
      } else {
        frames.pop_back();
      }
    }
    if (pc >= nops) {
      return fail("gav8: op buffer ended without OP_END");
    }

    const uint32_t op = p->ops[pc++];
    switch (op) {
      case GAV8_OP_NULL:
        stack.push_back(Null(iso));
        break;

      case GAV8_OP_UNDEF:
        stack.push_back(Undefined(iso));
        break;

      case GAV8_OP_TRUE:
        stack.push_back(True(iso));
        break;

      case GAV8_OP_FALSE:
        stack.push_back(False(iso));
        break;

      case GAV8_OP_BOOL: {
        if (num_cur >= static_cast<uint32_t>(p->nnums)) {
          return fail("gav8: nums cursor out of range at OP_BOOL");
        }
        stack.push_back(Boolean::New(iso, p->nums[num_cur++] != 0));
        break;
      }

      case GAV8_OP_INT: {
        if (num_cur >= static_cast<uint32_t>(p->nnums)) {
          return fail("gav8: nums cursor out of range at OP_INT");
        }
        const int64_t v = p->nums[num_cur++];
        // A Smi where one fits, a heap number otherwise. Past 2^53 a double is
        // lossy, but that is also what JSON.parse would produce for the same
        // literal, so the two paths agree.
        if (v >= INT32_MIN && v <= INT32_MAX) {
          stack.push_back(Integer::New(iso, static_cast<int32_t>(v)));
        } else {
          stack.push_back(Number::New(iso, static_cast<double>(v)));
        }
        break;
      }

      case GAV8_OP_F64: {
        if (flt_cur >= static_cast<uint32_t>(p->nfloats)) {
          return fail("gav8: floats cursor out of range at OP_F64");
        }
        stack.push_back(Number::New(iso, p->floats[flt_cur++]));
        break;
      }

      case GAV8_OP_STR: {
        const uint8_t* data;
        int len;
        if (!span_bytes(span_cur, &data, &len)) {
          return false;
        }
        span_cur++;
        Local<String> str;
        if (!gav8_new_string(iso, data, len).ToLocal(&str)) {
          return fail("gav8: could not create string");
        }
        stack.push_back(str);
        break;
      }

      case GAV8_OP_BYTES: {
        const uint8_t* data;
        int len;
        if (!span_bytes(span_cur, &data, &len)) {
          return false;
        }
        span_cur++;
        // Uninitialized: the memcpy below writes every byte, so zeroing first
        // would be a second pass over the whole buffer.
        Local<ArrayBuffer> buf =
            ArrayBuffer::New(iso, static_cast<size_t>(len),
                             BackingStoreInitializationMode::kUninitialized);
        if (len > 0) {
          memcpy(buf->Data(), data, static_cast<size_t>(len));
        }
        stack.push_back(Uint8Array::New(buf, 0, static_cast<size_t>(len)));
        break;
      }

      case GAV8_OP_OBJ: {
        if (pc >= nops) {
          return fail("gav8: OP_OBJ without a shape id");
        }
        const uint32_t id = p->ops[pc++];
        if (!intern_shape(id)) {
          return false;
        }
        LocalVector<Name>& names = shape_keys[id];
        const size_t n = names.size();
        if (n == 0) {
          // Object::New(iso) already yields {} with Object.prototype, and
          // avoids handing the bulk constructor a pair of empty data pointers.
          stack.push_back(Object::New(iso));
          break;
        }
        if (stack.size() < n) {
          return fail("gav8: stack underflow at OP_OBJ");
        }
        const size_t base = stack.size() - n;
        // Popping past the innermost mark would steal values belonging to an
        // array still being gathered.
        if (!marks.empty() && base < marks.back()) {
          return fail("gav8: OP_OBJ pops past an OP_MARK");
        }
        // Same theft, out through a conditional body instead: the stolen value
        // was pushed before the flag was read, so it exists on one path only.
        if (base < floor) {
          return fail(std::string("gav8: OP_OBJ pops out of ") +
                      kConditionalBody);
        }
        if (!ensure_obj_proto()) {
          return fail("gav8: could not resolve Object.prototype");
        }

        // One call per object with the shape's already-interned keys, instead
        // of n property stores. This constructor produces DICTIONARY-MODE
        // objects — %HasFastProperties is false here and true for the same
        // data out of JSON.parse — which costs the reader about 1.85x on
        // property loads (measured; see TestBuildReadCostRatio). Against a
        // build saving of ~1.4ms at 20x1000 that is break-even at ~80 full
        // passes over the data, and an SSR render reads it once. The
        // alternative, Object::New then n CreateDataProperty calls, gives fast
        // properties and read parity at ~2.2us per object against ~0.68us
        // here, which is worse than the read penalty it removes.
        Local<Object> obj =
            Object::New(iso, obj_proto, names.data(), stack.data() + base, n);
        stack[base] = obj;
        stack.resize(base + 1);
        break;
      }

      // OP_OBJ, minus the pairs whose value is undefined. It pops the same
      // fixed arity from the same shape — the program is still O(type), and a
      // producer still pushes something for every field — and the key set is
      // decided by the values rather than by the shape.
      case GAV8_OP_OBJ_OMIT: {
        if (pc >= nops) {
          return fail("gav8: OP_OBJ_OMIT without a shape id");
        }
        const uint32_t id = p->ops[pc++];
        if (!intern_shape(id)) {
          return false;
        }
        LocalVector<Name>& names = shape_keys[id];
        const size_t n = names.size();
        if (n == 0) {
          stack.push_back(Object::New(iso));
          break;
        }
        if (stack.size() < n) {
          return fail("gav8: stack underflow at OP_OBJ_OMIT");
        }
        const size_t base = stack.size() - n;
        if (!marks.empty() && base < marks.back()) {
          return fail("gav8: OP_OBJ_OMIT pops past an OP_MARK");
        }
        if (base < floor) {
          return fail(std::string("gav8: OP_OBJ_OMIT pops out of ") +
                      kConditionalBody);
        }
        if (!ensure_obj_proto()) {
          return fail("gav8: could not resolve Object.prototype");
        }

        // Find the first omitted field. Undefined is the one sentinel, and no
        // leaf op produces one by accident: OP_UNDEF and an absent OP_OPTIONAL
        // are the only two ways to get one, and inside this shape both mean
        // exactly this.
        size_t first_gap = 0;
        while (first_gap < n && !stack[base + first_gap]->IsUndefined()) {
          first_gap++;
        }

        Local<Object> obj;
        if (first_gap == n) {
          // Nothing omitted, which is the common case: a struct with optional
          // fields still arrives with all of them filled most of the time. It
          // IS an OP_OBJ, so it takes OP_OBJ's path exactly — the shape's own
          // interned key array, no scratch and no compaction — and the whole
          // difference between the two ops on that data is the scan above.
          obj =
              Object::New(iso, obj_proto, names.data(), stack.data() + base, n);
        } else {
          // Compact the survivors down over the popped region and collect
          // their keys, both in shape order, so the bulk constructor gets the
          // two contiguous arrays it wants. Values move in place; keys cannot,
          // because the shape's interned array is shared by every object of
          // that shape. Everything before the first gap is already in place.
          kept_keys.clear();
          kept_keys.insert(kept_keys.end(), names.begin(),
                           names.begin() + first_gap);
          size_t kept = first_gap;
          for (size_t i = first_gap + 1; i < n; i++) {
            const Local<Value> v = stack[base + i];
            if (v->IsUndefined()) {
              continue;
            }
            stack[base + kept] = v;
            kept_keys.push_back(names[i]);
            kept++;
          }
          // Every field absent. A struct whose optional fields are all empty
          // is an ordinary payload, not an edge case, and {} is what
          // JSON.parse would give for the same data.
          obj = kept == 0 ? Object::New(iso)
                          : Object::New(iso, obj_proto, kept_keys.data(),
                                        stack.data() + base, kept);
        }
        stack[base] = obj;
        stack.resize(base + 1);
        break;
      }

      case GAV8_OP_MARK:
        marks.push_back(stack.size());
        break;

      case GAV8_OP_ARR_FROM_MARK: {
        if (marks.empty()) {
          return fail("gav8: OP_ARR_FROM_MARK without an OP_MARK");
        }
        const size_t base = marks.back();
        if (base < floor) {
          return fail(
              std::string(
                  "gav8: OP_ARR_FROM_MARK closes an OP_MARK from outside ") +
              kConditionalBody);
        }
        marks.pop_back();
        if (base > stack.size()) {
          return fail("gav8: OP_ARR_FROM_MARK with a mark above the stack top");
        }
        const size_t n = stack.size() - base;
        if (n == 0) {
          stack.push_back(Array::New(iso, 0));
          break;
        }
        Local<Array> arr = Array::New(iso, stack.data() + base, n);
        stack[base] = arr;
        stack.resize(base + 1);
        break;
      }

      case GAV8_OP_REPEAT: {
        if (pc >= nops) {
          return fail("gav8: OP_REPEAT without a body length");
        }
        const uint32_t body_len = p->ops[pc++];
        // An empty body would spin: the frame would retire one iteration per
        // pass with nothing between, up to 2^31 times. Nothing legitimate
        // emits it.
        if (body_len == 0) {
          return fail("gav8: OP_REPEAT with an empty body");
        }
        const uint64_t end = static_cast<uint64_t>(pc) + body_len;
        if (end > static_cast<uint64_t>(nops)) {
          return fail(
              "gav8: OP_REPEAT body runs past the end of the op buffer");
        }
        if (cnt_cur >= static_cast<uint32_t>(p->ncounts)) {
          return fail("gav8: counts cursor out of range at OP_REPEAT");
        }
        const int32_t n = p->counts[cnt_cur++];
        if (n < 0) {
          return fail("gav8: negative OP_REPEAT count");
        }
        if (n == 0) {
          // Skip the body outright. Every other cursor stays where it was,
          // which is what makes an empty slice cost nothing.
          pc = static_cast<uint32_t>(end);
          break;
        }
        frames.push_back(
            {pc, static_cast<uint32_t>(end), FRAME_REPEAT, n, 0, 0, 0});
        break;
      }

      // One implementation, two absent values. OP_OPTIONAL is OP_NULLABLE with
      // undefined in place of null, and everything else about them — the frame,
      // the body-length checks, the skip rule, the exactly-one-value contract —
      // is the same, so they share it rather than being kept in step by hand.
      case GAV8_OP_NULLABLE:
      case GAV8_OP_OPTIONAL: {
        const bool nullable = op == GAV8_OP_NULLABLE;
        const char* name = nullable ? "OP_NULLABLE" : "OP_OPTIONAL";
        if (pc >= nops) {
          return fail(std::string("gav8: ") + name + " without a body length");
        }
        const uint32_t body_len = p->ops[pc++];
        // A body that pushes nothing cannot satisfy the exactly-one rule, so an
        // empty one is a producer bug rather than an absent value of some other
        // kind.
        if (body_len == 0) {
          return fail(std::string("gav8: ") + name + " with an empty body");
        }
        const uint64_t end = static_cast<uint64_t>(pc) + body_len;
        if (end > static_cast<uint64_t>(nops)) {
          return fail(std::string("gav8: ") + name +
                      " body runs past the end of the op buffer");
        }
        // Both checks above precede the count, so a malformed op does not half
        // consume the payload — same order as OP_REPEAT.
        if (cnt_cur >= static_cast<uint32_t>(p->ncounts)) {
          return fail(std::string("gav8: counts cursor out of range at ") +
                      name);
        }
        // Only zero is absent; the flag is a present/absent bit, not a length.
        if (p->counts[cnt_cur++] == 0) {
          // Skip the body outright, exactly as an n == 0 OP_REPEAT does. No
          // other cursor moves, because the producer staged nothing here.
          stack.push_back(nullable ? Local<Primitive>(Null(iso))
                                   : Local<Primitive>(Undefined(iso)));
          pc = static_cast<uint32_t>(end);
          break;
        }
        frames.push_back({pc, static_cast<uint32_t>(end),
                          nullable ? FRAME_NULLABLE : FRAME_OPTIONAL, 0,
                          stack.size(), marks.size(), floor});
        floor = stack.size();
        break;
      }

      case GAV8_OP_END: {
        if (!frames.empty()) {
          return fail(std::string("gav8: OP_END inside an ") +
                      frame_op_name(frames.back().kind) + " body");
        }
        if (!marks.empty()) {
          return fail("gav8: OP_END with " + std::to_string(marks.size()) +
                      " unclosed OP_MARK(s)");
        }
        if (stack.size() != 1) {
          return fail("gav8: OP_END with " + std::to_string(stack.size()) +
                      " values on the stack, want exactly 1");
        }
        return true;
      }

      default:
        return fail("gav8: unknown opcode " + std::to_string(op));
    }
  }
}

}  // namespace

RtnValue gav8_build(ContextPtr ctx, const gav8_payload* p) {
  g_build_calls.fetch_add(1, std::memory_order_relaxed);

  RtnValue rtn = {};
  if (ctx == nullptr) {
    rtn.error.msg = CopyString("gav8: nil context");
    return rtn;
  }
  if (const char* err = validate_payload(p)) {
    rtn.error.msg = CopyString(err);
    return rtn;
  }

  builder b(ctx, p);
  if (!b.run()) {
    rtn.error.msg = CopyString(b.failure);
    return rtn;
  }

  // The one and only tracked value this API creates. Every other node in the
  // tree dies with the builder's HandleScope on the way out.
  m_value* val = new m_value;
  val->id = 0;
  val->iso = b.iso;
  val->ctx = ctx;
  val->ptr = Global<Value>(b.iso, b.stack[0]);

  rtn.value = tracked_value(ctx, val);
  return rtn;
}

RtnValue gav8_build_args(ContextPtr ctx,
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
                         int ncounts) {
  gav8_payload p;
  p.ops = ops;
  p.nops = nops;
  p.shapes = shapes;
  p.nshapes = nshapes;
  p.buf = buf;
  p.buflen = buflen;
  p.spans = spans;
  p.nspans = nspans;
  p.nkeyspans = nkeyspans;
  // Same representation, different declared type: see the note in the header
  // on why the cgo-facing signature takes addresses rather than pointers.
  p.ptrs = reinterpret_cast<const void* const*>(ptrs);
  p.nptrs = nptrs;
  p.nums = nums;
  p.nnums = nnums;
  p.floats = floats;
  p.nfloats = nfloats;
  p.counts = counts;
  p.ncounts = ncounts;
  return gav8_build(ctx, &p);
}

uint64_t gav8_build_call_count(void) {
  return g_build_calls.load(std::memory_order_relaxed);
}

void gav8_build_reset_call_count(void) {
  g_build_calls.store(0, std::memory_order_relaxed);
}
