package jsonrpc

import (
	"context"
	"fmt"

	"github.com/erigontech/erigon-lib/common"
	txpool "github.com/erigontech/erigon-lib/gointerfaces/txpoolproto"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/eth/ethutils"
	bortypes "github.com/erigontech/erigon/polygon/bor/types"

	"github.com/erigontech/erigon-lib/common/math"

	"github.com/erigontech/erigon/rpc"

	"github.com/erigontech/erigon-lib/crypto"
	"github.com/erigontech/erigon/turbo/adapter/ethapi"
	"github.com/erigontech/erigon/turbo/rpchelper"
)

const enable_testing = true

type APIEthTraceImpl struct {
	APIImpl
	traceImpl *TraceAPIImpl
	borImpl   *BorImpl
}

func NewEthTraceAPI(base *BaseAPI, traceImpl *TraceAPIImpl, borImpl *BorImpl, db kv.TemporalRoDB, eth rpchelper.ApiBackend, txPool txpool.TxpoolClient, mining txpool.MiningClient, gascap uint64, returnDataLimit int, allowUnprotectedTxs bool, maxGetProofRewindBlockCount int, logger log.Logger) *APIEthTraceImpl {
	var gas_cap uint64
	if gascap == 0 {
		gas_cap = uint64(math.MaxUint64 / 2)
	}

	return &APIEthTraceImpl{
		APIImpl: APIImpl{
			BaseAPI:                     base,
			db:                          db,
			ethBackend:                  eth,
			txPool:                      txPool,
			mining:                      mining,
			gasCache:                    NewGasPriceCache(),
			GasCap:                      gas_cap,
			AllowUnprotectedTxs:         allowUnprotectedTxs,
			ReturnDataLimit:             returnDataLimit,
			MaxGetProofRewindBlockCount: maxGetProofRewindBlockCount,
			logger:                      logger,
		},
		traceImpl: traceImpl,
		borImpl:   borImpl,
	}
}

