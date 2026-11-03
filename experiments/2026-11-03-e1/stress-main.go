// cdk-go FFI lifetime stress — cashubtc/cdk#2788 follow-up, campaign
// "cdk-wallet-backend-evaluation-2026-11", Experiment 1 (x86_64 slice).
//
// Drives the ingredient the earlier single-threaded retest lacked:
// concurrent create/use/destroy cycles — overlapping async FFI work with
// interleaved foreign-object releases, forced GC, and resource telemetry.
// Run against different cdk-go checkouts by editing the replace directive:
//   /tmp/opencode/cdkgo      (upstream v0.18.3 tag)
//   /tmp/opencode/cdkgo-0182 (upstream v0.18.2 tag — negative control)
//
// PASS: no panic, no EOF, RSS/FDs/threads stable after warm-up.
// FAIL: any tokio drop panic, any EOF-poisoned call, or monotonic
// resource growth across the run.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cdk "github.com/cashubtc/cdk-go/bindings/cdkffi"
)

const (
	mint       = "https://testnut.cashu.space"
	cycles     = 120
	workers    = 4
	telemEvery = 20
)

var (
	errCount  atomic.Int64
	doneCount atomic.Int64
)

func procField(name string) string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				return fields[1]
			}
		}
	}
	return "?"
}

func fdCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

func telemetry(tag string) {
	fmt.Printf("[telemetry %s] rss=%skB threads=%s fds=%d goroutines=%d done=%d errs=%d\n",
		tag, procField("VmRSS"), procField("Threads"), fdCount(), runtime.NumGoroutine(), doneCount.Load(), errCount.Load())
}

func oneCycle(i int) error {
	dir, err := os.MkdirTemp("", fmt.Sprintf("cdkgo-stress-%d-*", i))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	m1, err := cdk.GenerateMnemonic()
	if err != nil {
		return err
	}
	w1, err := cdk.NewWallet(mint, cdk.CurrencyUnitSat{}, m1,
		cdk.WalletStoreSqlite{Path: filepath.Join(dir, "w1.db")}, cdk.WalletConfig{})
	if err != nil {
		return fmt.Errorf("w1 construct: %w", err)
	}

	q, err := w1.MintQuote(cdk.PaymentMethodBolt11{}, &cdk.Amount{Value: 21}, nil, nil)
	if err != nil {
		w1.Destroy()
		return fmt.Errorf("quote: %w", err)
	}
	paid := false
	for attempt := 0; attempt < 20; attempt++ {
		q, err = w1.CheckMintQuote(q.Id)
		if err != nil {
			w1.Destroy()
			return fmt.Errorf("check: %w", err)
		}
		if q.State == cdk.QuoteStatePaid || q.State == cdk.QuoteStateIssued {
			paid = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !paid {
		w1.Destroy()
		return fmt.Errorf("quote never paid (state %v)", q.State)
	}

	if _, err := w1.Mint(q.Id, cdk.SplitTargetNone{}, nil); err != nil {
		w1.Destroy()
		return fmt.Errorf("mint: %w", err)
	}

	prepared, err := w1.PrepareSend(cdk.Amount{Value: 5},
		cdk.SendOptions{AmountSplitTarget: cdk.SplitTargetNone{}, SendKind: cdk.SendKindOnlineExact{}, P2pkLockedProofSendMode: cdk.P2pkLockedProofSendModeSwap})
	if err != nil {
		w1.Destroy()
		return fmt.Errorf("prepare: %w", err)
	}
	token, err := prepared.Confirm(nil)
	if err != nil {
		prepared.Destroy()
		w1.Destroy()
		return fmt.Errorf("confirm: %w", err)
	}
	prepared.Destroy()
	w1.Destroy()

	// Second wallet receives while other workers hold their own wallets
	// and async machinery — the poison window.
	m2, err := cdk.GenerateMnemonic()
	if err != nil {
		return err
	}
	w2, err := cdk.NewWallet(mint, cdk.CurrencyUnitSat{}, m2,
		cdk.WalletStoreSqlite{Path: filepath.Join(dir, "w2.db")}, cdk.WalletConfig{})
	if err != nil {
		return fmt.Errorf("w2 construct (poison point): %w", err)
	}
	amt, err := w2.Receive(token, cdk.ReceiveOptions{AmountSplitTarget: cdk.SplitTargetNone{}})
	w2.Destroy()
	if err != nil {
		return fmt.Errorf("second receive (poison point): %w", err)
	}
	if amt.Value == 0 {
		return fmt.Errorf("received 0 sats")
	}
	return nil
}

func worker(id int, wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 0; i < cycles/workers; i++ {
		idx := int(doneCount.Add(1))
		if err := oneCycle(idx); err != nil {
			errCount.Add(1)
			fmt.Fprintf(os.Stderr, "[worker %d cycle %d] ERROR: %v\n", id, idx, err)
			if strings.Contains(err.Error(), "EOF") {
				fmt.Fprintf(os.Stderr, "[worker %d] EOF poison detected — aborting worker\n", id)
				return
			}
		}
		if idx%telemEvery == 0 {
			telemetry(fmt.Sprintf("cycle %d", idx))
		}
		runtime.GC()
		if idx%(telemEvery*2) == 0 {
			debug.FreeOSMemory()
		}
	}
}

func main() {
	fmt.Printf("cdk-go FFI lifetime stress: %d cycles / %d workers against %s\n", cycles, workers, mint)
	telemetry("start")
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(i, &wg)
	}
	wg.Wait()
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(2 * time.Second)
	telemetry("end")

	if errCount.Load() > 0 {
		fmt.Printf("STRESS FAIL: %d error(s)\n", errCount.Load())
		os.Exit(1)
	}
	fmt.Println("STRESS PASS: no panics, no EOF, all cycles completed")
}
