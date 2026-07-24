#!/usr/bin/env python3
import argparse
import glob
import os
import platform
import re
import shutil
import subprocess
import sys

# Abseil is vendored into V8's binary and, by default, lives in the
# `absl::lts_YYYYMMDD` (or `absl::head`) inline namespace. When a downstream Go
# binary links this static V8 *and* another library that also links Abseil, the
# two copies share those mangled symbol names and violate the ODR, which can
# manifest as crashes or subtle misbehaviour. Renaming V8's Abseil inline
# namespace to something unique keeps its symbols distinct. See
# patch_absl_inline_namespace() below.
ABSL_INLINE_NAMESPACE_NAME = "v8go"

valid_archs = ['arm64', 'amd64']
# "amd64" is called "x86_64" on everything but Windows.
current_arch = platform.uname()[4].lower().replace("x86_64", "amd64")
default_arch = current_arch if current_arch in valid_archs else None

parser = argparse.ArgumentParser()
parser.add_argument('--verbose', '-v', default=False, action='store_true')
parser.add_argument('--debug', default=False, action='store_true')
parser.add_argument('--ccache', default=False, action='store_true')
parser.add_argument('--clang', action='store_true')
parser.add_argument('--no-clang', dest='clang', action='store_false')
parser.set_defaults(clang=None)
# GitHub file size limits: warning at 50 MB, hard limit at 100 MB.
# Symbol indices can add 15% in the final .ar, so we need margin.
parser.add_argument('--max-file-size', default=int(40e6))
parser.add_argument('--arch',
    dest='arch',
    action='store',
    choices=valid_archs,
    default=default_arch,
    required=default_arch is None)
parser.add_argument(
    '--os',
    dest='os',
    choices=['android', 'ios', 'linux', 'darwin', 'windows'],
    default=platform.system().lower())
args = parser.parse_args()

deps_path = os.path.dirname(os.path.realpath(__file__))
v8_path = os.path.join(deps_path, "v8")
tools_path = os.path.join(deps_path, "depot_tools")
is_windows = platform.system().lower() == "windows"
# Whether we are *targeting* Windows. Keyed off the requested --os (not the host)
# so the MinGW patch/toolchain logic is explicit; on our CI this coincides with
# `is_windows` because we build the Windows library natively under MSYS2.
is_windows_build = args.os == "windows"
# Windows arm64 has no GCC option: mingw-w64 ships no aarch64 GCC target, so the
# only MinGW toolchain there is MSYS2's CLANGARM64 (clang). Everything else
# defaults as before -- MinGW x64 to GCC (MINGW64), non-Windows to Clang. An
# explicit `--clang`/`--no-clang` still wins.
is_windows_arm64 = is_windows_build and args.arch == "arm64"
is_clang = args.clang if args.clang is not None else (
    not is_windows_build or is_windows_arm64)

def get_custom_deps():
    # These deps are unnecessary for building.
    deps = {
        "v8/testing/gmock"                      : None,
        "v8/test/wasm-js"                       : None,
        "v8/third_party/colorama/src"           : None,
        "v8/tools/gyp"                          : None,
        "v8/tools/luci-go"                      : None,
    }
    if args.os != "android":
        deps["v8/third_party/catapult"] = None
        deps["v8/third_party/android_tools"] = None
    return deps

gclient_sln = [
    { "name"        : "v8",
        "url"         : "https://chromium.googlesource.com/v8/v8.git",
        "deps_file"   : "DEPS",
        "managed"     : False,
        "custom_deps" : get_custom_deps(),
        "custom_vars": {
            "build_for_node" : True,
        },
    },
]

