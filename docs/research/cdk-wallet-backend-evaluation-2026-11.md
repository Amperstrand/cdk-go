# CDK-Go wallet backend evaluation — 2026-11 (campaign decision record)

Status: **IN PROGRESS** — this record carries session 1 (2026-11-03):
Experiment 0 inventory, Experiment 1 differential (x86_64), Experiment 2
slice (x86_64 + fork native artifacts), initial branch classification.
Cross-arch builds, fault injection, physical deployment: NOT TESTED —
see the next-action queue. Campaign brief:
`/tmp/opencode-cdk-go-wallet-architecture-evaluation.md` (mirrored here as
`experiments/2026-11-03-e1/campaign-brief.md`).

## Verified state of the world (2026-11-03)

- `Amperstrand/cdk-go` `main` is **identical to `cashubtc/cdk-go` main**
  (compare: ahead 0, behind 0). Upstream latest release v0.18.3
  (2026-10-29). Upstream `bindings/cdkffi/CDK_COMMIT` stamp (`34882e8a`,
  an April 2026 cdk commit) is **stale on every branch** (v0.17.3,
  v0.18.3, nightly, main all carry it) while native checksums DO change
  per release — stamp ≠ build truth; do not use it for fix provenance.
- tollgate-module PRs/issues: #1136 OPEN (WalletPort parity + cdk-go
  0.18.2 in-process, branch-scoped replace → `Amperstrand/cdk-go/mips-ffi`),
  #1137 OPEN (sidecar parity lane), #1155 OPEN (wallet.backend wiring),
  #1139/#1157 OPEN (backend-selection bypass / degraded-mode split-brain),
  **#1149 OPEN (retire the fork replace — fed by this session, below)**,
  #1161 CLOSED (MIPS FFI 101-stub + QEMU parity history).
- `Amperstrand/cdk-walletd`: main + `feat/conflict-sentinel-caller`;
  issue #1 (stale melt-saga reconciliation — recovery gate open), issue #2
  (Actions billing-blocked; prior stripped sizes 19.1 MB x86_64 / 16.4 MB
  aarch64 recorded there, not re-measured here).
- No prior OpenCode/Hermes sessions found on these lanes in this
  environment's store (searched: backend/sidecar/FFI/mips/leakfix) — the
  GitHub threads above are the cross-agent record.

## Experiment 1 — FFI runtime lifetime, differential (x86_64, host)

Driver: `experiments/2026-11-03-e1/stress-main.go` — 120 cycles ×
(2 wallets + prepared-send object): NewWallet → MintQuote → FakeWallet
auto-pay → Mint → PrepareSend/Confirm/prepared.Destroy → W1.Destroy →
W2.Receive → W2.Destroy; 4 concurrent workers; forced GC (+FreeOSMemory);
telemetry RSS/threads/FDs. Mint: `testnut.cashu.space` (cdk-mintd 0.18.1
FakeWallet). Generated test mnemonics, disposable sqlite, no real funds.

