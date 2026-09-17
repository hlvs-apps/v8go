package v8go_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v8 "github.com/hlvs-apps/v8go"
)

func makeSnapshot(t *testing.T) *v8.Snapshot {
	t.Helper()
	s, err := v8.CreateSnapshot([]v8.SnapshotScript{
		{Source: `globalThis.privateBind = (() => { let host; globalThis.callHost = () => host(); return f => { host=f; }; })(); Promise.resolve().then(() => { if ('privateBind' in globalThis) throw Error('capture leaked into microtask'); globalThis.drained=true; });`, Origin: "bootstrap.js", CaptureGlobals: []string{"privateBind"}},
		{Source: `if ('privateBind' in globalThis) throw Error('capture leaked'); globalThis.count=41; globalThis.saved=()=>++count;`, Origin: "app.js", CaptureGlobals: []string{"saved"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func restoreSnapshot(t *testing.T, s *v8.Snapshot) {
	t.Helper()
	iso, err := v8.NewIsolateWithSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	defer iso.Dispose()
	internal := v8.NewContext(iso)
	v, err := internal.RunScript("typeof count", "default.js")
	if err != nil || v.String() != "undefined" {
		t.Fatalf("app duplicated in default context: %v %v", v, err)
	}
	internal.Close()
	for n := 0; n < 3; n++ {
		ctx, err := v8.NewContextFromSnapshot(iso, s.ContextIndex())
		if err != nil {
			t.Fatal(err)
		}
		bind, err := ctx.SnapshotData(0)
		if err != nil {
			t.Fatal(err)
		}
		fn, err := bind.AsFunction()
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		host := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
			defer info.Release()
			calls++
			v, _ := v8.NewValue(iso, "fresh")
			return v
		}).GetFunction(ctx)
		if _, err = fn.Call(v8.Undefined(iso), host.Value); err != nil {
			t.Fatal(err)
		}
		out, err := ctx.RunScript("callHost()", "restore.js")
		if err != nil || out.String() != "fresh" || calls != 1 {
			t.Fatalf("callback: %v %v %d", out, err, calls)
		}
		saved, err := ctx.SnapshotData(1)
		if err != nil {
			t.Fatal(err)
		}
		f, err := saved.AsFunction()
		if err != nil {
			t.Fatal(err)
		}
		out, err = f.Call(v8.Undefined(iso))
		if err != nil || out.Int32() != 42 {
			t.Fatalf("state: %v %v", out, err)
		}
		if _, err = ctx.SnapshotData(0); err == nil {
			t.Fatal("capture was not consumed")
		}
		out, err = ctx.RunScript("drained && !('saved' in globalThis)", "check.js")
		if err != nil || !out.Boolean() {
			t.Fatalf("checkpoint/capture: %v %v", out, err)
		}
		ctx.Close()
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := makeSnapshot(t)
	if s.CaptureCount() != 2 {
		t.Fatal(s.CaptureCount())
	}
	bytes := s.Bytes()
	parsed, err := v8.ParseSnapshot(bytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := range bytes {
		bytes[i] = 0
	}
	restoreSnapshot(t, parsed)
	restoreSnapshot(t, s)
}

func TestSnapshotProcess(t *testing.T) {
	path := os.Getenv("V8GO_SNAPSHOT_TEST_PATH")
	mode := os.Getenv("V8GO_SNAPSHOT_TEST_MODE")
	if mode == "create" {
		if err := os.WriteFile(path, makeSnapshot(t).Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "reject" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		v8.SetFlags("--jitless")
		_, err = v8.ParseSnapshot(b)
		var compatibility *v8.SnapshotCompatibilityError
		if !errors.As(err, &compatibility) {
			t.Fatalf("flags mismatch not rejected: %v", err)
		}
		return
	}
	if mode == "restore" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := v8.ParseSnapshot(b)
		if err != nil {
			t.Fatal(err)
		}
		restoreSnapshot(t, s)
		return
	}
	if mode == "mixed" || mode == "mixed-last" { //nolint:nestif // Exercise both complete native isolate lifetime orders in the child process.
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := v8.ParseSnapshot(b)
		if err != nil {
			t.Fatal(err)
		}
		var first *v8.Isolate
		if mode == "mixed-last" {
			first, err = v8.NewIsolateWithSnapshot(s)
			if err != nil {
				t.Fatal(err)
			}
		}
		ordinary := v8.NewIsolate()
		defer ordinary.Dispose()
		ordinaryContext := v8.NewContext(ordinary)
		defer ordinaryContext.Close()
		restoreSnapshot(t, s)
		if first == nil {
			first, err = v8.NewIsolateWithSnapshot(s)
			if err != nil {
				t.Fatal(err)
			}
		}
		defer first.Dispose()
		firstContext, err := v8.NewContextFromSnapshot(first, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer firstContext.Close()
		second, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: `globalThis.marker={name:'other', values:new Uint8Array([3,7,11])};globalThis.next=()=>marker.values.reduce((x,y)=>x+y,0)`, CaptureGlobals: []string{"next"}}})
		if err != nil {
			t.Fatal(err)
		}
		secondIsolate, err := v8.NewIsolateWithSnapshot(second)
		if err != nil {
			t.Fatal(err)
		}
		defer secondIsolate.Dispose()
		secondContext, err := v8.NewContextFromSnapshot(secondIsolate, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer secondContext.Close()
		next, err := secondContext.SnapshotData(0)
		if err != nil {
			t.Fatal(err)
		}
		fn, err := next.AsFunction()
		if err != nil {
			t.Fatal(err)
		}
		result, err := fn.Call(v8.Undefined(secondIsolate))
		if err != nil || result.Int32() != 21 {
			t.Fatalf("different snapshot: %v %v", result, err)
		}
		result, err = firstContext.RunScript("++count", "first.js")
		if err != nil || result.Int32() != 42 {
			t.Fatalf("first snapshot: %v %v", result, err)
		}
		result, err = ordinaryContext.RunScript("JSON.stringify({answer:42})", "ordinary.js")
		if err != nil || result.String() != `{"answer":42}` {
			t.Fatalf("ordinary: %v %v", result, err)
		}
		return
	}
	path = filepath.Join(t.TempDir(), "snapshot.bin")
	for _, mode = range []string{"create", "restore", "mixed", "mixed-last", "reject"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSnapshotProcess$") //nolint:gosec // Re-exec this test binary; neither executable nor arguments come from external input.
		cmd.Env = append(os.Environ(), "V8GO_SNAPSHOT_TEST_PATH="+path, "V8GO_SNAPSHOT_TEST_MODE="+mode)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, out)
		}
	}
}

func TestSnapshotRejects(t *testing.T) {
	s := makeSnapshot(t)
	for _, field := range []string{"Identity", "Digest", "Format", "Captures", "DataLength"} {
		var obj map[string]interface{}
		encoded := s.Bytes()
		headerEnd := 12 + int(binary.LittleEndian.Uint32(encoded[8:12]))
		if err := json.Unmarshal(encoded[12:headerEnd], &obj); err != nil {
			t.Fatal(err)
		}
		if field == "Captures" || field == "DataLength" {
			obj[field] = -1
		} else {
			obj[field] = "bad"
		}
		header, _ := json.Marshal(obj)
		b := make([]byte, 12+len(header)+len(encoded)-headerEnd)
		copy(b, "V8GOSNP2")
		binary.LittleEndian.PutUint32(b[8:12], uint32(len(header)))
		copy(b[12:], header)
		copy(b[12+len(header):], encoded[headerEnd:])
		_, err := v8.ParseSnapshot(b)
		var typed *v8.SnapshotCompatibilityError
		if !errors.As(err, &typed) {
			t.Fatalf("%s: expected typed rejection: %v", field, err)
		}
	}
	if _, err := v8.ParseSnapshot(nil); err == nil {
		t.Fatal("nil accepted")
	}
	if _, err := v8.NewIsolateWithSnapshot(nil); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	if _, err := v8.NewIsolateWithSnapshot(new(v8.Snapshot)); err == nil {
		t.Fatal("zero snapshot accepted")
	}
	if _, err := v8.NewContextFromSnapshot(nil, 0); err == nil {
		t.Fatal("nil isolate accepted")
	}
	for _, script := range []v8.SnapshotScript{{Source: "throw Error('failure')"}, {Source: "function ("}, {Source: "", CaptureGlobals: []string{"missing"}}, {Source: "Object.defineProperty(globalThis,'locked',{value:1})", CaptureGlobals: []string{"locked"}}, {CachedData: &v8.CompilerCachedData{}}} {
		if _, err := v8.CreateSnapshot([]v8.SnapshotScript{script}); err == nil {
			t.Fatalf("accepted %+v", script)
		}
	}
}

func TestSnapshotRepeatedLifecycle(t *testing.T) {
	for i := 0; i < 20; i++ {
		restoreSnapshot(t, makeSnapshot(t))
		if _, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: "throw Error('failure')"}}); err == nil {
			t.Fatal("missing error")
		}
	}
}

