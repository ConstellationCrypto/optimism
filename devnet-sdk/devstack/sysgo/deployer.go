package sysgo

import (
	"context"
	"encoding/json"

	"golang.org/x/exp/maps"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/devnet-sdk/devstack/shim"
	"github.com/ethereum-optimism/optimism/devnet-sdk/devstack/stack"
	"github.com/ethereum-optimism/optimism/op-chain-ops/devkeys"
	"github.com/ethereum-optimism/optimism/op-chain-ops/genesis"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer/artifacts"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer/inspect"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer/pipeline"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer/standard"
	"github.com/ethereum-optimism/optimism/op-deployer/pkg/deployer/state"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-supervisor/supervisor/backend/depset"
	supervisortypes "github.com/ethereum-optimism/optimism/op-supervisor/supervisor/types"
)

func WithDeployer(l1ID stack.L1NetworkID, superchainID stack.SuperchainID,
	clusterID stack.ClusterID, l2IDs []stack.L2NetworkID, res ContractPaths) stack.Option {
	return func(setup *stack.Setup) {
		orch := setup.Orchestrator.(*Orchestrator)
		wb := &worldBuilder{
			logger:  setup.Log,
			require: setup.Require,
			keys:    orch.keys,
			intent:  nil,
			l1Chain: eth.ChainID{},
		}
		wb.Init(l1ID.ChainID, res.FoundryArtifacts)
		for _, l2ID := range l2IDs {
			wb.AddL2(l2ID.ChainID)
		}
		wb.Build()

		l1Net := &L1Network{
			genesis:   wb.outL1Genesis,
			blockTime: 6,
		}
		orch.l1Nets.Set(l1ID, l1Net)

		sysL1Net := shim.NewL1Network(shim.L1NetworkConfig{
			NetworkConfig: shim.NetworkConfig{
				CommonConfig: shim.CommonConfigFromSetup(setup),
				ChainConfig:  wb.outL1Genesis.Config,
			},
			ID: l1ID,
		})
		setup.System.AddL1Network(sysL1Net)

		sysSuperchain := shim.NewSuperchain(shim.SuperchainConfig{
			CommonConfig: shim.CommonConfigFromSetup(setup),
			ID:           superchainID,
			Deployment:   wb.outSuperchainDeployment,
		})
		setup.System.AddSuperchain(sysSuperchain)

		sysCluster := shim.NewCluster(shim.ClusterConfig{
			CommonConfig:  shim.CommonConfigFromSetup(setup),
			ID:            clusterID,
			DependencySet: wb.outDepset,
		})
		setup.System.AddCluster(sysCluster)

		for _, l2ID := range l2IDs {
			l2Genesis, ok := wb.outL2Genesis[l2ID.ChainID]
			setup.Require.True(ok, "L2 genesis must exist")
			l2RollupCfg, ok := wb.outL2RollupCfg[l2ID.ChainID]
			setup.Require.True(ok, "L2 rollup config must exist")
			l2Dep, ok := wb.outL2Deployment[l2ID.ChainID]
			setup.Require.True(ok, "L2 deployment must exist")

			l2Net := &L2Network{
				genesis:   l2Genesis,
				rollupCfg: l2RollupCfg,
			}
			orch.l2Nets.Set(l2ID, l2Net)

			sysL2Net := shim.NewL2Network(shim.L2NetworkConfig{
				NetworkConfig: shim.NetworkConfig{
					CommonConfig: shim.CommonConfigFromSetup(setup),
					ChainConfig:  l2Genesis.Config,
				},
				ID:           l2ID,
				RollupConfig: l2RollupCfg,
				Deployment:   l2Dep,
				Keys:         &keyring{keys: orch.keys, require: setup.Require},
				Superchain:   nil,
				L1:           sysL1Net,
				Cluster:      nil,
			})
			setup.System.AddL2Network(sysL2Net)
		}
	}
}

type worldBuilder struct {
	logger  log.Logger
	require *require.Assertions
	keys    devkeys.Keys

	intent  *state.Intent
	l1Chain eth.ChainID

	output                  *state.State
	outL1Genesis            *core.Genesis
	outL2Genesis            map[eth.ChainID]*core.Genesis
	outL2RollupCfg          map[eth.ChainID]*rollup.Config
	outL2Deployment         map[eth.ChainID]*L2Deployment
	outDepset               *depset.StaticConfigDependencySet
	outSuperchainDeployment *SuperchainDeployment
}

func (wb *worldBuilder) addrFor(key devkeys.Key) common.Address {
	addr, err := wb.keys.Address(key)
	wb.require.NoError(err)
	return addr
}