gn_args = """
is_debug=%s
is_clang=%s
target_os="%s"
target_cpu="%s"
v8_target_cpu="%s"
symbol_level=%s
strip_debug_info=%s
is_component_build=false
v8_monolithic=true
v8_enable_webassembly=true
v8_enable_i18n_support=false
v8_enable_test_features=false
v8_use_external_startup_data=false
treat_warnings_as_errors=false
clang_use_chrome_plugins=false
use_custom_libcxx=false
use_clang_modules=false
use_libcxx_modules=false
use_allocator_shim=false
use_sysroot=false
use_glib=false
use_lto=false
use_thin_lto=false
v8_monolithic=true
v8_use_external_startup_data=false
treat_warnings_as_errors=false
v8_embedder_string="-v8go"
v8_enable_gdbjit=false
v8_enable_temporal_support=false
icu_use_data_file=false
v8_enable_test_features=false
exclude_unwind_tables=true
v8_android_log_stdout=true
v8_enable_temporal_support=false
v8_enable_embedder_custom_snapshot=false
v8_static_library=true
enable_crel=false
"""

# Extra GN args for the MinGW-w64 toolchain, mirroring the MSYS2 mingw-w64-v8
# package. Appended (for --os windows) on top of gn_args above. These disable
# features that assume MSVC/lld/rust or otherwise don't build under MinGW.
WINDOWS_GN_ARGS = """
use_lld=false
use_siso=false
enable_rust=false
enable_iterator_debugging=false
chrome_pgo_phase=0
v8_symbol_level=0
v8_enable_partition_alloc=false
v8_enable_verify_heap=false
v8_enable_etw_stack_walking=false
v8_enable_fuzztest=false
v8_enable_system_instrumentation=false
"""

def v8deps():
    # On Windows the spec is passed through `cmd /c` (see below); cmd truncates
    # arguments at embedded newlines, so join the two statements with `; `
    # (valid Python) instead of a newline there.
    sep = "; " if is_windows else "\n"
    spec = "solutions = %s%starget_os = [%r]" % (gclient_sln, sep, v8_os())
    env = os.environ.copy()
    env["PATH"] = tools_path + os.pathsep + env["PATH"]
    gclient_cmd = ["gclient", "sync", "--delete_unversioned_trees", "--no-history", "--spec", spec]
    if is_windows_build:
        # Skip DEPS hooks on Windows: they download the MSVC/clang toolchains and
        # gn/ninja, none of which we use (we build with MSYS2's MinGW toolchain).
        # The one build input a hook would otherwise generate, gclient_args.gni,
        # is written by apply_mingw_patches().
        gclient_cmd.append("--nohooks")
    if is_windows:
        # depot_tools ships `gclient` as a Bash script plus a `gclient.bat`
        # wrapper. We drive the build from native (MinGW) Python, whose
        # CreateProcess only resolves real executables (.exe) — it can't launch
        # the Bash script and won't append .bat. Routing through `cmd /c` lets
        # PATHEXT find gclient.bat and run the standard depot_tools Windows flow.
        gclient_cmd = ["cmd", "/c"] + gclient_cmd
    subprocess_check_call(gclient_cmd, cwd=deps_path, env=env)

def build_gn_args():
    is_debug = args.debug
    arch = v8_arch()
    # symbol_level = 1 includes line number information
    # symbol_level = 2 can be used for additional debug information, but it can increase the
    #   compiled library by an order of magnitude and further slow down compilation
    symbol_level = 1 if args.debug else 0
    strip_debug_info = not args.debug

    gnargs = gn_args % (
        str(bool(is_debug)).lower(),
        str(is_clang).lower(),
        v8_os(),
        arch,
        arch,
        symbol_level,
        str(strip_debug_info).lower(),
    )
    if args.ccache:
        gnargs += 'cc_wrapper="ccache"\n'
    if not is_clang and arch == "arm64":
        # https://chromium.googlesource.com/chromium/deps/icu/+/2958a507f15e475045906d73af39018d5038a93b
        # introduced -mmark-bti-property, which isn't supported by GCC.
        #
        # V8 itself fixed this in https://chromium-review.googlesource.com/c/v8/v8/+/3930160.
        gnargs += 'arm_control_flow_integrity="none"\n'
    if is_windows_build:
        gnargs += WINDOWS_GN_ARGS
        # Control Flow Guard is enabled for MinGW x64 but must stay off on
        # arm64, matching the MSYS2 mingw-w64-v8 package (its PKGBUILD sets
        # `_cfg=false` when CARCH is aarch64).
        gnargs += 'win_enable_cfg_guards=%s\n' % str(arch != "arm64").lower()

    return gnargs

