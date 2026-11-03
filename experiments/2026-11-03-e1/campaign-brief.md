# OpenCode mission: CDK-Go fork rationalization and TollGate wallet architecture experiments

You are the technical lead for a research-and-experiment campaign, not a release agent. Work autonomously with read-only inventory first, then isolated, reproducible experiments. Your ultimate objective is to recommend the least burdensome, safest architecture that supports the actual TollGate/OpenWrt hardware fleet, and determine which `Amperstrand/cdk-go` branches can be retired. Do not assume any candidate wins.

## Immediate objective and decision questions

Determine, using reproducible evidence:

1. Can the production TollGate Go service use **unmodified upstream `cashubtc/cdk-go` v0.18.3 or newer** without our `ffi-runtime-leakfix`? Has the Tokio `RuntimeGuard` defect truly been fixed under repeated wallet creation/destruction, repeated receives, and real crash/recovery conditions?
2. What is the **actual incremental installed footprint**, RAM, CPU, operational and maintenance cost of (A) gonuts in-process, (B) CDK-Go in-process with upstream dynamic FFI, (C) CDK-Go in-process with native static/musl libraries, (D) CDK-Go MIPS experimental FFI, and (E) `cdk-walletd` Rust sidecar? Treat native Rust TollGate as a strategic comparison if an existing build is available; treat `micronuts-walletd` as an experimental control, not an equivalent safe alternative until its recovery cases pass.
3. Which option is viable on **actual** OpenWrt `x86_64`, `aarch64`, `armv7`, `mips_24kc`, `mipsel_24kc` targets, particularly the 16 MB flash class? No marketing or theoretical size claim counts as a measurement.
4. How can we reduce fork divergence, build-system complexity, and duplicate maintenance? Can `mips-ffi` be replaced by a feature-correct generated FFI surface instead of hardcoded checksums and abort stubs? Can `experimental/arch` be replaced by upstream builds or a small, reusable cross-build recipe?
5. What must be done to migrate and **retire every unneeded `Amperstrand/cdk-go` branch**, without breaking downstream branches, CI, tagged releases, Go module pinning, or future reproducibility?

### Work context: verify all of this again before relying on it

- `Amperstrand/cdk-go`: 12 known branches as of 2026-11-03: `main`, `0.16.0-release`, `0.16.0-release-2`, `automation/update-bindings`, `experimental/arch`, `feat/go-ffi-release`, `feat/go-ffi-release-clean`, `feat/0.16.0`, `ffi-runtime-leakfix`, `fix/0.16-supabase-build`, `mips-ffi`, `release/v0.17.2-rc.0`. `main` matched upstream `cashubtc/cdk-go` on review; upstream latest release was v0.18.3 (2026-10-29). `0.16.0-release-2` had no commits ahead of main. DO NOT infer all other branches are useful just because they are divergent.
- `cashubtc/cdk` issue #2788 and PR #2731: upstream says `cdk-ffi` 0.18.3 includes `RuntimeGuard::drop` handling. The fork's 0.18.2 leak workaround uses `mem::forget`; upstream uses shutdown-background inside a runtime. Confirm BOTH panic absence and resource/lifetime correctness; Tokio's `shutdown_background` has different tradeoffs from deliberately leaking the runtime.
- `OpenTollGate/tollgate-module-basic-go` PR #1136: WalletPort parity contract, in-process FFI, historical 0.18.2 runtime bug, and a branch-scoped Go `replace` to `Amperstrand/cdk-go/mips-ffi`. PR #1137: sidecar parity/fault-injection lane. PR #1155: merchant `wallet.backend` wiring (OPEN at research time). Issues #1139 and #1157: backend selection bypass and degraded-mode split-brain wallet risks. Issue #1149: retire fork replace. Issue #1161: MIPS FFI QEMU parity history.
- `Amperstrand/cdk-walletd`: pure-Rust AF_UNIX JSON sidecar with recovery gate and procd packaging. Read `docs/backend-options-analysis.md`, `README.md`, `PIN.md`, `src/main.rs`, `scripts/arch-build-mipsel.sh`, `scripts/make-apk.sh`. The older size analysis includes hypotheses and size-optimized builds; a separate issue #2 documents 19.1 MB x86_64 and 16.4 MB aarch64 stripped **other** builds and Actions billing blockage. Do not mix profiles or imply CI ran if it did not. Issue #1 records stale melt-saga reconciliation.
- `OpenTollGate/tollgate-module-basic-go/docs/architecture/wallet-measurement-protocol.md` defines a canonical measurement methodology and blocker matrix. `docs/architecture/walletport-contract.md` defines the interface and funds-safety requirements. Read `AGENTS.md` first.
- `OpenTollGate/physical-router-test-automation` (PRTA) owns router/virtual test venues; `Amperstrand/conwrt-bench` owns bench inventory and labgrid logistics; `FreedomTechFeed/packages` owns shipping package recipes. Do not create competing test harnesses/packaging authorities.
- `Amperstrand/cdk` branch `experimental/arch` owns `scripts/arch-build.sh`, while `Amperstrand/cdk-go/experimental/arch` adds `cdk_static` build tags, `shim_open64.c`, and `experiments/harness/main.go`.
- `Amperstrand/cdk-go/mips-ffi` has 32-bit big/little-endian softfloat libraries. It stubs ~101 unsupported UniFFI symbols with hardcoded checksums and `abort()` functions because its native build excludes Nostr/NWC/npubcash. Existing QEMU parity was reported as 5/5 but does not imply all public FFI APIs are safe. Audit every exported API and its required link/runtime symbols.
- In `experimental/arch`, `shim_open64.c` unconditionally reads a vararg from `open64`, and treats `fcntl64` varargs as a pointer. Audit for undefined behavior, ABI correctness, and whether shims are still necessary with newer musl/SQLite/compile flags. Do not propagate these shims blindly.
- PRTA sidecar parity docs report that `micronuts-walletd` lost value on interrupted melt and receive paths (cases 20/30). Do not promote it until crash-safe durable reconciliation is demonstrated.

