// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package liveness

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness/livenesspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/rpc/nodedialer"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	diskStorage "github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/grpcutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil/singleflight"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

const (
	// TestTimeUntilNodeDead is the test value for TimeUntilNodeDead to quickly
	// mark stores as dead. This needs to be longer than gossip.StoresInterval
	TestTimeUntilNodeDead = 15 * time.Second

	// TestTimeUntilNodeDeadOff is the test value for TimeUntilNodeDead that
	// prevents the store pool from marking stores as dead.
	TestTimeUntilNodeDeadOff = 24 * time.Hour

	timeUntilNodeDeadSettingName    = "server.time_until_store_dead"
	timeAfterNodeSuspectSettingName = "server.time_after_store_suspect"
)

// TimeUntilNodeDead wraps "server.time_until_store_dead".
var TimeUntilNodeDead = settings.RegisterDurationSetting(
	settings.TenantWritable,
	timeUntilNodeDeadSettingName,
	"the time after which if there is no new gossiped information about a store, it is considered dead",
	5*time.Minute,
	func(v time.Duration) error {
		// Setting this to less than the interval for gossiping stores is a big
		// no-no, since this value is compared to the age of the most recent gossip
		// from each store to determine whether that store is live. Put a buffer of
		// 15 seconds on top to allow time for gossip to propagate.
		const minTimeUntilNodeDead = gossip.StoresInterval + 15*time.Second
		if v < minTimeUntilNodeDead {
			return errors.Errorf("cannot set %s to less than %v: %v",
				timeUntilNodeDeadSettingName, minTimeUntilNodeDead, v)
		}
		return nil
	},
).WithPublic()

// TimeAfterNodeSuspect measures how long we consider a store suspect since
// it's last failure.
var TimeAfterNodeSuspect = settings.RegisterDurationSetting(
	settings.SystemOnly,
	timeAfterNodeSuspectSettingName,
	"the amount of time we consider a node suspect for after it becomes unavailable."+
		" A suspect node is typically treated the same as an unavailable node.",
	30*time.Second,
	func(v time.Duration) error {
		// Setting this to less than the interval for gossiping stores is a big
		// no-no, since this value is compared to the age of the most recent gossip
		// from each store to determine whether that store is live.
		const minTimeUntilNodeSuspect = gossip.StoresInterval
		if v < minTimeUntilNodeSuspect {
			return errors.Errorf("cannot set %s to less than %v: %v",
				timeAfterNodeSuspectSettingName, minTimeUntilNodeSuspect, v)
		}
		return nil
	}, func(v time.Duration) error {
		// We enforce a maximum value of 5 minutes for this settings, as setting this
		// to high may result in a prolonged period of unavailability as a recovered
		// store will not be able to acquire leases or replicas for a long time.
		const maxTimeAfterNodeSuspect = 5 * time.Minute
		if v > maxTimeAfterNodeSuspect {
			return errors.Errorf("cannot set %s to more than %v: %v",
				timeAfterNodeSuspectSettingName, maxTimeAfterNodeSuspect, v)
		}
		return nil
	},
)

var (
	// ErrMissingRecord is returned when asking for liveness information
	// about a node for which nothing is known. This happens when attempting to
	// {d,r}ecommission a non-existent node.
	ErrMissingRecord = errors.New("missing liveness record")

	// ErrRecordCacheMiss is returned when asking for the liveness
	// record of a given node and it is not found in the in-memory cache.
	ErrRecordCacheMiss = errors.New("liveness record not found in cache")

	// errChangeMembershipStatusFailed is returned when we're not able to
	// conditionally write the target membership status. It's safe to retry
	// when encountering this error.
	errChangeMembershipStatusFailed = errors.New("failed to change the membership status")

	// ErrEpochIncremented is returned when a heartbeat request fails because
	// the underlying liveness record has had its epoch incremented.
	ErrEpochIncremented = errors.New("heartbeat failed on epoch increment")

	// ErrEpochAlreadyIncremented is returned by IncrementEpoch when
	// someone else has already incremented the epoch to the desired
	// value.
	ErrEpochAlreadyIncremented = errors.New("epoch already incremented")
)

type ErrEpochCondFailed struct {
	expected, actual livenesspb.Liveness
}

// SafeFormatError implements errors.SafeFormatter.
func (e *ErrEpochCondFailed) SafeFormatError(p errors.Printer) error {
	p.Printf(
		"liveness record changed while incrementing epoch for %+v; actual is %+v; is the node still live?",
		redact.Safe(e.expected), redact.Safe(e.actual))
	return nil
}

func (e *ErrEpochCondFailed) Format(s fmt.State, verb rune) { errors.FormatError(e, s, verb) }

func (e *ErrEpochCondFailed) Error() string {
	return fmt.Sprint(e)
}

type errRetryLiveness struct {
	error
}

func (e *errRetryLiveness) Cause() error {
	return e.error
}

func (e *errRetryLiveness) Error() string {
	return fmt.Sprintf("%T: %s", *e, e.error)
}

func isErrRetryLiveness(ctx context.Context, err error) bool {
	if errors.HasType(err, (*kvpb.AmbiguousResultError)(nil)) {
		// We generally want to retry ambiguous errors immediately, except if the
		// ctx is canceled - in which case the ambiguous error is probably caused
		// by the cancellation (and in any case it's pointless to retry with a
		// canceled ctx).
		return ctx.Err() == nil
	} else if errors.HasType(err, (*kvpb.TransactionStatusError)(nil)) {
		// 21.2 nodes can return a TransactionStatusError when they should have
		// returned an AmbiguousResultError.
		// TODO(andrei): Remove this in 22.2.
		return true
	} else if errors.Is(err, kv.OnePCNotAllowedError{}) {
		return true
	}
	return false
}

