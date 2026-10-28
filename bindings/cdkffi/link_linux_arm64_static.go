//go:build linux && arm64 && cdk_static

// AMPESTRAND experimental/arch: static musl linking against the Rust
// staticlib built by Amperstrand/cdk scripts/arch-build.sh.
// Build with: CGO_ENABLED=1 CC=<zig musl aarch64 cc> GOOS=linux GOARCH=arm64 go build -tags cdk_static
package cdk_ffi

/*
#cgo LDFLAGS: -L${SRCDIR}/native/static/linux_arm64 -l:libcdk_ffi.a -lm -ldl -lpthread -lunwind
*/
import "C"
