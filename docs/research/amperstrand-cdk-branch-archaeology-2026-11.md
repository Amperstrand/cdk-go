# Amperstrand/cdk branch archaeology — 2026-11-03

Campaign companion to `cdk-wallet-backend-evaluation-2026-11.md`. Method:
per-branch GitHub compare vs `cashubtc:main` (ahead_by = unique commits,
`.commits[-6:]` subjects = the branch's actual work), cross-referenced
against the cashu-audit divergence registry, the campaign brief, and
upstream log searches for merged-status. Owner context: "they were all
experiments and dogfooding."

**State changes this session (all reversible-by-SHA):**
- `main` fast-forwarded `d033f1bd` → `5e1cba0c` (= upstream head; was 346
  behind, 0 unique).
- 21 branches deleted (zero-information-loss set — see §deleted).
- 22 branches remain.

## Deleted (21) — with recorded heads

| Branch | Head | Why it was safe |
|---|---|---|
| v0.10.x…v0.15.x (6) | mirrors | **0/0 identical to upstream's same-name branches** — pure copies; upstream owns the history |
| `spark` | d3dec24c | 0 unique commits (pure ancestor of main) |
| `dependabot/*` (7) | 0507ff1b, 59a45fe7, f8bb428b, eea72019, 5e25c948, c45a30ae, 2f81eb1e | stale bot branches; upstream's dependabot manages its own |
| `ln_cln` | 037cd3a0 | June-2024 CLN backend experiment; upstream now ships a CLN backend — superseded |
| `tests-tokenv4-try-from-tokenv3` | e01d240a | Sept-2024 token test; long upstream |
| `build_on_mac` | ee148eaf | "add macos builds to release" — upstream now ships darwin builds (cdk-go prebuilts include them) |
| `htlc-refund-witness` | d1eeb519 | our signature-only-HTLC-refund fix — **superseded upstream by `cd8c84d9`** "fix(cashu): accept signature-only HTLC refund witnesses" (registry: cdk#2252 item 6) |
| `backport-1561-to-v0.14.x` | e096a1b8 | cherry-picks of upstream's own PRs onto the 0.14 line; content exists upstream |
| `fix/remove-test-feature-from-cdk-sqlite` | 32ecd96e | May-2026 build fix (unconditional `test` feature); obsoleted by upstream evolution, trivially re-derivable |
| `fix/v0.16-remove-test-feature` | 2a2896bc | same, v0.16 era |

## Remaining (22) — reason to live

### Live carriers (the fork's actual current purpose)

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| **`experimentalaislop`** (DEFAULT) | 25 | **The cdk-mintd-wrt carrier**: "carry: OpenWrt packaging + CI for cdk-mintd-wrt onto current upstream" + 5-commit drift threshold doc. Born as an AI-asslop experiment, properly superseded by `rebase/cdk-upstream-sync` (PR #8) into a clean upstream-based carrier. | KEEP — but it is the fork's default branch under a legacy joke name; **rename to `carry/openwrt-mintd`** (owner action; update lab references first) |
| `experimental/arch` | 61 | Campaign arch lane: "prove cdk-ffi staticlib builds for router arches", fresh tip 2026-10-28, carries 0.18.2 bump + mintd legacy-import fixes + "recover mint quote signing keys from seed". Feeds `Amperstrand/cdk-go/experimental/arch` (cdk_static tags, shims). | KEEP — active campaign lane |
| `tollgate` | 60 | Dogfood mint: lnd fixes ("round satoshi payment quotes up"), legacy Nutshell keyset import verification, SQLCipher import tests. Overlaps the `experimentalaislop` carrier + `experimental/arch` fixes. | KEEP — consolidation candidate with the carrier (owner call) |
| `rebase/cdk-upstream-sync` | 24 | The sync machinery that superseded pre-sync experimentalaislop; tip carries "Document MIPS AtomicUsize fix verification". | KEEP until that MIPS doc is ported into the carrier, then deletable |

### Staged upstream contributions (registry-backed)

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| `fix/duplicate-outputs-error-identity` | 62 | **Registry row (2026-10-29)**: mint maps duplicate blind-signature inserts to NUT-19 11003 instead of raw Database::Duplicate — unbricks seed-reusing wallets (cdk-mintd). Tip 2026-10-28, freshest branch. Prior-art sweep + filing = pending owner decision. | KEEP — active staged fix |
| `stream-stall-fix` | 3 | "fix(lnd): enable HTTP/2 keepalive on gRPC client" + timer + docs. **Not upstream** (verified by log search) — a genuine fix every LND-backed cdk mint wants. | KEEP → EXTRACT/UPSTREAM (candidate contribution; draft owner-gated) |

### Ecosystem experiment arms (registry NUT-00 campaign)

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| `nut00-per-keyset-leniency` | 1 | per-keyset legacy hex-decode leniency + rejection sensor — one of the staged fix arms in registry row 34 ("@nut00-per-keyset-leniency"). | KEEP (archive; reference implementation) |
| `nut00-legacy-v0.18.0` | 1 | legacy pre-0.15.1 hash-algorithm redemption + dual sensors, v0.18.0 backport shape. | KEEP (archive) |
| `legacy-three-modes` | 1 | three-mode legacy redemption policy (allow/observe/rugpull) — the policy experiment behind the position doc. | KEEP (archive) |

### Owner-project work

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| `pecan-nut32` | 8 | NUT-32 custom-unit resolution for pecan (runtime processors on melt saga, mint/melt quote paths; unit grammar alignment). | KEEP — pecan lane |
| `feat/fakewallet-voting-poc` | 12 | verifiable vote ledger w/ Merkle proofs, anonymity guide, e2e CI. | KEEP (only copy; owner decides if the idea is alive) |
| `feat/fakewallet-manual-approval` | 2 | manual-approval fakewallet for testing. | KEEP (only copy) |
| `feat/keyset-voting-exploration`, `feat/voting-side-channel` | (family) | keyset-voting scaffold; VoteBackend side-channel. | KEEP (only copies) |

### Campaign-relevant technique

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| **`slim-client-build`** | 1 | "slim client-only build — remove nostr, add LTO, **18 MB (was 29 MB)**" (2026-07-25). Exactly the size-optimization recipe campaign item 3 (16 MB MIPS tier) needs. | KEEP — **fold into campaign item 3** (re-apply technique on 0.18.3: strip nostr + LTO + musl static) |
| `fix/mips-atomicusize-v0.17.3` | 1 | MIPS AtomicUsize fix on 0.17.3 (feeds the mips-ffi lane). | KEEP — MIPS lane |
| `batch-spent-send-recovery` | 1 | draft: batch spent-check for Send/TokenCreated sagas at recovery (+ sql saga-index perf). | KEEP — draft, possibly upstreamable |

### Audit tooling artifacts

| Branch | Unique | Purpose | Verdict |
|---|---|---|---|
| `experiment/greatspectations-audit` | 6 | 125+ NUT spec-quote comments embedded in cdk source + BIP pins + divergence notes (the greatspectations audit lane). | KEEP (archive; annotations exist only here) |
| `experiment/greatspectations-v0.17.3` | 36 | 137 spec-quote comments pinned to v0.17.3 + the NUT-11 duplicate-tags fix (spec violation #1). | KEEP (archive) |
| `spectate/main` | 2 | spectate instrumentation: bind NUT-10/11/14 requirements at verification sites. | KEEP (audit infra) |
| `add-ffi-verification-workflows` | 13 | Jan-2026 python/flutter FFI verification CI. Upstream now ships binding CI; kept conservatively. | KEEP (weak — delete-after-reference candidate) |
| `main` | 0 | now = upstream head (5e1cba0c). | KEEP — aligned |

## Fork-level notes

1. **Default branch is `experimentalaislop`** — the OpenWrt mint carrier.
   Functional but misleading; rename recommendation above.
2. `merge-upstream` API is blocked for this token (no `workflow` scope —
   upstream's workflow-file changes) — the SSH route handled the ff; use
   SSH for future syncs of forks whose upstream touches workflows.
3. The fork's live purpose is now legible: **(a)** carry OpenWrt packaging
   for cdk-mintd-wrt, **(b)** staged upstream fixes (11003 identity, LND
   keepalive), **(c)** registry experiment arms, **(d)** arch/MIPS lanes.
   Everything else was archaeology and is now either deleted (with SHAs
   recorded above) or explicitly archived.