## Agent coordination and project ownership (do first)

1. Inspect all relevant `AGENTS.md` / repo-specific policy, `git status`, `git branch -vv`, `git worktree list`, active PRs, issue discussions, any handoff notes and any discoverable OpenCode/Hermes sessions. Other agents may be working on the same issues. Read their findings and request their opinions through supported communication mechanisms if available. Never overwrite, rebase, delete, or modify their worktrees. Prefer reusing existing work and assigning a clear lane per repo.
2. Establish an immutable **baseline snapshot**: upstream/fork commit SHA, all branch heads, Go module pins and `replace` directives in **every** module/worktree, CDK Rust SHA/crate version, current open PR states, CI availability, build-tool versions, hardware inventory, available labgrid places, and host/toolchain capabilities.
3. Check that the canonical repositories own proposed changes. Use a fresh, namespaced worktree/branch for experimental code and scripts. Keep the release and production worktrees untouched. No automatic merging, branch deletion, upstream issue/PR posting, factory resets, sysupgrades, or flashing. Never use real Cashu funds or production wallets: isolated fakewallet/test mint, generated test-only mnemonic, disposable storage.
4. Use labgrid place acquisition and release for physical resources. Never power-cycle a device subject to a no-power-cycle contract. Do not disturb other sessions or production networks. If hardware is unavailable, finish emulated/build-side evidence and label physical outcomes `NOT TESTED`.
5. If GitHub Actions are billing-blocked, run canonical local gates and record CI as blocked, not green. Avoid paid/cloud providers unless already explicitly authorized for this campaign.

## Experiment 0: audit existing work to avoid redundant research

- Reproduce the status of PRs #1136, #1137, #1155 and issues #1139, #1149, #1157, #1161. Inspect branch-only code and tests; distinguish merged from draft and historical from current. Identify which tests already exist in `src/tollwallet`, `tests/cloud-lab/parity`, PRTA, and `cdk-walletd`.
- Enumerate every `cdk-go` fork branch with creation reason, unique commits, patch-equivalence to *current upstream*, downstream references, native artifact differences, build scripts, target architectures, and hazards. Group obsolete release/binding-generation work separately from platform-specific patches. Search tags, Go dependency graphs and `.mod` pin replacement in active downstream PRs; an upstream update does not remove experimental branch references automatically.
- Write a short **state of the world** before modifying code: confirmed facts vs stale documentation vs unknowns. Correct the July 2026 estimated MIPS FFI size vs October 2026 optimized linker/package measurements: they are different artifact shapes, not directly comparable.

## Experiment 1: reproduce runtime lifetime correctness on real matching artifacts