def subprocess_check_call(cmdargs, *pargs, **kwargs):
    if args.verbose:
        print(sys.argv[0], ">", " ".join(cmdargs), file=sys.stderr)
    subprocess.check_call(cmd(cmdargs), *pargs, **kwargs)

def subprocess_check_output_text(cmdargs, *pargs, **kwargs):
    if args.verbose:
        print(sys.argv[0], ">", " ".join(cmdargs), file=sys.stderr)
    return subprocess.check_output(cmd(cmdargs), *pargs, **kwargs).decode('utf-8')

def cmd(args):
    # build.py runs under the MSYS2 shell on Windows, so every tool it shells out
    # to (gn, ninja, ar, git, patch, python) is a real executable already on
    # PATH — no legacy `cmd /c` wrapping needed.
    return args

def os_arch():
    return args.os + "_" + args.arch

def v8_os():
    # GN / gclient spell these "mac" and "win"; the Go GOOS names (used for the
    # deps/<os>_<arch> directory via os_arch()) are "darwin" and "windows".
    return args.os.replace('darwin', 'mac').replace('windows', 'win')

def v8_arch():
    if args.arch == "amd64":
        return "x64"
    return args.arch

# MinGW patches from patches/windows/, in the order the MSYS2 mingw-w64-v8
# package applies them. 001 patches deps/v8/build, 015 patches Abseil, the rest
# patch the V8 source root. See patches/windows/README.md for provenance and for
# why 007/016 (system zlib) are intentionally omitted.
WINDOWS_SOURCE_PATCHES = [
    "002-buildflags-fixes",
    "003-fix-macros-and-functions",
    "004-fix-static-assert-implementations",
    "005-fix-conflicting-macros",
    "006-support-clang-in-mingw-mode",
    "008-prioritized-native-thread-on-windows",
    "009-unicode-for-wide-char-functions",
    "010-disable-msvc-hack",
    "011-make-sure-that-__rdtsc-is-declared",
    "012-remove-dllimport-attributes",
    "013-builtin-deps-fixes",
    "014-heap-use-proper-sources",
    "017-highway-disable-avx10-on-mingw",
    # Not from MSYS2: they swap in system zlib, but we build V8's bundled zlib,
    # whose BUILD.gn feeds MSVC /wd flags to the compiler under is_win. Gate them
    # on is_msvc so MinGW GCC doesn't choke on them.
    "018-bundled-zlib-mingw-cflags",
]

def apply_mingw_patches():
    v8_build_path = os.path.join(v8_path, "build")
    abseil_path = os.path.join(v8_path, "third_party", "abseil-cpp")

    apply_windows_patch("001-add-mingw-toolchain", v8_build_path)
    apply_windows_patch("015-abseil-build-as-static-lib", abseil_path)
    for patch_name in WINDOWS_SOURCE_PATCHES:
        apply_windows_patch(patch_name, v8_path)
    update_last_change()

    # GN's BUILDCONFIG imports //build/config/gclient_args.gni, which is normally
    # produced by a gclient hook. We sync with --nohooks, so write it ourselves.
    gclient_args = os.path.join(v8_build_path, "config", "gclient_args.gni")
    with open(gclient_args, "wt") as f:
        f.write("build_with_chromium = false\n")

def apply_windows_patch(patch_name, working_dir):
    patch_path = os.path.join(
        os.path.dirname(deps_path), "patches", "windows", patch_name + ".patch")
    # `patch` (not `git apply`): several of these patches are `diff -ruN` output
    # with timestamped headers that git's stricter parser rejects.
    subprocess_check_call(["patch", "-p1", "-i", patch_path], cwd=working_dir)

