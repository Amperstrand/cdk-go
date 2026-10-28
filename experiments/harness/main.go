// harness — H1 evidence: the cdk wallet IN-PROCESS via CGO, statically
// linked against the Amperstrand/cdk experimental/arch staticlib.
//
// Runs a full wallet cycle against a live mint (the omarchy-cashu dev
// fakewallet by default): mnemonic → sqlite wallet → mint quote →
// settle → mint → balance → send → token decode. Prints PASS/FAIL per
// step; exit 0 only if everything held.
//
// Build (static musl):
//   cd bindings/cdkffi && ln -s ../../experiments/harness  # or run from module root
//   CGO_ENABLED=1 CC=musl-gcc go build -tags cdk_static -o /tmp/harness ./experiments/harness
package main

import (
	"fmt"
	"os"
	"time"

	cdk "github.com/cashubtc/cdk-go/bindings/cdkffi"
)

func fatal(step string, err error) {
	fmt.Printf("FAIL %s: %v\n", step, err)
	os.Exit(1)
}

func main() {
	mint := os.Getenv("MINT_URL")
	if mint == "" {
		mint = "http://192.168.13.208:3339"
	}
	dir, _ := os.MkdirTemp("", "harness-*")
	defer os.RemoveAll(dir)

	mnemonic, err := cdk.GenerateMnemonic()
	if err != nil {
		fatal("mnemonic", err)
	}
	fmt.Printf("PASS mnemonic (%d words)\n", len(split(mnemonic)))

	// WalletStore is a uniffi enum: pass WalletStoreSqlite{Path} (the
	// store is created INSIDE Rust), not a Go-side database handle.
	store := cdk.WalletStoreSqlite{Path: dir + "/w.sqlite"}
	w, err := cdk.NewWallet(mint, cdk.CurrencyUnitSat{}, mnemonic, store, cdk.WalletConfig{})
	if err != nil {
		fatal("wallet", err)
	}

	amt := cdk.Amount{Value: 1000}
	q, err := w.MintQuote(cdk.PaymentMethodBolt11{}, &amt, nil, nil)
	if err != nil {
		fatal("mint quote", err)
	}
	fmt.Printf("PASS mint quote %s\n", q.Id)

	// fakewallet settles instantly; poll to PAID.
	paid := false
	for i := 0; i < 40; i++ {
		st, err := w.CheckMintQuoteStatus(q.Id)
		if err == nil && st.State == cdk.QuoteStatePaid {
			paid = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !paid {
		fatal("quote settle", fmt.Errorf("never PAID"))
	}
	fmt.Println("PASS quote settled")

	proofs, err := w.Mint(q.Id, cdk.SplitTargetNone{}, nil)
	if err != nil {
		fatal("mint", err)
	}
	fmt.Printf("PASS minted %d proofs\n", len(proofs))

	bal, err := w.TotalBalance()
	if err != nil {
		fatal("balance", err)
	}
	if bal.Value != 1000 {
		fatal("balance", fmt.Errorf("want 1000, got %d", bal.Value))
	}
	fmt.Println("PASS balance 1000")

	prepared, err := w.PrepareSend(cdk.Amount{Value: 400}, cdk.SendOptions{
		AmountSplitTarget:      cdk.SplitTargetNone{},
		SendKind:               cdk.SendKindOnlineExact{},
		P2pkLockedProofSendMode: cdk.P2pkLockedProofSendModeSwap,
	})
	if err != nil {
		fatal("prepare send", err)
	}
	tok, err := prepared.Confirm(nil)
	if err != nil {
		fatal("confirm send", err)
	}
	enc := tok.Encode()
	fmt.Printf("PASS send → token (%d chars)\n", len(enc))

	dt, err := cdk.TokenDecode(enc)
	if err != nil {
		fatal("token decode", err)
	}
	v, err := dt.Value()
	if err != nil || v.Value != 400 {
		fatal("token value", fmt.Errorf("value=%v err=%v", v, err))
	}
	fmt.Println("PASS token decode 400")
	fmt.Println("HARNESS: ALL PASS")
}

func split(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
