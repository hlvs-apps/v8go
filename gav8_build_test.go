// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"bytes"
	"math"
	"runtime"
	"strconv"
	"testing"
	"time"
	"unsafe"

	v8 "github.com/hlvs-apps/v8go"
)

/********** program assembly **********/

// prog assembles a build program and the payload it indexes.
//
// The two halves are deliberately separable. In a straight-line program the
// ops and the data are emitted together, and the combined helpers below do
// that. Inside an OpRepeat body they are not: the body's ops are emitted once
// and its data is appended once per iteration, which is the entire reason the
// op buffer is O(type complexity) instead of O(data size). Tests that use
// OpRepeat emit the body with op and then append the data with the data*
// helpers, in execution order.
type prog struct {
	ops      []uint32
	shapes   []v8.ShapeDef
	buf      []byte
	keySpans []v8.Span
	valSpans []v8.Span
	ptrs     []unsafe.Pointer
	nums     []int64
	floats   []float64
	counts   []int32

	pinner runtime.Pinner
	held   [][]byte
}

// release unpins everything the program pinned. Every prog needs a deferred
// call to it, or the pinned leaves outlive their guarantee.
func (p *prog) release() { p.pinner.Unpin() }

func (p *prog) payload() *v8.Payload {
	spans := make([]v8.Span, 0, len(p.keySpans)+len(p.valSpans))
	spans = append(spans, p.keySpans...)
	spans = append(spans, p.valSpans...)
	return &v8.Payload{
		Ops:      p.ops,
		Shapes:   p.shapes,
		Buf:      p.buf,
		Spans:    spans,
		KeySpans: len(p.keySpans),
		Ptrs:     p.ptrs,
		Nums:     p.nums,
		Floats:   p.floats,
		Counts:   p.counts,
	}
}

func (p *prog) build(t *testing.T, ctx *v8.Context) *v8.Value {
	t.Helper()
	val, err := v8.BuildValue(ctx, p.payload())
	fatalIf(t, err)
	return val
}

// stage copies b into the shared buffer and returns a span pointing at it.
func (p *prog) stage(b []byte) v8.Span {
	off := uint32(len(p.buf))
	p.buf = append(p.buf, b...)
	return v8.Span{Off: off, Len: uint32(len(b)), Kind: v8.SpanStaged}
}

// pin leaves b where it is and pins it, which is what a producer does for a
// leaf too large to be worth copying. The pin lasts until release.
func (p *prog) pin(b []byte) v8.Span {
	idx := uint32(len(p.ptrs))
	if len(b) == 0 {
		p.ptrs = append(p.ptrs, nil)
		return v8.Span{Off: idx, Len: 0, Kind: v8.SpanPinned}
	}
	p.pinner.Pin(&b[0])
	p.held = append(p.held, b)
	p.ptrs = append(p.ptrs, unsafe.Pointer(&b[0]))
	return v8.Span{Off: idx, Len: uint32(len(b)), Kind: v8.SpanPinned}
}

// --- program only ---

func (p *prog) op(words ...uint32) { p.ops = append(p.ops, words...) }

// shape registers an object shape and returns the id an OpObj operand uses.
func (p *prog) shape(keys ...string) uint32 {
	first := uint32(len(p.keySpans))
	for _, k := range keys {
		p.keySpans = append(p.keySpans, p.stage([]byte(k)))
	}
	p.shapes = append(p.shapes, v8.ShapeDef{First: first, N: uint32(len(keys))})
	return uint32(len(p.shapes) - 1)
}

// --- data only ---

func (p *prog) dataStr(s string)    { p.valSpans = append(p.valSpans, p.stage([]byte(s))) }
func (p *prog) dataPinStr(s string) { p.valSpans = append(p.valSpans, p.pin([]byte(s))) }
func (p *prog) dataBytes(b []byte)  { p.valSpans = append(p.valSpans, p.stage(b)) }
func (p *prog) dataPinBytes(b []byte) {
	p.valSpans = append(p.valSpans, p.pin(b))
}
func (p *prog) dataInt(v int64)   { p.nums = append(p.nums, v) }
func (p *prog) dataF64(v float64) { p.floats = append(p.floats, v) }
func (p *prog) dataCount(n int32) { p.counts = append(p.counts, n) }

// dataFlag stages one OpNullable flag. It rides in Counts next to the repeat
// bounds because both are control flow, read in execution order.
func (p *prog) dataFlag(present bool) {
	if present {
		p.dataCount(1)
	} else {
		p.dataCount(0)
	}
}

// --- op and data together, for straight-line programs ---

func (p *prog) null()        { p.op(v8.OpNull) }
func (p *prog) undef()       { p.op(v8.OpUndef) }
func (p *prog) truth()       { p.op(v8.OpTrue) }
func (p *prog) falsity()     { p.op(v8.OpFalse) }
func (p *prog) mark()        { p.op(v8.OpMark) }
func (p *prog) arr()         { p.op(v8.OpArrFromMark) }
func (p *prog) end()         { p.op(v8.OpEnd) }
func (p *prog) obj(s uint32) { p.op(v8.OpObj, s) }

func (p *prog) boolean(v bool) {
	p.op(v8.OpBool)
	if v {
		p.dataInt(1)
	} else {
		p.dataInt(0)
	}
}

func (p *prog) integer(v int64) { p.op(v8.OpInt); p.dataInt(v) }
func (p *prog) float(v float64) { p.op(v8.OpF64); p.dataF64(v) }
func (p *prog) str(s string)    { p.op(v8.OpStr); p.dataStr(s) }
func (p *prog) pinStr(s string) { p.op(v8.OpStr); p.dataPinStr(s) }
func (p *prog) bytesv(b []byte) { p.op(v8.OpBytes); p.dataBytes(b) }
func (p *prog) pinBytes(b []byte) {
	p.op(v8.OpBytes)
	p.dataPinBytes(b)
}

/********** round trip **********/

