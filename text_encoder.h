// Copyright 2024 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

#ifndef V8GO_TEXT_ENCODER_H
#define V8GO_TEXT_ENCODER_H

#ifdef __cplusplus

namespace v8 {
class Isolate;
template <class F>
class FunctionCallbackInfo;
class Value;
}  // namespace v8

void TextEncoderEncodeCallback(const v8::FunctionCallbackInfo<v8::Value>& info);

typedef v8::Isolate v8Isolate;

extern "C" {
#else

typedef struct v8Isolate v8Isolate;

#endif

typedef struct m_template m_template;

// NewTextEncoderTemplate returns a FunctionTemplate whose callback is
// implemented entirely in C++: it takes a single string argument (coerced to a
// string if necessary) and returns a Uint8Array holding the UTF-8 encoding of
// that string. Because the callback never crosses back into Go, calling the
// resulting function is cheap (~100ns) — the same mechanism Node.js uses for
// its native TextEncoder.
extern m_template* NewTextEncoderTemplate(v8Isolate* iso_ptr);

#ifdef __cplusplus
}  // extern "C"
#endif
#endif
