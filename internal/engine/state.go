package engine

import "time"

// Lifecycle is the engine's operating state.
type Lifecycle string

// Engine lifecycle states.
const (
	// LifecycleStarting is the state before Run is called.
	LifecycleStarting Lifecycle = "starting"

	// LifecycleBootstrapping covers waiting for funding and the fanouts.
	LifecycleBootstrapping Lifecycle = "bootstrapping"

	// LifecyclePaused means chains exist but the target TPS is 0.
	LifecyclePaused Lifecycle = "paused"

	// LifecycleRunning means chains are broadcasting at the target TPS.
	LifecycleRunning Lifecycle = "running"

	// LifecycleDegraded means broadcasts are failing repeatedly.
	LifecycleDegraded Lifecycle = "degraded"

	// LifecycleStopping is reserved for an orderly shutdown in progress.
	LifecycleStopping Lifecycle = "stopping"

	// LifecycleStopped means Run returned after its context was canceled.
	LifecycleStopped Lifecycle = "stopped"

	// LifecycleFailed means bootstrap failed and no chains are running.
	LifecycleFailed Lifecycle = "failed"
)

// Stage is how far bootstrap has progressed. It is persisted, so restarts
// resume from the last completed stage.
type Stage string

// Bootstrap stages.
const (
	// StageNone means bootstrap has not completed any fanout.
	StageNone Stage = "none"

	// StageL1Done means the seed UTXO has been fanned out to the L1 outputs.
	StageL1Done Stage = "l1_done"

	// StageL2Partial means the L1-to-chains fanout started but did not finish.
	StageL2Partial Stage = "l2_partial"

	// StageL2Done means every chain UTXO exists; chains run from here on.
	StageL2Done Stage = "l2_done"

	// StageUnknown means a pending broadcast could not be replayed; the state
	// must be reconciled by hand before generation resumes.
	StageUnknown Stage = "unknown"
)

// State is a point-in-time snapshot of the engine, served on /status and
// checked by /readyz.
type State struct {
	// Lifecycle is the current operating state.
	Lifecycle Lifecycle `json:"lifecycle"`

	// RequestedTPS is the target transactions per second.
	RequestedTPS int64 `json:"requestedTPS"`

	// AcceptedTPS is the number of broadcasts accepted in the last second.
	AcceptedTPS float64 `json:"acceptedTPS"`

	// ChainsActive is the number of chains currently running.
	ChainsActive int `json:"chainsActive"`

	// ChainsTarget is the number of chains bootstrap creates.
	ChainsTarget int `json:"chainsTarget"`

	// ChainsExhausted counts chains that spent their whole value.
	ChainsExhausted int `json:"chainsExhausted"`

	// BootstrapStage is the persisted bootstrap stage.
	BootstrapStage Stage `json:"bootstrapStage"`

	// BootstrapFailures counts failed L2 fanouts.
	BootstrapFailures int `json:"bootstrapFailures"`

	// LastBroadcastOk is when Arcade last accepted a broadcast.
	LastBroadcastOk time.Time `json:"lastBroadcastOk"`

	// ConsecutiveArcadeFailures counts broadcast failures since the last
	// success.
	ConsecutiveArcadeFailures int `json:"consecutiveArcadeFailures"`

	// LastWoCTime is when WhatsOnChain was last polled for the seed UTXO.
	LastWoCTime time.Time `json:"lastWoCTime"`

	// LastWoCResult summarizes the last WhatsOnChain poll.
	LastWoCResult string `json:"lastWoCResult"`

	// SSE is the last chain tip seen on the Arcade streams.
	SSE SSEState `json:"sse"`

	// LastReorg is the most recent reorg, if any.
	LastReorg *ReorgRecord `json:"lastReorg,omitempty"`

	// LastError is the most recent significant error, if any.
	LastError *ErrorRecord `json:"lastError,omitempty"`
}

// ReorgRecord describes one chain reorganization.
type ReorgRecord struct {
	// Height is the first block height replaced by the reorg.
	Height int `json:"height"`

	// Depth is the number of blocks replaced.
	Depth int `json:"depth"`

	// Time is when the reorg was recorded.
	Time time.Time `json:"time"`

	// ChainsImpacted is the number of chains active at the time.
	ChainsImpacted int `json:"chainsImpacted"`

	// TipBefore is the orphaned tip hash.
	TipBefore string `json:"tipBefore"`

	// TipAfter is the new tip hash.
	TipAfter string `json:"tipAfter"`
}

// ErrorRecord captures the last significant engine error.
type ErrorRecord struct {
	// Component is the part of the engine that failed, such as "arcade".
	Component string `json:"component"`

	// Msg is the error message.
	Msg string `json:"msg"`

	// Time is when the error happened.
	Time time.Time `json:"time"`
}

// SSEState holds the last chain tip seen on the Arcade SSE streams.
type SSEState struct {
	// Connected reports whether a tip event has been received.
	Connected bool `json:"connected"`

	// TipHeight is the height of the last tip.
	TipHeight int `json:"tipHeight"`

	// TipHash is the hash of the last tip.
	TipHash string `json:"tipHash"`

	// TipTime is the block time of the last tip.
	TipTime time.Time `json:"tipTime"`
}