func TestBuildRoundTrip(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	tests := []struct {
		name  string
		build func(p *prog)
		expr  string
		want  string
	}{
		{
			name:  "null",
			build: func(p *prog) { p.null(); p.end() },
			expr:  "v === null ? 'null' : typeof v",
			want:  "null",
		},
		{
			name:  "undefined",
			build: func(p *prog) { p.undef(); p.end() },
			expr:  "typeof v",
			want:  "undefined",
		},
		{
			name:  "literal true",
			build: func(p *prog) { p.truth(); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "boolean:true",
		},
		{
			name:  "literal false",
			build: func(p *prog) { p.falsity(); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "boolean:false",
		},
		{
			// OpBool reads its value from Nums, where a producer parks
			// booleans as 0/1 rather than giving them an array of their own.
			name:  "bool from nums",
			build: func(p *prog) { p.boolean(true); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "boolean:true",
		},
		{
			name:  "int32 min",
			build: func(p *prog) { p.integer(-2147483648); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "number:-2147483648",
		},
		{
			// Past the Smi range the builder falls back to a heap number,
			// which is also what JSON.parse would produce for the literal.
			name:  "int64 beyond int32",
			build: func(p *prog) { p.integer(9007199254740991); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "number:9007199254740991",
		},
		{
			name:  "float64",
			build: func(p *prog) { p.float(1.5e300); p.end() },
			expr:  "typeof v + ':' + v",
			want:  "number:1.5e+300",
		},
		{
			name:  "float64 NaN",
			build: func(p *prog) { p.float(math.NaN()); p.end() },
			expr:  "Number.isNaN(v) ? 'nan' : String(v)",
			want:  "nan",
		},
		{
			name:  "ascii string",
			build: func(p *prog) { p.str("hello world"); p.end() },
			expr:  "typeof v + ':' + v.length + ':' + v",
			want:  "string:11:hello world",
		},
		{
			name:  "empty string",
			build: func(p *prog) { p.str(""); p.end() },
			expr:  "typeof v + ':' + v.length",
			want:  "string:0",
		},
		{
			name:  "non-ascii utf8",
			build: func(p *prog) { p.str("héllo · 日本 · 😀"); p.end() },
			expr:  "v",
			want:  "héllo · 日本 · 😀",
		},
		{
			// Spans are length-delimited, not NUL-terminated: the NUL is a
			// character like any other.
			name:  "interior NUL",
			build: func(p *prog) { p.str("a\x00b"); p.end() },
			expr:  "v.length + ':' + [...v].map(c => c.charCodeAt(0)).join(',')",
			want:  "3:97,0,98",
		},
		{
			name:  "bytes",
			build: func(p *prog) { p.bytesv([]byte{0, 1, 127, 128, 255}); p.end() },
			expr:  "(v instanceof Uint8Array) + ':' + v.length + ':' + Array.from(v).join(',')",
			want:  "true:5:0,1,127,128,255",
		},
		{
			// Not valid UTF-8 anywhere in it, and it must survive verbatim:
			// OpBytes copies, it does not transcode.
			name:  "non-utf8 bytes",
			build: func(p *prog) { p.bytesv([]byte{0xff, 0xfe, 0x80, 0xc0}); p.end() },
			expr:  "Array.from(v).join(',')",
			want:  "255,254,128,192",
		},
		{
			name:  "empty bytes",
			build: func(p *prog) { p.bytesv(nil); p.end() },
			expr:  "(v instanceof Uint8Array) + ':' + v.length",
			want:  "true:0",
		},
		{
			name:  "empty object",
			build: func(p *prog) { s := p.shape(); p.obj(s); p.end() },
			expr:  "JSON.stringify(v) + ':' + (Object.getPrototypeOf(v) === Object.prototype)",
			want:  "{}:true",
		},
		{
			name:  "empty array",
			build: func(p *prog) { p.mark(); p.arr(); p.end() },
			expr:  "Array.isArray(v) + ':' + v.length",
			want:  "true:0",
		},
		{
			name: "flat object",
			build: func(p *prog) {
				s := p.shape("id", "name", "ok")
				p.integer(7)
				p.str("seven")
				p.truth()
				p.obj(s)
				p.end()
			},
			expr: "JSON.stringify(v) + ':' + v.hasOwnProperty('id')",
			want: `{"id":7,"name":"seven","ok":true}:true`,
		},
		{
			name: "nested three deep",
			build: func(p *prog) {
				leaf := p.shape("deep")
				mid := p.shape("inner")
				top := p.shape("outer")
				// The array collects whatever was pushed since the mark, so
				// the mark has to precede the object it will collect.
				p.mark()
				p.str("bottom")
				p.obj(leaf)
				p.arr()
				p.obj(mid)
				p.obj(top)
				p.end()
			},
			expr: "JSON.stringify(v)",
			want: `{"outer":{"inner":[{"deep":"bottom"}]}}`,
		},
		{
			name: "object with 50 keys",
			build: func(p *prog) {
				keys := make([]string, 50)
				for i := range keys {
					keys[i] = "k" + strconv.Itoa(i)
				}
				s := p.shape(keys...)
				for i := range keys {
					p.integer(int64(i))
				}
				p.obj(s)
				p.end()
			},
			expr: "Object.keys(v).length + ':' + Object.keys(v).join(',') + ':' + Object.values(v).join(',')",
			want: "50:" + joinRange("k", 50) + ":" + joinRange("", 50),
		},
		{
			name: "mixed array",
			build: func(p *prog) {
				p.mark()
				p.null()
				p.undef()
				p.falsity()
				p.integer(3)
				p.float(0.5)
				p.str("x")
				p.mark()
				p.arr()
				p.arr()
				p.end()
			},
			expr: "v.map(x => typeof x).join(',')",
			want: "object,undefined,boolean,number,number,string,object",
		},
		{
			// The whole point: seven ops, one thousand rows.
			name: "1000 uniform objects from one repeat",
			build: func(p *prog) {
				s := p.shape("i", "s")
				p.mark()
				p.op(v8.OpRepeat, 4)
				p.op(v8.OpInt)
				p.op(v8.OpStr)
				p.op(v8.OpObj, s)
				p.arr()
				p.end()

				p.dataCount(1000)
				for i := 0; i < 1000; i++ {
					p.dataInt(int64(i))
					p.dataStr("row-" + strconv.Itoa(i))
				}
			},
			expr: `v.length + ':' + v[0].i + ':' + v[999].s + ':' +
				v.every((o, i) => o.i === i && o.s === 'row-' + i)`,
			want: "1000:0:row-999:true",
		},
		{
			// Nested repeats: 3 groups, each holding a different number of
			// rows. The inner count is read once per outer iteration, so the
			// counts array interleaves with the outer loop.
			name: "nested repeat",
			build: func(p *prog) {
				s := p.shape("n")
				p.mark()
				p.op(v8.OpRepeat, 7) // outer body: mark, repeat, 3 ops, arr
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 3)
				p.op(v8.OpInt)
				p.op(v8.OpObj, s)
				p.op(v8.OpArrFromMark)
				p.arr()
				p.end()

				groups := [][]int64{{1, 2}, {}, {3, 4, 5}}
				p.dataCount(int32(len(groups)))
				for _, g := range groups {
					p.dataCount(int32(len(g)))
					for _, n := range g {
						p.dataInt(n)
					}
				}
			},
			expr: "JSON.stringify(v)",
			want: `[[{"n":1},{"n":2}],[],[{"n":3},{"n":4},{"n":5}]]`,
		},
		{
			// A repeat that runs zero times must not touch any cursor but its
			// own count, so the value that follows it reads the first entry.
			name: "repeat with n == 0 leaves cursors alone",
			build: func(p *prog) {
				p.mark()
				p.op(v8.OpRepeat, 1)
				p.op(v8.OpInt)
				p.op(v8.OpInt)
				p.arr()
				p.end()

				p.dataCount(0)
				p.dataInt(42)
			},
			expr: "JSON.stringify(v)",
			want: "[42]",
		},
		{
			// OpNullable, the *T encoding: the flag says which of the two
			// happens, and the payload only carries what it needs.
			name: "nullable primitive present",
			build: func(p *prog) {
				p.op(v8.OpNullable, 1)
				p.op(v8.OpInt)
				p.end()

				p.dataFlag(true)
				p.dataInt(42)
			},
			expr: "typeof v + ':' + v",
			want: "number:42",
		},
		{
			// Nothing at all in Nums: a producer stages no payload for a value
			// it is not sending, so the null path must not look for one.
			name: "nullable primitive absent",
			build: func(p *prog) {
				p.op(v8.OpNullable, 1)
				p.op(v8.OpInt)
				p.end()

				p.dataFlag(false)
			},
			expr: "v === null ? 'null' : typeof v + ':' + v",
			want: "null",
		},
		{
			name: "nullable string absent",
			build: func(p *prog) {
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataFlag(false)
			},
			expr: "v === null ? 'null' : typeof v",
			want: "null",
		},
		{
			// Only zero is null. The flag is a present bit, not a count, and
			// nothing about it is a length.
			name: "nullable flag other than 1 is present",
			build: func(p *prog) {
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataCount(-1)
				p.dataStr("present")
			},
			expr: "typeof v + ':' + v",
			want: "string:present",
		},
		{
			name: "nullable struct present",
			build: func(p *prog) {
				s := p.shape("id", "name")
				p.op(v8.OpNullable, 4)
				p.op(v8.OpInt)
				p.op(v8.OpStr)
				p.op(v8.OpObj, s)
				p.end()

				p.dataFlag(true)
				p.dataInt(3)
				p.dataStr("three")
			},
			expr: "JSON.stringify(v)",
			want: `{"id":3,"name":"three"}`,
		},
		{
			name: "nullable struct absent",
			build: func(p *prog) {
				s := p.shape("id", "name")
				p.op(v8.OpNullable, 4)
				p.op(v8.OpInt)
				p.op(v8.OpStr)
				p.op(v8.OpObj, s)
				p.end()

				p.dataFlag(false)
			},
			expr: "JSON.stringify(v)",
			want: "null",
		},
		{
			name: "nullable array present",
			build: func(p *prog) {
				p.op(v8.OpNullable, 5)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 1)
				p.op(v8.OpInt)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(true)
				p.dataCount(3)
				p.dataInt(1)
				p.dataInt(2)
				p.dataInt(3)
			},
			expr: "JSON.stringify(v)",
			want: "[1,2,3]",
		},
		{
			// The skipped body holds an OpRepeat, whose count is also not
			// staged: the null path may not read Counts past its own flag.
			name: "nullable array absent",
			build: func(p *prog) {
				p.op(v8.OpNullable, 5)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 1)
				p.op(v8.OpInt)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(false)
			},
			expr: "JSON.stringify(v)",
			want: "null",
		},
		{
			// The spec's nesting case: OpNullable inside OpRepeat inside
			// OpNullable. The inner flag is read once per iteration, so the
			// flags interleave with the loop's own count.
			name: "nullable inside repeat inside nullable",
			build: func(p *prog) {
				p.op(v8.OpNullable, 7)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 3)
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(true)
				p.dataCount(3)
				p.dataFlag(true)
				p.dataStr("a")
				p.dataFlag(false)
				p.dataFlag(true)
				p.dataStr("b")
			},
			expr: "JSON.stringify(v)",
			want: `["a",null,"b"]`,
		},
		{
			// The same program with the outer flag clear: one Counts entry for
			// the whole tree, and neither the loop count nor any inner flag is
			// read.
			name: "nullable inside repeat inside nullable, outer absent",
			build: func(p *prog) {
				p.op(v8.OpNullable, 7)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 3)
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(false)
			},
			expr: "JSON.stringify(v)",
			want: "null",
		},
		{
			// OpOptional, the omitted-key encoding: the same machinery as
			// OpNullable with a different absent value.
			name: "optional primitive present",
			build: func(p *prog) {
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.end()

				p.dataFlag(true)
				p.dataInt(42)
			},
			expr: "typeof v + ':' + v",
			want: "number:42",
		},
		{
			// Undefined, and specifically NOT null: {"note":null} and {} are
			// different values, and telling them apart is what this op is for.
			name: "optional primitive absent",
			build: func(p *prog) {
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.end()

				p.dataFlag(false)
			},
			expr: "typeof v + ':' + (v === null)",
			want: "undefined:false",
		},
		{
			name: "optional string absent",
			build: func(p *prog) {
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataFlag(false)
			},
			expr: "typeof v",
			want: "undefined",
		},
		{
			// Only zero is absent, exactly as for OpNullable.
			name: "optional flag other than 1 is present",
			build: func(p *prog) {
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataCount(-1)
				p.dataStr("present")
			},
			expr: "typeof v + ':' + v",
			want: "string:present",
		},
		{
			name: "optional struct absent",
			build: func(p *prog) {
				s := p.shape("id", "name")
				p.op(v8.OpOptional, 4)
				p.op(v8.OpInt)
				p.op(v8.OpStr)
				p.op(v8.OpObj, s)
				p.end()

				p.dataFlag(false)
			},
			expr: "typeof v",
			want: "undefined",
		},
		{
			// The skipped body holds an OpRepeat, whose bound is not staged
			// either: the absent path may not read Counts past its own flag.
			name: "optional array absent",
			build: func(p *prog) {
				p.op(v8.OpOptional, 5)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 1)
				p.op(v8.OpStr)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(false)
			},
			expr: "typeof v",
			want: "undefined",
		},
		{
			// Two conditional frames ending on the SAME word: the nullable body
			// is the tail of the optional body, so both retire at that pc and
			// the inner one has to close first. One shared frame stack is what
			// makes that ordering expressible.
			name: "nullable body ending where an enclosing optional body ends",
			build: func(p *prog) {
				p.op(v8.OpOptional, 3)
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataFlag(true)  // the optional is present
				p.dataFlag(false) // and its value is a null
			},
			expr: "typeof v + ':' + (v === null)",
			want: "object:true",
		},
		{
			// And the mirror, with the optional inside.
			name: "optional body ending where an enclosing nullable body ends",
			build: func(p *prog) {
				p.op(v8.OpNullable, 3)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.end()

				p.dataFlag(true)
				p.dataFlag(false)
			},
			expr: "typeof v + ':' + (v === null)",
			want: "undefined:false",
		},
		{
			// The spec's nesting case for the new op: OpOptional inside
			// OpRepeat inside OpOptional. The inner flag is read once per
			// iteration, so the flags interleave with the loop's own bound.
			name: "optional inside repeat inside optional",
			build: func(p *prog) {
				p.op(v8.OpOptional, 7)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 3)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(true)
				p.dataCount(3)
				p.dataFlag(true)
				p.dataStr("a")
				p.dataFlag(false)
				p.dataFlag(true)
				p.dataStr("b")
			},
			// An undefined array element stringifies as null, so identity is
			// what distinguishes it from an OpNullable one.
			expr: "v.length + ':' + v.map(x => x === undefined ? 'undef' : x).join(',')",
			want: "3:a,undef,b",
		},
		{
			name: "optional inside repeat inside optional, outer absent",
			build: func(p *prog) {
				p.op(v8.OpOptional, 7)
				p.op(v8.OpMark)
				p.op(v8.OpRepeat, 3)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpArrFromMark)
				p.end()

				p.dataFlag(false)
			},
			expr: "typeof v",
			want: "undefined",
		},
		{
			name:  "objomit with an empty shape",
			build: func(p *prog) { s := p.shape(); p.op(v8.OpObjOmit, s); p.end() },
			expr:  "JSON.stringify(v) + ':' + (Object.getPrototypeOf(v) === Object.prototype)",
			want:  "{}:true",
		},
		{
			name: "objomit with every field present",
			build: func(p *prog) {
				s := p.shape("name", "note", "n")
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataStr("ada")
				p.dataFlag(true)
				p.dataStr("a note")
				p.dataFlag(true)
				p.dataInt(7)
			},
			expr: "Object.keys(v).join(',') + ':' + JSON.stringify(v)",
			want: `name,note,n:{"name":"ada","note":"a note","n":7}`,
		},
		{
			name: "objomit with a mix",
			build: func(p *prog) {
				s := p.shape("name", "note", "n")
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataStr("ada")
				p.dataFlag(false)
				p.dataFlag(true)
				p.dataInt(7)
			},
			// The key is GONE, not present-and-undefined: 'note' in v is the
			// assertion JSON.stringify cannot make, since it drops both.
			expr: "Object.keys(v).join(',') + ':' + ('note' in v) + ':' + JSON.stringify(v)",
			want: `name,n:false:{"name":"ada","n":7}`,
		},
		{
			// Every field omitempty and every field empty. Not an edge case:
			// it is what a zero-valued struct serializes to.
			name: "objomit with every field absent",
			build: func(p *prog) {
				s := p.shape("a", "b", "c")
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpF64)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataFlag(false)
				p.dataFlag(false)
				p.dataFlag(false)
			},
			expr: "JSON.stringify(v) + ':' + Object.keys(v).length + ':' + " +
				"(Object.getPrototypeOf(v) === Object.prototype) + ':' + (v instanceof Object)",
			want: "{}:0:true:true",
		},
		{
			// A partly-omitted object is still an ordinary object: prototype,
			// hasOwnProperty, instanceof. A null-proto one would have none of
			// them and would not match what JSON.parse produces.
			name: "objomit gets Object.prototype",
			build: func(p *prog) {
				s := p.shape("a", "b")
				p.undef()
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataInt(1)
			},
			expr: `JSON.stringify(v) + ':' + (Object.getPrototypeOf(v) === Object.prototype) +
				':' + (v instanceof Object) + ':' + v.hasOwnProperty('b') + ':' + v.hasOwnProperty('a')`,
			want: `{"b":1}:true:true:true:false`,
		},
		{
			// The contrast, and the reason OpObj is untouched: it has no
			// opinion about undefined. The key is there, holding undefined,
			// which Object.keys reports and JSON.stringify drops.
			name: "obj keeps an undefined value as a key",
			build: func(p *prog) {
				s := p.shape("a", "b")
				p.undef()
				p.op(v8.OpInt)
				p.op(v8.OpObj, s)
				p.end()

				p.dataInt(1)
			},
			expr: "Object.keys(v).join(',') + ':' + ('a' in v) + ':' + JSON.stringify(v)",
			want: `a,b:true:{"b":1}`,
		},
		{
			// Holes in the middle: the survivors keep shape order, they do not
			// compact toward the front in some other order.
			name: "objomit keeps shape order with holes",
			build: func(p *prog) {
				s := p.shape("k0", "k1", "k2", "k3", "k4")
				p.op(v8.OpInt)
				p.undef()
				p.op(v8.OpInt)
				p.undef()
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataInt(0)
				p.dataInt(2)
				p.dataInt(4)
			},
			expr: "Object.keys(v).join(',') + ':' + JSON.stringify(v)",
			want: `k0,k2,k4:{"k0":0,"k2":2,"k4":4}`,
		},
		{
			// The distinction the two ops draw, in one object: a nullable field
			// that is null is a key holding null; an optional field that is
			// absent is not a key at all.
			name: "nullable null and optional absent in one objomit",
			build: func(p *prog) {
				s := p.shape("nulled", "omitted", "kept")
				p.op(v8.OpNullable, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataFlag(false)
				p.dataFlag(false)
				p.dataInt(1)
			},
			expr: "Object.keys(v).join(',') + ':' + JSON.stringify(v)",
			want: `nulled,kept:{"nulled":null,"kept":1}`,
		},
		{
			// Both span kinds through an omitting object, since a producer
			// stages small leaves and pins large ones against a threshold.
			name: "objomit over staged and pinned spans",
			build: func(p *prog) {
				s := p.shape("staged", "gone", "pinned")
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpStr)
				p.op(v8.OpObjOmit, s)
				p.end()

				p.dataStr("small")
				p.dataFlag(false)
				p.dataPinStr("a pinned leaf a producer would not copy")
			},
			expr: "Object.keys(v).join(',') + ':' + JSON.stringify(v)",
			want: `staged,pinned:{"staged":"small","pinned":"a pinned leaf a producer would not copy"}`,
		},
		{
			name: "objomit inside a repeat, n == 0",
			build: func(p *prog) {
				s := p.shape("id", "note")
				p.mark()
				p.op(v8.OpRepeat, 6)
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpObjOmit, s)
				p.arr()
				p.end()

				p.dataCount(0)
			},
			expr: "JSON.stringify(v)",
			want: "[]",
		},
		{
			name: "objomit inside a repeat, n == 1",
			build: func(p *prog) {
				s := p.shape("id", "note")
				p.mark()
				p.op(v8.OpRepeat, 6)
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpObjOmit, s)
				p.arr()
				p.end()

				p.dataCount(1)
				p.dataInt(1)
				p.dataFlag(false)
			},
			expr: "JSON.stringify(v)",
			want: `[{"id":1}]`,
		},
		{
			// Many rows, alternating, so the shape's interned keys are reused
			// across objects with different key SETS. Interning is per shape;
			// the surviving subset is per object.
			name: "objomit inside a repeat, many rows",
			build: func(p *prog) {
				s := p.shape("id", "note")
				p.mark()
				p.op(v8.OpRepeat, 6)
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpObjOmit, s)
				p.arr()
				p.end()

				p.dataCount(4)
				for i := 0; i < 4; i++ {
					p.dataInt(int64(i))
					p.dataFlag(i%2 == 1)
					if i%2 == 1 {
						p.dataStr("n" + strconv.Itoa(i))
					}
				}
			},
			expr: "JSON.stringify(v)",
			want: `[{"id":0},{"id":1,"note":"n1"},{"id":2},{"id":3,"note":"n3"}]`,
		},
		{
			// An OP_OBJ_OMIT object as an optional field's own value, so the
			// two ops nest through each other.
			name: "objomit object as an optional field's value",
			build: func(p *prog) {
				row := p.shape("id", "meta")
				meta := p.shape("a", "b")
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 8)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, meta)
				p.op(v8.OpObjOmit, row)
				p.end()

				p.dataInt(1)
				p.dataFlag(true)  // meta is present
				p.dataFlag(false) // meta.a is not
				p.dataFlag(true)  // meta.b is
				p.dataInt(9)
			},
			expr: "JSON.stringify(v)",
			want: `{"id":1,"meta":{"b":9}}`,
		},
		{
			name: "objomit object as an absent optional field",
			build: func(p *prog) {
				row := p.shape("id", "meta")
				meta := p.shape("a", "b")
				p.op(v8.OpInt)
				p.op(v8.OpOptional, 8)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpStr)
				p.op(v8.OpOptional, 1)
				p.op(v8.OpInt)
				p.op(v8.OpObjOmit, meta)
				p.op(v8.OpObjOmit, row)
				p.end()

				p.dataInt(1)
				p.dataFlag(false)
			},
			expr: "Object.keys(v).join(',') + ':' + JSON.stringify(v)",
			want: `id:{"id":1}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &prog{}
			defer p.release()
			tt.build(p)

			val := p.build(t, ctx)
			if got := eval(t, ctx, val, tt.expr); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

// The failure OP_NULLABLE's skip rule exists to prevent, and the one a shallow
// test cannot see. A producer stages NOTHING for a nil value, so if the null
// path consumed an entry from Nums, Floats, Spans or Counts — which is what an
// implementation that runs the body and discards its result does, and what one
// that "advances past the body's data" does — then every leaf AFTER the null
// reads its neighbour's value. The nullable field itself still reads null, and
// a test that asserts only that passes while the rest of the row is garbage.
//
// So the assertion is on the leaves that follow. The nullable body consumes one
// entry from all four arrays, and the six leaves after it read all four again,
// so a leak from any one of them either shifts a value or runs a cursor off the
// end of its array.
func TestBuildNullableCursorInvariance(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// The program, identical for both cases: a row whose second field is a
	// pointer to a struct that itself holds an int, a float and a slice of
	// strings, followed by leaves reading Nums, Floats, Spans and Counts again.
	emit := func(p *prog) {
		row := p.shape("lead", "opt", "num", "flt", "str", "list")
		opt := p.shape("n", "f", "tags")

		p.op(v8.OpInt) // lead: Nums
		p.op(v8.OpNullable, 9)
		p.op(v8.OpInt)  //   opt.n:    Nums
		p.op(v8.OpF64)  //   opt.f:    Floats
		p.op(v8.OpMark) //   opt.tags:
		p.op(v8.OpRepeat, 1)
		p.op(v8.OpStr) //             Spans, and Counts for the bound
		p.op(v8.OpArrFromMark)
		p.op(v8.OpObj, opt)
		p.op(v8.OpInt)  // num: Nums
		p.op(v8.OpF64)  // flt: Floats
		p.op(v8.OpStr)  // str: Spans
		p.op(v8.OpMark) // list:
		p.op(v8.OpRepeat, 1)
		p.op(v8.OpStr) //      Spans, and Counts for the bound
		p.op(v8.OpArrFromMark)
		p.op(v8.OpObj, row)
		p.end()
	}

	// The data for those trailing leaves, the same in both cases. Whether they
	// decode to this is the whole test.
	tail := func(p *prog) {
		p.dataInt(22)
		p.dataF64(0.25)
		p.dataStr("after")
		p.dataCount(2)
		p.dataStr("x")
		p.dataStr("y")
	}
	const tailJSON = `"num":22,"flt":0.25,"str":"after","list":["x","y"]}`

	t.Run("null", func(t *testing.T) {
		p := &prog{}
		defer p.release()
		emit(p)
		p.dataInt(11)
		p.dataFlag(false)
		tail(p)

		// Nums is [11, 22]: reading one for the skipped body would make num
		// undefined, not merely wrong.
		want := `{"lead":11,"opt":null,` + tailJSON
		if got := eval(t, ctx, p.build(t, ctx), "JSON.stringify(v)"); got != want {
			t.Errorf("tree with a null nullable = %s, want %s", got, want)
		}
	})

	t.Run("present", func(t *testing.T) {
		p := &prog{}
		defer p.release()
		emit(p)
		p.dataInt(11)
		p.dataFlag(true)
		p.dataInt(7)   // opt.n
		p.dataF64(1.5) // opt.f
		p.dataCount(2) // opt.tags
		p.dataStr("t1")
		p.dataStr("t2")
		tail(p)

		want := `{"lead":11,"opt":{"n":7,"f":1.5,"tags":["t1","t2"]},` + tailJSON
		if got := eval(t, ctx, p.build(t, ctx), "JSON.stringify(v)"); got != want {
			t.Errorf("tree with the nullable present = %s, want %s", got, want)
		}
	})
}

// The same failure for OP_OPTIONAL, asserted three ways, because the skipped
// branch is the one thing about this op that a shallow test cannot see. The
// optional itself reads undefined whatever the cursors did, so an
// implementation that consumes an entry the producer never staged — which is
// what running the body and discarding its result does, and what "advance past
// the body's data" does — passes every assertion about the optional and
// corrupts every leaf after it.
//
//   - "exact arrays" is the DIRECT one. Nums, Floats, Spans and Counts are each
//     sized to exactly what the leaves after the absent optional consume, so
//     any movement on the skipped branch is not a wrong value, it is a cursor
//     off the end of its array and a hard error. The sizes are asserted, so
//     padding an array later cannot silently disarm it.
//   - "exact arrays, present" is its control: the same program with the flag
//     set consumes exactly the entries the body needs and no more.
//   - "shifted values" is the round-trip one, over rows that alternate, where a
//     leak lands on the NEXT row's leaves instead of off the end — a plausible
//     tree holding the wrong data, which no size check would catch.
func TestBuildOptionalCursorInvariance(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// A row whose first field is an optional struct that would read one entry
	// from each of the four arrays, followed by leaves that read all four
	// again. The row object omits, so the absent optional also drops its key.
	emit := func(p *prog) {
		opt := p.shape("n", "f", "s", "tags")
		row := p.shape("opt", "num", "flt", "str", "list")

		p.op(v8.OpOptional, 10)
		p.op(v8.OpInt)  //   opt.n:    Nums
		p.op(v8.OpF64)  //   opt.f:    Floats
		p.op(v8.OpStr)  //   opt.s:    Spans
		p.op(v8.OpMark) //   opt.tags:
		p.op(v8.OpRepeat, 1)
		p.op(v8.OpStr) //             Spans, and Counts for the bound
		p.op(v8.OpArrFromMark)
		p.op(v8.OpObj, opt)
		p.op(v8.OpInt)  // num:  Nums
		p.op(v8.OpF64)  // flt:  Floats
		p.op(v8.OpStr)  // str:  Spans
		p.op(v8.OpMark) // list:
		p.op(v8.OpRepeat, 1)
		p.op(v8.OpStr) //       Spans, and Counts for the bound
		p.op(v8.OpArrFromMark)
		p.op(v8.OpObjOmit, row)
		p.end()
	}

	// The trailing leaves, identical in both cases. An empty list, so that the
	// last Counts entry is the last one there is.
	tail := func(p *prog) {
		p.dataInt(22)
		p.dataF64(0.25)
		p.dataStr("after")
		p.dataCount(0)
	}
	const tailJSON = `"num":22,"flt":0.25,"str":"after","list":[]}`

	// exact fails the test unless every array holds precisely the given number
	// of entries. Without this the arrays could grow slack and the subtests
	// would still pass while proving nothing.
	exact := func(t *testing.T, p *prog, nums, floats, vspans, counts int) {
		t.Helper()
		pl := p.payload()
		if len(pl.Nums) != nums || len(pl.Floats) != floats ||
			len(pl.Spans)-pl.KeySpans != vspans || len(pl.Counts) != counts {
			t.Fatalf("payload is not exactly sized: nums %d/%d, floats %d/%d, "+
				"value spans %d/%d, counts %d/%d; an oversized array would let a "+
				"leaked cursor read a neighbour instead of failing",
				len(pl.Nums), nums, len(pl.Floats), floats,
				len(pl.Spans)-pl.KeySpans, vspans, len(pl.Counts), counts)
		}
	}

	t.Run("exact arrays", func(t *testing.T) {
		p := &prog{}
		defer p.release()
		emit(p)
		p.dataFlag(false)
		tail(p)

		// One entry in each of Nums, Floats and the value spans, and two in
		// Counts (the flag, then the trailing list's bound). Every one of them
		// belongs to a leaf AFTER the optional, so the skipped branch has
		// nowhere to move a cursor to.
		exact(t, p, 1, 1, 1, 2)

		want := `{` + tailJSON
		if got := eval(t, ctx, p.build(t, ctx), "JSON.stringify(v)"); got != want {
			t.Errorf("tree with an absent optional = %s, want %s", got, want)
		}
	})

	t.Run("exact arrays, present", func(t *testing.T) {
		p := &prog{}
		defer p.release()
		emit(p)
		p.dataFlag(true)
		p.dataInt(7)    // opt.n
		p.dataF64(1.5)  // opt.f
		p.dataStr("in") // opt.s
		p.dataCount(1)  // opt.tags bound
		p.dataStr("t1")
		tail(p)

		exact(t, p, 2, 2, 3, 3)

		want := `{"opt":{"n":7,"f":1.5,"s":"in","tags":["t1"]},` + tailJSON
		if got := eval(t, ctx, p.build(t, ctx), "JSON.stringify(v)"); got != want {
			t.Errorf("tree with the optional present = %s, want %s", got, want)
		}
	})

	t.Run("shifted values", func(t *testing.T) {
		p := &prog{}
		defer p.release()

		s := p.shape("id", "note")
		p.mark()
		p.op(v8.OpRepeat, 6)
		p.op(v8.OpInt)
		p.op(v8.OpOptional, 1)
		p.op(v8.OpStr)
		p.op(v8.OpObjOmit, s)
		p.arr()
		p.end()

		const rows = 6
		p.dataCount(rows)
		for i := 0; i < rows; i++ {
			p.dataInt(int64(i))
			p.dataFlag(i%2 == 1)
			if i%2 == 1 {
				p.dataStr("note-" + strconv.Itoa(i))
			}
		}

		// Every value is distinct and every absent row is followed by a present
		// one, so a cursor leaked on row i shows up as row i+1 wearing row
		// i+2's data rather than as an out-of-range error.
		want := `[{"id":0},{"id":1,"note":"note-1"},{"id":2},` +
			`{"id":3,"note":"note-3"},{"id":4},{"id":5,"note":"note-5"}]`
		if got := eval(t, ctx, p.build(t, ctx), "JSON.stringify(v)"); got != want {
			t.Errorf("rows with alternating optionals = %s, want %s", got, want)
		}
	})
}

// A producer stages small leaves and pins large ones, against a threshold, so
// production payloads carry both kinds. Both must work, mixed, in one payload
// and one shape.
func TestBuildBothSpanKinds(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i)
	}

	p := &prog{}
	defer p.release()

	s := p.shape("staged", "pinned", "stagedBytes", "pinnedBytes", "pinnedEmpty")
	p.str("small")
	p.pinStr("a pinned string that a producer would not bother copying")
	p.bytesv([]byte{1, 2, 3})
	p.pinBytes(big)
	p.pinStr("")
	p.obj(s)
	p.end()

	val := p.build(t, ctx)

	const expr = `[v.staged, v.pinned, Array.from(v.stagedBytes).join('-'),
		v.pinnedBytes.length, v.pinnedBytes[4095], v.pinnedEmpty.length].join('|')`
	want := "small|a pinned string that a producer would not bother copying|1-2-3|4096|255|0"
	if got := eval(t, ctx, val, expr); got != want {
		t.Errorf("mixed span kinds = %q, want %q", got, want)
	}
}

// pooledPtrs is package level on purpose. A local slice is kept on the
// goroutine stack — #cgo noescape says the arguments do not escape — and the
// cgo pointer check only deep-scans HEAP objects, so a stack-allocated Ptrs
// silently skips the check that broke the real caller. A producer's staging
// buffer is long-lived and on the heap; these tests have to be too, or they
// test nothing.
var pooledPtrs []unsafe.Pointer

// pinLeaves stages leaves into the pooled array the way a producer does:
// truncate, pin each, append. It returns the payload arrays for them.
func pinLeaves(pin *runtime.Pinner, leaves ...[]byte) ([]v8.Span, []unsafe.Pointer) {
	pooledPtrs = pooledPtrs[:0]
	spans := make([]v8.Span, len(leaves))
	for i, b := range leaves {
		pin.Pin(&b[0])
		spans[i] = v8.Span{
			Off:  uint32(len(pooledPtrs)),
			Len:  uint32(len(b)),
			Kind: v8.SpanPinned,
		}
		pooledPtrs = append(pooledPtrs, unsafe.Pointer(&b[0]))
	}
	return spans, pooledPtrs
}

// A producer pools its staging buffers: Ptrs is truncated with [:0] and
// refilled per call, and the previous call's pins are dropped when it is done.
// That is ordinary, correct Go, and it used to abort the process.
//
// Passing Ptrs as a Go pointer made the cgo pointer check scan the backing
// ARRAY — the whole allocation, not the first len(Ptrs) entries — and panic on
// any slot holding an unpinned Go pointer. The slots past len hold exactly
// that: pointers from a longer earlier call, unpinned when it finished. So the
// payload that broke was the one after a bigger one, which is why it took
// several calls on one engine to show up and never showed up on a fresh one.
//
// Nothing the caller can do fixes it: the requirement the check imposes is
// that memory the call never reads must also hold only pinned pointers.
func TestBuildPooledPtrsBacking(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// A bigger call first, whose pins are then dropped.
	var first runtime.Pinner
	spans, ptrs := pinLeaves(&first,
		[]byte("first leaf"), []byte("second leaf"),
		[]byte("third leaf"), []byte("fourth leaf"))

	val, err := v8.BuildValue(ctx, &v8.Payload{
		Ops:   []uint32{v8.OpMark, v8.OpStr, v8.OpStr, v8.OpStr, v8.OpStr, v8.OpArrFromMark, v8.OpEnd},
		Spans: spans,
		Ptrs:  ptrs,
	})
	fatalIf(t, err)
	if got := eval(t, ctx, val, "v.join('|')"); got != "first leaf|second leaf|third leaf|fourth leaf" {
		t.Fatalf("first build = %q", got)
	}
	val.Release()
	first.Unpin()

	// The pooled second call: same backing array, shorter, with stale and
	// now-unpinned pointers in the slots past len.
	var second runtime.Pinner
	defer second.Unpin()
	spans, ptrs = pinLeaves(&second, []byte("the only leaf this time"))

	val, err = v8.BuildValue(ctx, &v8.Payload{
		Ops:   []uint32{v8.OpStr, v8.OpEnd},
		Spans: spans,
		Ptrs:  ptrs,
	})
	fatalIf(t, err)
	defer val.Release()
	if got := eval(t, ctx, val, "v"); got != "the only leaf this time" {
		t.Errorf("pooled build = %q, want %q", got, "the only leaf this time")
	}
}

// A megabyte through a pinned span: the path that replaces base64, at a size
// where a producer would certainly not stage it.
func TestBuildBytesOneMiB(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const size = 1 << 20
	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i * 31)
	}

	p := &prog{}
	defer p.release()
	p.pinBytes(want)
	p.end()

	val := p.build(t, ctx)
	if got := eval(t, ctx, val, "(v instanceof Uint8Array) + ':' + v.length"); got != "true:1048576" {
		t.Fatalf("1 MiB payload arrived as %q", got)
	}
	if got := val.ArrayBufferViewBytes(); !bytes.Equal(got, want) {
		t.Errorf("1 MiB payload did not round-trip byte for byte")
	}
}

/********** the opcode numbers themselves **********/

// The opcode values are an ABI, and the only kind of breakage they have is
// silent: a producer that emits 15 for one op against a builder that reads 15
// as another executes a different program, with no error anywhere and a
// plausible tree at the end. Adding an op in the middle of the enum, or
// reordering it for tidiness, is a one-character change with that consequence,
// so the numbers are written out here as literals rather than derived from the
// constants they are checking.
//
// The rule this pins: append only. A new op takes the next free number.
func TestOpcodeABIValues(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"OpEnd", v8.OpEnd, 0},
		{"OpNull", v8.OpNull, 1},
		{"OpUndef", v8.OpUndef, 2},
		{"OpTrue", v8.OpTrue, 3},
		{"OpFalse", v8.OpFalse, 4},
		{"OpBool", v8.OpBool, 5},
		{"OpInt", v8.OpInt, 6},
		{"OpF64", v8.OpF64, 7},
		{"OpStr", v8.OpStr, 8},
		{"OpBytes", v8.OpBytes, 9},
		{"OpObj", v8.OpObj, 10},
		{"OpMark", v8.OpMark, 11},
		{"OpArrFromMark", v8.OpArrFromMark, 12},
		{"OpRepeat", v8.OpRepeat, 13},
		{"OpNullable", v8.OpNullable, 14},
		{"OpOptional", v8.OpOptional, 15},
		{"OpObjOmit", v8.OpObjOmit, 16},
		{"SpanStaged", v8.SpanStaged, 0},
		{"SpanPinned", v8.SpanPinned, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d; these values are an ABI and may be "+
				"added to but never renumbered", c.name, c.got, c.want)
		}
	}
}

/********** the two load-bearing properties **********/

// One crossing per tree. This is the defect v2 exists to fix — the previous
// ABI said "one crossing" in its rationale and then made every leaf its own
// cgo call — so it is measured, not asserted.
//
// Deliberately not parallel: the counter is process-wide, and Go runs all
// sequential tests to completion before releasing any parallel ones, so a
// sequential test has the counter to itself.
func TestBuildOneCrossing(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const cols, rows = 20, 1000
	keys, data := benchRows(cols, rows)
	p := buildProgram(keys, data)
	defer p.release()

	v8.ResetBuildCallCount()
	val, err := v8.BuildValue(ctx, p.payload())
	fatalIf(t, err)
	defer val.Release()

	if got := v8.BuildCallCount(); got != 1 {
		t.Fatalf("building a %dx%d tree took %d crossings, want exactly 1", cols, rows, got)
	}

	// And the one crossing really did build the whole thing.
	const expr = `v.length + ':' + v[999].col_0 + ':' + v[999].col_2 + ':' +
		Object.keys(v[0]).length`
	want := "1000:" + strconv.Itoa(999*cols) + ":value-999-2:20"
	if got := eval(t, ctx, val, expr); got != want {
		t.Errorf("tree = %q, want %q", got, want)
	}
}

// Nothing per node may become a tracked value: that was true of the scope API
// and has to stay true here. The only visible symptom is this counter.
func TestBuildNoPerNodeTracking(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 1000
	before := ctx.RetainedValueCount()

	p := &prog{}
	defer p.release()
	s := p.shape("id", "name", "score")
	p.mark()
	p.op(v8.OpRepeat, 5)
	p.op(v8.OpInt)
	p.op(v8.OpStr)
	p.op(v8.OpF64)
	p.op(v8.OpObj, s)
	p.arr()
	p.end()

	p.dataCount(rows)
	for i := 0; i < rows; i++ {
		p.dataInt(int64(i))
		p.dataStr("name-" + strconv.Itoa(i))
		p.dataF64(float64(i) / 3)
	}

	val := p.build(t, ctx)

	after := ctx.RetainedValueCount()
	if delta := after - before; delta != 1 {
		t.Fatalf("RetainedValueCount grew by %d building a %d-object tree, want exactly 1 (the root); "+
			"anything more means an m_value leaked per node", delta, rows)
	}

	if got := eval(t, ctx, val, "v.length + ':' + v[999].name"); got != "1000:name-999" {
		t.Errorf("root value = %q, want %q", got, "1000:name-999")
	}

	// Releasing the root frees it and nothing else — the tree was never in the
	// context's table to begin with. Measured against the count after the
	// assertion above, which retains a global and a script result of its own.
	afterEval := ctx.RetainedValueCount()
	val.Release()
	if got := ctx.RetainedValueCount(); got != afterEval-1 {
		t.Errorf("RetainedValueCount after releasing the root = %d, want %d", got, afterEval-1)
	}
}

/********** the scope API as an oracle **********/

// The scope API (gav8_values.go) is already tested against hand-written
// expectations, so building the same tree both ways and comparing transitively
// validates this one without restating any of them. Key order is compared as
// well as values: two objects can hold the same properties and still not be
// the same object to a consumer that iterates them.
//
// One row field is nullable and null on a third of the rows, which makes this
// the widest check of the OpNullable skip there is: 200 iterations of a body
// whose staged payload shifts under every later leaf whenever a flag is clear,
// compared against an implementation that has no flags at all.
func TestBuildMatchesBatchScope(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 200

	// --- the same tree through the op-buffer builder ---
	p := &prog{}
	defer p.release()
	rowShape := p.shape("id", "name", "score", "nick", "tags", "meta")
	metaShape := p.shape("live", "note")

	p.mark()
	p.op(v8.OpRepeat, 17)
	p.op(v8.OpInt) // id
	p.op(v8.OpStr) // name
	p.op(v8.OpF64) // score
	// nick is a *string, present on two rows in three. Its flag is read once
	// per iteration, so the flags interleave with the loop's own count and the
	// staged strings shift under every leaf after them on any row that is null.
	p.op(v8.OpNullable, 1)
	p.op(v8.OpStr)
	p.op(v8.OpMark) // tags
	p.op(v8.OpStr)
	p.op(v8.OpStr)
	p.op(v8.OpArrFromMark)
	p.op(v8.OpBool) // meta.live
	p.op(v8.OpStr)  // meta.note
	p.op(v8.OpObj, metaShape)
	p.op(v8.OpObj, rowShape)
	p.op(v8.OpNull) // a trailing null, dropped by nothing: it joins the array
	p.arr()
	p.end()

	hasNick := func(i int) bool { return i%3 != 0 }

	p.dataCount(rows)
	for i := 0; i < rows; i++ {
		p.dataInt(int64(i))
		p.dataStr("name-" + strconv.Itoa(i))
		p.dataF64(float64(i) / 7)
		p.dataFlag(hasNick(i))
		if hasNick(i) {
			p.dataStr("nick-" + strconv.Itoa(i))
		}
		p.dataStr("tag-a-" + strconv.Itoa(i))
		p.dataPinStr("tag-b-" + strconv.Itoa(i))
		p.dataInt(int64(i % 2))
		p.dataStr("note " + strconv.Itoa(i))
	}

	built := p.build(t, ctx)

	// --- and through the per-node scope API ---
	s := v8.NewBatchScope(ctx)
	rowShapeV1 := s.Shape([]string{"id", "name", "score", "nick", "tags", "meta"})
	metaShapeV1 := s.Shape([]string{"live", "note"})
	elems := make([]v8.LocalRef, 0, rows*2)
	for i := 0; i < rows; i++ {
		tags := s.Array([]v8.LocalRef{
			s.String("tag-a-" + strconv.Itoa(i)),
			s.String("tag-b-" + strconv.Itoa(i)),
		})
		meta := s.Object(metaShapeV1, []v8.LocalRef{
			s.Bool(i%2 == 1),
			s.String("note " + strconv.Itoa(i)),
		})
		nick := s.Null()
		if hasNick(i) {
			nick = s.String("nick-" + strconv.Itoa(i))
		}
		elems = append(elems, s.Object(rowShapeV1, []v8.LocalRef{
			s.Int32(int32(i)),
			s.String("name-" + strconv.Itoa(i)),
			s.Float64(float64(i) / 7),
			nick,
			tags,
			meta,
		}), s.Null())
	}
	oracle, err := s.Result(s.Array(elems))
	fatalIf(t, err)
	s.Close()

	fatalIf(t, ctx.Global().Set("built", built))
	fatalIf(t, ctx.Global().Set("oracle", oracle))

	if got := diffJS(t, ctx, "built", "oracle"); got != "equal" {
		t.Errorf("op-buffer tree differs from the scope-built tree: %s", got)
	}
}

// diffJS compares two values already installed on the global by name and
// returns "equal" or the first difference. Key order and prototype are compared
// at every object as well as the values: two objects can hold the same
// properties and still not be the same object to a consumer that iterates them,
// and a null-prototype one is not the same object to anything.
func diffJS(t *testing.T, ctx *v8.Context, a, b string) string {
	t.Helper()
	src := `(() => {
		const diff = (a, b, path) => {
			if (Array.isArray(a) || Array.isArray(b)) {
				if (!Array.isArray(a) || !Array.isArray(b)) return 'arrayness at ' + path;
				if (a.length !== b.length) return 'length at ' + path;
				for (let i = 0; i < a.length; i++) {
					const d = diff(a[i], b[i], path + '[' + i + ']');
					if (d) return d;
				}
				return '';
			}
			if (a !== null && typeof a === 'object') {
				if (b === null || typeof b !== 'object') return 'type at ' + path;
				if (Object.getPrototypeOf(a) !== Object.getPrototypeOf(b)) return 'prototype at ' + path;
				const ka = Object.keys(a), kb = Object.keys(b);
				if (ka.join(',') !== kb.join(',')) {
					return 'key order at ' + path + ': [' + ka + '] vs [' + kb + ']';
				}
				for (const k of ka) {
					const d = diff(a[k], b[k], path + '.' + k);
					if (d) return d;
				}
				return '';
			}
			if (a !== b) return 'value at ' + path + ': ' + String(a) + ' vs ' + String(b);
			return '';
		};
		return diff(` + a + `, ` + b + `, '$') || 'equal';
	})()`

	out, err := ctx.RunScript(src, "gav8_build_diff.js")
	fatalIf(t, err)
	return out.String()
}

// OP_OBJ_OMIT with nothing omitted must BE OP_OBJ. Otherwise a producer that
// switches ops the moment a struct gains one omitempty field would quietly
// change the shape of every object it emits — key order, prototype, or the
// values paired with the keys.
//
// The parsed arm is the contract that actually matters, and it is why the
// omitted keys are written out of the JSON rather than set to null: a consumer
// must not be able to tell a built object from the JSON.parse of the same data.
func TestBuildObjOmitMatchesObj(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 200
	// Row i keeps note when i%3 != 0 and score when i%4 != 0, so every
	// combination of present and absent occurs, including neither and both.
	hasNote := func(i int) bool { return i%3 != 0 }
	hasScore := func(i int) bool { return i%4 != 0 }

	// --- OP_OBJ_OMIT, with two of the four fields optional ---
	omitted := &prog{}
	defer omitted.release()
	s := omitted.shape("id", "note", "score", "tag")
	omitted.mark()
	omitted.op(v8.OpRepeat, 10)
	omitted.op(v8.OpInt) // id
	omitted.op(v8.OpOptional, 1)
	omitted.op(v8.OpStr) // note
	omitted.op(v8.OpOptional, 1)
	omitted.op(v8.OpF64) // score
	omitted.op(v8.OpStr) // tag, never optional
	omitted.op(v8.OpObjOmit, s)
	omitted.arr()
	omitted.end()

	omitted.dataCount(rows)
	for i := 0; i < rows; i++ {
		omitted.dataInt(int64(i))
		omitted.dataFlag(hasNote(i))
		if hasNote(i) {
			omitted.dataStr("note-" + strconv.Itoa(i))
		}
		omitted.dataFlag(hasScore(i))
		if hasScore(i) {
			omitted.dataF64(float64(i) / 7)
		}
		// Pinned on half the rows: a producer picks per value.
		if i%2 == 0 {
			omitted.dataStr("tag-" + strconv.Itoa(i))
		} else {
			omitted.dataPinStr("tag-" + strconv.Itoa(i))
		}
	}
	built := omitted.build(t, ctx)

	// --- the same rows through JSON.parse, keys genuinely absent ---
	var js []byte
	js = append(js, '[')
	for i := 0; i < rows; i++ {
		if i > 0 {
			js = append(js, ',')
		}
		js = append(js, `{"id":`...)
		js = strconv.AppendInt(js, int64(i), 10)
		if hasNote(i) {
			js = append(js, `,"note":"note-`...)
			js = strconv.AppendInt(js, int64(i), 10)
			js = append(js, '"')
		}
		if hasScore(i) {
			js = append(js, `,"score":`...)
			js = strconv.AppendFloat(js, float64(i)/7, 'g', -1, 64)
		}
		js = append(js, `,"tag":"tag-`...)
		js = strconv.AppendInt(js, int64(i), 10)
		js = append(js, '"', '}')
	}
	js = append(js, ']')

	parsed, err := v8.JSONParse(ctx, string(js))
	fatalIf(t, err)
	defer parsed.Release()

	fatalIf(t, ctx.Global().Set("built", built))
	fatalIf(t, ctx.Global().Set("parsed", parsed))
	if got := diffJS(t, ctx, "built", "parsed"); got != "equal" {
		t.Errorf("OP_OBJ_OMIT tree differs from the parsed one: %s", got)
	}

	// The all-present half of the requirement: OP_OBJ and OP_OBJ_OMIT over the
	// same shape and the same values, built side by side.
	keys, data := benchRows(6, 50)
	viaObj := buildProgramObjOp(keys, data, v8.OpObj)
	defer viaObj.release()
	viaOmit := buildProgramObjOp(keys, data, v8.OpObjOmit)
	defer viaOmit.release()

	a := viaObj.build(t, ctx)
	b := viaOmit.build(t, ctx)
	fatalIf(t, ctx.Global().Set("viaObj", a))
	fatalIf(t, ctx.Global().Set("viaOmit", b))
	if got := diffJS(t, ctx, "viaObj", "viaOmit"); got != "equal" {
		t.Errorf("OP_OBJ_OMIT with every key present differs from OP_OBJ: %s", got)
	}
	if got := eval(t, ctx, b, "JSON.stringify(v) === JSON.stringify(viaObj)"); got != "true" {
		t.Errorf("OP_OBJ_OMIT and OP_OBJ stringify differently with every key present")
	}
}

// Requirement 7, restated for the new ops: a tree that uses them is still one
// crossing and still exactly one tracked value. Neither op allocates anything
// the builder has to hand back, and the omit scratch is reused across the whole
// program rather than per object.
//
// Not parallel: BuildCallCount is process-wide.
func TestBuildOptionalOneCrossingOneValue(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 1000
	p := &prog{}
	defer p.release()
	s := p.shape("id", "note", "score")
	p.mark()
	p.op(v8.OpRepeat, 9)
	p.op(v8.OpInt)
	p.op(v8.OpOptional, 1)
	p.op(v8.OpStr)
	p.op(v8.OpOptional, 1)
	p.op(v8.OpF64)
	p.op(v8.OpObjOmit, s)
	p.arr()
	p.end()

	p.dataCount(rows)
	for i := 0; i < rows; i++ {
		p.dataInt(int64(i))
		p.dataFlag(i%2 == 0)
		if i%2 == 0 {
			p.dataStr("note-" + strconv.Itoa(i))
		}
		p.dataFlag(i%3 == 0)
		if i%3 == 0 {
			p.dataF64(float64(i) / 3)
		}
	}

	before := ctx.RetainedValueCount()
	v8.ResetBuildCallCount()
	val, err := v8.BuildValue(ctx, p.payload())
	fatalIf(t, err)

	if got := v8.BuildCallCount(); got != 1 {
		t.Fatalf("building a %d-row optional tree took %d crossings, want exactly 1", rows, got)
	}
	if delta := ctx.RetainedValueCount() - before; delta != 1 {
		t.Fatalf("RetainedValueCount grew by %d, want exactly 1 (the root)", delta)
	}

	const expr = `v.length + ':' + Object.keys(v[0]).join(',') + ':' +
		Object.keys(v[1]).join(',') + ':' + Object.keys(v[2]).join(',') + ':' +
		Object.keys(v[999]).join(',') + ':' + v[999].id`
	// Row 0 keeps both, row 1 keeps neither, row 2 keeps note only, and row 999
	// (odd, divisible by 3) keeps score only.
	want := "1000:id,note,score:id:id,note:id,score:999"
	if got := eval(t, ctx, val, expr); got != want {
		t.Errorf("tree = %q, want %q", got, want)
	}
	val.Release()
}

/********** failing closed **********/

// The op buffer is generated, but it is the only thing between a producer bug
// and an out-of-bounds read of process memory. Every one of these must come
// back as an error, with no value and nothing retained.
func TestBuildMalformed(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	oneKey := []v8.ShapeDef{{First: 0, N: 1}}
	twoKeys := []v8.ShapeDef{{First: 0, N: 2}}
	keySpans := []v8.Span{
		{Off: 0, Len: 1, Kind: v8.SpanStaged},
		{Off: 1, Len: 1, Kind: v8.SpanStaged},
	}

	tests := []struct {
		name string
		p    v8.Payload
	}{
		{
			name: "empty op buffer",
			p:    v8.Payload{},
		},
		{
			name: "no OP_END",
			p:    v8.Payload{Ops: []uint32{v8.OpNull}},
		},
		{
			name: "OP_END with an empty stack",
			p:    v8.Payload{Ops: []uint32{v8.OpEnd}},
		},
		{
			name: "OP_END with two values left",
			p:    v8.Payload{Ops: []uint32{v8.OpNull, v8.OpNull, v8.OpEnd}},
		},
		{
			name: "unknown opcode",
			p:    v8.Payload{Ops: []uint32{9999, v8.OpEnd}},
		},
		{
			name: "OP_OBJ truncated before its shape id",
			p:    v8.Payload{Ops: []uint32{v8.OpObj}, Shapes: oneKey, Spans: keySpans[:1], KeySpans: 1, Buf: []byte("ab")},
		},
		{
			name: "OP_REPEAT truncated before its body length",
			p:    v8.Payload{Ops: []uint32{v8.OpRepeat}, Counts: []int32{1}},
		},
		{
			name: "shape id out of range",
			p:    v8.Payload{Ops: []uint32{v8.OpObj, 7, v8.OpEnd}},
		},
		{
			name: "shape keys outside the key region",
			p: v8.Payload{
				Ops: []uint32{v8.OpNull, v8.OpNull, v8.OpObj, 0, v8.OpEnd},
				// Two keys claimed, one key span declared.
				Shapes: twoKeys, Spans: keySpans, KeySpans: 1, Buf: []byte("ab"),
			},
		},
		{
			name: "duplicate keys in a shape",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpNull, v8.OpObj, 0, v8.OpEnd},
				Shapes: twoKeys,
				Spans: []v8.Span{
					{Off: 0, Len: 1, Kind: v8.SpanStaged},
					{Off: 0, Len: 1, Kind: v8.SpanStaged},
				},
				KeySpans: 2, Buf: []byte("a"),
			},
		},
		{
			name: "stack underflow at OP_OBJ",
			p: v8.Payload{
				Ops: []uint32{v8.OpNull, v8.OpObj, 0, v8.OpEnd},
				// Shape wants two values, one was pushed.
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
			},
		},
		{
			name: "OP_OBJ pops past an OP_MARK",
			p: v8.Payload{
				Ops: []uint32{v8.OpNull, v8.OpNull, v8.OpMark, v8.OpNull,
					v8.OpObj, 0, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
			},
		},
		{
			name: "OP_ARR_FROM_MARK without a mark",
			p:    v8.Payload{Ops: []uint32{v8.OpNull, v8.OpArrFromMark, v8.OpEnd}},
		},
		{
			name: "OP_END with an unclosed mark",
			p:    v8.Payload{Ops: []uint32{v8.OpMark, v8.OpNull, v8.OpEnd}},
		},
		{
			name: "OP_END inside a repeat body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpRepeat, 2, v8.OpEnd, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "repeat body runs past the op buffer",
			p: v8.Payload{
				Ops:    []uint32{v8.OpRepeat, 99, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "repeat with an empty body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpRepeat, 0, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "negative repeat count",
			p: v8.Payload{
				Ops:    []uint32{v8.OpRepeat, 1, v8.OpNull, v8.OpEnd},
				Counts: []int32{-1},
			},
		},
		{
			name: "counts cursor exhausted",
			p:    v8.Payload{Ops: []uint32{v8.OpRepeat, 1, v8.OpNull, v8.OpEnd}},
		},
		{
			name: "OP_NULLABLE truncated before its body length",
			p:    v8.Payload{Ops: []uint32{v8.OpNullable}, Counts: []int32{1}},
		},
		{
			// A body that pushes nothing cannot satisfy exactly-one, so an
			// empty one is a producer bug however the flag reads.
			name: "nullable with an empty body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 0, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "nullable with an empty body and a clear flag",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 0, v8.OpNull, v8.OpEnd},
				Counts: []int32{0},
			},
		},
		{
			name: "nullable body runs past the op buffer",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 99, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			// Rejected before the flag is read, so the body length is checked
			// even for a null — a producer bug is a producer bug on both paths.
			name: "nullable body runs past the op buffer with a clear flag",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 99, v8.OpNull, v8.OpEnd},
				Counts: []int32{0},
			},
		},
		{
			name: "counts cursor exhausted at OP_NULLABLE",
			p:    v8.Payload{Ops: []uint32{v8.OpNullable, 1, v8.OpNull, v8.OpEnd}},
		},
		{
			// The body is a repeat that runs zero times, so it pushes nothing.
			// Left unchecked the enclosing object would take the field before
			// it twice and shift everything after — a plausible, wrong tree.
			name: "nullable body pushes nothing",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 3, v8.OpRepeat, 1, v8.OpInt, v8.OpEnd},
				Counts: []int32{1, 0},
				Nums:   []int64{5},
			},
		},
		{
			name: "nullable body pushes two values",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 2, v8.OpNull, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "OP_END inside a nullable body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 2, v8.OpEnd, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			// An OpMark opened inside the body and left open. It would be an
			// array boundary on the value path and no boundary at all on the
			// null path, so the tree would depend on the flag.
			name: "nullable body leaves an OP_MARK open",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNullable, 2, v8.OpMark, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			// And the mirror: an OpMark from outside, closed inside. On the
			// null path the array would still be open at OP_END.
			name: "nullable body closes an outer OP_MARK",
			p: v8.Payload{
				Ops:    []uint32{v8.OpMark, v8.OpNull, v8.OpNullable, 1, v8.OpArrFromMark, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			// Net +1 on the stack, so the exit depth check passes — and the
			// object still took a value pushed before the flag was read. Only
			// the floor sees it.
			name: "nullable body pops a value pushed outside it",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpNullable, 4, v8.OpNull, v8.OpObj, 0, v8.OpNull, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
				Counts: []int32{1},
			},
		},
		{
			// A nullable body whose end lies outside the repeat body it was
			// opened in. Nothing reads out of bounds — the repeat frame simply
			// never retires, and OP_END refuses to run with a frame open.
			name: "nullable body running out of an enclosing repeat body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpMark, v8.OpRepeat, 2, v8.OpNullable, 3, v8.OpNull, v8.OpArrFromMark, v8.OpEnd},
				Counts: []int32{1, 1},
			},
		},
		// OP_OPTIONAL shares OP_NULLABLE's frame, so it inherits every check
		// above — which is exactly why each one is restated here rather than
		// assumed: sharing is a property of the current implementation, and
		// these are properties of the ABI.
		{
			name: "OP_OPTIONAL truncated before its body length",
			p:    v8.Payload{Ops: []uint32{v8.OpOptional}, Counts: []int32{1}},
		},
		{
			name: "optional with an empty body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 0, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "optional with an empty body and a clear flag",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 0, v8.OpNull, v8.OpEnd},
				Counts: []int32{0},
			},
		},
		{
			name: "optional body runs past the op buffer",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 99, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "optional body runs past the op buffer with a clear flag",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 99, v8.OpNull, v8.OpEnd},
				Counts: []int32{0},
			},
		},
		{
			name: "counts cursor exhausted at OP_OPTIONAL",
			p:    v8.Payload{Ops: []uint32{v8.OpOptional, 1, v8.OpNull, v8.OpEnd}},
		},
		{
			name: "optional body pushes nothing",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 3, v8.OpRepeat, 1, v8.OpInt, v8.OpEnd},
				Counts: []int32{1, 0},
				Nums:   []int64{5},
			},
		},
		{
			name: "optional body pushes two values",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 2, v8.OpNull, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "OP_END inside an optional body",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 2, v8.OpEnd, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "optional body leaves an OP_MARK open",
			p: v8.Payload{
				Ops:    []uint32{v8.OpOptional, 2, v8.OpMark, v8.OpNull, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			name: "optional body closes an outer OP_MARK",
			p: v8.Payload{
				Ops:    []uint32{v8.OpMark, v8.OpNull, v8.OpOptional, 1, v8.OpArrFromMark, v8.OpEnd},
				Counts: []int32{1},
			},
		},
		{
			// Net +1 on the stack, so the exit depth check passes — and the
			// object still took a value pushed before the flag was read.
			name: "optional body pops a value pushed outside it",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpOptional, 4, v8.OpNull, v8.OpObj, 0, v8.OpNull, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
				Counts: []int32{1},
			},
		},
		{
			// The same theft out through OP_OBJ_OMIT, which needs its own floor
			// check: it is a second op that pops an arity.
			name: "optional body pops a value pushed outside it, via OP_OBJ_OMIT",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpOptional, 4, v8.OpNull, v8.OpObjOmit, 0, v8.OpNull, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
				Counts: []int32{1},
			},
		},
		{
			name: "OP_OBJ_OMIT truncated before its shape id",
			p: v8.Payload{
				Ops: []uint32{v8.OpObjOmit}, Shapes: oneKey,
				Spans: keySpans[:1], KeySpans: 1, Buf: []byte("ab"),
			},
		},
		{
			name: "OP_OBJ_OMIT shape id out of range",
			p:    v8.Payload{Ops: []uint32{v8.OpObjOmit, 7, v8.OpEnd}},
		},
		{
			name: "stack underflow at OP_OBJ_OMIT",
			p: v8.Payload{
				// Shape wants two values, one was pushed. The arity popped is
				// the shape's, not the number of non-undefined values, so an
				// all-undefined object is still an underflow when it is short.
				Ops:    []uint32{v8.OpNull, v8.OpObjOmit, 0, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
			},
		},
		{
			name: "stack underflow at OP_OBJ_OMIT with undefined values",
			p: v8.Payload{
				Ops:    []uint32{v8.OpUndef, v8.OpObjOmit, 0, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
			},
		},
		{
			name: "OP_OBJ_OMIT pops past an OP_MARK",
			p: v8.Payload{
				Ops: []uint32{v8.OpNull, v8.OpNull, v8.OpMark, v8.OpNull,
					v8.OpObjOmit, 0, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 2, Buf: []byte("ab"),
			},
		},
		{
			name: "OP_OBJ_OMIT shape keys outside the key region",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpNull, v8.OpObjOmit, 0, v8.OpEnd},
				Shapes: twoKeys, Spans: keySpans, KeySpans: 1, Buf: []byte("ab"),
			},
		},
		{
			name: "duplicate keys in an OP_OBJ_OMIT shape",
			p: v8.Payload{
				Ops:    []uint32{v8.OpNull, v8.OpNull, v8.OpObjOmit, 0, v8.OpEnd},
				Shapes: twoKeys,
				Spans: []v8.Span{
					{Off: 0, Len: 1, Kind: v8.SpanStaged},
					{Off: 0, Len: 1, Kind: v8.SpanStaged},
				},
				KeySpans: 2, Buf: []byte("a"),
			},
		},
		{
			name: "nums cursor exhausted",
			p:    v8.Payload{Ops: []uint32{v8.OpInt, v8.OpEnd}},
		},
		{
			name: "nums cursor exhausted at OP_BOOL",
			p:    v8.Payload{Ops: []uint32{v8.OpBool, v8.OpEnd}},
		},
		{
			name: "floats cursor exhausted",
			p:    v8.Payload{Ops: []uint32{v8.OpF64, v8.OpEnd}},
		},
		{
			name: "span cursor exhausted",
			p:    v8.Payload{Ops: []uint32{v8.OpStr, v8.OpEnd}},
		},
		{
			name: "value cursor may not reach into the key region",
			p: v8.Payload{
				Ops: []uint32{v8.OpStr, v8.OpEnd},
				// One span, and it is a key, so there is no value span at all.
				Spans: keySpans[:1], KeySpans: 1, Buf: []byte("a"),
			},
		},
		{
			name: "staged span runs past the end of buf",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 0, Len: 64, Kind: v8.SpanStaged}},
				Buf:   []byte("short"),
			},
		},
		{
			name: "staged span offset past the end of buf",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 4096, Len: 1, Kind: v8.SpanStaged}},
				Buf:   []byte("short"),
			},
		},
		{
			name: "staged span length wraps",
			p: v8.Payload{
				Ops:   []uint32{v8.OpBytes, v8.OpEnd},
				Spans: []v8.Span{{Off: 4, Len: 0xFFFFFFFF, Kind: v8.SpanStaged}},
				Buf:   []byte("short"),
			},
		},
		{
			name: "pinned span index out of range",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 3, Len: 1, Kind: v8.SpanPinned}},
				Ptrs:  []unsafe.Pointer{nil},
			},
		},
		{
			name: "pinned span with no ptrs at all",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 0, Len: 1, Kind: v8.SpanPinned}},
			},
		},
		{
			name: "null pinned pointer with a non-zero length",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 0, Len: 8, Kind: v8.SpanPinned}},
				Ptrs:  []unsafe.Pointer{nil},
			},
		},
		{
			name: "unknown span kind",
			p: v8.Payload{
				Ops:   []uint32{v8.OpStr, v8.OpEnd},
				Spans: []v8.Span{{Off: 0, Len: 1, Kind: 7}},
				Buf:   []byte("a"),
			},
		},
		{
			name: "KeySpans past the end of Spans",
			p: v8.Payload{
				Ops:      []uint32{v8.OpNull, v8.OpEnd},
				Spans:    keySpans,
				KeySpans: 9,
				Buf:      []byte("ab"),
			},
		},
		{
			name: "negative KeySpans",
			p: v8.Payload{
				Ops:      []uint32{v8.OpNull, v8.OpEnd},
				KeySpans: -1,
			},
		},
	}

	before := ctx.RetainedValueCount()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.p
			val, err := v8.BuildValue(ctx, &p)
			if err == nil {
				val.Release()
				t.Fatalf("malformed payload built a value instead of returning an error")
			}
			if val != nil {
				t.Errorf("error return also produced a value")
			}
		})
	}
	if got := ctx.RetainedValueCount(); got != before {
		t.Errorf("RetainedValueCount grew by %d over %d rejected payloads; a failure must build nothing",
			got-before, len(tests))
	}
}