def apply_build_patches():
    """Apply patches to files downloaded by gclient (v8/build/, etc.)."""
    patches_path = os.path.join(os.path.dirname(deps_path), "patches", "build")
    if not os.path.isdir(patches_path):
        return

    repo_root = os.path.dirname(deps_path)
    for patch_name in sorted(os.listdir(patches_path)):
        if not patch_name.endswith(".patch"):
            continue
        patch_path = os.path.join(patches_path, patch_name)
        # These patches use deps/v8/build paths, so apply from repo root
        subprocess_check_call(["patch", "-p1", "-i", patch_path], cwd=repo_root)

def patch_absl_inline_namespace():
    """Rename V8's bundled Abseil inline namespace to a unique name.

    Rewrites third_party/abseil-cpp/absl/base/options.h in place so that
    ABSL_OPTION_INLINE_NAMESPACE_NAME becomes ABSL_INLINE_NAMESPACE_NAME (with
    ABSL_OPTION_USE_INLINE_NAMESPACE forced on). This is done as a scripted
    rewrite rather than a patch under patches/build/ because a context diff is
    brittle across V8 (and therefore Abseil) rolls, whereas the two #define lines
    this targets have been stable for years. Runs after `gclient sync` fetches
    Abseil. Fails loudly if the file or the expected #defines are missing so a
    silent, colliding build never ships.
    """
    options_path = os.path.join(
        v8_path, "third_party", "abseil-cpp", "absl", "base", "options.h")
    if not os.path.exists(options_path):
        sys.exit("Abseil options.h not found at %s; cannot rename its inline "
                 "namespace (has V8's Abseil layout changed?)" % options_path)

    with open(options_path, "rt") as f:
        content = f.read()

    content, n_name = re.subn(
        r"(#define\s+ABSL_OPTION_INLINE_NAMESPACE_NAME\s+)\w+",
        r"\g<1>" + ABSL_INLINE_NAMESPACE_NAME,
        content)
    content, n_use = re.subn(
        r"(#define\s+ABSL_OPTION_USE_INLINE_NAMESPACE\s+)\d+",
        r"\g<1>1",
        content)

    if n_name == 0 or n_use == 0:
        sys.exit("Failed to rename Abseil inline namespace in %s "
                 "(name matches=%d, use matches=%d)" %
                 (options_path, n_name, n_use))

    with open(options_path, "wt") as f:
        f.write(content)
    print("%s: set ABSL inline namespace to '%s'" %
          (sys.argv[0], ABSL_INLINE_NAMESPACE_NAME), file=sys.stderr)


def update_last_change():
    out_path = os.path.join(v8_path, "build", "util", "LASTCHANGE")
    subprocess_check_call(["python", "build/util/lastchange.py", "-o", out_path], cwd=v8_path)


# File names that hold a license/copyright notice. V8 and its gclient-fetched
# dependencies are not consistent about which they use: LICENSE, LICENCE,
# COPYING, NOTICE, LICENSE.txt, LICENSE-APACHE, LICENSE.fdlibm, ...
LICENSE_FILE_RE = re.compile(
    r"^(license|licence|copying|copyright|notice)([._-].*)?$", re.IGNORECASE)

# ...but the pattern above also matches source files such as `license.cc` or
# `licenses.py` (V8 and Chromium both have them), which are code, not notices.
# Reject anything carrying a known code/data extension.
NON_LICENSE_EXTS = frozenset([
    ".a", ".bat", ".bzl", ".c", ".cc", ".cfg", ".cmake", ".conf", ".cpp", ".cs",
    ".css", ".dll", ".exe", ".go", ".gn", ".gni", ".gyp", ".gypi", ".h", ".hpp",
    ".html", ".in", ".ini", ".java", ".js", ".json", ".m", ".mm", ".o", ".pl",
    ".ps1", ".py", ".pyc", ".rs", ".s", ".sh", ".so", ".toml", ".ts", ".xml",
    ".yaml", ".yml",
])

