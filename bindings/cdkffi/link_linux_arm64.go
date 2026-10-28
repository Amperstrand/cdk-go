//go:build linux && arm64 && !cdk_static

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_arm64 -lcdk_ffi -Wl,-rpath,${SRCDIR}/native/linux_arm64 -lm -ldl
import "C"