func (wb *worldBuilder) Init(l1ChainID eth.ChainID, artifactsPath string) {
	wb.require.True(l1ChainID.IsUint64(), "L1 chain ID must be 64 bits")
	wb.require.Nil(wb.intent, "already have a L1 intent")
	wb.l1Chain = l1ChainID

	contractArtifacts, err := artifacts.NewFileLocator(artifactsPath)
	wb.require.NoError(err)

	l1ChID := l1ChainID.ToBig()
	wb.intent = &state.Intent{
		ConfigType: state.IntentTypeCustom,
		L1ChainID:  l1ChainID.Uint64(),
		SuperchainRoles: &state.SuperchainRoles{
			ProxyAdminOwner:       wb.addrFor(devkeys.L1ProxyAdminOwnerRole.Key(l1ChID)),
			ProtocolVersionsOwner: wb.addrFor(devkeys.SuperchainDeployerKey.Key(l1ChID)),
			Guardian:              wb.addrFor(devkeys.SuperchainConfigGuardianKey.Key(l1ChID)),
		},
		FundDevAccounts:    false,
		L1ContractsLocator: contractArtifacts,
		L2ContractsLocator: contractArtifacts,
		Chains:             []*state.ChainIntent{},
	}
}

func (wb *worldBuilder) AddL2(id eth.ChainID) {
	wb.require.True(id.IsUint64(), "L2 chain ID must be 64 bits")
	wb.require.NotNil(wb.intent, "must have L1 intent first")

	for _, v := range wb.intent.Chains {
		wb.require.NotEqual(v.ID, id.Bytes32(), "L2 must not already be added")
	}
	chID := id.ToBig()
	chIntent := &state.ChainIntent{
		ID:                         id.Bytes32(),
		BaseFeeVaultRecipient:      wb.addrFor(devkeys.BaseFeeVaultRecipientRole.Key(chID)),
		L1FeeVaultRecipient:        wb.addrFor(devkeys.L1FeeVaultRecipientRole.Key(chID)),
		SequencerFeeVaultRecipient: wb.addrFor(devkeys.SequencerFeeVaultRecipientRole.Key(chID)),
		Eip1559DenominatorCanyon:   standard.Eip1559DenominatorCanyon,
		Eip1559Denominator:         standard.Eip1559Denominator,
		Eip1559Elasticity:          standard.Eip1559Elasticity,
		Roles: state.ChainRoles{
			L1ProxyAdminOwner: wb.addrFor(devkeys.L2ProxyAdminOwnerRole.Key(chID)),
			L2ProxyAdminOwner: wb.addrFor(devkeys.L2ProxyAdminOwnerRole.Key(chID)),
			SystemConfigOwner: wb.addrFor(devkeys.SystemConfigOwner.Key(chID)),
			UnsafeBlockSigner: wb.addrFor(devkeys.SequencerP2PRole.Key(chID)),
			Batcher:           wb.addrFor(devkeys.BatcherRole.Key(chID)),
			Proposer:          wb.addrFor(devkeys.ProposerRole.Key(chID)),
			Challenger:        wb.addrFor(devkeys.ChallengerRole.Key(chID)),
		},
	}
	wb.intent.Chains = append(wb.intent.Chains, chIntent)
}

func (wb *worldBuilder) Overrides(name rollup.ForkName) {
	upgradeSchedule := new(genesis.UpgradeScheduleDeployConfig)
	upgradeSchedule.ActivateForkAtGenesis(name)
	upgradeOverridesJSON, err := json.Marshal(upgradeSchedule)
	wb.require.NoError(err, "failed to marshal upgrade schedule")

	var upgradeOverrides map[string]any
	err = json.Unmarshal(upgradeOverridesJSON, &upgradeOverrides)
	wb.require.NoError(err, "failed to unmarshal upgrade schedule")

	baseUpgradeSchedule := map[string]any{
		"l2GenesisRegolithTimeOffset": nil,
		"l2GenesisCanyonTimeOffset":   nil,
		"l2GenesisDeltaTimeOffset":    nil,
		"l2GenesisEcotoneTimeOffset":  nil,
		"l2GenesisFjordTimeOffset":    nil,
		"l2GenesisGraniteTimeOffset":  nil,
		"l2GenesisHoloceneTimeOffset": nil,
		"l2GenesisIsthmusTimeOffset":  nil,
	}
	// ensure all overrides are there
	maps.Copy(baseUpgradeSchedule, upgradeOverrides)

	// copy into intent
	maps.Copy(wb.intent.GlobalDeployOverrides, baseUpgradeSchedule)
}

