// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	v8 "github.com/hlvs-apps/v8go"
)

// eval installs val on the global as `v` and returns the string result of
// running expr against it. Asserting from JS rather than from Go is the point:
// what matters is that the tree V8 ends up with is the one a script sees.
func eval(t *testing.T, ctx *v8.Context, val *v8.Value, expr string) string {
	t.Helper()
	fatalIf(t, ctx.Global().Set("v", val))
	out, err := ctx.RunScript(expr, "gav8_test.js")
	fatalIf(t, err)
	return out.String()
}

func TestBatchScopeRoundTrip(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	tests := []struct {
		name  string
		build func(s *v8.BatchScope) v8.LocalRef
		expr  string
		want  string
	}{
		{
			name:  "null",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Null() },
			expr:  "v === null ? 'null' : typeof v",
			want:  "null",
		},
		{
			name:  "undefined",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Undefined() },
			expr:  "typeof v",
			want:  "undefined",
		},
		{
			name:  "true",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Bool(true) },
			expr:  "typeof v + ':' + v",
			want:  "boolean:true",
		},
		{
			name:  "false",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Bool(false) },
			expr:  "typeof v + ':' + v",
			want:  "boolean:false",
		},
		{
			name:  "int32",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Int32(-2147483648) },
			expr:  "typeof v + ':' + v",
			want:  "number:-2147483648",
		},
		{
			name:  "float64",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Float64(1.5e300) },
			expr:  "typeof v + ':' + v",
			want:  "number:1.5e+300",
		},
		{
			name:  "float64 NaN",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Float64(math.NaN()) },
			expr:  "Number.isNaN(v) ? 'nan' : String(v)",
			want:  "nan",
		},
		{
			name:  "ascii string",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.String("hello world") },
			expr:  "typeof v + ':' + v.length + ':' + v",
			want:  "string:11:hello world",
		},
		{
			name:  "empty string",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.String("") },
			expr:  "typeof v + ':' + v.length",
			want:  "string:0",
		},
		{
			// Non-ASCII must take the NewFromUtf8 path and come out as the same
			// code points, including a surrogate pair for the astral one.
			name:  "non-ascii utf8",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.String("héllo · 日本 · 😀") },
			expr:  "v",
			want:  "héllo · 日本 · 😀",
		},
		{
			// Length-delimited, not NUL-terminated: the NUL is a character.
			name:  "interior NUL",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.String("a\x00b") },
			expr:  "v.length + ':' + [...v].map(c => c.charCodeAt(0)).join(',')",
			want:  "3:97,0,98",
		},
		{
			name:  "lone NUL",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.String("\x00") },
			expr:  "v.length + ':' + v.charCodeAt(0)",
			want:  "1:0",
		},
		{
			name:  "bytes",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Bytes([]byte{0, 1, 127, 128, 255}) },
			expr:  "(v instanceof Uint8Array) + ':' + v.length + ':' + Array.from(v).join(',')",
			want:  "true:5:0,1,127,128,255",
		},
		{
			name:  "empty bytes",
			build: func(s *v8.BatchScope) v8.LocalRef { return s.Bytes(nil) },
			expr:  "(v instanceof Uint8Array) + ':' + v.length",
			want:  "true:0",
		},
		{
			name: "empty object",
			build: func(s *v8.BatchScope) v8.LocalRef {
				return s.Object(s.Shape(nil), nil)
			},
			expr: "JSON.stringify(v) + ':' + (Object.getPrototypeOf(v) === Object.prototype)",
			want: "{}:true",
		},
		{
			name: "empty array",
			build: func(s *v8.BatchScope) v8.LocalRef {
				return s.Array(nil)
			},
			expr: "Array.isArray(v) + ':' + v.length",
			want: "true:0",
		},
		{
			name: "flat object",
			build: func(s *v8.BatchScope) v8.LocalRef {
				shape := s.Shape([]string{"id", "name", "ok"})
				return s.Object(shape, []v8.LocalRef{s.Int32(7), s.String("seven"), s.Bool(true)})
			},
			// Objects must look like parsed JSON to the JS consuming them:
			// Object.prototype, so hasOwnProperty and friends exist.
			expr: "JSON.stringify(v) + ':' + v.hasOwnProperty('id')",
			want: `{"id":7,"name":"seven","ok":true}:true`,
		},
		{
			name: "nested three deep",
			build: func(s *v8.BatchScope) v8.LocalRef {
				leafShape := s.Shape([]string{"deep"})
				midShape := s.Shape([]string{"inner"})
				topShape := s.Shape([]string{"outer"})
				leaf := s.Object(leafShape, []v8.LocalRef{s.String("bottom")})
				mid := s.Object(midShape, []v8.LocalRef{s.Array([]v8.LocalRef{leaf})})
				return s.Object(topShape, []v8.LocalRef{mid})
			},
			expr: "JSON.stringify(v)",
			want: `{"outer":{"inner":[{"deep":"bottom"}]}}`,
		},
		{
			name: "object with 50 keys",
			build: func(s *v8.BatchScope) v8.LocalRef {
				keys := make([]string, 50)
				vals := make([]v8.LocalRef, 50)
				for i := range keys {
					keys[i] = "k" + strconv.Itoa(i)
					vals[i] = s.Int32(int32(i))
				}
				return s.Object(s.Shape(keys), vals)
			},
			expr: "Object.keys(v).length + ':' + Object.keys(v).join(',') + ':' + Object.values(v).join(',')",
			want: "50:" + joinRange("k", 50) + ":" + joinRange("", 50),
		},
		{
			name: "array of 1000 uniform objects",
			build: func(s *v8.BatchScope) v8.LocalRef {
				shape := s.Shape([]string{"i", "s"})
				rows := make([]v8.LocalRef, 1000)
				for i := range rows {
					rows[i] = s.Object(shape, []v8.LocalRef{
						s.Int32(int32(i)),
						s.String("row-" + strconv.Itoa(i)),
					})
				}
				return s.Array(rows)
			},
			expr: `v.length + ':' + v[0].i + ':' + v[999].s + ':' +
				v.every((o, i) => o.i === i && o.s === 'row-' + i)`,
			want: "1000:0:row-999:true",
		},
		{
			name: "mixed array",
			build: func(s *v8.BatchScope) v8.LocalRef {
				return s.Array([]v8.LocalRef{
					s.Null(), s.Undefined(), s.Bool(false), s.Int32(3),
					s.Float64(0.5), s.String("x"), s.Array(nil),
				})
			},
			expr: "v.map(x => typeof x).join(',')",
			want: "object,undefined,boolean,number,number,string,object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := v8.NewBatchScope(ctx)
			defer s.Close()

			root := tt.build(s)
			val, err := s.Result(root)
			fatalIf(t, err)

			if got := eval(t, ctx, val, tt.expr); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

// A megabyte of bytes has to survive verbatim, which is the point of having
// gav8_bytes at all: it is the path that replaces base64.
func TestBatchScopeBytesOneMiB(t *testing.T) {
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

	s := v8.NewBatchScope(ctx)
	val, err := s.Result(s.Bytes(want))
	fatalIf(t, err)
	s.Close()

	if got := eval(t, ctx, val, "(v instanceof Uint8Array) + ':' + v.length"); got != "true:1048576" {
		t.Fatalf("1 MiB payload arrived as %q", got)
	}
	if got := val.ArrayBufferViewBytes(); !bytes.Equal(got, want) {
		t.Errorf("1 MiB payload did not round-trip byte for byte")
	}
}

// The load-bearing test. If a scope creates an m_value per node, the whole ABI
// is pointless — and the only visible symptom is this counter.
func TestBatchScopeNoPerNodeTracking(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 1000
	before := ctx.RetainedValueCount()

	s := v8.NewBatchScope(ctx)
	shape := s.Shape([]string{"id", "name", "score"})
	refs := make([]v8.LocalRef, rows)
	for i := range refs {
		refs[i] = s.Object(shape, []v8.LocalRef{
			s.Int32(int32(i)),
			s.String("name-" + strconv.Itoa(i)),
			s.Float64(float64(i) / 3),
		})
	}
	val, err := s.Result(s.Array(refs))
	fatalIf(t, err)
	s.Close()

	after := ctx.RetainedValueCount()
	if delta := after - before; delta != 1 {
		t.Fatalf("RetainedValueCount grew by %d building a %d-object tree, want exactly 1 (the root); "+
			"anything more means an m_value leaked per node", delta, rows)
	}

	// And the one retained value really is the whole tree.
	if got := eval(t, ctx, val, "v.length + ':' + v[999].name"); got != "1000:name-999" {
		t.Errorf("root value = %q, want %q", got, "1000:name-999")
	}
}

// Shape reuse is the mechanism: one interned key set, one hidden class, N
// objects. Size() catches a stray Local slipped in behind the caller's back.
func TestBatchScopeShapeReuse(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	const rows = 1000
	s := v8.NewBatchScope(ctx)
	defer s.Close()

	shape := s.Shape([]string{"a", "b", "c"})
	if shape == v8.InvalidShapeRef {
		t.Fatal("Shape returned InvalidShapeRef")
	}

	refs := make([]v8.LocalRef, rows)
	for i := range refs {
		refs[i] = s.Object(shape, []v8.LocalRef{
			s.Int32(int32(i)), s.Int32(int32(i * 2)), s.Int32(int32(i * 3)),
		})
	}
	root := s.Array(refs)

	// 3 leaves + 1 object per row, plus the root array. The interned keys and
	// the cached Object.prototype are deliberately not in this table.
	const want = rows*4 + 1
	if got := s.Size(); got != want {
		t.Errorf("Size() = %d, want %d (%d rows x 4 nodes + 1 array)", got, want, rows)
	}

	val, err := s.Result(root)
	fatalIf(t, err)

	// Every object must report the same key order, which is what sharing one
	// shape means from JS.
	const expr = `(() => {
		const orders = new Set(v.map(o => Object.keys(o).join(',')));
		return orders.size + ':' + [...orders].join('|');
	})()`
	if got := eval(t, ctx, val, expr); got != "1:a,b,c" {
		t.Errorf("key orders across %d objects = %q, want %q", rows, got, "1:a,b,c")
	}
}

// Closing a scope must free the tree and leave behind only what Result handed
// out, and releasing that must leave nothing at all.
func TestBatchScopeCloseFrees(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	before := ctx.RetainedValueCount()

	s := v8.NewBatchScope(ctx)
	shape := s.Shape([]string{"n"})
	refs := make([]v8.LocalRef, 1000)
	for i := range refs {
		refs[i] = s.Object(shape, []v8.LocalRef{s.Int32(int32(i))})
	}
	val, err := s.Result(s.Array(refs))
	fatalIf(t, err)

	s.Close()
	if got := ctx.RetainedValueCount(); got != before+1 {
		t.Fatalf("RetainedValueCount after Close = %d, want %d (pre-build + the retained root)", got, before+1)
	}

	// Closing twice is a no-op, not a double free.
	s.Close()
	if got := ctx.RetainedValueCount(); got != before+1 {
		t.Fatalf("RetainedValueCount after a second Close = %d, want %d", got, before+1)
	}

	val.Release()
	if got := ctx.RetainedValueCount(); got != before {
		t.Fatalf("RetainedValueCount after releasing the root = %d, want %d", got, before)
	}
}

// A failed builder must poison the result rather than hand back a tree with a
// hole in it, and an invalid ref must never crash a consumer of it.
func TestBatchScopeInvalidRefs(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	t.Run("invalid root", func(t *testing.T) {
		s := v8.NewBatchScope(ctx)
		defer s.Close()
		if _, err := s.Result(v8.InvalidLocalRef); err == nil {
			t.Error("Result(InvalidLocalRef) returned no error")
		}
	})

	t.Run("invalid ref inside an array", func(t *testing.T) {
		s := v8.NewBatchScope(ctx)
		defer s.Close()
		root := s.Array([]v8.LocalRef{s.Int32(1), v8.InvalidLocalRef})
		if root != v8.InvalidLocalRef {
			t.Errorf("Array with an invalid element = %d, want InvalidLocalRef", root)
		}
		if _, err := s.Result(s.Int32(2)); err == nil {
			t.Error("Result after a failed builder returned no error; a tree with a hole must not ship")
		}
	})

	t.Run("invalid shape", func(t *testing.T) {
		s := v8.NewBatchScope(ctx)
		defer s.Close()
		if got := s.Object(v8.InvalidShapeRef, nil); got != v8.InvalidLocalRef {
			t.Errorf("Object(InvalidShapeRef) = %d, want InvalidLocalRef", got)
		}
	})

	t.Run("duplicate shape keys", func(t *testing.T) {
		s := v8.NewBatchScope(ctx)
		defer s.Close()
		if got := s.Shape([]string{"a", "b", "a"}); got != v8.InvalidShapeRef {
			t.Errorf("Shape with a duplicate key = %d, want InvalidShapeRef", got)
		}
	})

	t.Run("wrong value count", func(t *testing.T) {
		s := v8.NewBatchScope(ctx)
		defer s.Close()
		shape := s.Shape([]string{"a", "b"})
		if got := s.Object(shape, []v8.LocalRef{s.Int32(1)}); got != v8.InvalidLocalRef {
			t.Errorf("Object with too few values = %d, want InvalidLocalRef", got)
		}
	})

	t.Run("ref from another scope", func(t *testing.T) {
		first := v8.NewBatchScope(ctx)
		var stale v8.LocalRef
		for i := 0; i < 10; i++ {
			stale = first.Int32(int32(i))
		}
		first.Close()

		second := v8.NewBatchScope(ctx)
		defer second.Close()
		// The stale ref is in range for the new scope only once it has grown
		// that far; before that it must be refused rather than resolved.
		if got := second.Array([]v8.LocalRef{stale}); got != v8.InvalidLocalRef {
			t.Errorf("Array with a ref from a closed scope = %d, want InvalidLocalRef", got)
		}
	})
}

// Two scopes opened back to back on one context must not see each other's
// values, and each must be independently usable.
func TestBatchScopeSequentialScopes(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	for i := 0; i < 3; i++ {
		s := v8.NewBatchScope(ctx)
		if got := s.Size(); got != 0 {
			t.Fatalf("fresh scope Size() = %d, want 0", got)
		}
		val, err := s.Result(s.String("scope-" + strconv.Itoa(i)))
		fatalIf(t, err)
		s.Close()

		if got := val.String(); got != "scope-"+strconv.Itoa(i) {
			t.Errorf("value from scope %d = %q", i, got)
		}
	}
}

func TestNewBatchScopeNilContext(t *testing.T) {
	t.Parallel()
	if recovered := recoverPanic(func() { v8.NewBatchScope(nil) }); recovered == nil {
		t.Error("NewBatchScope(nil) did not panic")
	}
}

/********** benchmarks **********/

// benchRows builds a SQL-shaped result set: cols columns wide, rows deep, with
// the mix of ints, floats and strings a query actually returns.
func benchRows(cols, rows int) ([]string, [][]any) {
	keys := make([]string, cols)
	for i := range keys {
		keys[i] = "col_" + strconv.Itoa(i)
	}
	out := make([][]any, rows)
	for r := range out {
		row := make([]any, cols)
		for c := range row {
			switch c % 3 {
			case 0:
				row[c] = int32(r*cols + c)
			case 1:
				row[c] = float64(r) + float64(c)/8
			default:
				row[c] = "value-" + strconv.Itoa(r) + "-" + strconv.Itoa(c)
			}
		}
		out[r] = row
	}
	return keys, out
}

// benchJSON encodes the rows by hand rather than through reflection. That is
// deliberately the fastest realistic Go encoder — a generic json.Marshal over
// []map[string]any is several times slower, mostly on sorting map keys — so the
// baseline the builder is measured against is the hard one, not the easy one.
func benchJSON(keys []string, rows [][]any) string {
	var buf []byte
	buf = append(buf, '[')
	for r, row := range rows {
		if r > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '{')
		for c, key := range keys {
			if c > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, '"')
			buf = append(buf, key...)
			buf = append(buf, '"', ':')
			switch x := row[c].(type) {
			case int32:
				buf = strconv.AppendInt(buf, int64(x), 10)
			case float64:
				buf = strconv.AppendFloat(buf, x, 'g', -1, 64)
			default:
				quoted, err := json.Marshal(x)
				if err != nil {
					panic(err)
				}
				buf = append(buf, quoted...)
			}
		}
		buf = append(buf, '}')
	}
	buf = append(buf, ']')
	return string(buf)
}

