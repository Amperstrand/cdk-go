//go:build darwin && amd64

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/darwin_amd64 -lcdk_ffi_go -Wl,-rpath,${SRCDIR}/native/darwin_amd64 -lm
import "C"
