package txbuilder

import "errors"

var (
	// ErrInstanceIDRequired is returned when the tagged-drop lock script has
	// no INSTANCE_ID to embed.
	ErrInstanceIDRequired = errors.New("INSTANCE_ID is required for the tagged-drop lock script")

	// ErrPrivateKeyRequired is returned when P2PKH mode is requested without
	// a PRIVATE_KEY.
	ErrPrivateKeyRequired = errors.New("PRIVATE_KEY is required for P2PKH mode")

	// ErrInvalidPrivateKey is returned when PRIVATE_KEY is neither a WIF nor
	// a 32-byte hex private key.
	ErrInvalidPrivateKey = errors.New("PRIVATE_KEY must be a valid WIF or 32-byte hex private key")

	// ErrNilMode is returned when a transaction is built without a Mode.
	ErrNilMode = errors.New("transaction mode is required")

	// ErrInvalidFanoutCount is returned when a fanout asks for fewer than one
	// output.
	ErrInvalidFanoutCount = errors.New("fanout count must be positive")

	// ErrTxTooLarge is returned when a fanout transaction would exceed
	// MaxTxBytes.
	ErrTxTooLarge = errors.New("transaction exceeds the maximum size")

	// ErrInsufficientValue is returned when a UTXO cannot pay the fanout fee
	// and still fund every output with at least one satoshi.
	ErrInsufficientValue = errors.New("UTXO value too small")

	// ErrZeroValueUTXO is returned when asked to spend a zero-value UTXO.
	ErrZeroValueUTXO = errors.New("cannot spend a zero-value UTXO")

	// ErrInvalidTxID is returned when a UTXO's transaction hash is not a
	// valid 32-byte hex txid.
	ErrInvalidTxID = errors.New("invalid txid")
)
