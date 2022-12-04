// (c) 2019-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/lasthyphen/dijetsnodego/ids"
	"github.com/lasthyphen/dijetsnodego/snow/choices"
	"github.com/lasthyphen/dijetsnodego/utils/crypto"
	"github.com/lasthyphen/dijetsnodego/vms/components/chain"
	"github.com/lasthyphen/coreth/core"
	"github.com/lasthyphen/coreth/core/types"
	"github.com/lasthyphen/coreth/trie"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
)

var (
	// testClementineTime is an arbitrary time used to test the VM's behavior when
	// Clementine activates.
	testClementineTime = time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	// testClementineJSON is a modified genesisJSONClementine to include the Clementine
	// upgrade at testClementineTime.
	testClementineJSON string
)

func init() {
	var genesis core.Genesis
	if err := json.Unmarshal([]byte(genesisJSONClementine), &genesis); err != nil {
		panic(err)
	}
	genesis.Config.ClementineBlockTimestamp = big.NewInt(testClementineTime.Unix())
	json, err := json.Marshal(genesis)
	if err != nil {
		panic(err)
	}
	testClementineJSON = string(json)
}

type verifyExtraStateRootConfig struct {
	genesis                string
	blockTime1             time.Time
	blockTime2             time.Time
	expectedExtraStateRoot func(atomicRoot1, atomicRoot2 common.Hash) (common.Hash, common.Hash)
}

