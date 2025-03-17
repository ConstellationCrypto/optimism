package sysgo

import (
	"context"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum-optimism/optimism/devnet-sdk/devstack/shim"
	"github.com/ethereum-optimism/optimism/devnet-sdk/devstack/stack"
	"github.com/ethereum-optimism/optimism/op-chain-ops/devkeys"
	chconfig "github.com/ethereum-optimism/optimism/op-challenger/config"
	"github.com/ethereum-optimism/optimism/op-challenger/game"
	chmetrics "github.com/ethereum-optimism/optimism/op-challenger/metrics"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
)

type L2Challenger struct {
	service *game.Service
	// no challenger RPC endpoint, yet
}

func WithChallenger(challengerID stack.L2ChallengerID,
	l1ELID stack.L1ELNodeID, l1CLID stack.L1CLNodeID,
	l2CLID stack.L2CLNodeID, l2ELID stack.L2ELNodeID, supervisorID stack.SupervisorID) stack.Option {
	return func(setup *stack.Setup) {
		orch := setup.Orchestrator.(*Orchestrator)
		setup.Require.False(orch.challengers.Has(challengerID), "challenger must not already exist")

		challengerSecret, err := orch.keys.Secret(devkeys.ChallengerRole.Key(challengerID.ChainID.ToBig()))
		setup.Require.NoError(err)

		logger := setup.Log.New("id", challengerID)
		logger.Info("Challenger key acquired", "addr", crypto.PubkeyToAddress(challengerSecret.PublicKey))

		l2ID := setup.System.L2NetworkID(challengerID.ChainID)
		l2Net := setup.System.L2Network(l2ID).(stack.ExtensibleL2Network)
		disputeGameFactoryAddr := l2Net.Deployment().DisputeGameFactoryProxyAddr()

		l1EL, ok := orch.l1ELs.Get(l1ELID)
		setup.Require.True(ok)
		l1CL, ok := orch.l1CLs.Get(l1CLID)
		setup.Require.True(ok)
		l2CL, ok := orch.l2CLs.Get(l2CLID)
		setup.Require.True(ok)
		l2EL, ok := orch.l2ELs.Get(l2ELID)
		setup.Require.True(ok)
		super, ok := orch.supervisors.Get(supervisorID)
		setup.Require.True(ok)

		supervisorEndpoint := super.userRPC
		l1Endpoint := l1EL.userRPC
		l1Beacon := l1CL.beaconHTTPAddr
		rollupEndpoint := l2CL.rpc
		l2ELEndpoint := l2EL.userRPC

		chDir := orch.t.TempDir()

		cfg := chconfig.NewConfig(disputeGameFactoryAddr, l1Endpoint, l1Beacon, rollupEndpoint, l2ELEndpoint, chDir)
		cfg.Cannon.L2Custom = true
		// The devnet can't set the absolute prestate output root because the contracts are deployed in L1 genesis
		// before the L2 genesis is known.
		cfg.AllowInvalidPrestate = true
		cfg.TxMgrConfig.NumConfirmations = 1
		cfg.TxMgrConfig.ReceiptQueryInterval = 1 * time.Second
		if cfg.MaxConcurrency > 4 {
			// Limit concurrency to something more reasonable when there are also multiple tests executing in parallel
			cfg.MaxConcurrency = 4
		}
		cfg.MetricsConfig = metrics.CLIConfig{Enabled: false}

		// TODO: need equivalent of challenger-helper applyCannonConfig func, but not attached to e2e sys.
		// Also need to fix the rollup-config/chain-config json writers in there,
		// to not overwrite the same file for every config (?)

		// TODO: this needs to be interop-dependent
		cfg.SupervisorRPC = supervisorEndpoint

		setup.Require.NotEmpty(cfg.TxMgrConfig.PrivateKey, "Missing private key for TxMgrConfig")
		setup.Require.NoError(cfg.Check(), "op-challenger config should be valid")

		// TODO: these checks should be helper func, not copied from challenger helper code
		if cfg.Cannon.VmBin != "" {
			_, err := os.Stat(cfg.Cannon.VmBin)
			setup.Require.NoError(err, "cannon VM should be built. Make sure you've run make cannon-prestate")
		}
		if cfg.Cannon.Server != "" {
			_, err := os.Stat(cfg.Cannon.Server)
			setup.Require.NoError(err, "op-program should be built. Make sure you've run make cannon-prestate")
		}
		if cfg.CannonAbsolutePreState != "" {
			_, err := os.Stat(cfg.CannonAbsolutePreState)
			setup.Require.NoError(err, "cannon pre-state should be built. Make sure you've run make cannon-prestate")
		}
		if cfg.PollInterval == 0 {
			cfg.PollInterval = time.Second
		}

		setup.Require.NoError(cfg.Check(), "challenger config must pass checks")
		chl, err := game.NewService(setup.Ctx, logger, &cfg, chmetrics.NoopMetrics)
		setup.Require.NoError(err, "must init challenger")
		setup.Require.NoError(chl.Start(setup.Ctx), "must start challenger")

		orch.t.Cleanup(func() {
			ctx, cancel := context.WithCancel(setup.Ctx)
			cancel() // force-quit
			logger.Info("Closing challenger")
			_ = chl.Stop(ctx)
			logger.Info("Closed challenger")
		})

		c := &L2Challenger{
			service: chl,
		}
		orch.challengers.Set(challengerID, c)

		cFrontend := shim.NewL2Challenger(shim.L2ChallengerConfig{
			CommonConfig: shim.CommonConfigFromSetup(setup),
			ID:           challengerID,
		})
		l2Net.AddL2Challenger(cFrontend)
	}
}