func TestBuildNilArguments(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	if _, err := v8.BuildValue(nil, &v8.Payload{Ops: []uint32{v8.OpNull, v8.OpEnd}}); err == nil {
		t.Error("BuildValue(nil context) returned no error")
	}
	if _, err := v8.BuildValue(ctx, nil); err == nil {
		t.Error("BuildValue(nil payload) returned no error")
	}
}

// Random op buffers must not crash the process, whatever else they do. Only
// the seed corpus runs under `go test`; the caps keep a fuzzed count from
// turning a memory-safety test into an out-of-memory one.
func FuzzBuildValue(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{1, 0})
	f.Add([]byte{11, 8, 12, 0})
	f.Add([]byte{13, 4, 6, 8, 10, 0, 12, 0})
	f.Add([]byte{10, 0, 0, 255, 255, 255, 255})
	f.Add([]byte{14, 1, 8, 0})
	f.Add([]byte{14, 5, 11, 13, 1, 6, 12, 0})
	f.Add([]byte{15, 1, 8, 0})
	f.Add([]byte{15, 1, 2, 16, 1, 0})
	f.Add([]byte{2, 6, 16, 1, 0})
	f.Add([]byte{15, 5, 11, 13, 1, 6, 12, 0})

	iso := v8.NewIsolate()
	f.Cleanup(iso.Dispose)
	ctx := v8.NewContext(iso)
	f.Cleanup(ctx.Close)

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 512 {
			data = data[:512]
		}
		ops := make([]uint32, len(data))
		for i, b := range data {
			ops[i] = uint32(b)
		}
		p := &v8.Payload{
			Ops: ops,
			Shapes: []v8.ShapeDef{
				{First: 0, N: 0},
				{First: 0, N: 2},
				{First: 1, N: 9},
			},
			Buf:      []byte("abcdefgh"),
			Spans:    []v8.Span{{Off: 0, Len: 1}, {Off: 1, Len: 2}, {Off: 3, Len: 5}},
			KeySpans: 2,
			Nums:     []int64{0, 1, -1},
			Floats:   []float64{0.5, 2},
			Counts:   []int32{0, 1, 3},
		}
		if val, err := v8.BuildValue(ctx, p); err == nil {
			val.Release()
		}
	})
}

