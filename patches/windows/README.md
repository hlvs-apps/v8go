# Windows (MinGW) build patches

These patches make V8 build with the MinGW-w64 GCC/Clang toolchain, which is
what cgo uses to link on Windows. They are applied by `deps/build.py` only when
building with `--os windows` (see `apply_mingw_patches()`), on top of the V8
tree fetched by `gclient sync`.

## Provenance

The patches are taken verbatim from the MSYS2 `mingw-w64-v8` package, which
tracks V8 closely and is currently at **V8 14.6.202.26** — the same V8 minor as
this fork (`deps/v8_hash` → **14.6.202.28**), so they apply essentially as-is.

- Source: https://github.com/msys2/MINGW-packages/tree/master/mingw-w64-v8
- License: https://github.com/msys2/MINGW-packages/blob/master/LICENSE

The original numbering is preserved so that re-syncing from MSYS2 stays a simple
file-by-file diff. To refresh for a new V8, pull the updated patches from the
MSYS2 package and re-run a Windows CI build.

`018-bundled-zlib-mingw-cflags.patch` is **not** from MSYS2 — it's ours. MSYS2
replaces V8's bundled zlib with the system package; we keep the bundled zlib, so
its `BUILD.gn` needs `is_win` split into `is_msvc` (added by `001`) in two ways:
its MSVC `/wd` warning flags must only reach the real MSVC toolchain (MinGW GCC
reads `/wd4244` & co. as bogus input files), and its GCC SIMD flags
(`-mssse3`, `-msse4.2`, `-mpclmul`) must also reach MinGW, or the CRC32/adler32
intrinsics fail to inline ("target specific option mismatch").

## Where each patch is applied

`deps/build.py` applies these against specific submodule trees:

| Patch                                        | Applied in                     |
| -------------------------------------------- | ------------------------------ |
| `001-add-mingw-toolchain.patch`              | `deps/v8/build`                |
| `015-abseil-build-as-static-lib.patch`       | `deps/v8/third_party/abseil-cpp` |
| everything else (`002`–`014`, `017`)         | `deps/v8` (V8 source root)     |

`001` registers a MinGW GN toolchain (`//build/toolchain/win:mingw_$cpu`) and
defines `is_mingw`, which is switched on when the build runs with `CXX=g++`
(or `clang++`) and `target_os="win"`. The rest are source/build fixes that let
V8 compile under MinGW (macro clashes, `dllimport` attributes, wide-char APIs,
the x64 `push_registers` asm stub, Abseil pthread/bcrypt wiring, etc.).

## Patches intentionally NOT vendored

The MSYS2 package also ships patches that only make sense for *its* build, which
replaces V8's bundled third-party libraries with MSYS2 system packages. This
fork keeps the bundled libraries that `gclient` fetches, so these are skipped:

- `007-snapshot-use-system-zlib-header.patch` — swaps the bundled
  `third_party/zlib/zlib.h` include for the system `<zlib.h>`; we build bundled
  zlib.
- `016-zlib-use-system-lib.patch` — defines `USE_SYSTEM_ZLIB=1`; same reason.
- `icu.gn` — i18n is disabled (`v8_enable_i18n_support=false`), so ICU is not
  built.

If a future V8 bump makes the bundled-zlib include path diverge, revisit `007`.
