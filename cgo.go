// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

//go:generate clang-format -i --verbose -style=Chromium v8go.h v8go.cc

// On darwin, libv8's partition_alloc calls CoreFoundation directly. The
// deps/darwin_* modules only link Security, which no longer brings
// CoreFoundation in on current toolchains (./test failed to link under Go
// 1.27), so it is linked here, where consumers pick it up without a deps bump.

// #cgo CXXFLAGS: -fno-rtti -fPIC -std=c++20 -I${SRCDIR}/deps/include -Wall
// #cgo CXXFLAGS: -DV8_COMPRESS_POINTERS -DV8_31BIT_SMIS_ON_64BIT_ARCH -DV8_ENABLE_SANDBOX
// #cgo CXXFLAGS: -DV8_DEPRECATION_WARNINGS -DV8_IMMINENT_DEPRECATION_WARNINGS
// #cgo darwin LDFLAGS: -framework CoreFoundation
import "C"