// testVerifyExtraState root builds 2 blocks using a vm with [test.genesis].
// First block is built at [blockTime1] and includes an import tx.
// Second block is build at [blockTime2] and includes an export tx.
// After blocks build, [test.expectedExtraStateRoot] is called with the roots
// of the atomic trie at block1 and block2 and the ExtraStateRoot field of
// the blocks are checked against the return value of that function.
func testVerifyExtraStateRoot(t *testing.T, test verifyExtraStateRootConfig) {
	importAmount := uint64(50000000)
	issuer, vm, _, _, _ := GenesisVMWithUTXOs(t, true, test.genesis, "", "", map[ids.ShortID]uint64{
		testShortIDAddrs[0]: importAmount,
	})
	defer func() {
		if err := vm.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	// issue tx for block1
	vm.clock.Set(test.blockTime1)
	importTx, err := vm.newImportTx(vm.ctx.XChainID, testEthAddrs[0], initialBaseFee, []*crypto.PrivateKeySECP256K1R{testKeys[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.issueTx(importTx, true /*=local*/); err != nil {
		t.Fatal(err)
	}

	// build block1
	<-issuer
	blk, err := vm.BuildBlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := blk.Verify(); err != nil {
		t.Fatal(err)
	}
	if status := blk.Status(); status != choices.Processing {
		t.Fatalf("Expected status of built block to be %s, but found %s", choices.Processing, status)
	}
	if err := vm.SetPreference(blk.ID()); err != nil {
		t.Fatal(err)
	}
	if err := blk.Accept(); err != nil {
		t.Fatal(err)
	}
	if status := blk.Status(); status != choices.Accepted {
		t.Fatalf("Expected status of accepted block to be %s, but found %s", choices.Accepted, status)
	}
	if lastAcceptedID, err := vm.LastAccepted(); err != nil {
		t.Fatal(err)
	} else if lastAcceptedID != blk.ID() {
		t.Fatalf("Expected last accepted blockID to be the accepted block: %s, but found %s", blk.ID(), lastAcceptedID)
	}

	// issue tx for block2
	vm.clock.Set(test.blockTime2)
	exportAmount := importAmount / 2
	exportTx, err := vm.newExportTx(vm.ctx.DJTXAssetID, exportAmount, vm.ctx.XChainID, testShortIDAddrs[0], initialBaseFee, []*crypto.PrivateKeySECP256K1R{testKeys[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.issueTx(exportTx, true /*=local*/); err != nil {
		t.Fatal(err)
	}

	// build block2
	<-issuer
	blk2, err := vm.BuildBlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := blk2.Verify(); err != nil {
		t.Fatal(err)
	}
	if status := blk2.Status(); status != choices.Processing {
		t.Fatalf("Expected status of built block to be %s, but found %s", choices.Processing, status)
	}
	if err := blk2.Accept(); err != nil {
		t.Fatal(err)
	}
	if status := blk2.Status(); status != choices.Accepted {
		t.Fatalf("Expected status of accepted block to be %s, but found %s", choices.Accepted, status)
	}
	if lastAcceptedID, err := vm.LastAccepted(); err != nil {
		t.Fatal(err)
	} else if lastAcceptedID != blk2.ID() {
		t.Fatalf("Expected last accepted blockID to be the accepted block: %s, but found %s", blk2.ID(), lastAcceptedID)
	}

	// Check that both atomic transactions were indexed as expected.
	indexedImportTx, status, height, err := vm.getAtomicTx(importTx.ID())
	assert.NoError(t, err)
	assert.Equal(t, Accepted, status)
	assert.Equal(t, uint64(1), height, "expected height of indexed import tx to be 1")
	assert.Equal(t, indexedImportTx.ID(), importTx.ID(), "expected ID of indexed import tx to match original txID")

	indexedExportTx, status, height, err := vm.getAtomicTx(exportTx.ID())
	assert.NoError(t, err)
	assert.Equal(t, Accepted, status)
	assert.Equal(t, uint64(2), height, "expected height of indexed export tx to be 2")
	assert.Equal(t, indexedExportTx.ID(), exportTx.ID(), "expected ID of indexed import tx to match original txID")

	// Open an empty trie to re-create the expected atomic trie roots
	trie, err := vm.atomicTrie.OpenTrie(common.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	assert.NoError(t, vm.atomicTrie.UpdateTrie(trie, blk.Height(), importTx.mustAtomicOps()))
	atomicRootBlock1 := trie.Hash()
	assert.NoError(t, vm.atomicTrie.UpdateTrie(trie, blk2.Height(), exportTx.mustAtomicOps()))
	atomicRootBlock2 := trie.Hash()
	assert.NotZero(t, atomicRootBlock1)
	assert.NotZero(t, atomicRootBlock2)
	assert.NotEqual(t, atomicRootBlock1, atomicRootBlock2)

	// verify atomic trie roots included in block header.
	extraStateRoot := blk.(*chain.BlockWrapper).Block.(*Block).ethBlock.Header().ExtraStateRoot
	extraStateRoot2 := blk2.(*chain.BlockWrapper).Block.(*Block).ethBlock.Header().ExtraStateRoot
	expectedRoot1, expectedRoot2 := test.expectedExtraStateRoot(atomicRootBlock1, atomicRootBlock2)
	assert.Equal(t, expectedRoot1, extraStateRoot)
	assert.Equal(t, expectedRoot2, extraStateRoot2)
}

// Verifies the root of the atomic trie is inclued in Clementine blocks.
func TestIssueAtomicTxsClementine(t *testing.T) {
	testVerifyExtraStateRoot(t, verifyExtraStateRootConfig{
		genesis:    genesisJSONClementine,
		blockTime1: time.Unix(0, 0), // genesis
		blockTime2: time.Unix(2, 0), // a bit after, for fee purposes.
		expectedExtraStateRoot: func(atomicRoot1, atomicRoot2 common.Hash) (common.Hash, common.Hash) {
			return atomicRoot1, atomicRoot2 // we expect both blocks to contain the atomic trie roots respectively.
		},
	})
}

// Verifies the root of the atomic trie is inclued in the first Clementine block.
func TestIssueAtomicTxsClementineTransition(t *testing.T) {
	testVerifyExtraStateRoot(t, verifyExtraStateRootConfig{
		genesis:    testClementineJSON,
		blockTime1: testClementineTime.Add(-2 * time.Second), // a little before Clementine, so we can test next block at the upgrade timestamp
		blockTime2: testClementineTime,                       // at the upgrade timestamp
		expectedExtraStateRoot: func(atomicRoot1, atomicRoot2 common.Hash) (common.Hash, common.Hash) {
			return common.Hash{}, atomicRoot2 // we only expect the Clementine block to include the atomic trie root.
		},
	})
}

// Calling Verify should not succeed if the proper ExtraStateRoot is not included in a Clementine block.
// Calling Verify should not succeed if ExtraStateRoot is not empty pre-Clementine
func TestClementineInvalidExtraStateRootWillNotVerify(t *testing.T) {
	importAmount := uint64(50000000)
	issuer, vm, _, _, _ := GenesisVMWithUTXOs(t, true, testClementineJSON, "", "", map[ids.ShortID]uint64{
		testShortIDAddrs[0]: importAmount,
	})
	defer func() {
		if err := vm.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	// issue a tx and build a Clementine block
	vm.clock.Set(testClementineTime)
	importTx, err := vm.newImportTx(vm.ctx.XChainID, testEthAddrs[0], initialBaseFee, []*crypto.PrivateKeySECP256K1R{testKeys[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.issueTx(importTx, true /*=local*/); err != nil {
		t.Fatal(err)
	}

	<-issuer

	// calling Verify on blk will succeed, we use it as
	// a starting point to make an invalid block.
	blk, err := vm.BuildBlock()
	if err != nil {
		t.Fatal(err)
	}
	validEthBlk := blk.(*chain.BlockWrapper).Block.(*Block).ethBlock

	// make a bad block by setting ExtraStateRoot to common.Hash{}
	badHeader := validEthBlk.Header()
	badHeader.ExtraStateRoot = common.Hash{}
	ethBlkBad := types.NewBlock(badHeader, validEthBlk.Transactions(), validEthBlk.Uncles(), nil, trie.NewStackTrie(nil), validEthBlk.ExtData(), true)

	badBlk, err := vm.newBlock(ethBlkBad)
	if err != nil {
		t.Fatal(err)
	}
	err = badBlk.Verify()
	assert.ErrorIs(t, err, errInvalidExtraStateRoot)

	// make a bad block by setting ExtraStateRoot to an incorrect hash
	badHeader = validEthBlk.Header()
	badHeader.ExtraStateRoot = common.BytesToHash([]byte("incorrect"))
	ethBlkBad = types.NewBlock(badHeader, validEthBlk.Transactions(), validEthBlk.Uncles(), nil, trie.NewStackTrie(nil), validEthBlk.ExtData(), true)

	badBlk, err = vm.newBlock(ethBlkBad)
	if err != nil {
		t.Fatal(err)
	}
	err = badBlk.Verify()
	assert.ErrorIs(t, err, errInvalidExtraStateRoot)

	// make a bad block by setting the timestamp before Clementine.
	badHeader = validEthBlk.Header()
	badHeader.Time = uint64(testClementineTime.Add(-2 * time.Second).Unix())
	ethBlkBad = types.NewBlock(badHeader, validEthBlk.Transactions(), validEthBlk.Uncles(), nil, trie.NewStackTrie(nil), validEthBlk.ExtData(), true)

	badBlk, err = vm.newBlock(ethBlkBad)
	if err != nil {
		t.Fatal(err)
	}
	err = badBlk.Verify()
	assert.ErrorIs(t, err, errInvalidExtraStateRoot)
}
