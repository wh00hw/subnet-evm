// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Implements solidity tests.
package warp

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanche-network-runner/rpcpb"
	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/ethclient"
	"github.com/ava-labs/subnet-evm/interfaces"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/plugin/evm"
	"github.com/ava-labs/subnet-evm/predicate"
	"github.com/ava-labs/subnet-evm/tests/utils"
	"github.com/ava-labs/subnet-evm/tests/utils/runner"
	warpBackend "github.com/ava-labs/subnet-evm/warp"
	"github.com/ava-labs/subnet-evm/warp/aggregator"
	"github.com/ava-labs/subnet-evm/x/warp"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
)

const fundedKeyStr = "56289e99c94b6912bfc12adc093c9b51124f0dc54ac7a766b2bc5ccf558d8027"

var (
	config                 = runner.NewDefaultANRConfig()
	manager                = runner.NewNetworkManager(config)
	warpChainConfigPath    string
	testPayload            = []byte{1, 2, 3}
	nodesPerSubnet         = 5
	subnetToSubnetWarpTest *warpTest
)

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "subnet-evm warp e2e test")
}

func toWebsocketURI(uri string, blockchainID string) string {
	return fmt.Sprintf("ws://%s/ext/bc/%s/ws", strings.TrimPrefix(uri, "http://"), blockchainID)
}

// BeforeSuite starts the default network and adds 10 new nodes as validators with BLS keys
// registered on the P-Chain.
// Adds two disjoint sets of 5 of the new validator nodes to validate two new subnets with a
// a single Subnet-EVM blockchain.
var _ = ginkgo.BeforeSuite(func() {
	ctx := context.Background()
	require := require.New(ginkgo.GinkgoT())

	// Name 10 new validators (which should have BLS key registered)
	subnetANodeNames := make([]string, 0)
	subnetBNodeNames := make([]string, 0)
	for i := 0; i < nodesPerSubnet; i++ {
		subnetANodeNames = append(subnetANodeNames, fmt.Sprintf("node%d-subnetA-bls", i))
		subnetBNodeNames = append(subnetBNodeNames, fmt.Sprintf("node%d-subnetB-bls", i))
	}

	f, err := os.CreateTemp(os.TempDir(), "config.json")
	gomega.Expect(err).Should(gomega.BeNil())
	_, err = f.Write([]byte(`{"warp-api-enabled": true}`))
	gomega.Expect(err).Should(gomega.BeNil())
	warpChainConfigPath = f.Name()

	// Construct the network using the avalanche-network-runner
	_, err = manager.StartDefaultNetwork(ctx)
	gomega.Expect(err).Should(gomega.BeNil())
	err = manager.SetupNetwork(
		ctx,
		config.AvalancheGoExecPath,
		[]*rpcpb.BlockchainSpec{
			{
				VmName:      evm.IDStr,
				Genesis:     "./tests/precompile/genesis/warp.json",
				ChainConfig: warpChainConfigPath,
				SubnetSpec: &rpcpb.SubnetSpec{
					SubnetConfig: "",
					Participants: subnetANodeNames,
				},
			},
			{
				VmName:      evm.IDStr,
				Genesis:     "./tests/precompile/genesis/warp.json",
				ChainConfig: warpChainConfigPath,
				SubnetSpec: &rpcpb.SubnetSpec{
					SubnetConfig: "",
					Participants: subnetBNodeNames,
				},
			},
		},
	)
	gomega.Expect(err).Should(gomega.BeNil())

	fundedKey, err := crypto.HexToECDSA(fundedKeyStr)
	require.NoError(err)
	subnets := manager.GetSubnets()
	subnetIDA := subnets[0]
	subnetIDB := subnets[1]
	subnetA, ok := manager.GetSubnet(subnetIDA)
	gomega.Expect(ok).Should(gomega.BeTrue())
	subnetB, ok := manager.GetSubnet(subnetIDB)
	gomega.Expect(ok).Should(gomega.BeTrue())

	subnetToSubnetWarpTest = newWarpTest(ctx, subnetA, fundedKey, subnetB, fundedKey)
})