func TestCodeCacheEmptyAndAfterExecution(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	if _, err := iso.CompileUnboundScript("1", "", v8.CompileOptions{CachedData: &v8.CompilerCachedData{}}); err == nil {
		t.Fatal("empty cache accepted")
	}
	script, err := iso.CompileUnboundScript("globalThis.f=()=>42;f()", "cache.js", v8.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = script.Run(ctx); err != nil {
		t.Fatal(err)
	}
	cache := script.CreateCodeCache()
	if cache == nil || len(cache.Bytes) == 0 {
		t.Fatal("no cache")
	}
	snap, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: "globalThis.f=()=>42;f()", Origin: "cache.js", CachedData: cache}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := v8.NewIsolateWithSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Dispose()
	app, err := v8.NewContextFromSnapshot(restored, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	v, err := app.RunScript("f()", "use.js")
	if err != nil || v.Int32() != 42 {
		t.Fatalf("%v %v", v, err)
	}
}

func TestSnapshotRejectsCacheBeforeEvaluation(t *testing.T) {
	_, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: "throw Error('must not execute')", CachedData: &v8.CompilerCachedData{Bytes: []byte{1, 2, 3, 4}}}})
	if err == nil || err.Error() != "V8 rejected snapshot script code cache" {
		t.Fatalf("expected cache rejection before evaluation, got %v", err)
	}
}