// The shape the only real consumer uses: the builder is called from inside a
// FunctionTemplate callback, so the thread is already inside the isolate, a
// HandleScope and a Context::Scope, with a JS call in flight. Then the isolate
// is disposed.
//
// This is not a variation on the tests above, it is a different execution
// context, and getting it wrong does not fail a build or an assertion — it
// aborts the process at Isolate::Dispose with "Disposing the isolate that is
// entered by a thread", arbitrarily far from the call that caused it.
func TestBuildValueInsideCallback(t *testing.T) {
	// Sequential: shares the pooled Ptrs array with TestBuildPooledPtrsBacking,
	// and that array's stale slots are the point of both.
	iso := v8.NewIsolate()
	global := v8.NewObjectTemplate(iso)

	var buildErr error
	var pin runtime.Pinner
	defer pin.Unpin()

	fn := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		n := int32(info.Args()[0].Int32())

		// A negative count means: go back into JS from here, which comes back
		// into this callback and builds one level deeper. That is the
		// re-entrancy question — whether entering the isolate and a context
		// from inside an in-flight callback is safe — asked directly.
		if n < 0 {
			out, err := info.Context().RunScript("goBuild(3)", "gav8_nested.js")
			if err != nil {
				buildErr = err
				return nil
			}
			return out
		}

		// Pinned leaves out of the pooled array, so each call leaves stale
		// entries behind for the next one — the production shape, and the one
		// that turns a pointer-check panic into a process abort here.
		pin.Unpin()
		leaves := make([][]byte, n)
		for i := range leaves {
			leaves[i] = []byte("row-" + strconv.Itoa(i))
		}
		spans, ptrs := pinLeaves(&pin, leaves...)

		p := &prog{}
		defer p.release()
		s := p.shape("i", "s")
		p.mark()
		p.op(v8.OpRepeat, 4)
		p.op(v8.OpInt)
		p.op(v8.OpStr)
		p.op(v8.OpObj, s)
		p.arr()
		p.end()
		p.dataCount(n)
		for i := int32(0); i < n; i++ {
			p.dataInt(int64(i))
		}

		payload := p.payload()
		// The keys are staged; the values are the pinned leaves above.
		payload.Spans = append(payload.Spans[:payload.KeySpans:payload.KeySpans], spans...)
		payload.Ptrs = ptrs

		val, err := v8.BuildValue(info.Context(), payload)
		if err != nil {
			buildErr = err
			return nil
		}
		return val
	})
	global.Set("goBuild", fn)

	ctx := v8.NewContext(iso, global)

	// Several calls, and several sizes: the consumer's crash needed more than
	// one call on one engine to show up.
	for i, n := range []int{1, 10, 100, 1000, 7} {
		out, err := ctx.RunScript(
			"(() => { const v = goBuild("+strconv.Itoa(n)+");"+
				" return v.length + ':' + v[v.length - 1].s; })()",
			"gav8_callback.js")
		fatalIf(t, err)
		fatalIf(t, buildErr)
		want := strconv.Itoa(n) + ":row-" + strconv.Itoa(n-1)
		if got := out.String(); got != want {
			t.Fatalf("call %d built %q, want %q", i, got, want)
		}
		out.Release()
	}

	out, err := ctx.RunScript("goBuild(3).length + ':' + goBuild(4).length", "gav8_callback.js")
	fatalIf(t, err)
	fatalIf(t, buildErr)
	if got := out.String(); got != "3:4" {
		t.Errorf("two builds in one script = %q, want %q", got, "3:4")
	}
	out.Release()

	// JS -> Go -> JS -> Go -> build, then unwind through all of it.
	out, err = ctx.RunScript("goBuild(-1).length + ':' + goBuild(-1)[2].s", "gav8_callback.js")
	fatalIf(t, err)
	fatalIf(t, buildErr)
	if got := out.String(); got != "3:row-2" {
		t.Errorf("build under a nested callback = %q, want %q", got, "3:row-2")
	}
	out.Release()

	ctx.Close()

	// The assertion. A leaked isolate entry aborts the process here rather
	// than failing this test, which is exactly why the test has to exist.
	iso.Dispose()
}