var benchSizes = []struct {
	name string
	cols int
	rows int
}{
	{"20x100", 20, 100},
	{"20x1000", 20, 1000},
}

// BenchmarkBatchScopeBuild is the builder half of the comparison: Go data in,
// a JS value out, nothing serialized.
func BenchmarkBatchScopeBuild(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)
			cells := make([]v8.LocalRef, size.cols)
			refs := make([]v8.LocalRef, size.rows)

			b.ResetTimer()
			for range b.N {
				s := v8.NewBatchScope(ctx)
				shape := s.Shape(keys)
				for r, row := range rows {
					for c, cell := range row {
						switch x := cell.(type) {
						case int32:
							cells[c] = s.Int32(x)
						case float64:
							cells[c] = s.Float64(x)
						default:
							cells[c] = s.String(x.(string))
						}
					}
					refs[r] = s.Object(shape, cells)
				}
				val, err := s.Result(s.Array(refs))
				if err != nil {
					b.Fatal(err)
				}
				s.Close()
				val.Release()
			}
		})
	}
}

// BenchmarkJSONParse is the baseline the spec asks for: the same tree, already
// serialized, parsed by V8. It excludes Go's json.Marshal on purpose — see
// BenchmarkJSONMarshalParse for the cost of the whole path the builder would
// actually replace.
func BenchmarkJSONParse(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)
			payload := benchJSON(keys, rows)

			b.ResetTimer()
			for range b.N {
				val, err := v8.JSONParse(ctx, payload)
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}

// BenchmarkJSONMarshalParse measures the path a bridge shipping JSON really
// pays: encode in Go, parse in V8.
func BenchmarkJSONMarshalParse(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			iso := v8.NewIsolate()
			defer iso.Dispose()
			ctx := v8.NewContext(iso)
			defer ctx.Close()

			keys, rows := benchRows(size.cols, size.rows)

			b.ResetTimer()
			for range b.N {
				val, err := v8.JSONParse(ctx, benchJSON(keys, rows))
				if err != nil {
					b.Fatal(err)
				}
				val.Release()
			}
		})
	}
}

// joinRange renders "<prefix>0,<prefix>1,..." for n entries.
func joinRange(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = prefix + strconv.Itoa(i)
	}
	return strings.Join(parts, ",")
}
