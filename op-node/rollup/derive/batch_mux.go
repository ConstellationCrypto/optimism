package derive

import (
	"context"
	"fmt"
	"slices"

	"github.com/ethereum-optimism/optimism/op-core/forks"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/log"
)

// BatchMux multiplexes between different batch stages.
// Stages are swapped on demand during Reset calls, or explicitly with Transform.
// It currently chooses the BatchQueue pre-Holocene and the BatchStage post-Holocene.
type batchMuxL2Source interface {
	SafeBlockFetcher
	L2BlockRefByLabel(context.Context, eth.BlockLabel) (eth.L2BlockRef, error)
}

type BatchMux struct {
	log  log.Logger
	cfg  *rollup.Config
	prev NextBatchProvider
	l2   batchMuxL2Source

	// embedded active stage
	SingularBatchProvider
}

var _ SingularBatchProvider = (*BatchMux)(nil)

// NewBatchMux returns an uninitialized BatchMux. Reset has to be called before
// calling other methods, to activate the right stage for a given L1 origin.
func NewBatchMux(lgr log.Logger, cfg *rollup.Config, prev NextBatchProvider, l2 SafeBlockFetcher) *BatchMux {
	var l2Source batchMuxL2Source
	if l2, ok := l2.(batchMuxL2Source); ok {
		l2Source = l2
	}
	return &BatchMux{log: lgr, cfg: cfg, prev: prev, l2: l2Source}
}

func (b *BatchMux) holoceneStageActive(ctx context.Context, base eth.L1BlockRef) bool {
	if b.cfg.IsHolocene(base.Time) {
		return true
	}
	// After a Holocene reset, the pipeline origin can rewind to a pre-Holocene L1 block
	// (channel-timeout walkback) while the safe head already expects post-Holocene batches.
	if b.l2 == nil {
		return false
	}
	safe, err := b.l2.L2BlockRefByLabel(ctx, eth.Safe)
	if err != nil {
		b.log.Debug("BatchMux: unable to fetch safe head for stage selection", "err", err)
		return false
	}
	nextL2Time := safe.Time + b.cfg.BlockTime
	if b.cfg.IsHolocene(nextL2Time) {
		b.log.Info("BatchMux: safe head at Holocene boundary, using Holocene stage",
			"safe", safe, "next_l2_time", nextL2Time, "origin", base)
		return true
	}
	return false
}

func (b *BatchMux) Reset(ctx context.Context, base eth.L1BlockRef, sysCfg eth.SystemConfig) error {
	// TODO(12490): change to a switch over b.cfg.ActiveFork(base.Time)
	switch {
	default:
		if _, ok := b.SingularBatchProvider.(*BatchQueue); !ok {
			b.log.Info("BatchMux: activating pre-Holocene stage during reset", "origin", base)
			b.SingularBatchProvider = NewBatchQueue(b.log, b.cfg, b.prev, b.l2)
		}
	case b.holoceneStageActive(ctx, base):
		if _, ok := b.SingularBatchProvider.(*BatchStage); !ok {
			b.log.Info("BatchMux: activating Holocene stage during reset", "origin", base)
			b.SingularBatchProvider = NewBatchStage(b.log, b.cfg, b.prev, b.l2)
		}
	}
	return b.SingularBatchProvider.Reset(ctx, base, sysCfg)
}

func (b *BatchMux) Transform(f forks.Name) {
	switch f {
	case forks.Holocene:
		b.TransformHolocene()
	}
}

func (b *BatchMux) TransformHolocene() {
	switch bp := b.SingularBatchProvider.(type) {
	case *BatchQueue:
		b.log.Info("BatchMux: transforming to Holocene stage")
		bs := NewBatchStage(b.log, b.cfg, b.prev, b.l2)
		// Even though any ongoing span batch or queued batches are dropped at Holocene activation, the
		// post-Holocene batch stage still needs access to the collected l1Blocks pre-Holocene because
		// the first Holocene channel will contain pre-Holocene batches.
		bs.l1Blocks = slices.Clone(bp.l1Blocks)
		bs.origin = bp.origin
		b.SingularBatchProvider = bs
	case *BatchStage:
		// Even if the pipeline is Reset to the activation block, the previous origin will be the
		// same, so transformStages isn't called.
		panic(fmt.Sprintf("Holocene BatchStage already active, old origin: %v", bp.Origin()))
	default:
		panic(fmt.Sprintf("unknown batch stage type: %T", bp))
	}
}