/********** the read-cost measurement **********/

// The bulk object constructor produces dictionary-mode objects where
// JSON.parse produces hidden-class ones, so reading properties out of a built
// tree costs more. How much more is the whole trade: at the measured ratio the
// build saving pays for tens of full passes over the data, and an SSR render
// makes one. A V8 upgrade that makes it materially worse should fail a test
// rather than quietly slow every page down.
//
// Not parallel: it is a timing measurement.
func TestBuildReadCostRatio(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const cols, rows, scans, reps = 20, 1000, 100, 5
	keys, data := benchRows(cols, rows)

	p := buildProgram(keys, data)
	defer p.release()
	built, err := v8.BuildValue(ctx, p.payload())
	fatalIf(t, err)
	defer built.Release()

	parsed, err := v8.JSONParse(ctx, benchJSON(keys, data))
	fatalIf(t, err)
	defer parsed.Release()

	fatalIf(t, ctx.Global().Set("built", built))
	fatalIf(t, ctx.Global().Set("parsed", parsed))

	// Two separate Function objects over the same source. One shared function
	// would see both object kinds at the same property loads, go megamorphic,
	// and mis-measure both arms — which is the likeliest explanation for the
	// 11x this was once reported as.
	setup := `globalThis.scanBuilt = new Function('rows', SRC);
	          globalThis.scanParsed = new Function('rows', SRC);`
	src := "let s = 0;" +
		"for (let n = 0; n < " + strconv.Itoa(scans) + "; n++)" +
		"  for (let i = 0; i < rows.length; i++) {" +
		"    const r = rows[i]; s += r.col_0 + r.col_3 + r.col_6;" +
		"  }" +
		"return s;"
	_, err = ctx.RunScript("const SRC = "+strconv.Quote(src)+";"+setup, "gav8_readcost_setup.js")
	fatalIf(t, err)

	run := func(expr string) time.Duration {
		start := time.Now()
		out, err := ctx.RunScript(expr, "gav8_readcost.js")
		fatalIf(t, err)
		_ = out
		return time.Since(start)
	}

	// Interleaved, so that anything drifting over the run (GC, thermal, a
	// background compile) lands on both arms rather than on one.
	var builtTime, parsedTime time.Duration
	run("scanBuilt(built)")
	run("scanParsed(parsed)")
	for i := 0; i < reps; i++ {
		builtTime += run("scanBuilt(built)")
		parsedTime += run("scanParsed(parsed)")
	}

	ratio := float64(builtTime) / float64(parsedTime)
	t.Logf("%d scans x %d rows x 3 property loads, %d reps: built %v, parsed %v, ratio %.2fx",
		scans, rows, reps, builtTime, parsedTime, ratio)

	const limit = 4.0
	if ratio > limit {
		t.Errorf("reads from bulk-constructed objects cost %.2fx reads from parsed ones, limit %.1fx; "+
			"if this is real, the object constructor in gav8_build.cc is no longer the right trade",
			ratio, limit)
	}
}

