//go:build windows && amd64

package cdk_ffi

// #cgo LDFLAGS: -L${SRCDIR}/native/windows_amd64 -lcdk_ffi_go
import "C"
