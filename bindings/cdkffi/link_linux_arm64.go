//go:build linux && arm64

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_arm64 -lcdk_ffi_go -Wl,-rpath,${SRCDIR}/native/linux_arm64 -lm -ldl
import "C"