# Pruned while walking: version control, build output, and this script's own
# staging dirs. None of them hold a license text that isn't also present at its
# source location.
NOTICE_PRUNE_DIRS = frozenset([".git", ".build", "out", "obj"])

THIRD_PARTY_NOTICES_HEADER = """\
THIRD-PARTY NOTICES for the prebuilt V8 library in this directory
================================================================

The static library in this directory (libv8-*.a) was compiled from V8 at commit

    %s

It contains code from V8 and from the third-party libraries that V8 bundles.
Reproduced below are the license texts and copyright notices found in that exact
source tree, which the terms of those licenses require to accompany a binary
redistribution.

This listing is generated mechanically by deps/build.py at build time (see
write_third_party_notices()) by harvesting every license file from the synced V8
checkout. It is deliberately a SUPERSET: no attempt is made to determine which
components the linker actually pulled in, because over-attribution is harmless
whereas under-attribution is not. Some entries below therefore cover build
tooling or test-only dependencies that are not part of the library.

v8go's own license is BSD-3-Clause and is not reproduced here; see the LICENSE
file at the root of the v8go repository, alongside THIRD_PARTY_LICENSES.md for a
human-readable summary of the principal components.

%d license files follow, sorted by path relative to the V8 checkout root.
"""


def collect_license_files(root):
    """Returns a sorted [(relpath, text)] of every license file under root."""
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        # Prune in place so os.walk doesn't descend into them.
        dirnames[:] = sorted(d for d in dirnames if d not in NOTICE_PRUNE_DIRS)
        for filename in sorted(filenames):
            if not LICENSE_FILE_RE.match(filename):
                continue
            if os.path.splitext(filename)[1].lower() in NON_LICENSE_EXTS:
                continue
            full_path = os.path.join(dirpath, filename)
            if os.path.islink(full_path) or not os.path.isfile(full_path):
                continue
            # Guard against a stray large file matching the name pattern; real
            # license texts are a few KiB at most.
            if os.path.getsize(full_path) > 512 * 1024:
                continue
            try:
                with open(full_path, "rt", encoding="utf-8", errors="replace") as f:
                    text = f.read()
            except OSError as e:
                print("%s: skipping %s: %s" % (sys.argv[0], full_path, e),
                      file=sys.stderr)
                continue
            rel = os.path.relpath(full_path, root).replace(os.sep, "/")
            found.append((rel, text))
    found.sort(key=lambda item: item[0])
    return found


def write_third_party_notices(dest_path):
    """Writes dest_path/THIRD_PARTY_NOTICES harvested from the V8 checkout.

    Attribution has to be regenerated per build rather than committed by hand:
    the CI job that lands new binaries deletes deps/<os>_<arch>/ wholesale before
    unpacking the build artifact (see "Remove old static libraries" in
    .github/workflows/v8_build.yml), so anything not produced by the build is
    lost. Generating it here also keeps the notices pinned to the same V8 tree
    that produced the archive, and each deps/<os>_<arch>/ is its own Go module,
    so the file travels with the module when consumed standalone.
    """
    with open(os.path.join(deps_path, "v8_hash"), "rt") as f:
        v8_hash = f.read().strip()

    licenses = collect_license_files(v8_path)
    if not licenses:
        raise AssertionError(
            "no license files found under %s; refusing to ship binaries "
            "without third-party attribution" % v8_path)

    out_fn = os.path.join(dest_path, "THIRD_PARTY_NOTICES")
    with open(out_fn, "wt", encoding="utf-8", newline="\n") as f:
        f.write(THIRD_PARTY_NOTICES_HEADER % (v8_hash, len(licenses)))
        for rel, text in licenses:
            f.write("\n\n")
            f.write("=" * 80 + "\n")
            f.write("%s\n" % rel)
            f.write("=" * 80 + "\n\n")
            f.write(text if text.endswith("\n") else text + "\n")

    print("%s: wrote %s (%d license files)" % (sys.argv[0], out_fn, len(licenses)),
          file=sys.stderr)


