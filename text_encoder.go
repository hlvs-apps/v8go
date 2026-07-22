// Copyright 2024 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include "text_encoder.h"
import "C"

import "runtime"

// NewTextEncoderTemplate returns a FunctionTemplate for a native UTF-8 text
// encoder. The resulting function takes a single string argument (any non-string
// argument is coerced with ToString) and returns a Uint8Array holding the
// string's UTF-8 encoding.
//
// The callback is implemented entirely in C++ and never crosses back into Go,
// so — unlike a function created with NewFunctionTemplate — invoking it does not
// pay the cgo callback cost and is cheap enough to call on a hot path. It is the
// same mechanism Node.js uses for its own native TextEncoder.
//
// Use GetFunction to bind the template to a context, or InstallTextEncoder for
// the common case of exposing it as a named global.
func NewTextEncoderTemplate(iso *Isolate) *FunctionTemplate {
	if iso == nil {
		panic("nil Isolate argument not supported")
	}

	tmpl := &template{
		ptr: C.NewTextEncoderTemplate(iso.ptr),
		iso: iso,
	}
	runtime.SetFinalizer(tmpl, (*template).finalizer)
	return &FunctionTemplate{tmpl}
}

// InstallTextEncoder installs the native UTF-8 text encoder (see
// NewTextEncoderTemplate) as a function property named name on the context's
// global object. It is meant to be called once per context, typically before
// running any script that relies on it (for example a TextEncoder polyfill that
// delegates to a host-provided encode hook).
func (c *Context) InstallTextEncoder(name string) error {
	tmpl := NewTextEncoderTemplate(c.iso)
	fn := tmpl.GetFunction(c)
	return c.Global().Set(name, fn)
}