func (api *APIEthTraceImpl) GetBlockReceiptsTrace(ctx context.Context, numberOrHash rpc.BlockNumberOrHash) (map[string]interface{}, error) {
	block_trxs_enriched, block_trxs_err := api.APIImpl.GetBlockByNumber(ctx, *numberOrHash.BlockNumber, true)

	if block_trxs_err != nil {
		return nil, block_trxs_err
	}

	tx, err := api.db.BeginTemporalRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	blockNum, blockHash, _, err := rpchelper.GetBlockNumber(ctx, numberOrHash, tx, api._blockReader, api.filters)
	if err != nil {
		return nil, err
	}
	block, err := api.blockWithSenders(ctx, tx, blockHash, blockNum)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, fmt.Errorf("could not find block  %d", blockNum)
	}
	chainConfig, err := api.chainConfig(ctx, tx)
	if err != nil {
		return nil, err
	}

	var block_validator *common.Address
	var block_validator_err error

	if chainConfig.Bor != nil && api.borImpl != nil {
		block_validator, block_validator_err = api.borImpl.GetAuthor(&numberOrHash)
		if block_validator_err != nil {
			return nil, fmt.Errorf("could not get block validator for block %d: %w", *numberOrHash.BlockNumber, block_validator_err)
		}

		block_trxs_enriched["miner"] = block_validator
	}

	receipts, err := api.getReceipts(ctx, tx, block)
	if err != nil {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}

	result := make([]map[string]interface{}, 0, len(receipts))

	for _, receipt := range receipts {
		txn := block.Transactions()[receipt.TransactionIndex]

		full_result := ethutils.MarshalReceipt(receipt, txn, chainConfig, block.HeaderNoCopy(), txn.Hash(), true)

		result = append(result, full_result)
	}

	if chainConfig.Bor != nil {
		events, err := api.stateSyncEvents(ctx, tx, block.Hash(), blockNum, chainConfig)
		if err != nil {
			return nil, err
		}

		if len(events) != 0 {
			borReceipt, err := api.borReceiptGenerator.GenerateBorReceipt(ctx, tx, block, events, chainConfig)
			if err != nil {
				return nil, err
			}

			result = append(result, ethutils.MarshalReceipt(borReceipt, bortypes.NewBorTransaction(), chainConfig, block.HeaderNoCopy(), borReceipt.TxHash, false))
		}
	}

	trxs_len := len(block_trxs_enriched["transactions"].([]interface{}))

	for i := 0; i < trxs_len; i++ {
		trx := block_trxs_enriched["transactions"].([]interface{})[i].(*ethapi.RPCTransaction)
		if trx.Hash != result[i]["transactionHash"] {
			return nil, fmt.Errorf("transaction hash mismatch for transaction %d, trx number %d", *numberOrHash.BlockNumber, i)
		}
		trx.Receipts = result[i]
	}

	var gasBailOut *bool
	if gasBailOut == nil {
		gasBailOut = new(bool)
	}

	traceTypes := []string{"trace"}
	var traceTypeTrace, traceTypeStateDiff, traceTypeVmTrace bool
	traceTypeTrace = true

	signer := types.MakeSigner(chainConfig, blockNum, block.Time())
	traces, syscall, err := api.traceImpl.callBlock(ctx, tx, block, traceTypes, *gasBailOut, signer, chainConfig, nil)
	if err != nil {
		if len(result) > 0 {
			return block_trxs_enriched, nil
		}
		return nil, err
	}

	result_trace := make([]*TraceCallResult, len(traces))
	for i, trace := range traces {
		tr := &TraceCallResult{}
		tr.Output = trace.Output
		if traceTypeTrace {
			tr.Trace = trace.Trace
		} else {
			tr.Trace = []*ParityTrace{}
		}
		if traceTypeStateDiff {
			tr.StateDiff = trace.StateDiff
		}
		if traceTypeVmTrace {
			tr.VmTrace = trace.VmTrace
		}
		result_trace[i] = tr
	}

	rewards, err := api.engine().CalculateRewards(chainConfig, block.Header(), block.Uncles(), syscall)
	if err != nil {
		return nil, err
	}

	parity_traces := make([]ParityTrace, 0)

	for _, r := range rewards {
		var tr ParityTrace
		rewardAction := &RewardTraceAction{}
		rewardAction.Author = r.Beneficiary
		rewardAction.RewardType = rewardKindToString(r.Kind)
		rewardAction.Value.ToInt().Set(r.Amount.ToBig())
		tr.Action = rewardAction
		tr.BlockHash = &common.Hash{}
		copy(tr.BlockHash[:], block.Hash().Bytes())
		tr.BlockNumber = new(uint64)
		*tr.BlockNumber = block.NumberU64()
		tr.Type = "reward" // nolint: goconst
		tr.TraceAddress = []int{}
		parity_traces = append(parity_traces, tr)
	}

	if len(rewards) > 0 {
		block_trxs_enriched["rewards"] = parity_traces
	}

	trxs_in_block := block.Transactions()
	trx_with_senders_len := len(trxs_in_block)

	for i := 0; i < trxs_len; i++ {
		trx := block_trxs_enriched["transactions"].([]interface{})[i].(*ethapi.RPCTransaction)
		trx.Trace = result_trace[i]

		if i > trx_with_senders_len-1 {
			break
		}

		if trxs_in_block[i].Hash() != trx.Hash {
			return nil, fmt.Errorf("block_trxs_enriched.hash != trx_in_block.hash")
		}

		_, pub_key, signer_err := signer.Sender(trxs_in_block[i])
		if signer_err != nil {
			return nil, fmt.Errorf("cannot get pub key for block %d, trx index %d", *numberOrHash.BlockNumber, i)
		}

		ecdsa_pubkey, ecdsa_pubkey_err := crypto.UnmarshalPubkeyStd(pub_key)
		if ecdsa_pubkey_err != nil {
			return nil, fmt.Errorf("cannot get ECDSA pub key for block %d, trx index %d, error: %s", *numberOrHash.BlockNumber, i, ecdsa_pubkey_err.Error())
		}

		compressed_pubkey := crypto.CompressPubkey(ecdsa_pubkey)

		if enable_testing {
			// tx_hash := trxs_in_block[i].SigningHash(signer.ChainID().ToBig())

			// sig := make([]byte, 64)
			// copy(sig[32-len(trx.R.ToInt().Bytes()):32], trx.R.ToInt().Bytes())
			// copy(sig[64-len(trx.S.ToInt().Bytes()):64], trx.S.ToInt().Bytes())

			// verified := crypto.VerifySignature(compressed_pubkey, tx_hash[:], sig)
			// if !verified {
			// 	fmt.Println(tx_hash)
			// }

			dec_pub_key, _ := crypto.DecompressPubkey(compressed_pubkey)
			recovered_address := crypto.PubkeyToAddress(*dec_pub_key)
			if recovered_address != trx.From {
				return nil, fmt.Errorf("pub_key doesn't match address on block %d, trx %d", *numberOrHash.BlockNumber, i)
			}
		}

		trx.PubKey = common.PubKeyCompressedType(compressed_pubkey)
	}

	return block_trxs_enriched, nil
}
