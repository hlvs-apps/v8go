// Copyright 2024 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	v8 "github.com/hlvs-apps/v8go"
)

// joinBytes renders a byte slice the same way Array.from(uint8array).join(',')
// does in JS, so the two can be compared as strings.
func joinBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = strconv.Itoa(int(v))
	}
	return strings.Join(parts, ",")
}

func TestContextInstallTextEncoder(t *testing.T) {
	// Subtests share iso/ctx and the __input global, so this test is
	// intentionally not parallel.
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	if err := ctx.InstallTextEncoder("__hostTextEncode"); err != nil {
		t.Fatalf("InstallTextEncoder returned error: %v", err)
	}

	tests := []struct {
		name  string
		input string
		want  []byte
	}{
		{"ascii", "hello world", []byte("hello world")},
		{"empty", "", []byte{}},
		{"latin1", "héllo", []byte("héllo")},
		{"euro", "€", []byte("€")},
		{"emoji", "a😀b", []byte("a😀b")},
		{"nul", "a\x00b", []byte("a\x00b")},
	}

	global := ctx.Global()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := v8.NewValue(iso, tt.input)
			fatalIf(t, err)
			fatalIf(t, global.Set("__input", input))

			// Read the result back through JS itself so this test is
			// independent of ArrayBufferViewBytes.
			got, err := ctx.RunScript("Array.from(__hostTextEncode(__input)).join(',')", "encode.js")
			fatalIf(t, err)

			if want := joinBytes(tt.want); got.String() != want {
				t.Errorf("__hostTextEncode(%q) = [%s], want [%s]", tt.input, got.String(), want)
			}

			// The result must be a real Uint8Array of the right length.
			isU8, err := ctx.RunScript("__hostTextEncode(__input) instanceof Uint8Array", "check.js")
			fatalIf(t, err)
			if !isU8.Boolean() {
				t.Errorf("__hostTextEncode(%q) did not return a Uint8Array", tt.input)
			}
		})
	}
}

// TestContextInstallTextEncoderLoneSurrogate verifies the WHATWG TextEncoder
// behaviour of replacing lone surrogates with U+FFFD (0xEF 0xBF 0xBD).
func TestContextInstallTextEncoderLoneSurrogate(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	fatalIf(t, ctx.InstallTextEncoder("__enc"))

	got, err := ctx.RunScript("Array.from(__enc(String.fromCharCode(0xD800))).join(',')", "sur.js")
	fatalIf(t, err)
	if want := joinBytes([]byte{0xEF, 0xBF, 0xBD}); got.String() != want {
		t.Errorf("lone surrogate encoded to [%s], want [%s]", got.String(), want)
	}
}

// TestContextInstallTextEncoderCoercion verifies non-string arguments are
// coerced via ToString, matching TextEncoder.encode.
func TestContextInstallTextEncoderCoercion(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	fatalIf(t, ctx.InstallTextEncoder("__enc"))

	got, err := ctx.RunScript("Array.from(__enc(123)).join(',')", "coerce.js")
	fatalIf(t, err)
	if want := joinBytes([]byte("123")); got.String() != want {
		t.Errorf("__enc(123) = [%s], want [%s]", got.String(), want)
	}

	// No argument at all encodes to the empty buffer.
	got, err = ctx.RunScript("__enc().length", "noarg.js")
	fatalIf(t, err)
	if got.Integer() != 0 {
		t.Errorf("__enc().length = %d, want 0", got.Integer())
	}
}

// TestNewTextEncoderTemplate exercises the lower-level template API and, in
// doing so, checks the round trip through ArrayBufferViewBytes.
func TestNewTextEncoderTemplate(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	tmpl := v8.NewTextEncoderTemplate(iso)
	fn := tmpl.GetFunction(ctx)

	arg, err := v8.NewValue(iso, "hello €")
	fatalIf(t, err)

	res, err := fn.Call(v8.Undefined(iso), arg)
	fatalIf(t, err)

	if !res.IsUint8Array() {
		t.Fatalf("expected a Uint8Array result")
	}
	if got := res.ArrayBufferViewBytes(); !bytes.Equal(got, []byte("hello €")) {
		t.Errorf("encoded bytes = %v, want %v", got, []byte("hello €"))
	}
}