| Variant | Go-level result | Rust-level result | Resources |
|---|---|---|---|
| upstream **v0.18.2** (negative control) | 120/120 cycles, exit 0, no Go errors | **240 × tokio panic** `Cannot drop a runtime in a context where blocking is not allowed` (`async-compat/tokio-1`, tokio-1.53.2 shutdown.rs:51 — the exact cdk#2788 signature) | RSS plateau ~40 MB |
| upstream **v0.18.3** | 120/120 cycles, exit 0 | **zero panics** | RSS 8→37 MB warm-up then flat (±1.5 MB); threads 81 under load → **18 after destroys** (runtimes released, not leaked); FDs 36 → 9 |

**Verdict:** the concurrency shape is the reproduction ingredient the
earlier single-threaded probes lacked; v0.18.3 eliminates the panic class
AND is resource-correct under this workload (thread decay proves
`shutdown_background` actually releases, vs a leak's monotonic growth).
Honesty notes: (a) on 0.18.2 the panics did NOT cascade to the EOF process
poisoning in this workload — the reported EOF symptom requires a release
racing an in-flight call on the same runtime; the owner's original
acceptance suite remains the gate for that stronger symptom; (b) x86_64
host only — aarch64/armv7/MIPS NOT TESTED; (c) melt/saga paths not
stressed here (cdk-walletd#1 class untouched).

## Experiment 2 — footprint slice (x86_64 + fork artifacts)

Apples-to-apples caveat: gonuts baseline is `CGO_ENABLED=0` static Go;
cdk-go lane is the upstream prebuilt dynamic `.so`. Same host, `go build`
defaults, `go 1.25.8` host toolchain. Installed footprint = what must sit
on flash for a router.

| Lane | Installed artifacts (x86_64) | Idle/load RSS |
|---|---|---|
| (A) gonuts in-process | **12.0 MB** single static binary | 17.6 MB peak (LoadWallet+Shutdown) |
| (B) cdk-go in-process, upstream dynamic | 4.5 MB Go binary + **29.0 MB** `libcdk_ffi.so` (already stripped) = **~33.5 MB** | ~40 MB under 4-way load |
| (E) cdk-walletd sidecar | per its issue #2: 19.1 MB stripped (x86_64), 16.4 MB (aarch64) — not re-measured | not re-measured |
| (C/D) static/musl, MIPS FFI | NOT TESTED (build lane not run) | — |

Fork `mips-ffi` native artifact sizes (from the branch, not built here):
`linux_mips` 33.5 MB, `linux_mipsel` 33.5 MB, `linux_arm64` 26.6 MB —
**larger than upstream x86_64** (i.e. not size-minimized). Immediate
implication: **the in-process dynamic lane is dead on the 16 MB flash
class regardless of correctness** (33.5 MB .so + Go binary vs 16 MB total
flash); MIPS candidates are sidecar or a genuinely size-optimized static
build. The July-2026 MIPS size estimates and October-2026 optimized
linker measurements compare different artifact shapes — not reconciled
here; do not mix them.

## Branch classification — initial (no deletions this campaign)

| Branch | Initial class | Basis / gate |
|---|---|---|
| `main` | keep (mirror of upstream) | identical, 0/0 |
| `ffi-runtime-leakfix` | RETIRE AFTER UPSTREAM UPGRADE | its workaround is superseded by cdk#2581 (shipped 0.18.3); E1 differential is the first gate — passed on x86_64; remaining gates: cross-arch + owner's acceptance-suite rerun + #1149 consumer sweep |
| `mips-ffi` | KEEP EXPERIMENT | referenced by PR #1136 replace; 101 abort stubs unaudited; 33.5 MB artifacts unusable for 16 MB flash as-is; MIPS decision lane open |
| `experimental/arch` | KEEP EXPERIMENT | `shim_open64.c` UB questions open (brief §25); no re-audit this session |
| `0.16.0-release`, `0.16.0-release-2`, `feat/0.16.0`, `release/v0.17.2-rc.0`, `fix/0.16-supabase-build` | DELETE AFTER REFERENCE CHECK | historical release/binding-era branches; `0.16.0-release-2` has no commits ahead of main (verified earlier review); consumer sweep pending |
| `automation/update-bindings`, `feat/go-ffi-release`, `feat/go-ffi-release-clean` | EXTRACT / UPSTREAM or DELETE | binding-generation workflow era; check whether upstream CI now owns this before classifying final |

## Per-tier snapshot (what can and cannot be claimed today)

- **x86_64/aarch64 big-RAM tiers:** in-process cdk-go **v0.18.3** is
  technically viable (E1 pass) at ~33.5 MB installed + ~40 MB RSS —
  decision vs sidecar is now an operational tradeoff, not a correctness
  blocker. GO (conditional on owner-suite rerun).
- **armv7:** NOT TESTED.
- **16 MB MIPS/MIPSel:** dynamic in-process **NO-GO on footprint** (33.5
  MB .so). Open candidates: sidecar, size-optimized static — both unmeasured
  on-target. NOT-YET.

## Next-action queue (highest leverage first)

1. **Owner's #2788 acceptance-suite rerun against v0.18.3 prebuilts**
   (the EOF-symptom gate + the eventual thread reply; evidence staged in
   cashu-audit-private#1).
2. **#1149 fork-replace retirement slice:** bump tollgate-module cdk-go
   pin v0.17.3 → v0.18.3 on an isolated branch, run the #1136 parity
   contract, retire the mips-ffi replace on big-RAM targets only.
3. **MIPS lane decision experiment:** size-optimized static musl build
   (vs the 33.5 MB dynamic artifacts) — smallest experiment that can flip
   the 16 MB tier; else sidecar-only for MIPS.
4. **Sidecar readiness:** cdk-walletd issue #1 (stale melt saga) blocks
   its recovery gate; #1137 mutation lane proves the suite catches it.
5. Cross-arch (aarch64 QEMU/physical via PRTA) rerun of E1.

## Evidence

`experiments/2026-11-03-e1/`: `stress-main.go` (driver),
`run-v0.18.2.log` (240 panics), `run-v0.18.3.log` (clean),
`footprint.txt` (commands + numbers), this session's host/toolchain env.

## Session 2 (2026-11-03, later) — queue items executed

**E1 arm64 differential (QEMU user-mode, upstream prebuilt arm64 `.so`):**
same driver cross-built (`CC=aarch64-linux-gnu-gcc`, CGO arm64), run
under `qemu-aarch64-static` — **v0.18.2: 240 panics / v0.18.3: 0**,
identical to x86_64. Correctness verdict valid under emulation; RSS
(~274 MB) is QEMU translation overhead — NOT a physical number, do not
cite it. Physical aarch64 still NOT TESTED.

**#1149 retirement slice (big-RAM lane): DONE, evidence-backed.**
Branch `OpenTollGate/tollgate-module-basic-go@research/upstream-cdkgo-0183-slice`
(`9ffccbe8`, off PR #1136 head): single fork reference (the
`Amperstrand/cdk-go` replace) removed, upstream `cashubtc/cdk-go v0.18.3`
pinned, no adapter API drift (build clean with `cdk_wallet` tag). Results
against `testnut.cashu.space` (cdk-mintd 0.18.1 fakewallet):
- **WalletPort parity contract 5/5** (MintLifecycle, SendReceiveRoundTrip
  incl. helper leg, MeltLifecycle, Drain, DecodeRejectsGarbage;
  RecipientHelper self-skips by design as the launched-helper leg);
- full `src/tollwallet` suite green (`go test -tags cdk_wallet`, 28 s).
Note: `Amperstrand/tollgate-module-basic-go` fork is **archived** —
research branches for this repo go to `origin` (OpenTollGate) under the
`.pr-branch` marker protocol.

**Queue status:** EOF-cascade probe (release racing an in-flight call)
designed but not run this session — owner's #2788 acceptance suite
remains that gate. MIPS size-optimized static musl build: **BLOCKED by
toolchain** on this host (no mips musl cross-gcc; docker+`cross` route
available as follow-up). MIPS QEMU correctness run against the fork's
33.5 MB artifacts deliberately skipped — footprint already NO-GO for the
16 MB class.

**Updated per-tier verdicts:** x86_64 + aarch64 (emulated) in-process
upstream v0.18.3: **GO-pending-owner-suite** (correctness now doubly
differential; ops decision vs sidecar is a footprint/containment
tradeoff). armv7: NOT TESTED. 16 MB MIPS/MIPSel: NOT-YET (sidecar vs
size-optimized static undecided; item 3 blocked).