var _ = ginkgo.AfterSuite(func() {
	gomega.Expect(manager).ShouldNot(gomega.BeNil())
	gomega.Expect(manager.TeardownNetwork()).Should(gomega.BeNil())
	gomega.Expect(os.Remove(warpChainConfigPath)).Should(gomega.BeNil())
	// TODO: bootstrap an additional node to ensure that we can bootstrap the test data correctly
})

type warpTest struct {
	// network-wide fields set in the constructor
	networkID uint32

	// subnetA fields set in the constructor
	subnetA              *runner.Subnet
	subnetAURIs          []string
	subnetAClients       []ethclient.Client
	subnetAFundedKey     *ecdsa.PrivateKey
	subnetAFundedAddress common.Address
	chainIDA             *big.Int
	chainASigner         types.Signer

	// subnetB fields set in the constructor
	subnetB              *runner.Subnet
	subnetBURIs          []string
	subnetBClients       []ethclient.Client
	subnetBFundedKey     *ecdsa.PrivateKey
	subnetBFundedAddress common.Address
	chainIDB             *big.Int
	chainBSigner         types.Signer

	// Fields set throughout test execution
	blockID                     ids.ID
	blockPayload                *payload.Hash
	blockPayloadUnsignedMessage *avalancheWarp.UnsignedMessage
	blockPayloadSignedMessage   *avalancheWarp.Message

	addressedCallUnsignedMessage *avalancheWarp.UnsignedMessage
	addressedCallSignedMessage   *avalancheWarp.Message
}

// aggregate signatures
// deliver
// verify block hash
// send message using HardHat

func newWarpTest(ctx context.Context, subnetA *runner.Subnet, subnetAFundedKey *ecdsa.PrivateKey, subnetB *runner.Subnet, subnetBFundedKey *ecdsa.PrivateKey) *warpTest {
	require := require.New(ginkgo.GinkgoT())

	warpTest := &warpTest{
		subnetA:              subnetA,
		subnetAURIs:          subnetA.ValidatorURIs,
		subnetB:              subnetB,
		subnetBURIs:          subnetB.ValidatorURIs,
		subnetAFundedKey:     subnetAFundedKey,
		subnetAFundedAddress: crypto.PubkeyToAddress(subnetAFundedKey.PublicKey),
		subnetBFundedKey:     subnetBFundedKey,
		subnetBFundedAddress: crypto.PubkeyToAddress(subnetBFundedKey.PublicKey),
	}
	infoClient := info.NewClient(subnetA.ValidatorURIs[0])
	networkID, err := infoClient.GetNetworkID(ctx)
	require.NoError(err)
	warpTest.networkID = networkID

	for _, uri := range subnetA.ValidatorURIs {
		wsURI := toWebsocketURI(uri, subnetA.BlockchainID.String())
		log.Info("Creating ethclient for blockchain A", "blockchainID", subnetA.BlockchainID)
		client, err := ethclient.Dial(wsURI)
		require.NoError(err)
		warpTest.subnetAClients = append(warpTest.subnetAClients, client)
	}

	for _, uri := range subnetB.ValidatorURIs {
		wsURI := toWebsocketURI(uri, subnetB.BlockchainID.String())
		log.Info("Creating ethclient for blockchain B", "blockchainID", subnetB.BlockchainID)
		client, err := ethclient.Dial(wsURI)
		require.NoError(err)
		warpTest.subnetBClients = append(warpTest.subnetBClients, client)
	}

	clientA := warpTest.subnetAClients[0]
	chainIDA, err := clientA.ChainID(ctx)
	require.NoError(err)
	warpTest.chainIDA = chainIDA
	warpTest.chainASigner = types.LatestSignerForChainID(chainIDA)

	clientB := warpTest.subnetBClients[0]
	chainIDB, err := clientB.ChainID(ctx)
	require.NoError(err)
	// Issue transactions to activate ProposerVM on the receiving chain
	require.NoError(utils.IssueTxsToActivateProposerVMFork(ctx, chainIDB, subnetBFundedKey, clientB))
	warpTest.chainIDB = chainIDB
	warpTest.chainBSigner = types.LatestSignerForChainID(chainIDB)

	return warpTest
}