def split_ar(src_fn, dest_fn, dest_obj_dn):
    """Extracts all files from src_fn to dest_obj_dn/ and makes a thin archive at dest_fn.

    GitHub's file size limit is 100 MiB, and the archive is hitting that.
    """
    dest_path = os.path.dirname(dest_fn)

    ar_path = os.path.abspath(os.path.join(v8_path, "third_party/llvm-build/Release+Asserts/bin/llvm-ar"))
    if args.os == "linux" and args.arch == "arm64" and not is_clang:
        ar_path = "aarch64-linux-gnu-ar"
    elif not os.access(ar_path, os.X_OK) or not is_clang:
        ar_path = "ar"

    if os.path.exists(dest_obj_dn):
        shutil.rmtree(dest_obj_dn)
    os.makedirs(dest_obj_dn)

    # Directories may have been flattened, causing duplicate file
    # names. ar(1) simply overwrites earlier files, causing
    # headache-inducing "undefined symbol" errors.
    ar_files = subprocess_check_output_text(
        [
            ar_path,
            "t",
            src_fn,
        ],
        cwd=v8_path)
    ar_files = ar_files.splitlines()

    # mingw's binutils "ar" matches member names case-insensitively (like
    # Darwin's llvm-ar), but unlike llvm-ar its "ar t" prints the original case.
    # Lowercase the listing ourselves so the occurrence indices we feed to
    # "ar xN" match how extraction actually resolves members that differ only in
    # case (e.g. the inspector's Runtime.o vs V8's runtime.o): as two occurrences
    # of "runtime.o", extracted with N=1 and N=2. Without this, both are counted
    # separately, each extracted with N=1 -> "ar" returns the same first match
    # twice and the second member's symbols end up undefined at link time.
    if args.os == "windows":
        ar_files = [ar_file.lower() for ar_file in ar_files]

    # llvm-ar (--clang) on Darwin lowercases names on extraction, and both Darwin
    # and Windows have case-insensitive filesystems and case-insensitive "ar"
    # matching, so the lowercased canonical name is what lands on disk.
    case_sensitive = args.os not in ("darwin", "windows")

    # Extracting files one-by-one is slow, so let's group them into
    # disjoint sets and use "ar N"... Complicated by the occasional
    # case mangling.
    ar_file_groups = allocate_disjoint_files(ar_files, case_sensitive)

    j = 0
    for i, ar_files_group in ar_file_groups:
        subprocess_check_call(
            [
                ar_path,
                "xN",
                "--output", dest_obj_dn,
                str(1 + i),
                src_fn,
            ] + ar_files_group,
            cwd=v8_path)
        for ar_file in ar_files_group:
            ar_file_canon = ar_file if case_sensitive else ar_file.lower()
            os.rename(os.path.join(dest_obj_dn, ar_file_canon), os.path.join(dest_obj_dn, "{}.{}".format(1 + j, ar_file)))
            j += 1

    file_groups = [] # [(file, size)]
    size = 0
    for fn in sorted(glob.glob(os.path.join(dest_obj_dn, "*"))):
        fsize = os.stat(fn).st_size
        if not file_groups or size + fsize >= args.max_file_size:
            file_groups.append([])
            size = 0
        file_groups[-1].append(os.path.relpath(fn, dest_path))
        size += fsize

    dest_stem, dest_ext = os.path.splitext(dest_fn)
    for fn in glob.glob(os.path.join(dest_path, "lib*.a")):
        os.unlink(fn)

    dest_fns = []
    for i, files in enumerate(file_groups):
        if len(file_groups) == 1:
            dest_fn = "{}{}".format(dest_stem, dest_ext)
        else:
            dest_fn = "{}-{}{}".format(dest_stem, i, dest_ext)

        dest_fns.append(os.path.relpath(dest_fn, dest_path))
        subprocess_check_call(
            [
                ar_path,
                "qsc",
                os.path.relpath(dest_fn, dest_path),
            ] + files,
            cwd=dest_path)

    with open(os.path.join(dest_path, "libmanifest"), "wt") as f:
        for dest_fn in dest_fns:
            print(dest_fn, file=f)

