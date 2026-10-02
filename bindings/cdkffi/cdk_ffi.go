package cdk_ffi

// #include <cdk_ffi.h>
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"
)

// This is needed, because as of go 1.24
// type RustBuffer C.RustBuffer cannot have methods,
// RustBuffer is treated as non-local type
type GoRustBuffer struct {
	inner C.RustBuffer
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

// C.RustBuffer fields exposed as an interface so they can be accessed in different Go packages.
// See https://github.com/golang/go/issues/13467
type ExternalCRustBuffer interface {
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

func RustBufferFromC(b C.RustBuffer) ExternalCRustBuffer {
	return GoRustBuffer{
		inner: b,
	}
}

func CFromRustBuffer(b ExternalCRustBuffer) C.RustBuffer {
	return C.RustBuffer{
		capacity: C.uint64_t(b.Capacity()),
		len:      C.uint64_t(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func RustBufferFromExternal(b ExternalCRustBuffer) GoRustBuffer {
	return GoRustBuffer{
		inner: C.RustBuffer{
			capacity: C.uint64_t(b.Capacity()),
			len:      C.uint64_t(b.Len()),
			data:     (*C.uchar)(b.Data()),
		},
	}
}

func (cb GoRustBuffer) Capacity() uint64 {
	return uint64(cb.inner.capacity)
}

func (cb GoRustBuffer) Len() uint64 {
	return uint64(cb.inner.len)
}

func (cb GoRustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.inner.data)
}

func (cb GoRustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.inner.data), C.uint64_t(cb.inner.len))
	return bytes.NewReader(b)
}

func (cb GoRustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_cdk_ffi_rustbuffer_free(cb.inner, status)
		return false
	})
}

func (cb GoRustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.inner.data), C.int(cb.inner.len))
}

func stringToRustBuffer(str string) C.RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) C.RustBuffer {
	if len(b) == 0 {
		return C.RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) C.RustBuffer {
		return C.ffi_cdk_ffi_rustbuffer_from_bytes(foreign, status)
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) C.RustBuffer
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) C.RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[E any, U any](converter BufReader[E], callback func(*C.RustCallStatus) U) (U, E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[E], status C.RustCallStatus) E {
	switch status.code {
	case 0:
		var zero E
		return zero
	case 1:
		return LiftFromRustBuffer(converter, GoRustBuffer{inner: status.errorBuf})
	case 2:
		// when the rust code sees a panic, it tries to construct a rustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{inner: status.errorBuf})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		panic(fmt.Errorf("unknown status code: %d", status.code))
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a C.RustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{
				inner: status.errorBuf,
			})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError[error](nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

type NativeError interface {
	AsError() error
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	FfiConverterNostrInboxListenerINSTANCE.register()
	FfiConverterWalletDatabaseINSTANCE.register()
	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 30
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_cdk_ffi_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("cdk_ffi: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_create_bip321_uri()
		})
		if checksum != 2479 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_create_bip321_uri: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_create_wallet_db()
		})
		if checksum != 38981 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_create_wallet_db: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_custom_wallet_store()
		})
		if checksum != 34973 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_custom_wallet_store: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_auth_proof()
		})
		if checksum != 22357 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_auth_proof: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_conditions()
		})
		if checksum != 18453 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_conditions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_contact_info()
		})
		if checksum != 40231 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_contact_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_create_request_params()
		})
		if checksum != 8102 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_create_request_params: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_invoice()
		})
		if checksum != 20311 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_invoice: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_key_set()
		})
		if checksum != 64139 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_key_set: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_key_set_info()
		})
		if checksum != 26774 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_key_set_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_keys()
		})
		if checksum != 38114 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_melt_prepare_options()
		})
		if checksum != 21523 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_melt_prepare_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_melt_quote()
		})
		if checksum != 31843 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_mint_info()
		})
		if checksum != 4255 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_mint_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_mint_quote()
		})
		if checksum != 12595 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_mint_version()
		})
		if checksum != 54734 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_mint_version: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_nuts()
		})
		if checksum != 23702 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_nuts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_payment_request()
		})
		if checksum != 36715 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_payment_request: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_proof_info()
		})
		if checksum != 19899 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_proof_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_proof_state_update()
		})
		if checksum != 25192 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_proof_state_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_receive_options()
		})
		if checksum != 46457 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_receive_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_send_memo()
		})
		if checksum != 6016 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_send_memo: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_send_options()
		})
		if checksum != 43827 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_send_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_subscribe_params()
		})
		if checksum != 6793 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_subscribe_params: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_decode_transaction()
		})
		if checksum != 48687 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_decode_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_auth_proof()
		})
		if checksum != 15755 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_auth_proof: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_conditions()
		})
		if checksum != 48516 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_conditions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_contact_info()
		})
		if checksum != 44629 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_contact_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_create_request_params()
		})
		if checksum != 21001 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_create_request_params: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_key_set()
		})
		if checksum != 10879 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_key_set: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_key_set_info()
		})
		if checksum != 18895 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_key_set_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_keys()
		})
		if checksum != 20045 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_melt_prepare_options()
		})
		if checksum != 37237 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_melt_prepare_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_melt_quote()
		})
		if checksum != 25080 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_mint_info()
		})
		if checksum != 31825 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_mint_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_mint_quote()
		})
		if checksum != 52375 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_mint_version()
		})
		if checksum != 3369 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_mint_version: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_nuts()
		})
		if checksum != 30942 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_nuts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_proof_info()
		})
		if checksum != 32664 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_proof_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_proof_state_update()
		})
		if checksum != 62126 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_proof_state_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_receive_options()
		})
		if checksum != 34534 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_receive_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_send_memo()
		})
		if checksum != 10559 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_send_memo: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_send_options()
		})
		if checksum != 12512 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_send_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_subscribe_params()
		})
		if checksum != 58897 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_subscribe_params: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_encode_transaction()
		})
		if checksum != 38295 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_encode_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_generate_mnemonic()
		})
		if checksum != 17512 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_generate_mnemonic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_init_default_logging()
		})
		if checksum != 4192 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_init_default_logging: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_init_logging()
		})
		if checksum != 13465 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_init_logging: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_mint_quote_amount_mintable()
		})
		if checksum != 6913 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_mint_quote_amount_mintable: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_mint_quote_is_expired()
		})
		if checksum != 6685 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_mint_quote_is_expired: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_mint_quote_total_amount()
		})
		if checksum != 34269 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_mint_quote_total_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_mnemonic_to_entropy()
		})
		if checksum != 58572 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_mnemonic_to_entropy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nip44_decrypt()
		})
		if checksum != 39962 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nip44_decrypt: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nip44_encrypt()
		})
		if checksum != 485 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nip44_encrypt: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nostr_generate_secret_key()
		})
		if checksum != 13275 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nostr_generate_secret_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nostr_get_pubkey()
		})
		if checksum != 3969 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nostr_get_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_npubcash_derive_secret_key_from_seed()
		})
		if checksum != 6473 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_npubcash_derive_secret_key_from_seed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_npubcash_get_pubkey()
		})
		if checksum != 28438 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_npubcash_get_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_npubcash_quote_to_mint_quote()
		})
		if checksum != 58675 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_npubcash_quote_to_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nwc_derive_service_secret_key_from_seed()
		})
		if checksum != 58335 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nwc_derive_service_secret_key_from_seed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_nwc_get_pubkey()
		})
		if checksum != 3708 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_nwc_get_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_parse_bip321_payment_instruction()
		})
		if checksum != 49418 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_parse_bip321_payment_instruction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_has_dleq()
		})
		if checksum != 56072 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_has_dleq: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_is_active()
		})
		if checksum != 26064 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_is_active: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_sign_p2pk()
		})
		if checksum != 61649 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_sign_p2pk: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_verify_dleq()
		})
		if checksum != 1267 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_verify_dleq: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_verify_htlc()
		})
		if checksum != 24106 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_verify_htlc: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proof_y()
		})
		if checksum != 55958 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proof_y: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_proofs_total_amount()
		})
		if checksum != 58202 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_proofs_total_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_resolve_bip353_payment_instruction()
		})
		if checksum != 39566 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_resolve_bip353_payment_instruction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_sqlite_wallet_store()
		})
		if checksum != 59270 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_sqlite_wallet_store: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_func_transaction_matches_conditions()
		})
		if checksum != 45503 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_func_transaction_matches_conditions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_activesubscription_id()
		})
		if checksum != 53295 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_activesubscription_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_activesubscription_recv()
		})
		if checksum != 1020 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_activesubscription_recv: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_activesubscription_try_recv()
		})
		if checksum != 39356 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_activesubscription_try_recv: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrinbox_pubkey()
		})
		if checksum != 9918 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrinbox_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrinbox_start()
		})
		if checksum != 36353 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrinbox_start: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrinbox_stop()
		})
		if checksum != 16312 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrinbox_stop: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrinboxlistener_on_event()
		})
		if checksum != 50983 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrinboxlistener_on_event: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mint_preferred()
		})
		if checksum != 11066 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mint_preferred: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mints()
		})
		if checksum != 4177 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrwaitinfo_pubkey()
		})
		if checksum != 8372 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrwaitinfo_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nostrwaitinfo_relays()
		})
		if checksum != 40910 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nostrwaitinfo_relays: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_npubcashclient_get_missing_quotes()
		})
		if checksum != 31413 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_npubcashclient_get_missing_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_npubcashclient_get_quotes()
		})
		if checksum != 64169 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_npubcashclient_get_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_npubcashclient_get_user_info()
		})
		if checksum != 31487 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_npubcashclient_get_user_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_npubcashclient_set_mint_url()
		})
		if checksum != 8738 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_npubcashclient_set_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_npubcashclient_set_quote_locking()
		})
		if checksum != 61660 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_npubcashclient_set_quote_locking: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_client_pubkey()
		})
		if checksum != 1577 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_client_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_connection_uri()
		})
		if checksum != 23241 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_connection_uri: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_is_running()
		})
		if checksum != 55514 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_is_running: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_service_pubkey()
		})
		if checksum != 21181 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_service_pubkey: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_start()
		})
		if checksum != 29349 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_start: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_nwcservice_stop()
		})
		if checksum != 4827 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_nwcservice_stop: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_amount()
		})
		if checksum != 17196 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_description()
		})
		if checksum != 30652 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_description: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_mint_preferred()
		})
		if checksum != 18301 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_mint_preferred: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_mints()
		})
		if checksum != 17730 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_payment_id()
		})
		if checksum != 12834 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_payment_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_single_use()
		})
		if checksum != 17480 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_single_use: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_supported_methods()
		})
		if checksum != 2914 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_supported_methods: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_to_bech32_string()
		})
		if checksum != 10557 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_to_bech32_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_to_bip321()
		})
		if checksum != 42766 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_to_bip321: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_to_string_encoded()
		})
		if checksum != 63792 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_to_string_encoded: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_transports()
		})
		if checksum != 60834 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_transports: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequest_unit()
		})
		if checksum != 31184 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequest_unit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequestpayload_id()
		})
		if checksum != 27515 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequestpayload_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequestpayload_memo()
		})
		if checksum != 56685 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequestpayload_memo: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequestpayload_mint()
		})
		if checksum != 42962 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequestpayload_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequestpayload_proofs()
		})
		if checksum != 56354 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequestpayload_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_paymentrequestpayload_unit()
		})
		if checksum != 9118 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_paymentrequestpayload_unit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_pendingmelt_operation_id()
		})
		if checksum != 42211 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_pendingmelt_operation_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_pendingmelt_quote_id()
		})
		if checksum != 57764 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_pendingmelt_quote_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_pendingmelt_wait()
		})
		if checksum != 38621 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_pendingmelt_wait: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_amount()
		})
		if checksum != 25790 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_cancel()
		})
		if checksum != 14185 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_change_amount_without_swap()
		})
		if checksum != 59536 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_change_amount_without_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_confirm()
		})
		if checksum != 44853 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_confirm: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_confirm_prefer_async()
		})
		if checksum != 50879 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_confirm_prefer_async: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_confirm_with_options()
		})
		if checksum != 32808 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_confirm_with_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_fee_reserve()
		})
		if checksum != 24820 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_fee_reserve: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_fee_savings_without_swap()
		})
		if checksum != 13657 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_fee_savings_without_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_input_fee()
		})
		if checksum != 22331 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_input_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_input_fee_without_swap()
		})
		if checksum != 59303 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_input_fee_without_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_operation_id()
		})
		if checksum != 52002 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_operation_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_proofs()
		})
		if checksum != 22010 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_quote_id()
		})
		if checksum != 54442 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_quote_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_requires_swap()
		})
		if checksum != 26720 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_requires_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_swap_fee()
		})
		if checksum != 15287 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_swap_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_total_fee()
		})
		if checksum != 37542 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_total_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedmelt_total_fee_with_swap()
		})
		if checksum != 44787 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedmelt_total_fee_with_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_cancel()
		})
		if checksum != 42398 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_confirm()
		})
		if checksum != 39918 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_confirm: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_input_fee()
		})
		if checksum != 29609 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_input_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_method()
		})
		if checksum != 560 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_method: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_method_fee()
		})
		if checksum != 56458 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_method_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_mint_url()
		})
		if checksum != 40661 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_operation_id()
		})
		if checksum != 27901 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_operation_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_payment_amount()
		})
		if checksum != 26016 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_payment_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_requested_amount()
		})
		if checksum != 46200 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_requested_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_total_amount()
		})
		if checksum != 63155 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_total_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_unit()
		})
		if checksum != 44174 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedpaymentrequest_unit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_amount()
		})
		if checksum != 62180 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_amount: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_cancel()
		})
		if checksum != 48000 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_confirm()
		})
		if checksum != 5962 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_confirm: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_fee()
		})
		if checksum != 37119 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_operation_id()
		})
		if checksum != 33181 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_operation_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_preparedsend_proofs()
		})
		if checksum != 87 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_preparedsend_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_encode()
		})
		if checksum != 53245 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_encode: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_htlc_hashes()
		})
		if checksum != 14335 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_htlc_hashes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_locktimes()
		})
		if checksum != 44524 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_locktimes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_memo()
		})
		if checksum != 28883 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_memo: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_mint_url()
		})
		if checksum != 16820 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_p2pk_pubkeys()
		})
		if checksum != 56348 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_p2pk_pubkeys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_p2pk_refund_pubkeys()
		})
		if checksum != 16072 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_p2pk_refund_pubkeys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_proofs()
		})
		if checksum != 60002 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_proofs_simple()
		})
		if checksum != 23555 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_proofs_simple: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_spending_conditions()
		})
		if checksum != 55293 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_spending_conditions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_to_raw_bytes()
		})
		if checksum != 25396 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_to_raw_bytes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_unit()
		})
		if checksum != 55723 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_unit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_ur_encoder()
		})
		if checksum != 54523 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_ur_encoder: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_token_value()
		})
		if checksum != 22223 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_token_value: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurdecoder_complete()
		})
		if checksum != 1656 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurdecoder_complete: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurdecoder_fragment_count()
		})
		if checksum != 46834 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurdecoder_fragment_count: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurdecoder_receive()
		})
		if checksum != 26404 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurdecoder_receive: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurdecoder_resolved_fragment_count()
		})
		if checksum != 50324 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurdecoder_resolved_fragment_count: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurdecoder_token()
		})
		if checksum != 34297 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurdecoder_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurencoder_current_index()
		})
		if checksum != 47175 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurencoder_current_index: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurencoder_fragment_count()
		})
		if checksum != 40473 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurencoder_fragment_count: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurencoder_is_single_fragment()
		})
		if checksum != 17184 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurencoder_is_single_fragment: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_tokenurencoder_next_part()
		})
		if checksum != 49133 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_tokenurencoder_next_part: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_active_keyset()
		})
		if checksum != 21888 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_active_keyset: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_calculate_fee()
		})
		if checksum != 1751 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_calculate_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_all_pending_proofs()
		})
		if checksum != 29982 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_all_pending_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_melt_quote_status()
		})
		if checksum != 42910 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_melt_quote_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_mint_quote()
		})
		if checksum != 13747 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_mint_quote_status()
		})
		if checksum != 22697 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_mint_quote_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_proofs_spent()
		})
		if checksum != 31942 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_proofs_spent: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_check_send_status()
		})
		if checksum != 48245 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_check_send_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_claim_npubcash_quotes()
		})
		if checksum != 50606 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_claim_npubcash_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_cross_mint_transfer_quote_max()
		})
		if checksum != 41141 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_cross_mint_transfer_quote_max: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_enable_npubcash()
		})
		if checksum != 16091 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_enable_npubcash: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_fetch_mint_info()
		})
		if checksum != 41951 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_fetch_mint_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_fetch_mint_quote()
		})
		if checksum != 16670 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_fetch_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_finalize_pending_melts()
		})
		if checksum != 37779 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_finalize_pending_melts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_flush_rate_limits()
		})
		if checksum != 8300 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_flush_rate_limits: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_keyset_count_fee()
		})
		if checksum != 4726 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_keyset_count_fee: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_and_amounts()
		})
		if checksum != 39483 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_and_amounts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_and_amounts_by_id()
		})
		if checksum != 51885 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_and_amounts_by_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_by_id()
		})
		if checksum != 51180 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_keyset_fees_by_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_npubcash_user_info()
		})
		if checksum != 28646 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_npubcash_user_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_pending_sends()
		})
		if checksum != 56442 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_pending_sends: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_proofs_by_states()
		})
		if checksum != 49189 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_proofs_by_states: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_proofs_for_transaction()
		})
		if checksum != 4480 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_proofs_for_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_transaction()
		})
		if checksum != 62811 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_get_unspent_auth_proofs()
		})
		if checksum != 31137 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_get_unspent_auth_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_is_rate_limited()
		})
		if checksum != 18806 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_is_rate_limited: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_keyset()
		})
		if checksum != 20245 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_keyset: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_keysets()
		})
		if checksum != 6059 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_keysets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_list_transactions()
		})
		if checksum != 20673 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_list_transactions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_load_mint_info()
		})
		if checksum != 12995 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_load_mint_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_melt_bip353_quote()
		})
		if checksum != 1058 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_melt_bip353_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable()
		})
		if checksum != 32210 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable_quote()
		})
		if checksum != 20299 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_melt_lightning_address_quote()
		})
		if checksum != 35934 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_melt_lightning_address_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_melt_quote()
		})
		if checksum != 14346 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint()
		})
		if checksum != 15825 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint_blind_auth()
		})
		if checksum != 16547 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint_blind_auth: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint_quote()
		})
		if checksum != 4487 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint_unified()
		})
		if checksum != 41106 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint_unified: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint_unissued_quotes()
		})
		if checksum != 50549 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint_unissued_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_mint_url()
		})
		if checksum != 6804 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_melt()
		})
		if checksum != 18573 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_melt: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_proofs()
		})
		if checksum != 47387 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_proofs_with_options()
		})
		if checksum != 23204 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_proofs_with_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_token()
		})
		if checksum != 3555 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_token_with_options()
		})
		if checksum != 9417 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_melt_token_with_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_pay_request()
		})
		if checksum != 31967 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_pay_request: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_prepare_send()
		})
		if checksum != 18579 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_prepare_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_quote_onchain_melt_options()
		})
		if checksum != 54895 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_quote_onchain_melt_options: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_receive()
		})
		if checksum != 14004 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_receive: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_receive_proofs()
		})
		if checksum != 31628 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_receive_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_recover_incomplete_sagas()
		})
		if checksum != 619 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_recover_incomplete_sagas: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_refresh_access_token()
		})
		if checksum != 63251 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_refresh_access_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_restore()
		})
		if checksum != 15985 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_restore: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_restore_with_opts()
		})
		if checksum != 61743 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_restore_with_opts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_revert_transaction()
		})
		if checksum != 31115 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_revert_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_revoke_send()
		})
		if checksum != 52137 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_revoke_send: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_select_onchain_melt_quote()
		})
		if checksum != 64446 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_select_onchain_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_set_cat()
		})
		if checksum != 29016 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_set_cat: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_set_metadata_cache_ttl()
		})
		if checksum != 24324 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_set_metadata_cache_ttl: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_set_rate_limit()
		})
		if checksum != 59693 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_set_rate_limit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_set_refresh_token()
		})
		if checksum != 28616 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_set_refresh_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_subscribe()
		})
		if checksum != 18414 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_subscribe: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_subscribe_mint_quote_state()
		})
		if checksum != 33402 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_subscribe_mint_quote_state: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_swap()
		})
		if checksum != 45250 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_swap: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_sync_missing_npubcash_quotes()
		})
		if checksum != 17258 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_sync_missing_npubcash_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_total_balance()
		})
		if checksum != 37325 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_total_balance: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_total_pending_balance()
		})
		if checksum != 26959 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_total_pending_balance: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_total_reserved_balance()
		})
		if checksum != 65325 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_total_reserved_balance: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_unit()
		})
		if checksum != 33359 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_unit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_wallet_verify_token_dleq()
		})
		if checksum != 53589 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_wallet_verify_token_dleq: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint()
		})
		if checksum != 55827 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_mints()
		})
		if checksum != 42422 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_keysets()
		})
		if checksum != 65074 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_keysets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_keyset_by_id()
		})
		if checksum != 48623 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_keyset_by_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_quote()
		})
		if checksum != 27503 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_quotes()
		})
		if checksum != 1247 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_mint_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_unissued_mint_quotes()
		})
		if checksum != 14181 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_unissued_mint_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_melt_quote()
		})
		if checksum != 58705 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_melt_quotes()
		})
		if checksum != 27131 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_melt_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_keys()
		})
		if checksum != 15412 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_proofs()
		})
		if checksum != 2478 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_proofs_by_ys()
		})
		if checksum != 63784 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_proofs_by_ys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_balance()
		})
		if checksum != 34149 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_balance: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_transaction()
		})
		if checksum != 56818 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_list_transactions()
		})
		if checksum != 46759 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_list_transactions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_kv_read()
		})
		if checksum != 55817 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_kv_read: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_kv_list()
		})
		if checksum != 45446 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_kv_list: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_p2pk_key()
		})
		if checksum != 6104 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_p2pk_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_p2pk_key()
		})
		if checksum != 52967 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_p2pk_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_list_p2pk_keys()
		})
		if checksum != 12612 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_list_p2pk_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_latest_p2pk()
		})
		if checksum != 20908 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_latest_p2pk: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_kv_write()
		})
		if checksum != 49900 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_kv_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_kv_remove()
		})
		if checksum != 41738 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_kv_remove: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_update_proofs()
		})
		if checksum != 8968 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_update_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_update_proofs_state()
		})
		if checksum != 36889 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_update_proofs_state: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_transaction()
		})
		if checksum != 29739 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_remove_transaction()
		})
		if checksum != 42313 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_remove_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_update_mint_url()
		})
		if checksum != 46882 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_update_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_increment_keyset_counter()
		})
		if checksum != 1421 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_increment_keyset_counter: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_increment_derivation_counter()
		})
		if checksum != 44160 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_increment_derivation_counter: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint()
		})
		if checksum != 16094 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_remove_mint()
		})
		if checksum != 53917 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_remove_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint_keysets()
		})
		if checksum != 52020 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint_keysets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint_quote()
		})
		if checksum != 16039 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_remove_mint_quote()
		})
		if checksum != 13037 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_remove_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_melt_quote()
		})
		if checksum != 53379 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_remove_melt_quote()
		})
		if checksum != 26890 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_remove_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_keys()
		})
		if checksum != 45970 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_remove_keys()
		})
		if checksum != 20072 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_remove_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_add_saga()
		})
		if checksum != 14140 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_add_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_saga()
		})
		if checksum != 36270 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_update_saga()
		})
		if checksum != 13131 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_update_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_delete_saga()
		})
		if checksum != 17895 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_delete_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_incomplete_sagas()
		})
		if checksum != 38480 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_incomplete_sagas: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_proofs()
		})
		if checksum != 48472 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_release_proofs()
		})
		if checksum != 63052 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_release_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_get_reserved_proofs()
		})
		if checksum != 63318 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_get_reserved_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_melt_quote()
		})
		if checksum != 32706 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_release_melt_quote()
		})
		if checksum != 43239 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_release_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_mint_quote()
		})
		if checksum != 44403 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_reserve_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletdatabase_release_mint_quote()
		})
		if checksum != 10655 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletdatabase_release_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_backup_mints()
		})
		if checksum != 56268 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_backup_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_create_wallet()
		})
		if checksum != 32021 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_create_wallet: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_fetch_mint_backup()
		})
		if checksum != 24968 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_fetch_mint_backup: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_flush_rate_limits()
		})
		if checksum != 60820 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_flush_rate_limits: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_get_balances()
		})
		if checksum != 25632 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_get_balances: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_get_or_create_wallet()
		})
		if checksum != 32359 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_get_or_create_wallet: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_get_token_data()
		})
		if checksum != 37831 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_get_token_data: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_get_wallet()
		})
		if checksum != 57352 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_get_wallet: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_get_wallets()
		})
		if checksum != 2280 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_get_wallets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_has_mint()
		})
		if checksum != 64747 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_has_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_is_rate_limited()
		})
		if checksum != 46384 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_is_rate_limited: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_mint_backup_public_key()
		})
		if checksum != 19486 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_mint_backup_public_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_remove_wallet()
		})
		if checksum != 57714 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_remove_wallet: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_restore_mints()
		})
		if checksum != 31371 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_restore_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_set_metadata_cache_ttl_for_all_mints()
		})
		if checksum != 27302 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_set_metadata_cache_ttl_for_all_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_set_metadata_cache_ttl_for_mint()
		})
		if checksum != 23477 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_set_metadata_cache_ttl_for_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletrepository_set_rate_limit()
		})
		if checksum != 165 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletrepository_set_rate_limit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_keys()
		})
		if checksum != 5879 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_melt_quote()
		})
		if checksum != 34892 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint()
		})
		if checksum != 44674 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint_keysets()
		})
		if checksum != 13932 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint_keysets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint_quote()
		})
		if checksum != 62077 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_p2pk_key()
		})
		if checksum != 51558 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_p2pk_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_saga()
		})
		if checksum != 31549 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_transaction()
		})
		if checksum != 26193 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_add_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_delete_saga()
		})
		if checksum != 25611 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_delete_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_balance()
		})
		if checksum != 3300 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_balance: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_incomplete_sagas()
		})
		if checksum != 49190 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_incomplete_sagas: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_keys()
		})
		if checksum != 41498 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_keyset_by_id()
		})
		if checksum != 37425 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_keyset_by_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_melt_quote()
		})
		if checksum != 31302 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_melt_quotes()
		})
		if checksum != 1543 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_melt_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint()
		})
		if checksum != 23917 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_keysets()
		})
		if checksum != 13541 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_keysets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_quote()
		})
		if checksum != 57388 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_quotes()
		})
		if checksum != 50536 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mint_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mints()
		})
		if checksum != 14065 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_mints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_p2pk_key()
		})
		if checksum != 60367 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_p2pk_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_proofs()
		})
		if checksum != 48231 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_proofs_by_ys()
		})
		if checksum != 13344 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_proofs_by_ys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_reserved_proofs()
		})
		if checksum != 55044 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_reserved_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_saga()
		})
		if checksum != 59736 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_transaction()
		})
		if checksum != 52949 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_unissued_mint_quotes()
		})
		if checksum != 21540 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_get_unissued_mint_quotes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_increment_derivation_counter()
		})
		if checksum != 48532 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_increment_derivation_counter: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_increment_keyset_counter()
		})
		if checksum != 61780 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_increment_keyset_counter: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_list()
		})
		if checksum != 61619 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_list: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_read()
		})
		if checksum != 16906 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_read: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_remove()
		})
		if checksum != 63132 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_remove: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_write()
		})
		if checksum != 37177 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_kv_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_latest_p2pk()
		})
		if checksum != 31994 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_latest_p2pk: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_list_p2pk_keys()
		})
		if checksum != 6462 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_list_p2pk_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_list_transactions()
		})
		if checksum != 22793 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_list_transactions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_melt_quote()
		})
		if checksum != 7347 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_mint_quote()
		})
		if checksum != 48218 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_proofs()
		})
		if checksum != 3426 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_release_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_keys()
		})
		if checksum != 64071 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_melt_quote()
		})
		if checksum != 16969 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_mint()
		})
		if checksum != 32740 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_mint: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_mint_quote()
		})
		if checksum != 55358 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_transaction()
		})
		if checksum != 38835 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_remove_transaction: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_melt_quote()
		})
		if checksum != 17298 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_melt_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_mint_quote()
		})
		if checksum != 22470 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_mint_quote: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_proofs()
		})
		if checksum != 20833 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_reserve_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_mint_url()
		})
		if checksum != 2109 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_mint_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_proofs()
		})
		if checksum != 23133 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_proofs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_proofs_state()
		})
		if checksum != 51402 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_proofs_state: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_saga()
		})
		if checksum != 32010 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_method_walletsqlitedatabase_update_saga: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_nostrinbox_new()
		})
		if checksum != 44300 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_nostrinbox_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_npubcashclient_new()
		})
		if checksum != 49637 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_npubcashclient_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_nwcservice_create()
		})
		if checksum != 38371 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_nwcservice_create: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_nwcservice_restore()
		})
		if checksum != 58630 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_nwcservice_restore: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_paymentrequest_from_string()
		})
		if checksum != 4890 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_paymentrequest_from_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_paymentrequestpayload_from_string()
		})
		if checksum != 31548 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_paymentrequestpayload_from_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_token_decode()
		})
		if checksum != 17843 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_token_decode: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_token_from_raw_bytes()
		})
		if checksum != 53011 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_token_from_raw_bytes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_token_from_string()
		})
		if checksum != 43724 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_token_from_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_tokenurdecoder_new()
		})
		if checksum != 59837 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_tokenurdecoder_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_wallet_new()
		})
		if checksum != 21828 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_wallet_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_walletrepository_new()
		})
		if checksum != 35127 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_walletrepository_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_walletrepository_new_with_config()
		})
		if checksum != 44668 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_walletrepository_new_with_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_walletrepository_new_with_proxy()
		})
		if checksum != 18844 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_walletrepository_new_with_proxy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_walletsqlitedatabase_new()
		})
		if checksum != 57227 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_walletsqlitedatabase_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_cdk_ffi_checksum_constructor_walletsqlitedatabase_new_in_memory()
		})
		if checksum != 41747 {
			// If this happens try cleaning and rebuilding your project
			panic("cdk_ffi: uniffi_cdk_ffi_checksum_constructor_walletsqlitedatabase_new_in_memory: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint8 struct{}

var FfiConverterUint8INSTANCE = FfiConverterUint8{}

func (FfiConverterUint8) Lower(value uint8) C.uint8_t {
	return C.uint8_t(value)
}

func (FfiConverterUint8) Write(writer io.Writer, value uint8) {
	writeUint8(writer, value)
}

func (FfiConverterUint8) Lift(value C.uint8_t) uint8 {
	return uint8(value)
}

func (FfiConverterUint8) Read(reader io.Reader) uint8 {
	return readUint8(reader)
}

type FfiDestroyerUint8 struct{}

func (FfiDestroyerUint8) Destroy(_ uint8) {}

type FfiConverterUint16 struct{}

var FfiConverterUint16INSTANCE = FfiConverterUint16{}

func (FfiConverterUint16) Lower(value uint16) C.uint16_t {
	return C.uint16_t(value)
}

func (FfiConverterUint16) Write(writer io.Writer, value uint16) {
	writeUint16(writer, value)
}

func (FfiConverterUint16) Lift(value C.uint16_t) uint16 {
	return uint16(value)
}

func (FfiConverterUint16) Read(reader io.Reader) uint16 {
	return readUint16(reader)
}

type FfiDestroyerUint16 struct{}

func (FfiDestroyerUint16) Destroy(_ uint16) {}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) C.RustBuffer {
	return stringToRustBuffer(value)
}

func (c FfiConverterString) LowerExternal(value string) ExternalCRustBuffer {
	return RustBufferFromC(stringToRustBuffer(value))
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) C.RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) LowerExternal(value []byte) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	handle        C.uint64_t
	callCounter   atomic.Int64
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t
	freeFunction  func(C.uint64_t, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	handle C.uint64_t,
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t,
	freeFunction func(C.uint64_t, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		handle:        handle,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) C.uint64_t {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return rustCall(func(status *C.RustCallStatus) C.uint64_t {
		return ffiObject.cloneFunction(ffiObject.handle, status)
	})
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	if ffiObject.handle == 0 {
		return
	}
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.handle, status)
		return 0
	})
}

// FFI-compatible active wallet subscription.
//
// Subscriptions may keep polling or receiving network events while callers
// wait for notifications. Mobile hosts should cancel, drop, or stop waiting on
// subscriptions during app background transitions when background network or
// storage activity is not desired.
type ActiveSubscriptionInterface interface {
	// Get the subscription ID
	Id() string
	// Receive the next notification.
	//
	// This waits for subscription activity. Mobile hosts should stop waiting or
	// cancel the subscription during app background transitions unless
	// background network activity is intended.
	Recv() (NotificationPayload, error)
	// Try to receive a notification without blocking.
	//
	// Mobile hosts should still cancel or drop the subscription during app
	// background transitions when background network activity is not desired.
	TryRecv() (*NotificationPayload, error)
}

// FFI-compatible active wallet subscription.
//
// Subscriptions may keep polling or receiving network events while callers
// wait for notifications. Mobile hosts should cancel, drop, or stop waiting on
// subscriptions during app background transitions when background network or
// storage activity is not desired.
type ActiveSubscription struct {
	ffiObject FfiObject
}

// Get the subscription ID
func (_self *ActiveSubscription) Id() string {
	_pointer := _self.ffiObject.incrementPointer("*ActiveSubscription")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_activesubscription_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Receive the next notification.
//
// This waits for subscription activity. Mobile hosts should stop waiting or
// cancel the subscription during app background transitions unless
// background network activity is intended.
func (_self *ActiveSubscription) Recv() (NotificationPayload, error) {
	_pointer := _self.ffiObject.incrementPointer("*ActiveSubscription")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) NotificationPayload {
			return FfiConverterNotificationPayloadINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_activesubscription_recv(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Try to receive a notification without blocking.
//
// Mobile hosts should still cancel or drop the subscription during app
// background transitions when background network activity is not desired.
func (_self *ActiveSubscription) TryRecv() (*NotificationPayload, error) {
	_pointer := _self.ffiObject.incrementPointer("*ActiveSubscription")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *NotificationPayload {
			return FfiConverterOptionalNotificationPayloadINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_activesubscription_try_recv(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *ActiveSubscription) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterActiveSubscription struct{}

var FfiConverterActiveSubscriptionINSTANCE = FfiConverterActiveSubscription{}

func (c FfiConverterActiveSubscription) Lift(handle C.uint64_t) *ActiveSubscription {
	result := &ActiveSubscription{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_activesubscription(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_activesubscription(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ActiveSubscription).Destroy)
	return result
}

func (c FfiConverterActiveSubscription) Read(reader io.Reader) *ActiveSubscription {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterActiveSubscription) Lower(value *ActiveSubscription) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*ActiveSubscription")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterActiveSubscription) Write(writer io.Writer, value *ActiveSubscription) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalActiveSubscription(handle uint64) *ActiveSubscription {
	return FfiConverterActiveSubscriptionINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalActiveSubscription(value *ActiveSubscription) uint64 {
	return uint64(FfiConverterActiveSubscriptionINSTANCE.Lower(value))
}

type FfiDestroyerActiveSubscription struct{}

func (_ FfiDestroyerActiveSubscription) Destroy(value *ActiveSubscription) {
	value.Destroy()
}

// A standing NIP-17 inbox listener for a single Nostr identity
//
// Subscribes the given relays for gift wraps addressed to the identity's
// public key and delivers unwrapped rumors to a [`NostrInboxListener`].
type NostrInboxInterface interface {
	// Public key of the inbox identity (hex-encoded, x-only)
	Pubkey() string
	// Connect to the relays, activate the subscription and start delivering
	// events to `listener` on a background task
	//
	// Returns once the subscription is active. Events are delivered until
	// [`NostrInbox::stop`] is called.
	//
	// # Errors
	//
	// Returns an error if a relay cannot be added or the subscription cannot
	// be created.
	Start(listener NostrInboxListener) error
	// Stop listening: cancels the background pump and disconnects from the
	// relays
	Stop()
}

// A standing NIP-17 inbox listener for a single Nostr identity
//
// Subscribes the given relays for gift wraps addressed to the identity's
// public key and delivers unwrapped rumors to a [`NostrInboxListener`].
type NostrInbox struct {
	ffiObject FfiObject
}

// Create a new inbox listener
//
// # Arguments
//
// * `nostr_secret_key` - The identity's secret key (hex or bech32 `nsec`)
// * `relays` - Relay URLs (`ws://`/`wss://`) to subscribe; must be
// non-empty
// * `since` - Optional unix timestamp lower bound for the relay `since`
// filter. Because NIP-59 backdates gift wraps, pick a generous lookback
// window instead of "now".
//
// # Errors
//
// Returns an error if the secret key or a relay URL is invalid, or no
// relays are configured.
func NewNostrInbox(nostrSecretKey string, relays []string, since *uint64) (*NostrInbox, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_nostrinbox_new(FfiConverterStringINSTANCE.Lower(nostrSecretKey), FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterOptionalUint64INSTANCE.Lower(since), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *NostrInbox
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNostrInboxINSTANCE.Lift(_uniffiRV), nil
	}
}

// Public key of the inbox identity (hex-encoded, x-only)
func (_self *NostrInbox) Pubkey() string {
	_pointer := _self.ffiObject.incrementPointer("*NostrInbox")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nostrinbox_pubkey(
				_pointer, _uniffiStatus),
		}
	}))
}

// Connect to the relays, activate the subscription and start delivering
// events to `listener` on a background task
//
// Returns once the subscription is active. Events are delivered until
// [`NostrInbox::stop`] is called.
//
// # Errors
//
// Returns an error if a relay cannot be added or the subscription cannot
// be created.
func (_self *NostrInbox) Start(listener NostrInboxListener) error {
	_pointer := _self.ffiObject.incrementPointer("*NostrInbox")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_nostrinbox_start(
			_pointer, FfiConverterNostrInboxListenerINSTANCE.Lower(listener)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Stop listening: cancels the background pump and disconnects from the
// relays
func (_self *NostrInbox) Stop() {
	_pointer := _self.ffiObject.incrementPointer("*NostrInbox")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_nostrinbox_stop(
			_pointer, _uniffiStatus)
		return false
	})
}
func (object *NostrInbox) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterNostrInbox struct{}

var FfiConverterNostrInboxINSTANCE = FfiConverterNostrInbox{}

func (c FfiConverterNostrInbox) Lift(handle C.uint64_t) *NostrInbox {
	result := &NostrInbox{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_nostrinbox(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_nostrinbox(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*NostrInbox).Destroy)
	return result
}

func (c FfiConverterNostrInbox) Read(reader io.Reader) *NostrInbox {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterNostrInbox) Lower(value *NostrInbox) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*NostrInbox")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterNostrInbox) Write(writer io.Writer, value *NostrInbox) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalNostrInbox(handle uint64) *NostrInbox {
	return FfiConverterNostrInboxINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalNostrInbox(value *NostrInbox) uint64 {
	return uint64(FfiConverterNostrInboxINSTANCE.Lower(value))
}

type FfiDestroyerNostrInbox struct{}

func (_ FfiDestroyerNostrInbox) Destroy(value *NostrInbox) {
	value.Destroy()
}

// Callback interface for [`NostrInbox`] events
//
// Implementations must be non-blocking; hand expensive work (token claims,
// database writes) off to a separate task.
type NostrInboxListener interface {
	// Called once per successfully unwrapped gift wrap
	OnEvent(event NostrInboxEvent)
}

// Callback interface for [`NostrInbox`] events
//
// Implementations must be non-blocking; hand expensive work (token claims,
// database writes) off to a separate task.
type NostrInboxListenerImpl struct {
	ffiObject FfiObject
}

// Called once per successfully unwrapped gift wrap
func (_self *NostrInboxListenerImpl) OnEvent(event NostrInboxEvent) {
	_pointer := _self.ffiObject.incrementPointer("NostrInboxListener")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_nostrinboxlistener_on_event(
			_pointer, FfiConverterNostrInboxEventINSTANCE.Lower(event), _uniffiStatus)
		return false
	})
}
func (object *NostrInboxListenerImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterNostrInboxListener struct {
	handleMap *concurrentHandleMap[NostrInboxListener]
}

var FfiConverterNostrInboxListenerINSTANCE = FfiConverterNostrInboxListener{
	handleMap: newConcurrentHandleMap[NostrInboxListener](),
}

func (c FfiConverterNostrInboxListener) Lift(handle C.uint64_t) NostrInboxListener {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &NostrInboxListenerImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_cdk_ffi_fn_clone_nostrinboxlistener(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_cdk_ffi_fn_free_nostrinboxlistener(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*NostrInboxListenerImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterNostrInboxListener) Read(reader io.Reader) NostrInboxListener {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterNostrInboxListener) Lower(value NostrInboxListener) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*NostrInboxListenerImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("NostrInboxListener")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterNostrInboxListener) Write(writer io.Writer, value NostrInboxListener) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalNostrInboxListener(handle uint64) NostrInboxListener {
	return FfiConverterNostrInboxListenerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalNostrInboxListener(value NostrInboxListener) uint64 {
	return uint64(FfiConverterNostrInboxListenerINSTANCE.Lower(value))
}

type FfiDestroyerNostrInboxListener struct{}

func (_ FfiDestroyerNostrInboxListener) Destroy(value NostrInboxListener) {
	if val, ok := value.(*NostrInboxListenerImpl); ok {
		val.Destroy()
	}
}

type uniffiCallbackResult C.int8_t

const (
	uniffiIdxCallbackFree               uniffiCallbackResult = 0
	uniffiCallbackResultSuccess         uniffiCallbackResult = 0
	uniffiCallbackResultError           uniffiCallbackResult = 1
	uniffiCallbackUnexpectedResultError uniffiCallbackResult = 2
	uniffiCallbackCancelled             uniffiCallbackResult = 3
)

type concurrentHandleMap[T any] struct {
	handles       map[uint64]T
	currentHandle uint64
	lock          sync.RWMutex
}

func newConcurrentHandleMap[T any]() *concurrentHandleMap[T] {
	return &concurrentHandleMap[T]{
		handles:       map[uint64]T{},
		currentHandle: 1,
	}
}

func (cm *concurrentHandleMap[T]) insert(obj T) uint64 {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	handle := cm.currentHandle
	cm.currentHandle = cm.currentHandle + 2
	cm.handles[handle] = obj
	return handle
}

func (cm *concurrentHandleMap[T]) remove(handle uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	delete(cm.handles, handle)
}

func (cm *concurrentHandleMap[T]) tryGet(handle uint64) (T, bool) {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	val, ok := cm.handles[handle]
	return val, ok
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerMethod0
func cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerMethod0(uniffiHandle C.uint64_t, event C.RustBuffer, uniffiOutReturn *C.void, callStatus *C.RustCallStatus) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterNostrInboxListenerINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	uniffiObj.OnEvent(
		FfiConverterNostrInboxEventINSTANCE.Lift(GoRustBuffer{
			inner: event,
		}),
	)

}

var UniffiVTableCallbackInterfaceNostrInboxListenerINSTANCE = C.UniffiVTableCallbackInterfaceNostrInboxListener{
	uniffiFree:  (C.UniffiCallbackInterfaceFree)(C.cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerFree),
	uniffiClone: (C.UniffiCallbackInterfaceClone)(C.cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerClone),
	onEvent:     (C.UniffiCallbackInterfaceNostrInboxListenerMethod0)(C.cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerMethod0),
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerFree
func cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerFree(handle C.uint64_t) {
	FfiConverterNostrInboxListenerINSTANCE.handleMap.remove(uint64(handle))
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerClone
func cdk_ffi_cgo_dispatchCallbackInterfaceNostrInboxListenerClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterNostrInboxListenerINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterNostrInboxListenerINSTANCE.handleMap.insert(val))
}

func (c FfiConverterNostrInboxListener) register() {
	C.uniffi_cdk_ffi_fn_init_callback_vtable_nostrinboxlistener(&UniffiVTableCallbackInterfaceNostrInboxListenerINSTANCE)
}

// Information needed to wait for an incoming Nostr payment
//
// Returned by `create_request` when the transport is `nostr`. Pass this to
// `wait_for_nostr_payment` to connect, subscribe, and receive the incoming
// payment on the specified relays.
type NostrWaitInfoInterface interface {
	// Get whether the original request's mint list is preferred instead of strict
	MintPreferred() *bool
	// Get the mint URLs accepted or preferred by the original payment request
	Mints() []string
	// Get the recipient public key as a hex string
	Pubkey() string
	// Get the Nostr relays to connect to
	Relays() []string
}

// Information needed to wait for an incoming Nostr payment
//
// Returned by `create_request` when the transport is `nostr`. Pass this to
// `wait_for_nostr_payment` to connect, subscribe, and receive the incoming
// payment on the specified relays.
type NostrWaitInfo struct {
	ffiObject FfiObject
}

// Get whether the original request's mint list is preferred instead of strict
func (_self *NostrWaitInfo) MintPreferred() *bool {
	_pointer := _self.ffiObject.incrementPointer("*NostrWaitInfo")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nostrwaitinfo_mint_preferred(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the mint URLs accepted or preferred by the original payment request
func (_self *NostrWaitInfo) Mints() []string {
	_pointer := _self.ffiObject.incrementPointer("*NostrWaitInfo")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nostrwaitinfo_mints(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the recipient public key as a hex string
func (_self *NostrWaitInfo) Pubkey() string {
	_pointer := _self.ffiObject.incrementPointer("*NostrWaitInfo")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nostrwaitinfo_pubkey(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the Nostr relays to connect to
func (_self *NostrWaitInfo) Relays() []string {
	_pointer := _self.ffiObject.incrementPointer("*NostrWaitInfo")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nostrwaitinfo_relays(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *NostrWaitInfo) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterNostrWaitInfo struct{}

var FfiConverterNostrWaitInfoINSTANCE = FfiConverterNostrWaitInfo{}

func (c FfiConverterNostrWaitInfo) Lift(handle C.uint64_t) *NostrWaitInfo {
	result := &NostrWaitInfo{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_nostrwaitinfo(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_nostrwaitinfo(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*NostrWaitInfo).Destroy)
	return result
}

func (c FfiConverterNostrWaitInfo) Read(reader io.Reader) *NostrWaitInfo {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterNostrWaitInfo) Lower(value *NostrWaitInfo) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*NostrWaitInfo")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterNostrWaitInfo) Write(writer io.Writer, value *NostrWaitInfo) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalNostrWaitInfo(handle uint64) *NostrWaitInfo {
	return FfiConverterNostrWaitInfoINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalNostrWaitInfo(value *NostrWaitInfo) uint64 {
	return uint64(FfiConverterNostrWaitInfoINSTANCE.Lower(value))
}

type FfiDestroyerNostrWaitInfo struct{}

func (_ FfiDestroyerNostrWaitInfo) Destroy(value *NostrWaitInfo) {
	value.Destroy()
}

// FFI-compatible NpubCash client
//
// This client provides access to the NpubCash API for fetching quotes
// and managing user settings.
type NpubCashClientInterface interface {
	// Resolve full quote data for specific quote IDs
	//
	// Asks the NpubCash server for the quotes matching `quote_ids`. Used to
	// reconcile local state with the server: fetch all quote IDs, determine
	// which ones are unknown locally, and resolve only those.
	//
	// # Arguments
	//
	// * `quote_ids` - Quote IDs to resolve
	//
	// # Errors
	//
	// Returns an error if the API request fails or authentication fails
	GetMissingQuotes(quoteIds []string) ([]NpubCashQuote, error)
	// Fetch quotes from NpubCash
	//
	// # Arguments
	//
	// * `since` - Optional Unix timestamp to fetch quotes from. If `None`, fetches all quotes.
	//
	// # Returns
	//
	// A list of quotes from the NpubCash service. The client automatically handles
	// pagination to fetch all available quotes.
	//
	// # Errors
	//
	// Returns an error if the API request fails or authentication fails
	GetQuotes(since *uint64) ([]NpubCashQuote, error)
	// Fetch the NpubCash account settings
	//
	// Returns the configured mint URL and whether quote locking is enabled.
	//
	// # Errors
	//
	// Returns an error if the API request fails or authentication fails
	GetUserInfo() (NpubCashUserResponse, error)
	// Set the mint URL for the user on the NpubCash server
	//
	// Updates the default mint URL used by the NpubCash server when creating quotes.
	//
	// # Arguments
	//
	// * `mint_url` - URL of the Cashu mint to use (e.g., <https://mint.example.com>)
	//
	// # Errors
	//
	// Returns an error if the API request fails or authentication fails
	SetMintUrl(mintUrl string) (NpubCashUserResponse, error)
	// Enable or disable NUT-20 quote locking for this NpubCash account
	//
	// When enabled, the NpubCash server creates new mint quotes locked to the
	// account's Nostr public key, so claiming them requires a NUT-20 quote
	// signature from the matching secret key. The server rejects enabling
	// locking when the configured mint does not support NUT-20.
	//
	// Already-created quotes keep their original lock state.
	//
	// # Arguments
	//
	// * `lock_quotes` - Whether new quotes should be locked to the npub
	//
	// # Errors
	//
	// Returns an error if the API request fails or authentication fails
	SetQuoteLocking(lockQuotes bool) (NpubCashUserResponse, error)
}

// FFI-compatible NpubCash client
//
// This client provides access to the NpubCash API for fetching quotes
// and managing user settings.
type NpubCashClient struct {
	ffiObject FfiObject
}

// Create a new NpubCash client
//
// # Arguments
//
// * `base_url` - Base URL of the NpubCash service (e.g., <https://npub.cash>)
// * `nostr_secret_key` - Nostr secret key for authentication. Accepts either:
// - Hex-encoded secret key (64 characters)
// - Bech32 `nsec` format (e.g., "nsec1...")
//
// # Errors
//
// Returns an error if the secret key is invalid or cannot be parsed
func NewNpubCashClient(baseUrl string, nostrSecretKey string) (*NpubCashClient, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_npubcashclient_new(FfiConverterStringINSTANCE.Lower(baseUrl), FfiConverterStringINSTANCE.Lower(nostrSecretKey), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *NpubCashClient
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNpubCashClientINSTANCE.Lift(_uniffiRV), nil
	}
}

// Resolve full quote data for specific quote IDs
//
// Asks the NpubCash server for the quotes matching `quote_ids`. Used to
// reconcile local state with the server: fetch all quote IDs, determine
// which ones are unknown locally, and resolve only those.
//
// # Arguments
//
// * `quote_ids` - Quote IDs to resolve
//
// # Errors
//
// Returns an error if the API request fails or authentication fails
func (_self *NpubCashClient) GetMissingQuotes(quoteIds []string) ([]NpubCashQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*NpubCashClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []NpubCashQuote {
			return FfiConverterSequenceNpubCashQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_npubcashclient_get_missing_quotes(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(quoteIds)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch quotes from NpubCash
//
// # Arguments
//
// * `since` - Optional Unix timestamp to fetch quotes from. If `None`, fetches all quotes.
//
// # Returns
//
// A list of quotes from the NpubCash service. The client automatically handles
// pagination to fetch all available quotes.
//
// # Errors
//
// Returns an error if the API request fails or authentication fails
func (_self *NpubCashClient) GetQuotes(since *uint64) ([]NpubCashQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*NpubCashClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []NpubCashQuote {
			return FfiConverterSequenceNpubCashQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_npubcashclient_get_quotes(
			_pointer, FfiConverterOptionalUint64INSTANCE.Lower(since)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch the NpubCash account settings
//
// Returns the configured mint URL and whether quote locking is enabled.
//
// # Errors
//
// Returns an error if the API request fails or authentication fails
func (_self *NpubCashClient) GetUserInfo() (NpubCashUserResponse, error) {
	_pointer := _self.ffiObject.incrementPointer("*NpubCashClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) NpubCashUserResponse {
			return FfiConverterNpubCashUserResponseINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_npubcashclient_get_user_info(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Set the mint URL for the user on the NpubCash server
//
// Updates the default mint URL used by the NpubCash server when creating quotes.
//
// # Arguments
//
// * `mint_url` - URL of the Cashu mint to use (e.g., <https://mint.example.com>)
//
// # Errors
//
// Returns an error if the API request fails or authentication fails
func (_self *NpubCashClient) SetMintUrl(mintUrl string) (NpubCashUserResponse, error) {
	_pointer := _self.ffiObject.incrementPointer("*NpubCashClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) NpubCashUserResponse {
			return FfiConverterNpubCashUserResponseINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_npubcashclient_set_mint_url(
			_pointer, FfiConverterStringINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Enable or disable NUT-20 quote locking for this NpubCash account
//
// When enabled, the NpubCash server creates new mint quotes locked to the
// account's Nostr public key, so claiming them requires a NUT-20 quote
// signature from the matching secret key. The server rejects enabling
// locking when the configured mint does not support NUT-20.
//
// Already-created quotes keep their original lock state.
//
// # Arguments
//
// * `lock_quotes` - Whether new quotes should be locked to the npub
//
// # Errors
//
// Returns an error if the API request fails or authentication fails
func (_self *NpubCashClient) SetQuoteLocking(lockQuotes bool) (NpubCashUserResponse, error) {
	_pointer := _self.ffiObject.incrementPointer("*NpubCashClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) NpubCashUserResponse {
			return FfiConverterNpubCashUserResponseINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_npubcashclient_set_quote_locking(
			_pointer, FfiConverterBoolINSTANCE.Lower(lockQuotes)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *NpubCashClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterNpubCashClient struct{}

var FfiConverterNpubCashClientINSTANCE = FfiConverterNpubCashClient{}

func (c FfiConverterNpubCashClient) Lift(handle C.uint64_t) *NpubCashClient {
	result := &NpubCashClient{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_npubcashclient(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_npubcashclient(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*NpubCashClient).Destroy)
	return result
}

func (c FfiConverterNpubCashClient) Read(reader io.Reader) *NpubCashClient {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterNpubCashClient) Lower(value *NpubCashClient) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*NpubCashClient")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterNpubCashClient) Write(writer io.Writer, value *NpubCashClient) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalNpubCashClient(handle uint64) *NpubCashClient {
	return FfiConverterNpubCashClientINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalNpubCashClient(value *NpubCashClient) uint64 {
	return uint64(FfiConverterNpubCashClientINSTANCE.Lower(value))
}

type FfiDestroyerNpubCashClient struct{}

func (_ FfiDestroyerNpubCashClient) Destroy(value *NpubCashClient) {
	value.Destroy()
}

// A NIP-47 Nostr Wallet Connect wallet service bound to a CDK wallet.
//
// Create one with [`NwcService::create`] (new connection) or
// [`NwcService::restore`] (existing connection from a persisted client
// secret), call [`NwcService::connection_uri`] to obtain the URI for the
// Nostr app, then [`NwcService::start`] to begin servicing requests.
type NwcServiceInterface interface {
	// Hex-encoded public key of the authorized client.
	ClientPubkey() string
	// The `nostr+walletconnect://` connection URI to hand to the Nostr app.
	ConnectionUri() string
	// Whether the background service is currently running.
	IsRunning() bool
	// Hex-encoded public key of the wallet service (advertised in the URI).
	ServicePubkey() string
	// Start servicing requests in the background.
	//
	// Connects to the relays, publishes the info event, and begins answering
	// commands. Returns immediately; the service runs until [`Self::stop`] is
	// called. Per-request failures are answered with NIP-47 error responses
	// and logged rather than surfaced here.
	//
	// # Errors
	//
	// Returns an error if the service is already running.
	Start() error
	// Stop the background service if it is running.
	Stop() error
}

// A NIP-47 Nostr Wallet Connect wallet service bound to a CDK wallet.
//
// Create one with [`NwcService::create`] (new connection) or
// [`NwcService::restore`] (existing connection from a persisted client
// secret), call [`NwcService::connection_uri`] to obtain the URI for the
// Nostr app, then [`NwcService::start`] to begin servicing requests.
type NwcService struct {
	ffiObject FfiObject
}

// Create a new wallet service with a freshly generated client connection.
//
// # Arguments
//
// * `wallet` - The CDK wallet that backs the service.
// * `relays` - Relay URLs the service connects to and listens on.
// * `service_secret_key` - Secret key of the wallet service (the signer).
// Accepts hex or bech32 `nsec`. Derive a stable one from the wallet seed
// with [`nwc_derive_service_secret_key_from_seed`].
// * `max_payment_msat` - Optional cap (in millisatoshis) on any single
// `pay_invoice` request.
//
// # Errors
//
// Returns an error if a key or relay URL is invalid, or no relays are given.
func NwcServiceCreate(wallet *Wallet, relays []string, serviceSecretKey string, maxPaymentMsat *uint64) (*NwcService, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_nwcservice_create(FfiConverterWalletINSTANCE.Lower(wallet), FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterStringINSTANCE.Lower(serviceSecretKey), FfiConverterOptionalUint64INSTANCE.Lower(maxPaymentMsat), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *NwcService
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNwcServiceINSTANCE.Lift(_uniffiRV), nil
	}
}

// Restore a wallet service for an existing connection.
//
// Use this to rebuild a service after a restart from a persisted client
// secret, so the previously issued connection URI keeps working.
//
// # Arguments
//
// * `client_secret_key` - The client secret from the original connection
// URI (hex or `nsec`).
//
// See [`Self::create`] for the other arguments.
//
// # Errors
//
// Returns an error if a key or relay URL is invalid, or no relays are given.
func NwcServiceRestore(wallet *Wallet, relays []string, serviceSecretKey string, clientSecretKey string, maxPaymentMsat *uint64) (*NwcService, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_nwcservice_restore(FfiConverterWalletINSTANCE.Lower(wallet), FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterStringINSTANCE.Lower(serviceSecretKey), FfiConverterStringINSTANCE.Lower(clientSecretKey), FfiConverterOptionalUint64INSTANCE.Lower(maxPaymentMsat), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *NwcService
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNwcServiceINSTANCE.Lift(_uniffiRV), nil
	}
}

// Hex-encoded public key of the authorized client.
func (_self *NwcService) ClientPubkey() string {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nwcservice_client_pubkey(
				_pointer, _uniffiStatus),
		}
	}))
}

// The `nostr+walletconnect://` connection URI to hand to the Nostr app.
func (_self *NwcService) ConnectionUri() string {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nwcservice_connection_uri(
				_pointer, _uniffiStatus),
		}
	}))
}

// Whether the background service is currently running.
func (_self *NwcService) IsRunning() bool {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_nwcservice_is_running(
			_pointer, _uniffiStatus)
	}))
}

// Hex-encoded public key of the wallet service (advertised in the URI).
func (_self *NwcService) ServicePubkey() string {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_nwcservice_service_pubkey(
				_pointer, _uniffiStatus),
		}
	}))
}

// Start servicing requests in the background.
//
// Connects to the relays, publishes the info event, and begins answering
// commands. Returns immediately; the service runs until [`Self::stop`] is
// called. Per-request failures are answered with NIP-47 error responses
// and logged rather than surfaced here.
//
// # Errors
//
// Returns an error if the service is already running.
func (_self *NwcService) Start() error {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_nwcservice_start(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Stop the background service if it is running.
func (_self *NwcService) Stop() error {
	_pointer := _self.ffiObject.incrementPointer("*NwcService")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_nwcservice_stop(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *NwcService) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterNwcService struct{}

var FfiConverterNwcServiceINSTANCE = FfiConverterNwcService{}

func (c FfiConverterNwcService) Lift(handle C.uint64_t) *NwcService {
	result := &NwcService{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_nwcservice(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_nwcservice(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*NwcService).Destroy)
	return result
}

func (c FfiConverterNwcService) Read(reader io.Reader) *NwcService {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterNwcService) Lower(value *NwcService) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*NwcService")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterNwcService) Write(writer io.Writer, value *NwcService) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalNwcService(handle uint64) *NwcService {
	return FfiConverterNwcServiceINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalNwcService(value *NwcService) uint64 {
	return uint64(FfiConverterNwcServiceINSTANCE.Lower(value))
}

type FfiDestroyerNwcService struct{}

func (_ FfiDestroyerNwcService) Destroy(value *NwcService) {
	value.Destroy()
}

// NUT-18 Payment Request
//
// A payment request that can be shared to request Cashu tokens.
// Encoded as a string with the `creqA` prefix.
type PaymentRequestInterface interface {
	// Get the requested amount
	Amount() *Amount
	// Get the description
	Description() *string
	// Get whether the mint list is preferred instead of strict.
	MintPreferred() *bool
	// Get the list of acceptable mint URLs
	Mints() []string
	// Get the payment ID
	PaymentId() *string
	// Get whether this is a single-use request
	SingleUse() *bool
	// Get the list of supported payment methods the mint must support
	SupportedMethods() []SupportedMethod
	// Encode the payment request to a NUT-26 bech32m string (creqB prefix)
	ToBech32String() (string, error)
	// Convert this payment request to a BIP 321 `bitcoin:` URI string.
	//
	// The cashu payment request is encoded as a NUT-26 bech32m `CREQB1...`
	// string in the `creq=` query parameter. Optionally include a BOLT11
	// invoice (`lightning=`) and/or BOLT12 offer (`lno=`) as fallback
	// payment methods for wallets that don't support cashu.
	//
	// ```text
	// val request = PaymentRequest.fromString("CREQB1...")
	// val uri = request.toBip321(
	// bolt11 = "lnbc100n1p...",
	// bolt12 = "lno1qgsq..."
	// )
	// // => "bitcoin:?creq=CREQB1...&lightning=lnbc100n1p...&lno=lno1qgsq..."
	// ```
	ToBip321(bolt11 *string, bolt12 *string) (string, error)
	// Encode the payment request to a string
	ToStringEncoded() string
	// Get the transports for delivering the payment
	Transports() []Transport
	// Get the currency unit
	Unit() *CurrencyUnit
}

// NUT-18 Payment Request
//
// A payment request that can be shared to request Cashu tokens.
// Encoded as a string with the `creqA` prefix.
type PaymentRequest struct {
	ffiObject FfiObject
}

// Parse a payment request from its encoded string representation
func PaymentRequestFromString(encoded string) (*PaymentRequest, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_paymentrequest_from_string(FfiConverterStringINSTANCE.Lower(encoded), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *PaymentRequest
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterPaymentRequestINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the requested amount
func (_self *PaymentRequest) Amount() *Amount {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the description
func (_self *PaymentRequest) Description() *string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_description(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get whether the mint list is preferred instead of strict.
func (_self *PaymentRequest) MintPreferred() *bool {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_mint_preferred(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the list of acceptable mint URLs
func (_self *PaymentRequest) Mints() []string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_mints(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the payment ID
func (_self *PaymentRequest) PaymentId() *string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_payment_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get whether this is a single-use request
func (_self *PaymentRequest) SingleUse() *bool {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_single_use(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the list of supported payment methods the mint must support
func (_self *PaymentRequest) SupportedMethods() []SupportedMethod {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceSupportedMethodINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_supported_methods(
				_pointer, _uniffiStatus),
		}
	}))
}

// Encode the payment request to a NUT-26 bech32m string (creqB prefix)
func (_self *PaymentRequest) ToBech32String() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_to_bech32_string(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Convert this payment request to a BIP 321 `bitcoin:` URI string.
//
// The cashu payment request is encoded as a NUT-26 bech32m `CREQB1...`
// string in the `creq=` query parameter. Optionally include a BOLT11
// invoice (`lightning=`) and/or BOLT12 offer (`lno=`) as fallback
// payment methods for wallets that don't support cashu.
//
// ```text
// val request = PaymentRequest.fromString("CREQB1...")
// val uri = request.toBip321(
// bolt11 = "lnbc100n1p...",
// bolt12 = "lno1qgsq..."
// )
// // => "bitcoin:?creq=CREQB1...&lightning=lnbc100n1p...&lno=lno1qgsq..."
// ```
func (_self *PaymentRequest) ToBip321(bolt11 *string, bolt12 *string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_to_bip321(
				_pointer, FfiConverterOptionalStringINSTANCE.Lower(bolt11), FfiConverterOptionalStringINSTANCE.Lower(bolt12), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode the payment request to a string
func (_self *PaymentRequest) ToStringEncoded() string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_to_string_encoded(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the transports for delivering the payment
func (_self *PaymentRequest) Transports() []Transport {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceTransportINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_transports(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the currency unit
func (_self *PaymentRequest) Unit() *CurrencyUnit {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalCurrencyUnitINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequest_unit(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *PaymentRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPaymentRequest struct{}

var FfiConverterPaymentRequestINSTANCE = FfiConverterPaymentRequest{}

func (c FfiConverterPaymentRequest) Lift(handle C.uint64_t) *PaymentRequest {
	result := &PaymentRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_paymentrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_paymentrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PaymentRequest).Destroy)
	return result
}

func (c FfiConverterPaymentRequest) Read(reader io.Reader) *PaymentRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPaymentRequest) Lower(value *PaymentRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PaymentRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPaymentRequest) Write(writer io.Writer, value *PaymentRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPaymentRequest(handle uint64) *PaymentRequest {
	return FfiConverterPaymentRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPaymentRequest(value *PaymentRequest) uint64 {
	return uint64(FfiConverterPaymentRequestINSTANCE.Lower(value))
}

type FfiDestroyerPaymentRequest struct{}

func (_ FfiDestroyerPaymentRequest) Destroy(value *PaymentRequest) {
	value.Destroy()
}

// Payment Request Payload
//
// Sent over Nostr or other transports.
type PaymentRequestPayloadInterface interface {
	// Get the ID
	Id() *string
	// Get the memo
	Memo() *string
	// Get the mint URL
	Mint() MintUrl
	// Get the proofs
	Proofs() []Proof
	// Get the currency unit
	Unit() CurrencyUnit
}

// Payment Request Payload
//
// Sent over Nostr or other transports.
type PaymentRequestPayload struct {
	ffiObject FfiObject
}

// Decode PaymentRequestPayload from JSON string
func PaymentRequestPayloadFromString(json string) (*PaymentRequestPayload, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_paymentrequestpayload_from_string(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *PaymentRequestPayload
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterPaymentRequestPayloadINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the ID
func (_self *PaymentRequestPayload) Id() *string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequestpayload_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the memo
func (_self *PaymentRequestPayload) Memo() *string {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequestpayload_memo(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the mint URL
func (_self *PaymentRequestPayload) Mint() MintUrl {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMintUrlINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequestpayload_mint(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the proofs
func (_self *PaymentRequestPayload) Proofs() []Proof {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceProofINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequestpayload_proofs(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the currency unit
func (_self *PaymentRequestPayload) Unit() CurrencyUnit {
	_pointer := _self.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCurrencyUnitINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_paymentrequestpayload_unit(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *PaymentRequestPayload) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPaymentRequestPayload struct{}

var FfiConverterPaymentRequestPayloadINSTANCE = FfiConverterPaymentRequestPayload{}

func (c FfiConverterPaymentRequestPayload) Lift(handle C.uint64_t) *PaymentRequestPayload {
	result := &PaymentRequestPayload{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_paymentrequestpayload(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_paymentrequestpayload(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PaymentRequestPayload).Destroy)
	return result
}

func (c FfiConverterPaymentRequestPayload) Read(reader io.Reader) *PaymentRequestPayload {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPaymentRequestPayload) Lower(value *PaymentRequestPayload) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PaymentRequestPayload")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPaymentRequestPayload) Write(writer io.Writer, value *PaymentRequestPayload) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPaymentRequestPayload(handle uint64) *PaymentRequestPayload {
	return FfiConverterPaymentRequestPayloadINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPaymentRequestPayload(value *PaymentRequestPayload) uint64 {
	return uint64(FfiConverterPaymentRequestPayloadINSTANCE.Lower(value))
}

type FfiDestroyerPaymentRequestPayload struct{}

func (_ FfiDestroyerPaymentRequestPayload) Destroy(value *PaymentRequestPayload) {
	value.Destroy()
}

// A pending async melt accepted by the mint.
//
// FFI callers receive this handle when the mint accepts a melt for background
// processing. Call [`PendingMelt::wait`] from a background task/coroutine to
// poll existing wallet recovery until the melt settles.
//
// Mobile apps should also call [`crate::Wallet::recover_incomplete_sagas`] or
// [`crate::Wallet::finalize_pending_melts`] on startup/resume, because
// operating systems may suspend or cancel long-running background waits.
type PendingMeltInterface interface {
	// Operation ID for this pending melt saga.
	OperationId() string
	// Quote ID for this pending melt.
	QuoteId() string
	// Wait for this pending melt to complete.
	//
	// This method polls the wallet's existing melt recovery path until the
	// pending saga finalizes or fails.
	//
	// This can wait for an extended period. Swift/Kotlin callers should run it
	// in a cancellable background task or coroutine, not directly in UI
	// control flow. If the app is suspended or killed before this returns,
	// call `Wallet::recover_incomplete_sagas()` or
	// `Wallet::finalize_pending_melts()` after restart/resume.
	Wait() (FinalizedMelt, error)
}

// A pending async melt accepted by the mint.
//
// FFI callers receive this handle when the mint accepts a melt for background
// processing. Call [`PendingMelt::wait`] from a background task/coroutine to
// poll existing wallet recovery until the melt settles.
//
// Mobile apps should also call [`crate::Wallet::recover_incomplete_sagas`] or
// [`crate::Wallet::finalize_pending_melts`] on startup/resume, because
// operating systems may suspend or cancel long-running background waits.
type PendingMelt struct {
	ffiObject FfiObject
}

// Operation ID for this pending melt saga.
func (_self *PendingMelt) OperationId() string {
	_pointer := _self.ffiObject.incrementPointer("*PendingMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_pendingmelt_operation_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Quote ID for this pending melt.
func (_self *PendingMelt) QuoteId() string {
	_pointer := _self.ffiObject.incrementPointer("*PendingMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_pendingmelt_quote_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Wait for this pending melt to complete.
//
// This method polls the wallet's existing melt recovery path until the
// pending saga finalizes or fails.
//
// This can wait for an extended period. Swift/Kotlin callers should run it
// in a cancellable background task or coroutine, not directly in UI
// control flow. If the app is suspended or killed before this returns,
// call `Wallet::recover_incomplete_sagas()` or
// `Wallet::finalize_pending_melts()` after restart/resume.
func (_self *PendingMelt) Wait() (FinalizedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*PendingMelt")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) FinalizedMelt {
			return FfiConverterFinalizedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_pendingmelt_wait(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *PendingMelt) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPendingMelt struct{}

var FfiConverterPendingMeltINSTANCE = FfiConverterPendingMelt{}

func (c FfiConverterPendingMelt) Lift(handle C.uint64_t) *PendingMelt {
	result := &PendingMelt{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_pendingmelt(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_pendingmelt(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PendingMelt).Destroy)
	return result
}

func (c FfiConverterPendingMelt) Read(reader io.Reader) *PendingMelt {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPendingMelt) Lower(value *PendingMelt) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PendingMelt")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPendingMelt) Write(writer io.Writer, value *PendingMelt) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPendingMelt(handle uint64) *PendingMelt {
	return FfiConverterPendingMeltINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPendingMelt(value *PendingMelt) uint64 {
	return uint64(FfiConverterPendingMeltINSTANCE.Lower(value))
}

type FfiDestroyerPendingMelt struct{}

func (_ FfiDestroyerPendingMelt) Destroy(value *PendingMelt) {
	value.Destroy()
}

// FFI-compatible PreparedMelt
//
// This wraps the data from a prepared melt operation along with a reference
// to the wallet. The actual PreparedMelt<'a> from cdk has a lifetime parameter
// that doesn't work with FFI, so we store the wallet and cached data separately.
type PreparedMeltInterface interface {
	// Get the amount to be melted
	Amount() Amount
	// Cancel the prepared melt and release reserved proofs
	Cancel() error
	// Get the expected change amount if swap is skipped
	ChangeAmountWithoutSwap() Amount
	// Confirm the prepared melt and execute the payment
	Confirm() (FinalizedMelt, error)
	// Confirm the prepared melt using NUT-05 async support when the mint accepts it.
	//
	// If the melt completes immediately, this returns
	// `MeltConfirmOutcome::Paid`. If the mint accepts the payment for
	// background processing, this returns `MeltConfirmOutcome::Pending` with a
	// `PendingMelt` handle.
	//
	// FFI callers should call `PendingMelt::wait()` from a background
	// task/coroutine to poll for completion. Mobile apps should also call
	// `recover_incomplete_sagas()` or `finalize_pending_melts()` on
	// startup/resume, because operating systems may suspend or cancel
	// long-running background waits.
	ConfirmPreferAsync() (MeltConfirmOutcome, error)
	// Confirm the prepared melt with custom options
	ConfirmWithOptions(options MeltConfirmOptions) (FinalizedMelt, error)
	// Get the fee reserve from the quote
	FeeReserve() Amount
	// Get the fee savings from skipping the swap
	FeeSavingsWithoutSwap() Amount
	// Get the input fee
	InputFee() Amount
	// Get the input fee if swap is skipped (fee on all proofs sent directly)
	InputFeeWithoutSwap() Amount
	// Get the operation ID for this prepared melt
	OperationId() string
	// Get the proofs that will be used
	Proofs() []Proof
	// Get the quote ID
	QuoteId() string
	// Returns true if a swap would be performed (proofs_to_swap is not empty)
	RequiresSwap() bool
	// Get the swap fee
	SwapFee() Amount
	// Get the total fee (swap fee + input fee)
	TotalFee() Amount
	// Get the total fee if swap is performed (current default behavior)
	TotalFeeWithSwap() Amount
}

// FFI-compatible PreparedMelt
//
// This wraps the data from a prepared melt operation along with a reference
// to the wallet. The actual PreparedMelt<'a> from cdk has a lifetime parameter
// that doesn't work with FFI, so we store the wallet and cached data separately.
type PreparedMelt struct {
	ffiObject FfiObject
}

// Get the amount to be melted
func (_self *PreparedMelt) Amount() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Cancel the prepared melt and release reserved proofs
func (_self *PreparedMelt) Cancel() error {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_preparedmelt_cancel(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get the expected change amount if swap is skipped
func (_self *PreparedMelt) ChangeAmountWithoutSwap() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_change_amount_without_swap(
				_pointer, _uniffiStatus),
		}
	}))
}

// Confirm the prepared melt and execute the payment
func (_self *PreparedMelt) Confirm() (FinalizedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) FinalizedMelt {
			return FfiConverterFinalizedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_preparedmelt_confirm(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Confirm the prepared melt using NUT-05 async support when the mint accepts it.
//
// If the melt completes immediately, this returns
// `MeltConfirmOutcome::Paid`. If the mint accepts the payment for
// background processing, this returns `MeltConfirmOutcome::Pending` with a
// `PendingMelt` handle.
//
// FFI callers should call `PendingMelt::wait()` from a background
// task/coroutine to poll for completion. Mobile apps should also call
// `recover_incomplete_sagas()` or `finalize_pending_melts()` on
// startup/resume, because operating systems may suspend or cancel
// long-running background waits.
func (_self *PreparedMelt) ConfirmPreferAsync() (MeltConfirmOutcome, error) {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltConfirmOutcome {
			return FfiConverterMeltConfirmOutcomeINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_preparedmelt_confirm_prefer_async(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Confirm the prepared melt with custom options
func (_self *PreparedMelt) ConfirmWithOptions(options MeltConfirmOptions) (FinalizedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) FinalizedMelt {
			return FfiConverterFinalizedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_preparedmelt_confirm_with_options(
			_pointer, FfiConverterMeltConfirmOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the fee reserve from the quote
func (_self *PreparedMelt) FeeReserve() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_fee_reserve(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the fee savings from skipping the swap
func (_self *PreparedMelt) FeeSavingsWithoutSwap() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_fee_savings_without_swap(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the input fee
func (_self *PreparedMelt) InputFee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_input_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the input fee if swap is skipped (fee on all proofs sent directly)
func (_self *PreparedMelt) InputFeeWithoutSwap() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_input_fee_without_swap(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the operation ID for this prepared melt
func (_self *PreparedMelt) OperationId() string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_operation_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the proofs that will be used
func (_self *PreparedMelt) Proofs() []Proof {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceProofINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_proofs(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the quote ID
func (_self *PreparedMelt) QuoteId() string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_quote_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Returns true if a swap would be performed (proofs_to_swap is not empty)
func (_self *PreparedMelt) RequiresSwap() bool {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_preparedmelt_requires_swap(
			_pointer, _uniffiStatus)
	}))
}

// Get the swap fee
func (_self *PreparedMelt) SwapFee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_swap_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the total fee (swap fee + input fee)
func (_self *PreparedMelt) TotalFee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_total_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the total fee if swap is performed (current default behavior)
func (_self *PreparedMelt) TotalFeeWithSwap() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedMelt")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedmelt_total_fee_with_swap(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *PreparedMelt) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPreparedMelt struct{}

var FfiConverterPreparedMeltINSTANCE = FfiConverterPreparedMelt{}

func (c FfiConverterPreparedMelt) Lift(handle C.uint64_t) *PreparedMelt {
	result := &PreparedMelt{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_preparedmelt(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_preparedmelt(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PreparedMelt).Destroy)
	return result
}

func (c FfiConverterPreparedMelt) Read(reader io.Reader) *PreparedMelt {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPreparedMelt) Lower(value *PreparedMelt) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PreparedMelt")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPreparedMelt) Write(writer io.Writer, value *PreparedMelt) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPreparedMelt(handle uint64) *PreparedMelt {
	return FfiConverterPreparedMeltINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPreparedMelt(value *PreparedMelt) uint64 {
	return uint64(FfiConverterPreparedMeltINSTANCE.Lower(value))
}

type FfiDestroyerPreparedMelt struct{}

func (_ FfiDestroyerPreparedMelt) Destroy(value *PreparedMelt) {
	value.Destroy()
}

// FFI-compatible prepared NUT-18 payment request.
type PreparedPaymentRequestInterface interface {
	// Cancel the prepared payment and release reserved proofs.
	Cancel() error
	// Confirm and deliver the prepared payment.
	//
	// If delivery fails after token creation, this returns
	// `FfiError::PaymentRequestDeliveryFailed` with the pending operation ID.
	// Do not prepare the payment again. Call `Wallet.revoke_send(operation_id)`
	// to reclaim it if it remains unclaimed.
	Confirm() error
	// Total mint input fee.
	InputFee() Amount
	// Selected payment method, when restricted by the request.
	Method() *string
	// Applicable receiver-selected method fee (`mf`).
	MethodFee() Amount
	// Mint selected for the payment.
	MintUrl() string
	// Operation ID of the reserved send.
	OperationId() string
	// Requested amount plus the applicable method fee.
	PaymentAmount() Amount
	// Amount requested before the receiver-selected method fee.
	RequestedAmount() Amount
	// Total wallet debit.
	TotalAmount() Amount
	// Currency unit of the payment.
	Unit() CurrencyUnit
}

// FFI-compatible prepared NUT-18 payment request.
type PreparedPaymentRequest struct {
	ffiObject FfiObject
}

// Cancel the prepared payment and release reserved proofs.
func (_self *PreparedPaymentRequest) Cancel() error {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_cancel(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Confirm and deliver the prepared payment.
//
// If delivery fails after token creation, this returns
// `FfiError::PaymentRequestDeliveryFailed` with the pending operation ID.
// Do not prepare the payment again. Call `Wallet.revoke_send(operation_id)`
// to reclaim it if it remains unclaimed.
func (_self *PreparedPaymentRequest) Confirm() error {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_confirm(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Total mint input fee.
func (_self *PreparedPaymentRequest) InputFee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_input_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Selected payment method, when restricted by the request.
func (_self *PreparedPaymentRequest) Method() *string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_method(
				_pointer, _uniffiStatus),
		}
	}))
}

// Applicable receiver-selected method fee (`mf`).
func (_self *PreparedPaymentRequest) MethodFee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_method_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Mint selected for the payment.
func (_self *PreparedPaymentRequest) MintUrl() string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_mint_url(
				_pointer, _uniffiStatus),
		}
	}))
}

// Operation ID of the reserved send.
func (_self *PreparedPaymentRequest) OperationId() string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_operation_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Requested amount plus the applicable method fee.
func (_self *PreparedPaymentRequest) PaymentAmount() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_payment_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Amount requested before the receiver-selected method fee.
func (_self *PreparedPaymentRequest) RequestedAmount() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_requested_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Total wallet debit.
func (_self *PreparedPaymentRequest) TotalAmount() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_total_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Currency unit of the payment.
func (_self *PreparedPaymentRequest) Unit() CurrencyUnit {
	_pointer := _self.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCurrencyUnitINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedpaymentrequest_unit(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *PreparedPaymentRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPreparedPaymentRequest struct{}

var FfiConverterPreparedPaymentRequestINSTANCE = FfiConverterPreparedPaymentRequest{}

func (c FfiConverterPreparedPaymentRequest) Lift(handle C.uint64_t) *PreparedPaymentRequest {
	result := &PreparedPaymentRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_preparedpaymentrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_preparedpaymentrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PreparedPaymentRequest).Destroy)
	return result
}

func (c FfiConverterPreparedPaymentRequest) Read(reader io.Reader) *PreparedPaymentRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPreparedPaymentRequest) Lower(value *PreparedPaymentRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PreparedPaymentRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPreparedPaymentRequest) Write(writer io.Writer, value *PreparedPaymentRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPreparedPaymentRequest(handle uint64) *PreparedPaymentRequest {
	return FfiConverterPreparedPaymentRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPreparedPaymentRequest(value *PreparedPaymentRequest) uint64 {
	return uint64(FfiConverterPreparedPaymentRequestINSTANCE.Lower(value))
}

type FfiDestroyerPreparedPaymentRequest struct{}

func (_ FfiDestroyerPreparedPaymentRequest) Destroy(value *PreparedPaymentRequest) {
	value.Destroy()
}

// FFI-compatible PreparedSend
//
// This wraps the data from a prepared send operation along with a reference
// to the wallet. The actual PreparedSend<'a> from cdk has a lifetime parameter
// that doesn't work with FFI, so we store the wallet and cached data separately.
type PreparedSendInterface interface {
	// Get the amount to send
	Amount() Amount
	// Cancel the prepared send operation
	Cancel() error
	// Confirm the prepared send and create a token
	Confirm(memo *string) (*Token, error)
	// Get the total fee for this send operation
	Fee() Amount
	// Get the operation ID for this prepared send
	OperationId() string
	// Get the proofs that will be used
	Proofs() []Proof
}

// FFI-compatible PreparedSend
//
// This wraps the data from a prepared send operation along with a reference
// to the wallet. The actual PreparedSend<'a> from cdk has a lifetime parameter
// that doesn't work with FFI, so we store the wallet and cached data separately.
type PreparedSend struct {
	ffiObject FfiObject
}

// Get the amount to send
func (_self *PreparedSend) Amount() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedsend_amount(
				_pointer, _uniffiStatus),
		}
	}))
}

// Cancel the prepared send operation
func (_self *PreparedSend) Cancel() error {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_preparedsend_cancel(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Confirm the prepared send and create a token
func (_self *PreparedSend) Confirm(memo *string) (*Token, error) {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *Token {
			return FfiConverterTokenINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_preparedsend_confirm(
			_pointer, FfiConverterOptionalStringINSTANCE.Lower(memo)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the total fee for this send operation
func (_self *PreparedSend) Fee() Amount {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterAmountINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedsend_fee(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the operation ID for this prepared send
func (_self *PreparedSend) OperationId() string {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedsend_operation_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the proofs that will be used
func (_self *PreparedSend) Proofs() []Proof {
	_pointer := _self.ffiObject.incrementPointer("*PreparedSend")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceProofINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_preparedsend_proofs(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *PreparedSend) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPreparedSend struct{}

var FfiConverterPreparedSendINSTANCE = FfiConverterPreparedSend{}

func (c FfiConverterPreparedSend) Lift(handle C.uint64_t) *PreparedSend {
	result := &PreparedSend{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_preparedsend(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_preparedsend(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PreparedSend).Destroy)
	return result
}

func (c FfiConverterPreparedSend) Read(reader io.Reader) *PreparedSend {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPreparedSend) Lower(value *PreparedSend) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PreparedSend")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPreparedSend) Write(writer io.Writer, value *PreparedSend) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPreparedSend(handle uint64) *PreparedSend {
	return FfiConverterPreparedSendINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPreparedSend(value *PreparedSend) uint64 {
	return uint64(FfiConverterPreparedSendINSTANCE.Lower(value))
}

type FfiDestroyerPreparedSend struct{}

func (_ FfiDestroyerPreparedSend) Destroy(value *PreparedSend) {
	value.Destroy()
}

// FFI-compatible Token
type TokenInterface interface {
	// Encode token to string representation
	Encode() string
	// Return all HTLC hashes from spending conditions
	HtlcHashes() []string
	// Return all locktimes from spending conditions (sorted ascending)
	Locktimes() []uint64
	// Get the memo from the token
	Memo() *string
	// Get the mint URL
	MintUrl() (MintUrl, error)
	// Return all P2PK pubkeys referenced by this token's spending conditions
	P2pkPubkeys() []string
	// Return all refund pubkeys from P2PK spending conditions
	P2pkRefundPubkeys() []string
	// Get proofs from the token
	Proofs(mintKeysets []KeySetInfo) ([]Proof, error)
	// Get proofs from the token (simplified - no keyset filtering for now)
	ProofsSimple() ([]Proof, error)
	// Return unique spending conditions across all proofs in this token
	SpendingConditions() []SpendingConditions
	// Convert token to raw bytes
	ToRawBytes() ([]byte, error)
	// Get the currency unit
	Unit() *CurrencyUnit
	// Create a NUT-16 UR encoder for displaying this token as an animated
	// QR code
	//
	// `max_fragment_length` is the maximum number of payload bytes per QR
	// frame; `None` selects a default suited to most QR scanners. Each
	// `TokenUrEncoder::next_part` fragment is displayed as one QR frame.
	UrEncoder(maxFragmentLength *uint32) (*TokenUrEncoder, error)
	// Get the total value of the token
	Value() (Amount, error)
}

// FFI-compatible Token
type Token struct {
	ffiObject FfiObject
}

// Decode token from string representation
func TokenDecode(encodedToken string) (*Token, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_token_decode(FfiConverterStringINSTANCE.Lower(encodedToken), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Token
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterTokenINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode token from raw bytes
func TokenFromRawBytes(bytes []byte) (*Token, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_token_from_raw_bytes(FfiConverterBytesINSTANCE.Lower(bytes), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Token
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterTokenINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new Token from string
func TokenFromString(encodedToken string) (*Token, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_token_from_string(FfiConverterStringINSTANCE.Lower(encodedToken), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Token
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterTokenINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode token to string representation
func (_self *Token) Encode() string {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_encode(
				_pointer, _uniffiStatus),
		}
	}))
}

// Return all HTLC hashes from spending conditions
func (_self *Token) HtlcHashes() []string {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_htlc_hashes(
				_pointer, _uniffiStatus),
		}
	}))
}

// Return all locktimes from spending conditions (sorted ascending)
func (_self *Token) Locktimes() []uint64 {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_locktimes(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the memo from the token
func (_self *Token) Memo() *string {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_memo(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get the mint URL
func (_self *Token) MintUrl() (MintUrl, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_mint_url(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MintUrl
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMintUrlINSTANCE.Lift(_uniffiRV), nil
	}
}

// Return all P2PK pubkeys referenced by this token's spending conditions
func (_self *Token) P2pkPubkeys() []string {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_p2pk_pubkeys(
				_pointer, _uniffiStatus),
		}
	}))
}

// Return all refund pubkeys from P2PK spending conditions
func (_self *Token) P2pkRefundPubkeys() []string {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_p2pk_refund_pubkeys(
				_pointer, _uniffiStatus),
		}
	}))
}

// Get proofs from the token
func (_self *Token) Proofs(mintKeysets []KeySetInfo) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_proofs(
				_pointer, FfiConverterSequenceKeySetInfoINSTANCE.Lower(mintKeysets), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []Proof
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSequenceProofINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get proofs from the token (simplified - no keyset filtering for now)
func (_self *Token) ProofsSimple() ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_proofs_simple(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []Proof
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSequenceProofINSTANCE.Lift(_uniffiRV), nil
	}
}

// Return unique spending conditions across all proofs in this token
func (_self *Token) SpendingConditions() []SpendingConditions {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceSpendingConditionsINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_spending_conditions(
				_pointer, _uniffiStatus),
		}
	}))
}

// Convert token to raw bytes
func (_self *Token) ToRawBytes() ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_to_raw_bytes(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []byte
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBytesINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the currency unit
func (_self *Token) Unit() *CurrencyUnit {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalCurrencyUnitINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_unit(
				_pointer, _uniffiStatus),
		}
	}))
}

// Create a NUT-16 UR encoder for displaying this token as an animated
// QR code
//
// `max_fragment_length` is the maximum number of payload bytes per QR
// frame; `None` selects a default suited to most QR scanners. Each
// `TokenUrEncoder::next_part` fragment is displayed as one QR frame.
func (_self *Token) UrEncoder(maxFragmentLength *uint32) (*TokenUrEncoder, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_method_token_ur_encoder(
			_pointer, FfiConverterOptionalUint32INSTANCE.Lower(maxFragmentLength), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *TokenUrEncoder
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterTokenUrEncoderINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the total value of the token
func (_self *Token) Value() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Token")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_token_value(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Amount
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAmountINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *Token) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterToken struct{}

var FfiConverterTokenINSTANCE = FfiConverterToken{}

func (c FfiConverterToken) Lift(handle C.uint64_t) *Token {
	result := &Token{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_token(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_token(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Token).Destroy)
	return result
}

func (c FfiConverterToken) Read(reader io.Reader) *Token {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterToken) Lower(value *Token) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Token")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterToken) Write(writer io.Writer, value *Token) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalToken(handle uint64) *Token {
	return FfiConverterTokenINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalToken(value *Token) uint64 {
	return uint64(FfiConverterTokenINSTANCE.Lower(value))
}

type FfiDestroyerToken struct{}

func (_ FfiDestroyerToken) Destroy(value *Token) {
	value.Destroy()
}

// FFI-compatible NUT-16 UR decoder for animated QR codes
//
// Reassembles a token from scanned UR fragments. Feed each scanned QR
// frame with `receive` in any order until `complete` returns true, then
// read the token with `token`.
type TokenUrDecoderInterface interface {
	// Returns whether the token has been fully reassembled
	Complete() bool
	// Returns the total number of fragments the token was split into
	//
	// This is `0` until the first multi-part fragment is received.
	FragmentCount() uint32
	// Feed one scanned QR frame into the decoder
	//
	// Accepts both multi-part fragments (`ur:bytes/<seq>-<len>/<payload>`)
	// and the single-part form (`ur:bytes/<payload>`).
	Receive(part string) error
	// Returns the number of fragments resolved so far, either received
	// directly or recovered via the fountain code
	//
	// Useful for progress indication. Returns `None` before any fragment
	// has been received.
	ResolvedFragmentCount() *uint32
	// Returns the reassembled token once `complete`, `None` while decoding
	// is incomplete
	Token() (**Token, error)
}

// FFI-compatible NUT-16 UR decoder for animated QR codes
//
// Reassembles a token from scanned UR fragments. Feed each scanned QR
// frame with `receive` in any order until `complete` returns true, then
// read the token with `token`.
type TokenUrDecoder struct {
	ffiObject FfiObject
}

// Create a new decoder
func NewTokenUrDecoder() *TokenUrDecoder {
	return FfiConverterTokenUrDecoderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_tokenurdecoder_new(_uniffiStatus)
	}))
}

// Returns whether the token has been fully reassembled
func (_self *TokenUrDecoder) Complete() bool {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrDecoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_tokenurdecoder_complete(
			_pointer, _uniffiStatus)
	}))
}

// Returns the total number of fragments the token was split into
//
// This is `0` until the first multi-part fragment is received.
func (_self *TokenUrDecoder) FragmentCount() uint32 {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrDecoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint32INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.uniffi_cdk_ffi_fn_method_tokenurdecoder_fragment_count(
			_pointer, _uniffiStatus)
	}))
}

// Feed one scanned QR frame into the decoder
//
// Accepts both multi-part fragments (`ur:bytes/<seq>-<len>/<payload>`)
// and the single-part form (`ur:bytes/<payload>`).
func (_self *TokenUrDecoder) Receive(part string) error {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrDecoder")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_tokenurdecoder_receive(
			_pointer, FfiConverterStringINSTANCE.Lower(part), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Returns the number of fragments resolved so far, either received
// directly or recovered via the fountain code
//
// Useful for progress indication. Returns `None` before any fragment
// has been received.
func (_self *TokenUrDecoder) ResolvedFragmentCount() *uint32 {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrDecoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalUint32INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_tokenurdecoder_resolved_fragment_count(
				_pointer, _uniffiStatus),
		}
	}))
}

// Returns the reassembled token once `complete`, `None` while decoding
// is incomplete
func (_self *TokenUrDecoder) Token() (**Token, error) {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrDecoder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_tokenurdecoder_token(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue **Token
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterOptionalTokenINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *TokenUrDecoder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterTokenUrDecoder struct{}

var FfiConverterTokenUrDecoderINSTANCE = FfiConverterTokenUrDecoder{}

func (c FfiConverterTokenUrDecoder) Lift(handle C.uint64_t) *TokenUrDecoder {
	result := &TokenUrDecoder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_tokenurdecoder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_tokenurdecoder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*TokenUrDecoder).Destroy)
	return result
}

func (c FfiConverterTokenUrDecoder) Read(reader io.Reader) *TokenUrDecoder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterTokenUrDecoder) Lower(value *TokenUrDecoder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*TokenUrDecoder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterTokenUrDecoder) Write(writer io.Writer, value *TokenUrDecoder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalTokenUrDecoder(handle uint64) *TokenUrDecoder {
	return FfiConverterTokenUrDecoderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalTokenUrDecoder(value *TokenUrDecoder) uint64 {
	return uint64(FfiConverterTokenUrDecoderINSTANCE.Lower(value))
}

type FfiDestroyerTokenUrDecoder struct{}

func (_ FfiDestroyerTokenUrDecoder) Destroy(value *TokenUrDecoder) {
	value.Destroy()
}

// FFI-compatible NUT-16 UR encoder for animated QR codes
//
// Splits a token into a fountain-coded sequence of UR fragments
// (`ur:bytes/…`), one per QR frame. The first `fragment_count` frames cover
// the whole token; the stream is unbounded and frames beyond that are
// redundant fountain parts, so a receiver can complete from any
// sufficiently large subset of frames. Senders typically loop the frames
// until the receiver signals completion.
//
// If the token fits into a single frame, the single-part form
// (`ur:bytes/<payload>`, without fragment indices) is emitted.
type TokenUrEncoderInterface interface {
	// Returns the number of fragments emitted so far
	CurrentIndex() uint32
	// Returns the number of fragments the token was split into
	FragmentCount() uint32
	// Returns whether the token fits into a single QR frame
	IsSingleFragment() bool
	// Returns the next UR fragment to display as a QR frame
	NextPart() (string, error)
}

// FFI-compatible NUT-16 UR encoder for animated QR codes
//
// Splits a token into a fountain-coded sequence of UR fragments
// (`ur:bytes/…`), one per QR frame. The first `fragment_count` frames cover
// the whole token; the stream is unbounded and frames beyond that are
// redundant fountain parts, so a receiver can complete from any
// sufficiently large subset of frames. Senders typically loop the frames
// until the receiver signals completion.
//
// If the token fits into a single frame, the single-part form
// (`ur:bytes/<payload>`, without fragment indices) is emitted.
type TokenUrEncoder struct {
	ffiObject FfiObject
}

// Returns the number of fragments emitted so far
func (_self *TokenUrEncoder) CurrentIndex() uint32 {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrEncoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint32INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.uniffi_cdk_ffi_fn_method_tokenurencoder_current_index(
			_pointer, _uniffiStatus)
	}))
}

// Returns the number of fragments the token was split into
func (_self *TokenUrEncoder) FragmentCount() uint32 {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrEncoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint32INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.uniffi_cdk_ffi_fn_method_tokenurencoder_fragment_count(
			_pointer, _uniffiStatus)
	}))
}

// Returns whether the token fits into a single QR frame
func (_self *TokenUrEncoder) IsSingleFragment() bool {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrEncoder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_tokenurencoder_is_single_fragment(
			_pointer, _uniffiStatus)
	}))
}

// Returns the next UR fragment to display as a QR frame
func (_self *TokenUrEncoder) NextPart() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*TokenUrEncoder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_tokenurencoder_next_part(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *TokenUrEncoder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterTokenUrEncoder struct{}

var FfiConverterTokenUrEncoderINSTANCE = FfiConverterTokenUrEncoder{}

func (c FfiConverterTokenUrEncoder) Lift(handle C.uint64_t) *TokenUrEncoder {
	result := &TokenUrEncoder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_tokenurencoder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_tokenurencoder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*TokenUrEncoder).Destroy)
	return result
}

func (c FfiConverterTokenUrEncoder) Read(reader io.Reader) *TokenUrEncoder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterTokenUrEncoder) Lower(value *TokenUrEncoder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*TokenUrEncoder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterTokenUrEncoder) Write(writer io.Writer, value *TokenUrEncoder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalTokenUrEncoder(handle uint64) *TokenUrEncoder {
	return FfiConverterTokenUrEncoderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalTokenUrEncoder(value *TokenUrEncoder) uint64 {
	return uint64(FfiConverterTokenUrEncoderINSTANCE.Lower(value))
}

type FfiDestroyerTokenUrEncoder struct{}

func (_ FfiDestroyerTokenUrEncoder) Destroy(value *TokenUrEncoder) {
	value.Destroy()
}

// FFI-compatible wallet.
//
// Wallet methods can write to the configured local store while they perform
// mint, receive, recovery, subscription, and status operations. Mobile host
// apps own platform lifecycle handling around these calls: pause or cancel work
// when moving to the background unless background network and storage activity
// is intended, and use platform facilities such as iOS `beginBackgroundTask`
// when an operation must finish after a lifecycle transition.
type WalletInterface interface {
	// Get the active keyset with the lowest fees
	ActiveKeyset() (KeySet, error)
	// Calculate fee for a given number of proofs with the specified keyset
	CalculateFee(proofCount uint32, keysetId string) (Amount, error)
	// Check all pending proofs and return the total amount still pending
	//
	// This function checks orphaned pending proofs (not managed by active sagas)
	// with the mint and marks spent proofs accordingly.
	// It may perform network requests and write proof-state updates to the
	// local store, so mobile hosts should coordinate it with app lifecycle
	// transitions.
	CheckAllPendingProofs() (Amount, error)
	// Check melt quote status and attempt to complete any in-progress saga.
	CheckMeltQuoteStatus(quoteId string) (MeltQuote, error)
	// Check a mint quote status from the mint.
	//
	// Calls `GET /v1/mint/quote/{method}/{quote_id}` per NUT-04.
	// Updates local store with current state from mint.
	// If there was a crashed mid-mint (pending saga), attempts to complete it.
	// Does NOT mint tokens directly - use mint() for that.
	// This may perform network requests and write recovery/status updates to
	// the local store, so mobile hosts should coordinate it with app lifecycle
	// transitions.
	//
	// **Note:** The mint quote must be known to the wallet (stored locally) for this
	// function to work. If the quote is not stored locally, use `fetch_mint_quote`
	// instead.
	CheckMintQuote(quoteId string) (MintQuote, error)
	// Check a mint quote status from the mint.
	//
	// Calls `GET /v1/mint/quote/{method}/{quote_id}` per NUT-04.
	// Updates local store with current state from mint.
	// If there was a crashed mid-mint (pending saga), attempts to complete it.
	// Does NOT mint tokens directly - use mint() for that.
	// This may perform network requests and write recovery/status updates to
	// the local store, so mobile hosts should coordinate it with app lifecycle
	// transitions.
	//
	// **Note:** The mint quote must be known to the wallet (stored locally) for this
	// function to work. If the quote is not stored locally, use `fetch_mint_quote`
	// instead.
	//
	// # Errors
	// Returns an error for a mismatched response quote ID before changing local state.
	CheckMintQuoteStatus(quoteId string) (MintQuote, error)
	// Check if proofs are spent
	CheckProofsSpent(proofs []Proof) ([]bool, error)
	// Check status of a pending send operation
	CheckSendStatus(operationId string) (bool, error)
	// Claim all pending NpubCash quotes
	//
	// Syncs quotes from the NpubCash server (including reconciliation of
	// quotes missing locally) and mints every paid NpubCash quote that has
	// not been issued yet. Only quotes attributable to the wallet's NpubCash
	// accounts are claimed; unrelated mint quotes created through normal
	// wallet flows are left untouched. Mints that advertise NUT-29 are
	// claimed with batch minting automatically; other mints fall back to
	// individual minting.
	//
	// Returns the total amount minted across all claimed quotes.
	ClaimNpubcashQuotes() (Amount, error)
	// Create quotes for transferring the maximum amount allowed by the source
	// balance and both mints' advertised BOLT11 limits.
	//
	// The returned input fee assumes all currently unspent source proofs are
	// used. Prepare the returned melt quote with those proofs and confirm it
	// with `skip_swap` to preserve that accounting.
	//
	// This search may create multiple quote pairs at the remote mints. Only the
	// returned pair is persisted locally; unused remote quotes cannot be
	// cancelled and remain until they expire.
	CrossMintTransferQuoteMax(targetWallet *Wallet) (CrossMintTransferQuote, error)
	// Enable NpubCash integration for this wallet
	//
	// Derives the NpubCash Nostr keys from the wallet seed, creates the
	// API client, sets this wallet's mint URL on the server, and enables
	// NUT-20 quote locking for newly created quotes. The integration is
	// only activated once locking has been enabled and confirmed.
	//
	// Returns an error if quote locking cannot be established — for
	// example when the configured mint does not support NUT-20.
	EnableNpubcash(npubcashUrl string) error
	// Get mint info from mint
	FetchMintInfo() (*MintInfo, error)
	// Fetch a mint quote from the mint and store it locally.
	//
	// This performs network I/O and writes the fetched quote to the local store.
	// Mobile hosts should avoid starting it during app background transitions
	// unless background network and storage activity is intended.
	//
	// Works with all payment methods (Bolt11, Bolt12, and custom payment methods).
	//
	// # Arguments
	// * `quote_id` - The ID of the quote to fetch
	// * `payment_method` - The payment method for the quote. Required if the quote
	// is not already stored locally. If the quote exists locally, the stored
	// payment method will be used and this parameter is ignored.
	//
	// # Errors
	// Returns an error for a mismatched response quote ID before storing the quote.
	FetchMintQuote(quoteId string, paymentMethod *PaymentMethod) (MintQuote, error)
	// Finalize pending melt operations for this wallet.
	FinalizePendingMelts() ([]FinalizedMelt, error)
	// Wait until the rate-limit budget this wallet drew down has been stored.
	//
	// Await this before dropping the wallet on shutdown. Without it,
	// persistence is best effort and a rebuild can outrun the detached writer,
	// so the rebuilt wallet starts with a full burst against the mint's cap.
	FlushRateLimits()
	// Get fee for count of proofs in a keyset
	GetKeysetCountFee(keysetId string, count uint64) (Amount, error)
	// Get fees and amounts for all keysets
	GetKeysetFeesAndAmounts() (map[string]FeeAndAmounts, error)
	// Get fees and amounts for a specific keyset
	GetKeysetFeesAndAmountsById(keysetId string) (FeeAndAmounts, error)
	// Get fees for a specific keyset ID
	GetKeysetFeesById(keysetId string) (uint64, error)
	// Fetch this wallet's NpubCash account settings
	//
	// Returns the configured mint URL and whether quote locking is enabled.
	// Requires NpubCash to be enabled on this wallet first.
	GetNpubcashUserInfo() (NpubCashUserResponse, error)
	// Get all pending send operations
	GetPendingSends() ([]string, error)
	// Get proofs by states
	GetProofsByStates(states []ProofState) ([]Proof, error)
	// Get proofs for a transaction by transaction ID
	//
	// This retrieves all proofs associated with a transaction by looking up
	// the transaction's Y values and fetching the corresponding proofs.
	GetProofsForTransaction(id TransactionId) ([]Proof, error)
	// Get transaction by ID
	GetTransaction(id TransactionId) (*Transaction, error)
	// Get unspent auth proofs
	GetUnspentAuthProofs() ([]AuthProof, error)
	// Whether this wallet's requests are being paced right now.
	//
	// Also false when the wallet paces nothing at all, which is the case
	// [`Wallet::set_rate_limit`] silently ignores.
	IsRateLimited() bool
	// Get a single keyset by ID
	Keyset(keysetId string) (KeySet, error)
	// Get all keysets for this wallet's unit
	Keysets(policy *KeysetLoadPolicy) ([]KeySet, error)
	// List transactions
	ListTransactions(direction *TransactionDirection) ([]Transaction, error)
	// Load mint info
	//
	// This will get mint info from cache if it is fresh
	LoadMintInfo() (MintInfo, error)
	// Get a quote for a BIP353 melt
	//
	// This method resolves a BIP353 address (e.g., "alice@example.com") to a Bitcoin
	// payment instruction, requires a BOLT12 offer, and then creates a melt quote for it.
	//
	// The `network` parameter controls which on-chain address prefixes are accepted
	// in the resolved URI.
	MeltBip353Quote(bip353Address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error)
	// Get a quote for a human-readable address melt
	//
	// This method accepts a human-readable address that could be either a BIP353 address
	// or a Lightning address. It intelligently determines which to try based on mint support:
	//
	// 1. If the mint supports Bolt12, it tries BIP353 first
	// 2. Falls back to Lightning address only if BIP353 resolution fails
	// 3. If BIP353 resolves but has no usable BOLT12 offer, it does NOT fall back
	// 4. If the mint doesn't support Bolt12, it tries Lightning address directly
	//
	// The `network` parameter is forwarded to the BIP353 resolver for on-chain address
	// validation in the resolved URI.
	MeltHumanReadable(address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error)
	// Get a quote for a human-readable address melt
	//
	// Accepts a human-readable address that could be either a BIP353 address
	// or a Lightning address. Tries BIP353 first if mint supports Bolt12,
	// falls back to Lightning address.
	MeltHumanReadableQuote(address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error)
	// Get a quote for a Lightning address melt
	//
	// This method resolves a Lightning address (e.g., "alice@example.com") to a Lightning invoice
	// and then creates a melt quote for that invoice.
	MeltLightningAddressQuote(lightningAddress string, amountMsat Amount) (MeltQuote, error)
	// Get a melt quote using a unified interface for any payment method
	//
	// This method supports bolt11, bolt12, and custom payment methods.
	// For custom methods, you can pass extra JSON data that will be forwarded
	// to the payment processor.
	//
	// # Arguments
	// * `method` - Payment method to use (bolt11, bolt12, or custom)
	// * `request` - Payment request string (invoice, offer, or custom format)
	// * `options` - Optional melt options (MPP, amountless, etc.)
	// * `extra` - Optional JSON string with extra payment-method-specific fields (for custom methods)
	MeltQuote(method PaymentMethod, request string, options *MeltOptions, extra *string) (MeltQuote, error)
	// Mint tokens.
	//
	// This writes newly issued proofs and saga state to the local store while
	// communicating with the mint. Mobile hosts should coordinate it with app
	// lifecycle transitions, using platform background-task support when the
	// operation must finish after backgrounding.
	Mint(quoteId string, amountSplitTarget SplitTarget, spendingConditions *SpendingConditions) ([]Proof, error)
	// Mint blind auth tokens
	MintBlindAuth(amount Amount) ([]Proof, error)
	// Get a mint quote
	MintQuote(paymentMethod PaymentMethod, amount *Amount, description *string, extra *string) (MintQuote, error)
	// Mint tokens using the unified payment-method interface.
	//
	// This writes newly issued proofs and saga state to the local store while
	// communicating with the mint. Mobile hosts should coordinate it with app
	// lifecycle transitions, using platform background-task support when the
	// operation must finish after backgrounding.
	MintUnified(quoteId string, amountSplitTarget SplitTarget, spendingConditions *SpendingConditions) ([]Proof, error)
	// Check and mint any paid but unissued mint quotes.
	//
	// This is useful during startup or recovery after incomplete mint quote flows.
	// It may perform network requests and write newly issued proofs to the wallet store.
	MintUnissuedQuotes() (Amount, error)
	// Get the mint URL
	MintUrl() MintUrl
	// Prepare a melt operation
	//
	// Returns a `PreparedMelt` that can be confirmed or cancelled.
	PrepareMelt(quoteId string) (*PreparedMelt, error)
	// Prepare a melt operation with specific proofs
	//
	// This method allows melting proofs that may not be in the wallet's database,
	// similar to how `receive_proofs` handles external proofs. The proofs will be
	// added to the database and used for the melt operation.
	//
	// # Arguments
	//
	// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
	// * `proofs` - The proofs to melt (can be external proofs not in the wallet's database)
	//
	// # Returns
	//
	// A `PreparedMelt` that can be confirmed or cancelled
	PrepareMeltProofs(quoteId string, proofs []Proof) (*PreparedMelt, error)
	// Prepare a melt operation with specific proofs and additional options
	//
	// Same as `prepare_melt_proofs`, with P2PK/HTLC-locked proofs signed
	// using `options.p2pk_signing_keys` plus any signing keys known to the
	// wallet, and HTLC preimages from `options.preimages` attached, before
	// the proofs are reserved.
	//
	// # Arguments
	//
	// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
	// * `proofs` - The proofs to melt (can be external proofs not in the wallet's database)
	// * `options` - Signing keys, preimages, and metadata for the prepare step
	//
	// # Returns
	//
	// A `PreparedMelt` that can be confirmed or cancelled
	PrepareMeltProofsWithOptions(quoteId string, proofs []Proof, options MeltPrepareOptions) (*PreparedMelt, error)
	// Prepare a melt operation from an encoded token
	//
	// Decodes the token internally (handling keyset state for v2 keysets),
	// extracts proofs, and prepares the melt operation.
	//
	// # Arguments
	//
	// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
	// * `encoded_token` - The encoded token string (cashuA or cashuB format)
	//
	// # Returns
	//
	// A `PreparedMelt` that can be confirmed or cancelled
	PrepareMeltToken(quoteId string, encodedToken string) (*PreparedMelt, error)
	// Prepare a melt operation from an encoded token with additional options
	//
	// Same as `prepare_melt_token`, with locked inputs handled as described
	// in `prepare_melt_proofs_with_options`.
	//
	// # Arguments
	//
	// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
	// * `encoded_token` - The encoded token string (cashuA or cashuB format)
	// * `options` - Signing keys, preimages, and metadata for the prepare step
	//
	// # Returns
	//
	// A `PreparedMelt` that can be confirmed or cancelled
	PrepareMeltTokenWithOptions(quoteId string, encodedToken string, options MeltPrepareOptions) (*PreparedMelt, error)
	// Prepare a NUT-18 payment request so its method and input fees can be reviewed.
	//
	// Call `confirm` or `cancel` on the returned object to complete the flow.
	PreparePayRequest(paymentRequest *PaymentRequest, customAmount *Amount) (*PreparedPaymentRequest, error)
	// Prepare a send operation
	PrepareSend(amount Amount, options SendOptions) (*PreparedSend, error)
	// Fetch available onchain melt quote options.
	//
	// Each returned quote represents one selectable confirmation target/fee reserve.
	// Pass the chosen quote to `select_onchain_melt_quote`, then prepare and confirm
	// the returned quote ID through the normal melt flow.
	QuoteOnchainMeltOptions(address string, amount Amount, maxFeeAmount *Amount) ([]MeltQuote, error)
	// Receive tokens.
	//
	// This verifies and persists received proofs in the local store. Mobile
	// hosts should avoid starting it during app background transitions unless
	// background network and storage activity is intended.
	Receive(token *Token, options ReceiveOptions) (Amount, error)
	// Receive proofs directly.
	//
	// This verifies and persists received proofs in the local store. Mobile
	// hosts should avoid starting it during app background transitions unless
	// background network and storage activity is intended.
	ReceiveProofs(proofs []Proof, options ReceiveOptions, memo *string, token *string) (Amount, error)
	// Recover from incomplete wallet sagas after a crash
	//
	// Handles interrupted swap, send, receive, issue, and melt operations. Requires
	// network access to the mint for states that need external status checks.
	// Recovery writes saga, proof, quote, and transaction updates to the local
	// store. Mobile hosts should run it only when background network and storage
	// activity is acceptable, or wrap it in platform background-task support
	// such as iOS `beginBackgroundTask`.
	RecoverIncompleteSagas() (RecoveryReport, error)
	// Refresh access token using the stored refresh token
	RefreshAccessToken() error
	// Restore wallet from seed
	Restore() (Restored, error)
	// Restore wallet from seed with custom NUT-13 options
	RestoreWithOpts(opts Nut13Options) (Restored, error)
	// Revert a transaction
	RevertTransaction(id TransactionId) error
	// Revoke a pending send operation
	RevokeSend(operationId string) (Amount, error)
	// Persist the selected onchain melt quote before preparing it.
	SelectOnchainMeltQuote(quote MeltQuote) (MeltQuote, error)
	// Set Clear Auth Token (CAT) for authentication
	SetCat(cat string) error
	// Set metadata cache TTL (time-to-live) in seconds
	//
	// Controls how long cached mint metadata (keysets, keys, mint info) is considered fresh
	// before requiring a refresh from the mint server.
	//
	// # Arguments
	//
	// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires and is always used.
	//
	// # Example
	//
	// ```ignore
	// // Cache expires after 5 minutes
	// wallet.set_metadata_cache_ttl(Some(300));
	//
	// // Cache never expires (default)
	// wallet.set_metadata_cache_ttl(None);
	// ```
	SetMetadataCacheTtl(ttlSecs *uint64)
	// Change client-side request rate limiting on this wallet.
	//
	// A new wallet starts with whatever `WalletConfig.rate_limit` selected. Use
	// this to disable pacing, restore the default, or set a custom burst and
	// refill. It takes effect immediately and covers every host the wallet's
	// limiter paces, so it reaches the main and blind-auth clients as well as
	// any third-party host their transport reaches. For a wallet built through
	// a wallet repository the limiter is shared, so the change is
	// repository-wide.
	//
	// Returns an error if a `Custom` value has a zero field.
	SetRateLimit(rateLimit RateLimit) error
	// Set refresh token for authentication
	SetRefreshToken(refreshToken string) error
	// Subscribe to wallet events.
	//
	// The returned subscription may keep polling or receiving network events
	// until it is dropped or closed. Mobile hosts should cancel, drop, or stop
	// waiting on subscriptions during app background transitions when
	// background network or storage activity is not desired.
	Subscribe(params SubscribeParams) (*ActiveSubscription, error)
	// Subscribe to mint quote state updates
	//
	// Convenience method that creates a subscription to receive notifications
	// when any of the given mint quotes change state (e.g., Unpaid → Paid → Issued).
	//
	// Use `recv()` on the returned `ActiveSubscription` to receive updates as
	// `NotificationPayload::MintQuoteUpdate`.
	// The returned subscription may keep polling or receiving network events
	// until it is dropped or closed. Mobile hosts should cancel, drop, or stop
	// waiting on subscriptions during app background transitions when
	// background network or storage activity is not desired.
	//
	// All quote IDs must belong to the same payment method.
	//
	// # Arguments
	// * `quote_ids` - The IDs of the mint quotes to monitor
	// * `payment_method` - The payment method of the quotes
	SubscribeMintQuoteState(quoteIds []string, paymentMethod PaymentMethod) (*ActiveSubscription, error)
	// Swap proofs
	Swap(amount *Amount, amountSplitTarget SplitTarget, inputProofs []Proof, spendingConditions *SpendingConditions, includeFees bool) (*[]Proof, error)
	// Reconcile the wallet with NpubCash by resolving quotes missing locally
	//
	// Fetches all quote IDs from the NpubCash server, resolves full data for
	// quotes missing from the local store, and refreshes NUT-20 lock
	// provenance for quotes already known locally. Unlike
	// `claim_npubcash_quotes`, this does not mint anything; it only
	// reconciles the local quote store with the server.
	//
	// Returns the quotes that were missing locally and have now been added.
	//
	// Requires NpubCash to be enabled on this wallet first.
	SyncMissingNpubcashQuotes() ([]MintQuote, error)
	// Get total balance
	TotalBalance() (Amount, error)
	// Get total pending balance
	TotalPendingBalance() (Amount, error)
	// Get total reserved balance
	TotalReservedBalance() (Amount, error)
	// Get the currency unit
	Unit() CurrencyUnit
	// Verify token DLEQ proofs
	VerifyTokenDleq(token *Token) error
}

// FFI-compatible wallet.
//
// Wallet methods can write to the configured local store while they perform
// mint, receive, recovery, subscription, and status operations. Mobile host
// apps own platform lifecycle handling around these calls: pause or cancel work
// when moving to the background unless background network and storage activity
// is intended, and use platform facilities such as iOS `beginBackgroundTask`
// when an operation must finish after a lifecycle transition.
type Wallet struct {
	ffiObject FfiObject
}

// Create a new wallet.
//
// The returned wallet uses `store` for local state. FFI wallet methods may
// write to that store later, so mobile host apps are responsible for
// choosing durable storage locations and coordinating lifecycle transitions
// around wallet calls. For example, use iOS `beginBackgroundTask` if a
// write-capable operation must continue after the app backgrounds.
//
// Accepts a `WalletStore` which can be:
// - `Sqlite { path }` — built-in Rust SQLite backend
// - `Postgres { url }` — built-in Rust Postgres backend
// - `Custom { db }` — foreign-language implementation of `WalletDatabase`
func NewWallet(mintUrl string, unit CurrencyUnit, mnemonic string, store WalletStore, config WalletConfig) (*Wallet, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_wallet_new(FfiConverterStringINSTANCE.Lower(mintUrl), FfiConverterCurrencyUnitINSTANCE.Lower(unit), FfiConverterStringINSTANCE.Lower(mnemonic), FfiConverterWalletStoreINSTANCE.Lower(store), FfiConverterWalletConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Wallet
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the active keyset with the lowest fees
func (_self *Wallet) ActiveKeyset() (KeySet, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) KeySet {
			return FfiConverterKeySetINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_active_keyset(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Calculate fee for a given number of proofs with the specified keyset
func (_self *Wallet) CalculateFee(proofCount uint32, keysetId string) (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_calculate_fee(
			_pointer, FfiConverterUint32INSTANCE.Lower(proofCount), FfiConverterStringINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check all pending proofs and return the total amount still pending
//
// This function checks orphaned pending proofs (not managed by active sagas)
// with the mint and marks spent proofs accordingly.
// It may perform network requests and write proof-state updates to the
// local store, so mobile hosts should coordinate it with app lifecycle
// transitions.
func (_self *Wallet) CheckAllPendingProofs() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_all_pending_proofs(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check melt quote status and attempt to complete any in-progress saga.
func (_self *Wallet) CheckMeltQuoteStatus(quoteId string) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_melt_quote_status(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check a mint quote status from the mint.
//
// Calls `GET /v1/mint/quote/{method}/{quote_id}` per NUT-04.
// Updates local store with current state from mint.
// If there was a crashed mid-mint (pending saga), attempts to complete it.
// Does NOT mint tokens directly - use mint() for that.
// This may perform network requests and write recovery/status updates to
// the local store, so mobile hosts should coordinate it with app lifecycle
// transitions.
//
// **Note:** The mint quote must be known to the wallet (stored locally) for this
// function to work. If the quote is not stored locally, use `fetch_mint_quote`
// instead.
func (_self *Wallet) CheckMintQuote(quoteId string) (MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintQuote {
			return FfiConverterMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check a mint quote status from the mint.
//
// Calls `GET /v1/mint/quote/{method}/{quote_id}` per NUT-04.
// Updates local store with current state from mint.
// If there was a crashed mid-mint (pending saga), attempts to complete it.
// Does NOT mint tokens directly - use mint() for that.
// This may perform network requests and write recovery/status updates to
// the local store, so mobile hosts should coordinate it with app lifecycle
// transitions.
//
// **Note:** The mint quote must be known to the wallet (stored locally) for this
// function to work. If the quote is not stored locally, use `fetch_mint_quote`
// instead.
//
// # Errors
// Returns an error for a mismatched response quote ID before changing local state.
func (_self *Wallet) CheckMintQuoteStatus(quoteId string) (MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintQuote {
			return FfiConverterMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_mint_quote_status(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check if proofs are spent
func (_self *Wallet) CheckProofsSpent(proofs []Proof) ([]bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []bool {
			return FfiConverterSequenceBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_proofs_spent(
			_pointer, FfiConverterSequenceProofINSTANCE.Lower(proofs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check status of a pending send operation
func (_self *Wallet) CheckSendStatus(operationId string) (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.int8_t {
			res := C.ffi_cdk_ffi_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_check_send_status(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_i8(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Claim all pending NpubCash quotes
//
// Syncs quotes from the NpubCash server (including reconciliation of
// quotes missing locally) and mints every paid NpubCash quote that has
// not been issued yet. Only quotes attributable to the wallet's NpubCash
// accounts are claimed; unrelated mint quotes created through normal
// wallet flows are left untouched. Mints that advertise NUT-29 are
// claimed with batch minting automatically; other mints fall back to
// individual minting.
//
// Returns the total amount minted across all claimed quotes.
func (_self *Wallet) ClaimNpubcashQuotes() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_claim_npubcash_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create quotes for transferring the maximum amount allowed by the source
// balance and both mints' advertised BOLT11 limits.
//
// The returned input fee assumes all currently unspent source proofs are
// used. Prepare the returned melt quote with those proofs and confirm it
// with `skip_swap` to preserve that accounting.
//
// This search may create multiple quote pairs at the remote mints. Only the
// returned pair is persisted locally; unused remote quotes cannot be
// cancelled and remain until they expire.
func (_self *Wallet) CrossMintTransferQuoteMax(targetWallet *Wallet) (CrossMintTransferQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) CrossMintTransferQuote {
			return FfiConverterCrossMintTransferQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_cross_mint_transfer_quote_max(
			_pointer, FfiConverterWalletINSTANCE.Lower(targetWallet)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Enable NpubCash integration for this wallet
//
// Derives the NpubCash Nostr keys from the wallet seed, creates the
// API client, sets this wallet's mint URL on the server, and enables
// NUT-20 quote locking for newly created quotes. The integration is
// only activated once locking has been enabled and confirmed.
//
// Returns an error if quote locking cannot be established — for
// example when the configured mint does not support NUT-20.
func (_self *Wallet) EnableNpubcash(npubcashUrl string) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_enable_npubcash(
			_pointer, FfiConverterStringINSTANCE.Lower(npubcashUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get mint info from mint
func (_self *Wallet) FetchMintInfo() (*MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MintInfo {
			return FfiConverterOptionalMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_fetch_mint_info(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch a mint quote from the mint and store it locally.
//
// This performs network I/O and writes the fetched quote to the local store.
// Mobile hosts should avoid starting it during app background transitions
// unless background network and storage activity is intended.
//
// Works with all payment methods (Bolt11, Bolt12, and custom payment methods).
//
// # Arguments
// * `quote_id` - The ID of the quote to fetch
// * `payment_method` - The payment method for the quote. Required if the quote
// is not already stored locally. If the quote exists locally, the stored
// payment method will be used and this parameter is ignored.
//
// # Errors
// Returns an error for a mismatched response quote ID before storing the quote.
func (_self *Wallet) FetchMintQuote(quoteId string, paymentMethod *PaymentMethod) (MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintQuote {
			return FfiConverterMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_fetch_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterOptionalPaymentMethodINSTANCE.Lower(paymentMethod)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Finalize pending melt operations for this wallet.
func (_self *Wallet) FinalizePendingMelts() ([]FinalizedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []FinalizedMelt {
			return FfiConverterSequenceFinalizedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_finalize_pending_melts(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Wait until the rate-limit budget this wallet drew down has been stored.
//
// Await this before dropping the wallet on shutdown. Without it,
// persistence is best effort and a rebuild can outrun the detached writer,
// so the rebuilt wallet starts with a full burst against the mint's cap.
func (_self *Wallet) FlushRateLimits() {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_flush_rate_limits(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

}

// Get fee for count of proofs in a keyset
func (_self *Wallet) GetKeysetCountFee(keysetId string, count uint64) (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_keyset_count_fee(
			_pointer, FfiConverterStringINSTANCE.Lower(keysetId), FfiConverterUint64INSTANCE.Lower(count)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get fees and amounts for all keysets
func (_self *Wallet) GetKeysetFeesAndAmounts() (map[string]FeeAndAmounts, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) map[string]FeeAndAmounts {
			return FfiConverterMapStringFeeAndAmountsINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_keyset_fees_and_amounts(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get fees and amounts for a specific keyset
func (_self *Wallet) GetKeysetFeesAndAmountsById(keysetId string) (FeeAndAmounts, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) FeeAndAmounts {
			return FfiConverterFeeAndAmountsINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_keyset_fees_and_amounts_by_id(
			_pointer, FfiConverterStringINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get fees for a specific keyset ID
func (_self *Wallet) GetKeysetFeesById(keysetId string) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) uint64 {
			return FfiConverterUint64INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_keyset_fees_by_id(
			_pointer, FfiConverterStringINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch this wallet's NpubCash account settings
//
// Returns the configured mint URL and whether quote locking is enabled.
// Requires NpubCash to be enabled on this wallet first.
func (_self *Wallet) GetNpubcashUserInfo() (NpubCashUserResponse, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) NpubCashUserResponse {
			return FfiConverterNpubCashUserResponseINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_npubcash_user_info(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get all pending send operations
func (_self *Wallet) GetPendingSends() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []string {
			return FfiConverterSequenceStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_pending_sends(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get proofs by states
func (_self *Wallet) GetProofsByStates(states []ProofState) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Proof {
			return FfiConverterSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_proofs_by_states(
			_pointer, FfiConverterSequenceProofStateINSTANCE.Lower(states)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get proofs for a transaction by transaction ID
//
// This retrieves all proofs associated with a transaction by looking up
// the transaction's Y values and fetching the corresponding proofs.
func (_self *Wallet) GetProofsForTransaction(id TransactionId) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Proof {
			return FfiConverterSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_proofs_for_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get transaction by ID
func (_self *Wallet) GetTransaction(id TransactionId) (*Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *Transaction {
			return FfiConverterOptionalTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get unspent auth proofs
func (_self *Wallet) GetUnspentAuthProofs() ([]AuthProof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []AuthProof {
			return FfiConverterSequenceAuthProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_get_unspent_auth_proofs(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Whether this wallet's requests are being paced right now.
//
// Also false when the wallet paces nothing at all, which is the case
// [`Wallet::set_rate_limit`] silently ignores.
func (_self *Wallet) IsRateLimited() bool {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_wallet_is_rate_limited(
			_pointer, _uniffiStatus)
	}))
}

// Get a single keyset by ID
func (_self *Wallet) Keyset(keysetId string) (KeySet, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) KeySet {
			return FfiConverterKeySetINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_keyset(
			_pointer, FfiConverterStringINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get all keysets for this wallet's unit
func (_self *Wallet) Keysets(policy *KeysetLoadPolicy) ([]KeySet, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []KeySet {
			return FfiConverterSequenceKeySetINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_keysets(
			_pointer, FfiConverterOptionalKeysetLoadPolicyINSTANCE.Lower(policy)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// List transactions
func (_self *Wallet) ListTransactions(direction *TransactionDirection) ([]Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Transaction {
			return FfiConverterSequenceTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_list_transactions(
			_pointer, FfiConverterOptionalTransactionDirectionINSTANCE.Lower(direction)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Load mint info
//
// This will get mint info from cache if it is fresh
func (_self *Wallet) LoadMintInfo() (MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintInfo {
			return FfiConverterMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_load_mint_info(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a quote for a BIP353 melt
//
// This method resolves a BIP353 address (e.g., "alice@example.com") to a Bitcoin
// payment instruction, requires a BOLT12 offer, and then creates a melt quote for it.
//
// The `network` parameter controls which on-chain address prefixes are accepted
// in the resolved URI.
func (_self *Wallet) MeltBip353Quote(bip353Address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_melt_bip353_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(bip353Address), FfiConverterAmountINSTANCE.Lower(amountMsat), FfiConverterBitcoinNetworkINSTANCE.Lower(network)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a quote for a human-readable address melt
//
// This method accepts a human-readable address that could be either a BIP353 address
// or a Lightning address. It intelligently determines which to try based on mint support:
//
// 1. If the mint supports Bolt12, it tries BIP353 first
// 2. Falls back to Lightning address only if BIP353 resolution fails
// 3. If BIP353 resolves but has no usable BOLT12 offer, it does NOT fall back
// 4. If the mint doesn't support Bolt12, it tries Lightning address directly
//
// The `network` parameter is forwarded to the BIP353 resolver for on-chain address
// validation in the resolved URI.
func (_self *Wallet) MeltHumanReadable(address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_melt_human_readable(
			_pointer, FfiConverterStringINSTANCE.Lower(address), FfiConverterAmountINSTANCE.Lower(amountMsat), FfiConverterBitcoinNetworkINSTANCE.Lower(network)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a quote for a human-readable address melt
//
// Accepts a human-readable address that could be either a BIP353 address
// or a Lightning address. Tries BIP353 first if mint supports Bolt12,
// falls back to Lightning address.
func (_self *Wallet) MeltHumanReadableQuote(address string, amountMsat Amount, network BitcoinNetwork) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_melt_human_readable_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(address), FfiConverterAmountINSTANCE.Lower(amountMsat), FfiConverterBitcoinNetworkINSTANCE.Lower(network)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a quote for a Lightning address melt
//
// This method resolves a Lightning address (e.g., "alice@example.com") to a Lightning invoice
// and then creates a melt quote for that invoice.
func (_self *Wallet) MeltLightningAddressQuote(lightningAddress string, amountMsat Amount) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_melt_lightning_address_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(lightningAddress), FfiConverterAmountINSTANCE.Lower(amountMsat)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a melt quote using a unified interface for any payment method
//
// This method supports bolt11, bolt12, and custom payment methods.
// For custom methods, you can pass extra JSON data that will be forwarded
// to the payment processor.
//
// # Arguments
// * `method` - Payment method to use (bolt11, bolt12, or custom)
// * `request` - Payment request string (invoice, offer, or custom format)
// * `options` - Optional melt options (MPP, amountless, etc.)
// * `extra` - Optional JSON string with extra payment-method-specific fields (for custom methods)
func (_self *Wallet) MeltQuote(method PaymentMethod, request string, options *MeltOptions, extra *string) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_melt_quote(
			_pointer, FfiConverterPaymentMethodINSTANCE.Lower(method), FfiConverterStringINSTANCE.Lower(request), FfiConverterOptionalMeltOptionsINSTANCE.Lower(options), FfiConverterOptionalStringINSTANCE.Lower(extra)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Mint tokens.
//
// This writes newly issued proofs and saga state to the local store while
// communicating with the mint. Mobile hosts should coordinate it with app
// lifecycle transitions, using platform background-task support when the
// operation must finish after backgrounding.
func (_self *Wallet) Mint(quoteId string, amountSplitTarget SplitTarget, spendingConditions *SpendingConditions) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Proof {
			return FfiConverterSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_mint(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterSplitTargetINSTANCE.Lower(amountSplitTarget), FfiConverterOptionalSpendingConditionsINSTANCE.Lower(spendingConditions)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Mint blind auth tokens
func (_self *Wallet) MintBlindAuth(amount Amount) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Proof {
			return FfiConverterSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_mint_blind_auth(
			_pointer, FfiConverterAmountINSTANCE.Lower(amount)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a mint quote
func (_self *Wallet) MintQuote(paymentMethod PaymentMethod, amount *Amount, description *string, extra *string) (MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintQuote {
			return FfiConverterMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_mint_quote(
			_pointer, FfiConverterPaymentMethodINSTANCE.Lower(paymentMethod), FfiConverterOptionalAmountINSTANCE.Lower(amount), FfiConverterOptionalStringINSTANCE.Lower(description), FfiConverterOptionalStringINSTANCE.Lower(extra)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Mint tokens using the unified payment-method interface.
//
// This writes newly issued proofs and saga state to the local store while
// communicating with the mint. Mobile hosts should coordinate it with app
// lifecycle transitions, using platform background-task support when the
// operation must finish after backgrounding.
func (_self *Wallet) MintUnified(quoteId string, amountSplitTarget SplitTarget, spendingConditions *SpendingConditions) ([]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Proof {
			return FfiConverterSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_mint_unified(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterSplitTargetINSTANCE.Lower(amountSplitTarget), FfiConverterOptionalSpendingConditionsINSTANCE.Lower(spendingConditions)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check and mint any paid but unissued mint quotes.
//
// This is useful during startup or recovery after incomplete mint quote flows.
// It may perform network requests and write newly issued proofs to the wallet store.
func (_self *Wallet) MintUnissuedQuotes() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_mint_unissued_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the mint URL
func (_self *Wallet) MintUrl() MintUrl {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMintUrlINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_wallet_mint_url(
				_pointer, _uniffiStatus),
		}
	}))
}

// Prepare a melt operation
//
// Returns a `PreparedMelt` that can be confirmed or cancelled.
func (_self *Wallet) PrepareMelt(quoteId string) (*PreparedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedMelt {
			return FfiConverterPreparedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_melt(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a melt operation with specific proofs
//
// This method allows melting proofs that may not be in the wallet's database,
// similar to how `receive_proofs` handles external proofs. The proofs will be
// added to the database and used for the melt operation.
//
// # Arguments
//
// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
// * `proofs` - The proofs to melt (can be external proofs not in the wallet's database)
//
// # Returns
//
// A `PreparedMelt` that can be confirmed or cancelled
func (_self *Wallet) PrepareMeltProofs(quoteId string, proofs []Proof) (*PreparedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedMelt {
			return FfiConverterPreparedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_melt_proofs(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterSequenceProofINSTANCE.Lower(proofs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a melt operation with specific proofs and additional options
//
// Same as `prepare_melt_proofs`, with P2PK/HTLC-locked proofs signed
// using `options.p2pk_signing_keys` plus any signing keys known to the
// wallet, and HTLC preimages from `options.preimages` attached, before
// the proofs are reserved.
//
// # Arguments
//
// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
// * `proofs` - The proofs to melt (can be external proofs not in the wallet's database)
// * `options` - Signing keys, preimages, and metadata for the prepare step
//
// # Returns
//
// A `PreparedMelt` that can be confirmed or cancelled
func (_self *Wallet) PrepareMeltProofsWithOptions(quoteId string, proofs []Proof, options MeltPrepareOptions) (*PreparedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedMelt {
			return FfiConverterPreparedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_melt_proofs_with_options(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterSequenceProofINSTANCE.Lower(proofs), FfiConverterMeltPrepareOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a melt operation from an encoded token
//
// Decodes the token internally (handling keyset state for v2 keysets),
// extracts proofs, and prepares the melt operation.
//
// # Arguments
//
// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
// * `encoded_token` - The encoded token string (cashuA or cashuB format)
//
// # Returns
//
// A `PreparedMelt` that can be confirmed or cancelled
func (_self *Wallet) PrepareMeltToken(quoteId string, encodedToken string) (*PreparedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedMelt {
			return FfiConverterPreparedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_melt_token(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(encodedToken)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a melt operation from an encoded token with additional options
//
// Same as `prepare_melt_token`, with locked inputs handled as described
// in `prepare_melt_proofs_with_options`.
//
// # Arguments
//
// * `quote_id` - The melt quote ID (obtained from `melt_quote`)
// * `encoded_token` - The encoded token string (cashuA or cashuB format)
// * `options` - Signing keys, preimages, and metadata for the prepare step
//
// # Returns
//
// A `PreparedMelt` that can be confirmed or cancelled
func (_self *Wallet) PrepareMeltTokenWithOptions(quoteId string, encodedToken string, options MeltPrepareOptions) (*PreparedMelt, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedMelt {
			return FfiConverterPreparedMeltINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_melt_token_with_options(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(encodedToken), FfiConverterMeltPrepareOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a NUT-18 payment request so its method and input fees can be reviewed.
//
// Call `confirm` or `cancel` on the returned object to complete the flow.
func (_self *Wallet) PreparePayRequest(paymentRequest *PaymentRequest, customAmount *Amount) (*PreparedPaymentRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedPaymentRequest {
			return FfiConverterPreparedPaymentRequestINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_pay_request(
			_pointer, FfiConverterPaymentRequestINSTANCE.Lower(paymentRequest), FfiConverterOptionalAmountINSTANCE.Lower(customAmount)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Prepare a send operation
func (_self *Wallet) PrepareSend(amount Amount, options SendOptions) (*PreparedSend, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *PreparedSend {
			return FfiConverterPreparedSendINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_prepare_send(
			_pointer, FfiConverterAmountINSTANCE.Lower(amount), FfiConverterSendOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch available onchain melt quote options.
//
// Each returned quote represents one selectable confirmation target/fee reserve.
// Pass the chosen quote to `select_onchain_melt_quote`, then prepare and confirm
// the returned quote ID through the normal melt flow.
func (_self *Wallet) QuoteOnchainMeltOptions(address string, amount Amount, maxFeeAmount *Amount) ([]MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MeltQuote {
			return FfiConverterSequenceMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_quote_onchain_melt_options(
			_pointer, FfiConverterStringINSTANCE.Lower(address), FfiConverterAmountINSTANCE.Lower(amount), FfiConverterOptionalAmountINSTANCE.Lower(maxFeeAmount)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Receive tokens.
//
// This verifies and persists received proofs in the local store. Mobile
// hosts should avoid starting it during app background transitions unless
// background network and storage activity is intended.
func (_self *Wallet) Receive(token *Token, options ReceiveOptions) (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_receive(
			_pointer, FfiConverterTokenINSTANCE.Lower(token), FfiConverterReceiveOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Receive proofs directly.
//
// This verifies and persists received proofs in the local store. Mobile
// hosts should avoid starting it during app background transitions unless
// background network and storage activity is intended.
func (_self *Wallet) ReceiveProofs(proofs []Proof, options ReceiveOptions, memo *string, token *string) (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_receive_proofs(
			_pointer, FfiConverterSequenceProofINSTANCE.Lower(proofs), FfiConverterReceiveOptionsINSTANCE.Lower(options), FfiConverterOptionalStringINSTANCE.Lower(memo), FfiConverterOptionalStringINSTANCE.Lower(token)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Recover from incomplete wallet sagas after a crash
//
// Handles interrupted swap, send, receive, issue, and melt operations. Requires
// network access to the mint for states that need external status checks.
// Recovery writes saga, proof, quote, and transaction updates to the local
// store. Mobile hosts should run it only when background network and storage
// activity is acceptable, or wrap it in platform background-task support
// such as iOS `beginBackgroundTask`.
func (_self *Wallet) RecoverIncompleteSagas() (RecoveryReport, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) RecoveryReport {
			return FfiConverterRecoveryReportINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_recover_incomplete_sagas(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Refresh access token using the stored refresh token
func (_self *Wallet) RefreshAccessToken() error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_refresh_access_token(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Restore wallet from seed
func (_self *Wallet) Restore() (Restored, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Restored {
			return FfiConverterRestoredINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_restore(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Restore wallet from seed with custom NUT-13 options
func (_self *Wallet) RestoreWithOpts(opts Nut13Options) (Restored, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Restored {
			return FfiConverterRestoredINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_restore_with_opts(
			_pointer, FfiConverterNut13OptionsINSTANCE.Lower(opts)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Revert a transaction
func (_self *Wallet) RevertTransaction(id TransactionId) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_revert_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Revoke a pending send operation
func (_self *Wallet) RevokeSend(operationId string) (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_revoke_send(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Persist the selected onchain melt quote before preparing it.
func (_self *Wallet) SelectOnchainMeltQuote(quote MeltQuote) (MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MeltQuote {
			return FfiConverterMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_select_onchain_melt_quote(
			_pointer, FfiConverterMeltQuoteINSTANCE.Lower(quote)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Set Clear Auth Token (CAT) for authentication
func (_self *Wallet) SetCat(cat string) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_set_cat(
			_pointer, FfiConverterStringINSTANCE.Lower(cat)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Set metadata cache TTL (time-to-live) in seconds
//
// Controls how long cached mint metadata (keysets, keys, mint info) is considered fresh
// before requiring a refresh from the mint server.
//
// # Arguments
//
// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires and is always used.
//
// # Example
//
// ```ignore
// // Cache expires after 5 minutes
// wallet.set_metadata_cache_ttl(Some(300));
//
// // Cache never expires (default)
// wallet.set_metadata_cache_ttl(None);
// ```
func (_self *Wallet) SetMetadataCacheTtl(ttlSecs *uint64) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_wallet_set_metadata_cache_ttl(
			_pointer, FfiConverterOptionalUint64INSTANCE.Lower(ttlSecs), _uniffiStatus)
		return false
	})
}

// Change client-side request rate limiting on this wallet.
//
// A new wallet starts with whatever `WalletConfig.rate_limit` selected. Use
// this to disable pacing, restore the default, or set a custom burst and
// refill. It takes effect immediately and covers every host the wallet's
// limiter paces, so it reaches the main and blind-auth clients as well as
// any third-party host their transport reaches. For a wallet built through
// a wallet repository the limiter is shared, so the change is
// repository-wide.
//
// Returns an error if a `Custom` value has a zero field.
func (_self *Wallet) SetRateLimit(rateLimit RateLimit) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_wallet_set_rate_limit(
			_pointer, FfiConverterRateLimitINSTANCE.Lower(rateLimit), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set refresh token for authentication
func (_self *Wallet) SetRefreshToken(refreshToken string) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_set_refresh_token(
			_pointer, FfiConverterStringINSTANCE.Lower(refreshToken)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Subscribe to wallet events.
//
// The returned subscription may keep polling or receiving network events
// until it is dropped or closed. Mobile hosts should cancel, drop, or stop
// waiting on subscriptions during app background transitions when
// background network or storage activity is not desired.
func (_self *Wallet) Subscribe(params SubscribeParams) (*ActiveSubscription, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *ActiveSubscription {
			return FfiConverterActiveSubscriptionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_subscribe(
			_pointer, FfiConverterSubscribeParamsINSTANCE.Lower(params)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Subscribe to mint quote state updates
//
// Convenience method that creates a subscription to receive notifications
// when any of the given mint quotes change state (e.g., Unpaid → Paid → Issued).
//
// Use `recv()` on the returned `ActiveSubscription` to receive updates as
// `NotificationPayload::MintQuoteUpdate`.
// The returned subscription may keep polling or receiving network events
// until it is dropped or closed. Mobile hosts should cancel, drop, or stop
// waiting on subscriptions during app background transitions when
// background network or storage activity is not desired.
//
// All quote IDs must belong to the same payment method.
//
// # Arguments
// * `quote_ids` - The IDs of the mint quotes to monitor
// * `payment_method` - The payment method of the quotes
func (_self *Wallet) SubscribeMintQuoteState(quoteIds []string, paymentMethod PaymentMethod) (*ActiveSubscription, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *ActiveSubscription {
			return FfiConverterActiveSubscriptionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_subscribe_mint_quote_state(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(quoteIds), FfiConverterPaymentMethodINSTANCE.Lower(paymentMethod)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Swap proofs
func (_self *Wallet) Swap(amount *Amount, amountSplitTarget SplitTarget, inputProofs []Proof, spendingConditions *SpendingConditions, includeFees bool) (*[]Proof, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *[]Proof {
			return FfiConverterOptionalSequenceProofINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_swap(
			_pointer, FfiConverterOptionalAmountINSTANCE.Lower(amount), FfiConverterSplitTargetINSTANCE.Lower(amountSplitTarget), FfiConverterSequenceProofINSTANCE.Lower(inputProofs), FfiConverterOptionalSpendingConditionsINSTANCE.Lower(spendingConditions), FfiConverterBoolINSTANCE.Lower(includeFees)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Reconcile the wallet with NpubCash by resolving quotes missing locally
//
// Fetches all quote IDs from the NpubCash server, resolves full data for
// quotes missing from the local store, and refreshes NUT-20 lock
// provenance for quotes already known locally. Unlike
// `claim_npubcash_quotes`, this does not mint anything; it only
// reconciles the local quote store with the server.
//
// Returns the quotes that were missing locally and have now been added.
//
// Requires NpubCash to be enabled on this wallet first.
func (_self *Wallet) SyncMissingNpubcashQuotes() ([]MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MintQuote {
			return FfiConverterSequenceMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_sync_missing_npubcash_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get total balance
func (_self *Wallet) TotalBalance() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_total_balance(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get total pending balance
func (_self *Wallet) TotalPendingBalance() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_total_pending_balance(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get total reserved balance
func (_self *Wallet) TotalReservedBalance() (Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Amount {
			return FfiConverterAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_wallet_total_reserved_balance(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the currency unit
func (_self *Wallet) Unit() CurrencyUnit {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCurrencyUnitINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_wallet_unit(
				_pointer, _uniffiStatus),
		}
	}))
}

// Verify token DLEQ proofs
func (_self *Wallet) VerifyTokenDleq(token *Token) error {
	_pointer := _self.ffiObject.incrementPointer("*Wallet")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_wallet_verify_token_dleq(
			_pointer, FfiConverterTokenINSTANCE.Lower(token)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *Wallet) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWallet struct{}

var FfiConverterWalletINSTANCE = FfiConverterWallet{}

func (c FfiConverterWallet) Lift(handle C.uint64_t) *Wallet {
	result := &Wallet{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_wallet(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_wallet(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Wallet).Destroy)
	return result
}

func (c FfiConverterWallet) Read(reader io.Reader) *Wallet {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterWallet) Lower(value *Wallet) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Wallet")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterWallet) Write(writer io.Writer, value *Wallet) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalWallet(handle uint64) *Wallet {
	return FfiConverterWalletINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalWallet(value *Wallet) uint64 {
	return uint64(FfiConverterWalletINSTANCE.Lower(value))
}

type FfiDestroyerWallet struct{}

func (_ FfiDestroyerWallet) Destroy(value *Wallet) {
	value.Destroy()
}

// FFI-compatible wallet database trait with all read and write operations
// This trait mirrors the CDK WalletDatabase trait structure
type WalletDatabase interface {
	// Get mint from storage
	GetMint(mintUrl MintUrl) (*MintInfo, error)
	// Get all mints from storage
	GetMints() (map[MintUrl]*MintInfo, error)
	// Get mint keysets for mint url
	GetMintKeysets(mintUrl MintUrl) (*[]KeySetInfo, error)
	// Get mint keyset by id
	GetKeysetById(keysetId Id) (*KeySetInfo, error)
	// Get mint quote from storage
	GetMintQuote(quoteId string) (*MintQuote, error)
	// Get mint quotes from storage
	GetMintQuotes() ([]MintQuote, error)
	// Get unissued mint quotes from storage
	// Returns bolt11 quotes where nothing has been issued yet (amount_issued = 0) and all bolt12 quotes.
	GetUnissuedMintQuotes() ([]MintQuote, error)
	// Get melt quote from storage
	GetMeltQuote(quoteId string) (*MeltQuote, error)
	// Get melt quotes from storage
	GetMeltQuotes() ([]MeltQuote, error)
	// Get Keys from storage
	GetKeys(id Id) (*Keys, error)
	// Get proofs from storage
	GetProofs(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState, spendingConditions *[]SpendingConditions) ([]ProofInfo, error)
	// Get proofs by Y values
	GetProofsByYs(ys []PublicKey) ([]ProofInfo, error)
	// Get balance efficiently using SQL aggregation
	GetBalance(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState) (uint64, error)
	// Get transaction from storage
	GetTransaction(transactionId TransactionId) (*Transaction, error)
	// List transactions from storage
	ListTransactions(mintUrl *MintUrl, direction *TransactionDirection, unit *CurrencyUnit) ([]Transaction, error)
	// Read a value from the KV store
	KvRead(primaryNamespace string, secondaryNamespace string, key string) (*[]byte, error)
	// List keys in a namespace
	KvList(primaryNamespace string, secondaryNamespace string) ([]string, error)
	// Add P2PK signing key to storage
	AddP2pkKey(pubkey PublicKey, derivationPath string, derivationIndex uint32) error
	// Get P2PK signing key from storage
	GetP2pkKey(pubkey PublicKey) (*P2pkSigningKey, error)
	// List all P2PK signing keys from storage
	ListP2pkKeys() ([]P2pkSigningKey, error)
	// Get the latest P2PK signing key (most recently created)
	LatestP2pk() (*P2pkSigningKey, error)
	// Write a value to the KV store
	KvWrite(primaryNamespace string, secondaryNamespace string, key string, value []byte) error
	// Remove a value from the KV store
	KvRemove(primaryNamespace string, secondaryNamespace string, key string) error
	// Update the proofs in storage by adding new proofs or removing proofs by their Y value
	UpdateProofs(added []ProofInfo, removedYs []PublicKey) error
	// Update proofs state in storage
	UpdateProofsState(ys []PublicKey, state ProofState) error
	// Add transaction to storage
	AddTransaction(transaction Transaction) error
	// Remove transaction from storage
	RemoveTransaction(transactionId TransactionId) error
	// Update mint url
	UpdateMintUrl(oldMintUrl MintUrl, newMintUrl MintUrl) error
	// Atomically increment Keyset counter and return new value
	IncrementKeysetCounter(keysetId Id, count uint32) (uint32, error)
	// Atomically increment a namespaced derivation counter and return its new value.
	IncrementDerivationCounter(namespace string, count uint32) (uint32, error)
	// Add Mint to storage
	AddMint(mintUrl MintUrl, mintInfo *MintInfo) error
	// Remove Mint from storage
	RemoveMint(mintUrl MintUrl) error
	// Add mint keyset to storage
	AddMintKeysets(mintUrl MintUrl, keysets []KeySetInfo) error
	// Add mint quote to storage
	AddMintQuote(quote MintQuote) error
	// Remove mint quote from storage
	RemoveMintQuote(quoteId string) error
	// Add melt quote to storage
	AddMeltQuote(quote MeltQuote) error
	// Remove melt quote from storage
	RemoveMeltQuote(quoteId string) error
	// Add Keys to storage
	AddKeys(keyset KeySet) error
	// Remove Keys from storage
	RemoveKeys(id Id) error
	// Add a wallet saga to storage (JSON serialized)
	AddSaga(sagaJson string) error
	// Get a wallet saga by ID (returns JSON serialized)
	GetSaga(id string) (*string, error)
	// Update a wallet saga (JSON serialized) with optimistic locking.
	//
	// Returns `true` if the update succeeded (version matched),
	// `false` if another instance modified the saga first.
	UpdateSaga(sagaJson string) (bool, error)
	// Delete a wallet saga
	DeleteSaga(id string) error
	// Get all incomplete sagas (returns JSON serialized sagas)
	GetIncompleteSagas() ([]string, error)
	// Reserve proofs for an operation
	ReserveProofs(ys []PublicKey, operationId string) error
	// Release live proofs reserved by an operation.
	//
	// Implementations must only change Reserved or Pending proofs owned by
	// `operation_id`; Spent proofs and proofs owned by another operation must
	// be preserved.
	ReleaseProofs(operationId string) error
	// Get proofs reserved by an operation
	GetReservedProofs(operationId string) ([]ProofInfo, error)
	// Reserve a melt quote for an operation
	ReserveMeltQuote(quoteId string, operationId string) error
	// Release a melt quote reserved by an operation
	ReleaseMeltQuote(operationId string) error
	// Reserve a mint quote for an operation
	ReserveMintQuote(quoteId string, operationId string) error
	// Release a mint quote reserved by an operation
	ReleaseMintQuote(operationId string) error
}

// FFI-compatible wallet database trait with all read and write operations
// This trait mirrors the CDK WalletDatabase trait structure
type WalletDatabaseImpl struct {
	ffiObject FfiObject
}

// Get mint from storage
func (_self *WalletDatabaseImpl) GetMint(mintUrl MintUrl) (*MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MintInfo {
			return FfiConverterOptionalMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get all mints from storage
func (_self *WalletDatabaseImpl) GetMints() (map[MintUrl]*MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) map[MintUrl]*MintInfo {
			return FfiConverterMapMintUrlOptionalMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_mints(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get mint keysets for mint url
func (_self *WalletDatabaseImpl) GetMintKeysets(mintUrl MintUrl) (*[]KeySetInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *[]KeySetInfo {
			return FfiConverterOptionalSequenceKeySetInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_mint_keysets(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get mint keyset by id
func (_self *WalletDatabaseImpl) GetKeysetById(keysetId Id) (*KeySetInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *KeySetInfo {
			return FfiConverterOptionalKeySetInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_keyset_by_id(
			_pointer, FfiConverterIdINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get mint quote from storage
func (_self *WalletDatabaseImpl) GetMintQuote(quoteId string) (*MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MintQuote {
			return FfiConverterOptionalMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get mint quotes from storage
func (_self *WalletDatabaseImpl) GetMintQuotes() ([]MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MintQuote {
			return FfiConverterSequenceMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_mint_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get unissued mint quotes from storage
// Returns bolt11 quotes where nothing has been issued yet (amount_issued = 0) and all bolt12 quotes.
func (_self *WalletDatabaseImpl) GetUnissuedMintQuotes() ([]MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MintQuote {
			return FfiConverterSequenceMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_unissued_mint_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get melt quote from storage
func (_self *WalletDatabaseImpl) GetMeltQuote(quoteId string) (*MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MeltQuote {
			return FfiConverterOptionalMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get melt quotes from storage
func (_self *WalletDatabaseImpl) GetMeltQuotes() ([]MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MeltQuote {
			return FfiConverterSequenceMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_melt_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get Keys from storage
func (_self *WalletDatabaseImpl) GetKeys(id Id) (*Keys, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *Keys {
			return FfiConverterOptionalKeysINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_keys(
			_pointer, FfiConverterIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get proofs from storage
func (_self *WalletDatabaseImpl) GetProofs(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState, spendingConditions *[]SpendingConditions) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_proofs(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalSequenceProofStateINSTANCE.Lower(state), FfiConverterOptionalSequenceSpendingConditionsINSTANCE.Lower(spendingConditions)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get proofs by Y values
func (_self *WalletDatabaseImpl) GetProofsByYs(ys []PublicKey) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_proofs_by_ys(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get balance efficiently using SQL aggregation
func (_self *WalletDatabaseImpl) GetBalance(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) uint64 {
			return FfiConverterUint64INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_balance(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalSequenceProofStateINSTANCE.Lower(state)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get transaction from storage
func (_self *WalletDatabaseImpl) GetTransaction(transactionId TransactionId) (*Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *Transaction {
			return FfiConverterOptionalTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(transactionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// List transactions from storage
func (_self *WalletDatabaseImpl) ListTransactions(mintUrl *MintUrl, direction *TransactionDirection, unit *CurrencyUnit) ([]Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Transaction {
			return FfiConverterSequenceTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_list_transactions(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalTransactionDirectionINSTANCE.Lower(direction), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Read a value from the KV store
func (_self *WalletDatabaseImpl) KvRead(primaryNamespace string, secondaryNamespace string, key string) (*[]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *[]byte {
			return FfiConverterOptionalBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_kv_read(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// List keys in a namespace
func (_self *WalletDatabaseImpl) KvList(primaryNamespace string, secondaryNamespace string) ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []string {
			return FfiConverterSequenceStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_kv_list(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Add P2PK signing key to storage
func (_self *WalletDatabaseImpl) AddP2pkKey(pubkey PublicKey, derivationPath string, derivationIndex uint32) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_p2pk_key(
			_pointer, FfiConverterPublicKeyINSTANCE.Lower(pubkey), FfiConverterStringINSTANCE.Lower(derivationPath), FfiConverterUint32INSTANCE.Lower(derivationIndex)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get P2PK signing key from storage
func (_self *WalletDatabaseImpl) GetP2pkKey(pubkey PublicKey) (*P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *P2pkSigningKey {
			return FfiConverterOptionalP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_p2pk_key(
			_pointer, FfiConverterPublicKeyINSTANCE.Lower(pubkey)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// List all P2PK signing keys from storage
func (_self *WalletDatabaseImpl) ListP2pkKeys() ([]P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []P2pkSigningKey {
			return FfiConverterSequenceP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_list_p2pk_keys(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the latest P2PK signing key (most recently created)
func (_self *WalletDatabaseImpl) LatestP2pk() (*P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *P2pkSigningKey {
			return FfiConverterOptionalP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_latest_p2pk(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Write a value to the KV store
func (_self *WalletDatabaseImpl) KvWrite(primaryNamespace string, secondaryNamespace string, key string, value []byte) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_kv_write(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key), FfiConverterBytesINSTANCE.Lower(value)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove a value from the KV store
func (_self *WalletDatabaseImpl) KvRemove(primaryNamespace string, secondaryNamespace string, key string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_kv_remove(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Update the proofs in storage by adding new proofs or removing proofs by their Y value
func (_self *WalletDatabaseImpl) UpdateProofs(added []ProofInfo, removedYs []PublicKey) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_update_proofs(
			_pointer, FfiConverterSequenceProofInfoINSTANCE.Lower(added), FfiConverterSequencePublicKeyINSTANCE.Lower(removedYs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Update proofs state in storage
func (_self *WalletDatabaseImpl) UpdateProofsState(ys []PublicKey, state ProofState) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_update_proofs_state(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys), FfiConverterProofStateINSTANCE.Lower(state)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add transaction to storage
func (_self *WalletDatabaseImpl) AddTransaction(transaction Transaction) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_transaction(
			_pointer, FfiConverterTransactionINSTANCE.Lower(transaction)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove transaction from storage
func (_self *WalletDatabaseImpl) RemoveTransaction(transactionId TransactionId) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_remove_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(transactionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Update mint url
func (_self *WalletDatabaseImpl) UpdateMintUrl(oldMintUrl MintUrl, newMintUrl MintUrl) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_update_mint_url(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(oldMintUrl), FfiConverterMintUrlINSTANCE.Lower(newMintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Atomically increment Keyset counter and return new value
func (_self *WalletDatabaseImpl) IncrementKeysetCounter(keysetId Id, count uint32) (uint32, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint32_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u32(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint32_t) uint32 {
			return FfiConverterUint32INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_increment_keyset_counter(
			_pointer, FfiConverterIdINSTANCE.Lower(keysetId), FfiConverterUint32INSTANCE.Lower(count)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u32(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u32(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Atomically increment a namespaced derivation counter and return its new value.
func (_self *WalletDatabaseImpl) IncrementDerivationCounter(namespace string, count uint32) (uint32, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint32_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u32(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint32_t) uint32 {
			return FfiConverterUint32INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_increment_derivation_counter(
			_pointer, FfiConverterStringINSTANCE.Lower(namespace), FfiConverterUint32INSTANCE.Lower(count)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u32(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u32(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Add Mint to storage
func (_self *WalletDatabaseImpl) AddMint(mintUrl MintUrl, mintInfo *MintInfo) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalMintInfoINSTANCE.Lower(mintInfo)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove Mint from storage
func (_self *WalletDatabaseImpl) RemoveMint(mintUrl MintUrl) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_remove_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add mint keyset to storage
func (_self *WalletDatabaseImpl) AddMintKeysets(mintUrl MintUrl, keysets []KeySetInfo) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_mint_keysets(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterSequenceKeySetInfoINSTANCE.Lower(keysets)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add mint quote to storage
func (_self *WalletDatabaseImpl) AddMintQuote(quote MintQuote) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_mint_quote(
			_pointer, FfiConverterMintQuoteINSTANCE.Lower(quote)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove mint quote from storage
func (_self *WalletDatabaseImpl) RemoveMintQuote(quoteId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_remove_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add melt quote to storage
func (_self *WalletDatabaseImpl) AddMeltQuote(quote MeltQuote) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_melt_quote(
			_pointer, FfiConverterMeltQuoteINSTANCE.Lower(quote)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove melt quote from storage
func (_self *WalletDatabaseImpl) RemoveMeltQuote(quoteId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_remove_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add Keys to storage
func (_self *WalletDatabaseImpl) AddKeys(keyset KeySet) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_keys(
			_pointer, FfiConverterKeySetINSTANCE.Lower(keyset)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Remove Keys from storage
func (_self *WalletDatabaseImpl) RemoveKeys(id Id) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_remove_keys(
			_pointer, FfiConverterIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Add a wallet saga to storage (JSON serialized)
func (_self *WalletDatabaseImpl) AddSaga(sagaJson string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_add_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(sagaJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get a wallet saga by ID (returns JSON serialized)
func (_self *WalletDatabaseImpl) GetSaga(id string) (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Update a wallet saga (JSON serialized) with optimistic locking.
//
// Returns `true` if the update succeeded (version matched),
// `false` if another instance modified the saga first.
func (_self *WalletDatabaseImpl) UpdateSaga(sagaJson string) (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.int8_t {
			res := C.ffi_cdk_ffi_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_update_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(sagaJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_i8(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Delete a wallet saga
func (_self *WalletDatabaseImpl) DeleteSaga(id string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_delete_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get all incomplete sagas (returns JSON serialized sagas)
func (_self *WalletDatabaseImpl) GetIncompleteSagas() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []string {
			return FfiConverterSequenceStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_incomplete_sagas(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Reserve proofs for an operation
func (_self *WalletDatabaseImpl) ReserveProofs(ys []PublicKey, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_reserve_proofs(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Release live proofs reserved by an operation.
//
// Implementations must only change Reserved or Pending proofs owned by
// `operation_id`; Spent proofs and proofs owned by another operation must
// be preserved.
func (_self *WalletDatabaseImpl) ReleaseProofs(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_release_proofs(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Get proofs reserved by an operation
func (_self *WalletDatabaseImpl) GetReservedProofs(operationId string) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletdatabase_get_reserved_proofs(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Reserve a melt quote for an operation
func (_self *WalletDatabaseImpl) ReserveMeltQuote(quoteId string, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_reserve_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Release a melt quote reserved by an operation
func (_self *WalletDatabaseImpl) ReleaseMeltQuote(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_release_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Reserve a mint quote for an operation
func (_self *WalletDatabaseImpl) ReserveMintQuote(quoteId string, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_reserve_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Release a mint quote reserved by an operation
func (_self *WalletDatabaseImpl) ReleaseMintQuote(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("WalletDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletdatabase_release_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}
func (object *WalletDatabaseImpl) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWalletDatabase struct {
	handleMap *concurrentHandleMap[WalletDatabase]
}

var FfiConverterWalletDatabaseINSTANCE = FfiConverterWalletDatabase{
	handleMap: newConcurrentHandleMap[WalletDatabase](),
}

func (c FfiConverterWalletDatabase) Lift(handle C.uint64_t) WalletDatabase {
	if uint64(handle)&1 == 0 {
		// Rust-generated handle (even), construct a new object wrapping the handle
		result := &WalletDatabaseImpl{
			newFfiObject(
				handle,
				func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
					return C.uniffi_cdk_ffi_fn_clone_walletdatabase(handle, status)
				},
				func(handle C.uint64_t, status *C.RustCallStatus) {
					C.uniffi_cdk_ffi_fn_free_walletdatabase(handle, status)
				},
			),
		}
		runtime.SetFinalizer(result, (*WalletDatabaseImpl).Destroy)
		return result
	} else {
		// Go-generated handle (odd), retrieve from the handle map
		val, ok := c.handleMap.tryGet(uint64(handle))
		if !ok {
			panic(fmt.Errorf("no callback in handle map: %d", handle))
		}
		c.handleMap.remove(uint64(handle))
		return val
	}
}

func (c FfiConverterWalletDatabase) Read(reader io.Reader) WalletDatabase {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterWalletDatabase) Lower(value WalletDatabase) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	if val, ok := value.(*WalletDatabaseImpl); ok {
		// Rust-backed object, clone the handle
		handle := val.ffiObject.incrementPointer("WalletDatabase")
		defer val.ffiObject.decrementPointer()
		return handle
	} else {
		// Go-backed object, insert into handle map
		return C.uint64_t(c.handleMap.insert(value))
	}
}

func (c FfiConverterWalletDatabase) Write(writer io.Writer, value WalletDatabase) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalWalletDatabase(handle uint64) WalletDatabase {
	return FfiConverterWalletDatabaseINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalWalletDatabase(value WalletDatabase) uint64 {
	return uint64(FfiConverterWalletDatabaseINSTANCE.Lower(value))
}

type FfiDestroyerWalletDatabase struct{}

func (_ FfiDestroyerWalletDatabase) Destroy(value WalletDatabase) {
	if val, ok := value.(*WalletDatabaseImpl); ok {
		val.Destroy()
	}
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod0
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod0(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMint(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalMintInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod1
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod1(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMints()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterMapMintUrlOptionalMintInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod2
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod2(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMintKeysets(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalSequenceKeySetInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod3
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod3(uniffiHandle C.uint64_t, keysetId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetKeysetById(
				FfiConverterIdINSTANCE.Lift(GoRustBuffer{
					inner: keysetId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalKeySetInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod4
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod4(uniffiHandle C.uint64_t, quoteId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMintQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalMintQuoteINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod5
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod5(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMintQuotes()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceMintQuoteINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod6
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod6(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetUnissuedMintQuotes()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceMintQuoteINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod7
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod7(uniffiHandle C.uint64_t, quoteId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMeltQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalMeltQuoteINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod8
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod8(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetMeltQuotes()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceMeltQuoteINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod9
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod9(uniffiHandle C.uint64_t, id C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetKeys(
				FfiConverterIdINSTANCE.Lift(GoRustBuffer{
					inner: id,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalKeysINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod10
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod10(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, unit C.RustBuffer, state C.RustBuffer, spendingConditions C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetProofs(
				FfiConverterOptionalMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
				FfiConverterOptionalCurrencyUnitINSTANCE.Lift(GoRustBuffer{
					inner: unit,
				}),
				FfiConverterOptionalSequenceProofStateINSTANCE.Lift(GoRustBuffer{
					inner: state,
				}),
				FfiConverterOptionalSequenceSpendingConditionsINSTANCE.Lift(GoRustBuffer{
					inner: spendingConditions,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceProofInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod11
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod11(uniffiHandle C.uint64_t, ys C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetProofsByYs(
				FfiConverterSequencePublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: ys,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceProofInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod12
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod12(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, unit C.RustBuffer, state C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteU64, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultU64, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteU64(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultU64{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetBalance(
				FfiConverterOptionalMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
				FfiConverterOptionalCurrencyUnitINSTANCE.Lift(GoRustBuffer{
					inner: unit,
				}),
				FfiConverterOptionalSequenceProofStateINSTANCE.Lift(GoRustBuffer{
					inner: state,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterUint64INSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod13
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod13(uniffiHandle C.uint64_t, transactionId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetTransaction(
				FfiConverterTransactionIdINSTANCE.Lift(GoRustBuffer{
					inner: transactionId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalTransactionINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod14
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod14(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, direction C.RustBuffer, unit C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.ListTransactions(
				FfiConverterOptionalMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
				FfiConverterOptionalTransactionDirectionINSTANCE.Lift(GoRustBuffer{
					inner: direction,
				}),
				FfiConverterOptionalCurrencyUnitINSTANCE.Lift(GoRustBuffer{
					inner: unit,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceTransactionINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod15
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod15(uniffiHandle C.uint64_t, primaryNamespace C.RustBuffer, secondaryNamespace C.RustBuffer, key C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.KvRead(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: primaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: secondaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: key,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalBytesINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod16
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod16(uniffiHandle C.uint64_t, primaryNamespace C.RustBuffer, secondaryNamespace C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.KvList(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: primaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: secondaryNamespace,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceStringINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod17
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod17(uniffiHandle C.uint64_t, pubkey C.RustBuffer, derivationPath C.RustBuffer, derivationIndex C.uint32_t, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddP2pkKey(
				FfiConverterPublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: pubkey,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: derivationPath,
				}),
				FfiConverterUint32INSTANCE.Lift(derivationIndex),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod18
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod18(uniffiHandle C.uint64_t, pubkey C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetP2pkKey(
				FfiConverterPublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: pubkey,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalP2pkSigningKeyINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod19
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod19(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.ListP2pkKeys()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceP2pkSigningKeyINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod20
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod20(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.LatestP2pk()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalP2pkSigningKeyINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod21
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod21(uniffiHandle C.uint64_t, primaryNamespace C.RustBuffer, secondaryNamespace C.RustBuffer, key C.RustBuffer, value C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.KvWrite(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: primaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: secondaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: key,
				}),
				FfiConverterBytesINSTANCE.Lift(GoRustBuffer{
					inner: value,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod22
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod22(uniffiHandle C.uint64_t, primaryNamespace C.RustBuffer, secondaryNamespace C.RustBuffer, key C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.KvRemove(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: primaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: secondaryNamespace,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: key,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod23
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod23(uniffiHandle C.uint64_t, added C.RustBuffer, removedYs C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.UpdateProofs(
				FfiConverterSequenceProofInfoINSTANCE.Lift(GoRustBuffer{
					inner: added,
				}),
				FfiConverterSequencePublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: removedYs,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod24
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod24(uniffiHandle C.uint64_t, ys C.RustBuffer, state C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.UpdateProofsState(
				FfiConverterSequencePublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: ys,
				}),
				FfiConverterProofStateINSTANCE.Lift(GoRustBuffer{
					inner: state,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod25
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod25(uniffiHandle C.uint64_t, transaction C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddTransaction(
				FfiConverterTransactionINSTANCE.Lift(GoRustBuffer{
					inner: transaction,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod26
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod26(uniffiHandle C.uint64_t, transactionId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.RemoveTransaction(
				FfiConverterTransactionIdINSTANCE.Lift(GoRustBuffer{
					inner: transactionId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod27
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod27(uniffiHandle C.uint64_t, oldMintUrl C.RustBuffer, newMintUrl C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.UpdateMintUrl(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: oldMintUrl,
				}),
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: newMintUrl,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod28
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod28(uniffiHandle C.uint64_t, keysetId C.RustBuffer, count C.uint32_t, uniffiFutureCallback C.UniffiForeignFutureCompleteU32, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultU32, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteU32(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultU32{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.IncrementKeysetCounter(
				FfiConverterIdINSTANCE.Lift(GoRustBuffer{
					inner: keysetId,
				}),
				FfiConverterUint32INSTANCE.Lift(count),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterUint32INSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod29
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod29(uniffiHandle C.uint64_t, namespace C.RustBuffer, count C.uint32_t, uniffiFutureCallback C.UniffiForeignFutureCompleteU32, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultU32, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteU32(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultU32{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.IncrementDerivationCounter(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: namespace,
				}),
				FfiConverterUint32INSTANCE.Lift(count),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterUint32INSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod30
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod30(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, mintInfo C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddMint(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
				FfiConverterOptionalMintInfoINSTANCE.Lift(GoRustBuffer{
					inner: mintInfo,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod31
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod31(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.RemoveMint(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod32
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod32(uniffiHandle C.uint64_t, mintUrl C.RustBuffer, keysets C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddMintKeysets(
				FfiConverterMintUrlINSTANCE.Lift(GoRustBuffer{
					inner: mintUrl,
				}),
				FfiConverterSequenceKeySetInfoINSTANCE.Lift(GoRustBuffer{
					inner: keysets,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod33
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod33(uniffiHandle C.uint64_t, quote C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddMintQuote(
				FfiConverterMintQuoteINSTANCE.Lift(GoRustBuffer{
					inner: quote,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod34
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod34(uniffiHandle C.uint64_t, quoteId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.RemoveMintQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod35
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod35(uniffiHandle C.uint64_t, quote C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddMeltQuote(
				FfiConverterMeltQuoteINSTANCE.Lift(GoRustBuffer{
					inner: quote,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod36
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod36(uniffiHandle C.uint64_t, quoteId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.RemoveMeltQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod37
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod37(uniffiHandle C.uint64_t, keyset C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddKeys(
				FfiConverterKeySetINSTANCE.Lift(GoRustBuffer{
					inner: keyset,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod38
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod38(uniffiHandle C.uint64_t, id C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.RemoveKeys(
				FfiConverterIdINSTANCE.Lift(GoRustBuffer{
					inner: id,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod39
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod39(uniffiHandle C.uint64_t, sagaJson C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.AddSaga(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: sagaJson,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod40
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod40(uniffiHandle C.uint64_t, id C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetSaga(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: id,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterOptionalStringINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod41
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod41(uniffiHandle C.uint64_t, sagaJson C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteI8, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultI8, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteI8(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultI8{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.UpdateSaga(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: sagaJson,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterBoolINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod42
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod42(uniffiHandle C.uint64_t, id C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.DeleteSaga(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: id,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod43
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod43(uniffiHandle C.uint64_t, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetIncompleteSagas()

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceStringINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod44
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod44(uniffiHandle C.uint64_t, ys C.RustBuffer, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReserveProofs(
				FfiConverterSequencePublicKeyINSTANCE.Lift(GoRustBuffer{
					inner: ys,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod45
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod45(uniffiHandle C.uint64_t, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReleaseProofs(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod46
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod46(uniffiHandle C.uint64_t, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteRustBuffer, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultRustBuffer, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteRustBuffer(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultRustBuffer{}
		uniffiOutReturn := &asyncResult.returnValue
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		res, err :=
			uniffiObj.GetReservedProofs(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

		*uniffiOutReturn = FfiConverterSequenceProofInfoINSTANCE.Lower(res)
	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod47
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod47(uniffiHandle C.uint64_t, quoteId C.RustBuffer, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReserveMeltQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod48
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod48(uniffiHandle C.uint64_t, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReleaseMeltQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod49
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod49(uniffiHandle C.uint64_t, quoteId C.RustBuffer, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReserveMintQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: quoteId,
				}),
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod50
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod50(uniffiHandle C.uint64_t, operationId C.RustBuffer, uniffiFutureCallback C.UniffiForeignFutureCompleteVoid, uniffiCallbackData C.uint64_t, uniffiOutDroppedCallback *C.UniffiForeignFutureDroppedCallbackStruct) {
	handle := uint64(uniffiHandle)
	uniffiObj, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}

	result := make(chan C.UniffiForeignFutureResultVoid, 1)
	cancel := make(chan struct{}, 1)
	guardHandle := cgo.NewHandle(cancel)
	*uniffiOutDroppedCallback = C.UniffiForeignFutureDroppedCallbackStruct{
		handle: C.uint64_t(guardHandle),
		free:   C.UniffiForeignFutureDroppedCallback(C.cdkffi_uniffiFreeGorutine),
	}

	// Wait for compleation or cancel
	go func() {
		select {
		case <-cancel:
		case res := <-result:
			C.call_UniffiForeignFutureCompleteVoid(uniffiFutureCallback, uniffiCallbackData, res)
		}
	}()

	// Eval callback asynchroniously
	go func() {
		asyncResult := &C.UniffiForeignFutureResultVoid{}
		callStatus := &asyncResult.callStatus
		defer func() {
			result <- *asyncResult
		}()

		err :=
			uniffiObj.ReleaseMintQuote(
				FfiConverterStringINSTANCE.Lift(GoRustBuffer{
					inner: operationId,
				}),
			)

		if err != nil {
			var actualError *FfiError
			if errors.As(err, &actualError) {
				*callStatus = C.RustCallStatus{
					code:     C.int8_t(uniffiCallbackResultError),
					errorBuf: FfiConverterFfiErrorINSTANCE.Lower(actualError),
				}
			} else {
				*callStatus = C.RustCallStatus{
					code: C.int8_t(uniffiCallbackUnexpectedResultError),
				}
			}
			return
		}

	}()
}

var UniffiVTableCallbackInterfaceWalletDatabaseINSTANCE = C.UniffiVTableCallbackInterfaceWalletDatabase{
	uniffiFree:                 (C.UniffiCallbackInterfaceFree)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseFree),
	uniffiClone:                (C.UniffiCallbackInterfaceClone)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseClone),
	getMint:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod0)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod0),
	getMints:                   (C.UniffiCallbackInterfaceWalletDatabaseMethod1)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod1),
	getMintKeysets:             (C.UniffiCallbackInterfaceWalletDatabaseMethod2)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod2),
	getKeysetById:              (C.UniffiCallbackInterfaceWalletDatabaseMethod3)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod3),
	getMintQuote:               (C.UniffiCallbackInterfaceWalletDatabaseMethod4)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod4),
	getMintQuotes:              (C.UniffiCallbackInterfaceWalletDatabaseMethod5)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod5),
	getUnissuedMintQuotes:      (C.UniffiCallbackInterfaceWalletDatabaseMethod6)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod6),
	getMeltQuote:               (C.UniffiCallbackInterfaceWalletDatabaseMethod7)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod7),
	getMeltQuotes:              (C.UniffiCallbackInterfaceWalletDatabaseMethod8)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod8),
	getKeys:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod9)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod9),
	getProofs:                  (C.UniffiCallbackInterfaceWalletDatabaseMethod10)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod10),
	getProofsByYs:              (C.UniffiCallbackInterfaceWalletDatabaseMethod11)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod11),
	getBalance:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod12)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod12),
	getTransaction:             (C.UniffiCallbackInterfaceWalletDatabaseMethod13)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod13),
	listTransactions:           (C.UniffiCallbackInterfaceWalletDatabaseMethod14)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod14),
	kvRead:                     (C.UniffiCallbackInterfaceWalletDatabaseMethod15)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod15),
	kvList:                     (C.UniffiCallbackInterfaceWalletDatabaseMethod16)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod16),
	addP2pkKey:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod17)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod17),
	getP2pkKey:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod18)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod18),
	listP2pkKeys:               (C.UniffiCallbackInterfaceWalletDatabaseMethod19)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod19),
	latestP2pk:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod20)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod20),
	kvWrite:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod21)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod21),
	kvRemove:                   (C.UniffiCallbackInterfaceWalletDatabaseMethod22)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod22),
	updateProofs:               (C.UniffiCallbackInterfaceWalletDatabaseMethod23)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod23),
	updateProofsState:          (C.UniffiCallbackInterfaceWalletDatabaseMethod24)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod24),
	addTransaction:             (C.UniffiCallbackInterfaceWalletDatabaseMethod25)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod25),
	removeTransaction:          (C.UniffiCallbackInterfaceWalletDatabaseMethod26)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod26),
	updateMintUrl:              (C.UniffiCallbackInterfaceWalletDatabaseMethod27)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod27),
	incrementKeysetCounter:     (C.UniffiCallbackInterfaceWalletDatabaseMethod28)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod28),
	incrementDerivationCounter: (C.UniffiCallbackInterfaceWalletDatabaseMethod29)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod29),
	addMint:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod30)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod30),
	removeMint:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod31)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod31),
	addMintKeysets:             (C.UniffiCallbackInterfaceWalletDatabaseMethod32)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod32),
	addMintQuote:               (C.UniffiCallbackInterfaceWalletDatabaseMethod33)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod33),
	removeMintQuote:            (C.UniffiCallbackInterfaceWalletDatabaseMethod34)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod34),
	addMeltQuote:               (C.UniffiCallbackInterfaceWalletDatabaseMethod35)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod35),
	removeMeltQuote:            (C.UniffiCallbackInterfaceWalletDatabaseMethod36)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod36),
	addKeys:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod37)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod37),
	removeKeys:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod38)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod38),
	addSaga:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod39)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod39),
	getSaga:                    (C.UniffiCallbackInterfaceWalletDatabaseMethod40)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod40),
	updateSaga:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod41)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod41),
	deleteSaga:                 (C.UniffiCallbackInterfaceWalletDatabaseMethod42)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod42),
	getIncompleteSagas:         (C.UniffiCallbackInterfaceWalletDatabaseMethod43)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod43),
	reserveProofs:              (C.UniffiCallbackInterfaceWalletDatabaseMethod44)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod44),
	releaseProofs:              (C.UniffiCallbackInterfaceWalletDatabaseMethod45)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod45),
	getReservedProofs:          (C.UniffiCallbackInterfaceWalletDatabaseMethod46)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod46),
	reserveMeltQuote:           (C.UniffiCallbackInterfaceWalletDatabaseMethod47)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod47),
	releaseMeltQuote:           (C.UniffiCallbackInterfaceWalletDatabaseMethod48)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod48),
	reserveMintQuote:           (C.UniffiCallbackInterfaceWalletDatabaseMethod49)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod49),
	releaseMintQuote:           (C.UniffiCallbackInterfaceWalletDatabaseMethod50)(C.cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseMethod50),
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseFree
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseFree(handle C.uint64_t) {
	FfiConverterWalletDatabaseINSTANCE.handleMap.remove(uint64(handle))
}

//export cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseClone
func cdk_ffi_cgo_dispatchCallbackInterfaceWalletDatabaseClone(handle C.uint64_t) C.uint64_t {
	val, ok := FfiConverterWalletDatabaseINSTANCE.handleMap.tryGet(uint64(handle))
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return C.uint64_t(FfiConverterWalletDatabaseINSTANCE.handleMap.insert(val))
}

func (c FfiConverterWalletDatabase) register() {
	C.uniffi_cdk_ffi_fn_init_callback_vtable_walletdatabase(&UniffiVTableCallbackInterfaceWalletDatabaseINSTANCE)
}

// FFI-compatible WalletRepository
type WalletRepositoryInterface interface {
	// Backup the current mint list to Nostr relays using NUT-27.
	BackupMints(relays []string, options BackupOptions) (BackupResult, error)
	// Add a mint to this WalletRepository
	CreateWallet(mintUrl MintUrl, unit *CurrencyUnit, targetProofCount *uint32) error
	// Fetch the NUT-27 mint backup without adding mints to the repository.
	FetchMintBackup(relays []string, options RestoreOptions) (MintBackup, error)
	// Wait until the rate-limit budgets drawn down by every wallet in this
	// repository have been handed to storage.
	//
	// Await this before dropping the repository on shutdown. Without it,
	// persistence is best effort and a rebuild can outrun the detached
	// writer, so every rebuilt wallet starts with a full burst against the
	// mint's rate cap.
	FlushRateLimits()
	// Get wallet balances for all mints
	GetBalances() (map[WalletKey]Amount, error)
	// Get the wallet for a mint URL and unit, creating it if it does not exist
	//
	// Unlike `create_wallet`, an existing wallet is returned untouched: its
	// configuration is not replaced.
	GetOrCreateWallet(mintUrl MintUrl, unit CurrencyUnit, targetProofCount *uint32) (*Wallet, error)
	// Get token data, including the expected redemption fee, without redeeming it.
	GetTokenData(token *Token) (TokenData, error)
	// Get a specific wallet from WalletRepository by mint URL
	//
	// Returns an error if no wallet exists for the given mint URL.
	GetWallet(mintUrl MintUrl, unit CurrencyUnit) (*Wallet, error)
	// Get all wallets from WalletRepository
	GetWallets() []*Wallet
	// Check if mint is in wallet
	HasMint(mintUrl MintUrl) bool
	// Whether this repository is pacing requests right now.
	//
	// Wallets reached through a proxy or Tor are built with a custom client,
	// so they report false even while this is true.
	IsRateLimited() bool
	// Get the NUT-27 mint backup public key as hex.
	MintBackupPublicKey() (string, error)
	// Remove mint from WalletRepository
	RemoveWallet(mintUrl MintUrl, currencyUnit CurrencyUnit) error
	// Restore the mint list from Nostr relays using NUT-27.
	RestoreMints(relays []string, addMints bool, options RestoreOptions) (RestoreResult, error)
	// Set metadata cache TTL (time-to-live) in seconds for all mints
	//
	// Controls how long cached mint metadata is considered fresh for all mints
	// in this WalletRepository.
	//
	// # Arguments
	//
	// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires for any mint.
	SetMetadataCacheTtlForAllMints(ttlSecs *uint64)
	// Set metadata cache TTL (time-to-live) in seconds for a specific mint
	//
	// Controls how long cached mint metadata (keysets, keys, mint info) is considered fresh
	// before requiring a refresh from the mint server for a specific mint.
	//
	// # Arguments
	//
	// * `mint_url` - The mint URL to set the TTL for
	// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires.
	SetMetadataCacheTtlForMint(mintUrl MintUrl, ttlSecs *uint64) error
	// Change client-side request rate limiting for every wallet here.
	//
	// Pacing is repository-wide because one limiter is shared, which is why
	// `create_wallet` and `get_or_create_wallet` take no rate limit: a
	// per-wallet value would silently reconfigure its siblings. Repository
	// construction makes no network requests, so calling this immediately
	// after `new` is equivalent to configuring it through `new_with_config`.
	//
	// Returns an error if a `Custom` value has a zero field.
	SetRateLimit(rateLimit RateLimit) error
}

// FFI-compatible WalletRepository
type WalletRepository struct {
	ffiObject FfiObject
}

// Create a new WalletRepository from locally persisted wallet state.
//
// Construction does not make network requests to configured mints.
//
// Accepts a `WalletStore` which can be:
// - `Sqlite { path }` — built-in Rust SQLite backend
// - `Postgres { url }` — built-in Rust Postgres backend
// - `Custom { db }` — foreign-language implementation of `WalletDatabase`
func NewWalletRepository(mnemonic string, store WalletStore) (*WalletRepository, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_walletrepository_new(FfiConverterStringINSTANCE.Lower(mnemonic), FfiConverterWalletStoreINSTANCE.Lower(store), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WalletRepository
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletRepositoryINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new WalletRepository with proxy and rate-limit configuration.
//
// Construction restores locally persisted wallet state without making
// network requests to configured mints.
func WalletRepositoryNewWithConfig(mnemonic string, store WalletStore, config WalletRepositoryConfig) (*WalletRepository, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_walletrepository_new_with_config(FfiConverterStringINSTANCE.Lower(mnemonic), FfiConverterWalletStoreINSTANCE.Lower(store), FfiConverterWalletRepositoryConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WalletRepository
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletRepositoryINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a new WalletRepository with proxy configuration.
//
// Construction restores locally persisted wallet state without making
// network requests to configured mints. The proxy is used by subsequent
// mint operations.
func WalletRepositoryNewWithProxy(mnemonic string, store WalletStore, proxyUrl string) (*WalletRepository, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_walletrepository_new_with_proxy(FfiConverterStringINSTANCE.Lower(mnemonic), FfiConverterWalletStoreINSTANCE.Lower(store), FfiConverterStringINSTANCE.Lower(proxyUrl), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WalletRepository
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletRepositoryINSTANCE.Lift(_uniffiRV), nil
	}
}

// Backup the current mint list to Nostr relays using NUT-27.
func (_self *WalletRepository) BackupMints(relays []string, options BackupOptions) (BackupResult, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) BackupResult {
			return FfiConverterBackupResultINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_backup_mints(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterBackupOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Add a mint to this WalletRepository
func (_self *WalletRepository) CreateWallet(mintUrl MintUrl, unit *CurrencyUnit, targetProofCount *uint32) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletrepository_create_wallet(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalUint32INSTANCE.Lower(targetProofCount)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Fetch the NUT-27 mint backup without adding mints to the repository.
func (_self *WalletRepository) FetchMintBackup(relays []string, options RestoreOptions) (MintBackup, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) MintBackup {
			return FfiConverterMintBackupINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_fetch_mint_backup(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterRestoreOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Wait until the rate-limit budgets drawn down by every wallet in this
// repository have been handed to storage.
//
// Await this before dropping the repository on shutdown. Without it,
// persistence is best effort and a rebuild can outrun the detached
// writer, so every rebuilt wallet starts with a full burst against the
// mint's rate cap.
func (_self *WalletRepository) FlushRateLimits() {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletrepository_flush_rate_limits(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

}

// Get wallet balances for all mints
func (_self *WalletRepository) GetBalances() (map[WalletKey]Amount, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) map[WalletKey]Amount {
			return FfiConverterMapWalletKeyAmountINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_get_balances(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get the wallet for a mint URL and unit, creating it if it does not exist
//
// Unlike `create_wallet`, an existing wallet is returned untouched: its
// configuration is not replaced.
func (_self *WalletRepository) GetOrCreateWallet(mintUrl MintUrl, unit CurrencyUnit, targetProofCount *uint32) (*Wallet, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *Wallet {
			return FfiConverterWalletINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_get_or_create_wallet(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalUint32INSTANCE.Lower(targetProofCount)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get token data, including the expected redemption fee, without redeeming it.
func (_self *WalletRepository) GetTokenData(token *Token) (TokenData, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) TokenData {
			return FfiConverterTokenDataINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_get_token_data(
			_pointer, FfiConverterTokenINSTANCE.Lower(token)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get a specific wallet from WalletRepository by mint URL
//
// Returns an error if no wallet exists for the given mint URL.
func (_self *WalletRepository) GetWallet(mintUrl MintUrl, unit CurrencyUnit) (*Wallet, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *Wallet {
			return FfiConverterWalletINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_get_wallet(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterCurrencyUnitINSTANCE.Lower(unit)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Get all wallets from WalletRepository
func (_self *WalletRepository) GetWallets() []*Wallet {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []*Wallet {
			return FfiConverterSequenceWalletINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_get_wallets(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	return res
}

// Check if mint is in wallet
func (_self *WalletRepository) HasMint(mintUrl MintUrl) bool {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, _ := uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.int8_t {
			res := C.ffi_cdk_ffi_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_has_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_i8(handle)
		},
	)

	return res
}

// Whether this repository is pacing requests right now.
//
// Wallets reached through a proxy or Tor are built with a custom client,
// so they report false even while this is true.
func (_self *WalletRepository) IsRateLimited() bool {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_method_walletrepository_is_rate_limited(
			_pointer, _uniffiStatus)
	}))
}

// Get the NUT-27 mint backup public key as hex.
func (_self *WalletRepository) MintBackupPublicKey() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_method_walletrepository_mint_backup_public_key(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Remove mint from WalletRepository
func (_self *WalletRepository) RemoveWallet(mintUrl MintUrl, currencyUnit CurrencyUnit) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletrepository_remove_wallet(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterCurrencyUnitINSTANCE.Lower(currencyUnit)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Restore the mint list from Nostr relays using NUT-27.
func (_self *WalletRepository) RestoreMints(relays []string, addMints bool, options RestoreOptions) (RestoreResult, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) RestoreResult {
			return FfiConverterRestoreResultINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletrepository_restore_mints(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(relays), FfiConverterBoolINSTANCE.Lower(addMints), FfiConverterRestoreOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Set metadata cache TTL (time-to-live) in seconds for all mints
//
// Controls how long cached mint metadata is considered fresh for all mints
// in this WalletRepository.
//
// # Arguments
//
// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires for any mint.
func (_self *WalletRepository) SetMetadataCacheTtlForAllMints(ttlSecs *uint64) {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync[error](
		nil,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletrepository_set_metadata_cache_ttl_for_all_mints(
			_pointer, FfiConverterOptionalUint64INSTANCE.Lower(ttlSecs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

}

// Set metadata cache TTL (time-to-live) in seconds for a specific mint
//
// Controls how long cached mint metadata (keysets, keys, mint info) is considered fresh
// before requiring a refresh from the mint server for a specific mint.
//
// # Arguments
//
// * `mint_url` - The mint URL to set the TTL for
// * `ttl_secs` - Optional TTL in seconds. If None, cache never expires.
func (_self *WalletRepository) SetMetadataCacheTtlForMint(mintUrl MintUrl, ttlSecs *uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletrepository_set_metadata_cache_ttl_for_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalUint64INSTANCE.Lower(ttlSecs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Change client-side request rate limiting for every wallet here.
//
// Pacing is repository-wide because one limiter is shared, which is why
// `create_wallet` and `get_or_create_wallet` take no rate limit: a
// per-wallet value would silently reconfigure its siblings. Repository
// construction makes no network requests, so calling this immediately
// after `new` is equivalent to configuring it through `new_with_config`.
//
// Returns an error if a `Custom` value has a zero field.
func (_self *WalletRepository) SetRateLimit(rateLimit RateLimit) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletRepository")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_method_walletrepository_set_rate_limit(
			_pointer, FfiConverterRateLimitINSTANCE.Lower(rateLimit), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *WalletRepository) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWalletRepository struct{}

var FfiConverterWalletRepositoryINSTANCE = FfiConverterWalletRepository{}

func (c FfiConverterWalletRepository) Lift(handle C.uint64_t) *WalletRepository {
	result := &WalletRepository{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_walletrepository(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_walletrepository(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*WalletRepository).Destroy)
	return result
}

func (c FfiConverterWalletRepository) Read(reader io.Reader) *WalletRepository {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterWalletRepository) Lower(value *WalletRepository) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*WalletRepository")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterWalletRepository) Write(writer io.Writer, value *WalletRepository) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalWalletRepository(handle uint64) *WalletRepository {
	return FfiConverterWalletRepositoryINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalWalletRepository(value *WalletRepository) uint64 {
	return uint64(FfiConverterWalletRepositoryINSTANCE.Lower(value))
}

type FfiDestroyerWalletRepository struct{}

func (_ FfiDestroyerWalletRepository) Destroy(value *WalletRepository) {
	value.Destroy()
}

// FFI-compatible SQLite wallet database.
//
// Wallet methods can write to this database from FFI calls that mint, receive,
// recover, subscribe, or check quote/proof state. Mobile host apps own
// lifecycle handling for the database file: choose a durable app-owned path,
// avoid interrupting writes during background transitions, and use platform
// facilities such as iOS `beginBackgroundTask` when an operation must finish
// after backgrounding.
type WalletSqliteDatabaseInterface interface {
	AddKeys(keyset KeySet) error
	AddMeltQuote(quote MeltQuote) error
	AddMint(mintUrl MintUrl, mintInfo *MintInfo) error
	AddMintKeysets(mintUrl MintUrl, keysets []KeySetInfo) error
	AddMintQuote(quote MintQuote) error
	AddP2pkKey(pubkey PublicKey, derivationPath string, derivationIndex uint32) error
	AddSaga(sagaJson string) error
	AddTransaction(transaction Transaction) error
	DeleteSaga(id string) error
	GetBalance(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState) (uint64, error)
	GetIncompleteSagas() ([]string, error)
	GetKeys(id Id) (*Keys, error)
	GetKeysetById(keysetId Id) (*KeySetInfo, error)
	GetMeltQuote(quoteId string) (*MeltQuote, error)
	GetMeltQuotes() ([]MeltQuote, error)
	GetMint(mintUrl MintUrl) (*MintInfo, error)
	GetMintKeysets(mintUrl MintUrl) (*[]KeySetInfo, error)
	GetMintQuote(quoteId string) (*MintQuote, error)
	GetMintQuotes() ([]MintQuote, error)
	GetMints() (map[MintUrl]*MintInfo, error)
	GetP2pkKey(pubkey PublicKey) (*P2pkSigningKey, error)
	GetProofs(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState, spendingConditions *[]SpendingConditions) ([]ProofInfo, error)
	GetProofsByYs(ys []PublicKey) ([]ProofInfo, error)
	GetReservedProofs(operationId string) ([]ProofInfo, error)
	GetSaga(id string) (*string, error)
	GetTransaction(transactionId TransactionId) (*Transaction, error)
	GetUnissuedMintQuotes() ([]MintQuote, error)
	IncrementDerivationCounter(namespace string, count uint32) (uint32, error)
	IncrementKeysetCounter(keysetId Id, count uint32) (uint32, error)
	KvList(primaryNamespace string, secondaryNamespace string) ([]string, error)
	KvRead(primaryNamespace string, secondaryNamespace string, key string) (*[]byte, error)
	KvRemove(primaryNamespace string, secondaryNamespace string, key string) error
	KvWrite(primaryNamespace string, secondaryNamespace string, key string, value []byte) error
	LatestP2pk() (*P2pkSigningKey, error)
	ListP2pkKeys() ([]P2pkSigningKey, error)
	ListTransactions(mintUrl *MintUrl, direction *TransactionDirection, unit *CurrencyUnit) ([]Transaction, error)
	ReleaseMeltQuote(operationId string) error
	ReleaseMintQuote(operationId string) error
	ReleaseProofs(operationId string) error
	RemoveKeys(id Id) error
	RemoveMeltQuote(quoteId string) error
	RemoveMint(mintUrl MintUrl) error
	RemoveMintQuote(quoteId string) error
	RemoveTransaction(transactionId TransactionId) error
	ReserveMeltQuote(quoteId string, operationId string) error
	ReserveMintQuote(quoteId string, operationId string) error
	ReserveProofs(ys []PublicKey, operationId string) error
	UpdateMintUrl(oldMintUrl MintUrl, newMintUrl MintUrl) error
	UpdateProofs(added []ProofInfo, removedYs []PublicKey) error
	UpdateProofsState(ys []PublicKey, state ProofState) error
	UpdateSaga(sagaJson string) (bool, error)
}

// FFI-compatible SQLite wallet database.
//
// Wallet methods can write to this database from FFI calls that mint, receive,
// recover, subscribe, or check quote/proof state. Mobile host apps own
// lifecycle handling for the database file: choose a durable app-owned path,
// avoid interrupting writes during background transitions, and use platform
// facilities such as iOS `beginBackgroundTask` when an operation must finish
// after backgrounding.
type WalletSqliteDatabase struct {
	ffiObject FfiObject
}

// Create a new SQLite wallet database at `file_path`.
//
// Wallet operations may later write to this database. Mobile hosts are
// responsible for choosing a durable file location and coordinating app
// lifecycle transitions around write-capable wallet calls.
func NewWalletSqliteDatabase(filePath string) (*WalletSqliteDatabase, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_walletsqlitedatabase_new(FfiConverterStringINSTANCE.Lower(filePath), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WalletSqliteDatabase
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletSqliteDatabaseINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create an in-memory database
func WalletSqliteDatabaseNewInMemory() (*WalletSqliteDatabase, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_constructor_walletsqlitedatabase_new_in_memory(_uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WalletSqliteDatabase
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletSqliteDatabaseINSTANCE.Lift(_uniffiRV), nil
	}
}

func (_self *WalletSqliteDatabase) AddKeys(keyset KeySet) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_keys(
			_pointer, FfiConverterKeySetINSTANCE.Lower(keyset)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddMeltQuote(quote MeltQuote) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_melt_quote(
			_pointer, FfiConverterMeltQuoteINSTANCE.Lower(quote)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddMint(mintUrl MintUrl, mintInfo *MintInfo) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalMintInfoINSTANCE.Lower(mintInfo)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddMintKeysets(mintUrl MintUrl, keysets []KeySetInfo) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_mint_keysets(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl), FfiConverterSequenceKeySetInfoINSTANCE.Lower(keysets)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddMintQuote(quote MintQuote) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_mint_quote(
			_pointer, FfiConverterMintQuoteINSTANCE.Lower(quote)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddP2pkKey(pubkey PublicKey, derivationPath string, derivationIndex uint32) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_p2pk_key(
			_pointer, FfiConverterPublicKeyINSTANCE.Lower(pubkey), FfiConverterStringINSTANCE.Lower(derivationPath), FfiConverterUint32INSTANCE.Lower(derivationIndex)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddSaga(sagaJson string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(sagaJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) AddTransaction(transaction Transaction) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_add_transaction(
			_pointer, FfiConverterTransactionINSTANCE.Lower(transaction)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) DeleteSaga(id string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_delete_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) GetBalance(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) uint64 {
			return FfiConverterUint64INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_balance(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalSequenceProofStateINSTANCE.Lower(state)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetIncompleteSagas() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []string {
			return FfiConverterSequenceStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_incomplete_sagas(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetKeys(id Id) (*Keys, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *Keys {
			return FfiConverterOptionalKeysINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_keys(
			_pointer, FfiConverterIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetKeysetById(keysetId Id) (*KeySetInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *KeySetInfo {
			return FfiConverterOptionalKeySetInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_keyset_by_id(
			_pointer, FfiConverterIdINSTANCE.Lower(keysetId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMeltQuote(quoteId string) (*MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MeltQuote {
			return FfiConverterOptionalMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMeltQuotes() ([]MeltQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MeltQuote {
			return FfiConverterSequenceMeltQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_melt_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMint(mintUrl MintUrl) (*MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MintInfo {
			return FfiConverterOptionalMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMintKeysets(mintUrl MintUrl) (*[]KeySetInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *[]KeySetInfo {
			return FfiConverterOptionalSequenceKeySetInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_mint_keysets(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMintQuote(quoteId string) (*MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MintQuote {
			return FfiConverterOptionalMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMintQuotes() ([]MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MintQuote {
			return FfiConverterSequenceMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_mint_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetMints() (map[MintUrl]*MintInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) map[MintUrl]*MintInfo {
			return FfiConverterMapMintUrlOptionalMintInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_mints(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetP2pkKey(pubkey PublicKey) (*P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *P2pkSigningKey {
			return FfiConverterOptionalP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_p2pk_key(
			_pointer, FfiConverterPublicKeyINSTANCE.Lower(pubkey)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetProofs(mintUrl *MintUrl, unit *CurrencyUnit, state *[]ProofState, spendingConditions *[]SpendingConditions) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_proofs(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), FfiConverterOptionalSequenceProofStateINSTANCE.Lower(state), FfiConverterOptionalSequenceSpendingConditionsINSTANCE.Lower(spendingConditions)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetProofsByYs(ys []PublicKey) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_proofs_by_ys(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetReservedProofs(operationId string) ([]ProofInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ProofInfo {
			return FfiConverterSequenceProofInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_reserved_proofs(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetSaga(id string) (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetTransaction(transactionId TransactionId) (*Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *Transaction {
			return FfiConverterOptionalTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(transactionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) GetUnissuedMintQuotes() ([]MintQuote, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []MintQuote {
			return FfiConverterSequenceMintQuoteINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_get_unissued_mint_quotes(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) IncrementDerivationCounter(namespace string, count uint32) (uint32, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint32_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u32(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint32_t) uint32 {
			return FfiConverterUint32INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_increment_derivation_counter(
			_pointer, FfiConverterStringINSTANCE.Lower(namespace), FfiConverterUint32INSTANCE.Lower(count)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u32(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u32(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) IncrementKeysetCounter(keysetId Id, count uint32) (uint32, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint32_t {
			res := C.ffi_cdk_ffi_rust_future_complete_u32(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint32_t) uint32 {
			return FfiConverterUint32INSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_increment_keyset_counter(
			_pointer, FfiConverterIdINSTANCE.Lower(keysetId), FfiConverterUint32INSTANCE.Lower(count)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_u32(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_u32(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) KvList(primaryNamespace string, secondaryNamespace string) ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []string {
			return FfiConverterSequenceStringINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_kv_list(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) KvRead(primaryNamespace string, secondaryNamespace string, key string) (*[]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *[]byte {
			return FfiConverterOptionalBytesINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_kv_read(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) KvRemove(primaryNamespace string, secondaryNamespace string, key string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_kv_remove(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) KvWrite(primaryNamespace string, secondaryNamespace string, key string, value []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_kv_write(
			_pointer, FfiConverterStringINSTANCE.Lower(primaryNamespace), FfiConverterStringINSTANCE.Lower(secondaryNamespace), FfiConverterStringINSTANCE.Lower(key), FfiConverterBytesINSTANCE.Lower(value)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) LatestP2pk() (*P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *P2pkSigningKey {
			return FfiConverterOptionalP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_latest_p2pk(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) ListP2pkKeys() ([]P2pkSigningKey, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []P2pkSigningKey {
			return FfiConverterSequenceP2pkSigningKeyINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_list_p2pk_keys(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) ListTransactions(mintUrl *MintUrl, direction *TransactionDirection, unit *CurrencyUnit) ([]Transaction, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []Transaction {
			return FfiConverterSequenceTransactionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_list_transactions(
			_pointer, FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalTransactionDirectionINSTANCE.Lower(direction), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

func (_self *WalletSqliteDatabase) ReleaseMeltQuote(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_release_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) ReleaseMintQuote(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_release_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) ReleaseProofs(operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_release_proofs(
			_pointer, FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) RemoveKeys(id Id) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_remove_keys(
			_pointer, FfiConverterIdINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) RemoveMeltQuote(quoteId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_remove_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) RemoveMint(mintUrl MintUrl) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_remove_mint(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(mintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) RemoveMintQuote(quoteId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_remove_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) RemoveTransaction(transactionId TransactionId) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_remove_transaction(
			_pointer, FfiConverterTransactionIdINSTANCE.Lower(transactionId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) ReserveMeltQuote(quoteId string, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_reserve_melt_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) ReserveMintQuote(quoteId string, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_reserve_mint_quote(
			_pointer, FfiConverterStringINSTANCE.Lower(quoteId), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) ReserveProofs(ys []PublicKey, operationId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_reserve_proofs(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys), FfiConverterStringINSTANCE.Lower(operationId)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) UpdateMintUrl(oldMintUrl MintUrl, newMintUrl MintUrl) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_update_mint_url(
			_pointer, FfiConverterMintUrlINSTANCE.Lower(oldMintUrl), FfiConverterMintUrlINSTANCE.Lower(newMintUrl)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) UpdateProofs(added []ProofInfo, removedYs []PublicKey) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_update_proofs(
			_pointer, FfiConverterSequenceProofInfoINSTANCE.Lower(added), FfiConverterSequencePublicKeyINSTANCE.Lower(removedYs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) UpdateProofsState(ys []PublicKey, state ProofState) error {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_cdk_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_update_proofs_state(
			_pointer, FfiConverterSequencePublicKeyINSTANCE.Lower(ys), FfiConverterProofStateINSTANCE.Lower(state)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

func (_self *WalletSqliteDatabase) UpdateSaga(sagaJson string) (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.int8_t {
			res := C.ffi_cdk_ffi_rust_future_complete_i8(handle, status)
			return res
		},
		// liftFn
		func(ffi C.int8_t) bool {
			return FfiConverterBoolINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_method_walletsqlitedatabase_update_saga(
			_pointer, FfiConverterStringINSTANCE.Lower(sagaJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_i8(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_i8(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *WalletSqliteDatabase) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWalletSqliteDatabase struct{}

var FfiConverterWalletSqliteDatabaseINSTANCE = FfiConverterWalletSqliteDatabase{}

func (c FfiConverterWalletSqliteDatabase) Lift(handle C.uint64_t) *WalletSqliteDatabase {
	result := &WalletSqliteDatabase{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_cdk_ffi_fn_clone_walletsqlitedatabase(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_cdk_ffi_fn_free_walletsqlitedatabase(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*WalletSqliteDatabase).Destroy)
	return result
}

func (c FfiConverterWalletSqliteDatabase) Read(reader io.Reader) *WalletSqliteDatabase {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterWalletSqliteDatabase) Lower(value *WalletSqliteDatabase) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*WalletSqliteDatabase")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterWalletSqliteDatabase) Write(writer io.Writer, value *WalletSqliteDatabase) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalWalletSqliteDatabase(handle uint64) *WalletSqliteDatabase {
	return FfiConverterWalletSqliteDatabaseINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalWalletSqliteDatabase(value *WalletSqliteDatabase) uint64 {
	return uint64(FfiConverterWalletSqliteDatabaseINSTANCE.Lower(value))
}

type FfiDestroyerWalletSqliteDatabase struct{}

func (_ FfiDestroyerWalletSqliteDatabase) Destroy(value *WalletSqliteDatabase) {
	value.Destroy()
}

// FFI-compatible Amount type
type Amount struct {
	Value uint64
}

func (r *Amount) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.Value)
}

type FfiConverterAmount struct{}

var FfiConverterAmountINSTANCE = FfiConverterAmount{}

func (c FfiConverterAmount) Lift(rb RustBufferI) Amount {
	return LiftFromRustBuffer[Amount](c, rb)
}

func (c FfiConverterAmount) Read(reader io.Reader) Amount {
	return Amount{
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterAmount) Lower(value Amount) C.RustBuffer {
	return LowerIntoRustBuffer[Amount](c, value)
}

func (c FfiConverterAmount) LowerExternal(value Amount) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Amount](c, value))
}

func (c FfiConverterAmount) Write(writer io.Writer, value Amount) {
	FfiConverterUint64INSTANCE.Write(writer, value.Value)
}

type FfiDestroyerAmount struct{}

func (_ FfiDestroyerAmount) Destroy(value Amount) {
	value.Destroy()
}

// FFI-compatible AuthProof
type AuthProof struct {
	// Keyset ID
	KeysetId string
	// Secret message
	Secret string
	// Unblinded signature (C)
	C string
	// Y value (hash_to_curve of secret)
	Y string
}

func (r *AuthProof) Destroy() {
	FfiDestroyerString{}.Destroy(r.KeysetId)
	FfiDestroyerString{}.Destroy(r.Secret)
	FfiDestroyerString{}.Destroy(r.C)
	FfiDestroyerString{}.Destroy(r.Y)
}

type FfiConverterAuthProof struct{}

var FfiConverterAuthProofINSTANCE = FfiConverterAuthProof{}

func (c FfiConverterAuthProof) Lift(rb RustBufferI) AuthProof {
	return LiftFromRustBuffer[AuthProof](c, rb)
}

func (c FfiConverterAuthProof) Read(reader io.Reader) AuthProof {
	return AuthProof{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterAuthProof) Lower(value AuthProof) C.RustBuffer {
	return LowerIntoRustBuffer[AuthProof](c, value)
}

func (c FfiConverterAuthProof) LowerExternal(value AuthProof) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[AuthProof](c, value))
}

func (c FfiConverterAuthProof) Write(writer io.Writer, value AuthProof) {
	FfiConverterStringINSTANCE.Write(writer, value.KeysetId)
	FfiConverterStringINSTANCE.Write(writer, value.Secret)
	FfiConverterStringINSTANCE.Write(writer, value.C)
	FfiConverterStringINSTANCE.Write(writer, value.Y)
}

type FfiDestroyerAuthProof struct{}

func (_ FfiDestroyerAuthProof) Destroy(value AuthProof) {
	value.Destroy()
}

// Options for backup operations
type BackupOptions struct {
	// Client name to include in the event tags
	Client *string
}

func (r *BackupOptions) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Client)
}

type FfiConverterBackupOptions struct{}

var FfiConverterBackupOptionsINSTANCE = FfiConverterBackupOptions{}

func (c FfiConverterBackupOptions) Lift(rb RustBufferI) BackupOptions {
	return LiftFromRustBuffer[BackupOptions](c, rb)
}

func (c FfiConverterBackupOptions) Read(reader io.Reader) BackupOptions {
	return BackupOptions{
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterBackupOptions) Lower(value BackupOptions) C.RustBuffer {
	return LowerIntoRustBuffer[BackupOptions](c, value)
}

func (c FfiConverterBackupOptions) LowerExternal(value BackupOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[BackupOptions](c, value))
}

func (c FfiConverterBackupOptions) Write(writer io.Writer, value BackupOptions) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Client)
}

type FfiDestroyerBackupOptions struct{}

func (_ FfiDestroyerBackupOptions) Destroy(value BackupOptions) {
	value.Destroy()
}

// Result of a backup operation
type BackupResult struct {
	// The event ID of the published backup (hex encoded)
	EventId string
	// The public key used for the backup (hex encoded)
	PublicKey string
	// Number of mints backed up
	MintCount uint64
}

func (r *BackupResult) Destroy() {
	FfiDestroyerString{}.Destroy(r.EventId)
	FfiDestroyerString{}.Destroy(r.PublicKey)
	FfiDestroyerUint64{}.Destroy(r.MintCount)
}

type FfiConverterBackupResult struct{}

var FfiConverterBackupResultINSTANCE = FfiConverterBackupResult{}

func (c FfiConverterBackupResult) Lift(rb RustBufferI) BackupResult {
	return LiftFromRustBuffer[BackupResult](c, rb)
}

func (c FfiConverterBackupResult) Read(reader io.Reader) BackupResult {
	return BackupResult{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterBackupResult) Lower(value BackupResult) C.RustBuffer {
	return LowerIntoRustBuffer[BackupResult](c, value)
}

func (c FfiConverterBackupResult) LowerExternal(value BackupResult) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[BackupResult](c, value))
}

func (c FfiConverterBackupResult) Write(writer io.Writer, value BackupResult) {
	FfiConverterStringINSTANCE.Write(writer, value.EventId)
	FfiConverterStringINSTANCE.Write(writer, value.PublicKey)
	FfiConverterUint64INSTANCE.Write(writer, value.MintCount)
}

type FfiDestroyerBackupResult struct{}

func (_ FfiDestroyerBackupResult) Destroy(value BackupResult) {
	value.Destroy()
}

// FFI-compatible BlindAuthSettings (NUT-22)
type BlindAuthSettings struct {
	// Maximum number of blind auth tokens that can be minted per request
	BatMaxMint uint64
	// Protected endpoints requiring blind authentication
	ProtectedEndpoints []ProtectedEndpoint
}

func (r *BlindAuthSettings) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.BatMaxMint)
	FfiDestroyerSequenceProtectedEndpoint{}.Destroy(r.ProtectedEndpoints)
}

type FfiConverterBlindAuthSettings struct{}

var FfiConverterBlindAuthSettingsINSTANCE = FfiConverterBlindAuthSettings{}

func (c FfiConverterBlindAuthSettings) Lift(rb RustBufferI) BlindAuthSettings {
	return LiftFromRustBuffer[BlindAuthSettings](c, rb)
}

func (c FfiConverterBlindAuthSettings) Read(reader io.Reader) BlindAuthSettings {
	return BlindAuthSettings{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterSequenceProtectedEndpointINSTANCE.Read(reader),
	}
}

func (c FfiConverterBlindAuthSettings) Lower(value BlindAuthSettings) C.RustBuffer {
	return LowerIntoRustBuffer[BlindAuthSettings](c, value)
}

func (c FfiConverterBlindAuthSettings) LowerExternal(value BlindAuthSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[BlindAuthSettings](c, value))
}

func (c FfiConverterBlindAuthSettings) Write(writer io.Writer, value BlindAuthSettings) {
	FfiConverterUint64INSTANCE.Write(writer, value.BatMaxMint)
	FfiConverterSequenceProtectedEndpointINSTANCE.Write(writer, value.ProtectedEndpoints)
}

type FfiDestroyerBlindAuthSettings struct{}

func (_ FfiDestroyerBlindAuthSettings) Destroy(value BlindAuthSettings) {
	value.Destroy()
}

// FFI-compatible DLEQ proof for blind signatures
type BlindSignatureDleq struct {
	// e value (hex-encoded SecretKey)
	E string
	// s value (hex-encoded SecretKey)
	S string
}

func (r *BlindSignatureDleq) Destroy() {
	FfiDestroyerString{}.Destroy(r.E)
	FfiDestroyerString{}.Destroy(r.S)
}

type FfiConverterBlindSignatureDleq struct{}

var FfiConverterBlindSignatureDleqINSTANCE = FfiConverterBlindSignatureDleq{}

func (c FfiConverterBlindSignatureDleq) Lift(rb RustBufferI) BlindSignatureDleq {
	return LiftFromRustBuffer[BlindSignatureDleq](c, rb)
}

func (c FfiConverterBlindSignatureDleq) Read(reader io.Reader) BlindSignatureDleq {
	return BlindSignatureDleq{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterBlindSignatureDleq) Lower(value BlindSignatureDleq) C.RustBuffer {
	return LowerIntoRustBuffer[BlindSignatureDleq](c, value)
}

func (c FfiConverterBlindSignatureDleq) LowerExternal(value BlindSignatureDleq) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[BlindSignatureDleq](c, value))
}

func (c FfiConverterBlindSignatureDleq) Write(writer io.Writer, value BlindSignatureDleq) {
	FfiConverterStringINSTANCE.Write(writer, value.E)
	FfiConverterStringINSTANCE.Write(writer, value.S)
}

type FfiDestroyerBlindSignatureDleq struct{}

func (_ FfiDestroyerBlindSignatureDleq) Destroy(value BlindSignatureDleq) {
	value.Destroy()
}

// FFI-compatible ClearAuthSettings (NUT-21)
type ClearAuthSettings struct {
	// OpenID Connect discovery URL
	OpenidDiscovery string
	// OAuth 2.0 client ID
	ClientId string
	// Protected endpoints requiring clear authentication
	ProtectedEndpoints []ProtectedEndpoint
}

func (r *ClearAuthSettings) Destroy() {
	FfiDestroyerString{}.Destroy(r.OpenidDiscovery)
	FfiDestroyerString{}.Destroy(r.ClientId)
	FfiDestroyerSequenceProtectedEndpoint{}.Destroy(r.ProtectedEndpoints)
}

type FfiConverterClearAuthSettings struct{}

var FfiConverterClearAuthSettingsINSTANCE = FfiConverterClearAuthSettings{}

func (c FfiConverterClearAuthSettings) Lift(rb RustBufferI) ClearAuthSettings {
	return LiftFromRustBuffer[ClearAuthSettings](c, rb)
}

func (c FfiConverterClearAuthSettings) Read(reader io.Reader) ClearAuthSettings {
	return ClearAuthSettings{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterSequenceProtectedEndpointINSTANCE.Read(reader),
	}
}

func (c FfiConverterClearAuthSettings) Lower(value ClearAuthSettings) C.RustBuffer {
	return LowerIntoRustBuffer[ClearAuthSettings](c, value)
}

func (c FfiConverterClearAuthSettings) LowerExternal(value ClearAuthSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ClearAuthSettings](c, value))
}

func (c FfiConverterClearAuthSettings) Write(writer io.Writer, value ClearAuthSettings) {
	FfiConverterStringINSTANCE.Write(writer, value.OpenidDiscovery)
	FfiConverterStringINSTANCE.Write(writer, value.ClientId)
	FfiConverterSequenceProtectedEndpointINSTANCE.Write(writer, value.ProtectedEndpoints)
}

type FfiDestroyerClearAuthSettings struct{}

func (_ FfiDestroyerClearAuthSettings) Destroy(value ClearAuthSettings) {
	value.Destroy()
}

// FFI-compatible Conditions (for spending conditions)
type Conditions struct {
	// Unix locktime after which refund keys can be used
	Locktime *uint64
	// Additional Public keys (as hex strings)
	Pubkeys []string
	// Refund keys (as hex strings)
	RefundKeys []string
	// Number of signatures required (default 1)
	NumSigs *uint64
	// Signature flag (0 = SigInputs, 1 = SigAll)
	SigFlag uint8
	// Number of refund signatures required (default 1)
	NumSigsRefund *uint64
}

func (r *Conditions) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.Locktime)
	FfiDestroyerSequenceString{}.Destroy(r.Pubkeys)
	FfiDestroyerSequenceString{}.Destroy(r.RefundKeys)
	FfiDestroyerOptionalUint64{}.Destroy(r.NumSigs)
	FfiDestroyerUint8{}.Destroy(r.SigFlag)
	FfiDestroyerOptionalUint64{}.Destroy(r.NumSigsRefund)
}

type FfiConverterConditions struct{}

var FfiConverterConditionsINSTANCE = FfiConverterConditions{}

func (c FfiConverterConditions) Lift(rb RustBufferI) Conditions {
	return LiftFromRustBuffer[Conditions](c, rb)
}

func (c FfiConverterConditions) Read(reader io.Reader) Conditions {
	return Conditions{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterUint8INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterConditions) Lower(value Conditions) C.RustBuffer {
	return LowerIntoRustBuffer[Conditions](c, value)
}

func (c FfiConverterConditions) LowerExternal(value Conditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Conditions](c, value))
}

func (c FfiConverterConditions) Write(writer io.Writer, value Conditions) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Locktime)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Pubkeys)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.RefundKeys)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.NumSigs)
	FfiConverterUint8INSTANCE.Write(writer, value.SigFlag)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.NumSigsRefund)
}

type FfiDestroyerConditions struct{}

func (_ FfiDestroyerConditions) Destroy(value Conditions) {
	value.Destroy()
}

// FFI-compatible ContactInfo
type ContactInfo struct {
	// Contact Method i.e. nostr
	Method string
	// Contact info i.e. npub...
	Info string
}

func (r *ContactInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Method)
	FfiDestroyerString{}.Destroy(r.Info)
}

type FfiConverterContactInfo struct{}

var FfiConverterContactInfoINSTANCE = FfiConverterContactInfo{}

func (c FfiConverterContactInfo) Lift(rb RustBufferI) ContactInfo {
	return LiftFromRustBuffer[ContactInfo](c, rb)
}

func (c FfiConverterContactInfo) Read(reader io.Reader) ContactInfo {
	return ContactInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterContactInfo) Lower(value ContactInfo) C.RustBuffer {
	return LowerIntoRustBuffer[ContactInfo](c, value)
}

func (c FfiConverterContactInfo) LowerExternal(value ContactInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ContactInfo](c, value))
}

func (c FfiConverterContactInfo) Write(writer io.Writer, value ContactInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Method)
	FfiConverterStringINSTANCE.Write(writer, value.Info)
}

type FfiDestroyerContactInfo struct{}

func (_ FfiDestroyerContactInfo) Destroy(value ContactInfo) {
	value.Destroy()
}

// Parameters for creating a NUT-18 payment request
type CreateRequestParams struct {
	// Optional amount to request (in smallest unit for the currency)
	Amount *uint64
	// Currency unit (e.g., "sat", "msat", "usd")
	Unit string
	// Optional description for the request
	Description *string
	// Optional public keys for P2PK spending conditions (hex-encoded)
	Pubkeys *[]string
	// Required number of signatures for multisig (defaults to 1)
	NumSigs uint64
	// Optional HTLC hash (hex-encoded SHA-256)
	Hash *string
	// Optional HTLC preimage (alternative to hash)
	Preimage *string
	// Transport type: "nostr", "http", or "none"
	Transport string
	// HTTP URL for HTTP transport (required if transport is "http")
	HttpUrl *string
	// Nostr relay URLs (required if transport is "nostr")
	NostrRelays *[]string
	// Optional list of mint URLs the receiver accepts or prefers; `None` emits no mint list
	Mints *[]string
	// Whether the mint list is preferred rather than required
	MintPreferred *bool
	// Payment methods the payer's mint must support, with optional per-method fees
	SupportedMethods []SupportedMethod
}

func (r *CreateRequestParams) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.Amount)
	FfiDestroyerString{}.Destroy(r.Unit)
	FfiDestroyerOptionalString{}.Destroy(r.Description)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Pubkeys)
	FfiDestroyerUint64{}.Destroy(r.NumSigs)
	FfiDestroyerOptionalString{}.Destroy(r.Hash)
	FfiDestroyerOptionalString{}.Destroy(r.Preimage)
	FfiDestroyerString{}.Destroy(r.Transport)
	FfiDestroyerOptionalString{}.Destroy(r.HttpUrl)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.NostrRelays)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Mints)
	FfiDestroyerOptionalBool{}.Destroy(r.MintPreferred)
	FfiDestroyerSequenceSupportedMethod{}.Destroy(r.SupportedMethods)
}

type FfiConverterCreateRequestParams struct{}

var FfiConverterCreateRequestParamsINSTANCE = FfiConverterCreateRequestParams{}

func (c FfiConverterCreateRequestParams) Lift(rb RustBufferI) CreateRequestParams {
	return LiftFromRustBuffer[CreateRequestParams](c, rb)
}

func (c FfiConverterCreateRequestParams) Read(reader io.Reader) CreateRequestParams {
	return CreateRequestParams{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterSequenceSupportedMethodINSTANCE.Read(reader),
	}
}

func (c FfiConverterCreateRequestParams) Lower(value CreateRequestParams) C.RustBuffer {
	return LowerIntoRustBuffer[CreateRequestParams](c, value)
}

func (c FfiConverterCreateRequestParams) LowerExternal(value CreateRequestParams) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[CreateRequestParams](c, value))
}

func (c FfiConverterCreateRequestParams) Write(writer io.Writer, value CreateRequestParams) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Amount)
	FfiConverterStringINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Description)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Pubkeys)
	FfiConverterUint64INSTANCE.Write(writer, value.NumSigs)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Hash)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Preimage)
	FfiConverterStringINSTANCE.Write(writer, value.Transport)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.HttpUrl)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.NostrRelays)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Mints)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.MintPreferred)
	FfiConverterSequenceSupportedMethodINSTANCE.Write(writer, value.SupportedMethods)
}

type FfiDestroyerCreateRequestParams struct{}

func (_ FfiDestroyerCreateRequestParams) Destroy(value CreateRequestParams) {
	value.Destroy()
}

// Result of creating a payment request
//
// Contains the payment request and optionally the Nostr wait info
// if the transport was set to "nostr".
type CreateRequestResult struct {
	// The payment request to share with the payer
	PaymentRequest *PaymentRequest
	// Nostr wait info (present when transport is "nostr")
	NostrWaitInfo **NostrWaitInfo
}

func (r *CreateRequestResult) Destroy() {
	FfiDestroyerPaymentRequest{}.Destroy(r.PaymentRequest)
	FfiDestroyerOptionalNostrWaitInfo{}.Destroy(r.NostrWaitInfo)
}

type FfiConverterCreateRequestResult struct{}

var FfiConverterCreateRequestResultINSTANCE = FfiConverterCreateRequestResult{}

func (c FfiConverterCreateRequestResult) Lift(rb RustBufferI) CreateRequestResult {
	return LiftFromRustBuffer[CreateRequestResult](c, rb)
}

func (c FfiConverterCreateRequestResult) Read(reader io.Reader) CreateRequestResult {
	return CreateRequestResult{
		FfiConverterPaymentRequestINSTANCE.Read(reader),
		FfiConverterOptionalNostrWaitInfoINSTANCE.Read(reader),
	}
}

func (c FfiConverterCreateRequestResult) Lower(value CreateRequestResult) C.RustBuffer {
	return LowerIntoRustBuffer[CreateRequestResult](c, value)
}

func (c FfiConverterCreateRequestResult) LowerExternal(value CreateRequestResult) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[CreateRequestResult](c, value))
}

func (c FfiConverterCreateRequestResult) Write(writer io.Writer, value CreateRequestResult) {
	FfiConverterPaymentRequestINSTANCE.Write(writer, value.PaymentRequest)
	FfiConverterOptionalNostrWaitInfoINSTANCE.Write(writer, value.NostrWaitInfo)
}

type FfiDestroyerCreateRequestResult struct{}

func (_ FfiDestroyerCreateRequestResult) Destroy(value CreateRequestResult) {
	value.Destroy()
}

// FFI-compatible quote for a maximum cross-mint Lightning transfer.
type CrossMintTransferQuote struct {
	// Quote used to receive the Lightning payment at the destination mint.
	MintQuote MintQuote
	// Quote used to pay the destination invoice from the source mint.
	MeltQuote MeltQuote
	// Input fee for spending all currently unspent source proofs.
	InputFee Amount
}

func (r *CrossMintTransferQuote) Destroy() {
	FfiDestroyerMintQuote{}.Destroy(r.MintQuote)
	FfiDestroyerMeltQuote{}.Destroy(r.MeltQuote)
	FfiDestroyerAmount{}.Destroy(r.InputFee)
}

type FfiConverterCrossMintTransferQuote struct{}

var FfiConverterCrossMintTransferQuoteINSTANCE = FfiConverterCrossMintTransferQuote{}

func (c FfiConverterCrossMintTransferQuote) Lift(rb RustBufferI) CrossMintTransferQuote {
	return LiftFromRustBuffer[CrossMintTransferQuote](c, rb)
}

func (c FfiConverterCrossMintTransferQuote) Read(reader io.Reader) CrossMintTransferQuote {
	return CrossMintTransferQuote{
		FfiConverterMintQuoteINSTANCE.Read(reader),
		FfiConverterMeltQuoteINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
	}
}

func (c FfiConverterCrossMintTransferQuote) Lower(value CrossMintTransferQuote) C.RustBuffer {
	return LowerIntoRustBuffer[CrossMintTransferQuote](c, value)
}

func (c FfiConverterCrossMintTransferQuote) LowerExternal(value CrossMintTransferQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[CrossMintTransferQuote](c, value))
}

func (c FfiConverterCrossMintTransferQuote) Write(writer io.Writer, value CrossMintTransferQuote) {
	FfiConverterMintQuoteINSTANCE.Write(writer, value.MintQuote)
	FfiConverterMeltQuoteINSTANCE.Write(writer, value.MeltQuote)
	FfiConverterAmountINSTANCE.Write(writer, value.InputFee)
}

type FfiDestroyerCrossMintTransferQuote struct{}

func (_ FfiDestroyerCrossMintTransferQuote) Destroy(value CrossMintTransferQuote) {
	value.Destroy()
}

// Decoded invoice or offer information
type DecodedInvoice struct {
	// Type of payment request (Bolt11 or Bolt12)
	PaymentType PaymentType
	// Amount in millisatoshis, if specified
	AmountMsat *uint64
	// Expiry timestamp (Unix timestamp), if specified
	Expiry *uint64
	// Description or offer description, if specified
	Description *string
}

func (r *DecodedInvoice) Destroy() {
	FfiDestroyerPaymentType{}.Destroy(r.PaymentType)
	FfiDestroyerOptionalUint64{}.Destroy(r.AmountMsat)
	FfiDestroyerOptionalUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalString{}.Destroy(r.Description)
}

type FfiConverterDecodedInvoice struct{}

var FfiConverterDecodedInvoiceINSTANCE = FfiConverterDecodedInvoice{}

func (c FfiConverterDecodedInvoice) Lift(rb RustBufferI) DecodedInvoice {
	return LiftFromRustBuffer[DecodedInvoice](c, rb)
}

func (c FfiConverterDecodedInvoice) Read(reader io.Reader) DecodedInvoice {
	return DecodedInvoice{
		FfiConverterPaymentTypeINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterDecodedInvoice) Lower(value DecodedInvoice) C.RustBuffer {
	return LowerIntoRustBuffer[DecodedInvoice](c, value)
}

func (c FfiConverterDecodedInvoice) LowerExternal(value DecodedInvoice) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[DecodedInvoice](c, value))
}

func (c FfiConverterDecodedInvoice) Write(writer io.Writer, value DecodedInvoice) {
	FfiConverterPaymentTypeINSTANCE.Write(writer, value.PaymentType)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.AmountMsat)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Description)
}

type FfiDestroyerDecodedInvoice struct{}

func (_ FfiDestroyerDecodedInvoice) Destroy(value DecodedInvoice) {
	value.Destroy()
}

// FFI-compatible FeeAndAmounts
type FeeAndAmounts struct {
	// Input fee per thousand (ppk)
	Fee uint64
	// Available amounts for this keyset
	Amounts []uint64
}

func (r *FeeAndAmounts) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.Fee)
	FfiDestroyerSequenceUint64{}.Destroy(r.Amounts)
}

type FfiConverterFeeAndAmounts struct{}

var FfiConverterFeeAndAmountsINSTANCE = FfiConverterFeeAndAmounts{}

func (c FfiConverterFeeAndAmounts) Lift(rb RustBufferI) FeeAndAmounts {
	return LiftFromRustBuffer[FeeAndAmounts](c, rb)
}

func (c FfiConverterFeeAndAmounts) Read(reader io.Reader) FeeAndAmounts {
	return FeeAndAmounts{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterSequenceUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterFeeAndAmounts) Lower(value FeeAndAmounts) C.RustBuffer {
	return LowerIntoRustBuffer[FeeAndAmounts](c, value)
}

func (c FfiConverterFeeAndAmounts) LowerExternal(value FeeAndAmounts) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[FeeAndAmounts](c, value))
}

func (c FfiConverterFeeAndAmounts) Write(writer io.Writer, value FeeAndAmounts) {
	FfiConverterUint64INSTANCE.Write(writer, value.Fee)
	FfiConverterSequenceUint64INSTANCE.Write(writer, value.Amounts)
}

type FfiDestroyerFeeAndAmounts struct{}

func (_ FfiDestroyerFeeAndAmounts) Destroy(value FeeAndAmounts) {
	value.Destroy()
}

// FFI-compatible FinalizedMelt result
type FinalizedMelt struct {
	QuoteId  string
	State    QuoteState
	Preimage *string
	Change   *[]Proof
	Amount   Amount
	FeePaid  Amount
}

func (r *FinalizedMelt) Destroy() {
	FfiDestroyerString{}.Destroy(r.QuoteId)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerOptionalString{}.Destroy(r.Preimage)
	FfiDestroyerOptionalSequenceProof{}.Destroy(r.Change)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerAmount{}.Destroy(r.FeePaid)
}

type FfiConverterFinalizedMelt struct{}

var FfiConverterFinalizedMeltINSTANCE = FfiConverterFinalizedMelt{}

func (c FfiConverterFinalizedMelt) Lift(rb RustBufferI) FinalizedMelt {
	return LiftFromRustBuffer[FinalizedMelt](c, rb)
}

func (c FfiConverterFinalizedMelt) Read(reader io.Reader) FinalizedMelt {
	return FinalizedMelt{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceProofINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
	}
}

func (c FfiConverterFinalizedMelt) Lower(value FinalizedMelt) C.RustBuffer {
	return LowerIntoRustBuffer[FinalizedMelt](c, value)
}

func (c FfiConverterFinalizedMelt) LowerExternal(value FinalizedMelt) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[FinalizedMelt](c, value))
}

func (c FfiConverterFinalizedMelt) Write(writer io.Writer, value FinalizedMelt) {
	FfiConverterStringINSTANCE.Write(writer, value.QuoteId)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Preimage)
	FfiConverterOptionalSequenceProofINSTANCE.Write(writer, value.Change)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterAmountINSTANCE.Write(writer, value.FeePaid)
}

type FfiDestroyerFinalizedMelt struct{}

func (_ FfiDestroyerFinalizedMelt) Destroy(value FinalizedMelt) {
	value.Destroy()
}

// FFI-compatible Id (for keyset IDs)
type Id struct {
	Hex string
}

func (r *Id) Destroy() {
	FfiDestroyerString{}.Destroy(r.Hex)
}

type FfiConverterId struct{}

var FfiConverterIdINSTANCE = FfiConverterId{}

func (c FfiConverterId) Lift(rb RustBufferI) Id {
	return LiftFromRustBuffer[Id](c, rb)
}

func (c FfiConverterId) Read(reader io.Reader) Id {
	return Id{
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterId) Lower(value Id) C.RustBuffer {
	return LowerIntoRustBuffer[Id](c, value)
}

func (c FfiConverterId) LowerExternal(value Id) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Id](c, value))
}

func (c FfiConverterId) Write(writer io.Writer, value Id) {
	FfiConverterStringINSTANCE.Write(writer, value.Hex)
}

type FfiDestroyerId struct{}

func (_ FfiDestroyerId) Destroy(value Id) {
	value.Destroy()
}

// FFI-compatible KeySet
type KeySet struct {
	// Keyset ID
	Id string
	// Currency unit
	Unit CurrencyUnit
	// Keyset state - indicates whether the mint will sign new outputs with this keyset
	Active *bool
	// Input fee in parts per thousand (ppk) per input spent from this keyset
	InputFeePpk uint64
	// The keys (map of amount to public key hex)
	Keys map[uint64]string
	// Optional expiry timestamp
	FinalExpiry *uint64
}

func (r *KeySet) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalBool{}.Destroy(r.Active)
	FfiDestroyerUint64{}.Destroy(r.InputFeePpk)
	FfiDestroyerMapUint64String{}.Destroy(r.Keys)
	FfiDestroyerOptionalUint64{}.Destroy(r.FinalExpiry)
}

type FfiConverterKeySet struct{}

var FfiConverterKeySetINSTANCE = FfiConverterKeySet{}

func (c FfiConverterKeySet) Lift(rb RustBufferI) KeySet {
	return LiftFromRustBuffer[KeySet](c, rb)
}

func (c FfiConverterKeySet) Read(reader io.Reader) KeySet {
	return KeySet{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterMapUint64StringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterKeySet) Lower(value KeySet) C.RustBuffer {
	return LowerIntoRustBuffer[KeySet](c, value)
}

func (c FfiConverterKeySet) LowerExternal(value KeySet) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[KeySet](c, value))
}

func (c FfiConverterKeySet) Write(writer io.Writer, value KeySet) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Active)
	FfiConverterUint64INSTANCE.Write(writer, value.InputFeePpk)
	FfiConverterMapUint64StringINSTANCE.Write(writer, value.Keys)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.FinalExpiry)
}

type FfiDestroyerKeySet struct{}

func (_ FfiDestroyerKeySet) Destroy(value KeySet) {
	value.Destroy()
}

// FFI-compatible KeySetInfo
type KeySetInfo struct {
	Id     string
	Unit   CurrencyUnit
	Active bool
	// Input fee per thousand (ppk)
	InputFeePpk uint64
}

func (r *KeySetInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerBool{}.Destroy(r.Active)
	FfiDestroyerUint64{}.Destroy(r.InputFeePpk)
}

type FfiConverterKeySetInfo struct{}

var FfiConverterKeySetInfoINSTANCE = FfiConverterKeySetInfo{}

func (c FfiConverterKeySetInfo) Lift(rb RustBufferI) KeySetInfo {
	return LiftFromRustBuffer[KeySetInfo](c, rb)
}

func (c FfiConverterKeySetInfo) Read(reader io.Reader) KeySetInfo {
	return KeySetInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterKeySetInfo) Lower(value KeySetInfo) C.RustBuffer {
	return LowerIntoRustBuffer[KeySetInfo](c, value)
}

func (c FfiConverterKeySetInfo) LowerExternal(value KeySetInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[KeySetInfo](c, value))
}

func (c FfiConverterKeySetInfo) Write(writer io.Writer, value KeySetInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterBoolINSTANCE.Write(writer, value.Active)
	FfiConverterUint64INSTANCE.Write(writer, value.InputFeePpk)
}

type FfiDestroyerKeySetInfo struct{}

func (_ FfiDestroyerKeySetInfo) Destroy(value KeySetInfo) {
	value.Destroy()
}

// FFI-compatible Keys
type Keys struct {
	// Map of amount to public key hex
	Keys map[uint64]string
}

func (r *Keys) Destroy() {
	FfiDestroyerMapUint64String{}.Destroy(r.Keys)
}

type FfiConverterKeys struct{}

var FfiConverterKeysINSTANCE = FfiConverterKeys{}

func (c FfiConverterKeys) Lift(rb RustBufferI) Keys {
	return LiftFromRustBuffer[Keys](c, rb)
}

func (c FfiConverterKeys) Read(reader io.Reader) Keys {
	return Keys{
		FfiConverterMapUint64StringINSTANCE.Read(reader),
	}
}

func (c FfiConverterKeys) Lower(value Keys) C.RustBuffer {
	return LowerIntoRustBuffer[Keys](c, value)
}

func (c FfiConverterKeys) LowerExternal(value Keys) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Keys](c, value))
}

func (c FfiConverterKeys) Write(writer io.Writer, value Keys) {
	FfiConverterMapUint64StringINSTANCE.Write(writer, value.Keys)
}

type FfiDestroyerKeys struct{}

func (_ FfiDestroyerKeys) Destroy(value Keys) {
	value.Destroy()
}

// FFI-compatible options for confirming a melt operation
type MeltConfirmOptions struct {
	// Skip the pre-melt swap and send proofs directly to melt.
	// When true, saves swap input fees but gets change from melt instead.
	SkipSwap bool
}

func (r *MeltConfirmOptions) Destroy() {
	FfiDestroyerBool{}.Destroy(r.SkipSwap)
}

type FfiConverterMeltConfirmOptions struct{}

var FfiConverterMeltConfirmOptionsINSTANCE = FfiConverterMeltConfirmOptions{}

func (c FfiConverterMeltConfirmOptions) Lift(rb RustBufferI) MeltConfirmOptions {
	return LiftFromRustBuffer[MeltConfirmOptions](c, rb)
}

func (c FfiConverterMeltConfirmOptions) Read(reader io.Reader) MeltConfirmOptions {
	return MeltConfirmOptions{
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltConfirmOptions) Lower(value MeltConfirmOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MeltConfirmOptions](c, value)
}

func (c FfiConverterMeltConfirmOptions) LowerExternal(value MeltConfirmOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltConfirmOptions](c, value))
}

func (c FfiConverterMeltConfirmOptions) Write(writer io.Writer, value MeltConfirmOptions) {
	FfiConverterBoolINSTANCE.Write(writer, value.SkipSwap)
}

type FfiDestroyerMeltConfirmOptions struct{}

func (_ FfiDestroyerMeltConfirmOptions) Destroy(value MeltConfirmOptions) {
	value.Destroy()
}

// FFI-compatible MeltMethodSettings (NUT-05)
type MeltMethodSettings struct {
	Method PaymentMethod
	Unit   CurrencyUnit
	// Human-readable name for the payment method.
	//
	// If null or omitted, wallets should derive it from `method` by replacing
	// `_` and `-` with spaces and title-casing each word.
	MethodName *string
	MinAmount  *Amount
	MaxAmount  *Amount
	// For bolt11, whether mint supports amountless invoices
	Amountless *bool
}

func (r *MeltMethodSettings) Destroy() {
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalString{}.Destroy(r.MethodName)
	FfiDestroyerOptionalAmount{}.Destroy(r.MinAmount)
	FfiDestroyerOptionalAmount{}.Destroy(r.MaxAmount)
	FfiDestroyerOptionalBool{}.Destroy(r.Amountless)
}

type FfiConverterMeltMethodSettings struct{}

var FfiConverterMeltMethodSettingsINSTANCE = FfiConverterMeltMethodSettings{}

func (c FfiConverterMeltMethodSettings) Lift(rb RustBufferI) MeltMethodSettings {
	return LiftFromRustBuffer[MeltMethodSettings](c, rb)
}

func (c FfiConverterMeltMethodSettings) Read(reader io.Reader) MeltMethodSettings {
	return MeltMethodSettings{
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltMethodSettings) Lower(value MeltMethodSettings) C.RustBuffer {
	return LowerIntoRustBuffer[MeltMethodSettings](c, value)
}

func (c FfiConverterMeltMethodSettings) LowerExternal(value MeltMethodSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltMethodSettings](c, value))
}

func (c FfiConverterMeltMethodSettings) Write(writer io.Writer, value MeltMethodSettings) {
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MethodName)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.MinAmount)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.MaxAmount)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Amountless)
}

type FfiDestroyerMeltMethodSettings struct{}

func (_ FfiDestroyerMeltMethodSettings) Destroy(value MeltMethodSettings) {
	value.Destroy()
}

// FFI-compatible Melt prepare options
type MeltPrepareOptions struct {
	// Signing keys for P2PK/HTLC-locked input proofs; keys known to the
	// wallet are merged in automatically
	P2pkSigningKeys []SecretKey
	// Preimages for HTLC-locked input proofs
	Preimages []string
	// Metadata
	Metadata map[string]string
}

func (r *MeltPrepareOptions) Destroy() {
	FfiDestroyerSequenceSecretKey{}.Destroy(r.P2pkSigningKeys)
	FfiDestroyerSequenceString{}.Destroy(r.Preimages)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
}

type FfiConverterMeltPrepareOptions struct{}

var FfiConverterMeltPrepareOptionsINSTANCE = FfiConverterMeltPrepareOptions{}

func (c FfiConverterMeltPrepareOptions) Lift(rb RustBufferI) MeltPrepareOptions {
	return LiftFromRustBuffer[MeltPrepareOptions](c, rb)
}

func (c FfiConverterMeltPrepareOptions) Read(reader io.Reader) MeltPrepareOptions {
	return MeltPrepareOptions{
		FfiConverterSequenceSecretKeyINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltPrepareOptions) Lower(value MeltPrepareOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MeltPrepareOptions](c, value)
}

func (c FfiConverterMeltPrepareOptions) LowerExternal(value MeltPrepareOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltPrepareOptions](c, value))
}

func (c FfiConverterMeltPrepareOptions) Write(writer io.Writer, value MeltPrepareOptions) {
	FfiConverterSequenceSecretKeyINSTANCE.Write(writer, value.P2pkSigningKeys)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Preimages)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
}

type FfiDestroyerMeltPrepareOptions struct{}

func (_ FfiDestroyerMeltPrepareOptions) Destroy(value MeltPrepareOptions) {
	value.Destroy()
}

// FFI-compatible MeltQuote
type MeltQuote struct {
	// Quote ID
	Id string
	// Mint URL
	MintUrl *MintUrl
	// Quote amount
	Amount Amount
	// Currency unit
	Unit CurrencyUnit
	// Payment request
	Request string
	// Fee reserve
	FeeReserve Amount
	// Quote state
	State QuoteState
	// Expiry timestamp
	Expiry uint64
	// Payment proof (e.g. Lightning preimage or onchain outpoint)
	PaymentProof *string
	// Estimated confirmation target in blocks for onchain quotes
	EstimatedBlocks *uint32
	// Selected fee option index for onchain quotes
	FeeIndex *uint32
	// Payment method
	PaymentMethod PaymentMethod
	// Operation ID that reserved this quote
	UsedByOperation *string
	// Version for optimistic locking
	Version uint32
}

func (r *MeltQuote) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerOptionalMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerAmount{}.Destroy(r.FeeReserve)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalString{}.Destroy(r.PaymentProof)
	FfiDestroyerOptionalUint32{}.Destroy(r.EstimatedBlocks)
	FfiDestroyerOptionalUint32{}.Destroy(r.FeeIndex)
	FfiDestroyerPaymentMethod{}.Destroy(r.PaymentMethod)
	FfiDestroyerOptionalString{}.Destroy(r.UsedByOperation)
	FfiDestroyerUint32{}.Destroy(r.Version)
}

type FfiConverterMeltQuote struct{}

var FfiConverterMeltQuoteINSTANCE = FfiConverterMeltQuote{}

func (c FfiConverterMeltQuote) Lift(rb RustBufferI) MeltQuote {
	return LiftFromRustBuffer[MeltQuote](c, rb)
}

func (c FfiConverterMeltQuote) Read(reader io.Reader) MeltQuote {
	return MeltQuote{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalMintUrlINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltQuote) Lower(value MeltQuote) C.RustBuffer {
	return LowerIntoRustBuffer[MeltQuote](c, value)
}

func (c FfiConverterMeltQuote) LowerExternal(value MeltQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltQuote](c, value))
}

func (c FfiConverterMeltQuote) Write(writer io.Writer, value MeltQuote) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterOptionalMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterAmountINSTANCE.Write(writer, value.FeeReserve)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PaymentProof)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.EstimatedBlocks)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.FeeIndex)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.PaymentMethod)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UsedByOperation)
	FfiConverterUint32INSTANCE.Write(writer, value.Version)
}

type FfiDestroyerMeltQuote struct{}

func (_ FfiDestroyerMeltQuote) Destroy(value MeltQuote) {
	value.Destroy()
}

// FFI-compatible MeltQuoteBolt11Response
type MeltQuoteBolt11Response struct {
	// Quote ID
	Quote string
	// Payment method
	Method PaymentMethod
	// Amount
	Amount Amount
	// Fee reserve
	FeeReserve Amount
	// State of the quote
	State QuoteState
	// Expiry timestamp
	Expiry uint64
	// Payment proof (optional)
	PaymentProof *string
	// Request string (optional)
	Request *string
	// Unit (optional)
	Unit *CurrencyUnit
}

func (r *MeltQuoteBolt11Response) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerAmount{}.Destroy(r.FeeReserve)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalString{}.Destroy(r.PaymentProof)
	FfiDestroyerOptionalString{}.Destroy(r.Request)
	FfiDestroyerOptionalCurrencyUnit{}.Destroy(r.Unit)
}

type FfiConverterMeltQuoteBolt11Response struct{}

var FfiConverterMeltQuoteBolt11ResponseINSTANCE = FfiConverterMeltQuoteBolt11Response{}

func (c FfiConverterMeltQuoteBolt11Response) Lift(rb RustBufferI) MeltQuoteBolt11Response {
	return LiftFromRustBuffer[MeltQuoteBolt11Response](c, rb)
}

func (c FfiConverterMeltQuoteBolt11Response) Read(reader io.Reader) MeltQuoteBolt11Response {
	return MeltQuoteBolt11Response{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalCurrencyUnitINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltQuoteBolt11Response) Lower(value MeltQuoteBolt11Response) C.RustBuffer {
	return LowerIntoRustBuffer[MeltQuoteBolt11Response](c, value)
}

func (c FfiConverterMeltQuoteBolt11Response) LowerExternal(value MeltQuoteBolt11Response) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltQuoteBolt11Response](c, value))
}

func (c FfiConverterMeltQuoteBolt11Response) Write(writer io.Writer, value MeltQuoteBolt11Response) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterAmountINSTANCE.Write(writer, value.FeeReserve)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PaymentProof)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Request)
	FfiConverterOptionalCurrencyUnitINSTANCE.Write(writer, value.Unit)
}

type FfiDestroyerMeltQuoteBolt11Response struct{}

func (_ FfiDestroyerMeltQuoteBolt11Response) Destroy(value MeltQuoteBolt11Response) {
	value.Destroy()
}

// FFI-compatible MeltQuoteCustomResponse
//
// This is a unified response type for custom payment methods that includes
// extra fields for method-specific data.
type MeltQuoteCustomResponse struct {
	// Quote ID
	Quote string
	// Payment method
	Method PaymentMethod
	// Amount
	Amount Amount
	// Fee reserve
	FeeReserve *Amount
	// State of the quote
	State QuoteState
	// Expiry timestamp
	Expiry uint64
	// Payment proof (optional)
	PaymentProof *string
	// Request string (optional)
	Request *string
	// Unit (optional)
	Unit *CurrencyUnit
	// Extra payment-method-specific fields as JSON string
	//
	// These fields are flattened into the JSON representation, allowing
	// custom payment methods to include additional data without nesting.
	Extra *string
}

func (r *MeltQuoteCustomResponse) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerOptionalAmount{}.Destroy(r.FeeReserve)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalString{}.Destroy(r.PaymentProof)
	FfiDestroyerOptionalString{}.Destroy(r.Request)
	FfiDestroyerOptionalCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalString{}.Destroy(r.Extra)
}

type FfiConverterMeltQuoteCustomResponse struct{}

var FfiConverterMeltQuoteCustomResponseINSTANCE = FfiConverterMeltQuoteCustomResponse{}

func (c FfiConverterMeltQuoteCustomResponse) Lift(rb RustBufferI) MeltQuoteCustomResponse {
	return LiftFromRustBuffer[MeltQuoteCustomResponse](c, rb)
}

func (c FfiConverterMeltQuoteCustomResponse) Read(reader io.Reader) MeltQuoteCustomResponse {
	return MeltQuoteCustomResponse{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltQuoteCustomResponse) Lower(value MeltQuoteCustomResponse) C.RustBuffer {
	return LowerIntoRustBuffer[MeltQuoteCustomResponse](c, value)
}

func (c FfiConverterMeltQuoteCustomResponse) LowerExternal(value MeltQuoteCustomResponse) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltQuoteCustomResponse](c, value))
}

func (c FfiConverterMeltQuoteCustomResponse) Write(writer io.Writer, value MeltQuoteCustomResponse) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.FeeReserve)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PaymentProof)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Request)
	FfiConverterOptionalCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Extra)
}

type FfiDestroyerMeltQuoteCustomResponse struct{}

func (_ FfiDestroyerMeltQuoteCustomResponse) Destroy(value MeltQuoteCustomResponse) {
	value.Destroy()
}

// Fee option for an onchain melt quote.
type MeltQuoteOnchainFeeOption struct {
	// Server-assigned identifier the wallet echoes back to select this option
	FeeIndex uint32
	// Maximum onchain transaction fee the mint may charge
	FeeReserve Amount
	// Estimated confirmation target in blocks
	EstimatedBlocks uint32
}

func (r *MeltQuoteOnchainFeeOption) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.FeeIndex)
	FfiDestroyerAmount{}.Destroy(r.FeeReserve)
	FfiDestroyerUint32{}.Destroy(r.EstimatedBlocks)
}

type FfiConverterMeltQuoteOnchainFeeOption struct{}

var FfiConverterMeltQuoteOnchainFeeOptionINSTANCE = FfiConverterMeltQuoteOnchainFeeOption{}

func (c FfiConverterMeltQuoteOnchainFeeOption) Lift(rb RustBufferI) MeltQuoteOnchainFeeOption {
	return LiftFromRustBuffer[MeltQuoteOnchainFeeOption](c, rb)
}

func (c FfiConverterMeltQuoteOnchainFeeOption) Read(reader io.Reader) MeltQuoteOnchainFeeOption {
	return MeltQuoteOnchainFeeOption{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltQuoteOnchainFeeOption) Lower(value MeltQuoteOnchainFeeOption) C.RustBuffer {
	return LowerIntoRustBuffer[MeltQuoteOnchainFeeOption](c, value)
}

func (c FfiConverterMeltQuoteOnchainFeeOption) LowerExternal(value MeltQuoteOnchainFeeOption) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltQuoteOnchainFeeOption](c, value))
}

func (c FfiConverterMeltQuoteOnchainFeeOption) Write(writer io.Writer, value MeltQuoteOnchainFeeOption) {
	FfiConverterUint32INSTANCE.Write(writer, value.FeeIndex)
	FfiConverterAmountINSTANCE.Write(writer, value.FeeReserve)
	FfiConverterUint32INSTANCE.Write(writer, value.EstimatedBlocks)
}

type FfiDestroyerMeltQuoteOnchainFeeOption struct{}

func (_ FfiDestroyerMeltQuoteOnchainFeeOption) Destroy(value MeltQuoteOnchainFeeOption) {
	value.Destroy()
}

// FFI-compatible MeltQuoteOnchainResponse.
type MeltQuoteOnchainResponse struct {
	// Quote ID
	Quote string
	// Payment method
	Method PaymentMethod
	// Amount being paid to the onchain address
	Amount Amount
	// Unit
	Unit CurrencyUnit
	// Quote state
	State QuoteState
	// Expiry timestamp
	Expiry uint64
	// Bitcoin address to pay
	Request string
	// Available onchain fee options
	FeeOptions []MeltQuoteOnchainFeeOption
	// Selected fee option index, once execution has started
	SelectedFeeIndex *uint32
	// Broadcast outpoint (`txid:vout`), once available
	Outpoint *string
	// Change blind signatures as JSON, when the mint returns change
	Change *string
}

func (r *MeltQuoteOnchainResponse) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerUint64{}.Destroy(r.Expiry)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerSequenceMeltQuoteOnchainFeeOption{}.Destroy(r.FeeOptions)
	FfiDestroyerOptionalUint32{}.Destroy(r.SelectedFeeIndex)
	FfiDestroyerOptionalString{}.Destroy(r.Outpoint)
	FfiDestroyerOptionalString{}.Destroy(r.Change)
}

type FfiConverterMeltQuoteOnchainResponse struct{}

var FfiConverterMeltQuoteOnchainResponseINSTANCE = FfiConverterMeltQuoteOnchainResponse{}

func (c FfiConverterMeltQuoteOnchainResponse) Lift(rb RustBufferI) MeltQuoteOnchainResponse {
	return LiftFromRustBuffer[MeltQuoteOnchainResponse](c, rb)
}

func (c FfiConverterMeltQuoteOnchainResponse) Read(reader io.Reader) MeltQuoteOnchainResponse {
	return MeltQuoteOnchainResponse{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterSequenceMeltQuoteOnchainFeeOptionINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMeltQuoteOnchainResponse) Lower(value MeltQuoteOnchainResponse) C.RustBuffer {
	return LowerIntoRustBuffer[MeltQuoteOnchainResponse](c, value)
}

func (c FfiConverterMeltQuoteOnchainResponse) LowerExternal(value MeltQuoteOnchainResponse) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltQuoteOnchainResponse](c, value))
}

func (c FfiConverterMeltQuoteOnchainResponse) Write(writer io.Writer, value MeltQuoteOnchainResponse) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterSequenceMeltQuoteOnchainFeeOptionINSTANCE.Write(writer, value.FeeOptions)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.SelectedFeeIndex)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Outpoint)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Change)
}

type FfiDestroyerMeltQuoteOnchainResponse struct{}

func (_ FfiDestroyerMeltQuoteOnchainResponse) Destroy(value MeltQuoteOnchainResponse) {
	value.Destroy()
}

// Mint backup data containing the list of mints and timestamp
type MintBackup struct {
	// List of mint URLs in the backup
	Mints []MintUrl
	// Unix timestamp of when the backup was created
	Timestamp uint64
}

func (r *MintBackup) Destroy() {
	FfiDestroyerSequenceMintUrl{}.Destroy(r.Mints)
	FfiDestroyerUint64{}.Destroy(r.Timestamp)
}

type FfiConverterMintBackup struct{}

var FfiConverterMintBackupINSTANCE = FfiConverterMintBackup{}

func (c FfiConverterMintBackup) Lift(rb RustBufferI) MintBackup {
	return LiftFromRustBuffer[MintBackup](c, rb)
}

func (c FfiConverterMintBackup) Read(reader io.Reader) MintBackup {
	return MintBackup{
		FfiConverterSequenceMintUrlINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMintBackup) Lower(value MintBackup) C.RustBuffer {
	return LowerIntoRustBuffer[MintBackup](c, value)
}

func (c FfiConverterMintBackup) LowerExternal(value MintBackup) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintBackup](c, value))
}

func (c FfiConverterMintBackup) Write(writer io.Writer, value MintBackup) {
	FfiConverterSequenceMintUrlINSTANCE.Write(writer, value.Mints)
	FfiConverterUint64INSTANCE.Write(writer, value.Timestamp)
}

type FfiDestroyerMintBackup struct{}

func (_ FfiDestroyerMintBackup) Destroy(value MintBackup) {
	value.Destroy()
}

// FFI-compatible MintInfo
type MintInfo struct {
	// name of the mint and should be recognizable
	Name *string
	// hex pubkey of the mint
	Pubkey *string
	// implementation name and the version running
	Version *MintVersion
	// short description of the mint
	Description *string
	// long description
	DescriptionLong *string
	// Contact info
	Contact *[]ContactInfo
	// shows which NUTs the mint supports
	Nuts Nuts
	// Mint's icon URL
	IconUrl *string
	// Mint's endpoint URLs
	Urls *[]string
	// message of the day that the wallet must display to the user
	Motd *string
	// server unix timestamp
	Time *uint64
	// terms of url service of the mint
	TosUrl *string
	// max length the mint accepts for any array in a request
	MaxArrayLength *uint64
}

func (r *MintInfo) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Name)
	FfiDestroyerOptionalString{}.Destroy(r.Pubkey)
	FfiDestroyerOptionalMintVersion{}.Destroy(r.Version)
	FfiDestroyerOptionalString{}.Destroy(r.Description)
	FfiDestroyerOptionalString{}.Destroy(r.DescriptionLong)
	FfiDestroyerOptionalSequenceContactInfo{}.Destroy(r.Contact)
	FfiDestroyerNuts{}.Destroy(r.Nuts)
	FfiDestroyerOptionalString{}.Destroy(r.IconUrl)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Urls)
	FfiDestroyerOptionalString{}.Destroy(r.Motd)
	FfiDestroyerOptionalUint64{}.Destroy(r.Time)
	FfiDestroyerOptionalString{}.Destroy(r.TosUrl)
	FfiDestroyerOptionalUint64{}.Destroy(r.MaxArrayLength)
}

type FfiConverterMintInfo struct{}

var FfiConverterMintInfoINSTANCE = FfiConverterMintInfo{}

func (c FfiConverterMintInfo) Lift(rb RustBufferI) MintInfo {
	return LiftFromRustBuffer[MintInfo](c, rb)
}

func (c FfiConverterMintInfo) Read(reader io.Reader) MintInfo {
	return MintInfo{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalMintVersionINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceContactInfoINSTANCE.Read(reader),
		FfiConverterNutsINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMintInfo) Lower(value MintInfo) C.RustBuffer {
	return LowerIntoRustBuffer[MintInfo](c, value)
}

func (c FfiConverterMintInfo) LowerExternal(value MintInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintInfo](c, value))
}

func (c FfiConverterMintInfo) Write(writer io.Writer, value MintInfo) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Name)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Pubkey)
	FfiConverterOptionalMintVersionINSTANCE.Write(writer, value.Version)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Description)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.DescriptionLong)
	FfiConverterOptionalSequenceContactInfoINSTANCE.Write(writer, value.Contact)
	FfiConverterNutsINSTANCE.Write(writer, value.Nuts)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.IconUrl)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Urls)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Motd)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Time)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TosUrl)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.MaxArrayLength)
}

type FfiDestroyerMintInfo struct{}

func (_ FfiDestroyerMintInfo) Destroy(value MintInfo) {
	value.Destroy()
}

// FFI-compatible MintMethodSettings (NUT-04)
type MintMethodSettings struct {
	Method PaymentMethod
	Unit   CurrencyUnit
	// Human-readable name for the payment method.
	//
	// If null or omitted, wallets should derive it from `method` by replacing
	// `_` and `-` with spaces and title-casing each word.
	MethodName *string
	MinAmount  *Amount
	MaxAmount  *Amount
	// For bolt11/bolt12, whether mint supports setting invoice/offer description
	Description *bool
}

func (r *MintMethodSettings) Destroy() {
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalString{}.Destroy(r.MethodName)
	FfiDestroyerOptionalAmount{}.Destroy(r.MinAmount)
	FfiDestroyerOptionalAmount{}.Destroy(r.MaxAmount)
	FfiDestroyerOptionalBool{}.Destroy(r.Description)
}

type FfiConverterMintMethodSettings struct{}

var FfiConverterMintMethodSettingsINSTANCE = FfiConverterMintMethodSettings{}

func (c FfiConverterMintMethodSettings) Lift(rb RustBufferI) MintMethodSettings {
	return LiftFromRustBuffer[MintMethodSettings](c, rb)
}

func (c FfiConverterMintMethodSettings) Read(reader io.Reader) MintMethodSettings {
	return MintMethodSettings{
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMintMethodSettings) Lower(value MintMethodSettings) C.RustBuffer {
	return LowerIntoRustBuffer[MintMethodSettings](c, value)
}

func (c FfiConverterMintMethodSettings) LowerExternal(value MintMethodSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintMethodSettings](c, value))
}

func (c FfiConverterMintMethodSettings) Write(writer io.Writer, value MintMethodSettings) {
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MethodName)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.MinAmount)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.MaxAmount)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Description)
}

type FfiDestroyerMintMethodSettings struct{}

func (_ FfiDestroyerMintMethodSettings) Destroy(value MintMethodSettings) {
	value.Destroy()
}

// FFI-compatible MintQuote
type MintQuote struct {
	// Quote ID
	Id string
	// Requested or fixed quote amount, when defined by the payment method.
	//
	// Variable-amount methods such as onchain leave this unset and track
	// funds through `amount_paid` and `amount_issued`.
	Amount *Amount
	// Currency unit
	Unit CurrencyUnit
	// Payment request
	Request string
	// Quote state
	State QuoteState
	// Expiry timestamp
	Expiry uint64
	// Mint URL
	MintUrl MintUrl
	// Amount issued
	AmountIssued Amount
	// Amount paid
	AmountPaid Amount
	// Last update timestamp
	UpdatedAt uint64
	// Estimated confirmation target in blocks for onchain quotes
	EstimatedBlocks *uint32
	// Payment method
	PaymentMethod PaymentMethod
	// Secret key (optional, hex-encoded)
	SecretKey *string
	// Operation ID that reserved this quote
	UsedByOperation *string
	// Version for optimistic locking
	Version uint32
}

func (r *MintQuote) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerOptionalAmount{}.Destroy(r.Amount)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerUint64{}.Destroy(r.Expiry)
	FfiDestroyerMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerAmount{}.Destroy(r.AmountIssued)
	FfiDestroyerAmount{}.Destroy(r.AmountPaid)
	FfiDestroyerUint64{}.Destroy(r.UpdatedAt)
	FfiDestroyerOptionalUint32{}.Destroy(r.EstimatedBlocks)
	FfiDestroyerPaymentMethod{}.Destroy(r.PaymentMethod)
	FfiDestroyerOptionalString{}.Destroy(r.SecretKey)
	FfiDestroyerOptionalString{}.Destroy(r.UsedByOperation)
	FfiDestroyerUint32{}.Destroy(r.Version)
}

type FfiConverterMintQuote struct{}

var FfiConverterMintQuoteINSTANCE = FfiConverterMintQuote{}

func (c FfiConverterMintQuote) Lift(rb RustBufferI) MintQuote {
	return LiftFromRustBuffer[MintQuote](c, rb)
}

func (c FfiConverterMintQuote) Read(reader io.Reader) MintQuote {
	return MintQuote{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterMintUrlINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMintQuote) Lower(value MintQuote) C.RustBuffer {
	return LowerIntoRustBuffer[MintQuote](c, value)
}

func (c FfiConverterMintQuote) LowerExternal(value MintQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintQuote](c, value))
}

func (c FfiConverterMintQuote) Write(writer io.Writer, value MintQuote) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountIssued)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountPaid)
	FfiConverterUint64INSTANCE.Write(writer, value.UpdatedAt)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.EstimatedBlocks)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.PaymentMethod)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.SecretKey)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UsedByOperation)
	FfiConverterUint32INSTANCE.Write(writer, value.Version)
}

type FfiDestroyerMintQuote struct{}

func (_ FfiDestroyerMintQuote) Destroy(value MintQuote) {
	value.Destroy()
}

// FFI-compatible MintQuoteBolt11Response
type MintQuoteBolt11Response struct {
	// Quote ID
	Quote string
	// Request string
	Request string
	// Payment method
	Method PaymentMethod
	// State of the quote
	State QuoteState
	// Expiry timestamp (optional)
	Expiry *uint64
	// Amount (optional)
	Amount *Amount
	// Unit (optional)
	Unit *CurrencyUnit
	// Amount paid
	AmountPaid Amount
	// Amount issued
	AmountIssued Amount
	// Last update timestamp
	UpdatedAt uint64
	// Pubkey (optional)
	Pubkey *string
}

func (r *MintQuoteBolt11Response) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerQuoteState{}.Destroy(r.State)
	FfiDestroyerOptionalUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalAmount{}.Destroy(r.Amount)
	FfiDestroyerOptionalCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerAmount{}.Destroy(r.AmountPaid)
	FfiDestroyerAmount{}.Destroy(r.AmountIssued)
	FfiDestroyerUint64{}.Destroy(r.UpdatedAt)
	FfiDestroyerOptionalString{}.Destroy(r.Pubkey)
}

type FfiConverterMintQuoteBolt11Response struct{}

var FfiConverterMintQuoteBolt11ResponseINSTANCE = FfiConverterMintQuoteBolt11Response{}

func (c FfiConverterMintQuoteBolt11Response) Lift(rb RustBufferI) MintQuoteBolt11Response {
	return LiftFromRustBuffer[MintQuoteBolt11Response](c, rb)
}

func (c FfiConverterMintQuoteBolt11Response) Read(reader io.Reader) MintQuoteBolt11Response {
	return MintQuoteBolt11Response{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterQuoteStateINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterOptionalCurrencyUnitINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMintQuoteBolt11Response) Lower(value MintQuoteBolt11Response) C.RustBuffer {
	return LowerIntoRustBuffer[MintQuoteBolt11Response](c, value)
}

func (c FfiConverterMintQuoteBolt11Response) LowerExternal(value MintQuoteBolt11Response) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintQuoteBolt11Response](c, value))
}

func (c FfiConverterMintQuoteBolt11Response) Write(writer io.Writer, value MintQuoteBolt11Response) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterQuoteStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterOptionalCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountPaid)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountIssued)
	FfiConverterUint64INSTANCE.Write(writer, value.UpdatedAt)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Pubkey)
}

type FfiDestroyerMintQuoteBolt11Response struct{}

func (_ FfiDestroyerMintQuoteBolt11Response) Destroy(value MintQuoteBolt11Response) {
	value.Destroy()
}

// FFI-compatible MintQuoteCustomResponse
//
// This is a unified response type for custom payment methods that includes
// extra fields for method-specific data (e.g., ehash share).
type MintQuoteCustomResponse struct {
	// Quote ID
	Quote string
	// Request string
	Request string
	// Payment method
	Method PaymentMethod
	// Expiry timestamp (optional)
	Expiry *uint64
	// Amount (optional)
	Amount *Amount
	// Amount paid
	AmountPaid Amount
	// Amount issued
	AmountIssued Amount
	// Last update timestamp
	UpdatedAt uint64
	// Unit (optional)
	Unit *CurrencyUnit
	// Pubkey (optional)
	Pubkey *string
	// Extra payment-method-specific fields as JSON string
	//
	// These fields are flattened into the JSON representation, allowing
	// custom payment methods to include additional data without nesting.
	Extra *string
}

func (r *MintQuoteCustomResponse) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerOptionalUint64{}.Destroy(r.Expiry)
	FfiDestroyerOptionalAmount{}.Destroy(r.Amount)
	FfiDestroyerAmount{}.Destroy(r.AmountPaid)
	FfiDestroyerAmount{}.Destroy(r.AmountIssued)
	FfiDestroyerUint64{}.Destroy(r.UpdatedAt)
	FfiDestroyerOptionalCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalString{}.Destroy(r.Pubkey)
	FfiDestroyerOptionalString{}.Destroy(r.Extra)
}

type FfiConverterMintQuoteCustomResponse struct{}

var FfiConverterMintQuoteCustomResponseINSTANCE = FfiConverterMintQuoteCustomResponse{}

func (c FfiConverterMintQuoteCustomResponse) Lift(rb RustBufferI) MintQuoteCustomResponse {
	return LiftFromRustBuffer[MintQuoteCustomResponse](c, rb)
}

func (c FfiConverterMintQuoteCustomResponse) Read(reader io.Reader) MintQuoteCustomResponse {
	return MintQuoteCustomResponse{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMintQuoteCustomResponse) Lower(value MintQuoteCustomResponse) C.RustBuffer {
	return LowerIntoRustBuffer[MintQuoteCustomResponse](c, value)
}

func (c FfiConverterMintQuoteCustomResponse) LowerExternal(value MintQuoteCustomResponse) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintQuoteCustomResponse](c, value))
}

func (c FfiConverterMintQuoteCustomResponse) Write(writer io.Writer, value MintQuoteCustomResponse) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountPaid)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountIssued)
	FfiConverterUint64INSTANCE.Write(writer, value.UpdatedAt)
	FfiConverterOptionalCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Pubkey)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Extra)
}

type FfiDestroyerMintQuoteCustomResponse struct{}

func (_ FfiDestroyerMintQuoteCustomResponse) Destroy(value MintQuoteCustomResponse) {
	value.Destroy()
}

// FFI-compatible MintQuoteOnchainResponse.
type MintQuoteOnchainResponse struct {
	// Quote ID
	Quote string
	// Bitcoin address to pay
	Request string
	// Payment method
	Method PaymentMethod
	// Unit
	Unit CurrencyUnit
	// Expiry timestamp
	Expiry *uint64
	// NUT-20 public key
	Pubkey string
	// Total confirmed amount paid to the onchain address
	AmountPaid Amount
	// Amount already issued for this quote
	AmountIssued Amount
	// Last update timestamp
	UpdatedAt uint64
}

func (r *MintQuoteOnchainResponse) Destroy() {
	FfiDestroyerString{}.Destroy(r.Quote)
	FfiDestroyerString{}.Destroy(r.Request)
	FfiDestroyerPaymentMethod{}.Destroy(r.Method)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalUint64{}.Destroy(r.Expiry)
	FfiDestroyerString{}.Destroy(r.Pubkey)
	FfiDestroyerAmount{}.Destroy(r.AmountPaid)
	FfiDestroyerAmount{}.Destroy(r.AmountIssued)
	FfiDestroyerUint64{}.Destroy(r.UpdatedAt)
}

type FfiConverterMintQuoteOnchainResponse struct{}

var FfiConverterMintQuoteOnchainResponseINSTANCE = FfiConverterMintQuoteOnchainResponse{}

func (c FfiConverterMintQuoteOnchainResponse) Lift(rb RustBufferI) MintQuoteOnchainResponse {
	return LiftFromRustBuffer[MintQuoteOnchainResponse](c, rb)
}

func (c FfiConverterMintQuoteOnchainResponse) Read(reader io.Reader) MintQuoteOnchainResponse {
	return MintQuoteOnchainResponse{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterPaymentMethodINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMintQuoteOnchainResponse) Lower(value MintQuoteOnchainResponse) C.RustBuffer {
	return LowerIntoRustBuffer[MintQuoteOnchainResponse](c, value)
}

func (c FfiConverterMintQuoteOnchainResponse) LowerExternal(value MintQuoteOnchainResponse) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintQuoteOnchainResponse](c, value))
}

func (c FfiConverterMintQuoteOnchainResponse) Write(writer io.Writer, value MintQuoteOnchainResponse) {
	FfiConverterStringINSTANCE.Write(writer, value.Quote)
	FfiConverterStringINSTANCE.Write(writer, value.Request)
	FfiConverterPaymentMethodINSTANCE.Write(writer, value.Method)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Expiry)
	FfiConverterStringINSTANCE.Write(writer, value.Pubkey)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountPaid)
	FfiConverterAmountINSTANCE.Write(writer, value.AmountIssued)
	FfiConverterUint64INSTANCE.Write(writer, value.UpdatedAt)
}

type FfiDestroyerMintQuoteOnchainResponse struct{}

func (_ FfiDestroyerMintQuoteOnchainResponse) Destroy(value MintQuoteOnchainResponse) {
	value.Destroy()
}

// FFI-compatible Mint URL
type MintUrl struct {
	Url string
}

func (r *MintUrl) Destroy() {
	FfiDestroyerString{}.Destroy(r.Url)
}

type FfiConverterMintUrl struct{}

var FfiConverterMintUrlINSTANCE = FfiConverterMintUrl{}

func (c FfiConverterMintUrl) Lift(rb RustBufferI) MintUrl {
	return LiftFromRustBuffer[MintUrl](c, rb)
}

func (c FfiConverterMintUrl) Read(reader io.Reader) MintUrl {
	return MintUrl{
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMintUrl) Lower(value MintUrl) C.RustBuffer {
	return LowerIntoRustBuffer[MintUrl](c, value)
}

func (c FfiConverterMintUrl) LowerExternal(value MintUrl) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintUrl](c, value))
}

func (c FfiConverterMintUrl) Write(writer io.Writer, value MintUrl) {
	FfiConverterStringINSTANCE.Write(writer, value.Url)
}

type FfiDestroyerMintUrl struct{}

func (_ FfiDestroyerMintUrl) Destroy(value MintUrl) {
	value.Destroy()
}

// FFI-compatible MintVersion
type MintVersion struct {
	// Mint Software name
	Name string
	// Mint Version
	Version string
}

func (r *MintVersion) Destroy() {
	FfiDestroyerString{}.Destroy(r.Name)
	FfiDestroyerString{}.Destroy(r.Version)
}

type FfiConverterMintVersion struct{}

var FfiConverterMintVersionINSTANCE = FfiConverterMintVersion{}

func (c FfiConverterMintVersion) Lift(rb RustBufferI) MintVersion {
	return LiftFromRustBuffer[MintVersion](c, rb)
}

func (c FfiConverterMintVersion) Read(reader io.Reader) MintVersion {
	return MintVersion{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMintVersion) Lower(value MintVersion) C.RustBuffer {
	return LowerIntoRustBuffer[MintVersion](c, value)
}

func (c FfiConverterMintVersion) LowerExternal(value MintVersion) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MintVersion](c, value))
}

func (c FfiConverterMintVersion) Write(writer io.Writer, value MintVersion) {
	FfiConverterStringINSTANCE.Write(writer, value.Name)
	FfiConverterStringINSTANCE.Write(writer, value.Version)
}

type FfiDestroyerMintVersion struct{}

func (_ FfiDestroyerMintVersion) Destroy(value MintVersion) {
	value.Destroy()
}

// FFI-compatible NUT-13 restore options
type Nut13Options struct {
	// Number of blinded messages to request per batch
	BatchSize uint32
	// Number of consecutive empty batches that terminate the scan
	MaxGap uint32
}

func (r *Nut13Options) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.BatchSize)
	FfiDestroyerUint32{}.Destroy(r.MaxGap)
}

type FfiConverterNut13Options struct{}

var FfiConverterNut13OptionsINSTANCE = FfiConverterNut13Options{}

func (c FfiConverterNut13Options) Lift(rb RustBufferI) Nut13Options {
	return LiftFromRustBuffer[Nut13Options](c, rb)
}

func (c FfiConverterNut13Options) Read(reader io.Reader) Nut13Options {
	return Nut13Options{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterNut13Options) Lower(value Nut13Options) C.RustBuffer {
	return LowerIntoRustBuffer[Nut13Options](c, value)
}

func (c FfiConverterNut13Options) LowerExternal(value Nut13Options) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Nut13Options](c, value))
}

func (c FfiConverterNut13Options) Write(writer io.Writer, value Nut13Options) {
	FfiConverterUint32INSTANCE.Write(writer, value.BatchSize)
	FfiConverterUint32INSTANCE.Write(writer, value.MaxGap)
}

type FfiDestroyerNut13Options struct{}

func (_ FfiDestroyerNut13Options) Destroy(value Nut13Options) {
	value.Destroy()
}

// An unwrapped NIP-17 gift wrap received by a [`NostrInbox`]
//
// All IDs and keys are hex-encoded; timestamps are unix seconds.
type NostrInboxEvent struct {
	// ID of the (ephemeral) kind `1059` gift wrap event. Use it to
	// de-duplicate deliveries across relay reconnects and restarts.
	WrapId string
	// `created_at` of the gift wrap (NIP-59 randomizes/backdates it)
	WrapCreatedAt uint64
	// Author of the verified seal — the real sender of the rumor
	SenderPubkey string
	// ID of the rumor, if the sender included one
	RumorId *string
	// Kind of the rumor (commonly `14` for chat/DM payloads)
	RumorKind uint16
	// Content of the rumor (e.g. a NUT-18 payment request payload for
	// kind `14` rumors)
	RumorContent string
	// `created_at` of the rumor
	RumorCreatedAt uint64
	// Tags of the rumor
	RumorTags [][]string
}

func (r *NostrInboxEvent) Destroy() {
	FfiDestroyerString{}.Destroy(r.WrapId)
	FfiDestroyerUint64{}.Destroy(r.WrapCreatedAt)
	FfiDestroyerString{}.Destroy(r.SenderPubkey)
	FfiDestroyerOptionalString{}.Destroy(r.RumorId)
	FfiDestroyerUint16{}.Destroy(r.RumorKind)
	FfiDestroyerString{}.Destroy(r.RumorContent)
	FfiDestroyerUint64{}.Destroy(r.RumorCreatedAt)
	FfiDestroyerSequenceSequenceString{}.Destroy(r.RumorTags)
}

type FfiConverterNostrInboxEvent struct{}

var FfiConverterNostrInboxEventINSTANCE = FfiConverterNostrInboxEvent{}

func (c FfiConverterNostrInboxEvent) Lift(rb RustBufferI) NostrInboxEvent {
	return LiftFromRustBuffer[NostrInboxEvent](c, rb)
}

func (c FfiConverterNostrInboxEvent) Read(reader io.Reader) NostrInboxEvent {
	return NostrInboxEvent{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterUint16INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterSequenceSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterNostrInboxEvent) Lower(value NostrInboxEvent) C.RustBuffer {
	return LowerIntoRustBuffer[NostrInboxEvent](c, value)
}

func (c FfiConverterNostrInboxEvent) LowerExternal(value NostrInboxEvent) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[NostrInboxEvent](c, value))
}

func (c FfiConverterNostrInboxEvent) Write(writer io.Writer, value NostrInboxEvent) {
	FfiConverterStringINSTANCE.Write(writer, value.WrapId)
	FfiConverterUint64INSTANCE.Write(writer, value.WrapCreatedAt)
	FfiConverterStringINSTANCE.Write(writer, value.SenderPubkey)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.RumorId)
	FfiConverterUint16INSTANCE.Write(writer, value.RumorKind)
	FfiConverterStringINSTANCE.Write(writer, value.RumorContent)
	FfiConverterUint64INSTANCE.Write(writer, value.RumorCreatedAt)
	FfiConverterSequenceSequenceStringINSTANCE.Write(writer, value.RumorTags)
}

type FfiDestroyerNostrInboxEvent struct{}

func (_ FfiDestroyerNostrInboxEvent) Destroy(value NostrInboxEvent) {
	value.Destroy()
}

// A quote from the NpubCash service
type NpubCashQuote struct {
	// Unique identifier for the quote
	Id string
	// Amount in the specified unit
	Amount uint64
	// Currency or unit for the amount (e.g., "sat")
	Unit string
	// Unix timestamp when the quote was created
	CreatedAt uint64
	// Unix timestamp when the quote was paid (if paid)
	PaidAt *uint64
	// Unix timestamp when the quote expires
	ExpiresAt *uint64
	// Mint URL associated with the quote
	MintUrl *string
	// Lightning invoice request
	Request *string
	// Quote state (e.g., "PAID", "PENDING")
	State *string
	// Whether the quote is locked
	Locked *bool
}

func (r *NpubCashQuote) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerUint64{}.Destroy(r.Amount)
	FfiDestroyerString{}.Destroy(r.Unit)
	FfiDestroyerUint64{}.Destroy(r.CreatedAt)
	FfiDestroyerOptionalUint64{}.Destroy(r.PaidAt)
	FfiDestroyerOptionalUint64{}.Destroy(r.ExpiresAt)
	FfiDestroyerOptionalString{}.Destroy(r.MintUrl)
	FfiDestroyerOptionalString{}.Destroy(r.Request)
	FfiDestroyerOptionalString{}.Destroy(r.State)
	FfiDestroyerOptionalBool{}.Destroy(r.Locked)
}

type FfiConverterNpubCashQuote struct{}

var FfiConverterNpubCashQuoteINSTANCE = FfiConverterNpubCashQuote{}

func (c FfiConverterNpubCashQuote) Lift(rb RustBufferI) NpubCashQuote {
	return LiftFromRustBuffer[NpubCashQuote](c, rb)
}

func (c FfiConverterNpubCashQuote) Read(reader io.Reader) NpubCashQuote {
	return NpubCashQuote{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterNpubCashQuote) Lower(value NpubCashQuote) C.RustBuffer {
	return LowerIntoRustBuffer[NpubCashQuote](c, value)
}

func (c FfiConverterNpubCashQuote) LowerExternal(value NpubCashQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[NpubCashQuote](c, value))
}

func (c FfiConverterNpubCashQuote) Write(writer io.Writer, value NpubCashQuote) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterUint64INSTANCE.Write(writer, value.Amount)
	FfiConverterStringINSTANCE.Write(writer, value.Unit)
	FfiConverterUint64INSTANCE.Write(writer, value.CreatedAt)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PaidAt)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.ExpiresAt)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MintUrl)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Request)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.State)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Locked)
}

type FfiDestroyerNpubCashQuote struct{}

func (_ FfiDestroyerNpubCashQuote) Destroy(value NpubCashQuote) {
	value.Destroy()
}

// Response from updating user settings on NpubCash
type NpubCashUserResponse struct {
	// Whether the request resulted in an error
	Error bool
	// User's public key
	Pubkey string
	// Configured mint URL
	MintUrl *string
	// Whether quotes are locked
	LockQuote bool
}

func (r *NpubCashUserResponse) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Error)
	FfiDestroyerString{}.Destroy(r.Pubkey)
	FfiDestroyerOptionalString{}.Destroy(r.MintUrl)
	FfiDestroyerBool{}.Destroy(r.LockQuote)
}

type FfiConverterNpubCashUserResponse struct{}

var FfiConverterNpubCashUserResponseINSTANCE = FfiConverterNpubCashUserResponse{}

func (c FfiConverterNpubCashUserResponse) Lift(rb RustBufferI) NpubCashUserResponse {
	return LiftFromRustBuffer[NpubCashUserResponse](c, rb)
}

func (c FfiConverterNpubCashUserResponse) Read(reader io.Reader) NpubCashUserResponse {
	return NpubCashUserResponse{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterNpubCashUserResponse) Lower(value NpubCashUserResponse) C.RustBuffer {
	return LowerIntoRustBuffer[NpubCashUserResponse](c, value)
}

func (c FfiConverterNpubCashUserResponse) LowerExternal(value NpubCashUserResponse) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[NpubCashUserResponse](c, value))
}

func (c FfiConverterNpubCashUserResponse) Write(writer io.Writer, value NpubCashUserResponse) {
	FfiConverterBoolINSTANCE.Write(writer, value.Error)
	FfiConverterStringINSTANCE.Write(writer, value.Pubkey)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MintUrl)
	FfiConverterBoolINSTANCE.Write(writer, value.LockQuote)
}

type FfiDestroyerNpubCashUserResponse struct{}

func (_ FfiDestroyerNpubCashUserResponse) Destroy(value NpubCashUserResponse) {
	value.Destroy()
}

// FFI-compatible Nut04 Settings
type Nut04Settings struct {
	Methods  []MintMethodSettings
	Disabled bool
}

func (r *Nut04Settings) Destroy() {
	FfiDestroyerSequenceMintMethodSettings{}.Destroy(r.Methods)
	FfiDestroyerBool{}.Destroy(r.Disabled)
}

type FfiConverterNut04Settings struct{}

var FfiConverterNut04SettingsINSTANCE = FfiConverterNut04Settings{}

func (c FfiConverterNut04Settings) Lift(rb RustBufferI) Nut04Settings {
	return LiftFromRustBuffer[Nut04Settings](c, rb)
}

func (c FfiConverterNut04Settings) Read(reader io.Reader) Nut04Settings {
	return Nut04Settings{
		FfiConverterSequenceMintMethodSettingsINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterNut04Settings) Lower(value Nut04Settings) C.RustBuffer {
	return LowerIntoRustBuffer[Nut04Settings](c, value)
}

func (c FfiConverterNut04Settings) LowerExternal(value Nut04Settings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Nut04Settings](c, value))
}

func (c FfiConverterNut04Settings) Write(writer io.Writer, value Nut04Settings) {
	FfiConverterSequenceMintMethodSettingsINSTANCE.Write(writer, value.Methods)
	FfiConverterBoolINSTANCE.Write(writer, value.Disabled)
}

type FfiDestroyerNut04Settings struct{}

func (_ FfiDestroyerNut04Settings) Destroy(value Nut04Settings) {
	value.Destroy()
}

// FFI-compatible Nut05 Settings
type Nut05Settings struct {
	Methods  []MeltMethodSettings
	Disabled bool
}

func (r *Nut05Settings) Destroy() {
	FfiDestroyerSequenceMeltMethodSettings{}.Destroy(r.Methods)
	FfiDestroyerBool{}.Destroy(r.Disabled)
}

type FfiConverterNut05Settings struct{}

var FfiConverterNut05SettingsINSTANCE = FfiConverterNut05Settings{}

func (c FfiConverterNut05Settings) Lift(rb RustBufferI) Nut05Settings {
	return LiftFromRustBuffer[Nut05Settings](c, rb)
}

func (c FfiConverterNut05Settings) Read(reader io.Reader) Nut05Settings {
	return Nut05Settings{
		FfiConverterSequenceMeltMethodSettingsINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterNut05Settings) Lower(value Nut05Settings) C.RustBuffer {
	return LowerIntoRustBuffer[Nut05Settings](c, value)
}

func (c FfiConverterNut05Settings) LowerExternal(value Nut05Settings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Nut05Settings](c, value))
}

func (c FfiConverterNut05Settings) Write(writer io.Writer, value Nut05Settings) {
	FfiConverterSequenceMeltMethodSettingsINSTANCE.Write(writer, value.Methods)
	FfiConverterBoolINSTANCE.Write(writer, value.Disabled)
}

type FfiDestroyerNut05Settings struct{}

func (_ FfiDestroyerNut05Settings) Destroy(value Nut05Settings) {
	value.Destroy()
}

// FFI-compatible Nut29Settings (NUT-29)
type Nut29Settings struct {
	// Maximum number of quotes allowed in a single batch
	MaxBatchSize *uint64
	// Supported payment methods for batch minting
	Methods *[]string
}

func (r *Nut29Settings) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.MaxBatchSize)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Methods)
}

type FfiConverterNut29Settings struct{}

var FfiConverterNut29SettingsINSTANCE = FfiConverterNut29Settings{}

func (c FfiConverterNut29Settings) Lift(rb RustBufferI) Nut29Settings {
	return LiftFromRustBuffer[Nut29Settings](c, rb)
}

func (c FfiConverterNut29Settings) Read(reader io.Reader) Nut29Settings {
	return Nut29Settings{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterNut29Settings) Lower(value Nut29Settings) C.RustBuffer {
	return LowerIntoRustBuffer[Nut29Settings](c, value)
}

func (c FfiConverterNut29Settings) LowerExternal(value Nut29Settings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Nut29Settings](c, value))
}

func (c FfiConverterNut29Settings) Write(writer io.Writer, value Nut29Settings) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.MaxBatchSize)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Methods)
}

type FfiDestroyerNut29Settings struct{}

func (_ FfiDestroyerNut29Settings) Destroy(value Nut29Settings) {
	value.Destroy()
}

// FFI-compatible Nuts settings (extended to include NUT-04 and NUT-05 settings)
type Nuts struct {
	// NUT04 Settings
	Nut04 Nut04Settings
	// NUT05 Settings
	Nut05 Nut05Settings
	// NUT07 Settings - Token state check
	Nut07Supported bool
	// NUT08 Settings - Lightning fee return
	Nut08Supported bool
	// NUT09 Settings - Restore signature
	Nut09Supported bool
	// NUT10 Settings - Spending conditions
	Nut10Supported bool
	// NUT11 Settings - Pay to Public Key Hash
	Nut11Supported bool
	// NUT12 Settings - DLEQ proofs
	Nut12Supported bool
	// NUT14 Settings - Hashed Time Locked Contracts
	Nut14Supported bool
	// NUT20 Settings - Web sockets
	Nut20Supported bool
	// NUT21 Settings - Clear authentication
	Nut21 *ClearAuthSettings
	// NUT22 Settings - Blind authentication
	Nut22 *BlindAuthSettings
	// NUT29 Settings - Batch minting
	Nut29 Nut29Settings
	// Supported currency units for minting
	MintUnits []CurrencyUnit
	// Supported currency units for melting
	MeltUnits []CurrencyUnit
}

func (r *Nuts) Destroy() {
	FfiDestroyerNut04Settings{}.Destroy(r.Nut04)
	FfiDestroyerNut05Settings{}.Destroy(r.Nut05)
	FfiDestroyerBool{}.Destroy(r.Nut07Supported)
	FfiDestroyerBool{}.Destroy(r.Nut08Supported)
	FfiDestroyerBool{}.Destroy(r.Nut09Supported)
	FfiDestroyerBool{}.Destroy(r.Nut10Supported)
	FfiDestroyerBool{}.Destroy(r.Nut11Supported)
	FfiDestroyerBool{}.Destroy(r.Nut12Supported)
	FfiDestroyerBool{}.Destroy(r.Nut14Supported)
	FfiDestroyerBool{}.Destroy(r.Nut20Supported)
	FfiDestroyerOptionalClearAuthSettings{}.Destroy(r.Nut21)
	FfiDestroyerOptionalBlindAuthSettings{}.Destroy(r.Nut22)
	FfiDestroyerNut29Settings{}.Destroy(r.Nut29)
	FfiDestroyerSequenceCurrencyUnit{}.Destroy(r.MintUnits)
	FfiDestroyerSequenceCurrencyUnit{}.Destroy(r.MeltUnits)
}

type FfiConverterNuts struct{}

var FfiConverterNutsINSTANCE = FfiConverterNuts{}

func (c FfiConverterNuts) Lift(rb RustBufferI) Nuts {
	return LiftFromRustBuffer[Nuts](c, rb)
}

func (c FfiConverterNuts) Read(reader io.Reader) Nuts {
	return Nuts{
		FfiConverterNut04SettingsINSTANCE.Read(reader),
		FfiConverterNut05SettingsINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalClearAuthSettingsINSTANCE.Read(reader),
		FfiConverterOptionalBlindAuthSettingsINSTANCE.Read(reader),
		FfiConverterNut29SettingsINSTANCE.Read(reader),
		FfiConverterSequenceCurrencyUnitINSTANCE.Read(reader),
		FfiConverterSequenceCurrencyUnitINSTANCE.Read(reader),
	}
}

func (c FfiConverterNuts) Lower(value Nuts) C.RustBuffer {
	return LowerIntoRustBuffer[Nuts](c, value)
}

func (c FfiConverterNuts) LowerExternal(value Nuts) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Nuts](c, value))
}

func (c FfiConverterNuts) Write(writer io.Writer, value Nuts) {
	FfiConverterNut04SettingsINSTANCE.Write(writer, value.Nut04)
	FfiConverterNut05SettingsINSTANCE.Write(writer, value.Nut05)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut07Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut08Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut09Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut10Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut11Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut12Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut14Supported)
	FfiConverterBoolINSTANCE.Write(writer, value.Nut20Supported)
	FfiConverterOptionalClearAuthSettingsINSTANCE.Write(writer, value.Nut21)
	FfiConverterOptionalBlindAuthSettingsINSTANCE.Write(writer, value.Nut22)
	FfiConverterNut29SettingsINSTANCE.Write(writer, value.Nut29)
	FfiConverterSequenceCurrencyUnitINSTANCE.Write(writer, value.MintUnits)
	FfiConverterSequenceCurrencyUnitINSTANCE.Write(writer, value.MeltUnits)
}

type FfiDestroyerNuts struct{}

func (_ FfiDestroyerNuts) Destroy(value Nuts) {
	value.Destroy()
}

// FFI-compatible P2PKSigningKey
type P2pkSigningKey struct {
	// Public key
	Pubkey PublicKey
	// Derivation path as string
	DerivationPath string
	// Derivation index
	DerivationIndex uint32
	// Created time
	CreatedTime uint64
}

func (r *P2pkSigningKey) Destroy() {
	FfiDestroyerPublicKey{}.Destroy(r.Pubkey)
	FfiDestroyerString{}.Destroy(r.DerivationPath)
	FfiDestroyerUint32{}.Destroy(r.DerivationIndex)
	FfiDestroyerUint64{}.Destroy(r.CreatedTime)
}

type FfiConverterP2pkSigningKey struct{}

var FfiConverterP2pkSigningKeyINSTANCE = FfiConverterP2pkSigningKey{}

func (c FfiConverterP2pkSigningKey) Lift(rb RustBufferI) P2pkSigningKey {
	return LiftFromRustBuffer[P2pkSigningKey](c, rb)
}

func (c FfiConverterP2pkSigningKey) Read(reader io.Reader) P2pkSigningKey {
	return P2pkSigningKey{
		FfiConverterPublicKeyINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterP2pkSigningKey) Lower(value P2pkSigningKey) C.RustBuffer {
	return LowerIntoRustBuffer[P2pkSigningKey](c, value)
}

func (c FfiConverterP2pkSigningKey) LowerExternal(value P2pkSigningKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[P2pkSigningKey](c, value))
}

func (c FfiConverterP2pkSigningKey) Write(writer io.Writer, value P2pkSigningKey) {
	FfiConverterPublicKeyINSTANCE.Write(writer, value.Pubkey)
	FfiConverterStringINSTANCE.Write(writer, value.DerivationPath)
	FfiConverterUint32INSTANCE.Write(writer, value.DerivationIndex)
	FfiConverterUint64INSTANCE.Write(writer, value.CreatedTime)
}

type FfiDestroyerP2pkSigningKey struct{}

func (_ FfiDestroyerP2pkSigningKey) Destroy(value P2pkSigningKey) {
	value.Destroy()
}

// A parsed BIP 321 payment instruction containing all payment methods found.
//
// After parsing, inspect the lists to determine which payment methods are
// available and choose the best one for your wallet. A single URI can contain
// multiple methods (e.g. cashu + BOLT11 + on-chain) to give the payer options.
//
// # Examples
//
// ```text
// // Parse a BIP 321 URI that bundles cashu, BOLT11, and an on-chain address
// val parsed = parseBip321PaymentInstruction(
// "bitcoin:bc1qar0s...?creq=CREQB1...&lightning=lnbc100n1p..."
// )
//
// // Check which payment methods are available and pick one
// when {
// parsed.cashuRequests.isNotEmpty() -> {
// // Prefer ecash: instant settlement, zero fees
// val request = parsed.cashuRequests.first()
// val id = request.paymentId()         // e.g. "b7a90176"
// val amount = request.amount()         // e.g. Amount(10)
// val unit = request.unit()             // e.g. CurrencyUnit.Sat
// val mints = request.mints()           // acceptable mint URLs
// val transports = request.transports() // how to deliver proofs
// }
// parsed.bolt11Invoices.isNotEmpty() -> {
// // Fall back to Lightning BOLT11
// val invoice = parsed.bolt11Invoices.first()
// }
// parsed.bolt12Offers.isNotEmpty() -> {
// // Fall back to Lightning BOLT12
// val offer = parsed.bolt12Offers.first()
// }
// parsed.onchainAddresses.isNotEmpty() -> {
// // Last resort: on-chain payment
// val address = parsed.onchainAddresses.first()
// }
// }
//
// // Amount info
// val msats = parsed.amountMsats           // fixed amount in msats, or null
// val flexible = parsed.isConfigurableAmount // true if payer chooses amount
// val desc = parsed.description             // URI label/message, or null
// ```
type ParsedPaymentInstruction struct {
	// Cashu NUT-26 payment requests.
	CashuRequests []*PaymentRequest
	// BOLT11 invoice strings.
	Bolt11Invoices []string
	// BOLT12 offer strings.
	Bolt12Offers []string
	// On-chain bitcoin addresses.
	OnchainAddresses []string
	// Description / label / message from the URI.
	Description *string
	// Amount in millisatoshis (if a fixed-amount instruction).
	AmountMsats *uint64
	// Whether the amount is configurable (vs fixed).
	IsConfigurableAmount bool
}

func (r *ParsedPaymentInstruction) Destroy() {
	FfiDestroyerSequencePaymentRequest{}.Destroy(r.CashuRequests)
	FfiDestroyerSequenceString{}.Destroy(r.Bolt11Invoices)
	FfiDestroyerSequenceString{}.Destroy(r.Bolt12Offers)
	FfiDestroyerSequenceString{}.Destroy(r.OnchainAddresses)
	FfiDestroyerOptionalString{}.Destroy(r.Description)
	FfiDestroyerOptionalUint64{}.Destroy(r.AmountMsats)
	FfiDestroyerBool{}.Destroy(r.IsConfigurableAmount)
}

type FfiConverterParsedPaymentInstruction struct{}

var FfiConverterParsedPaymentInstructionINSTANCE = FfiConverterParsedPaymentInstruction{}

func (c FfiConverterParsedPaymentInstruction) Lift(rb RustBufferI) ParsedPaymentInstruction {
	return LiftFromRustBuffer[ParsedPaymentInstruction](c, rb)
}

func (c FfiConverterParsedPaymentInstruction) Read(reader io.Reader) ParsedPaymentInstruction {
	return ParsedPaymentInstruction{
		FfiConverterSequencePaymentRequestINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterParsedPaymentInstruction) Lower(value ParsedPaymentInstruction) C.RustBuffer {
	return LowerIntoRustBuffer[ParsedPaymentInstruction](c, value)
}

func (c FfiConverterParsedPaymentInstruction) LowerExternal(value ParsedPaymentInstruction) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ParsedPaymentInstruction](c, value))
}

func (c FfiConverterParsedPaymentInstruction) Write(writer io.Writer, value ParsedPaymentInstruction) {
	FfiConverterSequencePaymentRequestINSTANCE.Write(writer, value.CashuRequests)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Bolt11Invoices)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Bolt12Offers)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.OnchainAddresses)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Description)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.AmountMsats)
	FfiConverterBoolINSTANCE.Write(writer, value.IsConfigurableAmount)
}

type FfiDestroyerParsedPaymentInstruction struct{}

func (_ FfiDestroyerParsedPaymentInstruction) Destroy(value ParsedPaymentInstruction) {
	value.Destroy()
}

// FFI-compatible Proof
type Proof struct {
	// Proof amount
	Amount Amount
	// Secret (as string)
	Secret string
	// Unblinded signature C (as hex string)
	C string
	// Keyset ID (as hex string)
	KeysetId string
	// Optional witness
	Witness *Witness
	// Optional DLEQ proof
	Dleq *ProofDleq
	// Optional P2BK Ephemeral Public Key (NUT-28)
	P2pkE *string
}

func (r *Proof) Destroy() {
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerString{}.Destroy(r.Secret)
	FfiDestroyerString{}.Destroy(r.C)
	FfiDestroyerString{}.Destroy(r.KeysetId)
	FfiDestroyerOptionalWitness{}.Destroy(r.Witness)
	FfiDestroyerOptionalProofDleq{}.Destroy(r.Dleq)
	FfiDestroyerOptionalString{}.Destroy(r.P2pkE)
}

type FfiConverterProof struct{}

var FfiConverterProofINSTANCE = FfiConverterProof{}

func (c FfiConverterProof) Lift(rb RustBufferI) Proof {
	return LiftFromRustBuffer[Proof](c, rb)
}

func (c FfiConverterProof) Read(reader io.Reader) Proof {
	return Proof{
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalWitnessINSTANCE.Read(reader),
		FfiConverterOptionalProofDleqINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterProof) Lower(value Proof) C.RustBuffer {
	return LowerIntoRustBuffer[Proof](c, value)
}

func (c FfiConverterProof) LowerExternal(value Proof) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Proof](c, value))
}

func (c FfiConverterProof) Write(writer io.Writer, value Proof) {
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterStringINSTANCE.Write(writer, value.Secret)
	FfiConverterStringINSTANCE.Write(writer, value.C)
	FfiConverterStringINSTANCE.Write(writer, value.KeysetId)
	FfiConverterOptionalWitnessINSTANCE.Write(writer, value.Witness)
	FfiConverterOptionalProofDleqINSTANCE.Write(writer, value.Dleq)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.P2pkE)
}

type FfiDestroyerProof struct{}

func (_ FfiDestroyerProof) Destroy(value Proof) {
	value.Destroy()
}

// FFI-compatible DLEQ proof for proofs
type ProofDleq struct {
	// e value (hex-encoded SecretKey)
	E string
	// s value (hex-encoded SecretKey)
	S string
	// r value - blinding factor (hex-encoded SecretKey)
	R string
}

func (r *ProofDleq) Destroy() {
	FfiDestroyerString{}.Destroy(r.E)
	FfiDestroyerString{}.Destroy(r.S)
	FfiDestroyerString{}.Destroy(r.R)
}

type FfiConverterProofDleq struct{}

var FfiConverterProofDleqINSTANCE = FfiConverterProofDleq{}

func (c FfiConverterProofDleq) Lift(rb RustBufferI) ProofDleq {
	return LiftFromRustBuffer[ProofDleq](c, rb)
}

func (c FfiConverterProofDleq) Read(reader io.Reader) ProofDleq {
	return ProofDleq{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterProofDleq) Lower(value ProofDleq) C.RustBuffer {
	return LowerIntoRustBuffer[ProofDleq](c, value)
}

func (c FfiConverterProofDleq) LowerExternal(value ProofDleq) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ProofDleq](c, value))
}

func (c FfiConverterProofDleq) Write(writer io.Writer, value ProofDleq) {
	FfiConverterStringINSTANCE.Write(writer, value.E)
	FfiConverterStringINSTANCE.Write(writer, value.S)
	FfiConverterStringINSTANCE.Write(writer, value.R)
}

type FfiDestroyerProofDleq struct{}

func (_ FfiDestroyerProofDleq) Destroy(value ProofDleq) {
	value.Destroy()
}

// FFI-compatible ProofInfo
type ProofInfo struct {
	// Proof
	Proof Proof
	// Y value (hash_to_curve of secret)
	Y PublicKey
	// Mint URL
	MintUrl MintUrl
	// Proof state
	State ProofState
	// Proof Spending Conditions
	SpendingCondition *SpendingConditions
	// Currency unit
	Unit CurrencyUnit
	// NUT-13 derivation index for deterministic proofs, if known.
	//
	// Wallet-internal metadata used to prefer older deterministic proofs
	// during coin selection. Custom database implementations may persist
	// this field; leaving it unset keeps legacy selection behavior.
	DerivationIndex *uint32
	// Operation ID that is using/spending this proof
	UsedByOperation *string
	// Operation ID that created this proof
	CreatedByOperation *string
}

func (r *ProofInfo) Destroy() {
	FfiDestroyerProof{}.Destroy(r.Proof)
	FfiDestroyerPublicKey{}.Destroy(r.Y)
	FfiDestroyerMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerProofState{}.Destroy(r.State)
	FfiDestroyerOptionalSpendingConditions{}.Destroy(r.SpendingCondition)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalUint32{}.Destroy(r.DerivationIndex)
	FfiDestroyerOptionalString{}.Destroy(r.UsedByOperation)
	FfiDestroyerOptionalString{}.Destroy(r.CreatedByOperation)
}

type FfiConverterProofInfo struct{}

var FfiConverterProofInfoINSTANCE = FfiConverterProofInfo{}

func (c FfiConverterProofInfo) Lift(rb RustBufferI) ProofInfo {
	return LiftFromRustBuffer[ProofInfo](c, rb)
}

func (c FfiConverterProofInfo) Read(reader io.Reader) ProofInfo {
	return ProofInfo{
		FfiConverterProofINSTANCE.Read(reader),
		FfiConverterPublicKeyINSTANCE.Read(reader),
		FfiConverterMintUrlINSTANCE.Read(reader),
		FfiConverterProofStateINSTANCE.Read(reader),
		FfiConverterOptionalSpendingConditionsINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterProofInfo) Lower(value ProofInfo) C.RustBuffer {
	return LowerIntoRustBuffer[ProofInfo](c, value)
}

func (c FfiConverterProofInfo) LowerExternal(value ProofInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ProofInfo](c, value))
}

func (c FfiConverterProofInfo) Write(writer io.Writer, value ProofInfo) {
	FfiConverterProofINSTANCE.Write(writer, value.Proof)
	FfiConverterPublicKeyINSTANCE.Write(writer, value.Y)
	FfiConverterMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterProofStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalSpendingConditionsINSTANCE.Write(writer, value.SpendingCondition)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.DerivationIndex)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UsedByOperation)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.CreatedByOperation)
}

type FfiDestroyerProofInfo struct{}

func (_ FfiDestroyerProofInfo) Destroy(value ProofInfo) {
	value.Destroy()
}

// FFI-compatible ProofStateUpdate
type ProofStateUpdate struct {
	// Y value (hash_to_curve of secret)
	Y string
	// Current state
	State ProofState
	// Optional witness data
	Witness *Witness
}

func (r *ProofStateUpdate) Destroy() {
	FfiDestroyerString{}.Destroy(r.Y)
	FfiDestroyerProofState{}.Destroy(r.State)
	FfiDestroyerOptionalWitness{}.Destroy(r.Witness)
}

type FfiConverterProofStateUpdate struct{}

var FfiConverterProofStateUpdateINSTANCE = FfiConverterProofStateUpdate{}

func (c FfiConverterProofStateUpdate) Lift(rb RustBufferI) ProofStateUpdate {
	return LiftFromRustBuffer[ProofStateUpdate](c, rb)
}

func (c FfiConverterProofStateUpdate) Read(reader io.Reader) ProofStateUpdate {
	return ProofStateUpdate{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterProofStateINSTANCE.Read(reader),
		FfiConverterOptionalWitnessINSTANCE.Read(reader),
	}
}

func (c FfiConverterProofStateUpdate) Lower(value ProofStateUpdate) C.RustBuffer {
	return LowerIntoRustBuffer[ProofStateUpdate](c, value)
}

func (c FfiConverterProofStateUpdate) LowerExternal(value ProofStateUpdate) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ProofStateUpdate](c, value))
}

func (c FfiConverterProofStateUpdate) Write(writer io.Writer, value ProofStateUpdate) {
	FfiConverterStringINSTANCE.Write(writer, value.Y)
	FfiConverterProofStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalWitnessINSTANCE.Write(writer, value.Witness)
}

type FfiDestroyerProofStateUpdate struct{}

func (_ FfiDestroyerProofStateUpdate) Destroy(value ProofStateUpdate) {
	value.Destroy()
}

// FFI-compatible ProtectedEndpoint (for auth nuts)
type ProtectedEndpoint struct {
	// HTTP method (GET, POST, etc.)
	Method string
	// Endpoint path
	Path string
}

func (r *ProtectedEndpoint) Destroy() {
	FfiDestroyerString{}.Destroy(r.Method)
	FfiDestroyerString{}.Destroy(r.Path)
}

type FfiConverterProtectedEndpoint struct{}

var FfiConverterProtectedEndpointINSTANCE = FfiConverterProtectedEndpoint{}

func (c FfiConverterProtectedEndpoint) Lift(rb RustBufferI) ProtectedEndpoint {
	return LiftFromRustBuffer[ProtectedEndpoint](c, rb)
}

func (c FfiConverterProtectedEndpoint) Read(reader io.Reader) ProtectedEndpoint {
	return ProtectedEndpoint{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterProtectedEndpoint) Lower(value ProtectedEndpoint) C.RustBuffer {
	return LowerIntoRustBuffer[ProtectedEndpoint](c, value)
}

func (c FfiConverterProtectedEndpoint) LowerExternal(value ProtectedEndpoint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ProtectedEndpoint](c, value))
}

func (c FfiConverterProtectedEndpoint) Write(writer io.Writer, value ProtectedEndpoint) {
	FfiConverterStringINSTANCE.Write(writer, value.Method)
	FfiConverterStringINSTANCE.Write(writer, value.Path)
}

type FfiDestroyerProtectedEndpoint struct{}

func (_ FfiDestroyerProtectedEndpoint) Destroy(value ProtectedEndpoint) {
	value.Destroy()
}

// FFI-compatible PublicKey
type PublicKey struct {
	// Hex-encoded public key
	Hex string
}

func (r *PublicKey) Destroy() {
	FfiDestroyerString{}.Destroy(r.Hex)
}

type FfiConverterPublicKey struct{}

var FfiConverterPublicKeyINSTANCE = FfiConverterPublicKey{}

func (c FfiConverterPublicKey) Lift(rb RustBufferI) PublicKey {
	return LiftFromRustBuffer[PublicKey](c, rb)
}

func (c FfiConverterPublicKey) Read(reader io.Reader) PublicKey {
	return PublicKey{
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterPublicKey) Lower(value PublicKey) C.RustBuffer {
	return LowerIntoRustBuffer[PublicKey](c, value)
}

func (c FfiConverterPublicKey) LowerExternal(value PublicKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PublicKey](c, value))
}

func (c FfiConverterPublicKey) Write(writer io.Writer, value PublicKey) {
	FfiConverterStringINSTANCE.Write(writer, value.Hex)
}

type FfiDestroyerPublicKey struct{}

func (_ FfiDestroyerPublicKey) Destroy(value PublicKey) {
	value.Destroy()
}

// FFI-compatible Receive options
type ReceiveOptions struct {
	// Amount split target
	AmountSplitTarget SplitTarget
	// P2PK signing keys
	P2pkSigningKeys []SecretKey
	// Preimages for HTLC conditions
	Preimages []string
	// Metadata
	Metadata map[string]string
}

func (r *ReceiveOptions) Destroy() {
	FfiDestroyerSplitTarget{}.Destroy(r.AmountSplitTarget)
	FfiDestroyerSequenceSecretKey{}.Destroy(r.P2pkSigningKeys)
	FfiDestroyerSequenceString{}.Destroy(r.Preimages)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
}

type FfiConverterReceiveOptions struct{}

var FfiConverterReceiveOptionsINSTANCE = FfiConverterReceiveOptions{}

func (c FfiConverterReceiveOptions) Lift(rb RustBufferI) ReceiveOptions {
	return LiftFromRustBuffer[ReceiveOptions](c, rb)
}

func (c FfiConverterReceiveOptions) Read(reader io.Reader) ReceiveOptions {
	return ReceiveOptions{
		FfiConverterSplitTargetINSTANCE.Read(reader),
		FfiConverterSequenceSecretKeyINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterReceiveOptions) Lower(value ReceiveOptions) C.RustBuffer {
	return LowerIntoRustBuffer[ReceiveOptions](c, value)
}

func (c FfiConverterReceiveOptions) LowerExternal(value ReceiveOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ReceiveOptions](c, value))
}

func (c FfiConverterReceiveOptions) Write(writer io.Writer, value ReceiveOptions) {
	FfiConverterSplitTargetINSTANCE.Write(writer, value.AmountSplitTarget)
	FfiConverterSequenceSecretKeyINSTANCE.Write(writer, value.P2pkSigningKeys)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Preimages)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
}

type FfiDestroyerReceiveOptions struct{}

func (_ FfiDestroyerReceiveOptions) Destroy(value ReceiveOptions) {
	value.Destroy()
}

// Report of wallet saga recovery operations.
type RecoveryReport struct {
	// Operations successfully completed after crash.
	Recovered uint64
	// Operations rolled back and resources released.
	Compensated uint64
	// Operations still pending and left for a later retry.
	Skipped uint64
	// Operations that could not be recovered.
	Failed uint64
}

func (r *RecoveryReport) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.Recovered)
	FfiDestroyerUint64{}.Destroy(r.Compensated)
	FfiDestroyerUint64{}.Destroy(r.Skipped)
	FfiDestroyerUint64{}.Destroy(r.Failed)
}

type FfiConverterRecoveryReport struct{}

var FfiConverterRecoveryReportINSTANCE = FfiConverterRecoveryReport{}

func (c FfiConverterRecoveryReport) Lift(rb RustBufferI) RecoveryReport {
	return LiftFromRustBuffer[RecoveryReport](c, rb)
}

func (c FfiConverterRecoveryReport) Read(reader io.Reader) RecoveryReport {
	return RecoveryReport{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterRecoveryReport) Lower(value RecoveryReport) C.RustBuffer {
	return LowerIntoRustBuffer[RecoveryReport](c, value)
}

func (c FfiConverterRecoveryReport) LowerExternal(value RecoveryReport) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RecoveryReport](c, value))
}

func (c FfiConverterRecoveryReport) Write(writer io.Writer, value RecoveryReport) {
	FfiConverterUint64INSTANCE.Write(writer, value.Recovered)
	FfiConverterUint64INSTANCE.Write(writer, value.Compensated)
	FfiConverterUint64INSTANCE.Write(writer, value.Skipped)
	FfiConverterUint64INSTANCE.Write(writer, value.Failed)
}

type FfiDestroyerRecoveryReport struct{}

func (_ FfiDestroyerRecoveryReport) Destroy(value RecoveryReport) {
	value.Destroy()
}

// Options for restore operations
type RestoreOptions struct {
	// Timeout in seconds for waiting for relay responses
	TimeoutSecs uint64
}

func (r *RestoreOptions) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.TimeoutSecs)
}

type FfiConverterRestoreOptions struct{}

var FfiConverterRestoreOptionsINSTANCE = FfiConverterRestoreOptions{}

func (c FfiConverterRestoreOptions) Lift(rb RustBufferI) RestoreOptions {
	return LiftFromRustBuffer[RestoreOptions](c, rb)
}

func (c FfiConverterRestoreOptions) Read(reader io.Reader) RestoreOptions {
	return RestoreOptions{
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterRestoreOptions) Lower(value RestoreOptions) C.RustBuffer {
	return LowerIntoRustBuffer[RestoreOptions](c, value)
}

func (c FfiConverterRestoreOptions) LowerExternal(value RestoreOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RestoreOptions](c, value))
}

func (c FfiConverterRestoreOptions) Write(writer io.Writer, value RestoreOptions) {
	FfiConverterUint64INSTANCE.Write(writer, value.TimeoutSecs)
}

type FfiDestroyerRestoreOptions struct{}

func (_ FfiDestroyerRestoreOptions) Destroy(value RestoreOptions) {
	value.Destroy()
}

// Result of a restore operation
type RestoreResult struct {
	// The restored mint backup data
	Backup MintBackup
	// Number of mints found in the backup
	MintCount uint64
	// Number of mints that were newly added (not already in wallet)
	MintsAdded uint64
}

func (r *RestoreResult) Destroy() {
	FfiDestroyerMintBackup{}.Destroy(r.Backup)
	FfiDestroyerUint64{}.Destroy(r.MintCount)
	FfiDestroyerUint64{}.Destroy(r.MintsAdded)
}

type FfiConverterRestoreResult struct{}

var FfiConverterRestoreResultINSTANCE = FfiConverterRestoreResult{}

func (c FfiConverterRestoreResult) Lift(rb RustBufferI) RestoreResult {
	return LiftFromRustBuffer[RestoreResult](c, rb)
}

func (c FfiConverterRestoreResult) Read(reader io.Reader) RestoreResult {
	return RestoreResult{
		FfiConverterMintBackupINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterRestoreResult) Lower(value RestoreResult) C.RustBuffer {
	return LowerIntoRustBuffer[RestoreResult](c, value)
}

func (c FfiConverterRestoreResult) LowerExternal(value RestoreResult) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RestoreResult](c, value))
}

func (c FfiConverterRestoreResult) Write(writer io.Writer, value RestoreResult) {
	FfiConverterMintBackupINSTANCE.Write(writer, value.Backup)
	FfiConverterUint64INSTANCE.Write(writer, value.MintCount)
	FfiConverterUint64INSTANCE.Write(writer, value.MintsAdded)
}

type FfiDestroyerRestoreResult struct{}

func (_ FfiDestroyerRestoreResult) Destroy(value RestoreResult) {
	value.Destroy()
}

// Restored Data
type Restored struct {
	Spent   Amount
	Unspent Amount
	Pending Amount
}

func (r *Restored) Destroy() {
	FfiDestroyerAmount{}.Destroy(r.Spent)
	FfiDestroyerAmount{}.Destroy(r.Unspent)
	FfiDestroyerAmount{}.Destroy(r.Pending)
}

type FfiConverterRestored struct{}

var FfiConverterRestoredINSTANCE = FfiConverterRestored{}

func (c FfiConverterRestored) Lift(rb RustBufferI) Restored {
	return LiftFromRustBuffer[Restored](c, rb)
}

func (c FfiConverterRestored) Read(reader io.Reader) Restored {
	return Restored{
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
	}
}

func (c FfiConverterRestored) Lower(value Restored) C.RustBuffer {
	return LowerIntoRustBuffer[Restored](c, value)
}

func (c FfiConverterRestored) LowerExternal(value Restored) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Restored](c, value))
}

func (c FfiConverterRestored) Write(writer io.Writer, value Restored) {
	FfiConverterAmountINSTANCE.Write(writer, value.Spent)
	FfiConverterAmountINSTANCE.Write(writer, value.Unspent)
	FfiConverterAmountINSTANCE.Write(writer, value.Pending)
}

type FfiDestroyerRestored struct{}

func (_ FfiDestroyerRestored) Destroy(value Restored) {
	value.Destroy()
}

// FFI-compatible SecretKey
type SecretKey struct {
	// Hex-encoded secret key (64 characters)
	Hex string
}

func (r *SecretKey) Destroy() {
	FfiDestroyerString{}.Destroy(r.Hex)
}

type FfiConverterSecretKey struct{}

var FfiConverterSecretKeyINSTANCE = FfiConverterSecretKey{}

func (c FfiConverterSecretKey) Lift(rb RustBufferI) SecretKey {
	return LiftFromRustBuffer[SecretKey](c, rb)
}

func (c FfiConverterSecretKey) Read(reader io.Reader) SecretKey {
	return SecretKey{
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterSecretKey) Lower(value SecretKey) C.RustBuffer {
	return LowerIntoRustBuffer[SecretKey](c, value)
}

func (c FfiConverterSecretKey) LowerExternal(value SecretKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SecretKey](c, value))
}

func (c FfiConverterSecretKey) Write(writer io.Writer, value SecretKey) {
	FfiConverterStringINSTANCE.Write(writer, value.Hex)
}

type FfiDestroyerSecretKey struct{}

func (_ FfiDestroyerSecretKey) Destroy(value SecretKey) {
	value.Destroy()
}

// FFI-compatible SendMemo
type SendMemo struct {
	// Memo text
	Memo string
	// Include memo in token
	IncludeMemo bool
}

func (r *SendMemo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Memo)
	FfiDestroyerBool{}.Destroy(r.IncludeMemo)
}

type FfiConverterSendMemo struct{}

var FfiConverterSendMemoINSTANCE = FfiConverterSendMemo{}

func (c FfiConverterSendMemo) Lift(rb RustBufferI) SendMemo {
	return LiftFromRustBuffer[SendMemo](c, rb)
}

func (c FfiConverterSendMemo) Read(reader io.Reader) SendMemo {
	return SendMemo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterSendMemo) Lower(value SendMemo) C.RustBuffer {
	return LowerIntoRustBuffer[SendMemo](c, value)
}

func (c FfiConverterSendMemo) LowerExternal(value SendMemo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SendMemo](c, value))
}

func (c FfiConverterSendMemo) Write(writer io.Writer, value SendMemo) {
	FfiConverterStringINSTANCE.Write(writer, value.Memo)
	FfiConverterBoolINSTANCE.Write(writer, value.IncludeMemo)
}

type FfiDestroyerSendMemo struct{}

func (_ FfiDestroyerSendMemo) Destroy(value SendMemo) {
	value.Destroy()
}

// FFI-compatible Send options
type SendOptions struct {
	// Memo
	Memo *SendMemo
	// Spending conditions
	Conditions *SpendingConditions
	// Amount split target
	AmountSplitTarget SplitTarget
	// Send kind
	SendKind SendKind
	// Include fee
	IncludeFee bool
	UseP2bk    bool
	// Maximum number of proofs to include in the token
	MaxProofs *uint32
	// Metadata
	Metadata map[string]string
	// Signing keys for P2PK-locked input proofs
	P2pkSigningKeys []SecretKey
	// How P2PK-locked input proofs should be handled during send
	P2pkLockedProofSendMode P2pkLockedProofSendMode
}

func (r *SendOptions) Destroy() {
	FfiDestroyerOptionalSendMemo{}.Destroy(r.Memo)
	FfiDestroyerOptionalSpendingConditions{}.Destroy(r.Conditions)
	FfiDestroyerSplitTarget{}.Destroy(r.AmountSplitTarget)
	FfiDestroyerSendKind{}.Destroy(r.SendKind)
	FfiDestroyerBool{}.Destroy(r.IncludeFee)
	FfiDestroyerBool{}.Destroy(r.UseP2bk)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxProofs)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
	FfiDestroyerSequenceSecretKey{}.Destroy(r.P2pkSigningKeys)
	FfiDestroyerP2pkLockedProofSendMode{}.Destroy(r.P2pkLockedProofSendMode)
}

type FfiConverterSendOptions struct{}

var FfiConverterSendOptionsINSTANCE = FfiConverterSendOptions{}

func (c FfiConverterSendOptions) Lift(rb RustBufferI) SendOptions {
	return LiftFromRustBuffer[SendOptions](c, rb)
}

func (c FfiConverterSendOptions) Read(reader io.Reader) SendOptions {
	return SendOptions{
		FfiConverterOptionalSendMemoINSTANCE.Read(reader),
		FfiConverterOptionalSpendingConditionsINSTANCE.Read(reader),
		FfiConverterSplitTargetINSTANCE.Read(reader),
		FfiConverterSendKindINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
		FfiConverterSequenceSecretKeyINSTANCE.Read(reader),
		FfiConverterP2pkLockedProofSendModeINSTANCE.Read(reader),
	}
}

func (c FfiConverterSendOptions) Lower(value SendOptions) C.RustBuffer {
	return LowerIntoRustBuffer[SendOptions](c, value)
}

func (c FfiConverterSendOptions) LowerExternal(value SendOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SendOptions](c, value))
}

func (c FfiConverterSendOptions) Write(writer io.Writer, value SendOptions) {
	FfiConverterOptionalSendMemoINSTANCE.Write(writer, value.Memo)
	FfiConverterOptionalSpendingConditionsINSTANCE.Write(writer, value.Conditions)
	FfiConverterSplitTargetINSTANCE.Write(writer, value.AmountSplitTarget)
	FfiConverterSendKindINSTANCE.Write(writer, value.SendKind)
	FfiConverterBoolINSTANCE.Write(writer, value.IncludeFee)
	FfiConverterBoolINSTANCE.Write(writer, value.UseP2bk)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxProofs)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
	FfiConverterSequenceSecretKeyINSTANCE.Write(writer, value.P2pkSigningKeys)
	FfiConverterP2pkLockedProofSendModeINSTANCE.Write(writer, value.P2pkLockedProofSendMode)
}

type FfiDestroyerSendOptions struct{}

func (_ FfiDestroyerSendOptions) Destroy(value SendOptions) {
	value.Destroy()
}

// FFI-compatible SubscribeParams
type SubscribeParams struct {
	// Subscription kind
	Kind SubscriptionKind
	// Filters
	Filters []string
	// Subscription ID (optional, will be generated if not provided)
	Id *string
}

func (r *SubscribeParams) Destroy() {
	FfiDestroyerSubscriptionKind{}.Destroy(r.Kind)
	FfiDestroyerSequenceString{}.Destroy(r.Filters)
	FfiDestroyerOptionalString{}.Destroy(r.Id)
}

type FfiConverterSubscribeParams struct{}

var FfiConverterSubscribeParamsINSTANCE = FfiConverterSubscribeParams{}

func (c FfiConverterSubscribeParams) Lift(rb RustBufferI) SubscribeParams {
	return LiftFromRustBuffer[SubscribeParams](c, rb)
}

func (c FfiConverterSubscribeParams) Read(reader io.Reader) SubscribeParams {
	return SubscribeParams{
		FfiConverterSubscriptionKindINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterSubscribeParams) Lower(value SubscribeParams) C.RustBuffer {
	return LowerIntoRustBuffer[SubscribeParams](c, value)
}

func (c FfiConverterSubscribeParams) LowerExternal(value SubscribeParams) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SubscribeParams](c, value))
}

func (c FfiConverterSubscribeParams) Write(writer io.Writer, value SubscribeParams) {
	FfiConverterSubscriptionKindINSTANCE.Write(writer, value.Kind)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Filters)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Id)
}

type FfiDestroyerSubscribeParams struct{}

func (_ FfiDestroyerSubscribeParams) Destroy(value SubscribeParams) {
	value.Destroy()
}

// Supported payment method for a NUT-18 payment request
type SupportedMethod struct {
	// Payment method name, such as "bolt11", "bolt12", or "onchain"
	Method string
	// Additional fee for payments from non-preferred mints
	Fee *Amount
}

func (r *SupportedMethod) Destroy() {
	FfiDestroyerString{}.Destroy(r.Method)
	FfiDestroyerOptionalAmount{}.Destroy(r.Fee)
}

type FfiConverterSupportedMethod struct{}

var FfiConverterSupportedMethodINSTANCE = FfiConverterSupportedMethod{}

func (c FfiConverterSupportedMethod) Lift(rb RustBufferI) SupportedMethod {
	return LiftFromRustBuffer[SupportedMethod](c, rb)
}

func (c FfiConverterSupportedMethod) Read(reader io.Reader) SupportedMethod {
	return SupportedMethod{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
	}
}

func (c FfiConverterSupportedMethod) Lower(value SupportedMethod) C.RustBuffer {
	return LowerIntoRustBuffer[SupportedMethod](c, value)
}

func (c FfiConverterSupportedMethod) LowerExternal(value SupportedMethod) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SupportedMethod](c, value))
}

func (c FfiConverterSupportedMethod) Write(writer io.Writer, value SupportedMethod) {
	FfiConverterStringINSTANCE.Write(writer, value.Method)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.Fee)
}

type FfiDestroyerSupportedMethod struct{}

func (_ FfiDestroyerSupportedMethod) Destroy(value SupportedMethod) {
	value.Destroy()
}

// FFI-compatible SupportedSettings
type SupportedSettings struct {
	// Setting supported
	Supported bool
}

func (r *SupportedSettings) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Supported)
}

type FfiConverterSupportedSettings struct{}

var FfiConverterSupportedSettingsINSTANCE = FfiConverterSupportedSettings{}

func (c FfiConverterSupportedSettings) Lift(rb RustBufferI) SupportedSettings {
	return LiftFromRustBuffer[SupportedSettings](c, rb)
}

func (c FfiConverterSupportedSettings) Read(reader io.Reader) SupportedSettings {
	return SupportedSettings{
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterSupportedSettings) Lower(value SupportedSettings) C.RustBuffer {
	return LowerIntoRustBuffer[SupportedSettings](c, value)
}

func (c FfiConverterSupportedSettings) LowerExternal(value SupportedSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SupportedSettings](c, value))
}

func (c FfiConverterSupportedSettings) Write(writer io.Writer, value SupportedSettings) {
	FfiConverterBoolINSTANCE.Write(writer, value.Supported)
}

type FfiDestroyerSupportedSettings struct{}

func (_ FfiDestroyerSupportedSettings) Destroy(value SupportedSettings) {
	value.Destroy()
}

// Token data FFI type
//
// Contains information extracted from a parsed token.
type TokenData struct {
	// The mint URL from the token
	MintUrl MintUrl
	// The proofs contained in the token
	Proofs []Proof
	// The memo from the token, if present
	Memo *string
	// Value of token in smallest unit
	Value Amount
	// Currency unit
	Unit CurrencyUnit
	// Fee to redeem (None if unknown)
	RedeemFee *Amount
}

func (r *TokenData) Destroy() {
	FfiDestroyerMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerSequenceProof{}.Destroy(r.Proofs)
	FfiDestroyerOptionalString{}.Destroy(r.Memo)
	FfiDestroyerAmount{}.Destroy(r.Value)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerOptionalAmount{}.Destroy(r.RedeemFee)
}

type FfiConverterTokenData struct{}

var FfiConverterTokenDataINSTANCE = FfiConverterTokenData{}

func (c FfiConverterTokenData) Lift(rb RustBufferI) TokenData {
	return LiftFromRustBuffer[TokenData](c, rb)
}

func (c FfiConverterTokenData) Read(reader io.Reader) TokenData {
	return TokenData{
		FfiConverterMintUrlINSTANCE.Read(reader),
		FfiConverterSequenceProofINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterOptionalAmountINSTANCE.Read(reader),
	}
}

func (c FfiConverterTokenData) Lower(value TokenData) C.RustBuffer {
	return LowerIntoRustBuffer[TokenData](c, value)
}

func (c FfiConverterTokenData) LowerExternal(value TokenData) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[TokenData](c, value))
}

func (c FfiConverterTokenData) Write(writer io.Writer, value TokenData) {
	FfiConverterMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterSequenceProofINSTANCE.Write(writer, value.Proofs)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Memo)
	FfiConverterAmountINSTANCE.Write(writer, value.Value)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterOptionalAmountINSTANCE.Write(writer, value.RedeemFee)
}

type FfiDestroyerTokenData struct{}

func (_ FfiDestroyerTokenData) Destroy(value TokenData) {
	value.Destroy()
}

// FFI-compatible Transaction
type Transaction struct {
	// Transaction ID
	Id TransactionId
	// Mint URL
	MintUrl MintUrl
	// Transaction direction
	Direction TransactionDirection
	// Amount
	Amount Amount
	// Fee
	Fee Amount
	// Currency Unit
	Unit CurrencyUnit
	// Proof Ys (Y values from proofs)
	Ys []PublicKey
	// Unix timestamp
	Timestamp uint64
	// Memo
	Memo *string
	// User-defined metadata
	Metadata map[string]string
	// Quote ID if this is a mint or melt transaction
	QuoteId *string
	// Payment request (e.g., BOLT11 invoice, BOLT12 offer)
	PaymentRequest *string
	// Payment proof (e.g., preimage for Lightning melt transactions)
	PaymentProof *string
	// Payment method (e.g., Bolt11, Bolt12) for mint/melt transactions
	PaymentMethod *PaymentMethod
	// Saga ID if this transaction was part of a saga
	SagaId *string
	// Transaction status
	Status TransactionStatus
}

func (r *Transaction) Destroy() {
	FfiDestroyerTransactionId{}.Destroy(r.Id)
	FfiDestroyerMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerTransactionDirection{}.Destroy(r.Direction)
	FfiDestroyerAmount{}.Destroy(r.Amount)
	FfiDestroyerAmount{}.Destroy(r.Fee)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
	FfiDestroyerSequencePublicKey{}.Destroy(r.Ys)
	FfiDestroyerUint64{}.Destroy(r.Timestamp)
	FfiDestroyerOptionalString{}.Destroy(r.Memo)
	FfiDestroyerMapStringString{}.Destroy(r.Metadata)
	FfiDestroyerOptionalString{}.Destroy(r.QuoteId)
	FfiDestroyerOptionalString{}.Destroy(r.PaymentRequest)
	FfiDestroyerOptionalString{}.Destroy(r.PaymentProof)
	FfiDestroyerOptionalPaymentMethod{}.Destroy(r.PaymentMethod)
	FfiDestroyerOptionalString{}.Destroy(r.SagaId)
	FfiDestroyerTransactionStatus{}.Destroy(r.Status)
}

type FfiConverterTransaction struct{}

var FfiConverterTransactionINSTANCE = FfiConverterTransaction{}

func (c FfiConverterTransaction) Lift(rb RustBufferI) Transaction {
	return LiftFromRustBuffer[Transaction](c, rb)
}

func (c FfiConverterTransaction) Read(reader io.Reader) Transaction {
	return Transaction{
		FfiConverterTransactionIdINSTANCE.Read(reader),
		FfiConverterMintUrlINSTANCE.Read(reader),
		FfiConverterTransactionDirectionINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterAmountINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
		FfiConverterSequencePublicKeyINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalPaymentMethodINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterTransactionStatusINSTANCE.Read(reader),
	}
}

func (c FfiConverterTransaction) Lower(value Transaction) C.RustBuffer {
	return LowerIntoRustBuffer[Transaction](c, value)
}

func (c FfiConverterTransaction) LowerExternal(value Transaction) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Transaction](c, value))
}

func (c FfiConverterTransaction) Write(writer io.Writer, value Transaction) {
	FfiConverterTransactionIdINSTANCE.Write(writer, value.Id)
	FfiConverterMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterTransactionDirectionINSTANCE.Write(writer, value.Direction)
	FfiConverterAmountINSTANCE.Write(writer, value.Amount)
	FfiConverterAmountINSTANCE.Write(writer, value.Fee)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
	FfiConverterSequencePublicKeyINSTANCE.Write(writer, value.Ys)
	FfiConverterUint64INSTANCE.Write(writer, value.Timestamp)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Memo)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Metadata)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.QuoteId)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PaymentRequest)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PaymentProof)
	FfiConverterOptionalPaymentMethodINSTANCE.Write(writer, value.PaymentMethod)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.SagaId)
	FfiConverterTransactionStatusINSTANCE.Write(writer, value.Status)
}

type FfiDestroyerTransaction struct{}

func (_ FfiDestroyerTransaction) Destroy(value Transaction) {
	value.Destroy()
}

// FFI-compatible TransactionId
type TransactionId struct {
	// Hex-encoded transaction ID (64 characters)
	Hex string
}

func (r *TransactionId) Destroy() {
	FfiDestroyerString{}.Destroy(r.Hex)
}

type FfiConverterTransactionId struct{}

var FfiConverterTransactionIdINSTANCE = FfiConverterTransactionId{}

func (c FfiConverterTransactionId) Lift(rb RustBufferI) TransactionId {
	return LiftFromRustBuffer[TransactionId](c, rb)
}

func (c FfiConverterTransactionId) Read(reader io.Reader) TransactionId {
	return TransactionId{
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTransactionId) Lower(value TransactionId) C.RustBuffer {
	return LowerIntoRustBuffer[TransactionId](c, value)
}

func (c FfiConverterTransactionId) LowerExternal(value TransactionId) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[TransactionId](c, value))
}

func (c FfiConverterTransactionId) Write(writer io.Writer, value TransactionId) {
	FfiConverterStringINSTANCE.Write(writer, value.Hex)
}

type FfiDestroyerTransactionId struct{}

func (_ FfiDestroyerTransactionId) Destroy(value TransactionId) {
	value.Destroy()
}

// Transport for payment request delivery
type Transport struct {
	// Transport type
	TransportType TransportType
	// Target (e.g., nprofile for Nostr, URL for HTTP)
	Target string
	// Tags
	Tags [][]string
}

func (r *Transport) Destroy() {
	FfiDestroyerTransportType{}.Destroy(r.TransportType)
	FfiDestroyerString{}.Destroy(r.Target)
	FfiDestroyerSequenceSequenceString{}.Destroy(r.Tags)
}

type FfiConverterTransport struct{}

var FfiConverterTransportINSTANCE = FfiConverterTransport{}

func (c FfiConverterTransport) Lift(rb RustBufferI) Transport {
	return LiftFromRustBuffer[Transport](c, rb)
}

func (c FfiConverterTransport) Read(reader io.Reader) Transport {
	return Transport{
		FfiConverterTransportTypeINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterSequenceSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTransport) Lower(value Transport) C.RustBuffer {
	return LowerIntoRustBuffer[Transport](c, value)
}

func (c FfiConverterTransport) LowerExternal(value Transport) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Transport](c, value))
}

func (c FfiConverterTransport) Write(writer io.Writer, value Transport) {
	FfiConverterTransportTypeINSTANCE.Write(writer, value.TransportType)
	FfiConverterStringINSTANCE.Write(writer, value.Target)
	FfiConverterSequenceSequenceStringINSTANCE.Write(writer, value.Tags)
}

type FfiDestroyerTransport struct{}

func (_ FfiDestroyerTransport) Destroy(value Transport) {
	value.Destroy()
}

// Configuration for creating wallets
type WalletConfig struct {
	TargetProofCount *uint32
	// Client-side request pacing to start with. Omit it to keep the built-in
	// default, which paces the wallet under a mint's per-minute request cap.
	//
	// `Disabled` builds the limiter but leaves it off, so a later
	// [`Wallet::set_rate_limit`] can turn pacing back on.
	RateLimit *RateLimit
}

func (r *WalletConfig) Destroy() {
	FfiDestroyerOptionalUint32{}.Destroy(r.TargetProofCount)
	FfiDestroyerOptionalRateLimit{}.Destroy(r.RateLimit)
}

type FfiConverterWalletConfig struct{}

var FfiConverterWalletConfigINSTANCE = FfiConverterWalletConfig{}

func (c FfiConverterWalletConfig) Lift(rb RustBufferI) WalletConfig {
	return LiftFromRustBuffer[WalletConfig](c, rb)
}

func (c FfiConverterWalletConfig) Read(reader io.Reader) WalletConfig {
	return WalletConfig{
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalRateLimitINSTANCE.Read(reader),
	}
}

func (c FfiConverterWalletConfig) Lower(value WalletConfig) C.RustBuffer {
	return LowerIntoRustBuffer[WalletConfig](c, value)
}

func (c FfiConverterWalletConfig) LowerExternal(value WalletConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[WalletConfig](c, value))
}

func (c FfiConverterWalletConfig) Write(writer io.Writer, value WalletConfig) {
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.TargetProofCount)
	FfiConverterOptionalRateLimitINSTANCE.Write(writer, value.RateLimit)
}

type FfiDestroyerWalletConfig struct{}

func (_ FfiDestroyerWalletConfig) Destroy(value WalletConfig) {
	value.Destroy()
}

// FFI-compatible WalletKey
type WalletKey struct {
	// Mint Url
	MintUrl MintUrl
	// Currency Unit
	Unit CurrencyUnit
}

func (r *WalletKey) Destroy() {
	FfiDestroyerMintUrl{}.Destroy(r.MintUrl)
	FfiDestroyerCurrencyUnit{}.Destroy(r.Unit)
}

type FfiConverterWalletKey struct{}

var FfiConverterWalletKeyINSTANCE = FfiConverterWalletKey{}

func (c FfiConverterWalletKey) Lift(rb RustBufferI) WalletKey {
	return LiftFromRustBuffer[WalletKey](c, rb)
}

func (c FfiConverterWalletKey) Read(reader io.Reader) WalletKey {
	return WalletKey{
		FfiConverterMintUrlINSTANCE.Read(reader),
		FfiConverterCurrencyUnitINSTANCE.Read(reader),
	}
}

func (c FfiConverterWalletKey) Lower(value WalletKey) C.RustBuffer {
	return LowerIntoRustBuffer[WalletKey](c, value)
}

func (c FfiConverterWalletKey) LowerExternal(value WalletKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[WalletKey](c, value))
}

func (c FfiConverterWalletKey) Write(writer io.Writer, value WalletKey) {
	FfiConverterMintUrlINSTANCE.Write(writer, value.MintUrl)
	FfiConverterCurrencyUnitINSTANCE.Write(writer, value.Unit)
}

type FfiDestroyerWalletKey struct{}

func (_ FfiDestroyerWalletKey) Destroy(value WalletKey) {
	value.Destroy()
}

// Configuration for creating a wallet repository.
//
// Rate limiting is a repository-wide setting: every wallet the repository
// hands out shares one limiter, so there is no per-wallet equivalent.
type WalletRepositoryConfig struct {
	// Proxy used by every mint operation. Omit for a direct connection.
	ProxyUrl *string
	// Client-side request pacing to start with. Omit it to keep the built-in
	// default.
	RateLimit *RateLimit
}

func (r *WalletRepositoryConfig) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.ProxyUrl)
	FfiDestroyerOptionalRateLimit{}.Destroy(r.RateLimit)
}

type FfiConverterWalletRepositoryConfig struct{}

var FfiConverterWalletRepositoryConfigINSTANCE = FfiConverterWalletRepositoryConfig{}

func (c FfiConverterWalletRepositoryConfig) Lift(rb RustBufferI) WalletRepositoryConfig {
	return LiftFromRustBuffer[WalletRepositoryConfig](c, rb)
}

func (c FfiConverterWalletRepositoryConfig) Read(reader io.Reader) WalletRepositoryConfig {
	return WalletRepositoryConfig{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalRateLimitINSTANCE.Read(reader),
	}
}

func (c FfiConverterWalletRepositoryConfig) Lower(value WalletRepositoryConfig) C.RustBuffer {
	return LowerIntoRustBuffer[WalletRepositoryConfig](c, value)
}

func (c FfiConverterWalletRepositoryConfig) LowerExternal(value WalletRepositoryConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[WalletRepositoryConfig](c, value))
}

func (c FfiConverterWalletRepositoryConfig) Write(writer io.Writer, value WalletRepositoryConfig) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ProxyUrl)
	FfiConverterOptionalRateLimitINSTANCE.Write(writer, value.RateLimit)
}

type FfiDestroyerWalletRepositoryConfig struct{}

func (_ FfiDestroyerWalletRepositoryConfig) Destroy(value WalletRepositoryConfig) {
	value.Destroy()
}

// Bitcoin network for on-chain address validation.
//
// This determines which address prefixes are accepted when parsing a BIP 321
// `bitcoin:` URI that contains an on-chain component.
//
// ```text
// val parsed = parseBip321PaymentInstruction(
// "bitcoin:bc1qar0s...?creq=CREQB1...",
// BitcoinNetwork.BITCOIN  // mainnet addresses only
// )
// ```
type BitcoinNetwork uint

const (
	// Bitcoin mainnet (addresses start with `bc1`, `1`, or `3`).
	BitcoinNetworkBitcoin BitcoinNetwork = 1
	// Bitcoin testnet (addresses start with `tb1`, `m`, or `n`).
	BitcoinNetworkTestnet BitcoinNetwork = 2
	// Bitcoin signet (addresses start with `tb1`).
	BitcoinNetworkSignet BitcoinNetwork = 3
	// Bitcoin regtest (addresses start with `bcrt1`).
	BitcoinNetworkRegtest BitcoinNetwork = 4
)

type FfiConverterBitcoinNetwork struct{}

var FfiConverterBitcoinNetworkINSTANCE = FfiConverterBitcoinNetwork{}

func (c FfiConverterBitcoinNetwork) Lift(rb RustBufferI) BitcoinNetwork {
	return LiftFromRustBuffer[BitcoinNetwork](c, rb)
}

func (c FfiConverterBitcoinNetwork) Lower(value BitcoinNetwork) C.RustBuffer {
	return LowerIntoRustBuffer[BitcoinNetwork](c, value)
}

func (c FfiConverterBitcoinNetwork) LowerExternal(value BitcoinNetwork) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[BitcoinNetwork](c, value))
}
func (FfiConverterBitcoinNetwork) Read(reader io.Reader) BitcoinNetwork {
	id := readInt32(reader)
	return BitcoinNetwork(id)
}

func (FfiConverterBitcoinNetwork) Write(writer io.Writer, value BitcoinNetwork) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerBitcoinNetwork struct{}

func (_ FfiDestroyerBitcoinNetwork) Destroy(value BitcoinNetwork) {
}

// FFI-compatible Currency Unit
type CurrencyUnit interface {
	Destroy()
}
type CurrencyUnitSat struct {
}

func (e CurrencyUnitSat) Destroy() {
}

type CurrencyUnitMsat struct {
}

func (e CurrencyUnitMsat) Destroy() {
}

type CurrencyUnitUsd struct {
}

func (e CurrencyUnitUsd) Destroy() {
}

type CurrencyUnitEur struct {
}

func (e CurrencyUnitEur) Destroy() {
}

type CurrencyUnitAuth struct {
}

func (e CurrencyUnitAuth) Destroy() {
}

type CurrencyUnitCustom struct {
	Unit string
}

func (e CurrencyUnitCustom) Destroy() {
	FfiDestroyerString{}.Destroy(e.Unit)
}

type FfiConverterCurrencyUnit struct{}

var FfiConverterCurrencyUnitINSTANCE = FfiConverterCurrencyUnit{}

func (c FfiConverterCurrencyUnit) Lift(rb RustBufferI) CurrencyUnit {
	return LiftFromRustBuffer[CurrencyUnit](c, rb)
}

func (c FfiConverterCurrencyUnit) Lower(value CurrencyUnit) C.RustBuffer {
	return LowerIntoRustBuffer[CurrencyUnit](c, value)
}

func (c FfiConverterCurrencyUnit) LowerExternal(value CurrencyUnit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[CurrencyUnit](c, value))
}
func (FfiConverterCurrencyUnit) Read(reader io.Reader) CurrencyUnit {
	id := readInt32(reader)
	switch id {
	case 1:
		return CurrencyUnitSat{}
	case 2:
		return CurrencyUnitMsat{}
	case 3:
		return CurrencyUnitUsd{}
	case 4:
		return CurrencyUnitEur{}
	case 5:
		return CurrencyUnitAuth{}
	case 6:
		return CurrencyUnitCustom{
			FfiConverterStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterCurrencyUnit.Read()", id))
	}
}

func (FfiConverterCurrencyUnit) Write(writer io.Writer, value CurrencyUnit) {
	switch variant_value := value.(type) {
	case CurrencyUnitSat:
		writeInt32(writer, 1)
	case CurrencyUnitMsat:
		writeInt32(writer, 2)
	case CurrencyUnitUsd:
		writeInt32(writer, 3)
	case CurrencyUnitEur:
		writeInt32(writer, 4)
	case CurrencyUnitAuth:
		writeInt32(writer, 5)
	case CurrencyUnitCustom:
		writeInt32(writer, 6)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Unit)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterCurrencyUnit.Write", value))
	}
}

type FfiDestroyerCurrencyUnit struct{}

func (_ FfiDestroyerCurrencyUnit) Destroy(value CurrencyUnit) {
	value.Destroy()
}

// FFI Error type that wraps CDK errors for cross-language use
//
// This simplified error type uses protocol-compliant error codes from `ErrorCode`
// in `cdk-common`, reducing duplication while providing structured error information
// to FFI consumers.
type FfiError struct {
	err error
}

// Convience method to turn *FfiError into error
// Avoiding treating nil pointer as non nil error interface
func (err *FfiError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err FfiError) Error() string {
	return fmt.Sprintf("FfiError: %s", err.err.Error())
}

func (err FfiError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrFfiErrorCdk = fmt.Errorf("FfiErrorCdk")
var ErrFfiErrorInternal = fmt.Errorf("FfiErrorInternal")
var ErrFfiErrorPaymentRequestDeliveryFailed = fmt.Errorf("FfiErrorPaymentRequestDeliveryFailed")

// Variant structs
// CDK error with protocol-compliant error code
// The code corresponds to the Cashu protocol error codes (e.g., 11001, 20001, etc.)
type FfiErrorCdk struct {
	Code         uint32
	ErrorMessage string
}

// CDK error with protocol-compliant error code
// The code corresponds to the Cashu protocol error codes (e.g., 11001, 20001, etc.)
func NewFfiErrorCdk(
	code uint32,
	errorMessage string,
) *FfiError {
	return &FfiError{err: &FfiErrorCdk{
		Code:         code,
		ErrorMessage: errorMessage}}
}

func (e FfiErrorCdk) destroy() {
	FfiDestroyerUint32{}.Destroy(e.Code)
	FfiDestroyerString{}.Destroy(e.ErrorMessage)
}

func (err FfiErrorCdk) Error() string {
	return fmt.Sprint("Cdk",
		": ",

		"Code=",
		err.Code,
		", ",
		"ErrorMessage=",
		err.ErrorMessage,
	)
}

func (self FfiErrorCdk) Is(target error) bool {
	return target == ErrFfiErrorCdk
}

// Internal/infrastructure error (no protocol error code)
// Used for errors that don't map to Cashu protocol codes
type FfiErrorInternal struct {
	ErrorMessage string
}

// Internal/infrastructure error (no protocol error code)
// Used for errors that don't map to Cashu protocol codes
func NewFfiErrorInternal(
	errorMessage string,
) *FfiError {
	return &FfiError{err: &FfiErrorInternal{
		ErrorMessage: errorMessage}}
}

func (e FfiErrorInternal) destroy() {
	FfiDestroyerString{}.Destroy(e.ErrorMessage)
}

func (err FfiErrorInternal) Error() string {
	return fmt.Sprint("Internal",
		": ",

		"ErrorMessage=",
		err.ErrorMessage,
	)
}

func (self FfiErrorInternal) Is(target error) bool {
	return target == ErrFfiErrorInternal
}

// A payment token was created but transport delivery failed.
//
// The token remains pending. Pass `operation_id` to `Wallet.revoke_send`
// to reclaim it if the receiver has not already claimed it.
type FfiErrorPaymentRequestDeliveryFailed struct {
	OperationId  string
	ErrorMessage string
}

// A payment token was created but transport delivery failed.
//
// The token remains pending. Pass `operation_id` to `Wallet.revoke_send`
// to reclaim it if the receiver has not already claimed it.
func NewFfiErrorPaymentRequestDeliveryFailed(
	operationId string,
	errorMessage string,
) *FfiError {
	return &FfiError{err: &FfiErrorPaymentRequestDeliveryFailed{
		OperationId:  operationId,
		ErrorMessage: errorMessage}}
}

func (e FfiErrorPaymentRequestDeliveryFailed) destroy() {
	FfiDestroyerString{}.Destroy(e.OperationId)
	FfiDestroyerString{}.Destroy(e.ErrorMessage)
}

func (err FfiErrorPaymentRequestDeliveryFailed) Error() string {
	return fmt.Sprint("PaymentRequestDeliveryFailed",
		": ",

		"OperationId=",
		err.OperationId,
		", ",
		"ErrorMessage=",
		err.ErrorMessage,
	)
}

func (self FfiErrorPaymentRequestDeliveryFailed) Is(target error) bool {
	return target == ErrFfiErrorPaymentRequestDeliveryFailed
}

type FfiConverterFfiError struct{}

var FfiConverterFfiErrorINSTANCE = FfiConverterFfiError{}

func (c FfiConverterFfiError) Lift(eb RustBufferI) *FfiError {
	return LiftFromRustBuffer[*FfiError](c, eb)
}

func (c FfiConverterFfiError) Lower(value *FfiError) C.RustBuffer {
	return LowerIntoRustBuffer[*FfiError](c, value)
}

func (c FfiConverterFfiError) LowerExternal(value *FfiError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*FfiError](c, value))
}

func (c FfiConverterFfiError) Read(reader io.Reader) *FfiError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &FfiError{&FfiErrorCdk{
			Code:         FfiConverterUint32INSTANCE.Read(reader),
			ErrorMessage: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 2:
		return &FfiError{&FfiErrorInternal{
			ErrorMessage: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 3:
		return &FfiError{&FfiErrorPaymentRequestDeliveryFailed{
			OperationId:  FfiConverterStringINSTANCE.Read(reader),
			ErrorMessage: FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterFfiError.Read()", errorID))
	}
}

func (c FfiConverterFfiError) Write(writer io.Writer, value *FfiError) {
	switch variantValue := value.err.(type) {
	case *FfiErrorCdk:
		writeInt32(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, variantValue.Code)
		FfiConverterStringINSTANCE.Write(writer, variantValue.ErrorMessage)
	case *FfiErrorInternal:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variantValue.ErrorMessage)
	case *FfiErrorPaymentRequestDeliveryFailed:
		writeInt32(writer, 3)
		FfiConverterStringINSTANCE.Write(writer, variantValue.OperationId)
		FfiConverterStringINSTANCE.Write(writer, variantValue.ErrorMessage)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterFfiError.Write", value))
	}
}

type FfiDestroyerFfiError struct{}

func (_ FfiDestroyerFfiError) Destroy(value *FfiError) {
	switch variantValue := value.err.(type) {
	case FfiErrorCdk:
		variantValue.destroy()
	case FfiErrorInternal:
		variantValue.destroy()
	case FfiErrorPaymentRequestDeliveryFailed:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerFfiError.Destroy", value))
	}
}

// Policy controlling how keysets are loaded
type KeysetLoadPolicy uint

const (
	// Use in-memory cache and local database only. Never contacts the network.
	KeysetLoadPolicyCacheOnly KeysetLoadPolicy = 1
	// Check cache first (respects TTL). Falls back to database, then network.
	KeysetLoadPolicyCacheThenNetwork KeysetLoadPolicy = 2
	// Always fetch fresh data from the mint over the network.
	KeysetLoadPolicyRefresh KeysetLoadPolicy = 3
)

type FfiConverterKeysetLoadPolicy struct{}

var FfiConverterKeysetLoadPolicyINSTANCE = FfiConverterKeysetLoadPolicy{}

func (c FfiConverterKeysetLoadPolicy) Lift(rb RustBufferI) KeysetLoadPolicy {
	return LiftFromRustBuffer[KeysetLoadPolicy](c, rb)
}

func (c FfiConverterKeysetLoadPolicy) Lower(value KeysetLoadPolicy) C.RustBuffer {
	return LowerIntoRustBuffer[KeysetLoadPolicy](c, value)
}

func (c FfiConverterKeysetLoadPolicy) LowerExternal(value KeysetLoadPolicy) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[KeysetLoadPolicy](c, value))
}
func (FfiConverterKeysetLoadPolicy) Read(reader io.Reader) KeysetLoadPolicy {
	id := readInt32(reader)
	return KeysetLoadPolicy(id)
}

func (FfiConverterKeysetLoadPolicy) Write(writer io.Writer, value KeysetLoadPolicy) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerKeysetLoadPolicy struct{}

func (_ FfiDestroyerKeysetLoadPolicy) Destroy(value KeysetLoadPolicy) {
}

// Result of async-preferred melt confirmation.
//
// `Paid` means the melt finalized during confirmation. `Pending` means the
// mint accepted the melt for asynchronous processing; call
// [`PendingMelt::wait`] to complete the normal app flow.
type MeltConfirmOutcome interface {
	Destroy()
}

// Melt finalized during confirmation.
type MeltConfirmOutcomePaid struct {
	Finalized FinalizedMelt
}

func (e MeltConfirmOutcomePaid) Destroy() {
	FfiDestroyerFinalizedMelt{}.Destroy(e.Finalized)
}

// Mint accepted async melt processing and the payment is still pending.
type MeltConfirmOutcomePending struct {
	Pending *PendingMelt
}

func (e MeltConfirmOutcomePending) Destroy() {
	FfiDestroyerPendingMelt{}.Destroy(e.Pending)
}

type FfiConverterMeltConfirmOutcome struct{}

var FfiConverterMeltConfirmOutcomeINSTANCE = FfiConverterMeltConfirmOutcome{}

func (c FfiConverterMeltConfirmOutcome) Lift(rb RustBufferI) MeltConfirmOutcome {
	return LiftFromRustBuffer[MeltConfirmOutcome](c, rb)
}

func (c FfiConverterMeltConfirmOutcome) Lower(value MeltConfirmOutcome) C.RustBuffer {
	return LowerIntoRustBuffer[MeltConfirmOutcome](c, value)
}

func (c FfiConverterMeltConfirmOutcome) LowerExternal(value MeltConfirmOutcome) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltConfirmOutcome](c, value))
}
func (FfiConverterMeltConfirmOutcome) Read(reader io.Reader) MeltConfirmOutcome {
	id := readInt32(reader)
	switch id {
	case 1:
		return MeltConfirmOutcomePaid{
			FfiConverterFinalizedMeltINSTANCE.Read(reader),
		}
	case 2:
		return MeltConfirmOutcomePending{
			FfiConverterPendingMeltINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMeltConfirmOutcome.Read()", id))
	}
}

func (FfiConverterMeltConfirmOutcome) Write(writer io.Writer, value MeltConfirmOutcome) {
	switch variant_value := value.(type) {
	case MeltConfirmOutcomePaid:
		writeInt32(writer, 1)
		FfiConverterFinalizedMeltINSTANCE.Write(writer, variant_value.Finalized)
	case MeltConfirmOutcomePending:
		writeInt32(writer, 2)
		FfiConverterPendingMeltINSTANCE.Write(writer, variant_value.Pending)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMeltConfirmOutcome.Write", value))
	}
}

type FfiDestroyerMeltConfirmOutcome struct{}

func (_ FfiDestroyerMeltConfirmOutcome) Destroy(value MeltConfirmOutcome) {
	value.Destroy()
}

// FFI-compatible MeltOptions
type MeltOptions interface {
	Destroy()
}

// MPP (Multi-Part Payments) options
type MeltOptionsMpp struct {
	Amount Amount
}

func (e MeltOptionsMpp) Destroy() {
	FfiDestroyerAmount{}.Destroy(e.Amount)
}

// Amountless options
type MeltOptionsAmountless struct {
	AmountMsat Amount
}

func (e MeltOptionsAmountless) Destroy() {
	FfiDestroyerAmount{}.Destroy(e.AmountMsat)
}

type FfiConverterMeltOptions struct{}

var FfiConverterMeltOptionsINSTANCE = FfiConverterMeltOptions{}

func (c FfiConverterMeltOptions) Lift(rb RustBufferI) MeltOptions {
	return LiftFromRustBuffer[MeltOptions](c, rb)
}

func (c FfiConverterMeltOptions) Lower(value MeltOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MeltOptions](c, value)
}

func (c FfiConverterMeltOptions) LowerExternal(value MeltOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MeltOptions](c, value))
}
func (FfiConverterMeltOptions) Read(reader io.Reader) MeltOptions {
	id := readInt32(reader)
	switch id {
	case 1:
		return MeltOptionsMpp{
			FfiConverterAmountINSTANCE.Read(reader),
		}
	case 2:
		return MeltOptionsAmountless{
			FfiConverterAmountINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMeltOptions.Read()", id))
	}
}

func (FfiConverterMeltOptions) Write(writer io.Writer, value MeltOptions) {
	switch variant_value := value.(type) {
	case MeltOptionsMpp:
		writeInt32(writer, 1)
		FfiConverterAmountINSTANCE.Write(writer, variant_value.Amount)
	case MeltOptionsAmountless:
		writeInt32(writer, 2)
		FfiConverterAmountINSTANCE.Write(writer, variant_value.AmountMsat)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMeltOptions.Write", value))
	}
}

type FfiDestroyerMeltOptions struct{}

func (_ FfiDestroyerMeltOptions) Destroy(value MeltOptions) {
	value.Destroy()
}

// FFI-compatible NotificationPayload
type NotificationPayload interface {
	Destroy()
}

// Proof state update
type NotificationPayloadProofState struct {
	ProofStates []ProofStateUpdate
}

func (e NotificationPayloadProofState) Destroy() {
	FfiDestroyerSequenceProofStateUpdate{}.Destroy(e.ProofStates)
}

// Mint quote update
type NotificationPayloadMintQuoteUpdate struct {
	Quote MintQuoteBolt11Response
}

func (e NotificationPayloadMintQuoteUpdate) Destroy() {
	FfiDestroyerMintQuoteBolt11Response{}.Destroy(e.Quote)
}

// Melt quote update
type NotificationPayloadMeltQuoteUpdate struct {
	Quote MeltQuoteBolt11Response
}

func (e NotificationPayloadMeltQuoteUpdate) Destroy() {
	FfiDestroyerMeltQuoteBolt11Response{}.Destroy(e.Quote)
}

// Onchain mint quote update
type NotificationPayloadMintQuoteOnchainUpdate struct {
	Quote MintQuoteOnchainResponse
}

func (e NotificationPayloadMintQuoteOnchainUpdate) Destroy() {
	FfiDestroyerMintQuoteOnchainResponse{}.Destroy(e.Quote)
}

// Onchain melt quote update
type NotificationPayloadMeltQuoteOnchainUpdate struct {
	Quote MeltQuoteOnchainResponse
}

func (e NotificationPayloadMeltQuoteOnchainUpdate) Destroy() {
	FfiDestroyerMeltQuoteOnchainResponse{}.Destroy(e.Quote)
}

type FfiConverterNotificationPayload struct{}

var FfiConverterNotificationPayloadINSTANCE = FfiConverterNotificationPayload{}

func (c FfiConverterNotificationPayload) Lift(rb RustBufferI) NotificationPayload {
	return LiftFromRustBuffer[NotificationPayload](c, rb)
}

func (c FfiConverterNotificationPayload) Lower(value NotificationPayload) C.RustBuffer {
	return LowerIntoRustBuffer[NotificationPayload](c, value)
}

func (c FfiConverterNotificationPayload) LowerExternal(value NotificationPayload) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[NotificationPayload](c, value))
}
func (FfiConverterNotificationPayload) Read(reader io.Reader) NotificationPayload {
	id := readInt32(reader)
	switch id {
	case 1:
		return NotificationPayloadProofState{
			FfiConverterSequenceProofStateUpdateINSTANCE.Read(reader),
		}
	case 2:
		return NotificationPayloadMintQuoteUpdate{
			FfiConverterMintQuoteBolt11ResponseINSTANCE.Read(reader),
		}
	case 3:
		return NotificationPayloadMeltQuoteUpdate{
			FfiConverterMeltQuoteBolt11ResponseINSTANCE.Read(reader),
		}
	case 4:
		return NotificationPayloadMintQuoteOnchainUpdate{
			FfiConverterMintQuoteOnchainResponseINSTANCE.Read(reader),
		}
	case 5:
		return NotificationPayloadMeltQuoteOnchainUpdate{
			FfiConverterMeltQuoteOnchainResponseINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterNotificationPayload.Read()", id))
	}
}

func (FfiConverterNotificationPayload) Write(writer io.Writer, value NotificationPayload) {
	switch variant_value := value.(type) {
	case NotificationPayloadProofState:
		writeInt32(writer, 1)
		FfiConverterSequenceProofStateUpdateINSTANCE.Write(writer, variant_value.ProofStates)
	case NotificationPayloadMintQuoteUpdate:
		writeInt32(writer, 2)
		FfiConverterMintQuoteBolt11ResponseINSTANCE.Write(writer, variant_value.Quote)
	case NotificationPayloadMeltQuoteUpdate:
		writeInt32(writer, 3)
		FfiConverterMeltQuoteBolt11ResponseINSTANCE.Write(writer, variant_value.Quote)
	case NotificationPayloadMintQuoteOnchainUpdate:
		writeInt32(writer, 4)
		FfiConverterMintQuoteOnchainResponseINSTANCE.Write(writer, variant_value.Quote)
	case NotificationPayloadMeltQuoteOnchainUpdate:
		writeInt32(writer, 5)
		FfiConverterMeltQuoteOnchainResponseINSTANCE.Write(writer, variant_value.Quote)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterNotificationPayload.Write", value))
	}
}

type FfiDestroyerNotificationPayload struct{}

func (_ FfiDestroyerNotificationPayload) Destroy(value NotificationPayload) {
	value.Destroy()
}

// FFI-compatible P2PK locked proof send mode
type P2pkLockedProofSendMode uint

const (
	// Swap locked proofs into fresh proofs before creating the token
	P2pkLockedProofSendModeSwap P2pkLockedProofSendMode = 1
	// Sign locked proofs and include them directly in the token
	P2pkLockedProofSendModeSignAndSend P2pkLockedProofSendMode = 2
)

type FfiConverterP2pkLockedProofSendMode struct{}

var FfiConverterP2pkLockedProofSendModeINSTANCE = FfiConverterP2pkLockedProofSendMode{}

func (c FfiConverterP2pkLockedProofSendMode) Lift(rb RustBufferI) P2pkLockedProofSendMode {
	return LiftFromRustBuffer[P2pkLockedProofSendMode](c, rb)
}

func (c FfiConverterP2pkLockedProofSendMode) Lower(value P2pkLockedProofSendMode) C.RustBuffer {
	return LowerIntoRustBuffer[P2pkLockedProofSendMode](c, value)
}

func (c FfiConverterP2pkLockedProofSendMode) LowerExternal(value P2pkLockedProofSendMode) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[P2pkLockedProofSendMode](c, value))
}
func (FfiConverterP2pkLockedProofSendMode) Read(reader io.Reader) P2pkLockedProofSendMode {
	id := readInt32(reader)
	return P2pkLockedProofSendMode(id)
}

func (FfiConverterP2pkLockedProofSendMode) Write(writer io.Writer, value P2pkLockedProofSendMode) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerP2pkLockedProofSendMode struct{}

func (_ FfiDestroyerP2pkLockedProofSendMode) Destroy(value P2pkLockedProofSendMode) {
}

// FFI-compatible PaymentMethod
type PaymentMethod interface {
	Destroy()
}

// Bolt11 payment type
type PaymentMethodBolt11 struct {
}

func (e PaymentMethodBolt11) Destroy() {
}

// Bolt12 payment type
type PaymentMethodBolt12 struct {
}

func (e PaymentMethodBolt12) Destroy() {
}

// Onchain Bitcoin payment type
type PaymentMethodOnchain struct {
}

func (e PaymentMethodOnchain) Destroy() {
}

// Custom payment type
type PaymentMethodCustom struct {
	Method string
}

func (e PaymentMethodCustom) Destroy() {
	FfiDestroyerString{}.Destroy(e.Method)
}

type FfiConverterPaymentMethod struct{}

var FfiConverterPaymentMethodINSTANCE = FfiConverterPaymentMethod{}

func (c FfiConverterPaymentMethod) Lift(rb RustBufferI) PaymentMethod {
	return LiftFromRustBuffer[PaymentMethod](c, rb)
}

func (c FfiConverterPaymentMethod) Lower(value PaymentMethod) C.RustBuffer {
	return LowerIntoRustBuffer[PaymentMethod](c, value)
}

func (c FfiConverterPaymentMethod) LowerExternal(value PaymentMethod) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PaymentMethod](c, value))
}
func (FfiConverterPaymentMethod) Read(reader io.Reader) PaymentMethod {
	id := readInt32(reader)
	switch id {
	case 1:
		return PaymentMethodBolt11{}
	case 2:
		return PaymentMethodBolt12{}
	case 3:
		return PaymentMethodOnchain{}
	case 4:
		return PaymentMethodCustom{
			FfiConverterStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterPaymentMethod.Read()", id))
	}
}

func (FfiConverterPaymentMethod) Write(writer io.Writer, value PaymentMethod) {
	switch variant_value := value.(type) {
	case PaymentMethodBolt11:
		writeInt32(writer, 1)
	case PaymentMethodBolt12:
		writeInt32(writer, 2)
	case PaymentMethodOnchain:
		writeInt32(writer, 3)
	case PaymentMethodCustom:
		writeInt32(writer, 4)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Method)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterPaymentMethod.Write", value))
	}
}

type FfiDestroyerPaymentMethod struct{}

func (_ FfiDestroyerPaymentMethod) Destroy(value PaymentMethod) {
	value.Destroy()
}

// Type of Lightning payment request
type PaymentType uint

const (
	// Bolt11 invoice
	PaymentTypeBolt11 PaymentType = 1
	// Bolt12 offer
	PaymentTypeBolt12 PaymentType = 2
)

type FfiConverterPaymentType struct{}

var FfiConverterPaymentTypeINSTANCE = FfiConverterPaymentType{}

func (c FfiConverterPaymentType) Lift(rb RustBufferI) PaymentType {
	return LiftFromRustBuffer[PaymentType](c, rb)
}

func (c FfiConverterPaymentType) Lower(value PaymentType) C.RustBuffer {
	return LowerIntoRustBuffer[PaymentType](c, value)
}

func (c FfiConverterPaymentType) LowerExternal(value PaymentType) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PaymentType](c, value))
}
func (FfiConverterPaymentType) Read(reader io.Reader) PaymentType {
	id := readInt32(reader)
	return PaymentType(id)
}

func (FfiConverterPaymentType) Write(writer io.Writer, value PaymentType) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerPaymentType struct{}

func (_ FfiDestroyerPaymentType) Destroy(value PaymentType) {
}

// FFI-compatible Proof state
type ProofState uint

const (
	ProofStateUnspent      ProofState = 1
	ProofStatePending      ProofState = 2
	ProofStateSpent        ProofState = 3
	ProofStateReserved     ProofState = 4
	ProofStatePendingSpent ProofState = 5
)

type FfiConverterProofState struct{}

var FfiConverterProofStateINSTANCE = FfiConverterProofState{}

func (c FfiConverterProofState) Lift(rb RustBufferI) ProofState {
	return LiftFromRustBuffer[ProofState](c, rb)
}

func (c FfiConverterProofState) Lower(value ProofState) C.RustBuffer {
	return LowerIntoRustBuffer[ProofState](c, value)
}

func (c FfiConverterProofState) LowerExternal(value ProofState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ProofState](c, value))
}
func (FfiConverterProofState) Read(reader io.Reader) ProofState {
	id := readInt32(reader)
	return ProofState(id)
}

func (FfiConverterProofState) Write(writer io.Writer, value ProofState) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerProofState struct{}

func (_ FfiDestroyerProofState) Destroy(value ProofState) {
}

// FFI-compatible QuoteState
type QuoteState uint

const (
	QuoteStateUnpaid  QuoteState = 1
	QuoteStatePaid    QuoteState = 2
	QuoteStatePending QuoteState = 3
	QuoteStateIssued  QuoteState = 4
)

type FfiConverterQuoteState struct{}

var FfiConverterQuoteStateINSTANCE = FfiConverterQuoteState{}

func (c FfiConverterQuoteState) Lift(rb RustBufferI) QuoteState {
	return LiftFromRustBuffer[QuoteState](c, rb)
}

func (c FfiConverterQuoteState) Lower(value QuoteState) C.RustBuffer {
	return LowerIntoRustBuffer[QuoteState](c, value)
}

func (c FfiConverterQuoteState) LowerExternal(value QuoteState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[QuoteState](c, value))
}
func (FfiConverterQuoteState) Read(reader io.Reader) QuoteState {
	id := readInt32(reader)
	return QuoteState(id)
}

func (FfiConverterQuoteState) Write(writer io.Writer, value QuoteState) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerQuoteState struct{}

func (_ FfiDestroyerQuoteState) Destroy(value QuoteState) {
}

// Client-side request pacing for a wallet's mint traffic.
//
// A wallet starts with whatever [`WalletConfig::rate_limit`] selected, which
// defaults to the built-in pacing so it stays under a mint's per-minute request
// cap. Pass one of these to [`Wallet::set_rate_limit`] to change it on a live
// wallet.
//
// # Example
//
// ```ignore
// // Turn pacing off (e.g. a mint with no request cap).
// wallet.set_rate_limit(RateLimit::Disabled)?;
//
// // Restore the built-in default pacing.
// wallet.set_rate_limit(RateLimit::Default)?;
//
// // Allow a burst of 20 requests, refilling 60 per minute.
// wallet.set_rate_limit(RateLimit::Custom {
// capacity: 20,
// refill_per_minute: 60,
// })?;
// ```
type RateLimit interface {
	Destroy()
}

// Built-in default pacing (capacity 20, refill 20/min).
type RateLimitDefault struct {
}

func (e RateLimitDefault) Destroy() {
}

// No client-side pacing.
type RateLimitDisabled struct {
}

func (e RateLimitDisabled) Destroy() {
}

// Custom burst capacity and per-minute refill. Both must be non-zero.
type RateLimitCustom struct {
	Capacity        uint32
	RefillPerMinute uint32
}

func (e RateLimitCustom) Destroy() {
	FfiDestroyerUint32{}.Destroy(e.Capacity)
	FfiDestroyerUint32{}.Destroy(e.RefillPerMinute)
}

type FfiConverterRateLimit struct{}

var FfiConverterRateLimitINSTANCE = FfiConverterRateLimit{}

func (c FfiConverterRateLimit) Lift(rb RustBufferI) RateLimit {
	return LiftFromRustBuffer[RateLimit](c, rb)
}

func (c FfiConverterRateLimit) Lower(value RateLimit) C.RustBuffer {
	return LowerIntoRustBuffer[RateLimit](c, value)
}

func (c FfiConverterRateLimit) LowerExternal(value RateLimit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RateLimit](c, value))
}
func (FfiConverterRateLimit) Read(reader io.Reader) RateLimit {
	id := readInt32(reader)
	switch id {
	case 1:
		return RateLimitDefault{}
	case 2:
		return RateLimitDisabled{}
	case 3:
		return RateLimitCustom{
			FfiConverterUint32INSTANCE.Read(reader),
			FfiConverterUint32INSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterRateLimit.Read()", id))
	}
}

func (FfiConverterRateLimit) Write(writer io.Writer, value RateLimit) {
	switch variant_value := value.(type) {
	case RateLimitDefault:
		writeInt32(writer, 1)
	case RateLimitDisabled:
		writeInt32(writer, 2)
	case RateLimitCustom:
		writeInt32(writer, 3)
		FfiConverterUint32INSTANCE.Write(writer, variant_value.Capacity)
		FfiConverterUint32INSTANCE.Write(writer, variant_value.RefillPerMinute)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterRateLimit.Write", value))
	}
}

type FfiDestroyerRateLimit struct{}

func (_ FfiDestroyerRateLimit) Destroy(value RateLimit) {
	value.Destroy()
}

// FFI-compatible SendKind
type SendKind interface {
	Destroy()
}

// Allow online swap before send if wallet does not have exact amount
type SendKindOnlineExact struct {
}

func (e SendKindOnlineExact) Destroy() {
}

// Prefer offline send if difference is less than tolerance
type SendKindOnlineTolerance struct {
	Tolerance Amount
}

func (e SendKindOnlineTolerance) Destroy() {
	FfiDestroyerAmount{}.Destroy(e.Tolerance)
}

// Wallet cannot do an online swap and selected proof must be exactly send amount
type SendKindOfflineExact struct {
}

func (e SendKindOfflineExact) Destroy() {
}

// Wallet must remain offline but can over pay if below tolerance
type SendKindOfflineTolerance struct {
	Tolerance Amount
}

func (e SendKindOfflineTolerance) Destroy() {
	FfiDestroyerAmount{}.Destroy(e.Tolerance)
}

type FfiConverterSendKind struct{}

var FfiConverterSendKindINSTANCE = FfiConverterSendKind{}

func (c FfiConverterSendKind) Lift(rb RustBufferI) SendKind {
	return LiftFromRustBuffer[SendKind](c, rb)
}

func (c FfiConverterSendKind) Lower(value SendKind) C.RustBuffer {
	return LowerIntoRustBuffer[SendKind](c, value)
}

func (c FfiConverterSendKind) LowerExternal(value SendKind) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SendKind](c, value))
}
func (FfiConverterSendKind) Read(reader io.Reader) SendKind {
	id := readInt32(reader)
	switch id {
	case 1:
		return SendKindOnlineExact{}
	case 2:
		return SendKindOnlineTolerance{
			FfiConverterAmountINSTANCE.Read(reader),
		}
	case 3:
		return SendKindOfflineExact{}
	case 4:
		return SendKindOfflineTolerance{
			FfiConverterAmountINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterSendKind.Read()", id))
	}
}

func (FfiConverterSendKind) Write(writer io.Writer, value SendKind) {
	switch variant_value := value.(type) {
	case SendKindOnlineExact:
		writeInt32(writer, 1)
	case SendKindOnlineTolerance:
		writeInt32(writer, 2)
		FfiConverterAmountINSTANCE.Write(writer, variant_value.Tolerance)
	case SendKindOfflineExact:
		writeInt32(writer, 3)
	case SendKindOfflineTolerance:
		writeInt32(writer, 4)
		FfiConverterAmountINSTANCE.Write(writer, variant_value.Tolerance)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterSendKind.Write", value))
	}
}

type FfiDestroyerSendKind struct{}

func (_ FfiDestroyerSendKind) Destroy(value SendKind) {
	value.Destroy()
}

// FFI-compatible SpendingConditions
type SpendingConditions interface {
	Destroy()
}

// P2PK (Pay to Public Key) conditions
type SpendingConditionsP2pk struct {
	Pubkey     string
	Conditions *Conditions
}

func (e SpendingConditionsP2pk) Destroy() {
	FfiDestroyerString{}.Destroy(e.Pubkey)
	FfiDestroyerOptionalConditions{}.Destroy(e.Conditions)
}

// HTLC (Hash Time Locked Contract) conditions
type SpendingConditionsHtlc struct {
	Hash       string
	Conditions *Conditions
}

func (e SpendingConditionsHtlc) Destroy() {
	FfiDestroyerString{}.Destroy(e.Hash)
	FfiDestroyerOptionalConditions{}.Destroy(e.Conditions)
}

type FfiConverterSpendingConditions struct{}

var FfiConverterSpendingConditionsINSTANCE = FfiConverterSpendingConditions{}

func (c FfiConverterSpendingConditions) Lift(rb RustBufferI) SpendingConditions {
	return LiftFromRustBuffer[SpendingConditions](c, rb)
}

func (c FfiConverterSpendingConditions) Lower(value SpendingConditions) C.RustBuffer {
	return LowerIntoRustBuffer[SpendingConditions](c, value)
}

func (c FfiConverterSpendingConditions) LowerExternal(value SpendingConditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SpendingConditions](c, value))
}
func (FfiConverterSpendingConditions) Read(reader io.Reader) SpendingConditions {
	id := readInt32(reader)
	switch id {
	case 1:
		return SpendingConditionsP2pk{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterOptionalConditionsINSTANCE.Read(reader),
		}
	case 2:
		return SpendingConditionsHtlc{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterOptionalConditionsINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterSpendingConditions.Read()", id))
	}
}

func (FfiConverterSpendingConditions) Write(writer io.Writer, value SpendingConditions) {
	switch variant_value := value.(type) {
	case SpendingConditionsP2pk:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Pubkey)
		FfiConverterOptionalConditionsINSTANCE.Write(writer, variant_value.Conditions)
	case SpendingConditionsHtlc:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Hash)
		FfiConverterOptionalConditionsINSTANCE.Write(writer, variant_value.Conditions)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterSpendingConditions.Write", value))
	}
}

type FfiDestroyerSpendingConditions struct{}

func (_ FfiDestroyerSpendingConditions) Destroy(value SpendingConditions) {
	value.Destroy()
}

// FFI-compatible SplitTarget
type SplitTarget interface {
	Destroy()
}

// Default target; least amount of proofs
type SplitTargetNone struct {
}

func (e SplitTargetNone) Destroy() {
}

// Target amount for wallet to have most proofs that add up to value
type SplitTargetValue struct {
	Amount Amount
}

func (e SplitTargetValue) Destroy() {
	FfiDestroyerAmount{}.Destroy(e.Amount)
}

// Specific amounts to split into (must equal amount being split)
type SplitTargetValues struct {
	Amounts []Amount
}

func (e SplitTargetValues) Destroy() {
	FfiDestroyerSequenceAmount{}.Destroy(e.Amounts)
}

type FfiConverterSplitTarget struct{}

var FfiConverterSplitTargetINSTANCE = FfiConverterSplitTarget{}

func (c FfiConverterSplitTarget) Lift(rb RustBufferI) SplitTarget {
	return LiftFromRustBuffer[SplitTarget](c, rb)
}

func (c FfiConverterSplitTarget) Lower(value SplitTarget) C.RustBuffer {
	return LowerIntoRustBuffer[SplitTarget](c, value)
}

func (c FfiConverterSplitTarget) LowerExternal(value SplitTarget) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SplitTarget](c, value))
}
func (FfiConverterSplitTarget) Read(reader io.Reader) SplitTarget {
	id := readInt32(reader)
	switch id {
	case 1:
		return SplitTargetNone{}
	case 2:
		return SplitTargetValue{
			FfiConverterAmountINSTANCE.Read(reader),
		}
	case 3:
		return SplitTargetValues{
			FfiConverterSequenceAmountINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterSplitTarget.Read()", id))
	}
}

func (FfiConverterSplitTarget) Write(writer io.Writer, value SplitTarget) {
	switch variant_value := value.(type) {
	case SplitTargetNone:
		writeInt32(writer, 1)
	case SplitTargetValue:
		writeInt32(writer, 2)
		FfiConverterAmountINSTANCE.Write(writer, variant_value.Amount)
	case SplitTargetValues:
		writeInt32(writer, 3)
		FfiConverterSequenceAmountINSTANCE.Write(writer, variant_value.Amounts)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterSplitTarget.Write", value))
	}
}

type FfiDestroyerSplitTarget struct{}

func (_ FfiDestroyerSplitTarget) Destroy(value SplitTarget) {
	value.Destroy()
}

// FFI-compatible SubscriptionKind
type SubscriptionKind uint

const (
	// Bolt 11 Melt Quote
	SubscriptionKindBolt11MeltQuote SubscriptionKind = 1
	// Bolt 11 Mint Quote
	SubscriptionKindBolt11MintQuote SubscriptionKind = 2
	// Bolt 12 Mint Quote
	SubscriptionKindBolt12MintQuote SubscriptionKind = 3
	// Bolt 12 Melt Quote
	SubscriptionKindBolt12MeltQuote SubscriptionKind = 4
	// Onchain Mint Quote
	SubscriptionKindOnchainMintQuote SubscriptionKind = 5
	// Onchain Melt Quote
	SubscriptionKindOnchainMeltQuote SubscriptionKind = 6
	// Proof State
	SubscriptionKindProofState SubscriptionKind = 7
)

type FfiConverterSubscriptionKind struct{}

var FfiConverterSubscriptionKindINSTANCE = FfiConverterSubscriptionKind{}

func (c FfiConverterSubscriptionKind) Lift(rb RustBufferI) SubscriptionKind {
	return LiftFromRustBuffer[SubscriptionKind](c, rb)
}

func (c FfiConverterSubscriptionKind) Lower(value SubscriptionKind) C.RustBuffer {
	return LowerIntoRustBuffer[SubscriptionKind](c, value)
}

func (c FfiConverterSubscriptionKind) LowerExternal(value SubscriptionKind) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SubscriptionKind](c, value))
}
func (FfiConverterSubscriptionKind) Read(reader io.Reader) SubscriptionKind {
	id := readInt32(reader)
	return SubscriptionKind(id)
}

func (FfiConverterSubscriptionKind) Write(writer io.Writer, value SubscriptionKind) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerSubscriptionKind struct{}

func (_ FfiDestroyerSubscriptionKind) Destroy(value SubscriptionKind) {
}

// FFI-compatible TransactionDirection
type TransactionDirection uint

const (
	// Incoming transaction (i.e., receive or mint)
	TransactionDirectionIncoming TransactionDirection = 1
	// Outgoing transaction (i.e., send or melt)
	TransactionDirectionOutgoing TransactionDirection = 2
)

type FfiConverterTransactionDirection struct{}

var FfiConverterTransactionDirectionINSTANCE = FfiConverterTransactionDirection{}

func (c FfiConverterTransactionDirection) Lift(rb RustBufferI) TransactionDirection {
	return LiftFromRustBuffer[TransactionDirection](c, rb)
}

func (c FfiConverterTransactionDirection) Lower(value TransactionDirection) C.RustBuffer {
	return LowerIntoRustBuffer[TransactionDirection](c, value)
}

func (c FfiConverterTransactionDirection) LowerExternal(value TransactionDirection) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[TransactionDirection](c, value))
}
func (FfiConverterTransactionDirection) Read(reader io.Reader) TransactionDirection {
	id := readInt32(reader)
	return TransactionDirection(id)
}

func (FfiConverterTransactionDirection) Write(writer io.Writer, value TransactionDirection) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerTransactionDirection struct{}

func (_ FfiDestroyerTransactionDirection) Destroy(value TransactionDirection) {
}

// FFI-compatible transaction status.
type TransactionStatus uint

const (
	// The transaction is still in progress.
	TransactionStatusPending TransactionStatus = 1
	// The transaction completed successfully.
	TransactionStatusCompleted TransactionStatus = 2
	// The transaction failed or was revoked.
	TransactionStatusFailed TransactionStatus = 3
)

type FfiConverterTransactionStatus struct{}

var FfiConverterTransactionStatusINSTANCE = FfiConverterTransactionStatus{}

func (c FfiConverterTransactionStatus) Lift(rb RustBufferI) TransactionStatus {
	return LiftFromRustBuffer[TransactionStatus](c, rb)
}

func (c FfiConverterTransactionStatus) Lower(value TransactionStatus) C.RustBuffer {
	return LowerIntoRustBuffer[TransactionStatus](c, value)
}

func (c FfiConverterTransactionStatus) LowerExternal(value TransactionStatus) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[TransactionStatus](c, value))
}
func (FfiConverterTransactionStatus) Read(reader io.Reader) TransactionStatus {
	id := readInt32(reader)
	return TransactionStatus(id)
}

func (FfiConverterTransactionStatus) Write(writer io.Writer, value TransactionStatus) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerTransactionStatus struct{}

func (_ FfiDestroyerTransactionStatus) Destroy(value TransactionStatus) {
}

// Transport type for payment request delivery
type TransportType uint

const (
	// Nostr transport (privacy-preserving)
	TransportTypeNostr TransportType = 1
	// HTTP POST transport
	TransportTypeHttpPost TransportType = 2
)

type FfiConverterTransportType struct{}

var FfiConverterTransportTypeINSTANCE = FfiConverterTransportType{}

func (c FfiConverterTransportType) Lift(rb RustBufferI) TransportType {
	return LiftFromRustBuffer[TransportType](c, rb)
}

func (c FfiConverterTransportType) Lower(value TransportType) C.RustBuffer {
	return LowerIntoRustBuffer[TransportType](c, value)
}

func (c FfiConverterTransportType) LowerExternal(value TransportType) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[TransportType](c, value))
}
func (FfiConverterTransportType) Read(reader io.Reader) TransportType {
	id := readInt32(reader)
	return TransportType(id)
}

func (FfiConverterTransportType) Write(writer io.Writer, value TransportType) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerTransportType struct{}

func (_ FfiDestroyerTransportType) Destroy(value TransportType) {
}

// FFI-safe database type enum
type WalletDbBackend interface {
	Destroy()
}
type WalletDbBackendSqlite struct {
	Path string
}

func (e WalletDbBackendSqlite) Destroy() {
	FfiDestroyerString{}.Destroy(e.Path)
}

type FfiConverterWalletDbBackend struct{}

var FfiConverterWalletDbBackendINSTANCE = FfiConverterWalletDbBackend{}

func (c FfiConverterWalletDbBackend) Lift(rb RustBufferI) WalletDbBackend {
	return LiftFromRustBuffer[WalletDbBackend](c, rb)
}

func (c FfiConverterWalletDbBackend) Lower(value WalletDbBackend) C.RustBuffer {
	return LowerIntoRustBuffer[WalletDbBackend](c, value)
}

func (c FfiConverterWalletDbBackend) LowerExternal(value WalletDbBackend) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[WalletDbBackend](c, value))
}
func (FfiConverterWalletDbBackend) Read(reader io.Reader) WalletDbBackend {
	id := readInt32(reader)
	switch id {
	case 1:
		return WalletDbBackendSqlite{
			FfiConverterStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterWalletDbBackend.Read()", id))
	}
}

func (FfiConverterWalletDbBackend) Write(writer io.Writer, value WalletDbBackend) {
	switch variant_value := value.(type) {
	case WalletDbBackendSqlite:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Path)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterWalletDbBackend.Write", value))
	}
}

type FfiDestroyerWalletDbBackend struct{}

func (_ FfiDestroyerWalletDbBackend) Destroy(value WalletDbBackend) {
	value.Destroy()
}

// Unified wallet storage: either a built-in Rust backend or a custom
// foreign-language implementation of the `WalletDatabase` callback interface.
//
// Wallet methods can write to this store from FFI calls that mint, receive,
// recover, subscribe, or check quote/proof state. Mobile host apps own platform
// lifecycle handling for the chosen backend: use a durable app-owned location,
// pause or cancel wallet work when backgrounding unless background storage
// activity is intended, and use platform facilities such as iOS
// `beginBackgroundTask` when writes must finish after a lifecycle transition.
//
// This is an enum rather than accepting `WalletDatabase` directly because UniFFI
// does not support trait objects as constructor parameters — only callback interfaces
// wrapped in `Arc<dyn Trait>` inside an enum variant work across the FFI boundary.
type WalletStore interface {
	Destroy()
}
type WalletStoreSqlite struct {
	Path string
}

func (e WalletStoreSqlite) Destroy() {
	FfiDestroyerString{}.Destroy(e.Path)
}

type WalletStoreCustom struct {
	Db WalletDatabase
}

func (e WalletStoreCustom) Destroy() {
	FfiDestroyerWalletDatabase{}.Destroy(e.Db)
}

type FfiConverterWalletStore struct{}

var FfiConverterWalletStoreINSTANCE = FfiConverterWalletStore{}

func (c FfiConverterWalletStore) Lift(rb RustBufferI) WalletStore {
	return LiftFromRustBuffer[WalletStore](c, rb)
}

func (c FfiConverterWalletStore) Lower(value WalletStore) C.RustBuffer {
	return LowerIntoRustBuffer[WalletStore](c, value)
}

func (c FfiConverterWalletStore) LowerExternal(value WalletStore) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[WalletStore](c, value))
}
func (FfiConverterWalletStore) Read(reader io.Reader) WalletStore {
	id := readInt32(reader)
	switch id {
	case 1:
		return WalletStoreSqlite{
			FfiConverterStringINSTANCE.Read(reader),
		}
	case 2:
		return WalletStoreCustom{
			FfiConverterWalletDatabaseINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterWalletStore.Read()", id))
	}
}

func (FfiConverterWalletStore) Write(writer io.Writer, value WalletStore) {
	switch variant_value := value.(type) {
	case WalletStoreSqlite:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Path)
	case WalletStoreCustom:
		writeInt32(writer, 2)
		FfiConverterWalletDatabaseINSTANCE.Write(writer, variant_value.Db)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterWalletStore.Write", value))
	}
}

type FfiDestroyerWalletStore struct{}

func (_ FfiDestroyerWalletStore) Destroy(value WalletStore) {
	value.Destroy()
}

// FFI-compatible Witness
type Witness interface {
	Destroy()
}

// P2PK Witness
type WitnessP2pk struct {
	Signatures []string
}

func (e WitnessP2pk) Destroy() {
	FfiDestroyerSequenceString{}.Destroy(e.Signatures)
}

// HTLC Witness
type WitnessHtlc struct {
	Preimage   string
	Signatures *[]string
}

func (e WitnessHtlc) Destroy() {
	FfiDestroyerString{}.Destroy(e.Preimage)
	FfiDestroyerOptionalSequenceString{}.Destroy(e.Signatures)
}

type FfiConverterWitness struct{}

var FfiConverterWitnessINSTANCE = FfiConverterWitness{}

func (c FfiConverterWitness) Lift(rb RustBufferI) Witness {
	return LiftFromRustBuffer[Witness](c, rb)
}

func (c FfiConverterWitness) Lower(value Witness) C.RustBuffer {
	return LowerIntoRustBuffer[Witness](c, value)
}

func (c FfiConverterWitness) LowerExternal(value Witness) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Witness](c, value))
}
func (FfiConverterWitness) Read(reader io.Reader) Witness {
	id := readInt32(reader)
	switch id {
	case 1:
		return WitnessP2pk{
			FfiConverterSequenceStringINSTANCE.Read(reader),
		}
	case 2:
		return WitnessHtlc{
			FfiConverterStringINSTANCE.Read(reader),
			FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterWitness.Read()", id))
	}
}

func (FfiConverterWitness) Write(writer io.Writer, value Witness) {
	switch variant_value := value.(type) {
	case WitnessP2pk:
		writeInt32(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, variant_value.Signatures)
	case WitnessHtlc:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variant_value.Preimage)
		FfiConverterOptionalSequenceStringINSTANCE.Write(writer, variant_value.Signatures)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterWitness.Write", value))
	}
}

type FfiDestroyerWitness struct{}

func (_ FfiDestroyerWitness) Destroy(value Witness) {
	value.Destroy()
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) C.RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (c FfiConverterOptionalUint32) LowerExternal(value *uint32) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint32](c, value))
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) C.RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (c FfiConverterOptionalUint64) LowerExternal(value *uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint64](c, value))
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBool struct{}

var FfiConverterOptionalBoolINSTANCE = FfiConverterOptionalBool{}

func (c FfiConverterOptionalBool) Lift(rb RustBufferI) *bool {
	return LiftFromRustBuffer[*bool](c, rb)
}

func (_ FfiConverterOptionalBool) Read(reader io.Reader) *bool {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBoolINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBool) Lower(value *bool) C.RustBuffer {
	return LowerIntoRustBuffer[*bool](c, value)
}

func (c FfiConverterOptionalBool) LowerExternal(value *bool) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*bool](c, value))
}

func (_ FfiConverterOptionalBool) Write(writer io.Writer, value *bool) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBoolINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBool struct{}

func (_ FfiDestroyerOptionalBool) Destroy(value *bool) {
	if value != nil {
		FfiDestroyerBool{}.Destroy(*value)
	}
}

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) C.RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (c FfiConverterOptionalString) LowerExternal(value *string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*string](c, value))
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) C.RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (c FfiConverterOptionalBytes) LowerExternal(value *[]byte) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]byte](c, value))
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalNostrWaitInfo struct{}

var FfiConverterOptionalNostrWaitInfoINSTANCE = FfiConverterOptionalNostrWaitInfo{}

func (c FfiConverterOptionalNostrWaitInfo) Lift(rb RustBufferI) **NostrWaitInfo {
	return LiftFromRustBuffer[**NostrWaitInfo](c, rb)
}

func (_ FfiConverterOptionalNostrWaitInfo) Read(reader io.Reader) **NostrWaitInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterNostrWaitInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalNostrWaitInfo) Lower(value **NostrWaitInfo) C.RustBuffer {
	return LowerIntoRustBuffer[**NostrWaitInfo](c, value)
}

func (c FfiConverterOptionalNostrWaitInfo) LowerExternal(value **NostrWaitInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**NostrWaitInfo](c, value))
}

func (_ FfiConverterOptionalNostrWaitInfo) Write(writer io.Writer, value **NostrWaitInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterNostrWaitInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalNostrWaitInfo struct{}

func (_ FfiDestroyerOptionalNostrWaitInfo) Destroy(value **NostrWaitInfo) {
	if value != nil {
		FfiDestroyerNostrWaitInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalToken struct{}

var FfiConverterOptionalTokenINSTANCE = FfiConverterOptionalToken{}

func (c FfiConverterOptionalToken) Lift(rb RustBufferI) **Token {
	return LiftFromRustBuffer[**Token](c, rb)
}

func (_ FfiConverterOptionalToken) Read(reader io.Reader) **Token {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterTokenINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalToken) Lower(value **Token) C.RustBuffer {
	return LowerIntoRustBuffer[**Token](c, value)
}

func (c FfiConverterOptionalToken) LowerExternal(value **Token) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**Token](c, value))
}

func (_ FfiConverterOptionalToken) Write(writer io.Writer, value **Token) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterTokenINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalToken struct{}

func (_ FfiDestroyerOptionalToken) Destroy(value **Token) {
	if value != nil {
		FfiDestroyerToken{}.Destroy(*value)
	}
}

type FfiConverterOptionalAmount struct{}

var FfiConverterOptionalAmountINSTANCE = FfiConverterOptionalAmount{}

func (c FfiConverterOptionalAmount) Lift(rb RustBufferI) *Amount {
	return LiftFromRustBuffer[*Amount](c, rb)
}

func (_ FfiConverterOptionalAmount) Read(reader io.Reader) *Amount {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterAmountINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalAmount) Lower(value *Amount) C.RustBuffer {
	return LowerIntoRustBuffer[*Amount](c, value)
}

func (c FfiConverterOptionalAmount) LowerExternal(value *Amount) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Amount](c, value))
}

func (_ FfiConverterOptionalAmount) Write(writer io.Writer, value *Amount) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterAmountINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalAmount struct{}

func (_ FfiDestroyerOptionalAmount) Destroy(value *Amount) {
	if value != nil {
		FfiDestroyerAmount{}.Destroy(*value)
	}
}

type FfiConverterOptionalBlindAuthSettings struct{}

var FfiConverterOptionalBlindAuthSettingsINSTANCE = FfiConverterOptionalBlindAuthSettings{}

func (c FfiConverterOptionalBlindAuthSettings) Lift(rb RustBufferI) *BlindAuthSettings {
	return LiftFromRustBuffer[*BlindAuthSettings](c, rb)
}

func (_ FfiConverterOptionalBlindAuthSettings) Read(reader io.Reader) *BlindAuthSettings {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBlindAuthSettingsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBlindAuthSettings) Lower(value *BlindAuthSettings) C.RustBuffer {
	return LowerIntoRustBuffer[*BlindAuthSettings](c, value)
}

func (c FfiConverterOptionalBlindAuthSettings) LowerExternal(value *BlindAuthSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*BlindAuthSettings](c, value))
}

func (_ FfiConverterOptionalBlindAuthSettings) Write(writer io.Writer, value *BlindAuthSettings) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBlindAuthSettingsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBlindAuthSettings struct{}

func (_ FfiDestroyerOptionalBlindAuthSettings) Destroy(value *BlindAuthSettings) {
	if value != nil {
		FfiDestroyerBlindAuthSettings{}.Destroy(*value)
	}
}

type FfiConverterOptionalClearAuthSettings struct{}

var FfiConverterOptionalClearAuthSettingsINSTANCE = FfiConverterOptionalClearAuthSettings{}

func (c FfiConverterOptionalClearAuthSettings) Lift(rb RustBufferI) *ClearAuthSettings {
	return LiftFromRustBuffer[*ClearAuthSettings](c, rb)
}

func (_ FfiConverterOptionalClearAuthSettings) Read(reader io.Reader) *ClearAuthSettings {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterClearAuthSettingsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalClearAuthSettings) Lower(value *ClearAuthSettings) C.RustBuffer {
	return LowerIntoRustBuffer[*ClearAuthSettings](c, value)
}

func (c FfiConverterOptionalClearAuthSettings) LowerExternal(value *ClearAuthSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*ClearAuthSettings](c, value))
}

func (_ FfiConverterOptionalClearAuthSettings) Write(writer io.Writer, value *ClearAuthSettings) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterClearAuthSettingsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalClearAuthSettings struct{}

func (_ FfiDestroyerOptionalClearAuthSettings) Destroy(value *ClearAuthSettings) {
	if value != nil {
		FfiDestroyerClearAuthSettings{}.Destroy(*value)
	}
}

type FfiConverterOptionalConditions struct{}

var FfiConverterOptionalConditionsINSTANCE = FfiConverterOptionalConditions{}

func (c FfiConverterOptionalConditions) Lift(rb RustBufferI) *Conditions {
	return LiftFromRustBuffer[*Conditions](c, rb)
}

func (_ FfiConverterOptionalConditions) Read(reader io.Reader) *Conditions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterConditionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalConditions) Lower(value *Conditions) C.RustBuffer {
	return LowerIntoRustBuffer[*Conditions](c, value)
}

func (c FfiConverterOptionalConditions) LowerExternal(value *Conditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Conditions](c, value))
}

func (_ FfiConverterOptionalConditions) Write(writer io.Writer, value *Conditions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterConditionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalConditions struct{}

func (_ FfiDestroyerOptionalConditions) Destroy(value *Conditions) {
	if value != nil {
		FfiDestroyerConditions{}.Destroy(*value)
	}
}

type FfiConverterOptionalKeySetInfo struct{}

var FfiConverterOptionalKeySetInfoINSTANCE = FfiConverterOptionalKeySetInfo{}

func (c FfiConverterOptionalKeySetInfo) Lift(rb RustBufferI) *KeySetInfo {
	return LiftFromRustBuffer[*KeySetInfo](c, rb)
}

func (_ FfiConverterOptionalKeySetInfo) Read(reader io.Reader) *KeySetInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterKeySetInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalKeySetInfo) Lower(value *KeySetInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*KeySetInfo](c, value)
}

func (c FfiConverterOptionalKeySetInfo) LowerExternal(value *KeySetInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*KeySetInfo](c, value))
}

func (_ FfiConverterOptionalKeySetInfo) Write(writer io.Writer, value *KeySetInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterKeySetInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalKeySetInfo struct{}

func (_ FfiDestroyerOptionalKeySetInfo) Destroy(value *KeySetInfo) {
	if value != nil {
		FfiDestroyerKeySetInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalKeys struct{}

var FfiConverterOptionalKeysINSTANCE = FfiConverterOptionalKeys{}

func (c FfiConverterOptionalKeys) Lift(rb RustBufferI) *Keys {
	return LiftFromRustBuffer[*Keys](c, rb)
}

func (_ FfiConverterOptionalKeys) Read(reader io.Reader) *Keys {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterKeysINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalKeys) Lower(value *Keys) C.RustBuffer {
	return LowerIntoRustBuffer[*Keys](c, value)
}

func (c FfiConverterOptionalKeys) LowerExternal(value *Keys) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Keys](c, value))
}

func (_ FfiConverterOptionalKeys) Write(writer io.Writer, value *Keys) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterKeysINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalKeys struct{}

func (_ FfiDestroyerOptionalKeys) Destroy(value *Keys) {
	if value != nil {
		FfiDestroyerKeys{}.Destroy(*value)
	}
}

type FfiConverterOptionalMeltQuote struct{}

var FfiConverterOptionalMeltQuoteINSTANCE = FfiConverterOptionalMeltQuote{}

func (c FfiConverterOptionalMeltQuote) Lift(rb RustBufferI) *MeltQuote {
	return LiftFromRustBuffer[*MeltQuote](c, rb)
}

func (_ FfiConverterOptionalMeltQuote) Read(reader io.Reader) *MeltQuote {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMeltQuoteINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMeltQuote) Lower(value *MeltQuote) C.RustBuffer {
	return LowerIntoRustBuffer[*MeltQuote](c, value)
}

func (c FfiConverterOptionalMeltQuote) LowerExternal(value *MeltQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MeltQuote](c, value))
}

func (_ FfiConverterOptionalMeltQuote) Write(writer io.Writer, value *MeltQuote) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMeltQuoteINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMeltQuote struct{}

func (_ FfiDestroyerOptionalMeltQuote) Destroy(value *MeltQuote) {
	if value != nil {
		FfiDestroyerMeltQuote{}.Destroy(*value)
	}
}

type FfiConverterOptionalMintInfo struct{}

var FfiConverterOptionalMintInfoINSTANCE = FfiConverterOptionalMintInfo{}

func (c FfiConverterOptionalMintInfo) Lift(rb RustBufferI) *MintInfo {
	return LiftFromRustBuffer[*MintInfo](c, rb)
}

func (_ FfiConverterOptionalMintInfo) Read(reader io.Reader) *MintInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMintInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMintInfo) Lower(value *MintInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*MintInfo](c, value)
}

func (c FfiConverterOptionalMintInfo) LowerExternal(value *MintInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MintInfo](c, value))
}

func (_ FfiConverterOptionalMintInfo) Write(writer io.Writer, value *MintInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMintInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMintInfo struct{}

func (_ FfiDestroyerOptionalMintInfo) Destroy(value *MintInfo) {
	if value != nil {
		FfiDestroyerMintInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalMintQuote struct{}

var FfiConverterOptionalMintQuoteINSTANCE = FfiConverterOptionalMintQuote{}

func (c FfiConverterOptionalMintQuote) Lift(rb RustBufferI) *MintQuote {
	return LiftFromRustBuffer[*MintQuote](c, rb)
}

func (_ FfiConverterOptionalMintQuote) Read(reader io.Reader) *MintQuote {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMintQuoteINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMintQuote) Lower(value *MintQuote) C.RustBuffer {
	return LowerIntoRustBuffer[*MintQuote](c, value)
}

func (c FfiConverterOptionalMintQuote) LowerExternal(value *MintQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MintQuote](c, value))
}

func (_ FfiConverterOptionalMintQuote) Write(writer io.Writer, value *MintQuote) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMintQuoteINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMintQuote struct{}

func (_ FfiDestroyerOptionalMintQuote) Destroy(value *MintQuote) {
	if value != nil {
		FfiDestroyerMintQuote{}.Destroy(*value)
	}
}

type FfiConverterOptionalMintUrl struct{}

var FfiConverterOptionalMintUrlINSTANCE = FfiConverterOptionalMintUrl{}

func (c FfiConverterOptionalMintUrl) Lift(rb RustBufferI) *MintUrl {
	return LiftFromRustBuffer[*MintUrl](c, rb)
}

func (_ FfiConverterOptionalMintUrl) Read(reader io.Reader) *MintUrl {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMintUrlINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMintUrl) Lower(value *MintUrl) C.RustBuffer {
	return LowerIntoRustBuffer[*MintUrl](c, value)
}

func (c FfiConverterOptionalMintUrl) LowerExternal(value *MintUrl) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MintUrl](c, value))
}

func (_ FfiConverterOptionalMintUrl) Write(writer io.Writer, value *MintUrl) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMintUrlINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMintUrl struct{}

func (_ FfiDestroyerOptionalMintUrl) Destroy(value *MintUrl) {
	if value != nil {
		FfiDestroyerMintUrl{}.Destroy(*value)
	}
}

type FfiConverterOptionalMintVersion struct{}

var FfiConverterOptionalMintVersionINSTANCE = FfiConverterOptionalMintVersion{}

func (c FfiConverterOptionalMintVersion) Lift(rb RustBufferI) *MintVersion {
	return LiftFromRustBuffer[*MintVersion](c, rb)
}

func (_ FfiConverterOptionalMintVersion) Read(reader io.Reader) *MintVersion {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMintVersionINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMintVersion) Lower(value *MintVersion) C.RustBuffer {
	return LowerIntoRustBuffer[*MintVersion](c, value)
}

func (c FfiConverterOptionalMintVersion) LowerExternal(value *MintVersion) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MintVersion](c, value))
}

func (_ FfiConverterOptionalMintVersion) Write(writer io.Writer, value *MintVersion) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMintVersionINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMintVersion struct{}

func (_ FfiDestroyerOptionalMintVersion) Destroy(value *MintVersion) {
	if value != nil {
		FfiDestroyerMintVersion{}.Destroy(*value)
	}
}

type FfiConverterOptionalP2pkSigningKey struct{}

var FfiConverterOptionalP2pkSigningKeyINSTANCE = FfiConverterOptionalP2pkSigningKey{}

func (c FfiConverterOptionalP2pkSigningKey) Lift(rb RustBufferI) *P2pkSigningKey {
	return LiftFromRustBuffer[*P2pkSigningKey](c, rb)
}

func (_ FfiConverterOptionalP2pkSigningKey) Read(reader io.Reader) *P2pkSigningKey {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterP2pkSigningKeyINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalP2pkSigningKey) Lower(value *P2pkSigningKey) C.RustBuffer {
	return LowerIntoRustBuffer[*P2pkSigningKey](c, value)
}

func (c FfiConverterOptionalP2pkSigningKey) LowerExternal(value *P2pkSigningKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*P2pkSigningKey](c, value))
}

func (_ FfiConverterOptionalP2pkSigningKey) Write(writer io.Writer, value *P2pkSigningKey) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterP2pkSigningKeyINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalP2pkSigningKey struct{}

func (_ FfiDestroyerOptionalP2pkSigningKey) Destroy(value *P2pkSigningKey) {
	if value != nil {
		FfiDestroyerP2pkSigningKey{}.Destroy(*value)
	}
}

type FfiConverterOptionalProofDleq struct{}

var FfiConverterOptionalProofDleqINSTANCE = FfiConverterOptionalProofDleq{}

func (c FfiConverterOptionalProofDleq) Lift(rb RustBufferI) *ProofDleq {
	return LiftFromRustBuffer[*ProofDleq](c, rb)
}

func (_ FfiConverterOptionalProofDleq) Read(reader io.Reader) *ProofDleq {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterProofDleqINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalProofDleq) Lower(value *ProofDleq) C.RustBuffer {
	return LowerIntoRustBuffer[*ProofDleq](c, value)
}

func (c FfiConverterOptionalProofDleq) LowerExternal(value *ProofDleq) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*ProofDleq](c, value))
}

func (_ FfiConverterOptionalProofDleq) Write(writer io.Writer, value *ProofDleq) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterProofDleqINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalProofDleq struct{}

func (_ FfiDestroyerOptionalProofDleq) Destroy(value *ProofDleq) {
	if value != nil {
		FfiDestroyerProofDleq{}.Destroy(*value)
	}
}

type FfiConverterOptionalSendMemo struct{}

var FfiConverterOptionalSendMemoINSTANCE = FfiConverterOptionalSendMemo{}

func (c FfiConverterOptionalSendMemo) Lift(rb RustBufferI) *SendMemo {
	return LiftFromRustBuffer[*SendMemo](c, rb)
}

func (_ FfiConverterOptionalSendMemo) Read(reader io.Reader) *SendMemo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSendMemoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSendMemo) Lower(value *SendMemo) C.RustBuffer {
	return LowerIntoRustBuffer[*SendMemo](c, value)
}

func (c FfiConverterOptionalSendMemo) LowerExternal(value *SendMemo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*SendMemo](c, value))
}

func (_ FfiConverterOptionalSendMemo) Write(writer io.Writer, value *SendMemo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSendMemoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSendMemo struct{}

func (_ FfiDestroyerOptionalSendMemo) Destroy(value *SendMemo) {
	if value != nil {
		FfiDestroyerSendMemo{}.Destroy(*value)
	}
}

type FfiConverterOptionalTransaction struct{}

var FfiConverterOptionalTransactionINSTANCE = FfiConverterOptionalTransaction{}

func (c FfiConverterOptionalTransaction) Lift(rb RustBufferI) *Transaction {
	return LiftFromRustBuffer[*Transaction](c, rb)
}

func (_ FfiConverterOptionalTransaction) Read(reader io.Reader) *Transaction {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterTransactionINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalTransaction) Lower(value *Transaction) C.RustBuffer {
	return LowerIntoRustBuffer[*Transaction](c, value)
}

func (c FfiConverterOptionalTransaction) LowerExternal(value *Transaction) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Transaction](c, value))
}

func (_ FfiConverterOptionalTransaction) Write(writer io.Writer, value *Transaction) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterTransactionINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalTransaction struct{}

func (_ FfiDestroyerOptionalTransaction) Destroy(value *Transaction) {
	if value != nil {
		FfiDestroyerTransaction{}.Destroy(*value)
	}
}

type FfiConverterOptionalCurrencyUnit struct{}

var FfiConverterOptionalCurrencyUnitINSTANCE = FfiConverterOptionalCurrencyUnit{}

func (c FfiConverterOptionalCurrencyUnit) Lift(rb RustBufferI) *CurrencyUnit {
	return LiftFromRustBuffer[*CurrencyUnit](c, rb)
}

func (_ FfiConverterOptionalCurrencyUnit) Read(reader io.Reader) *CurrencyUnit {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterCurrencyUnitINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalCurrencyUnit) Lower(value *CurrencyUnit) C.RustBuffer {
	return LowerIntoRustBuffer[*CurrencyUnit](c, value)
}

func (c FfiConverterOptionalCurrencyUnit) LowerExternal(value *CurrencyUnit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*CurrencyUnit](c, value))
}

func (_ FfiConverterOptionalCurrencyUnit) Write(writer io.Writer, value *CurrencyUnit) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterCurrencyUnitINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalCurrencyUnit struct{}

func (_ FfiDestroyerOptionalCurrencyUnit) Destroy(value *CurrencyUnit) {
	if value != nil {
		FfiDestroyerCurrencyUnit{}.Destroy(*value)
	}
}

type FfiConverterOptionalKeysetLoadPolicy struct{}

var FfiConverterOptionalKeysetLoadPolicyINSTANCE = FfiConverterOptionalKeysetLoadPolicy{}

func (c FfiConverterOptionalKeysetLoadPolicy) Lift(rb RustBufferI) *KeysetLoadPolicy {
	return LiftFromRustBuffer[*KeysetLoadPolicy](c, rb)
}

func (_ FfiConverterOptionalKeysetLoadPolicy) Read(reader io.Reader) *KeysetLoadPolicy {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterKeysetLoadPolicyINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalKeysetLoadPolicy) Lower(value *KeysetLoadPolicy) C.RustBuffer {
	return LowerIntoRustBuffer[*KeysetLoadPolicy](c, value)
}

func (c FfiConverterOptionalKeysetLoadPolicy) LowerExternal(value *KeysetLoadPolicy) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*KeysetLoadPolicy](c, value))
}

func (_ FfiConverterOptionalKeysetLoadPolicy) Write(writer io.Writer, value *KeysetLoadPolicy) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterKeysetLoadPolicyINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalKeysetLoadPolicy struct{}

func (_ FfiDestroyerOptionalKeysetLoadPolicy) Destroy(value *KeysetLoadPolicy) {
	if value != nil {
		FfiDestroyerKeysetLoadPolicy{}.Destroy(*value)
	}
}

type FfiConverterOptionalMeltOptions struct{}

var FfiConverterOptionalMeltOptionsINSTANCE = FfiConverterOptionalMeltOptions{}

func (c FfiConverterOptionalMeltOptions) Lift(rb RustBufferI) *MeltOptions {
	return LiftFromRustBuffer[*MeltOptions](c, rb)
}

func (_ FfiConverterOptionalMeltOptions) Read(reader io.Reader) *MeltOptions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMeltOptionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMeltOptions) Lower(value *MeltOptions) C.RustBuffer {
	return LowerIntoRustBuffer[*MeltOptions](c, value)
}

func (c FfiConverterOptionalMeltOptions) LowerExternal(value *MeltOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MeltOptions](c, value))
}

func (_ FfiConverterOptionalMeltOptions) Write(writer io.Writer, value *MeltOptions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMeltOptionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMeltOptions struct{}

func (_ FfiDestroyerOptionalMeltOptions) Destroy(value *MeltOptions) {
	if value != nil {
		FfiDestroyerMeltOptions{}.Destroy(*value)
	}
}

type FfiConverterOptionalNotificationPayload struct{}

var FfiConverterOptionalNotificationPayloadINSTANCE = FfiConverterOptionalNotificationPayload{}

func (c FfiConverterOptionalNotificationPayload) Lift(rb RustBufferI) *NotificationPayload {
	return LiftFromRustBuffer[*NotificationPayload](c, rb)
}

func (_ FfiConverterOptionalNotificationPayload) Read(reader io.Reader) *NotificationPayload {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterNotificationPayloadINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalNotificationPayload) Lower(value *NotificationPayload) C.RustBuffer {
	return LowerIntoRustBuffer[*NotificationPayload](c, value)
}

func (c FfiConverterOptionalNotificationPayload) LowerExternal(value *NotificationPayload) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*NotificationPayload](c, value))
}

func (_ FfiConverterOptionalNotificationPayload) Write(writer io.Writer, value *NotificationPayload) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterNotificationPayloadINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalNotificationPayload struct{}

func (_ FfiDestroyerOptionalNotificationPayload) Destroy(value *NotificationPayload) {
	if value != nil {
		FfiDestroyerNotificationPayload{}.Destroy(*value)
	}
}

type FfiConverterOptionalPaymentMethod struct{}

var FfiConverterOptionalPaymentMethodINSTANCE = FfiConverterOptionalPaymentMethod{}

func (c FfiConverterOptionalPaymentMethod) Lift(rb RustBufferI) *PaymentMethod {
	return LiftFromRustBuffer[*PaymentMethod](c, rb)
}

func (_ FfiConverterOptionalPaymentMethod) Read(reader io.Reader) *PaymentMethod {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterPaymentMethodINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalPaymentMethod) Lower(value *PaymentMethod) C.RustBuffer {
	return LowerIntoRustBuffer[*PaymentMethod](c, value)
}

func (c FfiConverterOptionalPaymentMethod) LowerExternal(value *PaymentMethod) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*PaymentMethod](c, value))
}

func (_ FfiConverterOptionalPaymentMethod) Write(writer io.Writer, value *PaymentMethod) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterPaymentMethodINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalPaymentMethod struct{}

func (_ FfiDestroyerOptionalPaymentMethod) Destroy(value *PaymentMethod) {
	if value != nil {
		FfiDestroyerPaymentMethod{}.Destroy(*value)
	}
}

type FfiConverterOptionalRateLimit struct{}

var FfiConverterOptionalRateLimitINSTANCE = FfiConverterOptionalRateLimit{}

func (c FfiConverterOptionalRateLimit) Lift(rb RustBufferI) *RateLimit {
	return LiftFromRustBuffer[*RateLimit](c, rb)
}

func (_ FfiConverterOptionalRateLimit) Read(reader io.Reader) *RateLimit {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterRateLimitINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalRateLimit) Lower(value *RateLimit) C.RustBuffer {
	return LowerIntoRustBuffer[*RateLimit](c, value)
}

func (c FfiConverterOptionalRateLimit) LowerExternal(value *RateLimit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*RateLimit](c, value))
}

func (_ FfiConverterOptionalRateLimit) Write(writer io.Writer, value *RateLimit) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterRateLimitINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalRateLimit struct{}

func (_ FfiDestroyerOptionalRateLimit) Destroy(value *RateLimit) {
	if value != nil {
		FfiDestroyerRateLimit{}.Destroy(*value)
	}
}

type FfiConverterOptionalSpendingConditions struct{}

var FfiConverterOptionalSpendingConditionsINSTANCE = FfiConverterOptionalSpendingConditions{}

func (c FfiConverterOptionalSpendingConditions) Lift(rb RustBufferI) *SpendingConditions {
	return LiftFromRustBuffer[*SpendingConditions](c, rb)
}

func (_ FfiConverterOptionalSpendingConditions) Read(reader io.Reader) *SpendingConditions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSpendingConditionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSpendingConditions) Lower(value *SpendingConditions) C.RustBuffer {
	return LowerIntoRustBuffer[*SpendingConditions](c, value)
}

func (c FfiConverterOptionalSpendingConditions) LowerExternal(value *SpendingConditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*SpendingConditions](c, value))
}

func (_ FfiConverterOptionalSpendingConditions) Write(writer io.Writer, value *SpendingConditions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSpendingConditionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSpendingConditions struct{}

func (_ FfiDestroyerOptionalSpendingConditions) Destroy(value *SpendingConditions) {
	if value != nil {
		FfiDestroyerSpendingConditions{}.Destroy(*value)
	}
}

type FfiConverterOptionalTransactionDirection struct{}

var FfiConverterOptionalTransactionDirectionINSTANCE = FfiConverterOptionalTransactionDirection{}

func (c FfiConverterOptionalTransactionDirection) Lift(rb RustBufferI) *TransactionDirection {
	return LiftFromRustBuffer[*TransactionDirection](c, rb)
}

func (_ FfiConverterOptionalTransactionDirection) Read(reader io.Reader) *TransactionDirection {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterTransactionDirectionINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalTransactionDirection) Lower(value *TransactionDirection) C.RustBuffer {
	return LowerIntoRustBuffer[*TransactionDirection](c, value)
}

func (c FfiConverterOptionalTransactionDirection) LowerExternal(value *TransactionDirection) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*TransactionDirection](c, value))
}

func (_ FfiConverterOptionalTransactionDirection) Write(writer io.Writer, value *TransactionDirection) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterTransactionDirectionINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalTransactionDirection struct{}

func (_ FfiDestroyerOptionalTransactionDirection) Destroy(value *TransactionDirection) {
	if value != nil {
		FfiDestroyerTransactionDirection{}.Destroy(*value)
	}
}

type FfiConverterOptionalWitness struct{}

var FfiConverterOptionalWitnessINSTANCE = FfiConverterOptionalWitness{}

func (c FfiConverterOptionalWitness) Lift(rb RustBufferI) *Witness {
	return LiftFromRustBuffer[*Witness](c, rb)
}

func (_ FfiConverterOptionalWitness) Read(reader io.Reader) *Witness {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterWitnessINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalWitness) Lower(value *Witness) C.RustBuffer {
	return LowerIntoRustBuffer[*Witness](c, value)
}

func (c FfiConverterOptionalWitness) LowerExternal(value *Witness) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Witness](c, value))
}

func (_ FfiConverterOptionalWitness) Write(writer io.Writer, value *Witness) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterWitnessINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalWitness struct{}

func (_ FfiDestroyerOptionalWitness) Destroy(value *Witness) {
	if value != nil {
		FfiDestroyerWitness{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceString struct{}

var FfiConverterOptionalSequenceStringINSTANCE = FfiConverterOptionalSequenceString{}

func (c FfiConverterOptionalSequenceString) Lift(rb RustBufferI) *[]string {
	return LiftFromRustBuffer[*[]string](c, rb)
}

func (_ FfiConverterOptionalSequenceString) Read(reader io.Reader) *[]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceString) Lower(value *[]string) C.RustBuffer {
	return LowerIntoRustBuffer[*[]string](c, value)
}

func (c FfiConverterOptionalSequenceString) LowerExternal(value *[]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]string](c, value))
}

func (_ FfiConverterOptionalSequenceString) Write(writer io.Writer, value *[]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceString struct{}

func (_ FfiDestroyerOptionalSequenceString) Destroy(value *[]string) {
	if value != nil {
		FfiDestroyerSequenceString{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceContactInfo struct{}

var FfiConverterOptionalSequenceContactInfoINSTANCE = FfiConverterOptionalSequenceContactInfo{}

func (c FfiConverterOptionalSequenceContactInfo) Lift(rb RustBufferI) *[]ContactInfo {
	return LiftFromRustBuffer[*[]ContactInfo](c, rb)
}

func (_ FfiConverterOptionalSequenceContactInfo) Read(reader io.Reader) *[]ContactInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceContactInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceContactInfo) Lower(value *[]ContactInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*[]ContactInfo](c, value)
}

func (c FfiConverterOptionalSequenceContactInfo) LowerExternal(value *[]ContactInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]ContactInfo](c, value))
}

func (_ FfiConverterOptionalSequenceContactInfo) Write(writer io.Writer, value *[]ContactInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceContactInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceContactInfo struct{}

func (_ FfiDestroyerOptionalSequenceContactInfo) Destroy(value *[]ContactInfo) {
	if value != nil {
		FfiDestroyerSequenceContactInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceKeySetInfo struct{}

var FfiConverterOptionalSequenceKeySetInfoINSTANCE = FfiConverterOptionalSequenceKeySetInfo{}

func (c FfiConverterOptionalSequenceKeySetInfo) Lift(rb RustBufferI) *[]KeySetInfo {
	return LiftFromRustBuffer[*[]KeySetInfo](c, rb)
}

func (_ FfiConverterOptionalSequenceKeySetInfo) Read(reader io.Reader) *[]KeySetInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceKeySetInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceKeySetInfo) Lower(value *[]KeySetInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*[]KeySetInfo](c, value)
}

func (c FfiConverterOptionalSequenceKeySetInfo) LowerExternal(value *[]KeySetInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]KeySetInfo](c, value))
}

func (_ FfiConverterOptionalSequenceKeySetInfo) Write(writer io.Writer, value *[]KeySetInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceKeySetInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceKeySetInfo struct{}

func (_ FfiDestroyerOptionalSequenceKeySetInfo) Destroy(value *[]KeySetInfo) {
	if value != nil {
		FfiDestroyerSequenceKeySetInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceProof struct{}

var FfiConverterOptionalSequenceProofINSTANCE = FfiConverterOptionalSequenceProof{}

func (c FfiConverterOptionalSequenceProof) Lift(rb RustBufferI) *[]Proof {
	return LiftFromRustBuffer[*[]Proof](c, rb)
}

func (_ FfiConverterOptionalSequenceProof) Read(reader io.Reader) *[]Proof {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceProofINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceProof) Lower(value *[]Proof) C.RustBuffer {
	return LowerIntoRustBuffer[*[]Proof](c, value)
}

func (c FfiConverterOptionalSequenceProof) LowerExternal(value *[]Proof) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]Proof](c, value))
}

func (_ FfiConverterOptionalSequenceProof) Write(writer io.Writer, value *[]Proof) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceProofINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceProof struct{}

func (_ FfiDestroyerOptionalSequenceProof) Destroy(value *[]Proof) {
	if value != nil {
		FfiDestroyerSequenceProof{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceProofState struct{}

var FfiConverterOptionalSequenceProofStateINSTANCE = FfiConverterOptionalSequenceProofState{}

func (c FfiConverterOptionalSequenceProofState) Lift(rb RustBufferI) *[]ProofState {
	return LiftFromRustBuffer[*[]ProofState](c, rb)
}

func (_ FfiConverterOptionalSequenceProofState) Read(reader io.Reader) *[]ProofState {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceProofStateINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceProofState) Lower(value *[]ProofState) C.RustBuffer {
	return LowerIntoRustBuffer[*[]ProofState](c, value)
}

func (c FfiConverterOptionalSequenceProofState) LowerExternal(value *[]ProofState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]ProofState](c, value))
}

func (_ FfiConverterOptionalSequenceProofState) Write(writer io.Writer, value *[]ProofState) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceProofStateINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceProofState struct{}

func (_ FfiDestroyerOptionalSequenceProofState) Destroy(value *[]ProofState) {
	if value != nil {
		FfiDestroyerSequenceProofState{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceSpendingConditions struct{}

var FfiConverterOptionalSequenceSpendingConditionsINSTANCE = FfiConverterOptionalSequenceSpendingConditions{}

func (c FfiConverterOptionalSequenceSpendingConditions) Lift(rb RustBufferI) *[]SpendingConditions {
	return LiftFromRustBuffer[*[]SpendingConditions](c, rb)
}

func (_ FfiConverterOptionalSequenceSpendingConditions) Read(reader io.Reader) *[]SpendingConditions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceSpendingConditionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceSpendingConditions) Lower(value *[]SpendingConditions) C.RustBuffer {
	return LowerIntoRustBuffer[*[]SpendingConditions](c, value)
}

func (c FfiConverterOptionalSequenceSpendingConditions) LowerExternal(value *[]SpendingConditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]SpendingConditions](c, value))
}

func (_ FfiConverterOptionalSequenceSpendingConditions) Write(writer io.Writer, value *[]SpendingConditions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceSpendingConditionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceSpendingConditions struct{}

func (_ FfiDestroyerOptionalSequenceSpendingConditions) Destroy(value *[]SpendingConditions) {
	if value != nil {
		FfiDestroyerSequenceSpendingConditions{}.Destroy(*value)
	}
}

type FfiConverterSequenceUint64 struct{}

var FfiConverterSequenceUint64INSTANCE = FfiConverterSequenceUint64{}

func (c FfiConverterSequenceUint64) Lift(rb RustBufferI) []uint64 {
	return LiftFromRustBuffer[[]uint64](c, rb)
}

func (c FfiConverterSequenceUint64) Read(reader io.Reader) []uint64 {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]uint64, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterUint64INSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceUint64) Lower(value []uint64) C.RustBuffer {
	return LowerIntoRustBuffer[[]uint64](c, value)
}

func (c FfiConverterSequenceUint64) LowerExternal(value []uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]uint64](c, value))
}

func (c FfiConverterSequenceUint64) Write(writer io.Writer, value []uint64) {
	if len(value) > math.MaxInt32 {
		panic("[]uint64 is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterUint64INSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceUint64 struct{}

func (FfiDestroyerSequenceUint64) Destroy(sequence []uint64) {
	for _, value := range sequence {
		FfiDestroyerUint64{}.Destroy(value)
	}
}

type FfiConverterSequenceBool struct{}

var FfiConverterSequenceBoolINSTANCE = FfiConverterSequenceBool{}

func (c FfiConverterSequenceBool) Lift(rb RustBufferI) []bool {
	return LiftFromRustBuffer[[]bool](c, rb)
}

func (c FfiConverterSequenceBool) Read(reader io.Reader) []bool {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]bool, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterBoolINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceBool) Lower(value []bool) C.RustBuffer {
	return LowerIntoRustBuffer[[]bool](c, value)
}

func (c FfiConverterSequenceBool) LowerExternal(value []bool) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]bool](c, value))
}

func (c FfiConverterSequenceBool) Write(writer io.Writer, value []bool) {
	if len(value) > math.MaxInt32 {
		panic("[]bool is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterBoolINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceBool struct{}

func (FfiDestroyerSequenceBool) Destroy(sequence []bool) {
	for _, value := range sequence {
		FfiDestroyerBool{}.Destroy(value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) C.RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) LowerExternal(value []string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]string](c, value))
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterSequencePaymentRequest struct{}

var FfiConverterSequencePaymentRequestINSTANCE = FfiConverterSequencePaymentRequest{}

func (c FfiConverterSequencePaymentRequest) Lift(rb RustBufferI) []*PaymentRequest {
	return LiftFromRustBuffer[[]*PaymentRequest](c, rb)
}

func (c FfiConverterSequencePaymentRequest) Read(reader io.Reader) []*PaymentRequest {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]*PaymentRequest, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterPaymentRequestINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequencePaymentRequest) Lower(value []*PaymentRequest) C.RustBuffer {
	return LowerIntoRustBuffer[[]*PaymentRequest](c, value)
}

func (c FfiConverterSequencePaymentRequest) LowerExternal(value []*PaymentRequest) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]*PaymentRequest](c, value))
}

func (c FfiConverterSequencePaymentRequest) Write(writer io.Writer, value []*PaymentRequest) {
	if len(value) > math.MaxInt32 {
		panic("[]*PaymentRequest is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterPaymentRequestINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequencePaymentRequest struct{}

func (FfiDestroyerSequencePaymentRequest) Destroy(sequence []*PaymentRequest) {
	for _, value := range sequence {
		FfiDestroyerPaymentRequest{}.Destroy(value)
	}
}

type FfiConverterSequenceWallet struct{}

var FfiConverterSequenceWalletINSTANCE = FfiConverterSequenceWallet{}

func (c FfiConverterSequenceWallet) Lift(rb RustBufferI) []*Wallet {
	return LiftFromRustBuffer[[]*Wallet](c, rb)
}

func (c FfiConverterSequenceWallet) Read(reader io.Reader) []*Wallet {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]*Wallet, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterWalletINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceWallet) Lower(value []*Wallet) C.RustBuffer {
	return LowerIntoRustBuffer[[]*Wallet](c, value)
}

func (c FfiConverterSequenceWallet) LowerExternal(value []*Wallet) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]*Wallet](c, value))
}

func (c FfiConverterSequenceWallet) Write(writer io.Writer, value []*Wallet) {
	if len(value) > math.MaxInt32 {
		panic("[]*Wallet is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterWalletINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceWallet struct{}

func (FfiDestroyerSequenceWallet) Destroy(sequence []*Wallet) {
	for _, value := range sequence {
		FfiDestroyerWallet{}.Destroy(value)
	}
}

type FfiConverterSequenceAmount struct{}

var FfiConverterSequenceAmountINSTANCE = FfiConverterSequenceAmount{}

func (c FfiConverterSequenceAmount) Lift(rb RustBufferI) []Amount {
	return LiftFromRustBuffer[[]Amount](c, rb)
}

func (c FfiConverterSequenceAmount) Read(reader io.Reader) []Amount {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]Amount, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterAmountINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceAmount) Lower(value []Amount) C.RustBuffer {
	return LowerIntoRustBuffer[[]Amount](c, value)
}

func (c FfiConverterSequenceAmount) LowerExternal(value []Amount) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]Amount](c, value))
}

func (c FfiConverterSequenceAmount) Write(writer io.Writer, value []Amount) {
	if len(value) > math.MaxInt32 {
		panic("[]Amount is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterAmountINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceAmount struct{}

func (FfiDestroyerSequenceAmount) Destroy(sequence []Amount) {
	for _, value := range sequence {
		FfiDestroyerAmount{}.Destroy(value)
	}
}

type FfiConverterSequenceAuthProof struct{}

var FfiConverterSequenceAuthProofINSTANCE = FfiConverterSequenceAuthProof{}

func (c FfiConverterSequenceAuthProof) Lift(rb RustBufferI) []AuthProof {
	return LiftFromRustBuffer[[]AuthProof](c, rb)
}

func (c FfiConverterSequenceAuthProof) Read(reader io.Reader) []AuthProof {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]AuthProof, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterAuthProofINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceAuthProof) Lower(value []AuthProof) C.RustBuffer {
	return LowerIntoRustBuffer[[]AuthProof](c, value)
}

func (c FfiConverterSequenceAuthProof) LowerExternal(value []AuthProof) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]AuthProof](c, value))
}

func (c FfiConverterSequenceAuthProof) Write(writer io.Writer, value []AuthProof) {
	if len(value) > math.MaxInt32 {
		panic("[]AuthProof is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterAuthProofINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceAuthProof struct{}

func (FfiDestroyerSequenceAuthProof) Destroy(sequence []AuthProof) {
	for _, value := range sequence {
		FfiDestroyerAuthProof{}.Destroy(value)
	}
}

type FfiConverterSequenceContactInfo struct{}

var FfiConverterSequenceContactInfoINSTANCE = FfiConverterSequenceContactInfo{}

func (c FfiConverterSequenceContactInfo) Lift(rb RustBufferI) []ContactInfo {
	return LiftFromRustBuffer[[]ContactInfo](c, rb)
}

func (c FfiConverterSequenceContactInfo) Read(reader io.Reader) []ContactInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ContactInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterContactInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceContactInfo) Lower(value []ContactInfo) C.RustBuffer {
	return LowerIntoRustBuffer[[]ContactInfo](c, value)
}

func (c FfiConverterSequenceContactInfo) LowerExternal(value []ContactInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ContactInfo](c, value))
}

func (c FfiConverterSequenceContactInfo) Write(writer io.Writer, value []ContactInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]ContactInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterContactInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceContactInfo struct{}

func (FfiDestroyerSequenceContactInfo) Destroy(sequence []ContactInfo) {
	for _, value := range sequence {
		FfiDestroyerContactInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceFinalizedMelt struct{}

var FfiConverterSequenceFinalizedMeltINSTANCE = FfiConverterSequenceFinalizedMelt{}

func (c FfiConverterSequenceFinalizedMelt) Lift(rb RustBufferI) []FinalizedMelt {
	return LiftFromRustBuffer[[]FinalizedMelt](c, rb)
}

func (c FfiConverterSequenceFinalizedMelt) Read(reader io.Reader) []FinalizedMelt {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]FinalizedMelt, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterFinalizedMeltINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceFinalizedMelt) Lower(value []FinalizedMelt) C.RustBuffer {
	return LowerIntoRustBuffer[[]FinalizedMelt](c, value)
}

func (c FfiConverterSequenceFinalizedMelt) LowerExternal(value []FinalizedMelt) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]FinalizedMelt](c, value))
}

func (c FfiConverterSequenceFinalizedMelt) Write(writer io.Writer, value []FinalizedMelt) {
	if len(value) > math.MaxInt32 {
		panic("[]FinalizedMelt is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterFinalizedMeltINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceFinalizedMelt struct{}

func (FfiDestroyerSequenceFinalizedMelt) Destroy(sequence []FinalizedMelt) {
	for _, value := range sequence {
		FfiDestroyerFinalizedMelt{}.Destroy(value)
	}
}

type FfiConverterSequenceKeySet struct{}

var FfiConverterSequenceKeySetINSTANCE = FfiConverterSequenceKeySet{}

func (c FfiConverterSequenceKeySet) Lift(rb RustBufferI) []KeySet {
	return LiftFromRustBuffer[[]KeySet](c, rb)
}

func (c FfiConverterSequenceKeySet) Read(reader io.Reader) []KeySet {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]KeySet, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterKeySetINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceKeySet) Lower(value []KeySet) C.RustBuffer {
	return LowerIntoRustBuffer[[]KeySet](c, value)
}

func (c FfiConverterSequenceKeySet) LowerExternal(value []KeySet) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]KeySet](c, value))
}

func (c FfiConverterSequenceKeySet) Write(writer io.Writer, value []KeySet) {
	if len(value) > math.MaxInt32 {
		panic("[]KeySet is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterKeySetINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceKeySet struct{}

func (FfiDestroyerSequenceKeySet) Destroy(sequence []KeySet) {
	for _, value := range sequence {
		FfiDestroyerKeySet{}.Destroy(value)
	}
}

type FfiConverterSequenceKeySetInfo struct{}

var FfiConverterSequenceKeySetInfoINSTANCE = FfiConverterSequenceKeySetInfo{}

func (c FfiConverterSequenceKeySetInfo) Lift(rb RustBufferI) []KeySetInfo {
	return LiftFromRustBuffer[[]KeySetInfo](c, rb)
}

func (c FfiConverterSequenceKeySetInfo) Read(reader io.Reader) []KeySetInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]KeySetInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterKeySetInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceKeySetInfo) Lower(value []KeySetInfo) C.RustBuffer {
	return LowerIntoRustBuffer[[]KeySetInfo](c, value)
}

func (c FfiConverterSequenceKeySetInfo) LowerExternal(value []KeySetInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]KeySetInfo](c, value))
}

func (c FfiConverterSequenceKeySetInfo) Write(writer io.Writer, value []KeySetInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]KeySetInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterKeySetInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceKeySetInfo struct{}

func (FfiDestroyerSequenceKeySetInfo) Destroy(sequence []KeySetInfo) {
	for _, value := range sequence {
		FfiDestroyerKeySetInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceMeltMethodSettings struct{}

var FfiConverterSequenceMeltMethodSettingsINSTANCE = FfiConverterSequenceMeltMethodSettings{}

func (c FfiConverterSequenceMeltMethodSettings) Lift(rb RustBufferI) []MeltMethodSettings {
	return LiftFromRustBuffer[[]MeltMethodSettings](c, rb)
}

func (c FfiConverterSequenceMeltMethodSettings) Read(reader io.Reader) []MeltMethodSettings {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MeltMethodSettings, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMeltMethodSettingsINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMeltMethodSettings) Lower(value []MeltMethodSettings) C.RustBuffer {
	return LowerIntoRustBuffer[[]MeltMethodSettings](c, value)
}

func (c FfiConverterSequenceMeltMethodSettings) LowerExternal(value []MeltMethodSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MeltMethodSettings](c, value))
}

func (c FfiConverterSequenceMeltMethodSettings) Write(writer io.Writer, value []MeltMethodSettings) {
	if len(value) > math.MaxInt32 {
		panic("[]MeltMethodSettings is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMeltMethodSettingsINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMeltMethodSettings struct{}

func (FfiDestroyerSequenceMeltMethodSettings) Destroy(sequence []MeltMethodSettings) {
	for _, value := range sequence {
		FfiDestroyerMeltMethodSettings{}.Destroy(value)
	}
}

type FfiConverterSequenceMeltQuote struct{}

var FfiConverterSequenceMeltQuoteINSTANCE = FfiConverterSequenceMeltQuote{}

func (c FfiConverterSequenceMeltQuote) Lift(rb RustBufferI) []MeltQuote {
	return LiftFromRustBuffer[[]MeltQuote](c, rb)
}

func (c FfiConverterSequenceMeltQuote) Read(reader io.Reader) []MeltQuote {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MeltQuote, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMeltQuoteINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMeltQuote) Lower(value []MeltQuote) C.RustBuffer {
	return LowerIntoRustBuffer[[]MeltQuote](c, value)
}

func (c FfiConverterSequenceMeltQuote) LowerExternal(value []MeltQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MeltQuote](c, value))
}

func (c FfiConverterSequenceMeltQuote) Write(writer io.Writer, value []MeltQuote) {
	if len(value) > math.MaxInt32 {
		panic("[]MeltQuote is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMeltQuoteINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMeltQuote struct{}

func (FfiDestroyerSequenceMeltQuote) Destroy(sequence []MeltQuote) {
	for _, value := range sequence {
		FfiDestroyerMeltQuote{}.Destroy(value)
	}
}

type FfiConverterSequenceMeltQuoteOnchainFeeOption struct{}

var FfiConverterSequenceMeltQuoteOnchainFeeOptionINSTANCE = FfiConverterSequenceMeltQuoteOnchainFeeOption{}

func (c FfiConverterSequenceMeltQuoteOnchainFeeOption) Lift(rb RustBufferI) []MeltQuoteOnchainFeeOption {
	return LiftFromRustBuffer[[]MeltQuoteOnchainFeeOption](c, rb)
}

func (c FfiConverterSequenceMeltQuoteOnchainFeeOption) Read(reader io.Reader) []MeltQuoteOnchainFeeOption {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MeltQuoteOnchainFeeOption, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMeltQuoteOnchainFeeOptionINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMeltQuoteOnchainFeeOption) Lower(value []MeltQuoteOnchainFeeOption) C.RustBuffer {
	return LowerIntoRustBuffer[[]MeltQuoteOnchainFeeOption](c, value)
}

func (c FfiConverterSequenceMeltQuoteOnchainFeeOption) LowerExternal(value []MeltQuoteOnchainFeeOption) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MeltQuoteOnchainFeeOption](c, value))
}

func (c FfiConverterSequenceMeltQuoteOnchainFeeOption) Write(writer io.Writer, value []MeltQuoteOnchainFeeOption) {
	if len(value) > math.MaxInt32 {
		panic("[]MeltQuoteOnchainFeeOption is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMeltQuoteOnchainFeeOptionINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMeltQuoteOnchainFeeOption struct{}

func (FfiDestroyerSequenceMeltQuoteOnchainFeeOption) Destroy(sequence []MeltQuoteOnchainFeeOption) {
	for _, value := range sequence {
		FfiDestroyerMeltQuoteOnchainFeeOption{}.Destroy(value)
	}
}

type FfiConverterSequenceMintMethodSettings struct{}

var FfiConverterSequenceMintMethodSettingsINSTANCE = FfiConverterSequenceMintMethodSettings{}

func (c FfiConverterSequenceMintMethodSettings) Lift(rb RustBufferI) []MintMethodSettings {
	return LiftFromRustBuffer[[]MintMethodSettings](c, rb)
}

func (c FfiConverterSequenceMintMethodSettings) Read(reader io.Reader) []MintMethodSettings {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MintMethodSettings, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMintMethodSettingsINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMintMethodSettings) Lower(value []MintMethodSettings) C.RustBuffer {
	return LowerIntoRustBuffer[[]MintMethodSettings](c, value)
}

func (c FfiConverterSequenceMintMethodSettings) LowerExternal(value []MintMethodSettings) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MintMethodSettings](c, value))
}

func (c FfiConverterSequenceMintMethodSettings) Write(writer io.Writer, value []MintMethodSettings) {
	if len(value) > math.MaxInt32 {
		panic("[]MintMethodSettings is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMintMethodSettingsINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMintMethodSettings struct{}

func (FfiDestroyerSequenceMintMethodSettings) Destroy(sequence []MintMethodSettings) {
	for _, value := range sequence {
		FfiDestroyerMintMethodSettings{}.Destroy(value)
	}
}

type FfiConverterSequenceMintQuote struct{}

var FfiConverterSequenceMintQuoteINSTANCE = FfiConverterSequenceMintQuote{}

func (c FfiConverterSequenceMintQuote) Lift(rb RustBufferI) []MintQuote {
	return LiftFromRustBuffer[[]MintQuote](c, rb)
}

func (c FfiConverterSequenceMintQuote) Read(reader io.Reader) []MintQuote {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MintQuote, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMintQuoteINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMintQuote) Lower(value []MintQuote) C.RustBuffer {
	return LowerIntoRustBuffer[[]MintQuote](c, value)
}

func (c FfiConverterSequenceMintQuote) LowerExternal(value []MintQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MintQuote](c, value))
}

func (c FfiConverterSequenceMintQuote) Write(writer io.Writer, value []MintQuote) {
	if len(value) > math.MaxInt32 {
		panic("[]MintQuote is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMintQuoteINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMintQuote struct{}

func (FfiDestroyerSequenceMintQuote) Destroy(sequence []MintQuote) {
	for _, value := range sequence {
		FfiDestroyerMintQuote{}.Destroy(value)
	}
}

type FfiConverterSequenceMintUrl struct{}

var FfiConverterSequenceMintUrlINSTANCE = FfiConverterSequenceMintUrl{}

func (c FfiConverterSequenceMintUrl) Lift(rb RustBufferI) []MintUrl {
	return LiftFromRustBuffer[[]MintUrl](c, rb)
}

func (c FfiConverterSequenceMintUrl) Read(reader io.Reader) []MintUrl {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]MintUrl, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterMintUrlINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceMintUrl) Lower(value []MintUrl) C.RustBuffer {
	return LowerIntoRustBuffer[[]MintUrl](c, value)
}

func (c FfiConverterSequenceMintUrl) LowerExternal(value []MintUrl) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]MintUrl](c, value))
}

func (c FfiConverterSequenceMintUrl) Write(writer io.Writer, value []MintUrl) {
	if len(value) > math.MaxInt32 {
		panic("[]MintUrl is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterMintUrlINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceMintUrl struct{}

func (FfiDestroyerSequenceMintUrl) Destroy(sequence []MintUrl) {
	for _, value := range sequence {
		FfiDestroyerMintUrl{}.Destroy(value)
	}
}

type FfiConverterSequenceNpubCashQuote struct{}

var FfiConverterSequenceNpubCashQuoteINSTANCE = FfiConverterSequenceNpubCashQuote{}

func (c FfiConverterSequenceNpubCashQuote) Lift(rb RustBufferI) []NpubCashQuote {
	return LiftFromRustBuffer[[]NpubCashQuote](c, rb)
}

func (c FfiConverterSequenceNpubCashQuote) Read(reader io.Reader) []NpubCashQuote {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]NpubCashQuote, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterNpubCashQuoteINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceNpubCashQuote) Lower(value []NpubCashQuote) C.RustBuffer {
	return LowerIntoRustBuffer[[]NpubCashQuote](c, value)
}

func (c FfiConverterSequenceNpubCashQuote) LowerExternal(value []NpubCashQuote) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]NpubCashQuote](c, value))
}

func (c FfiConverterSequenceNpubCashQuote) Write(writer io.Writer, value []NpubCashQuote) {
	if len(value) > math.MaxInt32 {
		panic("[]NpubCashQuote is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterNpubCashQuoteINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceNpubCashQuote struct{}

func (FfiDestroyerSequenceNpubCashQuote) Destroy(sequence []NpubCashQuote) {
	for _, value := range sequence {
		FfiDestroyerNpubCashQuote{}.Destroy(value)
	}
}

type FfiConverterSequenceP2pkSigningKey struct{}

var FfiConverterSequenceP2pkSigningKeyINSTANCE = FfiConverterSequenceP2pkSigningKey{}

func (c FfiConverterSequenceP2pkSigningKey) Lift(rb RustBufferI) []P2pkSigningKey {
	return LiftFromRustBuffer[[]P2pkSigningKey](c, rb)
}

func (c FfiConverterSequenceP2pkSigningKey) Read(reader io.Reader) []P2pkSigningKey {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]P2pkSigningKey, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterP2pkSigningKeyINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceP2pkSigningKey) Lower(value []P2pkSigningKey) C.RustBuffer {
	return LowerIntoRustBuffer[[]P2pkSigningKey](c, value)
}

func (c FfiConverterSequenceP2pkSigningKey) LowerExternal(value []P2pkSigningKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]P2pkSigningKey](c, value))
}

func (c FfiConverterSequenceP2pkSigningKey) Write(writer io.Writer, value []P2pkSigningKey) {
	if len(value) > math.MaxInt32 {
		panic("[]P2pkSigningKey is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterP2pkSigningKeyINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceP2pkSigningKey struct{}

func (FfiDestroyerSequenceP2pkSigningKey) Destroy(sequence []P2pkSigningKey) {
	for _, value := range sequence {
		FfiDestroyerP2pkSigningKey{}.Destroy(value)
	}
}

type FfiConverterSequenceProof struct{}

var FfiConverterSequenceProofINSTANCE = FfiConverterSequenceProof{}

func (c FfiConverterSequenceProof) Lift(rb RustBufferI) []Proof {
	return LiftFromRustBuffer[[]Proof](c, rb)
}

func (c FfiConverterSequenceProof) Read(reader io.Reader) []Proof {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]Proof, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterProofINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceProof) Lower(value []Proof) C.RustBuffer {
	return LowerIntoRustBuffer[[]Proof](c, value)
}

func (c FfiConverterSequenceProof) LowerExternal(value []Proof) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]Proof](c, value))
}

func (c FfiConverterSequenceProof) Write(writer io.Writer, value []Proof) {
	if len(value) > math.MaxInt32 {
		panic("[]Proof is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterProofINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceProof struct{}

func (FfiDestroyerSequenceProof) Destroy(sequence []Proof) {
	for _, value := range sequence {
		FfiDestroyerProof{}.Destroy(value)
	}
}

type FfiConverterSequenceProofInfo struct{}

var FfiConverterSequenceProofInfoINSTANCE = FfiConverterSequenceProofInfo{}

func (c FfiConverterSequenceProofInfo) Lift(rb RustBufferI) []ProofInfo {
	return LiftFromRustBuffer[[]ProofInfo](c, rb)
}

func (c FfiConverterSequenceProofInfo) Read(reader io.Reader) []ProofInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ProofInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterProofInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceProofInfo) Lower(value []ProofInfo) C.RustBuffer {
	return LowerIntoRustBuffer[[]ProofInfo](c, value)
}

func (c FfiConverterSequenceProofInfo) LowerExternal(value []ProofInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ProofInfo](c, value))
}

func (c FfiConverterSequenceProofInfo) Write(writer io.Writer, value []ProofInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]ProofInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterProofInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceProofInfo struct{}

func (FfiDestroyerSequenceProofInfo) Destroy(sequence []ProofInfo) {
	for _, value := range sequence {
		FfiDestroyerProofInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceProofStateUpdate struct{}

var FfiConverterSequenceProofStateUpdateINSTANCE = FfiConverterSequenceProofStateUpdate{}

func (c FfiConverterSequenceProofStateUpdate) Lift(rb RustBufferI) []ProofStateUpdate {
	return LiftFromRustBuffer[[]ProofStateUpdate](c, rb)
}

func (c FfiConverterSequenceProofStateUpdate) Read(reader io.Reader) []ProofStateUpdate {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ProofStateUpdate, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterProofStateUpdateINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceProofStateUpdate) Lower(value []ProofStateUpdate) C.RustBuffer {
	return LowerIntoRustBuffer[[]ProofStateUpdate](c, value)
}

func (c FfiConverterSequenceProofStateUpdate) LowerExternal(value []ProofStateUpdate) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ProofStateUpdate](c, value))
}

func (c FfiConverterSequenceProofStateUpdate) Write(writer io.Writer, value []ProofStateUpdate) {
	if len(value) > math.MaxInt32 {
		panic("[]ProofStateUpdate is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterProofStateUpdateINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceProofStateUpdate struct{}

func (FfiDestroyerSequenceProofStateUpdate) Destroy(sequence []ProofStateUpdate) {
	for _, value := range sequence {
		FfiDestroyerProofStateUpdate{}.Destroy(value)
	}
}

type FfiConverterSequenceProtectedEndpoint struct{}

var FfiConverterSequenceProtectedEndpointINSTANCE = FfiConverterSequenceProtectedEndpoint{}

func (c FfiConverterSequenceProtectedEndpoint) Lift(rb RustBufferI) []ProtectedEndpoint {
	return LiftFromRustBuffer[[]ProtectedEndpoint](c, rb)
}

func (c FfiConverterSequenceProtectedEndpoint) Read(reader io.Reader) []ProtectedEndpoint {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ProtectedEndpoint, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterProtectedEndpointINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceProtectedEndpoint) Lower(value []ProtectedEndpoint) C.RustBuffer {
	return LowerIntoRustBuffer[[]ProtectedEndpoint](c, value)
}

func (c FfiConverterSequenceProtectedEndpoint) LowerExternal(value []ProtectedEndpoint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ProtectedEndpoint](c, value))
}

func (c FfiConverterSequenceProtectedEndpoint) Write(writer io.Writer, value []ProtectedEndpoint) {
	if len(value) > math.MaxInt32 {
		panic("[]ProtectedEndpoint is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterProtectedEndpointINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceProtectedEndpoint struct{}

func (FfiDestroyerSequenceProtectedEndpoint) Destroy(sequence []ProtectedEndpoint) {
	for _, value := range sequence {
		FfiDestroyerProtectedEndpoint{}.Destroy(value)
	}
}

type FfiConverterSequencePublicKey struct{}

var FfiConverterSequencePublicKeyINSTANCE = FfiConverterSequencePublicKey{}

func (c FfiConverterSequencePublicKey) Lift(rb RustBufferI) []PublicKey {
	return LiftFromRustBuffer[[]PublicKey](c, rb)
}

func (c FfiConverterSequencePublicKey) Read(reader io.Reader) []PublicKey {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]PublicKey, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterPublicKeyINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequencePublicKey) Lower(value []PublicKey) C.RustBuffer {
	return LowerIntoRustBuffer[[]PublicKey](c, value)
}

func (c FfiConverterSequencePublicKey) LowerExternal(value []PublicKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]PublicKey](c, value))
}

func (c FfiConverterSequencePublicKey) Write(writer io.Writer, value []PublicKey) {
	if len(value) > math.MaxInt32 {
		panic("[]PublicKey is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterPublicKeyINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequencePublicKey struct{}

func (FfiDestroyerSequencePublicKey) Destroy(sequence []PublicKey) {
	for _, value := range sequence {
		FfiDestroyerPublicKey{}.Destroy(value)
	}
}

type FfiConverterSequenceSecretKey struct{}

var FfiConverterSequenceSecretKeyINSTANCE = FfiConverterSequenceSecretKey{}

func (c FfiConverterSequenceSecretKey) Lift(rb RustBufferI) []SecretKey {
	return LiftFromRustBuffer[[]SecretKey](c, rb)
}

func (c FfiConverterSequenceSecretKey) Read(reader io.Reader) []SecretKey {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SecretKey, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterSecretKeyINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceSecretKey) Lower(value []SecretKey) C.RustBuffer {
	return LowerIntoRustBuffer[[]SecretKey](c, value)
}

func (c FfiConverterSequenceSecretKey) LowerExternal(value []SecretKey) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]SecretKey](c, value))
}

func (c FfiConverterSequenceSecretKey) Write(writer io.Writer, value []SecretKey) {
	if len(value) > math.MaxInt32 {
		panic("[]SecretKey is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterSecretKeyINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceSecretKey struct{}

func (FfiDestroyerSequenceSecretKey) Destroy(sequence []SecretKey) {
	for _, value := range sequence {
		FfiDestroyerSecretKey{}.Destroy(value)
	}
}

type FfiConverterSequenceSupportedMethod struct{}

var FfiConverterSequenceSupportedMethodINSTANCE = FfiConverterSequenceSupportedMethod{}

func (c FfiConverterSequenceSupportedMethod) Lift(rb RustBufferI) []SupportedMethod {
	return LiftFromRustBuffer[[]SupportedMethod](c, rb)
}

func (c FfiConverterSequenceSupportedMethod) Read(reader io.Reader) []SupportedMethod {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SupportedMethod, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterSupportedMethodINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceSupportedMethod) Lower(value []SupportedMethod) C.RustBuffer {
	return LowerIntoRustBuffer[[]SupportedMethod](c, value)
}

func (c FfiConverterSequenceSupportedMethod) LowerExternal(value []SupportedMethod) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]SupportedMethod](c, value))
}

func (c FfiConverterSequenceSupportedMethod) Write(writer io.Writer, value []SupportedMethod) {
	if len(value) > math.MaxInt32 {
		panic("[]SupportedMethod is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterSupportedMethodINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceSupportedMethod struct{}

func (FfiDestroyerSequenceSupportedMethod) Destroy(sequence []SupportedMethod) {
	for _, value := range sequence {
		FfiDestroyerSupportedMethod{}.Destroy(value)
	}
}

type FfiConverterSequenceTransaction struct{}

var FfiConverterSequenceTransactionINSTANCE = FfiConverterSequenceTransaction{}

func (c FfiConverterSequenceTransaction) Lift(rb RustBufferI) []Transaction {
	return LiftFromRustBuffer[[]Transaction](c, rb)
}

func (c FfiConverterSequenceTransaction) Read(reader io.Reader) []Transaction {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]Transaction, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTransactionINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTransaction) Lower(value []Transaction) C.RustBuffer {
	return LowerIntoRustBuffer[[]Transaction](c, value)
}

func (c FfiConverterSequenceTransaction) LowerExternal(value []Transaction) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]Transaction](c, value))
}

func (c FfiConverterSequenceTransaction) Write(writer io.Writer, value []Transaction) {
	if len(value) > math.MaxInt32 {
		panic("[]Transaction is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTransactionINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTransaction struct{}

func (FfiDestroyerSequenceTransaction) Destroy(sequence []Transaction) {
	for _, value := range sequence {
		FfiDestroyerTransaction{}.Destroy(value)
	}
}

type FfiConverterSequenceTransport struct{}

var FfiConverterSequenceTransportINSTANCE = FfiConverterSequenceTransport{}

func (c FfiConverterSequenceTransport) Lift(rb RustBufferI) []Transport {
	return LiftFromRustBuffer[[]Transport](c, rb)
}

func (c FfiConverterSequenceTransport) Read(reader io.Reader) []Transport {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]Transport, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTransportINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTransport) Lower(value []Transport) C.RustBuffer {
	return LowerIntoRustBuffer[[]Transport](c, value)
}

func (c FfiConverterSequenceTransport) LowerExternal(value []Transport) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]Transport](c, value))
}

func (c FfiConverterSequenceTransport) Write(writer io.Writer, value []Transport) {
	if len(value) > math.MaxInt32 {
		panic("[]Transport is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTransportINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTransport struct{}

func (FfiDestroyerSequenceTransport) Destroy(sequence []Transport) {
	for _, value := range sequence {
		FfiDestroyerTransport{}.Destroy(value)
	}
}

type FfiConverterSequenceCurrencyUnit struct{}

var FfiConverterSequenceCurrencyUnitINSTANCE = FfiConverterSequenceCurrencyUnit{}

func (c FfiConverterSequenceCurrencyUnit) Lift(rb RustBufferI) []CurrencyUnit {
	return LiftFromRustBuffer[[]CurrencyUnit](c, rb)
}

func (c FfiConverterSequenceCurrencyUnit) Read(reader io.Reader) []CurrencyUnit {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]CurrencyUnit, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterCurrencyUnitINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceCurrencyUnit) Lower(value []CurrencyUnit) C.RustBuffer {
	return LowerIntoRustBuffer[[]CurrencyUnit](c, value)
}

func (c FfiConverterSequenceCurrencyUnit) LowerExternal(value []CurrencyUnit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]CurrencyUnit](c, value))
}

func (c FfiConverterSequenceCurrencyUnit) Write(writer io.Writer, value []CurrencyUnit) {
	if len(value) > math.MaxInt32 {
		panic("[]CurrencyUnit is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterCurrencyUnitINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceCurrencyUnit struct{}

func (FfiDestroyerSequenceCurrencyUnit) Destroy(sequence []CurrencyUnit) {
	for _, value := range sequence {
		FfiDestroyerCurrencyUnit{}.Destroy(value)
	}
}

type FfiConverterSequenceProofState struct{}

var FfiConverterSequenceProofStateINSTANCE = FfiConverterSequenceProofState{}

func (c FfiConverterSequenceProofState) Lift(rb RustBufferI) []ProofState {
	return LiftFromRustBuffer[[]ProofState](c, rb)
}

func (c FfiConverterSequenceProofState) Read(reader io.Reader) []ProofState {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ProofState, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterProofStateINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceProofState) Lower(value []ProofState) C.RustBuffer {
	return LowerIntoRustBuffer[[]ProofState](c, value)
}

func (c FfiConverterSequenceProofState) LowerExternal(value []ProofState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ProofState](c, value))
}

func (c FfiConverterSequenceProofState) Write(writer io.Writer, value []ProofState) {
	if len(value) > math.MaxInt32 {
		panic("[]ProofState is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterProofStateINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceProofState struct{}

func (FfiDestroyerSequenceProofState) Destroy(sequence []ProofState) {
	for _, value := range sequence {
		FfiDestroyerProofState{}.Destroy(value)
	}
}

type FfiConverterSequenceSpendingConditions struct{}

var FfiConverterSequenceSpendingConditionsINSTANCE = FfiConverterSequenceSpendingConditions{}

func (c FfiConverterSequenceSpendingConditions) Lift(rb RustBufferI) []SpendingConditions {
	return LiftFromRustBuffer[[]SpendingConditions](c, rb)
}

func (c FfiConverterSequenceSpendingConditions) Read(reader io.Reader) []SpendingConditions {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SpendingConditions, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterSpendingConditionsINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceSpendingConditions) Lower(value []SpendingConditions) C.RustBuffer {
	return LowerIntoRustBuffer[[]SpendingConditions](c, value)
}

func (c FfiConverterSequenceSpendingConditions) LowerExternal(value []SpendingConditions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]SpendingConditions](c, value))
}

func (c FfiConverterSequenceSpendingConditions) Write(writer io.Writer, value []SpendingConditions) {
	if len(value) > math.MaxInt32 {
		panic("[]SpendingConditions is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterSpendingConditionsINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceSpendingConditions struct{}

func (FfiDestroyerSequenceSpendingConditions) Destroy(sequence []SpendingConditions) {
	for _, value := range sequence {
		FfiDestroyerSpendingConditions{}.Destroy(value)
	}
}

type FfiConverterSequenceSequenceString struct{}

var FfiConverterSequenceSequenceStringINSTANCE = FfiConverterSequenceSequenceString{}

func (c FfiConverterSequenceSequenceString) Lift(rb RustBufferI) [][]string {
	return LiftFromRustBuffer[[][]string](c, rb)
}

func (c FfiConverterSequenceSequenceString) Read(reader io.Reader) [][]string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([][]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterSequenceStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceSequenceString) Lower(value [][]string) C.RustBuffer {
	return LowerIntoRustBuffer[[][]string](c, value)
}

func (c FfiConverterSequenceSequenceString) LowerExternal(value [][]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[][]string](c, value))
}

func (c FfiConverterSequenceSequenceString) Write(writer io.Writer, value [][]string) {
	if len(value) > math.MaxInt32 {
		panic("[][]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterSequenceStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceSequenceString struct{}

func (FfiDestroyerSequenceSequenceString) Destroy(sequence [][]string) {
	for _, value := range sequence {
		FfiDestroyerSequenceString{}.Destroy(value)
	}
}

type FfiConverterMapUint64String struct{}

var FfiConverterMapUint64StringINSTANCE = FfiConverterMapUint64String{}

func (c FfiConverterMapUint64String) Lift(rb RustBufferI) map[uint64]string {
	return LiftFromRustBuffer[map[uint64]string](c, rb)
}

func (_ FfiConverterMapUint64String) Read(reader io.Reader) map[uint64]string {
	result := make(map[uint64]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterUint64INSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapUint64String) Lower(value map[uint64]string) C.RustBuffer {
	return LowerIntoRustBuffer[map[uint64]string](c, value)
}

func (c FfiConverterMapUint64String) LowerExternal(value map[uint64]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[uint64]string](c, value))
}

func (_ FfiConverterMapUint64String) Write(writer io.Writer, mapValue map[uint64]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[uint64]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterUint64INSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapUint64String struct{}

func (_ FfiDestroyerMapUint64String) Destroy(mapValue map[uint64]string) {
	for key, value := range mapValue {
		FfiDestroyerUint64{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringString struct{}

var FfiConverterMapStringStringINSTANCE = FfiConverterMapStringString{}

func (c FfiConverterMapStringString) Lift(rb RustBufferI) map[string]string {
	return LiftFromRustBuffer[map[string]string](c, rb)
}

func (_ FfiConverterMapStringString) Read(reader io.Reader) map[string]string {
	result := make(map[string]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringString) Lower(value map[string]string) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]string](c, value)
}

func (c FfiConverterMapStringString) LowerExternal(value map[string]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]string](c, value))
}

func (_ FfiConverterMapStringString) Write(writer io.Writer, mapValue map[string]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringString struct{}

func (_ FfiDestroyerMapStringString) Destroy(mapValue map[string]string) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringFeeAndAmounts struct{}

var FfiConverterMapStringFeeAndAmountsINSTANCE = FfiConverterMapStringFeeAndAmounts{}

func (c FfiConverterMapStringFeeAndAmounts) Lift(rb RustBufferI) map[string]FeeAndAmounts {
	return LiftFromRustBuffer[map[string]FeeAndAmounts](c, rb)
}

func (_ FfiConverterMapStringFeeAndAmounts) Read(reader io.Reader) map[string]FeeAndAmounts {
	result := make(map[string]FeeAndAmounts)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterFeeAndAmountsINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringFeeAndAmounts) Lower(value map[string]FeeAndAmounts) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]FeeAndAmounts](c, value)
}

func (c FfiConverterMapStringFeeAndAmounts) LowerExternal(value map[string]FeeAndAmounts) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]FeeAndAmounts](c, value))
}

func (_ FfiConverterMapStringFeeAndAmounts) Write(writer io.Writer, mapValue map[string]FeeAndAmounts) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]FeeAndAmounts is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterFeeAndAmountsINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringFeeAndAmounts struct{}

func (_ FfiDestroyerMapStringFeeAndAmounts) Destroy(mapValue map[string]FeeAndAmounts) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerFeeAndAmounts{}.Destroy(value)
	}
}

type FfiConverterMapMintUrlOptionalMintInfo struct{}

var FfiConverterMapMintUrlOptionalMintInfoINSTANCE = FfiConverterMapMintUrlOptionalMintInfo{}

func (c FfiConverterMapMintUrlOptionalMintInfo) Lift(rb RustBufferI) map[MintUrl]*MintInfo {
	return LiftFromRustBuffer[map[MintUrl]*MintInfo](c, rb)
}

func (_ FfiConverterMapMintUrlOptionalMintInfo) Read(reader io.Reader) map[MintUrl]*MintInfo {
	result := make(map[MintUrl]*MintInfo)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterMintUrlINSTANCE.Read(reader)
		value := FfiConverterOptionalMintInfoINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapMintUrlOptionalMintInfo) Lower(value map[MintUrl]*MintInfo) C.RustBuffer {
	return LowerIntoRustBuffer[map[MintUrl]*MintInfo](c, value)
}

func (c FfiConverterMapMintUrlOptionalMintInfo) LowerExternal(value map[MintUrl]*MintInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[MintUrl]*MintInfo](c, value))
}

func (_ FfiConverterMapMintUrlOptionalMintInfo) Write(writer io.Writer, mapValue map[MintUrl]*MintInfo) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[MintUrl]*MintInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterMintUrlINSTANCE.Write(writer, key)
		FfiConverterOptionalMintInfoINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapMintUrlOptionalMintInfo struct{}

func (_ FfiDestroyerMapMintUrlOptionalMintInfo) Destroy(mapValue map[MintUrl]*MintInfo) {
	for key, value := range mapValue {
		FfiDestroyerMintUrl{}.Destroy(key)
		FfiDestroyerOptionalMintInfo{}.Destroy(value)
	}
}

type FfiConverterMapWalletKeyAmount struct{}

var FfiConverterMapWalletKeyAmountINSTANCE = FfiConverterMapWalletKeyAmount{}

func (c FfiConverterMapWalletKeyAmount) Lift(rb RustBufferI) map[WalletKey]Amount {
	return LiftFromRustBuffer[map[WalletKey]Amount](c, rb)
}

func (_ FfiConverterMapWalletKeyAmount) Read(reader io.Reader) map[WalletKey]Amount {
	result := make(map[WalletKey]Amount)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterWalletKeyINSTANCE.Read(reader)
		value := FfiConverterAmountINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapWalletKeyAmount) Lower(value map[WalletKey]Amount) C.RustBuffer {
	return LowerIntoRustBuffer[map[WalletKey]Amount](c, value)
}

func (c FfiConverterMapWalletKeyAmount) LowerExternal(value map[WalletKey]Amount) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[WalletKey]Amount](c, value))
}

func (_ FfiConverterMapWalletKeyAmount) Write(writer io.Writer, mapValue map[WalletKey]Amount) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[WalletKey]Amount is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterWalletKeyINSTANCE.Write(writer, key)
		FfiConverterAmountINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapWalletKeyAmount struct{}

func (_ FfiDestroyerMapWalletKeyAmount) Destroy(mapValue map[WalletKey]Amount) {
	for key, value := range mapValue {
		FfiDestroyerWalletKey{}.Destroy(key)
		FfiDestroyerAmount{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
)

type rustFuturePollFunc func(C.uint64_t, C.UniffiRustFutureContinuationCallback, C.uint64_t)
type rustFutureCompleteFunc[T any] func(C.uint64_t, *C.RustCallStatus) T
type rustFutureFreeFunc func(C.uint64_t)

//export cdkffi_uniffiFutureContinuationCallback
func cdkffi_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiRustCallAsync[E any, T any, F any](
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
	pollFunc rustFuturePollFunc,
	freeFunc rustFutureFreeFunc,
) (T, E) {
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.cdkffi_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		pollResult = <-waiter
	}

	ffiValue, err := rustCallWithError(errConverter, func(status *C.RustCallStatus) F {
		return completeFunc(rustFuture, status)
	})
	return liftFunc(ffiValue), err
}

//export cdkffi_uniffiFreeGorutine
func cdkffi_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}

// Create a BIP 321 `bitcoin:` URI from raw string components.
//
// Combines optional `creq`, `lightning`, and `lno` query parameters into a
// single URI without requiring a `PaymentRequest` object.
//
// ```text
// val uri = createBip321Uri(
// creq = "CREQB1...",
// bolt11 = "lnbc100n1p...",
// bolt12 = "lno1qgsq..."
// )
// // => "bitcoin:?creq=CREQB1...&lightning=lnbc100n1p...&lno=lno1qgsq..."
// ```
func CreateBip321Uri(creq *string, bolt11 *string, bolt12 *string) string {
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_create_bip321_uri(FfiConverterOptionalStringINSTANCE.Lower(creq), FfiConverterOptionalStringINSTANCE.Lower(bolt11), FfiConverterOptionalStringINSTANCE.Lower(bolt12), _uniffiStatus),
		}
	}))
}

// Factory helpers returning a CDK wallet database behind the FFI trait
func CreateWalletDb(backend WalletDbBackend) (WalletDatabase, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_func_create_wallet_db(FfiConverterWalletDbBackendINSTANCE.Lower(backend), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue WalletDatabase
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWalletDatabaseINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a wallet store backed by a custom foreign-language database implementation.
//
// Wallet operations may later call write methods on the supplied database.
// Mobile hosts are responsible for making those callbacks lifecycle-safe and
// durable across app foreground/background transitions.
func CustomWalletStore(db WalletDatabase) WalletStore {
	return FfiConverterWalletStoreINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_custom_wallet_store(FfiConverterWalletDatabaseINSTANCE.Lower(db), _uniffiStatus),
		}
	}))
}

// Decode AuthProof from JSON string
func DecodeAuthProof(json string) (AuthProof, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_auth_proof(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue AuthProof
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAuthProofINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode Conditions from JSON string
func DecodeConditions(json string) (Conditions, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_conditions(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Conditions
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterConditionsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode ContactInfo from JSON string
func DecodeContactInfo(json string) (ContactInfo, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_contact_info(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ContactInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterContactInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode CreateRequestParams from JSON string
func DecodeCreateRequestParams(json string) (CreateRequestParams, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_create_request_params(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue CreateRequestParams
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCreateRequestParamsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode a bolt11 invoice or bolt12 offer from a string
//
// This function attempts to parse the input as a bolt11 invoice first,
// then as a bolt12 offer if bolt11 parsing fails.
//
// # Arguments
//
// * `invoice_str` - The invoice or offer string to decode
//
// # Returns
//
// * `Ok(DecodedInvoice)` - Successfully decoded invoice/offer information
// * `Err(FfiError)` - Failed to parse as either bolt11 or bolt12
//
// # Example
//
// ```kotlin
// val decoded = decodeInvoice("lnbc...")
// when (decoded.paymentType) {
// PaymentType.BOLT11 -> println("Bolt11 invoice")
// PaymentType.BOLT12 -> println("Bolt12 offer")
// }
// ```
func DecodeInvoice(invoiceStr string) (DecodedInvoice, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_invoice(FfiConverterStringINSTANCE.Lower(invoiceStr), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue DecodedInvoice
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterDecodedInvoiceINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode KeySet from JSON string
func DecodeKeySet(json string) (KeySet, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_key_set(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue KeySet
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterKeySetINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode KeySetInfo from JSON string
func DecodeKeySetInfo(json string) (KeySetInfo, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_key_set_info(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue KeySetInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterKeySetInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode Keys from JSON string
func DecodeKeys(json string) (Keys, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_keys(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Keys
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterKeysINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode MeltPrepareOptions from JSON string
func DecodeMeltPrepareOptions(json string) (MeltPrepareOptions, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_melt_prepare_options(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MeltPrepareOptions
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMeltPrepareOptionsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode MeltQuote from JSON string
func DecodeMeltQuote(json string) (MeltQuote, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_melt_quote(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MeltQuote
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMeltQuoteINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode MintInfo from JSON string
func DecodeMintInfo(json string) (MintInfo, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_mint_info(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MintInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMintInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode MintQuote from JSON string
func DecodeMintQuote(json string) (MintQuote, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_mint_quote(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MintQuote
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMintQuoteINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode MintVersion from JSON string
func DecodeMintVersion(json string) (MintVersion, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_mint_version(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MintVersion
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMintVersionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode Nuts from JSON string
func DecodeNuts(json string) (Nuts, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_nuts(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Nuts
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterNutsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode a payment request from its encoded string representation
func DecodePaymentRequest(encoded string) (*PaymentRequest, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_cdk_ffi_fn_func_decode_payment_request(FfiConverterStringINSTANCE.Lower(encoded), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *PaymentRequest
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterPaymentRequestINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode ProofInfo from JSON string
func DecodeProofInfo(json string) (ProofInfo, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_proof_info(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ProofInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterProofInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode ProofStateUpdate from JSON string
func DecodeProofStateUpdate(json string) (ProofStateUpdate, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_proof_state_update(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ProofStateUpdate
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterProofStateUpdateINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode ReceiveOptions from JSON string
func DecodeReceiveOptions(json string) (ReceiveOptions, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_receive_options(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ReceiveOptions
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterReceiveOptionsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode SendMemo from JSON string
func DecodeSendMemo(json string) (SendMemo, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_send_memo(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SendMemo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSendMemoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode SendOptions from JSON string
func DecodeSendOptions(json string) (SendOptions, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_send_options(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SendOptions
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSendOptionsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode SubscribeParams from JSON string
func DecodeSubscribeParams(json string) (SubscribeParams, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_subscribe_params(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue SubscribeParams
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSubscribeParamsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decode Transaction from JSON string
func DecodeTransaction(json string) (Transaction, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_decode_transaction(FfiConverterStringINSTANCE.Lower(json), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Transaction
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterTransactionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode AuthProof to JSON string
func EncodeAuthProof(proof AuthProof) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_auth_proof(FfiConverterAuthProofINSTANCE.Lower(proof), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode Conditions to JSON string
func EncodeConditions(conditions Conditions) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_conditions(FfiConverterConditionsINSTANCE.Lower(conditions), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode ContactInfo to JSON string
func EncodeContactInfo(info ContactInfo) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_contact_info(FfiConverterContactInfoINSTANCE.Lower(info), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode CreateRequestParams to JSON string
func EncodeCreateRequestParams(params CreateRequestParams) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_create_request_params(FfiConverterCreateRequestParamsINSTANCE.Lower(params), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode KeySet to JSON string
func EncodeKeySet(keyset KeySet) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_key_set(FfiConverterKeySetINSTANCE.Lower(keyset), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode KeySetInfo to JSON string
func EncodeKeySetInfo(info KeySetInfo) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_key_set_info(FfiConverterKeySetInfoINSTANCE.Lower(info), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode Keys to JSON string
func EncodeKeys(keys Keys) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_keys(FfiConverterKeysINSTANCE.Lower(keys), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode MeltPrepareOptions to JSON string
func EncodeMeltPrepareOptions(options MeltPrepareOptions) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_melt_prepare_options(FfiConverterMeltPrepareOptionsINSTANCE.Lower(options), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode MeltQuote to JSON string
func EncodeMeltQuote(quote MeltQuote) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_melt_quote(FfiConverterMeltQuoteINSTANCE.Lower(quote), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode MintInfo to JSON string
func EncodeMintInfo(info MintInfo) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_mint_info(FfiConverterMintInfoINSTANCE.Lower(info), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode MintQuote to JSON string
func EncodeMintQuote(quote MintQuote) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_mint_quote(FfiConverterMintQuoteINSTANCE.Lower(quote), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode MintVersion to JSON string
func EncodeMintVersion(version MintVersion) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_mint_version(FfiConverterMintVersionINSTANCE.Lower(version), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode Nuts to JSON string
func EncodeNuts(nuts Nuts) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_nuts(FfiConverterNutsINSTANCE.Lower(nuts), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode ProofInfo to JSON string
func EncodeProofInfo(info ProofInfo) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_proof_info(FfiConverterProofInfoINSTANCE.Lower(info), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode ProofStateUpdate to JSON string
func EncodeProofStateUpdate(update ProofStateUpdate) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_proof_state_update(FfiConverterProofStateUpdateINSTANCE.Lower(update), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode ReceiveOptions to JSON string
func EncodeReceiveOptions(options ReceiveOptions) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_receive_options(FfiConverterReceiveOptionsINSTANCE.Lower(options), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode SendMemo to JSON string
func EncodeSendMemo(memo SendMemo) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_send_memo(FfiConverterSendMemoINSTANCE.Lower(memo), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode SendOptions to JSON string
func EncodeSendOptions(options SendOptions) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_send_options(FfiConverterSendOptionsINSTANCE.Lower(options), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode SubscribeParams to JSON string
func EncodeSubscribeParams(params SubscribeParams) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_subscribe_params(FfiConverterSubscribeParamsINSTANCE.Lower(params), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encode Transaction to JSON string
func EncodeTransaction(transaction Transaction) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_encode_transaction(FfiConverterTransactionINSTANCE.Lower(transaction), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Generates a new random mnemonic phrase
func GenerateMnemonic() (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_generate_mnemonic(_uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Initialize logging with default "info" level
func InitDefaultLogging() {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_func_init_default_logging(_uniffiStatus)
		return false
	})
}

// Initialize the tracing subscriber for stdout logging.
//
// This function sets up a tracing subscriber that outputs logs to stdout,
// making them visible when using the FFI from other languages.
//
// Call this function once at application startup, before creating
// any wallets. Subsequent calls are safe but have no effect.
//
// # Arguments
//
// * `level` - Log level filter (e.g., "debug", "info", "warn", "error", "trace")
//
// # Example (from Flutter/Dart)
//
// ```dart
// await CdkFfi.initLogging("debug");
// // Now all logs will be visible in stdout
// final wallet = await WalletRepository.create(...);
// ```
func InitLogging(level string) {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_func_init_logging(FfiConverterStringINSTANCE.Lower(level), _uniffiStatus)
		return false
	})
}

// Get amount that can be minted from a mint quote
func MintQuoteAmountMintable(quote MintQuote) (Amount, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_mint_quote_amount_mintable(FfiConverterMintQuoteINSTANCE.Lower(quote), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Amount
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAmountINSTANCE.Lift(_uniffiRV), nil
	}
}

// Check if mint quote is expired
func MintQuoteIsExpired(quote MintQuote, currentTime uint64) (bool, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_func_mint_quote_is_expired(FfiConverterMintQuoteINSTANCE.Lower(quote), FfiConverterUint64INSTANCE.Lower(currentTime), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue bool
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBoolINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get total amount for a mint quote (amount paid)
func MintQuoteTotalAmount(quote MintQuote) (Amount, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_mint_quote_total_amount(FfiConverterMintQuoteINSTANCE.Lower(quote), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Amount
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAmountINSTANCE.Lift(_uniffiRV), nil
	}
}

// Converts a mnemonic phrase to its entropy bytes
func MnemonicToEntropy(mnemonic string) ([]byte, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_mnemonic_to_entropy(FfiConverterStringINSTANCE.Lower(mnemonic), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []byte
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBytesINSTANCE.Lift(_uniffiRV), nil
	}
}

// Decrypt a NIP-44 v2 payload
//
// Derives the conversation key via secp256k1 ECDH between `nostr_secret_key`
// and `sender_pubkey` and decrypts the base64-encoded payload.
//
// # Arguments
//
// * `nostr_secret_key` - Recipient secret key (hex or bech32 `nsec`)
// * `sender_pubkey` - Sender x-only public key (hex, 64 characters)
// * `payload` - Base64-encoded NIP-44 v2 payload
//
// # Errors
//
// Returns an error if a key is invalid, the payload is malformed, or MAC
// verification fails
func Nip44Decrypt(nostrSecretKey string, senderPubkey string, payload string) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nip44_decrypt(FfiConverterStringINSTANCE.Lower(nostrSecretKey), FfiConverterStringINSTANCE.Lower(senderPubkey), FfiConverterStringINSTANCE.Lower(payload), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Encrypt a message with NIP-44 v2
//
// Derives the conversation key via secp256k1 ECDH between `nostr_secret_key`
// and `recipient_pubkey` and returns the base64-encoded payload.
//
// # Arguments
//
// * `nostr_secret_key` - Sender secret key (hex or bech32 `nsec`)
// * `recipient_pubkey` - Recipient x-only public key (hex, 64 characters)
// * `plaintext` - Message to encrypt
//
// # Errors
//
// Returns an error if a key is invalid or encryption fails
func Nip44Encrypt(nostrSecretKey string, recipientPubkey string, plaintext string) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nip44_encrypt(FfiConverterStringINSTANCE.Lower(nostrSecretKey), FfiConverterStringINSTANCE.Lower(recipientPubkey), FfiConverterStringINSTANCE.Lower(plaintext), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Generate a new random Nostr secret key
//
// # Returns
//
// The hex-encoded secret key (64 characters)
func NostrGenerateSecretKey() string {
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nostr_generate_secret_key(_uniffiStatus),
		}
	}))
}

// Get the public key for a Nostr secret key
//
// # Arguments
//
// * `nostr_secret_key` - Nostr secret key. Accepts either:
// - Hex-encoded secret key (64 characters)
// - Bech32 `nsec` format (e.g., "nsec1...")
//
// # Returns
//
// The hex-encoded x-only public key (64 characters)
//
// # Errors
//
// Returns an error if the secret key is invalid
func NostrGetPubkey(nostrSecretKey string) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nostr_get_pubkey(FfiConverterStringINSTANCE.Lower(nostrSecretKey), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Derive Nostr keys from a wallet seed
//
// This function derives the same Nostr keys that a wallet would use for NpubCash
// authentication, using the NIP-06 path `m/44'/1237'/0'/0/0`.
//
// # Arguments
//
// * `seed` - The wallet seed bytes (must be at least 64 bytes)
//
// # Returns
//
// The hex-encoded Nostr secret key that can be used with `NpubCashClient::new()`
//
// # Errors
//
// Returns an error if the seed is too short or key derivation fails
func NpubcashDeriveSecretKeyFromSeed(seed []byte) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_npubcash_derive_secret_key_from_seed(FfiConverterBytesINSTANCE.Lower(seed), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the public key for a given Nostr secret key
//
// # Arguments
//
// * `nostr_secret_key` - Nostr secret key. Accepts either:
// - Hex-encoded secret key (64 characters)
// - Bech32 `nsec` format (e.g., "nsec1...")
//
// # Returns
//
// # The hex-encoded public key
//
// # Errors
//
// Returns an error if the secret key is invalid
func NpubcashGetPubkey(nostrSecretKey string) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_npubcash_get_pubkey(FfiConverterStringINSTANCE.Lower(nostrSecretKey), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Convert a NpubCash quote to a wallet MintQuote
//
// This allows the quote to be used with the wallet's minting functions.
// Note that the resulting MintQuote will not have a secret key set,
// which may be required for locked quotes.
//
// # Arguments
//
// * `quote` - The NpubCash quote to convert
//
// # Returns
//
// A MintQuote that can be used with wallet minting functions
func NpubcashQuoteToMintQuote(quote NpubCashQuote) MintQuote {
	return FfiConverterMintQuoteINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_npubcash_quote_to_mint_quote(FfiConverterNpubCashQuoteINSTANCE.Lower(quote), _uniffiStatus),
		}
	}))
}

// Derive the NWC wallet-service secret key from a wallet seed.
//
// Returns a hex-encoded secret key for use as `service_secret_key`. Deriving
// from the seed keeps the connection URI stable across restarts. Uses the
// NIP-06 path `m/44'/1237'/1'/0/0`, distinct from the npub.cash key.
//
// # Errors
//
// Returns an error if the seed is shorter than 64 bytes or derivation fails.
func NwcDeriveServiceSecretKeyFromSeed(seed []byte) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nwc_derive_service_secret_key_from_seed(FfiConverterBytesINSTANCE.Lower(seed), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Get the hex-encoded public key for a Nostr secret key (hex or `nsec`).
//
// # Errors
//
// Returns an error if the secret key is invalid.
func NwcGetPubkey(nostrSecretKey string) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_nwc_get_pubkey(FfiConverterStringINSTANCE.Lower(nostrSecretKey), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Parse a BIP 321 `bitcoin:` URI or standalone payment instruction string,
// validating on-chain addresses against the given [`bitcoin::Network`].
//
// Returns a [`ParsedPaymentInstruction`] from a BIP 321 URI or standalone
// payment string (Cashu, BOLT11, BOLT12, or on-chain address).
//
// ```text
// val parsed = parseBip321PaymentInstruction(
// "bitcoin:?creq=CREQB1...&lightning=lnbc100n1p...",
// Network.BITCOIN
// )
// // parsed.cashuRequests and parsed.bolt11Invoices are populated when present
// ```
func ParseBip321PaymentInstruction(instruction string, network BitcoinNetwork) (ParsedPaymentInstruction, error) {
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) ParsedPaymentInstruction {
			return FfiConverterParsedPaymentInstructionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_func_parse_bip321_payment_instruction(FfiConverterStringINSTANCE.Lower(instruction), FfiConverterBitcoinNetworkINSTANCE.Lower(network)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Check if proof has DLEQ proof
func ProofHasDleq(proof Proof) bool {
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_func_proof_has_dleq(FfiConverterProofINSTANCE.Lower(proof), _uniffiStatus)
	}))
}

// Check if proof is active with given keyset IDs
func ProofIsActive(proof Proof, activeKeysetIds []string) bool {
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_func_proof_is_active(FfiConverterProofINSTANCE.Lower(proof), FfiConverterSequenceStringINSTANCE.Lower(activeKeysetIds), _uniffiStatus)
	}))
}

// Sign a P2PK proof with a secret key, returning a new signed proof
func ProofSignP2pk(proof Proof, secretKeyHex string) (Proof, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_proof_sign_p2pk(FfiConverterProofINSTANCE.Lower(proof), FfiConverterStringINSTANCE.Lower(secretKeyHex), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Proof
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterProofINSTANCE.Lift(_uniffiRV), nil
	}
}

// Verify DLEQ proof on a proof
func ProofVerifyDleq(proof Proof, mintPubkey PublicKey) error {
	_, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_func_proof_verify_dleq(FfiConverterProofINSTANCE.Lower(proof), FfiConverterPublicKeyINSTANCE.Lower(mintPubkey), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Verify HTLC witness on a proof
func ProofVerifyHtlc(proof Proof) error {
	_, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_cdk_ffi_fn_func_proof_verify_htlc(FfiConverterProofINSTANCE.Lower(proof), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Get the Y value (hash_to_curve of secret) for a proof
func ProofY(proof Proof) (string, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_proof_y(FfiConverterProofINSTANCE.Lower(proof), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Helper function to calculate total amount of proofs
func ProofsTotalAmount(proofs []Proof) (Amount, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_proofs_total_amount(FfiConverterSequenceProofINSTANCE.Lower(proofs), _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Amount
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterAmountINSTANCE.Lift(_uniffiRV), nil
	}
}

// Resolve a BIP353 human-readable address into a parsed payment instruction.
//
// This uses the wallet's configured connector to resolve the address, then parses the resolved
// `bitcoin:` URI into a [`ParsedPaymentInstruction`] so callers can inspect the available
// methods before deciding how to pay or whether a BIP353 melt is possible.
//
// The `network` parameter controls which on-chain address prefixes are accepted
// in the resolved URI.
func ResolveBip353PaymentInstruction(wallet *Wallet, address string, network BitcoinNetwork) (ParsedPaymentInstruction, error) {
	res, err := uniffiRustCallAsync[*FfiError](
		FfiConverterFfiErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_cdk_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) ParsedPaymentInstruction {
			return FfiConverterParsedPaymentInstructionINSTANCE.Lift(ffi)
		},
		C.uniffi_cdk_ffi_fn_func_resolve_bip353_payment_instruction(FfiConverterWalletINSTANCE.Lower(wallet), FfiConverterStringINSTANCE.Lower(address), FfiConverterBitcoinNetworkINSTANCE.Lower(network)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_cdk_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Create a SQLite-backed wallet store.
//
// Wallet operations may later write to this SQLite database. Mobile hosts are
// responsible for choosing a durable file path and coordinating background
// lifecycle transitions around wallet calls.
func SqliteWalletStore(path string) WalletStore {
	return FfiConverterWalletStoreINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_cdk_ffi_fn_func_sqlite_wallet_store(FfiConverterStringINSTANCE.Lower(path), _uniffiStatus),
		}
	}))
}

// Check if a transaction matches the given filter conditions
func TransactionMatchesConditions(transaction Transaction, mintUrl *MintUrl, direction *TransactionDirection, unit *CurrencyUnit) (bool, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*FfiError](FfiConverterFfiError{}, func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_cdk_ffi_fn_func_transaction_matches_conditions(FfiConverterTransactionINSTANCE.Lower(transaction), FfiConverterOptionalMintUrlINSTANCE.Lower(mintUrl), FfiConverterOptionalTransactionDirectionINSTANCE.Lower(direction), FfiConverterOptionalCurrencyUnitINSTANCE.Lower(unit), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue bool
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterBoolINSTANCE.Lift(_uniffiRV), nil
	}
}