Compare at least:
- upstream `cashubtc/cdk-go` 0.18.2 (negative control, if reproducible);
- fork 0.18.2 `ffi-runtime-leakfix` (positive control, only if historically relevant);
- upstream `cashubtc/cdk-go` v0.18.3 (preferred replacement; pin its actual SHA and associated native library hashes).

Run the SAME isolated tests, generated with a fake mint, across variants. Include wallet creation, multiple wallet objects, concurrent and sequential Receive, the **second receive**, repeated `Destroy()` and token `Close()` in both ordinary Go and async/FFI completion contexts, forced GC, socket/network failures, and hundreds of create/use/destroy iterations. Record process exit code, panic/backtrace, errors vs `EOF`, resident/PSS, Go/Rust thread count, open FDs, SQLite handles, and trends after warm-up. Include ASan/UBSan/Valgrind where feasible; do not claim sanitizers tested if incompatible. Stress `shutdown_background` vs leak behavior and inspect sqlx cleanup/saga durability.

Do not confuse the FFI API generated from 0.18.2 with 0.18.3 binaries; regenerate matching bindings and verify UniFFI symbol/checksum and CDK source identity. If upstream 0.18.3 works safely, prove parity and remove fork `replace` in an isolated downstream branch; do not merge the migration or delete the old branch. If it fails, produce a minimized reproducer with precise hashes/backtrace and proposed fix, not an unsupported assertion that upstream failed.

## Experiment 2: build and packaging matrix — measure installed reality

Test the relevant candidates, as available:

A. Existing gonuts-only Go service (`CGO_ENABLED=0`) — baseline.
B. Go + unmodified upstream CDK-Go 0.18.3, upstream prebuilt `.so` on supported targets.
C. Go + `cdk_static`/musl build, first x86_64 and aarch64, then ARMv7 if practical.
D. Go + MIPS/MIPSel experimental FFI, with explicit feature-minimized Rust library; compare dynamic `.so` and static linking where possible.
E. Go + `cdk-walletd` process-isolated Rust sidecar, on matching targets and CDK pin.
F. Existing native Rust TollGate artifact (optional strategic reference), but do not spend the campaign rewriting TollGate.
G. `micronuts-walletd` only as a *negative-control/experimental* comparison with known saga failures; never include it in shipping recommendations until recovery tests pass.

Architectures: `linux/amd64`, `aarch64`, `armv7`, MIPS32 big-endian softfloat `mips_24kc`, MIPS32 little-endian softfloat `mipsel_24kc`. Verify actual OpenWrt SDK target, ABI, float ABI, musl version and compiler flags. Use the correct native toolchain and `-Z build-std` as needed; separate host building from target execution. Verify ISA/ABI (`file`, `readelf`, native linker, `ldd` where safe) and dynamic loader/RPATH on the target. Preserve proper `go.mod` import paths, module provenance and linker reproducibility. Run `cgo` tests on target or emulated runtime; cross compilation alone is not a PASS.

**Measure apples-to-apples**: same exact CDK feature set and comparable wallet functionality; same optimization/strip/profile, same Go code, same OpenWrt image and compiler versions. Report feature differences explicitly when unavoidable. Record full stripped executable + shared libs + dynamic loaders + runtime dependencies + service metadata; static `.a` archives are NOT installed footprint. Record `.apk`/`.ipk` package compressed size, actual installed files, squashfs/rootfs size delta, overlay free space, persistence location, writable/erase-block overhead, and install/upgrade/rollback headroom on **real** 16 MB, 32 MB, 128/256 MB hardware classes where available. Avoid assuming nominal flash is writable overlay. Verify package manager (`apk` vs `opkg`) per actual OpenWrt release.

For runtime, collect RSS and PSS *per process and for the whole deployment*, peak/idle threads, file descriptors, CPU time, wallet-open/boot-ready time, hot/cold RPC latency p50/p95, throughput under limited concurrency, on-flash writes per money operation, and process recovery behavior. Never present host/QEMU timing as physical-router performance. Run enough iterations for meaningful distributions, note sample size and confidence limitations. Test cross-boot and intermittent WAN conditions. Use instrumented builds separately from release-size builds.

## Experiment 3: fix the MIPS and static-linking hazards at the design level