def allocate_disjoint_files(ar_files, case_sensitive=True):
    ar_file_counts = {} # file -> count
    for ar_file in ar_files:
        ar_file_counts[ar_file] = ar_file_counts.get(ar_file, 0) + 1
    ar_file_counts = list(ar_file_counts.items())
    ar_file_counts.sort(key=lambda item: -item[1])

    ar_file_groups = [] # [(index, files)]
    while ar_file_counts:
        canon_file_set = {} # canon file -> (file, count)
        file_set = set()
        max_count = 0
        for ar_file, count in ar_file_counts:
            ar_file_canon = ar_file if case_sensitive else ar_file.lower()
            if ar_file_canon in canon_file_set: continue
            canon_file_set[ar_file_canon] = (ar_file, count)
            file_set.add(ar_file)
            max_count = max(max_count, count)

        ar_file_counts = [(ar_file, count) for ar_file, count in ar_file_counts if ar_file not in file_set]
        groups = [(i, []) for i in range(max_count)]
        for ar_file, count in canon_file_set.values():
            for i in range(count):
                groups[i][1].append(ar_file)
        ar_file_groups.extend(groups)

    return ar_file_groups

def main():
    if is_windows_build:
        # Build V8 with the MinGW-w64 toolchain (see patches/windows/). Pointing
        # CXX at g++/clang++ is what flips GN's `is_mingw` on (added by patch
        # 001), and DEPOT_TOOLS_WIN_TOOLCHAIN=0 stops gclient from trying to
        # fetch the MSVC toolchain.
        os.environ.setdefault("CC", "clang" if is_clang else "gcc")
        os.environ.setdefault("CXX", "clang++" if is_clang else "g++")
        os.environ.setdefault("DEPOT_TOOLS_WIN_TOOLCHAIN", "0")

    v8deps()
    apply_build_patches()
    patch_absl_inline_namespace()
    if is_windows_build:
        apply_mingw_patches()

    if is_windows_build:
        # Use the MSYS2-provided gn/ninja; the depot_tools copies assume MSVC.
        gn_path = "gn"
        ninja_path = "ninja"
    else:
        gn_path = os.path.join(tools_path, "gn")
        assert(os.path.exists(gn_path))
        ninja_path = os.path.join(tools_path, "ninja" + (".exe" if is_windows else ""))
        assert(os.path.exists(ninja_path))

    build_path = os.path.join(deps_path, ".build", os_arch())

    gnargs = build_gn_args()

    # gn gen occasionally aborts with an internal assertion
    # (ninja_target_writer.cc "Check failed: files.empty()") that a fresh retry
    # clears. Regenerate from a clean build dir a couple of times before failing.
    for attempt in range(3):
        if os.path.exists(build_path):
            shutil.rmtree(build_path)
        try:
            subprocess_check_call([gn_path, "gen", build_path, "--args=" + gnargs.replace('\n', ' ')], cwd=v8_path)
            break
        except subprocess.CalledProcessError:
            if attempt == 2:
                raise
            print("%s: gn gen failed (attempt %d/3), retrying..." % (sys.argv[0], attempt + 1), file=sys.stderr)

    subprocess_check_call([ninja_path, "-j", str(os.cpu_count()), "-C", build_path, "v8_monolith"], cwd=v8_path)

    dest_path = os.path.join(deps_path, os_arch())
    dest_obj_dn = os.path.join(dest_path, "obj")
    try:
        split_ar(
            os.path.join(build_path, "obj/libv8_monolith.a"),
            os.path.join(dest_path, "libv8.a"),
            dest_obj_dn)
    finally:
        if os.path.exists(dest_obj_dn):
            shutil.rmtree(dest_obj_dn)

    write_third_party_notices(dest_path)

if __name__ == "__main__":
    main()