func (w *warpTest) sendMessageFromSubnetA() {
	ctx := context.Background()
	require := require.New(ginkgo.GinkgoT())

	client := w.subnetAClients[0]
	log.Info("Subscribing to new heads")
	newHeads := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, newHeads)
	require.NoError(err)
	defer sub.Unsubscribe()

	startingNonce, err := client.NonceAt(ctx, w.subnetAFundedAddress, nil)
	require.NoError(err)

	packedInput, err := warp.PackSendWarpMessage(testPayload)
	require.NoError(err)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   w.chainIDA,
		Nonce:     startingNonce,
		To:        &warp.Module.Address,
		Gas:       200_000,
		GasFeeCap: big.NewInt(225 * params.GWei),
		GasTipCap: big.NewInt(params.GWei),
		Value:     common.Big0,
		Data:      packedInput,
	})
	signedTx, err := types.SignTx(tx, w.chainASigner, w.subnetAFundedKey)
	require.NoError(err)
	log.Info("Sending sendWarpMessage transaction", "txHash", signedTx.Hash())
	err = client.SendTransaction(ctx, signedTx)
	require.NoError(err)

	log.Info("Waiting for new block confirmation")
	newHead := <-newHeads
	blockHash := newHead.Hash()

	log.Info("Constructing warp block hash unsigned message", "blockHash", blockHash)
	w.blockID = ids.ID(blockHash) // Set blockID to construct a warp message containing a block hash payload later
	w.blockPayload, err = payload.NewHash(w.blockID)
	require.NoError(err)
	w.blockPayloadUnsignedMessage, err = avalancheWarp.NewUnsignedMessage(w.networkID, w.subnetA.BlockchainID, w.blockPayload.Bytes())
	require.NoError(err)

	log.Info("Fetching relevant warp logs from the newly produced block")
	logs, err := client.FilterLogs(ctx, interfaces.FilterQuery{
		BlockHash: &blockHash,
		Addresses: []common.Address{warp.Module.Address},
	})
	gomega.Expect(err).Should(gomega.BeNil())
	gomega.Expect(len(logs)).Should(gomega.Equal(1))

	// Check for relevant warp log from subscription and ensure that it matches
	// the log extracted from the last block.
	txLog := logs[0]
	log.Info("Parsing logData as unsigned warp message")
	unsignedMsg, err := warp.UnpackSendWarpEventDataToMessage(txLog.Data)
	gomega.Expect(err).Should(gomega.BeNil())

	// Set local variables for the duration of the test
	w.addressedCallUnsignedMessage = unsignedMsg
	log.Info("Parsed unsignedWarpMsg", "unsignedWarpMessageID", w.addressedCallUnsignedMessage.ID(), "unsignedWarpMessage", w.addressedCallUnsignedMessage)

	// Loop over each client on chain A to ensure they all have time to accept the block.
	// Note: if we did not confirm this here, the next stage could be racy since it assumes every node
	// has accepted the block.
	for i, client := range w.subnetAClients {
		// Loop until each node has advanced to >= the height of the block that emitted the warp log
		for {
			block, err := client.BlockByNumber(ctx, nil)
			gomega.Expect(err).Should(gomega.BeNil())
			if block.NumberU64() >= newHead.Number.Uint64() {
				log.Info("client accepted the block containing SendWarpMessage", "client", i, "height", block.NumberU64())
				break
			}
		}
	}
}