/********** benchmarks **********/

// buildProgram encodes the SQL-shaped result set from benchRows as a build
// program: one loop body of cols+2 ops, whatever the row count.
func buildProgram(keys []string, rows [][]any) *prog {
	return buildProgramObjOp(keys, rows, v8.OpObj)
}

// buildProgramObjOp is buildProgram with the object op chosen by the caller, so
// that OpObj and OpObjOmit can be measured and compared over identical data
// with every key present. Nothing here ever pushes undefined, so the two builds
// must produce the same tree.
func buildProgramObjOp(keys []string, rows [][]any, objOp uint32) *prog {
	p := &prog{}
	shape := p.shape(keys...)

	p.mark()
	p.op(v8.OpRepeat, uint32(len(keys))+2)
	for c := range keys {
		switch c % 3 {
		case 0:
			p.op(v8.OpInt)
		case 1:
			p.op(v8.OpF64)
		default:
			p.op(v8.OpStr)
		}
	}
	p.op(objOp, shape)
	p.arr()
	p.end()

	p.dataCount(int32(len(rows)))
	for _, row := range rows {
		for _, cell := range row {
			switch x := cell.(type) {
			case int32:
				p.dataInt(int64(x))
			case float64:
				p.dataF64(x)
			default:
				p.dataStr(x.(string))
			}
		}
	}
	return p
}