// Node liveness metrics counter names.
var (
	metaLiveNodes = metric.Metadata{
		Name:        "liveness.livenodes",
		Help:        "Number of live nodes in the cluster (will be 0 if this node is not itself live)",
		Measurement: "Nodes",
		Unit:        metric.Unit_COUNT,
	}
	metaHeartbeatsInFlight = metric.Metadata{
		Name:        "liveness.heartbeatsinflight",
		Help:        "Number of in-flight liveness heartbeats from this node",
		Measurement: "Requests",
		Unit:        metric.Unit_COUNT,
	}
	metaHeartbeatSuccesses = metric.Metadata{
		Name:        "liveness.heartbeatsuccesses",
		Help:        "Number of successful node liveness heartbeats from this node",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaHeartbeatFailures = metric.Metadata{
		Name:        "liveness.heartbeatfailures",
		Help:        "Number of failed node liveness heartbeats from this node",
		Measurement: "Messages",
		Unit:        metric.Unit_COUNT,
	}
	metaEpochIncrements = metric.Metadata{
		Name:        "liveness.epochincrements",
		Help:        "Number of times this node has incremented its liveness epoch",
		Measurement: "Epochs",
		Unit:        metric.Unit_COUNT,
	}
	metaHeartbeatLatency = metric.Metadata{
		Name:        "liveness.heartbeatlatency",
		Help:        "Node liveness heartbeat latency",
		Measurement: "Latency",
		Unit:        metric.Unit_NANOSECONDS,
	}
)

// Metrics holds metrics for use with node liveness activity.
type Metrics struct {
	LiveNodes          *metric.Gauge
	HeartbeatsInFlight *metric.Gauge
	HeartbeatSuccesses *metric.Counter
	HeartbeatFailures  telemetry.CounterWithMetric
	EpochIncrements    telemetry.CounterWithMetric
	HeartbeatLatency   metric.IHistogram
}

// IsLiveCallback is invoked when a node's IsLive state changes to true.
// Callbacks can be registered via NodeLiveness.RegisterCallback().
type IsLiveCallback func(livenesspb.Liveness)

// HeartbeatCallback is invoked whenever this node updates its own liveness status,
// indicating that it is alive.
// TODO(baptist): Remove this callback. The only usage of this is for logging an
// event at startup. This is a little heavyweight of a mechanism for that.
type HeartbeatCallback func(context.Context)

// NodeLiveness is a centralized failure detector that coordinates
// with the epoch-based range system to provide for leases of
// indefinite length (replacing frequent per-range lease renewals with
// heartbeats to the liveness system).
//
// It is also used as a general-purpose failure detector, but it is
// not ideal for this purpose. It is inefficient due to the use of
// replicated durable writes, and is not very sensitive (it primarily
// tests connectivity from the node to the liveness range; a node with
// a failing disk could still be considered live by this system).
//
// The persistent state of node liveness is stored in the KV layer,
// near the beginning of the keyspace. These are normal MVCC keys,
// written by CPut operations in 1PC transactions (the use of
// transactions and MVCC is regretted because it means that the
// liveness span depends on MVCC GC and can get overwhelmed if GC is
// not working. Transactions were used only to piggyback on the
// transaction commit trigger). The leaseholder of the liveness range
// gossips its contents whenever they change (only the changed
// portion); other nodes rarely read from this range directly.
//
// The use of conditional puts is crucial to maintain the guarantees
// needed by epoch-based leases. Both the Heartbeat and IncrementEpoch
// on this type require an expected value to be passed in; see
// comments on those methods for more.
//
// TODO(bdarnell): Also document interaction with draining and decommissioning.
type NodeLiveness struct {
	ambientCtx        log.AmbientContext
	stopper           *stop.Stopper
	clock             *hlc.Clock
	storage           storage
	livenessThreshold time.Duration
	cache             *cache
	renewalDuration   time.Duration
	selfSem           chan struct{}
	st                *cluster.Settings
	otherSem          chan struct{}
	// heartbeatPaused contains an atomically-swapped number representing a bool
	// (1 or 0). heartbeatToken is a channel containing a token which is taken
	// when heartbeating or when pausing the heartbeat. Used for testing.
	heartbeatPaused       uint32
	heartbeatToken        chan struct{}
	metrics               Metrics
	onNodeDecommissioned  func(livenesspb.Liveness)  // noop if nil
	onNodeDecommissioning OnNodeDecommissionCallback // noop if nil
	nodeDialer            *nodedialer.Dialer
	engineSyncs           *singleflight.Group

	// onIsLive is a callback registered by stores prior to starting liveness.
	// It fires when a node transitions from not live to live.
	onIsLive []IsLiveCallback // see RegisterCallback

	// onSelfHeartbeat is invoked after every successful heartbeat
	// of the local liveness instance's heartbeat loop.
	onSelfHeartbeat HeartbeatCallback

	// engines is written to before heartbeating to avoid maintaining liveness
	// when a local disks is stalled.
	engines []diskStorage.Engine

	// Set to true once Start is called. RegisterCallback can not be called after
	// Start is called.
	started syncutil.AtomicBool
}

// Record is a liveness record that has been read from the database, together
// with its database encoding. The encoding is useful for CPut-ing an update to
// the liveness record: the raw value will act as the expected value. This way
// the proto's encoding can change without the CPut failing.
type Record struct {
	livenesspb.Liveness
	// raw represents the raw bytes read from the database - suitable to pass to a
	// CPut. Nil if the value doesn't exist in the DB.
	raw []byte
}

// NodeLivenessOptions is the input to NewNodeLiveness.
//
// The IsLiveCallbacks are registered after construction but before Start is
// called. Everything else is initialized through these Options.
type NodeLivenessOptions struct {
	AmbientCtx              log.AmbientContext
	Stopper                 *stop.Stopper
	Settings                *cluster.Settings
	Gossip                  *gossip.Gossip
	Clock                   *hlc.Clock
	DB                      *kv.DB
	LivenessThreshold       time.Duration
	RenewalDuration         time.Duration
	HistogramWindowInterval time.Duration
	// OnNodeDecommissioned is invoked whenever the instance learns that a
	// node was permanently removed from the cluster. This method must be
	// idempotent as it may be invoked multiple times and defaults to a
	// noop.
	// TODO(baptist): Change this to not take the liveness record
	OnNodeDecommissioned func(livenesspb.Liveness)
	// OnNodeDecommissioning is invoked when a node is detected to be
	// decommissioning.
	OnNodeDecommissioning OnNodeDecommissionCallback
	Engines               []diskStorage.Engine
	OnSelfHeartbeat       HeartbeatCallback
	NodeDialer            *nodedialer.Dialer
}

// NewNodeLiveness returns a new instance of NodeLiveness configured
// with the specified gossip instance.
func NewNodeLiveness(opts NodeLivenessOptions) *NodeLiveness {
	nl := &NodeLiveness{
		ambientCtx:            opts.AmbientCtx,
		stopper:               opts.Stopper,
		clock:                 opts.Clock,
		storage:               storage{db: opts.DB},
		livenessThreshold:     opts.LivenessThreshold,
		renewalDuration:       opts.RenewalDuration,
		selfSem:               make(chan struct{}, 1),
		st:                    opts.Settings,
		otherSem:              make(chan struct{}, 1),
		heartbeatToken:        make(chan struct{}, 1),
		onNodeDecommissioned:  opts.OnNodeDecommissioned,
		onNodeDecommissioning: opts.OnNodeDecommissioning,
		engineSyncs:           singleflight.NewGroup("engine sync", "engine"),
		engines:               opts.Engines,
		onSelfHeartbeat:       opts.OnSelfHeartbeat,
		nodeDialer:            opts.NodeDialer,
	}
	nl.metrics = Metrics{
		LiveNodes:          metric.NewFunctionalGauge(metaLiveNodes, nl.numLiveNodes),
		HeartbeatsInFlight: metric.NewGauge(metaHeartbeatsInFlight),
		HeartbeatSuccesses: metric.NewCounter(metaHeartbeatSuccesses),
		HeartbeatFailures:  telemetry.NewCounterWithMetric(metaHeartbeatFailures),
		EpochIncrements:    telemetry.NewCounterWithMetric(metaEpochIncrements),
		HeartbeatLatency: metric.NewHistogram(metric.HistogramOptions{
			Mode:     metric.HistogramModePreferHdrLatency,
			Metadata: metaHeartbeatLatency,
			Duration: opts.HistogramWindowInterval,
			Buckets:  metric.NetworkLatencyBuckets,
		}),
	}
	nl.cache = newCache(opts.Gossip, opts.Clock, nl.cacheUpdated)
	nl.heartbeatToken <- struct{}{}

	return nl
}

var errNodeDrainingSet = errors.New("node is already draining")

func (nl *NodeLiveness) sem(nodeID roachpb.NodeID) chan struct{} {
	if nodeID == nl.cache.selfID() {
		return nl.selfSem
	}
	return nl.otherSem
}

// SetDraining attempts to update this node's liveness record to put itself
// into the draining state.
//
// The reporter callback, if non-nil, is called on a best effort basis
// to report work that needed to be done and which may or may not have
// been done by the time this call returns. See the explanation in
// pkg/server/drain.go for details.
func (nl *NodeLiveness) SetDraining(
	ctx context.Context, drain bool, reporter func(int, redact.SafeString),
) error {
	ctx = nl.ambientCtx.AnnotateCtx(ctx)
	retryOpts := base.DefaultRetryOptions()
	retryOpts.Closer = nl.stopper.ShouldQuiesce()
	for r := retry.StartWithCtx(ctx, retryOpts); r.Next(); {
		oldLivenessRec, ok := nl.cache.Self()
		if !ok {
			// There was a cache miss, let's now fetch the record from KV
			// directly.
			nodeID := nl.cache.selfID()
			livenessRec, err := nl.getLivenessRecordFromKV(ctx, nodeID)
			if err != nil {
				return err
			}
			oldLivenessRec = livenessRec
		}
		if err := nl.setDrainingInternal(ctx, oldLivenessRec, drain, reporter); err != nil {
			if log.V(1) {
				log.Infof(ctx, "attempting to set liveness draining status to %v: %v", drain, err)
			}
			if grpcutil.IsConnectionRejected(err) {
				return err
			}
			continue
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("failed to drain self")
}

// SetMembershipStatus changes the liveness record to reflect the target
// membership status. It does so idempotently, and may retry internally until it
// observes its target state durably persisted. It returns whether it was able
// to change the membership status (as opposed to it returning early when
// finding the target status possibly set by another node).
func (nl *NodeLiveness) SetMembershipStatus(
	ctx context.Context, nodeID roachpb.NodeID, targetStatus livenesspb.MembershipStatus,
) (statusChanged bool, err error) {
	ctx = nl.ambientCtx.AnnotateCtx(ctx)

	attempt := func() (bool, error) {
		// Allow only one decommissioning attempt in flight per node at a time.
		// This is required for correct results since we may otherwise race with
		// concurrent `IncrementEpoch` calls and get stuck in a situation in
		// which the cached liveness is has decommissioning=false while it's
		// really true, and that means that SetDecommissioning becomes a no-op
		// (which is correct) but that our cached liveness never updates to
		// reflect that.
		//
		// See https://github.com/cockroachdb/cockroach/issues/17995.
		sem := nl.sem(nodeID)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		defer func() {
			<-sem
		}()

		// We need the current liveness in each iteration.
		//
		// We ignore any liveness record in Gossip because we may have to fall back
		// to the KV store anyway. The scenario in which this is needed is:
		// - kill node 2 and stop node 1
		// - wait for node 2's liveness record's Gossip entry to expire on all surviving nodes
		// - restart node 1; it'll never see node 2 in `GetLiveness` unless the whole
		//   node liveness span gets regossiped (unlikely if it wasn't the lease holder
		//   for that span)
		// - can't decommission node 2 from node 1 without KV fallback.
		//
		// See #20863.
		oldLivenessRec, err := nl.getLivenessRecordFromKV(ctx, nodeID)
		if err != nil {
			return false, err
		}

		return nl.setMembershipStatusInternal(ctx, oldLivenessRec, targetStatus)
	}

	for {
		statusChanged, err := attempt()
		if errors.Is(err, errChangeMembershipStatusFailed) {
			// Expected when epoch incremented, it's safe to retry.
			continue
		}
		return statusChanged, err
	}
}

func (nl *NodeLiveness) setDrainingInternal(
	ctx context.Context, oldLivenessRec Record, drain bool, reporter func(int, redact.SafeString),
) error {
	sem := nl.selfSem
	// Allow only one attempt to set the draining field at a time.
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() {
		<-sem
	}()

	if oldLivenessRec.Liveness == (livenesspb.Liveness{}) {
		return errors.AssertionFailedf("invalid old liveness record; found to be empty")
	}

	// Let's compute what our new liveness record should be. We start off with a
	// copy of our existing liveness record.
	newLiveness := oldLivenessRec.Liveness

	if reporter != nil && drain && !newLiveness.Draining {
		// Report progress to the Drain RPC.
		reporter(1, "liveness record")
	}
	newLiveness.Draining = drain

	update := livenessUpdate{
		oldLiveness: oldLivenessRec.Liveness,
		newLiveness: newLiveness,
		oldRaw:      oldLivenessRec.raw,
	}
	// TODO(baptist): retry on failure.
	written, err := nl.updateLiveness(ctx, update, func(actual Record) error {
		// Handle a stale cache by updating with the value we just read.
		nl.cache.maybeUpdate(ctx, actual)

		if actual.Draining == update.newLiveness.Draining {
			return errNodeDrainingSet
		}
		return errors.New("failed to update liveness record because record has changed")
	})
	if err != nil {
		if log.V(1) {
			log.Infof(ctx, "updating liveness record: %v", err)
		}
		if errors.Is(err, errNodeDrainingSet) {
			return nil
		}
		return err
	}

	nl.cache.maybeUpdate(ctx, written)
	return nil
}

func (nl *NodeLiveness) cacheUpdated(old livenesspb.Liveness, new livenesspb.Liveness) {
	// TODO(baptist): This won't work correctly we remove expiration timestamp.
	// Need to use a different signal to determine if liveness changed.
	now := nl.clock.Now()
	if !old.IsLive(now) && new.IsLive(now) {
		// NB: If we are not started, we don't use the onIsLive callbacks since they
		// can still change. This is a bit of a tangled mess since the startup of
		// liveness requires the stores to be started, but stores can't start until
		// liveness can run. Ideally we could cache all these updates and call
		// onIsLive as part of start.
		if nl.started.Get() {
			for _, fn := range nl.onIsLive {
				fn(new)
			}
		}
	}
	if !old.Membership.Decommissioned() && new.Membership.Decommissioned() && nl.onNodeDecommissioned != nil {
		nl.onNodeDecommissioned(new)
	}
	if !old.Membership.Decommissioning() && new.Membership.Decommissioning() && nl.onNodeDecommissioning != nil {
		nl.onNodeDecommissioning(new.NodeID)
	}
}

// CreateLivenessRecord creates a liveness record for the node specified by the
// given node ID. This is typically used when adding a new node to a running
// cluster, or when bootstrapping a cluster through a given node.
//
// This is a pared down version of Start; it exists only to durably
// persist a liveness to record the node's existence. Nodes will heartbeat their
// records after starting up, and incrementing to epoch=1 when doing so, at
// which point we'll set an appropriate expiration timestamp, gossip the
// liveness record, and update our in-memory representation of it.
//
// NB: An existing liveness record is not overwritten by this method, we return
// an error instead.
func (nl *NodeLiveness) CreateLivenessRecord(ctx context.Context, nodeID roachpb.NodeID) error {
	return nl.storage.create(ctx, nodeID)
}

func (nl *NodeLiveness) setMembershipStatusInternal(
	ctx context.Context, oldLivenessRec Record, targetStatus livenesspb.MembershipStatus,
) (statusChanged bool, err error) {
	if valid, err := livenesspb.ValidateTransition(oldLivenessRec.Liveness, targetStatus); !valid {
		return false, err
	}

	// Let's compute what our new liveness record should be. We start off with a
	// copy of our existing liveness record.
	newLiveness := oldLivenessRec.Liveness
	newLiveness.Membership = targetStatus

	update := livenessUpdate{
		newLiveness: newLiveness,
		oldLiveness: oldLivenessRec.Liveness,
		oldRaw:      oldLivenessRec.raw,
	}
	statusChanged = true
	if _, err := nl.updateLiveness(ctx, update, func(actual Record) error {
		if actual.Membership != update.newLiveness.Membership {
			// We're racing with another attempt at updating the liveness
			// record, we error out in order to retry.
			return errChangeMembershipStatusFailed
		}
		// The found liveness membership status is the same as the target one,
		// so we consider our work done. We inform the caller that this attempt
		// was a no-op.
		statusChanged = false
		return nil
	}); err != nil {
		return false, err
	}

	return statusChanged, nil
}

// GetLivenessThreshold returns the maximum duration between heartbeats
// before a node is considered not-live.
func (nl *NodeLiveness) GetLivenessThreshold() time.Duration {
	return nl.livenessThreshold
}

// IsLive returns whether or not the specified node is considered live based on
// whether or not its liveness has expired regardless of the liveness status. It
// is an error if the specified node is not in the local liveness table.
func (nl *NodeLiveness) IsLive(nodeID roachpb.NodeID) (bool, error) {
	liveness, ok := nl.GetLiveness(nodeID)
	if !ok {
		// TODO(irfansharif): We only expect callers to supply us with node IDs
		// they learnt through existing liveness records, which implies we
		// should never find ourselves here. We should clean up this conditional
		// once we re-visit the caching structure used within NodeLiveness;
		// we should be able to return ErrMissingRecord instead.
		return false, ErrRecordCacheMiss
	}
	// NB: We use clock.Now() in order to consider clock signals from other nodes.
	return liveness.IsLive(nl.clock.Now()), nil
}

// IsAvailable returns whether or not the specified node is available to serve
// requests. It checks both the liveness and decommissioned states, but not
// draining or decommissioning (since it may still be a leaseholder for ranges).
// Returns false if the node is not in the local liveness table.
func (nl *NodeLiveness) IsAvailable(nodeID roachpb.NodeID) bool {
	liveness, ok := nl.GetLiveness(nodeID)
	return ok && liveness.IsLive(nl.clock.Now()) && !liveness.Membership.Decommissioned()
}

// IsAvailableNotDraining returns whether or not the specified node is available
// to serve requests (i.e. it is live and not decommissioned) and is not in the
// process of draining/decommissioning. Note that draining/decommissioning nodes
// could still be leaseholders for ranges until drained, so this should not be
// used when the caller needs to be able to contact leaseholders directly.
// Returns false if the node is not in the local liveness table.
func (nl *NodeLiveness) IsAvailableNotDraining(nodeID roachpb.NodeID) bool {
	liveness, ok := nl.GetLiveness(nodeID)
	return ok &&
		liveness.IsLive(nl.clock.Now()) &&
		!liveness.Membership.Decommissioning() &&
		!liveness.Membership.Decommissioned() &&
		!liveness.Draining
}

// OnNodeDecommissionCallback is a callback that is invoked when a node is
// detected to be decommissioning.
type OnNodeDecommissionCallback func(nodeID roachpb.NodeID)

// Start starts a periodic heartbeat to refresh this node's last
// heartbeat in the node liveness table. The optionally provided
// HeartbeatCallback will be invoked whenever this node updates its
// own liveness. The slice of engines will be written to before each
// heartbeat to avoid maintaining liveness in the presence of disk stalls.
// TODO(baptist): If we completely remove epoch leases, this can be merged with
// the NewNodeLiveness function. Currently the liveness is required prior to
// Start getting called in replica_range_lease. For non-epoch leases this should
// be possible.
func (nl *NodeLiveness) Start(ctx context.Context) {
	log.VEventf(ctx, 1, "starting node liveness instance")
	if nl.started.Get() {
		// This is meant to prevent tests from calling start twice.
		log.Fatal(ctx, "liveness already started")
	}

	retryOpts := base.DefaultRetryOptions()
	retryOpts.Closer = nl.stopper.ShouldQuiesce()

	nl.started.Set(true)
	// We may have received some liveness records from Gossip prior to Start being
	// called. We need to go through and notify all the callers of them now.
	for _, entry := range nl.ScanNodeVitalityFromCache() {
		if entry.IsAlive() {
			for _, fn := range nl.onIsLive {
				fn(entry.Liveness)
			}
		}
	}

	_ = nl.stopper.RunAsyncTaskEx(ctx, stop.TaskOpts{TaskName: "liveness-hb", SpanOpt: stop.SterileRootSpan}, func(context.Context) {
		ambient := nl.ambientCtx
		ambient.AddLogTag("liveness-hb", nil)
		ctx, cancel := nl.stopper.WithCancelOnQuiesce(context.Background())
		defer cancel()
		ctx, sp := ambient.AnnotateCtxWithSpan(ctx, "liveness heartbeat loop")
		defer sp.Finish()

		incrementEpoch := true
		heartbeatInterval := nl.livenessThreshold - nl.renewalDuration
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-nl.heartbeatToken:
			case <-nl.stopper.ShouldQuiesce():
				return
			}
			// Give the context a timeout approximately as long as the time we
			// have left before our liveness entry expires.
			if err := timeutil.RunWithTimeout(ctx, "node liveness heartbeat", nl.renewalDuration,
				func(ctx context.Context) error {
					nl.cache.CheckForStaleEntries(gossip.StoreTTL)
					// Retry heartbeat in the event the conditional put fails.
					for r := retry.StartWithCtx(ctx, retryOpts); r.Next(); {
						oldLiveness, ok := nl.Self()
						if !ok {
							nodeID := nl.cache.selfID()
							liveness, err := nl.getLivenessRecordFromKV(ctx, nodeID)
							if err != nil {
								log.Infof(ctx, "unable to get liveness record from KV: %s", err)
								if grpcutil.IsConnectionRejected(err) {
									return err
								}
								continue
							}
							oldLiveness = liveness.Liveness
						}
						if err := nl.heartbeatInternal(ctx, oldLiveness, incrementEpoch); err != nil {
							if errors.Is(err, ErrEpochIncremented) {
								log.Infof(ctx, "%s; retrying", err)
								continue
							}
							return err
						}
						incrementEpoch = false // don't increment epoch after first heartbeat
						break
					}
					return nil
				}); err != nil {
				log.Warningf(ctx, heartbeatFailureLogFormat, err)
			}

			nl.heartbeatToken <- struct{}{}
			select {
			case <-ticker.C:
			case <-nl.stopper.ShouldQuiesce():
				return
			}
		}
	})
}

const heartbeatFailureLogFormat = `failed node liveness heartbeat: %v

An inability to maintain liveness will prevent a node from participating in a
cluster. If this problem persists, it may be a sign of resource starvation or
of network connectivity problems. For help troubleshooting, visit:

    https://www.cockroachlabs.com/docs/stable/cluster-setup-troubleshooting.html#node-liveness-issues

`

// PauseHeartbeatLoopForTest stops the periodic heartbeat. The function
// waits until it acquires the heartbeatToken (unless heartbeat was
// already paused); this ensures that no heartbeats happen after this is
// called. Returns a closure to call to re-enable the heartbeat loop.
// This function is only safe for use in tests.
func (nl *NodeLiveness) PauseHeartbeatLoopForTest() func() {
	if swapped := atomic.CompareAndSwapUint32(&nl.heartbeatPaused, 0, 1); swapped {
		<-nl.heartbeatToken
	}
	return func() {
		if swapped := atomic.CompareAndSwapUint32(&nl.heartbeatPaused, 1, 0); swapped {
			nl.heartbeatToken <- struct{}{}
		}
	}
}

// PauseSynchronousHeartbeatsForTest disables all node liveness
// heartbeats triggered from outside the normal Start loop.
// Returns a closure to call to re-enable synchronous heartbeats. Only
// safe for use in tests.
func (nl *NodeLiveness) PauseSynchronousHeartbeatsForTest() func() {
	nl.selfSem <- struct{}{}
	nl.otherSem <- struct{}{}
	return func() {
		<-nl.selfSem
		<-nl.otherSem
	}
}

// PauseAllHeartbeatsForTest disables all node liveness heartbeats,
// including those triggered from outside the normal Start
// loop. Returns a closure to call to re-enable heartbeats. Only safe
// for use in tests.
func (nl *NodeLiveness) PauseAllHeartbeatsForTest() func() {
	enableLoop := nl.PauseHeartbeatLoopForTest()
	enableSync := nl.PauseSynchronousHeartbeatsForTest()
	return func() {
		enableLoop()
		enableSync()
	}
}

var errNodeAlreadyLive = errors.New("node already live")

// Heartbeat is called to update a node's expiration timestamp. This
// method does a conditional put on the node liveness record, and if
// successful, stores the updated liveness record in the nodes map.
//
// The liveness argument is the expected previous value of this node's
// liveness.
//
// If this method returns nil, the node's liveness has been extended,
// relative to the previous value. It may or may not still be alive
// when this method returns.
//
// On failure, this method returns ErrEpochIncremented, although this
// may not necessarily mean that the epoch was actually incremented.
// TODO(bdarnell): Fix error semantics here.
//
// This method is rarely called directly; heartbeats are normally sent
// by the Start loop.
// TODO(bdarnell): Should we just remove this synchronous heartbeat completely?
func (nl *NodeLiveness) Heartbeat(ctx context.Context, liveness livenesspb.Liveness) error {
	return nl.heartbeatInternal(ctx, liveness, false /* increment epoch */)
}

func (nl *NodeLiveness) heartbeatInternal(
	ctx context.Context, oldLiveness livenesspb.Liveness, incrementEpoch bool,
) (err error) {
	ctx, sp := tracing.EnsureChildSpan(ctx, nl.ambientCtx.Tracer, "liveness heartbeat")
	defer sp.Finish()
	defer func(start time.Time) {
		dur := timeutil.Since(start)
		nl.metrics.HeartbeatLatency.RecordValue(dur.Nanoseconds())
		if dur > time.Second {
			log.Warningf(ctx, "slow heartbeat took %s; err=%v", dur, err)
		}
	}(timeutil.Now())

	// Collect a clock reading from before we begin queuing on the heartbeat
	// semaphore. This method (attempts to, see [*]) guarantees that, if
	// successful, the liveness record's expiration will be at least the
	// liveness threshold above the time that the method was called.
	// Collecting this clock reading before queuing allows us to enforce
	// this while avoiding redundant liveness heartbeats during thundering
	// herds without needing to explicitly coalesce heartbeats.
	//
	// [*]: see TODO below about how errNodeAlreadyLive handling does not
	//      enforce this guarantee.
	beforeQueueTS := nl.clock.Now()
	minExpiration := beforeQueueTS.Add(nl.livenessThreshold.Nanoseconds(), 0).ToLegacyTimestamp()

	// Before queueing, record the heartbeat as in-flight.
	nl.metrics.HeartbeatsInFlight.Inc(1)
	defer nl.metrics.HeartbeatsInFlight.Dec(1)

	// Allow only one heartbeat at a time.
	sem := nl.selfSem
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() {
		<-sem
	}()

	// If we are not intending to increment the node's liveness epoch, detect
	// whether this heartbeat is needed anymore. It is possible that we queued
	// for long enough on the semaphore such that other heartbeat attempts ahead
	// of us already incremented the expiration past what we wanted. Note that
	// if we allowed the heartbeat to proceed in this case, we know that it
	// would hit a ConditionFailedError and return a errNodeAlreadyLive down
	// below.
	if !incrementEpoch {
		curLiveness, ok := nl.Self()
		if ok && minExpiration.Less(curLiveness.Expiration) {
			return nil
		}
	}

	if oldLiveness == (livenesspb.Liveness{}) {
		return errors.AssertionFailedf("invalid old liveness record; found to be empty")
	}

	// Let's compute what our new liveness record should be. Start off with our
	// existing view of things.
	newLiveness := oldLiveness
	if incrementEpoch {
		newLiveness.Epoch++
		newLiveness.Draining = false // clear draining field
	}

	// Grab a new clock reading to compute the new expiration time,
	// since we may have queued on the semaphore for a while.
	afterQueueTS := nl.clock.Now()
	newLiveness.Expiration = afterQueueTS.Add(nl.livenessThreshold.Nanoseconds(), 0).ToLegacyTimestamp()
	// This guards against the system clock moving backwards. As long
	// as the cockroach process is running, checks inside hlc.Clock
	// will ensure that the clock never moves backwards, but these
	// checks don't work across process restarts.
	if newLiveness.Expiration.Less(oldLiveness.Expiration) {
		return errors.Errorf("proposed liveness update expires earlier than previous record")
	}

	update := livenessUpdate{
		oldLiveness: oldLiveness,
		newLiveness: newLiveness,
	}
	written, err := nl.updateLiveness(ctx, update, func(actual Record) error {
		// Update liveness to actual value on mismatch.
		nl.cache.maybeUpdate(ctx, actual)

		// If the actual liveness is different than expected, but is
		// considered live, treat the heartbeat as a success. This can
		// happen when the periodic heartbeater races with a concurrent
		// lease acquisition.
		//
		// TODO(bdarnell): If things are very slow, the new liveness may
		// have already expired and we'd incorrectly return
		// ErrEpochIncremented. Is this check even necessary? The common
		// path through this method doesn't check whether the liveness
		// expired while in flight, so maybe we don't have to care about
		// that and only need to distinguish between same and different
		// epochs in our return value.
		//
		// TODO(nvanbenschoten): Unlike the early return above, this doesn't
		// guarantee that the resulting expiration is past minExpiration,
		// only that it's different than our oldLiveness. Is that ok? It
		// hasn't caused issues so far, but we might want to detect this
		// case and retry, at least in the case of the liveness heartbeat
		// loop. The downside of this is that a heartbeat that's intending
		// to bump the expiration of a record out 9s into the future may
		// return a success even if the expiration is only 5 seconds in the
		// future. The next heartbeat will then start with only 0.5 seconds
		// before expiration.
		if actual.IsLive(nl.clock.Now()) && !incrementEpoch {
			return errNodeAlreadyLive
		}
		// Otherwise, return error.
		return ErrEpochIncremented
	})
	if err != nil {
		if errors.Is(err, errNodeAlreadyLive) {
			nl.metrics.HeartbeatSuccesses.Inc(1)
			return nil
		}
		nl.metrics.HeartbeatFailures.Inc()
		return err
	}

	log.VEventf(ctx, 1, "heartbeat %+v", written.Expiration)
	nl.cache.maybeUpdate(ctx, written)
	nl.metrics.HeartbeatSuccesses.Inc(1)
	return nil
}

// Self returns the liveness record for this node. ErrMissingRecord
// is returned in the event that the node has neither heartbeat its
// liveness record successfully, nor received a gossip message containing
// a former liveness update on restart.
func (nl *NodeLiveness) Self() (_ livenesspb.Liveness, ok bool) {
	rec, ok := nl.cache.Self()
	if !ok {
		return livenesspb.Liveness{}, false
	}
	return rec.Liveness, true
}

// GetIsLiveMap returns a map of nodeID to boolean liveness status of
// each node. This excludes nodes that were removed completely (dead +
// decommissioning).
// TODO(baptist): Remove.
func (nl *NodeLiveness) GetIsLiveMap() livenesspb.IsLiveMap {
	return nl.cache.GetIsLiveMap()
}

// ScanNodeVitalityFromCache returns a map of nodeID to boolean liveness status
// of each node from the cache. This excludes nodes that were decommissioned.
// Decommissioned nodes are kept in the KV store and the cache forever, but are
// typically not referenced in normal usage. The method ScanNodeVitalityFromKV
// does return decommissioned nodes.
func (nl *NodeLiveness) ScanNodeVitalityFromCache() livenesspb.NodeVitalityMap {
	entries := nl.cache.getAllLivenessEntries()
	statusMap := make(map[roachpb.NodeID]livenesspb.NodeVitality, len(entries))
	for _, l := range entries {
		if l.Membership.Decommissioned() {
			// This is a node that was completely removed. Skip over it.
			continue
		}
		statusMap[l.NodeID] = nl.convertToNodeVitality(l)
	}
	return statusMap
}

// GetLivenessesFromKV returns a slice containing the liveness record of all
// nodes that have ever been a part of the cluster. The records are read from
// the KV layer in a KV transaction. This is in contrast to GetLivenesses above,
// which consults a (possibly stale) in-memory cache.
// TODO(baptist): Remove.
func (nl *NodeLiveness) GetLivenessesFromKV(ctx context.Context) ([]livenesspb.Liveness, error) {
	records, err := nl.storage.scan(ctx)
	if err != nil {
		return nil, err
	}
	livenesses := make([]livenesspb.Liveness, len(records))
	for i, r := range records {
		livenesses[i] = r.Liveness
		nl.cache.maybeUpdate(ctx, r)
	}
	return livenesses, nil
}

// ScanNodeVitalityFromKV returns the status for all the nodes from KV including
// nodes that have been decommissioned. This method is typically used when the
// set of results must be accurate as of a point in time. since decisions can be
// made based on the values. Most code should call either
// ScanNodeVitalityFromCache or GetNodeVitalityFromCache.
func (nl *NodeLiveness) ScanNodeVitalityFromKV(
	ctx context.Context,
) (livenesspb.NodeVitalityMap, error) {
	records, err := nl.storage.scan(ctx)
	if err != nil {
		return nil, err
	}

	statusMap := make(map[roachpb.NodeID]livenesspb.NodeVitality, len(records))
	for _, liveness := range records {
		vitality := nl.convertToNodeVitality(liveness.Liveness)
		nl.cache.maybeUpdate(ctx, liveness)
		statusMap[liveness.NodeID] = vitality
	}
	return statusMap, nil
}

// convertToNodeVitality is used if you already have a Liveness record received
// externally.
func (nl *NodeLiveness) convertToNodeVitality(l livenesspb.Liveness) livenesspb.NodeVitality {
	// The store is considered dead if it hasn't been updated via gossip
	// within the liveness threshold. Note that LastUpdatedTime is set
	// when the store detail is created and will have a non-zero value
	// even before the first gossip arrives for a store.
	now := nl.clock.Now()
	timeUntilNodeDead := TimeUntilNodeDead.Get(&nl.st.SV)
	suspectDuration := TimeAfterNodeSuspect.Get(&nl.st.SV)

	var health livenesspb.VitalityStatus
	isLive := l.IsLive(now)
	isDead := l.IsDead(now, timeUntilNodeDead)

	if isDead {
		health = livenesspb.VitalityDead
	} else if isLive {
		health = livenesspb.VitalityAlive
	} else {
		health = livenesspb.VitalityUnavailable
	}

	// If there is a valid descriptor, check that it is being updated. If we don't
	// have one it may be because we haven't gotten the first gossip update yet.
	if lastDescUpdate, valid := nl.cache.LastDescriptorUpdate(l.NodeID); valid {
		deadAsOf := lastDescUpdate.lastUpdateTime.AddDuration(timeUntilNodeDead)
		if now.After(deadAsOf) {
			// If the store descriptor is not being updated, we mark the node as dead
			// regardless of what liveness says.
			health = livenesspb.VitalityDead
		}
		if lastDescUpdate.lastUnavailableTime.AddDuration(suspectDuration).After(now) {
			health = livenesspb.VitalitySuspect
		}
	}
	// NB: nodeDialer is nil in some tests.
	connected := nl.nodeDialer == nil || nl.nodeDialer.ConnHealth(l.NodeID, rpc.SystemClass) == nil

	return l.CreateNodeVitality(health, connected)
}

// GetNodeVitalityFromCache returns the current status of the node. This method
// is "time sensitive", so the result of calling it should not be cached. The
// liveness is calculated based at the time this method is called. The return
// NodeVitality records are "static" and calculated based on the HLC clock when
// this method is called. The results should not be cached externally as they
// may no longer be accurate in the future. See livenesspb.NodeVitality for
// using this method.
func (nl *NodeLiveness) GetNodeVitalityFromCache(nodeID roachpb.NodeID) livenesspb.NodeVitality {
	l, ok := nl.cache.GetLiveness(nodeID)
	if !ok {
		// If we don't have a liveness record, we can't create a full NodeVitality.
		// This is a little unfortunate as we may have a NodeDescriptor.
		return livenesspb.NodeVitality{}
	}
	return nl.convertToNodeVitality(l.Liveness)
}

// GetLiveness returns the liveness record for the specified nodeID. If the
// liveness record is not found (due to gossip propagation delays or due to the
// node not existing), we surface that to the caller. The record returned also
// includes the raw, encoded value that the database has for this liveness
// record in addition to the decoded liveness proto.
// TODO(baptist): Remove.
func (nl *NodeLiveness) GetLiveness(nodeID roachpb.NodeID) (_ Record, ok bool) {
	return nl.cache.GetLiveness(nodeID)
}

// getLivenessRecordFromKV fetches the liveness record from KV for a given node,
// and updates the internal in-memory cache when doing so. It returns a Record
// with the encoded value that the database has for this liveness record in
// addition to the decoded liveness proto. The Record is required for updates.
func (nl *NodeLiveness) getLivenessRecordFromKV(
	ctx context.Context, nodeID roachpb.NodeID,
) (Record, error) {
	livenessRec, err := nl.storage.get(ctx, nodeID)
	if err == nil {
		// Update our cache with the liveness record we just found.
		nl.cache.maybeUpdate(ctx, livenessRec)
	}

	return livenessRec, err
}

// IncrementEpoch is called to attempt to revoke another node's
// current epoch, causing an expiration of all its leases. This method
// does a conditional put on the node liveness record, and if
// successful, stores the updated liveness record in the nodes map. If
// this method is called on a node ID which is considered live
// according to the most recent information gathered through gossip,
// an error is returned.
//
// The liveness argument is used as the expected value on the
// conditional put. If this method returns nil, there was a match and
// the epoch has been incremented. This means that the expiration time
// in the supplied liveness accurately reflects the time at which the
// epoch ended.
//
// If this method returns ErrEpochAlreadyIncremented, the epoch has
// already been incremented past the one in the liveness argument, but
// the conditional put did not find a match. This means that another
// node performed a successful IncrementEpoch, but we can't tell at
// what time the epoch actually ended. (Usually when multiple
// IncrementEpoch calls race, they're using the same expected value.
// But when there is a severe backlog, it's possible for one increment
// to get stuck in a queue long enough for the dead node to make
// another successful heartbeat, and a second increment to come in
// after that)
func (nl *NodeLiveness) IncrementEpoch(ctx context.Context, liveness livenesspb.Liveness) error {
	// Allow only one increment at a time.
	sem := nl.sem(liveness.NodeID)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() {
		<-sem
	}()

	if liveness.IsLive(nl.clock.Now()) {
		return errors.Errorf("cannot increment epoch on live node: %+v", liveness)
	}

	update := livenessUpdate{
		newLiveness: liveness,
		oldLiveness: liveness,
	}
	update.newLiveness.Epoch++

	written, err := nl.updateLiveness(ctx, update, func(actual Record) error {
		nl.cache.maybeUpdate(ctx, actual)

		if actual.Epoch > liveness.Epoch {
			return ErrEpochAlreadyIncremented
		} else if actual.Epoch < liveness.Epoch {
			return errors.Errorf("unexpected liveness epoch %d; expected >= %d", actual.Epoch, liveness.Epoch)
		}
		return &ErrEpochCondFailed{
			expected: liveness,
			actual:   actual.Liveness,
		}
	})
	if err != nil {
		return err
	}

	log.Infof(ctx, "incremented n%d liveness epoch to %d", written.NodeID, written.Epoch)
	nl.cache.maybeUpdate(ctx, written)
	nl.metrics.EpochIncrements.Inc()
	return nil
}

// Metrics returns a struct which contains metrics related to node
// liveness activity.
func (nl *NodeLiveness) Metrics() Metrics {
	return nl.metrics
}

// RegisterCallback registers a callback to be invoked any time a
// node's IsLive() state changes to true. This must be called before Start.
func (nl *NodeLiveness) RegisterCallback(cb IsLiveCallback) {
	if nl.started.Get() {
		log.Fatalf(context.TODO(), "RegisterCallback called after Start")
	}
	nl.onIsLive = append(nl.onIsLive, cb)
}

// updateLiveness does a conditional put on the node liveness record for the
// node specified by nodeID. In the event that the conditional put fails, the
// handleCondFailed callback is invoked with the actual node liveness record;
// the error returned by the callback replaces the ConditionFailedError as the
// retval, and an empty Record is returned.
//
// The conditional put is done as a 1PC transaction with a ModifiedSpanTrigger
// which indicates the node liveness record that the range leader should gossip
// on commit.
//
// updateLiveness terminates certain errors that are expected to occur
// sporadically, such as ambiguous results.
//
// If the CPut is successful (i.e. no error is returned and handleCondFailed is
// not called), the value that has been written is returned as a Record.
// This includes the encoded bytes, and it can be used to update the local
// cache.
func (nl *NodeLiveness) updateLiveness(
	ctx context.Context, update livenessUpdate, handleCondFailed func(actual Record) error,
) (Record, error) {
	if err := nl.verifyDiskHealth(ctx); err != nil {
		return Record{}, err
	}
	retryOpts := base.DefaultRetryOptions()
	retryOpts.Closer = nl.stopper.ShouldQuiesce()
	for r := retry.StartWithCtx(ctx, retryOpts); r.Next(); {
		written, err := nl.updateLivenessAttempt(ctx, update, handleCondFailed)
		if err != nil {
			if errors.HasType(err, (*errRetryLiveness)(nil)) {
				log.Infof(ctx, "retrying liveness update after %s", err)
				continue
			}
			return Record{}, err
		}
		if nl.started.Get() && nl.onSelfHeartbeat != nil {
			nl.onSelfHeartbeat(ctx)
		}
		return written, nil
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	return Record{}, errors.New("retry loop ended without error - likely shutting down")
}

// verifyDiskHealth does a sync write to all disks before updating liveness, so
// that a faulty or stalled disk will cause us to fail liveness and lose our
// leases. All disks are written concurrently.
// We do this asynchronously in order to respect the caller's context, and
// coalesce concurrent writes onto an in-flight one. This is particularly
// relevant for a stalled disk during a lease acquisition heartbeat, where we
// need to return a timely NLHE to the caller such that it will try a different
// replica and nudge it into acquiring the lease. This can leak a goroutine in
// the case of a stalled disk.
func (nl *NodeLiveness) verifyDiskHealth(ctx context.Context) error {
	resultCs := make([]singleflight.Future, len(nl.engines))
	for i, eng := range nl.engines {
		eng := eng // pin the loop variable
		resultCs[i], _ = nl.engineSyncs.DoChan(ctx,
			strconv.Itoa(i),
			singleflight.DoOpts{
				Stop:               nl.stopper,
				InheritCancelation: false,
			},
			func(ctx context.Context) (interface{}, error) {
				return nil, diskStorage.WriteSyncNoop(eng)
			})
	}
	for _, resultC := range resultCs {
		r := resultC.WaitForResult(ctx)
		if r.Err != nil {
			return errors.Wrapf(r.Err, "disk write failed while updating node liveness")
		}
	}
	return nil
}

func (nl *NodeLiveness) updateLivenessAttempt(
	ctx context.Context, update livenessUpdate, handleCondFailed func(actual Record) error,
) (Record, error) {
	// If the caller is not manually providing the previous value in
	// update.oldRaw. we need to read it from our cache.
	if update.oldRaw == nil {
		l, ok := nl.cache.GetLiveness(update.newLiveness.NodeID)
		if !ok {
			// TODO(irfansharif): See TODO in `NodeLiveness.IsLive`, the same
			// applies to this conditional. We probably want to be able to
			// return ErrMissingRecord here instead.
			return Record{}, ErrRecordCacheMiss
		}
		if l.Liveness != update.oldLiveness {
			return Record{}, handleCondFailed(l)
		}
		update.oldRaw = l.raw
	}
	return nl.storage.update(ctx, update, handleCondFailed)
}

// numLiveNodes is used to populate a metric that tracks the number of live
// nodes in the cluster. Returns 0 if this node is not itself live, to avoid
// reporting potentially inaccurate data.
// We export this metric from every live node rather than a single particular
// live node because liveness information is gossiped and thus may be stale.
// That staleness could result in no nodes reporting the metric or multiple
// nodes reporting the metric, so it's simplest to just have all live nodes
// report it.
func (nl *NodeLiveness) numLiveNodes() int64 {

	selfID := nl.cache.selfID()
	// if our node id isn't set, don't return a count
	if selfID == 0 {
		return 0
	}

	var liveNodes int64
	for n, v := range nl.ScanNodeVitalityFromCache() {
		if v.IsAlive() {
			liveNodes++
		}
		// If this node isn't live, we don't want to report its view of node liveness
		// because it's more likely to be inaccurate than the view of a live node.
		if n == selfID && !v.IsAlive() {
			return 0
		}
	}
	return liveNodes
}

// GetNodeCount returns a count of the number of nodes in the cluster,
// including dead nodes, but excluding decommissioning or decommissioned nodes.
// TODO(baptist): remove this method. There are better alternatives.
func (nl *NodeLiveness) GetNodeCount() int {
	nl.cache.mu.RLock()
	defer nl.cache.mu.RUnlock()
	var count int
	for _, l := range nl.cache.mu.nodes {
		if l.Membership.Active() {
			count++
		}
	}
	return count
}

// GetNodeCountWithOverrides returns a count of the number of nodes in the cluster,
// including dead nodes, but excluding decommissioning or decommissioned nodes,
// using the provided set of liveness overrides.
// TODO(baptist): remove this method. There are better alternatives.
func (nl *NodeLiveness) GetNodeCountWithOverrides(
	overrides map[roachpb.NodeID]livenesspb.NodeLivenessStatus,
) int {
	nl.cache.mu.RLock()
	defer nl.cache.mu.RUnlock()
	var count int
	for _, l := range nl.cache.mu.nodes {
		if l.Membership.Active() {
			if overrideStatus, ok := overrides[l.NodeID]; !ok ||
				(overrideStatus != livenesspb.NodeLivenessStatus_DECOMMISSIONING &&
					overrideStatus != livenesspb.NodeLivenessStatus_DECOMMISSIONED) {
				count++
			}
		}
	}
	return count
}

// TestingSetDrainingInternal is a testing helper to set the internal draining
// state for a NodeLiveness instance.
func (nl *NodeLiveness) TestingSetDrainingInternal(
	ctx context.Context, liveness Record, drain bool,
) error {
	return nl.setDrainingInternal(ctx, liveness, drain, nil /* reporter */)
}

// TestingSetDecommissioningInternal is a testing helper to set the internal
// decommissioning state for a NodeLiveness instance.
func (nl *NodeLiveness) TestingSetDecommissioningInternal(
	ctx context.Context, oldLivenessRec Record, targetStatus livenesspb.MembershipStatus,
) (changeCommitted bool, err error) {
	return nl.setMembershipStatusInternal(ctx, oldLivenessRec, targetStatus)
}

// TestingMaybeUpdate replaces the liveness (if it appears newer) and invokes
// the registered callbacks if the node became live in the process. For testing.
func (nl *NodeLiveness) TestingMaybeUpdate(ctx context.Context, newRec Record) {
	nl.cache.maybeUpdate(ctx, newRec)
}
