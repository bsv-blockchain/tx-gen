package txbuilder

import "testing"

func BenchmarkBuildSustainTaggedDrop(b *testing.B) {
	mode := taggedDropMode(b)
	utxo := UTXO{TxHash: testTxID, Value: 1_000}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := BuildSustain(utxo, mode); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildSustainP2PKH(b *testing.B) {
	mode := p2pkhMode(b)
	utxo := UTXO{TxHash: testTxID, Value: 1_000}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := BuildSustain(utxo, mode); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildFanout100(b *testing.B) {
	mode := taggedDropMode(b)
	utxo := UTXO{TxHash: testTxID, Value: 10_000_000}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := BuildFanout(utxo, 100, mode); err != nil {
			b.Fatal(err)
		}
	}
}