func (w *warpTest) aggregateSignaturesViaAPI() {
	require := require.New(ginkgo.GinkgoT())
	ctx := context.Background()

	warpAPIs := make(map[ids.NodeID]warpBackend.Client, len(w.subnetAURIs))
	for _, uri := range w.subnetAURIs {
		client, err := warpBackend.NewClient(uri, w.subnetA.BlockchainID.String())
		require.NoError(err)

		infoClient := info.NewClient(uri)
		nodeID, _, err := infoClient.GetNodeID(ctx)
		require.NoError(err)
		warpAPIs[nodeID] = client
	}

	pChainClient := platformvm.NewClient(w.subnetAURIs[0])
	pChainHeight, err := pChainClient.GetHeight(ctx)
	require.NoError(err)
	// If the source subnet is the Primary Network, then we only need to aggregate signatures from the receiving
	// subnet's validator set instead of the entire Primary Network.
	var validators map[ids.NodeID]*validators.GetValidatorOutput
	if w.subnetA.SubnetID == constants.PrimaryNetworkID {
		validators, err = pChainClient.GetValidatorsAt(ctx, w.subnetB.SubnetID, pChainHeight)
	} else {
		validators, err = pChainClient.GetValidatorsAt(ctx, w.subnetA.SubnetID, pChainHeight)
	}
	require.NoError(err)
	require.Len(validators, nodesPerSubnet, "expected validators at height to equal number specified to network constructor")

	totalWeight := uint64(0)
	warpValidators := make([]*avalancheWarp.Validator, 0, len(validators))
	for nodeID, validator := range validators {
		warpValidators = append(warpValidators, &avalancheWarp.Validator{
			PublicKey: validator.PublicKey,
			Weight:    validator.Weight,
			NodeIDs:   []ids.NodeID{nodeID},
		})
		totalWeight += validator.Weight
	}

	log.Info("Aggregating signatures from validator set", "numValidators", len(warpValidators), "totalWeight", totalWeight)
	apiSignatureGetter := warpBackend.NewAPIFetcher(warpAPIs)
	signatureResult, err := aggregator.New(apiSignatureGetter, warpValidators, totalWeight).AggregateSignatures(ctx, w.addressedCallUnsignedMessage, 100)
	require.NoError(err)
	require.Equal(signatureResult.SignatureWeight, signatureResult.TotalWeight)
	require.Equal(signatureResult.SignatureWeight, totalWeight)

	w.addressedCallSignedMessage = signatureResult.Message

	signatureResult, err = aggregator.New(apiSignatureGetter, warpValidators, totalWeight).AggregateSignatures(ctx, w.blockPayloadUnsignedMessage, 100)
	require.NoError(err)
	require.Equal(signatureResult.SignatureWeight, signatureResult.TotalWeight)
	require.Equal(signatureResult.SignatureWeight, totalWeight)
	w.blockPayloadSignedMessage = signatureResult.Message

	log.Info("Aggregated signatures for warp messages", "addressedCallMessage", common.Bytes2Hex(w.addressedCallSignedMessage.Bytes()), "blockPayloadMessage", common.Bytes2Hex(w.blockPayloadSignedMessage.Bytes()))
}

func (w *warpTest) aggregateSignatures() {
	require := require.New(ginkgo.GinkgoT())
	ctx := context.Background()

	// Verify that the signature aggregation matches the results of manually constructing the warp message
	client, err := warpBackend.NewClient(w.subnetAURIs[0], w.subnetA.BlockchainID.String())
	require.NoError(err)

	log.Info("Fetching addressed call aggregate signature via p2p API")
	signedWarpMessageBytes, err := client.GetMessageAggregateSignature(ctx, w.addressedCallSignedMessage.ID(), params.WarpQuorumDenominator)
	require.NoError(err)
	require.Equal(w.addressedCallSignedMessage.Bytes(), signedWarpMessageBytes)

	log.Info("Fetching block payload aggregate signature via p2p API")
	signedWarpBlockBytes, err := client.GetBlockAggregateSignature(ctx, w.blockID, params.WarpQuorumDenominator)
	require.NoError(err)
	require.Equal(w.blockPayloadSignedMessage.Bytes(), signedWarpBlockBytes)
}