1. Understand the reason the MIPS Rust build excludes Nostr/NWC/npubcash (32-bit atomics, portable-atomic, dependency feature flags). Test whether a minimal **wallet-only** CDK FFI with a generated, accurately restricted API can compile/link without abort stubs. Prefer removing unsupported symbols from generated bindings/API over pretending they exist. Ensure ABI types and checksum constraints are real, not hardcoded to suppress initialization warnings. Negative-test unsupported functions: they must be uncallable at compile time or return explicit safe errors, not abort the payment process.
2. Review and, in experimental worktree only, replace/remove unsafe `open64`/`fcntl64` shim behavior. Enumerate actual unresolved symbols per toolchain, use the standard musl/SQLite feature macros or proper typed wrappers wherever feasible. Add compiled C tests and undefined-behavior checks where supported. Check 32-bit offset widths and LFS variants; never assume 64-bit aliasing applies to 32-bit MIPS.
3. Separate failures caused by UniFFI code generation from Rust cross-compilation, CGO static linking, and OpenWrt packaging. Identify small, reviewable patches to feed back to the correct upstream project (`cashubtc/cdk`, `cashubtc/cdk-go`, UniFFI bindgen, nostr-relay-pool) and artifacts that should stay in a local build script.
4. Confirm whether `cdk_static` is genuinely smaller *after final link* and whether it performs correctly on musl. Do not optimize by dropping fund-safety features or introducing implicit abort-on-panic in money-moving paths without recording that contract change.

## Experiment 4: wallet correctness, fault injection and full merchant integration

Reuse canonical tests, do not construct a parallel parity framework:
- PR #1136's five WalletPort scenarios: mint quote lifecycle, send/receive with spent check and double-receive, melt, drain, hostile decode.
- PR #1137's sidecar parity + mutation suite (`tests/cloud-lab/parity/run-parity.sh`, `--mutation`), cases 00, 10, 20, 30, 40, 50 and 60 as currently defined. Include NUT-07, NUT-09 restore, V3/V4 tokens, V2 keysets, DLEQ, P2PK/HTLC witnesses, duplicate output/counter monotonicity, seed restoration, canonical mint URL, quote ID/status preservation and serialization contract.
- Test failures: SIGKILL at each mint request/response boundary, dropped **accepted** responses, daemon death, Go-service death, simultaneous restarts, 429/5xx/timeout/DNS outage, partial writes/disk full, corrupt store, short-lived disconnected WAN, wrong machine time, low memory, malformed/hostile tokens, rollback and duplicate retries.
- Define invariant ledger: issued value + spendable value + provably consumed value + documented reserved/ambiguous amount; never conflate `pending` and `lost`, never resubmit an ambiguous money op without reconciliation. Fail closed when state cannot be established. Tests must distinguish spent/unspent/unknown. Mutation-testing must show the suite fails when safety guards are removed.
- Validate the full **running merchant**, not merely the `WalletPort`: `wallet.backend=cdk` through schema, startup, runtime wallet selection, degraded/boot fallback, WAN down/return, service restore, and end-to-end receive→session grant. Verify issues #1139/#1157 are resolved or fail the CDK production verdict. Never allow a degraded path to instantiate gonuts storage while CDK owns that mint/seed, or silent cross-backend fallback after funds move.
- For sidecar, verify `SO_PEERCRED`, socket ownership/mode, procd supervision, startup ordering, health check, protocol version negotiation, idempotency/reconciliation for ambiguous RPCs, correct seed provenance/permissions, storage durability and downgrade rollback. Use the same funds-safety rules on both FFI and sidecar.
- `micronuts-walletd` is disqualified as a safe backend until cases 20/30 no longer lose value. It may be used to prove the suite catches known-bad designs, not to weaken acceptance criteria.

## Experiment 5: supervised physical deployment and operational costs

Run the cheapest informative tests first: host correctness → QEMU builds and fault injection → selected physical OpenWrt router(s) using PRTA+labgrid place leases. Inspect PRTA inventory/current lease and conwrt-bench before selecting any device. Prefer available aarch64 and one actual MIPS/MIPSel target; report unavailable architectures as UNTESTED. Do not flash or reset a router without an explicit documented, authorized recovery plan; prefer reversible package installation or an ephemeral VM. Use isolated test mints/credentials only.

For each executable architecture, evaluate cold boot before WAN/NTP, daemon ordering, kill/restart, WAN loss, pending sagas, fs-specific storage (including JFFS2/shared-mmap differences), OpenWrt overlay space, log visibility, service upgrade, and state preservation. Make failed environment setup a visible TEST BLOCKED, not a silent skip or PASS. If a backend fits only via a special image composition, report the special constraints.

