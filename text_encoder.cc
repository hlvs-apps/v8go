// Copyright 2024 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

#include "text_encoder.h"

#include <memory>

#include "deps/include/v8-array-buffer.h"
#include "deps/include/v8-context.h"
#include "deps/include/v8-function-callback.h"
#include "deps/include/v8-isolate.h"
#include "deps/include/v8-local-handle.h"
#include "deps/include/v8-locker.h"
#include "deps/include/v8-primitive.h"
#include "deps/include/v8-template.h"
#include "deps/include/v8-typed-array.h"

#include "template.h"

using namespace v8;

// TextEncoderEncodeCallback is the native implementation of a TextEncoder-style
// `encode(string) -> Uint8Array`. It runs entirely in C++ and never enters Go,
// so it is safe (and fast) to call on a hot path.
void TextEncoderEncodeCallback(const FunctionCallbackInfo<Value>& info) {
  Isolate* iso = info.GetIsolate();
  HandleScope handle_scope(iso);

  Local<String> str;
  if (info.Length() < 1) {
    str = String::Empty(iso);
  } else if (info[0]->IsString()) {
    str = info[0].As<String>();
  } else {
    // Match TextEncoder semantics: coerce non-string arguments via ToString.
    if (!info[0]->ToString(iso->GetCurrentContext()).ToLocal(&str)) {
      // ToString scheduled an exception; propagate it by returning.
      return;
    }
  }

  // Utf8LengthV2 reports the exact number of bytes WriteUtf8V2 will emit,
  // including the 3-byte replacement character for any lone surrogate, so the
  // backing store is sized exactly and written without truncation.
  size_t byte_length = str->Utf8LengthV2(iso);
  std::unique_ptr<BackingStore> backing_store =
      ArrayBuffer::NewBackingStore(iso, byte_length);
  if (byte_length > 0) {
    str->WriteUtf8V2(iso, static_cast<char*>(backing_store->Data()),
                     byte_length, String::WriteFlags::kReplaceInvalidUtf8);
  }

  Local<ArrayBuffer> array_buffer =
      ArrayBuffer::New(iso, std::move(backing_store));
  Local<Uint8Array> result = Uint8Array::New(array_buffer, 0, byte_length);
  info.GetReturnValue().Set(result);
}

extern "C" {

m_template* NewTextEncoderTemplate(v8Isolate* iso) {
  Locker locker(iso);
  Isolate::Scope isolate_scope(iso);
  HandleScope handle_scope(iso);

  m_template* ot = new m_template;
  ot->iso = iso;
  ot->ptr.Reset(iso, FunctionTemplate::New(iso, TextEncoderEncodeCallback));
  return ot;
}

}  // extern "C"
