//go:build linux && mipsle

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/linux_mipsel -lcdk_ffi -Wl,-rpath,${SRCDIR}/native/linux_mipsel -lm -ldl
import "C"