## Decision matrix and branch-retirement gate

Use hard blockers first: **no fund loss**, no silent split-brain, deterministic recovery/fail-closed ambiguity, correct ABI/feature exposure, boot/install viability on the claimed tier, genuine wallet parity and licensing, safe seed handling. If any blocker fails, candidate is NOT SHIPPABLE on that tier irrespective of size or speed. Then compare measurable weighted considerations: full deployed flash delta, peak PSS/RSS, startup/operation latency, crash blast radius, build repeatability, patch count, CI feasibility, maintenance burden, upgrade/rollback safety. Do not create fake precision or score UNMEASURED as a pass. It is valid to choose different defaults for 16 MB MIPS and 256 MB ARM, or to conclude no candidate is ready yet.

For all 11 non-main `Amperstrand/cdk-go` branches, classify one of:
- `DELETE AFTER REFERENCE CHECK` — merged/ancestor or obsolete generated artifacts;
- `RETIRE AFTER UPSTREAM UPGRADE` — 0.18.3+ correctly replaces workaround and tests pass;
- `EXTRACT / UPSTREAM` — one valuable patch should be ported first;
- `KEEP EXPERIMENT` — unique reproducible functionality still under comparative test;
- `BLOCKED` — still referenced or data needed to validate safety.

Explicitly name the consumer PR/module/replace, oldest and latest relevant commits, patch equivalence, test gate, and reversible sequence. **Do not delete branches in this campaign.** Prepare a concrete branch cleanup plan and minimal PR slices for eventual review. If the sidecar is preferred, say whether a thin upstream `cdk-go` checkout is still necessary for developer interoperability or only historically interesting.

## Deliverables (persist in the appropriate repository, ideally an isolated research branch)

1. A concise `docs/research/cdk-wallet-backend-evaluation-2026-11.md` decision record with: architecture diagram, verified repo/status snapshot, tested candidates, hard blockers, benefits, maintenance cost, per-tier recommendation, remaining unknowns, and branch-by-branch retirement table.
2. Machine-readable CSV/JSON results and complete evidence bundle for every executed test: `env.txt` (host, target, toolchain, exact SHA and commands), sha256 of every artifact, full stdout/stderr, `file/readelf/ldd`, package size, installed footprint, runtime samples, fault-injection ledger, explicit `PASS/FAIL/BLOCKED/UNTESTED` status, whether host/QEMU/physical, and reproduction commands. Never include tokens, wallet seeds, secrets, or credentials.
3. Reusable scripts or minimal PR(s) for the experiments actually performed, layered on existing authoritative harnesses. Avoid massive unrelated refactors or generated binary churn. Commit experimental results to a clearly named branch; no merge, release, deletions, destructive device operations or external upstream submissions without owner approval.
4. A prioritized next-action queue: **quick upstream-0.18.3 retirement**, **MIPS safe-FFI decision**, **sidecar physical-fit and full-merchant readiness**, then optional long-term Rust migration. Identify owners, prerequisites, and concise verification commands. Where other sessions are active, note their conclusions and avoid duplicate work.
5. At the end, report a one-page summary with exact links/SHAs, the three strongest experimental findings, recommendation per hardware tier, branches safe to retire *after* validation, unresolved blockers, and the single highest-value next experiment.

## Operating discipline

- Start by surveying and documenting; only then run tests. Do not assume historical claims are true: independently re-run the most decision-relevant experiments.
- Work in small falsifiable hypotheses; when one path is blocked, move to another and record the blocker. Prioritize evidence over activity. Never spend the session on giant rebuilds that cannot change the decision.
- Follow each repository's `AGENTS.md` and engineering-policy ownership model. Coordinate with parallel agents and active worktrees before touch. Never report that a workflow or router test passed if it never ran.
- Use only disposable fake funds, non-production storage and reversible infrastructure. Preserve existing data and devices.
- Keep the final result actionable, with reproducible commands and a clear GO / NO-GO / NOT-YET verdict for each tier. You are authorized to explore and commit experimental branches; you are NOT authorized to delete fork branches, merge to main, flash hardware, use real funds, or publish external upstream PRs.

Begin with Experiment 0 and immediately identify the smallest experiment most likely to overturn the current sidecar-versus-FFI decision.
