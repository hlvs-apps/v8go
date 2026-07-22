// Copyright 2024 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"bytes"
	"testing"

	v8 "github.com/katallaxie/v8go"
)

func TestValueArrayBufferViewBytes(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	tests := []struct {
		name   string
		script string
		want   []byte
	}{
		{
			name:   "Uint8Array",
			script: "new Uint8Array([1, 2, 3, 255])",
			want:   []byte{1, 2, 3, 255},
		},
		{
			// The accessor must respect byteOffset/byteLength and only copy the
			// bytes the view actually spans, not the whole backing buffer.
			name: "Uint8Array with offset",
			script: `(() => {
				const buf = new ArrayBuffer(8);
				const full = new Uint8Array(buf);
				for (let i = 0; i < 8; i++) full[i] = i + 1;
				return new Uint8Array(buf, 2, 3);
			})()`,
			want: []byte{3, 4, 5},
		},
		{
			// Multi-byte typed arrays expose their raw little-endian bytes.
			name:   "Int16Array",
			script: "new Int16Array([1, 2, -1])",
			want:   []byte{1, 0, 2, 0, 255, 255},
		},
		{
			name: "DataView",
			script: `(() => {
				const dv = new DataView(new ArrayBuffer(4));
				dv.setUint8(0, 0xAA);
				dv.setUint8(1, 0xBB);
				dv.setUint8(2, 0xCC);
				dv.setUint8(3, 0xDD);
				return dv;
			})()`,
			want: []byte{0xAA, 0xBB, 0xCC, 0xDD},
		},
		{
			name:   "empty Uint8Array",
			script: "new Uint8Array(0)",
			want:   []byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := ctx.RunScript(tt.script, "view.js")
			fatalIf(t, err)

			if !val.IsArrayBufferView() {
				t.Fatalf("expected script result to be an ArrayBufferView")
			}

			got := val.ArrayBufferViewBytes()
			if got == nil {
				t.Fatalf("ArrayBufferViewBytes returned nil for an ArrayBufferView")
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("ArrayBufferViewBytes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValueArrayBufferViewBytesNotAView(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// A plain value, and an ArrayBuffer (which is not itself an
	// ArrayBufferView), must both return nil.
	for _, script := range []string{"7", "'hello'", "new ArrayBuffer(4)"} {
		val, err := ctx.RunScript(script, "notaview.js")
		fatalIf(t, err)
		if got := val.ArrayBufferViewBytes(); got != nil {
			t.Errorf("ArrayBufferViewBytes() for %q = %v, want nil", script, got)
		}
	}
}