func (w *warpTest) deliverAddressedCallToSubnetB() {
	require := require.New(ginkgo.GinkgoT())
	ctx := context.Background()

	client := w.subnetBClients[0]
	log.Info("Subscribing to new heads")
	newHeads := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, newHeads)
	require.NoError(err)
	defer sub.Unsubscribe()

	nonce, err := client.NonceAt(ctx, w.subnetBFundedAddress, nil)
	gomega.Expect(err).Should(gomega.BeNil())

	packedInput, err := warp.PackGetVerifiedWarpMessage(0)
	gomega.Expect(err).Should(gomega.BeNil())
	tx := predicate.NewPredicateTx(
		w.chainIDB,
		nonce,
		&warp.Module.Address,
		5_000_000,
		big.NewInt(225*params.GWei),
		big.NewInt(params.GWei),
		common.Big0,
		packedInput,
		types.AccessList{},
		warp.ContractAddress,
		w.addressedCallSignedMessage.Bytes(),
	)
	signedTx, err := types.SignTx(tx, w.chainASigner, w.subnetAFundedKey)
	require.NoError(err)
	txBytes, err := signedTx.MarshalBinary()
	require.NoError(err)
	log.Info("Sending getVerifiedWarpMessage transaction", "txHash", signedTx.Hash(), "txBytes", common.Bytes2Hex(txBytes))
	require.NoError(client.SendTransaction(ctx, signedTx))

	log.Info("Waiting for new block confirmation")
	newHead := <-newHeads
	blockHash := newHead.Hash()
	log.Info("Fetching relevant warp logs and receipts from new block")
	logs, err := client.FilterLogs(ctx, interfaces.FilterQuery{
		BlockHash: &blockHash,
		Addresses: []common.Address{warp.Module.Address},
	})
	require.NoError(err)
	require.Len(logs, 0)
	receipt, err := client.TransactionReceipt(ctx, signedTx.Hash())
	require.NoError(err)
	require.Equal(receipt.Status, types.ReceiptStatusSuccessful)
}

func (w *warpTest) deliverBlockHashPayload() {
	require := require.New(ginkgo.GinkgoT())
	ctx := context.Background()

	client := w.subnetBClients[0]
	log.Info("Subscribing to new heads")
	newHeads := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, newHeads)
	require.NoError(err)
	defer sub.Unsubscribe()

	nonce, err := client.NonceAt(ctx, w.subnetBFundedAddress, nil)
	require.NoError(err)

	packedInput, err := warp.PackGetVerifiedWarpBlockHash(0)
	require.NoError(err)
	tx := predicate.NewPredicateTx(
		w.chainIDB,
		nonce,
		&warp.Module.Address,
		5_000_000,
		big.NewInt(225*params.GWei),
		big.NewInt(params.GWei),
		common.Big0,
		packedInput,
		types.AccessList{},
		warp.ContractAddress,
		w.blockPayloadSignedMessage.Bytes(),
	)
	signedTx, err := types.SignTx(tx, w.chainBSigner, w.subnetBFundedKey)
	require.NoError(err)
	txBytes, err := signedTx.MarshalBinary()
	require.NoError(err)
	log.Info("Sending getVerifiedWarpBlockHash transaction", "txHash", signedTx.Hash(), "txBytes", common.Bytes2Hex(txBytes))
	err = client.SendTransaction(ctx, signedTx)
	require.NoError(err)

	log.Info("Waiting for new block confirmation")
	newHead := <-newHeads
	blockHash := newHead.Hash()
	log.Info("Fetching relevant warp logs and receipts from new block")
	logs, err := client.FilterLogs(ctx, interfaces.FilterQuery{
		BlockHash: &blockHash,
		Addresses: []common.Address{warp.Module.Address},
	})
	require.NoError(err)
	require.Len(logs, 0)
	receipt, err := client.TransactionReceipt(ctx, signedTx.Hash())
	require.NoError(err)
	require.Equal(receipt.Status, types.ReceiptStatusSuccessful)
}