// BenchmarkBuildValue is the arm that matters: Go data in, a JS value out,
// nothing serialized and one crossing. It stages the payload inside the timed
// loop, reusing the buffers the way a pooled producer would, so it carries the
// same Go-side per-value work the BatchScope arm does.
func BenchmarkBuildValue(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)
			seed := buildProgram(keys, rows)
			defer seed.release()

			// Steady state: ops, shapes and key spans are fixed by the type,
			// so a producer computes them once. Everything downstream of the
			// key region is refilled per result set.
			payload := seed.payload()
			buf := make([]byte, 0, len(seed.buf))
			spans := make([]v8.Span, 0, len(seed.keySpans)+len(seed.valSpans))
			nums := make([]int64, 0, len(seed.nums))
			floats := make([]float64, 0, len(seed.floats))

			b.ResetTimer()
			for range b.N {
				buf = buf[:0]
				spans = spans[:0]
				nums = nums[:0]
				floats = floats[:0]
				for _, s := range seed.keySpans {
					// Key bytes are staged once, at the front of the buffer.
					off := uint32(len(buf))
					buf = append(buf, seed.buf[s.Off:s.Off+s.Len]...)
					spans = append(spans, v8.Span{Off: off, Len: s.Len})
				}
				for _, row := range rows {
					for _, cell := range row {
						switch x := cell.(type) {
						case int32:
							nums = append(nums, int64(x))
						case float64:
							floats = append(floats, x)
						default:
							s := x.(string)
							off := uint32(len(buf))
							buf = append(buf, s...)
							spans = append(spans, v8.Span{Off: off, Len: uint32(len(s))})
						}
					}
				}
				payload.Buf = buf
				payload.Spans = spans
				payload.Nums = nums
				payload.Floats = floats

				val, err := v8.BuildValue(ctx, payload)
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}

