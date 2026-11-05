package txbuilder

// UTXO is an unspent transaction output that a generated transaction can spend.
//
// The field names are part of the persisted BoltDB (gob) state format:
// renaming one makes existing state files unreadable.
type UTXO struct {
	// TxHash is the hex txid of the transaction that created the output.
	TxHash string

	// TxPos is the output index (vout) within that transaction.
	TxPos uint32

	// Value is the output amount in satoshis.
	Value uint64

	// Height is the block height that confirmed the output, or 0 while it is
	// unconfirmed.
	Height int64
}
