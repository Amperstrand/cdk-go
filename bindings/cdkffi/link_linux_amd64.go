//go:build linux && amd64

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_amd64 -lcdk_ffi_go -Wl,-rpath,${SRCDIR}/native/linux_amd64 -lm -ldl
import "C"
