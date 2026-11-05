package txbuilder_test

import (
	"fmt"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// ExampleBuildSustain shows one hop of a chain: each sustain transaction
// spends the previous output and pays the mode's fee.
func ExampleBuildSustain() {
	mode, err := txbuilder.NewTaggedDropMode("runner", 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	utxo := txbuilder.UTXO{
		TxHash: "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0",
		Value:  1_000,
	}

	_, next, err := txbuilder.BuildSustain(utxo, mode)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(next.Value)
	// Output: 993
}

// ExampleBuildFanout shows a bootstrap fanout splitting one output four ways.
func ExampleBuildFanout() {
	mode, err := txbuilder.NewTaggedDropMode("runner", 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	utxo := txbuilder.UTXO{
		TxHash: "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0",
		Value:  10_000,
	}

	_, outputs, err := txbuilder.BuildFanout(utxo, 4, mode)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(len(outputs), outputs[0].Value)
	// Output: 4 2497
}
