//go:build linux && amd64 && !cdk_static

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_amd64 -lcdk_ffi -Wl,-rpath,${SRCDIR}/native/linux_amd64 -lm -ldl
import "C"
