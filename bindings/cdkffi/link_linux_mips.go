//go:build linux && mips

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_mips -lcdk_ffi -Wl,-rpath,${SRCDIR}/native/linux_mips -lm -ldl
import "C"
