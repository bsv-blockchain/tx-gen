package engine

import "errors"

var (
	// ErrModeRequired is returned by New without a transaction mode.
	ErrModeRequired = errors.New("engine: transaction mode is required")

	// ErrBroadcasterRequired is returned by New without a Broadcaster.
	ErrBroadcasterRequired = errors.New("engine: broadcaster is required")

	// ErrBootstrapLocked is returned by ConfigureBootstrap once the funding
	// UTXO has been detected.
	ErrBootstrapLocked = errors.New("bootstrap parameters can only be changed before initial funding is detected")

	// ErrInvalidBootstrap is returned for a non-positive bootstrap parameter.
	ErrInvalidBootstrap = errors.New("invalid bootstrap parameter")

	// ErrNoUTXOs is returned when there is nothing to bootstrap from.
	ErrNoUTXOs = errors.New("no UTXOs available")

	// ErrManualRecovery is returned when the persisted state needs to be
	// reconciled by hand before generation can resume.
	ErrManualRecovery = errors.New("bootstrap requires manual recovery")

	// ErrBootstrapIncomplete is returned when the L2 fanout did not create
	// every chain.
	ErrBootstrapIncomplete = errors.New("bootstrap incomplete")
)
