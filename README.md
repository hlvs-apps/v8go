# Execute JavaScript from Go

<a href="https://github.com/hlvs-apps/v8go/releases"><img src="https://img.shields.io/github/v/release/hlvs-apps/v8go" alt="Github release"></a>
[![Go Report Card](https://goreportcard.com/badge/hlvs-apps/v8go)](https://goreportcard.com/report/hlvs-apps/v8go)
[![Go Reference](https://pkg.go.dev/badge/hlvs-apps/v8go.svg)](https://pkg.go.dev/hlvs-apps/v8go)
[![Test](https://github.com/hlvs-apps/v8go/actions/workflows/main.yml/badge.svg)](https://github.com/hlvs-apps/v8go/actions/workflows/main.yml)
![V8 Build](https://github.com/hlvs-apps/v8go/workflows/V8%20Build/badge.svg)
![V8 Version](https://img.shields.io/badge/V8-14.6.202.28-blue)

<img src="gopher.jpg" width="200px" alt="V8 Gopher based on original artwork from the amazing Renee French" />

## What this is

`v8go` lets you execute JavaScript from Go using [V8](https://v8.dev), Google's
JavaScript engine. Prebuilt static V8 libraries are shipped for Linux, macOS and
Windows, so `go get` works out of the box — you should not need to build V8
yourself.

## What it is based on

This repository is a continuation of the original
[rogchap/v8go](https://github.com/rogchap/v8go), created by
[Roger Chapman](https://github.com/rogchap) and the v8go contributors. Upstream
has been dormant since April 2023. The project was carried forward by
[Sebastian Döll](https://github.com/katallaxie) at
[katallaxie/v8go](https://github.com/katallaxie/v8go), and this repository
continues from there.

Most of the code here is upstream's work. It is distributed under the
BSD-3-Clause terms in [LICENSE](LICENSE), Copyright (c) 2019 Roger Chapman and
the v8go contributors.

The git history is rooted on rogchap's original commits — they keep their
original hashes and PGP signatures, so they can be verified against upstream —
rather than being collapsed into a single initial commit. `git log` therefore
shows the full lineage of the project.

### What this repository adds

- **Current V8.** Tracks recent V8 releases (currently 14.6.202.28) with
  binaries built in CI, rather than the 11.x series upstream stopped at.
- **Windows (amd64) support** via the MinGW-w64 toolchain — see
  [Windows](#windows).
- **`Value.ArrayBufferViewBytes() []byte`** — copies the bytes of any
  `ArrayBufferView` (typed array or `DataView`) into a Go-owned slice with a
  single memcpy, respecting `byteOffset`/`byteLength`.
- **Abseil isolation.** V8's bundled Abseil is rebuilt into the `absl::v8go`
  inline namespace, so it cannot collide at link time with another copy of
  Abseil in your binary.

## Usage

```go
import v8 "github.com/hlvs-apps/v8go"
```

### Running a script

```go
ctx := v8.NewContext() // creates a new V8 context with a new Isolate aka VM
ctx.RunScript("const add = (a, b) => a + b", "math.js") // executes a script on the global context
ctx.RunScript("const result = add(3, 4)", "main.js") // any functions previously added to the context can be called
val, _ := ctx.RunScript("result", "value.js") // return a value in JavaScript back to Go
fmt.Printf("addition result: %s", val)
```

### One VM, many contexts

```go
iso := v8.NewIsolate() // creates a new JavaScript VM
ctx1 := v8.NewContext(iso) // new context within the VM
ctx1.RunScript("const multiply = (a, b) => a * b", "math.js")

ctx2 := v8.NewContext(iso) // another context on the same VM
if _, err := ctx2.RunScript("multiply(3, 4)", "main.js"); err != nil {
  // this will error as multiply is not defined in this context
}
```

### JavaScript function with Go callback

```go
iso := v8.NewIsolate() // create a new VM
// a template that represents a JS function
printfn := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
    fmt.Printf("%v", info.Args()) // when the JS function is called this Go callback will execute
    return nil // you can return a value back to the JS caller if required
})
global := v8.NewObjectTemplate(iso) // a template that represents a JS Object
global.Set("print", printfn) // sets the "print" property of the Object to our function
ctx := v8.NewContext(iso, global) // new Context with the global Object set to our object template
ctx.RunScript("print('foo')", "print.js") // will execute the Go callback with a single argunent 'foo'
```

### Update a JavaScript object from Go

```go
ctx := v8.NewContext() // new context with a default VM
obj := ctx.Global() // get the global object from the context
obj.Set("version", "v1.0.0") // set the property "version" on the object
val, _ := ctx.RunScript("version", "version.js") // global object will have the property set within the JS VM
fmt.Printf("version: %s", val)

if obj.Has("version") { // check if a property exists on the object
    obj.Delete("version") // remove the property from the object
}
```

### JavaScript errors

```go
val, err := ctx.RunScript(src, filename)
if err != nil {
  e := err.(*v8.JSError) // JavaScript errors will be returned as the JSError struct
  fmt.Println(e.Message) // the message of the exception thrown
  fmt.Println(e.Location) // the filename, line number and the column where the error occured
  fmt.Println(e.StackTrace) // the full stack trace of the error, if available

  fmt.Printf("javascript error: %v", e) // will format the standard error message
  fmt.Printf("javascript stack trace: %+v", e) // will format the full error stack trace
}
```

### Pre-compile context-independent scripts to speed-up execution times

For scripts that are large or are repeatedly run in different contexts,
it is beneficial to compile the script once and used the cached data from that
compilation to avoid recompiling every time you want to run it.

```go
source := "const multiply = (a, b) => a * b"
iso1 := v8.NewIsolate() // creates a new JavaScript VM
ctx1 := v8.NewContext(iso1) // new context within the VM
script1, _ := iso1.CompileUnboundScript(source, "math.js", v8.CompileOptions{}) // compile script to get cached data
val, _ := script1.Run(ctx1)

cachedData := script1.CreateCodeCache()

iso2 := v8.NewIsolate() // create a new JavaScript VM
ctx2 := v8.NewContext(iso2) // new context within the VM

script2, _ := iso2.CompileUnboundScript(source, "math.js", v8.CompileOptions{CachedData: cachedData}) // compile script in new isolate with cached data
val, _ = script2.Run(ctx2)
```

### Terminate long running scripts

```go
vals := make(chan *v8.Value, 1)
errs := make(chan error, 1)

go func() {
    val, err := ctx.RunScript(script, "forever.js") // exec a long running script
    if err != nil {
        errs <- err
        return
    }
    vals <- val
}()

select {
case val := <- vals:
    // success
case err := <- errs:
    // javascript error
case <- time.After(200 * time.Milliseconds):
    vm := ctx.Isolate() // get the Isolate from the context
    vm.TerminateExecution() // terminate the execution
    err := <- errs // will get a termination error back from the running script
}
```

### CPU Profiler

```go
func createProfile() {
	iso := v8.NewIsolate()
	ctx := v8.NewContext(iso)
	cpuProfiler := v8.NewCPUProfiler(iso)

	cpuProfiler.StartProfiling("my-profile")

	ctx.RunScript(profileScript, "script.js") # this script is defined in cpuprofiler_test.go
	val, _ := ctx.Global().Get("start")
	fn, _ := val.AsFunction()
	fn.Call(ctx.Global())

	cpuProfile := cpuProfiler.StopProfiling("my-profile")

	printTree("", cpuProfile.GetTopDownRoot()) # helper function to print the profile
}

func printTree(nest string, node *v8.CPUProfileNode) {
	fmt.Printf("%s%s %s:%d:%d\n", nest, node.GetFunctionName(), node.GetScriptResourceName(), node.GetLineNumber(), node.GetColumnNumber())
	count := node.GetChildrenCount()
	if count == 0 {
		return
	}
	nest = fmt.Sprintf("%s  ", nest)
	for i := 0; i < count; i++ {
		printTree(nest, node.GetChild(i))
	}
}

// Output
// (root) :0:0
//   (program) :0:0
//   start script.js:23:15
//     foo script.js:15:13
//       delay script.js:12:15
//         loop script.js:1:14
//       bar script.js:13:13
//         delay script.js:12:15
//           loop script.js:1:14
//       baz script.js:14:13
//         delay script.js:12:15
//           loop script.js:1:14
//   (garbage collector) :0:0
```

## Benchmark

Run the benchmarks via `make bench`.

```bash
go vet ./...
go test -bench=. | go tool golang.org/x/perf/cmd/benchstat -
goos: linux
goarch: arm64
pkg: github.com/hlvs-apps/v8go
                        │      -       │
                        │    sec/op    │
Context-8                 117.9µ ± ∞ ¹
IsolateInitialization-8   305.1µ ± ∞ ¹
IsolateInitAndRun-8       434.5µ ± ∞ ¹
IsolateCodeCache-8        420.8µ ± ∞ ¹
geomean                   284.8µ
¹ need >= 6 samples for confidence interval at level 0.95

                        │      -      │
                        │    B/op     │
Context-8                 768.0 ± ∞ ¹
IsolateInitialization-8   152.0 ± ∞ ¹
IsolateInitAndRun-8       921.0 ± ∞ ¹
IsolateCodeCache-8        264.0 ± ∞ ¹
geomean                   410.5
¹ need >= 6 samples for confidence interval at level 0.95

                        │      -      │
                        │  allocs/op  │
Context-8                 18.00 ± ∞ ¹
IsolateInitialization-8   5.000 ± ∞ ¹
IsolateInitAndRun-8       23.00 ± ∞ ¹
IsolateCodeCache-8        12.00 ± ∞ ¹
geomean                   12.55
¹ need >= 6 samples for confidence interval at level 0.95
```

## Documentation

Go Reference & more examples: https://pkg.go.dev/hlvs-apps/v8go

### Support

If you would like to ask questions about this library or want to keep up-to-date with the latest changes and releases,
please join the [**#v8go**](https://gophers.slack.com/channels/v8go) channel on Gophers Slack. [Click here to join the Gophers Slack community!](https://invite.slack.golangbridge.org/)

### Windows

Windows (amd64) is supported via the **MinGW-w64** toolchain, which is what cgo
links with on Windows. As with Linux and macOS, a prebuilt static library is
included, so `go get` works out of the box — **but your build environment must
use a MinGW-w64 GCC** (e.g. the `mingw-w64-x86_64-gcc` toolchain from
[MSYS2](https://www.msys2.org/), or another mingw-w64 distribution) as cgo's C
compiler. MSVC is not supported.

The V8 static library is built in CI (see the `windows` job in
`.github/workflows/v8_build.yml`) from the patches under
[`patches/windows/`](patches/windows/), which are vendored from the
actively-maintained [MSYS2 `mingw-w64-v8`](https://github.com/msys2/MINGW-packages/tree/master/mingw-w64-v8)
package and track the same V8 version this project pins. `deps/build.py` applies
them (see `apply_mingw_patches()`) on top of the `gclient`-fetched V8 tree when
invoked with `--os windows`.

> Historical note: Windows support was previously removed upstream in
> [rogchap/v8go#234](https://github.com/rogchap/v8go/pull/234) and reintroduced
> here on the MinGW-w64 toolchain.

## V8 dependency

V8 version: **14.6.202.28** (March 2026)

In order to make `v8go` usable as a standard Go package, prebuilt static libraries of V8
are included for Linux (amd64 and arm64), macOS (amd64 and arm64) and Windows (amd64), so
you *should not* need to build V8 yourself. Each platform's library lives in its own Go
module under `deps/<os>_<arch>/`, split into `libv8-N.a` parts to stay under GitHub's
100 MiB file size limit.

Due to security concerns of binary blobs hiding malicious code, the V8 binary is built via CI *ONLY*.

## Project Goals

To provide a high quality, idiomatic, Go binding to the [V8 C++ API](https://v8.github.io/api/head/index.html).

The API should match the original API as closely as possible, but with an API that Gophers (Go enthusiasts) expect. For
example: using multiple return values to return both result and error from a function, rather than throwing an
exception.

This project also aims to keep up-to-date with the latest (stable) release of V8.

## Development

### Recompile V8 with debug info and debug checks

[Aside from data races, Go should be memory-safe](https://research.swtch.com/gorace) and v8go should preserve this property by adding the necessary checks to return an error or panic on these unsupported code paths. Release builds of v8go don't include debugging information for the V8 library since it significantly adds to the binary size, slows down compilation and shouldn't be needed by users of v8go. However, if a v8go bug causes a crash (e.g. during new feature development) then it can be helpful to build V8 with debugging information to get a C++ backtrace with line numbers. The following steps will not only do that, but also enable V8 debug checking, which can help with catching misuse of the V8 API.

1) Make sure to clone the projects submodules (ie. the V8's `depot_tools` project): `git submodule update --init --recursive`
1) Build the V8 binary for your OS: `deps/build.py --debug`. V8 is a large project, and building the binary can take up to 30 minutes.
1) Build the executable to debug, using `go build` for commands or `go test -c` for tests. You may need to add the `-ldflags=-compressdwarf=false` option to disable debug information compression so this information can be read by the debugger (e.g. lldb that comes with Xcode v12.5.1, the latest Xcode released at the time of writing)
1) Run the executable with a debugger (e.g. `lldb -- ./v8go.test -test.run TestThatIsCrashing`, `run` to start execution then use `bt` to print a bracktrace after it breaks on a crash), since backtraces printed by Go or V8 don't currently include line number information.

### Upgrading the V8 binaries

We have the [v8_upgrade](.github/workflows/v8_upgrade.yml) workflow.
The workflow is triggered every day or manually.

If the current [v8_hash](deps/v8_hash) is different from the latest stable version, the workflow takes care of fetching the latest stable v8 files and copying them into `deps/include`. The last step of the workflow opens a new PR with the branch name `v8_upgrade/<v8-version>` with all the changes.

The next steps are:

1) The build is not yet triggered automatically. To trigger it manually, go to the [V8
Build](https://github.com/hlvs-apps/v8go/actions?query=workflow%3A%22V8+Build%22) Github Action, Select "Run workflow",
and select your pushed branch eg. `v8_upgrade/<v8-version>`.
1) Once built, this opens a PR against your branch for each supported platform —
Linux (amd64 and arm64), macOS (amd64 and arm64) and Windows (amd64) — adding that
platform's static library under `deps/<os>_<arch>/`. GitHub's hard file size limit is
100 MiB, so each library is committed as a set of split archive parts (`libv8-0.a`,
`libv8-1.a`, …) listed in that directory's `libmanifest`; `deps/build.py` (see
`split_ar()`) produces them and cgo links the parts. Merge these PRs into your branch.

1) **Re-pin the `deps/*` modules.** Each `deps/<os>_<arch>` directory is its own Go
module, and the root `go.mod` `require`s all five at a pseudo-version. Bump those five
lines to a commit that contains **every** `deps/*/go.mod` together with the newly built
binaries — normally the commit that merged the last platform PR. Be careful here: the
`replace ... => ./deps/...` directives hide a wrong pin during local development,
because a `replace` only applies while `v8go` is the main module. A consumer running
`go get github.com/hlvs-apps/v8go@main` resolves the pseudo-versions for real and fails
with `invalid version: missing .../go.mod at revision ...` if the pinned commit predates
a platform. Verify from outside the repo before releasing:

    ```
    cd $(mktemp -d) && go mod init check
    go get github.com/hlvs-apps/v8go@main
    ```

You are now ready to raise the PR against `main` with the latest version of V8.

### Flushing after C/C++ standard library printing for debugging

When using the C/C++ standard library functions for printing (e.g. `printf`), then the output will be buffered by default.
This can cause some confusion, especially because the test binary (created through `go test`) does not flush the buffer
at exit (at the time of writing). When standard output is the terminal, then it will use line buffering and flush when
a new line is printed, otherwise (e.g. if the output is redirected to a pipe or file) it will be fully buffered and not even
flush at the end of a line. When the test binary is executed through `go test .` (e.g. instead of
separately compiled with `go test -c` and run with `./v8go.test`) Go may redirect standard output internally, resulting in
standard output being fully buffered.

A simple way to avoid this problem is to flush the standard output stream after printing with the `fflush(stdout);` statement.
Not relying on the flushing at exit can also help ensure the output is printed before a crash.

### Local leak checking

Leak checking is automatically done in CI, but it can be useful to do locally to debug leaks.

Leak checking is done using the [Leak Sanitizer](https://clang.llvm.org/docs/LeakSanitizer.html) which
is a part of LLVM. As such, compiling with clang as the C/C++ compiler seems to produce more complete
backtraces (unfortunately still only of the system stack at the time of writing).

For instance, on a Debian-based Linux system, you can use `sudo apt-get install clang-12` to install a
recent version of clang.  Then CC and CXX environment variables are needed to use that compiler. With
that compiler, the tests can be run as follows

```
CC=clang-12 CXX=clang++-12 go test -c --tags leakcheck && ./v8go.test
```

The separate compile and link commands are currently needed to get line numbers in the backtrace.

On macOS, leak checking isn't available with the version of clang that comes with Xcode, so a separate
compiler installation is needed.  For example, with homebrew, `brew install llvm` will install a version
of clang with support for this. The ASAN_OPTIONS environment variable will also be needed to run the code
with leak checking enabled, since it isn't enabled by default on macOS. E.g. with the homebrew
installation of llvm, the tests can be run with

```
CXX=/usr/local/opt/llvm/bin/clang++ CC=/usr/local/opt/llvm/bin/clang go test -c --tags leakcheck -ldflags=-compressdwarf=false
ASAN_OPTIONS=detect_leaks=1 ./v8go.test
```

The `-ldflags=-compressdwarf=false` is currently (with clang 13) needed to get line numbers in the backtrace.

### Formatting

Go has `go fmt`, C has `clang-format`. Any changes to the `v8go.h|cc` should be formated with `clang-format` with the
"Chromium" Coding style. This can be done easily by running the `go generate` command.

`brew install clang-format` to install on macOS.

---

V8 Gopher image based on original artwork from the amazing [Renee French](http://reneefrench.blogspot.com).

## Credits

`v8go` was created by [Roger Chapman](https://github.com/rogchap) and the v8go
contributors at [rogchap/v8go](https://github.com/rogchap/v8go), and carried
forward by [Sebastian Döll](https://github.com/katallaxie) at
[katallaxie/v8go](https://github.com/katallaxie/v8go). See [LICENSE](LICENSE)
for the terms this project is distributed under.