// BenchmarkBuildValuePrebuilt isolates the crossing and the V8 work from the
// cost of staging the payload, which is what a producer that already holds its
// data in this layout would pay.
func BenchmarkBuildValuePrebuilt(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)
			p := buildProgram(keys, rows)
			defer p.release()
			payload := p.payload()

			b.ResetTimer()
			for range b.N {
				val, err := v8.BuildValue(ctx, payload)
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}

// What OP_OBJ_OMIT costs when it omits nothing: the same tree as
// BenchmarkBuildValuePrebuilt, key for key, with the one op swapped. The
// difference is the compaction scan and the scratch key array, paid on the case
// a producer would hit by default if it emitted the omitting form
// unconditionally. If it is material, emit OP_OBJ for structs with no optional
// field — which is free, since the producer already knows which those are.
func BenchmarkBuildValuePrebuiltObjOmit(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)
			p := buildProgramObjOp(keys, rows, v8.OpObjOmit)
			defer p.release()
			payload := p.payload()

			b.ResetTimer()
			for range b.N {
				val, err := v8.BuildValue(ctx, payload)
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}

// And the shape the ops exist for: the same rows with a third of the columns
// optional and a third of those absent, so the omitting object really omits.
// Fewer properties are built, so this is not comparable to the two arms above
// as a like-for-like — it is here to show what the whole mechanism costs end to
// end on a payload that uses it.
func BenchmarkBuildValuePrebuiltOptional(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)

			p := &prog{}
			defer p.release()
			shape := p.shape(keys...)

			// Every third column is optional, which costs it two extra words.
			bodyLen := uint32(len(keys)) + 2
			for c := range keys {
				if c%3 == 2 {
					bodyLen += 2
				}
			}
			p.mark()
			p.op(v8.OpRepeat, bodyLen)
			for c := range keys {
				if c%3 == 2 {
					p.op(v8.OpOptional, 1)
					p.op(v8.OpStr)
					continue
				}
				if c%3 == 0 {
					p.op(v8.OpInt)
				} else {
					p.op(v8.OpF64)
				}
			}
			p.op(v8.OpObjOmit, shape)
			p.arr()
			p.end()

			p.dataCount(int32(len(rows)))
			for r, row := range rows {
				for c, cell := range row {
					switch x := cell.(type) {
					case int32:
						p.dataInt(int64(x))
					case float64:
						p.dataF64(x)
					default:
						present := (r+c)%3 != 0
						p.dataFlag(present)
						if present {
							p.dataStr(x.(string))
						}
					}
				}
			}
			payload := p.payload()

			b.ResetTimer()
			for range b.N {
				val, err := v8.BuildValue(ctx, payload)
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}
