//go:build linux && mips

package cdk_ffi

// The linux_mips native library is built WITHOUT the nostr feature (32-bit
// MIPS has no std AtomicU64, which nostr-relay-pool requires — the
// jmcorgan/fips wall). The generated bindings still reference a handful of
// nostr-only FFI entry points; they are unreachable on the gateway's wallet
// paths, so they resolve to loud aborts rather than link failures. If one
// of these ever fires, the caller tried to use a nostr feature on mips —
// not supported by this build.
//
// #include <stdlib.h>
// #include <stdint.h>
// #include <stdio.h>
// static void mips_nostr_abort(const char *name) {
//     fprintf(stderr, "cdk_ffi(mips): nostr feature %s is not built into the linux_mips native library\n", name);
//     abort();
// }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequest_unit(void *ptr) { (void)ptr; mips_nostr_abort("paymentrequest_unit"); return 0; }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequestpayload_id(void *ptr) { (void)ptr; mips_nostr_abort("prp_id"); return 0; }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequestpayload_memo(void *ptr) { (void)ptr; mips_nostr_abort("prp_memo"); return 0; }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequestpayload_mint(void *ptr) { (void)ptr; mips_nostr_abort("prp_mint"); return 0; }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequestpayload_proofs(void *ptr) { (void)ptr; mips_nostr_abort("prp_proofs"); return 0; }
// int8_t uniffi_cdk_ffi_fn_method_paymentrequestpayload_unit(void *ptr) { (void)ptr; mips_nostr_abort("prp_unit"); return 0; }
// uint16_t uniffi_cdk_ffi_checksum_func_decode_proof_state_update(void) { mips_nostr_abort("decode_proof_state_update"); return 0; }
import "C"
