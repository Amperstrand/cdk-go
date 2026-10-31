//go:build linux && mipsle

package cdk_ffi

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdio.h>
// The linux_mipsel native library is built WITHOUT the nostr/nwc/npubcash
// features (32-bit MIPS lacks std AtomicU64, which nostr-relay-pool
// requires). The generated bindings reference these entry points.
// - checksum_* must return the SAME constants the bindings assert at
//   package init (values extracted from cdk_ffi.go, 2026-10-31), or every
//   process panics at startup with a checksum mismatch.
// - the callback-vtable init runs unconditionally at init: no-op it.
// - everything else is unreachable on gateway wallet paths: loud abort.
static void mips_nostr_abort(const char *name) {
    fprintf(stderr, "cdk_ffi(mips): %s is not built into the linux_mipsel native library\n", name);
    abort();
}
uint16_t uniffi_cdk_ffi_checksum_constructor_nostrinbox_new(void) { return 44300; }
uint16_t uniffi_cdk_ffi_checksum_constructor_npubcashclient_new(void) { return 49637; }
uint16_t uniffi_cdk_ffi_checksum_constructor_nwcservice_create(void) { return 38371; }
uint16_t uniffi_cdk_ffi_checksum_constructor_nwcservice_restore(void) { return 58630; }
uint16_t uniffi_cdk_ffi_checksum_func_nip44_decrypt(void) { return 39962; }
uint16_t uniffi_cdk_ffi_checksum_func_nip44_encrypt(void) { return 485; }
uint16_t uniffi_cdk_ffi_checksum_func_nostr_generate_secret_key(void) { return 13275; }
uint16_t uniffi_cdk_ffi_checksum_func_nostr_get_pubkey(void) { return 3969; }
uint16_t uniffi_cdk_ffi_checksum_func_npubcash_derive_secret_key_from_seed(void) { return 6473; }
uint16_t uniffi_cdk_ffi_checksum_func_npubcash_get_pubkey(void) { return 28438; }
uint16_t uniffi_cdk_ffi_checksum_func_npubcash_quote_to_mint_quote(void) { return 58675; }
uint16_t uniffi_cdk_ffi_checksum_func_nwc_derive_service_secret_key_from_seed(void) { return 58335; }
uint16_t uniffi_cdk_ffi_checksum_func_nwc_get_pubkey(void) { return 3708; }
uint16_t uniffi_cdk_ffi_checksum_func_resolve_bip353_payment_instruction(void) { return 39566; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrinboxlistener_on_event(void) { return 50983; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrinbox_pubkey(void) { return 9918; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrinbox_start(void) { return 36353; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrinbox_stop(void) { return 16312; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mint_preferred(void) { return 11066; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrwaitinfo_mints(void) { return 4177; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrwaitinfo_pubkey(void) { return 8372; }
uint16_t uniffi_cdk_ffi_checksum_method_nostrwaitinfo_relays(void) { return 40910; }
uint16_t uniffi_cdk_ffi_checksum_method_npubcashclient_get_missing_quotes(void) { return 31413; }
uint16_t uniffi_cdk_ffi_checksum_method_npubcashclient_get_quotes(void) { return 64169; }
uint16_t uniffi_cdk_ffi_checksum_method_npubcashclient_get_user_info(void) { return 31487; }
uint16_t uniffi_cdk_ffi_checksum_method_npubcashclient_set_mint_url(void) { return 8738; }
uint16_t uniffi_cdk_ffi_checksum_method_npubcashclient_set_quote_locking(void) { return 61660; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_client_pubkey(void) { return 1577; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_connection_uri(void) { return 23241; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_is_running(void) { return 55514; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_service_pubkey(void) { return 21181; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_start(void) { return 29349; }
uint16_t uniffi_cdk_ffi_checksum_method_nwcservice_stop(void) { return 4827; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_claim_npubcash_quotes(void) { return 50606; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_enable_npubcash(void) { return 16091; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_get_npubcash_user_info(void) { return 28646; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_melt_bip353_quote(void) { return 1058; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable(void) { return 32210; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_melt_human_readable_quote(void) { return 20299; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_melt_lightning_address_quote(void) { return 35934; }
uint16_t uniffi_cdk_ffi_checksum_method_walletrepository_backup_mints(void) { return 56268; }
uint16_t uniffi_cdk_ffi_checksum_method_walletrepository_fetch_mint_backup(void) { return 24968; }
uint16_t uniffi_cdk_ffi_checksum_method_walletrepository_mint_backup_public_key(void) { return 19486; }
uint16_t uniffi_cdk_ffi_checksum_method_walletrepository_restore_mints(void) { return 31371; }
uint16_t uniffi_cdk_ffi_checksum_method_wallet_sync_missing_npubcash_quotes(void) { return 17258; }
void uniffi_cdk_ffi_fn_init_callback_vtable_nostrinboxlistener(const void *vtable) { (void)vtable; }
void uniffi_cdk_ffi_fn_clone_nostrinbox(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_clone_nostrinbox"); }
void uniffi_cdk_ffi_fn_clone_nostrinboxlistener(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_clone_nostrinboxlistener"); }
void uniffi_cdk_ffi_fn_clone_nostrwaitinfo(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_clone_nostrwaitinfo"); }
void uniffi_cdk_ffi_fn_clone_npubcashclient(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_clone_npubcashclient"); }
void uniffi_cdk_ffi_fn_clone_nwcservice(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_clone_nwcservice"); }
void uniffi_cdk_ffi_fn_constructor_nostrinbox_new(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_constructor_nostrinbox_new"); }
void uniffi_cdk_ffi_fn_constructor_npubcashclient_new(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_constructor_npubcashclient_new"); }
void uniffi_cdk_ffi_fn_constructor_nwcservice_create(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_constructor_nwcservice_create"); }
void uniffi_cdk_ffi_fn_constructor_nwcservice_restore(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_constructor_nwcservice_restore"); }
void uniffi_cdk_ffi_fn_free_nostrinbox(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_free_nostrinbox"); }
void uniffi_cdk_ffi_fn_free_nostrinboxlistener(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_free_nostrinboxlistener"); }
void uniffi_cdk_ffi_fn_free_nostrwaitinfo(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_free_nostrwaitinfo"); }
void uniffi_cdk_ffi_fn_free_npubcashclient(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_free_npubcashclient"); }
void uniffi_cdk_ffi_fn_free_nwcservice(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_free_nwcservice"); }
void uniffi_cdk_ffi_fn_func_nip44_decrypt(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nip44_decrypt"); }
void uniffi_cdk_ffi_fn_func_nip44_encrypt(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nip44_encrypt"); }
void uniffi_cdk_ffi_fn_func_nostr_generate_secret_key(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nostr_generate_secret_key"); }
void uniffi_cdk_ffi_fn_func_nostr_get_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nostr_get_pubkey"); }
void uniffi_cdk_ffi_fn_func_npubcash_derive_secret_key_from_seed(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_npubcash_derive_secret_key_from_seed"); }
void uniffi_cdk_ffi_fn_func_npubcash_get_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_npubcash_get_pubkey"); }
void uniffi_cdk_ffi_fn_func_npubcash_quote_to_mint_quote(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_npubcash_quote_to_mint_quote"); }
void uniffi_cdk_ffi_fn_func_nwc_derive_service_secret_key_from_seed(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nwc_derive_service_secret_key_from_seed"); }
void uniffi_cdk_ffi_fn_func_nwc_get_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_nwc_get_pubkey"); }
void uniffi_cdk_ffi_fn_func_resolve_bip353_payment_instruction(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_func_resolve_bip353_payment_instruction"); }
void uniffi_cdk_ffi_fn_method_nostrinboxlistener_on_event(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrinboxlistener_on_event"); }
void uniffi_cdk_ffi_fn_method_nostrinbox_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrinbox_pubkey"); }
void uniffi_cdk_ffi_fn_method_nostrinbox_start(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrinbox_start"); }
void uniffi_cdk_ffi_fn_method_nostrinbox_stop(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrinbox_stop"); }
void uniffi_cdk_ffi_fn_method_nostrwaitinfo_mint_preferred(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrwaitinfo_mint_preferred"); }
void uniffi_cdk_ffi_fn_method_nostrwaitinfo_mints(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrwaitinfo_mints"); }
void uniffi_cdk_ffi_fn_method_nostrwaitinfo_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrwaitinfo_pubkey"); }
void uniffi_cdk_ffi_fn_method_nostrwaitinfo_relays(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nostrwaitinfo_relays"); }
void uniffi_cdk_ffi_fn_method_npubcashclient_get_missing_quotes(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_npubcashclient_get_missing_quotes"); }
void uniffi_cdk_ffi_fn_method_npubcashclient_get_quotes(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_npubcashclient_get_quotes"); }
void uniffi_cdk_ffi_fn_method_npubcashclient_get_user_info(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_npubcashclient_get_user_info"); }
void uniffi_cdk_ffi_fn_method_npubcashclient_set_mint_url(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_npubcashclient_set_mint_url"); }
void uniffi_cdk_ffi_fn_method_npubcashclient_set_quote_locking(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_npubcashclient_set_quote_locking"); }
void uniffi_cdk_ffi_fn_method_nwcservice_client_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_client_pubkey"); }
void uniffi_cdk_ffi_fn_method_nwcservice_connection_uri(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_connection_uri"); }
void uniffi_cdk_ffi_fn_method_nwcservice_is_running(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_is_running"); }
void uniffi_cdk_ffi_fn_method_nwcservice_service_pubkey(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_service_pubkey"); }
void uniffi_cdk_ffi_fn_method_nwcservice_start(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_start"); }
void uniffi_cdk_ffi_fn_method_nwcservice_stop(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_nwcservice_stop"); }
void uniffi_cdk_ffi_fn_method_wallet_claim_npubcash_quotes(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_claim_npubcash_quotes"); }
void uniffi_cdk_ffi_fn_method_wallet_enable_npubcash(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_enable_npubcash"); }
void uniffi_cdk_ffi_fn_method_wallet_get_npubcash_user_info(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_get_npubcash_user_info"); }
void uniffi_cdk_ffi_fn_method_wallet_melt_bip353_quote(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_melt_bip353_quote"); }
void uniffi_cdk_ffi_fn_method_wallet_melt_human_readable(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_melt_human_readable"); }
void uniffi_cdk_ffi_fn_method_wallet_melt_human_readable_quote(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_melt_human_readable_quote"); }
void uniffi_cdk_ffi_fn_method_wallet_melt_lightning_address_quote(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_melt_lightning_address_quote"); }
void uniffi_cdk_ffi_fn_method_walletrepository_backup_mints(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_walletrepository_backup_mints"); }
void uniffi_cdk_ffi_fn_method_walletrepository_fetch_mint_backup(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_walletrepository_fetch_mint_backup"); }
void uniffi_cdk_ffi_fn_method_walletrepository_mint_backup_public_key(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_walletrepository_mint_backup_public_key"); }
void uniffi_cdk_ffi_fn_method_walletrepository_restore_mints(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_walletrepository_restore_mints"); }
void uniffi_cdk_ffi_fn_method_wallet_sync_missing_npubcash_quotes(void) { mips_nostr_abort("uniffi_cdk_ffi_fn_method_wallet_sync_missing_npubcash_quotes"); }
*/
import "C"