func (w *warpTest) executeHardHatTest() {
	require := require.New(ginkgo.GinkgoT())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client := w.subnetAClients[0]
	log.Info("Subscribing to new heads")
	newHeads := make(chan *types.Header, 10)
	sub, err := client.SubscribeNewHead(ctx, newHeads)
	require.NoError(err)
	defer sub.Unsubscribe()

	rpcURI := toRPCURI(w.subnetAURIs[0], w.subnetA.BlockchainID.String())
	senderAddress := common.HexToAddress("0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC")
	addressedPayload, err := payload.NewAddressedCall(
		senderAddress.Bytes(),
		testPayload,
	)
	require.NoError(err)
	expectedUnsignedMessage, err := avalancheWarp.NewUnsignedMessage(
		1337,
		w.subnetA.BlockchainID,
		addressedPayload.Bytes(),
	)
	require.NoError(err)

	os.Setenv("SENDER_ADDRESS", senderAddress.Hex())
	os.Setenv("SOURCE_CHAIN_ID", "0x"+w.subnetA.BlockchainID.Hex())
	os.Setenv("PAYLOAD", "0x"+common.Bytes2Hex(testPayload))
	os.Setenv("EXPECTED_UNSIGNED_MESSAGE", "0x"+hex.EncodeToString(expectedUnsignedMessage.Bytes()))

	cmdPath := "./contracts"
	// test path is relative to the cmd path
	testPath := "./test/warp.ts"
	utils.RunHardhatTestsCustomURI(ctx, rpcURI, cmdPath, testPath)
}

var _ = ginkgo.Describe("[Warp]", ginkgo.Ordered, func() {
	// Send a transaction to Subnet A to issue a Warp Message to Subnet B
	ginkgo.It("Send Message from A to B", ginkgo.Label("Warp", "SendWarp"), func() {
		subnetToSubnetWarpTest.sendMessageFromSubnetA()
	})

	// Aggregate a Warp Signature by sending an API request to each node requesting its signature and manually
	// constructing a valid Avalanche Warp Message
	ginkgo.It("Aggregate Warp Signature via API", ginkgo.Label("Warp", "ReceiveWarp", "AggregateWarpManually"), func() {
		subnetToSubnetWarpTest.aggregateSignaturesViaAPI()
	})

	// Aggregate signature via the p2p API for the AddressedCall and BlockHash payload Warp Messages
	ginkgo.It("Aggregate AddressedCall and BlockPayload Signatures via Aggregator", ginkgo.Label("Warp", "ReceiveWarp", "AggregatorWarp"), func() {
		subnetToSubnetWarpTest.aggregateSignatures()
	})

	// Verify successful delivery of the Avalanche Warp Message from Chain A to Chain B
	ginkgo.It("Verify Message from A to B", ginkgo.Label("Warp", "VerifyMessage"), func() {
		subnetToSubnetWarpTest.deliverAddressedCallToSubnetB()
	})

	// Verify successful delivery of the Avalanche Warp Block Hash from Chain A to Chain B
	ginkgo.It("Verify Block Hash from A to B", ginkgo.Label("Warp", "VerifyMessage"), func() {
		subnetToSubnetWarpTest.deliverBlockHashPayload()
	})

	ginkgo.It("Send Message from A to B from Hardhat", ginkgo.Label("Warp", "IWarpMessenger", "SendWarpMessage"), func() {
		subnetToSubnetWarpTest.executeHardHatTest()
	})
})

func toRPCURI(uri string, blockchainID string) string {
	return fmt.Sprintf("%s/ext/bc/%s/rpc", uri, blockchainID)
}