func (wb *worldBuilder) buildL1Genesis() {
	// TODO can't determine this from the L1 deployment... so we fish it up from an arbitrary L2 chain, if it's even valid
	timestamp := wb.output.Chains[0].StartBlock.Time
	l1Genesis, err := genesis.NewL1GenesisMinimal(&genesis.DevL1DeployConfigMinimal{
		DevL1DeployConfig: genesis.DevL1DeployConfig{
			L1BlockTime:                 6,
			L1GenesisBlockTimestamp:     timestamp,
			L1GenesisBlockNonce:         0,
			L1GenesisBlockGasLimit:      0,
			L1GenesisBlockDifficulty:    nil,
			L1GenesisBlockMixHash:       common.Hash{},
			L1GenesisBlockCoinbase:      common.Address{},
			L1GenesisBlockNumber:        0,
			L1GenesisBlockGasUsed:       0,
			L1GenesisBlockParentHash:    common.Hash{},
			L1GenesisBlockBaseFeePerGas: nil,
			L1GenesisBlockExcessBlobGas: nil,
			L1GenesisBlockBlobGasUsed:   nil,
		},
		L1ChainID:          eth.ChainID{},
		L1PragueTimeOffset: nil,
	})
	wb.require.NoError(err, "must build L1 genesis")
	l1Genesis.Alloc = wb.output.L1StateDump.Data.Accounts
	wb.outL1Genesis = l1Genesis
}

func (wb *worldBuilder) buildL2Genesis() {
	wb.outL2Genesis = make(map[eth.ChainID]*core.Genesis)
	wb.outL2RollupCfg = make(map[eth.ChainID]*rollup.Config)
	for _, ch := range wb.output.Chains {
		l2Genesis, l2RollupCfg, err := inspect.GenesisAndRollup(wb.output, ch.ID)
		wb.require.NoError(err, "need L2 genesis and rollup")
		id := eth.ChainIDFromBytes32(ch.ID)
		wb.outL2Genesis[id] = l2Genesis
		wb.outL2RollupCfg[id] = l2RollupCfg
	}
}

func (wb *worldBuilder) buildDepSet() {
	depSetContents := make(map[eth.ChainID]*depset.StaticConfigDependency)
	for _, ch := range wb.output.Chains {
		chainID := eth.ChainIDFromBytes32(ch.ID)
		index, err := chainID.ToUInt32()
		wb.require.NoError(err)
		depSetContents[chainID] = &depset.StaticConfigDependency{
			ChainIndex:     supervisortypes.ChainIndex(index),
			ActivationTime: 0,
			HistoryMinTime: 0,
		}
	}
	staticDepSet, err := depset.NewStaticConfigDependencySet(depSetContents)
	wb.require.NoError(err)
	wb.outDepset = staticDepSet
}

func (wb *worldBuilder) buildL2DeploymentOutputs() {
	wb.outL2Deployment = make(map[eth.ChainID]*L2Deployment)
	for _, ch := range wb.output.Chains {
		chainID := eth.ChainIDFromBytes32(ch.ID)
		wb.outL2Deployment[chainID] = &L2Deployment{
			systemConfigProxyAddr:   ch.SystemConfigProxyAddress,
			disputeGameFactoryProxy: ch.DisputeGameFactoryProxyAddress,
		}
	}
	wb.outSuperchainDeployment = &SuperchainDeployment{
		protocolVersionsAddr: wb.output.SuperchainDeployment.ProtocolVersionsProxyAddress,
		superchainConfigAddr: wb.output.SuperchainDeployment.SuperchainConfigProxyAddress,
	}
}

func (wb *worldBuilder) Build() {
	st := &state.State{
		Version: 1,
	}

	// Work-around of op-deployer design issue.
	// We use the same deployer key for all L2 chains we deploy here.
	deployerKey, err := wb.keys.Secret(devkeys.DeployerRole.Key(wb.l1Chain.ToBig()))
	wb.require.NoError(err, "need deployer key")

	pipelineOpts := deployer.ApplyPipelineOpts{
		DeploymentTarget:   deployer.DeploymentTargetGenesis,
		L1RPCUrl:           "",
		DeployerPrivateKey: deployerKey,
		Intent:             wb.intent,
		State:              st,
		Logger:             wb.logger,
		StateWriter:        pipeline.NoopStateWriter(),
	}
	err = deployer.ApplyPipeline(context.Background(), pipelineOpts)
	wb.require.NoError(err)

	wb.buildL1Genesis()
	wb.buildL2Genesis()
	wb.buildL2DeploymentOutputs()
	wb.buildDepSet()
}

// WriteState is a callback used by deployer.ApplyPipeline to write the output
func (wb *worldBuilder) WriteState(st *state.State) error {
	wb.output = st
	return nil
}
