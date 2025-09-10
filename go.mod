module github.com/katallaxie/v8go

go 1.24.0

tool (
	github.com/golangci/golangci-lint/v2/cmd/golangci-lint
	github.com/vektra/mockery/v2
	gotest.tools/gotestsum
	mvdan.cc/gofumpt
)
require (
	github.com/katallaxie/v8go/deps/android_amd64 v0.0.0-20250515043113-5dcc98077472
	github.com/katallaxie/v8go/deps/android_arm64 v0.0.0-20250515043113-5dcc98077472
	github.com/katallaxie/v8go/deps/darwin_amd64 v0.0.0-20250515043113-5dcc98077472
	github.com/katallaxie/v8go/deps/darwin_arm64 v0.0.0-20250515043113-5dcc98077472
	github.com/katallaxie/v8go/deps/linux_amd64 v0.0.0-20250515043113-5dcc98077472
	github.com/katallaxie/v8go/deps/linux_arm64 v0.0.0-20250515043113-5dcc98077472
)