func TestSnapshotConcurrentCreators(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: "globalThis.answer=42"}})
			if err != nil {
				t.Error(err)
				return
			}
			iso, err := v8.NewIsolateWithSnapshot(s)
			if err != nil {
				t.Error(err)
				return
			}
			defer iso.Dispose()
			ctx, err := v8.NewContextFromSnapshot(iso, 0)
			if err != nil {
				t.Error(err)
				return
			}
			defer ctx.Close()
			value, err := ctx.RunScript("answer", "concurrent.js")
			if err != nil {
				t.Error(err)
				return
			}
			if value.Int32() != 42 {
				t.Error("incorrect state")
			}
		}()
	}
	wg.Wait()
}

func TestSnapshotPrivateValidatorAfterMicrotasks(t *testing.T) {
	setup := v8.SnapshotScript{Source: `(() => { let failed=false; globalThis.forbidden=()=>{failed=true;throw Error('forbidden')}; globalThis.guard=()=>{if(failed)throw Error('sticky failure')}; })()`, CaptureGlobals: []string{"guard"}}
	_, err := v8.CreateSnapshot([]v8.SnapshotScript{setup, {Source: `if ('guard' in globalThis) throw Error('exposed'); Promise.resolve().then(()=>{try{forbidden()}catch{}})`, ValidateCaptured: []int{0}}})
	if err == nil || err.Error() != "Error: sticky failure" {
		t.Fatalf("expected sticky failure after checkpoint: %v", err)
	}
	if _, err = v8.CreateSnapshot([]v8.SnapshotScript{setup, {Source: `Promise.resolve().then(()=>{globalThis.ok=42})`, ValidateCaptured: []int{0}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = v8.CreateSnapshot([]v8.SnapshotScript{{Source: "globalThis.x=1", CaptureGlobals: []string{"x"}, ValidateCaptured: []int{0}}}); err == nil {
		t.Fatal("noncallable validator accepted")
	}
	if _, err = v8.CreateSnapshot([]v8.SnapshotScript{{ValidateCaptured: []int{0}}}); err == nil {
		t.Fatal("missing validator accepted")
	}
	for _, body := range []string{"return 1", "throw Error('async failure')"} {
		_, err = v8.CreateSnapshot([]v8.SnapshotScript{{
			Source:           "globalThis.guard = async () => { " + body + " };",
			CaptureGlobals:   []string{"guard"},
			ValidateCaptured: []int{0},
		}})
		if err == nil || err.Error() != "snapshot validators must be synchronous" {
			t.Fatalf("async validator was not rejected: %v", err)
		}
	}
}

func TestSnapshotWithOrdinaryIsolate(t *testing.T) {
	s := makeSnapshot(t)
	ordinary := v8.NewIsolate()
	defer ordinary.Dispose()
	ctx := v8.NewContext(ordinary)
	defer ctx.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The helpers call t.Fatal, which must run on the goroutine of the test
		// it fails; a subtest gives this goroutine one of its own.
		t.Run("concurrent restores", func(t *testing.T) {
			for i := 0; i < 12; i++ {
				restoreSnapshot(t, makeSnapshot(t))
			}
		})
	}()
	// t.Run must return before this test does, including after a t.Fatal below.
	defer func() { <-done }()
	restoreSnapshot(t, s)
	for i := 0; i < 100; i++ {
		value, err := ctx.RunScript("Array.from({length:10},(_,i)=>i).join(',')", "ordinary.js")
		if err != nil || value.String() != "0,1,2,3,4,5,6,7,8,9" {
			t.Errorf("ordinary heap affected: %v %v", value, err)
			break
		}
		value.Release()
	}
}

func TestSnapshotValidatorMicrotasks(t *testing.T) {
	snapshot, err := v8.CreateSnapshot([]v8.SnapshotScript{{
		Source: `globalThis.ready = 0;
		globalThis.prepare = () => { Promise.resolve().then(() => { ready = 42; }); };
		globalThis.validate = () => { if (ready !== 42) throw Error('validator work pending'); };`,
		CaptureGlobals:   []string{"prepare", "validate"},
		ValidateCaptured: []int{0, 1},
	}, {
		Source:           `if (ready !== 42) throw Error('script ran before validator work');`,
		ValidateCaptured: []int{0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	iso, err := v8.NewIsolateWithSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer iso.Dispose()
	ctx, err := v8.NewContextFromSnapshot(iso, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	value, err := ctx.RunScript("ready", "validator-state.js")
	if err != nil || value.Int32() != 42 {
		t.Fatalf("validator work was not captured: %v %v", value, err)
	}
}

func TestSnapshotBinaryEnvelopeRejectsMalformed(t *testing.T) {
	original := makeSnapshot(t).Bytes()
	headerEnd := 12 + int(binary.LittleEndian.Uint32(original[8:12]))
	var header map[string]any
	if err := json.Unmarshal(original[12:headerEnd], &header); err != nil {
		t.Fatal(err)
	}
	if _, exists := header["Data"]; exists {
		t.Fatal("native payload must not be JSON encoded")
	}
	if got := int(header["DataLength"].(float64)); got != len(original)-headerEnd {
		t.Fatal("native data is not stored verbatim")
	}
	cases := map[string][]byte{
		"short prefix":     original[:11],
		"truncated header": original[:headerEnd-1],
		"truncated body":   original[:len(original)-1],
		"extra body":       append(bytes.Clone(original), 0),
	}
	for _, name := range []string{"magic", "zero header", "oversize header", "sign-bit header", "corrupt header", "corrupt body", "oversize declaration"} {
		data := bytes.Clone(original)
		switch name {
		case "magic":
			data[0] ^= 255
		case "zero header":
			binary.LittleEndian.PutUint32(data[8:12], 0)
		case "oversize header":
			binary.LittleEndian.PutUint32(data[8:12], ^uint32(0))
		case "sign-bit header":
			// Wraps negative through int on 32-bit targets.
			binary.LittleEndian.PutUint32(data[8:12], 1<<31)
		case "corrupt header":
			data[12] = 0
		case "corrupt body":
			data[len(data)-1] ^= 255
		case "oversize declaration":
			header["DataLength"] = 257 << 20
			metadata, _ := json.Marshal(header)
			data = make([]byte, 12+len(metadata)+len(original)-headerEnd)
			copy(data, "V8GOSNP2")
			binary.LittleEndian.PutUint32(data[8:12], uint32(len(metadata)))
			copy(data[12:], metadata)
			copy(data[12+len(metadata):], original[headerEnd:])
		}
		cases[name] = data
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v8.ParseSnapshot(data)
			var compatibility *v8.SnapshotCompatibilityError
			if !errors.As(err, &compatibility) {
				t.Fatalf("expected typed preflight rejection: %v", err)
			}
		})
	}
}

func TestSnapshotIsolatesShareNativeBlob(t *testing.T) {
	s := makeSnapshot(t)
	before := v8.SnapshotNativeBlobCount()
	isolates := make([]*v8.Isolate, 4)
	for i := range isolates {
		iso, err := v8.NewIsolateWithSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		defer iso.Dispose()
		isolates[i] = iso
	}
	if got := v8.SnapshotNativeBlobCount() - before; got != 1 {
		t.Fatalf("four isolates from one Snapshot hold %d native blob copies, want 1", got)
	}
	for _, iso := range isolates {
		iso.Dispose()
	}
	isolates[0].Dispose() // Dispose is idempotent and must not release the blob twice.
	if got := v8.SnapshotNativeBlobCount() - before; got != 0 {
		t.Fatalf("native blob outlived its last isolate: %d copies", got)
	}
	// Survivor check: the blob must stay valid while any isolate still uses it.
	first, err := v8.NewIsolateWithSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Dispose()
	second, err := v8.NewIsolateWithSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	second.Dispose()
	ctx, err := v8.NewContextFromSnapshot(first, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if v, err := ctx.RunScript("++count", "survivor.js"); err != nil || v.Int32() != 42 {
		t.Fatalf("isolate lost its snapshot blob: %v %v", v, err)
	}
}

// A copied Snapshot value must share the native blob's user count: otherwise
// disposing the original's only isolate frees the blob under the copy's.
func TestSnapshotCopySharesNativeBlob(t *testing.T) {
	original := makeSnapshot(t)
	copied := *original
	before := v8.SnapshotNativeBlobCount()
	first, err := v8.NewIsolateWithSnapshot(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := v8.NewIsolateWithSnapshot(&copied)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Dispose()
	if got := v8.SnapshotNativeBlobCount() - before; got != 1 {
		t.Fatalf("original and copy hold %d native blob copies, want 1", got)
	}
	first.Dispose()
	if got := v8.SnapshotNativeBlobCount() - before; got != 1 {
		t.Fatalf("blob freed while the copy's isolate still uses it: %d copies", got)
	}
	ctx, err := v8.NewContextFromSnapshot(second, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if v, err := ctx.RunScript("++count", "copy.js"); err != nil || v.Int32() != 42 {
		t.Fatalf("copy's isolate lost its snapshot blob: %v %v", v, err)
	}
}

func TestSnapshotRejectsUnmappedPlatform(t *testing.T) {
	encoded := makeSnapshot(t).Bytes()
	parsed, err := v8.ParseSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	restore := v8.SetSnapshotNativeIdentity(map[string]string{})
	defer restore()
	if id := v8.StartupIdentity(); !strings.Contains(id, "/unmapped-platform/") {
		t.Fatalf("identity does not name the unmapped platform: %s", id)
	}
	var compatibility *v8.SnapshotCompatibilityError
	if _, err = v8.CreateSnapshot([]v8.SnapshotScript{{Source: "1"}}); !errors.As(err, &compatibility) {
		t.Fatalf("create on an unmapped platform: %v", err)
	}
	if _, err = v8.ParseSnapshot(encoded); !errors.As(err, &compatibility) {
		t.Fatalf("parse on an unmapped platform: %v", err)
	}
	if _, err = v8.NewIsolateWithSnapshot(parsed); !errors.As(err, &compatibility) {
		t.Fatalf("restore on an unmapped platform: %v", err)
	}
}

// A throw inside a Promise job rejects that promise; it is not an exception
// the microtask checkpoint reports, so it does not fail preparation. The README
// points at ValidateCaptured for surfacing such failures.
func TestSnapshotPromiseJobThrowDoesNotFailPreparation(t *testing.T) {
	_, err := v8.CreateSnapshot([]v8.SnapshotScript{{Source: `Promise.resolve().then(() => { throw Error('late'); })`}})
	if err != nil {
		t.Fatalf("promise job throw failed preparation: %v", err)
	}
}
