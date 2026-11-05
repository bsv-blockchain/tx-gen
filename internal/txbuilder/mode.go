package txbuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdktx "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

const (
	// ModeTaggedDrop locks outputs with the instance-tagged push/drop script
	// and spends them with OP_TRUE. It is the default mode.
	ModeTaggedDrop = "tagged_drop"

	// ModeP2PKH locks outputs to the PRIVATE_KEY address and signs every input.
	ModeP2PKH = "p2pkh"

	// DefaultSustainFee is the tagged-drop fee per sustain hop in satoshis:
	// ceil(69 bytes x 100 sat/KB) = 7.
	DefaultSustainFee uint64 = 7

	// DefaultP2PKHSustainFee is the P2PKH fee per sustain hop in satoshis.
	DefaultP2PKHSustainFee uint64 = 20
)

// Mode describes how generated transactions lock and unlock their outputs.
//
// A Mode is immutable once built; use WithSustainFee to derive a variant.
type Mode struct {
	// Name is ModeTaggedDrop or ModeP2PKH.
	Name string

	// LockScript locks every output this mode creates.
	LockScript *script.Script

	// Unlocker signs inputs in P2PKH mode. It is nil in tagged-drop mode,
	// which spends with a bare OP_TRUE.
	Unlocker sdktx.UnlockingScriptTemplate

	// SustainFee is the fee in satoshis paid by each sustain hop.
	SustainFee uint64

	// Address is the mainnet P2PKH address, empty in tagged-drop mode.
	Address string
}

// NewMode returns a P2PKH mode when privateKey is set, and a tagged-drop mode
// for instanceID otherwise. A zero sustainFee selects the mode's default fee.
func NewMode(privateKey, instanceID string, sustainFee uint64) (*Mode, error) {
	if strings.TrimSpace(privateKey) != "" {
		return NewP2PKHMode(privateKey, sustainFee)
	}
	return NewTaggedDropMode(instanceID, sustainFee)
}

// NewTaggedDropMode returns the default mode: outputs are locked with
// OP_PUSHBYTES_6 <six-byte instance tag> OP_DROP and spent with OP_TRUE. A zero
// sustainFee selects DefaultSustainFee.
func NewTaggedDropMode(instanceID string, sustainFee uint64) (*Mode, error) {
	lockScript, err := TaggedDropLockScript(instanceID)
	if err != nil {
		return nil, err
	}
	if sustainFee == 0 {
		sustainFee = DefaultSustainFee
	}
	return &Mode{
		Name:       ModeTaggedDrop,
		LockScript: lockScript,
		SustainFee: sustainFee,
	}, nil
}

// NewP2PKHMode returns a mode that locks outputs to the mainnet address of
// privateKey (WIF or 32-byte hex) and signs every input with it. A zero
// sustainFee selects DefaultP2PKHSustainFee.
func NewP2PKHMode(privateKey string, sustainFee uint64) (*Mode, error) {
	priv, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	address, err := script.NewAddressFromPublicKey(priv.PubKey(), true)
	if err != nil {
		return nil, fmt.Errorf("derive P2PKH address: %w", err)
	}
	lockScript, err := p2pkh.Lock(address)
	if err != nil {
		return nil, fmt.Errorf("build P2PKH lock script: %w", err)
	}
	unlocker, err := p2pkh.Unlock(priv, nil)
	if err != nil {
		return nil, fmt.Errorf("build P2PKH unlocker: %w", err)
	}
	if sustainFee == 0 {
		sustainFee = DefaultP2PKHSustainFee
	}
	return &Mode{
		Name:       ModeP2PKH,
		LockScript: lockScript,
		Unlocker:   unlocker,
		SustainFee: sustainFee,
		Address:    address.AddressString,
	}, nil
}

// WithSustainFee returns a copy of m that pays fee per sustain hop.
func (m *Mode) WithSustainFee(fee uint64) *Mode {
	clone := *m
	clone.SustainFee = fee
	return &clone
}

// ScriptHash returns the WhatsOnChain script hash of the mode's lock script.
func (m *Mode) ScriptHash() string {
	return ScriptHash(m.LockScript)
}

// inputSize returns the serialized size in bytes of one input spending an
// output of this mode.
func (m *Mode) inputSize() uint64 {
	var scriptLen uint64
	if m.Unlocker != nil {
		scriptLen = uint64(m.Unlocker.EstimateLength(sdktx.NewTransaction(), 0))
	} else {
		scriptLen = uint64(len(*opTrueScript()))
	}
	// outpoint txid (32) + vout (4) + script length + script + sequence (4)
	return 32 + 4 + varIntSize(scriptLen) + scriptLen + 4
}

// ScriptHash returns the WhatsOnChain script hash of s: the SHA-256 digest of
// the script bytes, in reversed byte order and hex encoded.
func ScriptHash(s *script.Script) string {
	digest := sha256.Sum256(*s)
	slices.Reverse(digest[:])
	return hex.EncodeToString(digest[:])
}

// parsePrivateKey accepts a WIF or a 32-byte hex private key.
func parsePrivateKey(privateKey string) (*ec.PrivateKey, error) {
	key := strings.TrimSpace(privateKey)
	if key == "" {
		return nil, ErrPrivateKeyRequired
	}
	if priv, err := ec.PrivateKeyFromWif(key); err == nil {
		return priv, nil
	}
	if priv, err := ec.PrivateKeyFromHex(key); err == nil {
		return priv, nil
	}
	return nil, ErrInvalidPrivateKey
}

// varIntSize returns the size in bytes of the Bitcoin variable-length
// integer encoding of n.
func varIntSize(n uint64) uint64 {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	case n <= 0xffffffff:
		return 5
	default:
		return 9
	}
}
