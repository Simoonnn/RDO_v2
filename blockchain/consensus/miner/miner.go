package miner

import (
	"github.com/pkg/errors"
	"github.com/raidoNetwork/RDO_v2/blockchain/consensus"
	"github.com/raidoNetwork/RDO_v2/keystore"
	"github.com/raidoNetwork/RDO_v2/proto/prototype"
	"github.com/raidoNetwork/RDO_v2/shared/common"
	"github.com/raidoNetwork/RDO_v2/shared/params"
	"github.com/raidoNetwork/RDO_v2/shared/types"
	utypes "github.com/raidoNetwork/RDO_v2/utils/types"
	"github.com/sirupsen/logrus"
	"time"
)

var log = logrus.WithField("prefix", "Miner")

var ErrZeroFeeAmount = errors.New("Block has no transactions with fee.")

const txBatchLimit = 1000

// Config miner options
type Config struct {
	EnableMetrics bool
	BlockSize     int
	Proposer 	 *keystore.ValidatorAccount
}

func NewMiner(bc consensus.BlockForger, att consensus.AttestationPool, cfg *Config) *Miner {
	m := Miner{
		bc:       bc,
		att:      att,
		cfg:      cfg,
		skipAddr: map[string]struct{}{},
	}

	return &m
}

type Miner struct {
	bc   consensus.BlockForger
	att  consensus.AttestationPool
	cfg      *Config
	skipAddr map[string]struct{}
}

func (m *Miner) addRewardTxToBatch(txBatch []*prototype.Transaction, collapseBatch []*prototype.Transaction, totalSize int, bn int64) ([]*prototype.Transaction, []*prototype.Transaction, int, error) {
	rewardTx, err := m.createRewardTx(m.bc.GetBlockCount())
	if err != nil {
		if errors.Is(err, consensus.ErrNoStakers) {
			log.Warn("No stakers on current block.")
		} else {
			return nil, nil, 0, err
		}
	} else {
		txBatch = append(txBatch, rewardTx)
		totalSize += rewardTx.SizeSSZ()

		log.Debugf("Add RewardTx %s to the block", common.Encode(rewardTx.Hash))

		collapseTx, err := m.createCollapseTx(types.NewTransaction(rewardTx), m.bc.GetBlockCount())
		if err != nil {
			log.Errorf("Can't collapse reward tx %s outputs", common.Encode(rewardTx.Hash))
			return nil, nil, 0, errors.Wrap(err, "Error collapsing tx outputs")
		}

		if collapseTx != nil {
			err := m.att.TxPool().InsertCollapseTx(collapseTx)
			if err != nil {
				return txBatch, collapseBatch, totalSize, nil
			}

			collapseBatch = append(collapseBatch, collapseTx.GetTx())
			totalSize += collapseTx.Size()

			log.Debugf("Add CollapseTx %s to the block %d", collapseTx.Hash().Hex(), bn)
		}
	}

	return txBatch, collapseBatch, totalSize, nil
}

// ForgeBlock create block from tx pool data
func (m *Miner) ForgeBlock() (*prototype.Block, error) {
	start := time.Now()
	bn := start.Unix()
	totalSize := 0 // current size of block in bytes

	txQueueOrigin := m.att.TxPool().GetQueue()
	txQueue := make([]*types.Transaction, len(txQueueOrigin))
	copy(txQueue, txQueueOrigin)

	txQueueLen := len(txQueue)
	txBatch := make([]*prototype.Transaction, 0, txQueueLen)
	collapseBatch := make([]*prototype.Transaction, 0, txQueueLen)

	pendingTxCounter.Set(float64(txQueueLen))

	// create reward transaction for current block
	txBatch, collapseBatch, totalSize, err := m.addRewardTxToBatch(txBatch, collapseBatch, totalSize, bn)
	if err != nil {
		return nil, err
	}

	// limit tx count in block according to marshaller settings
	txCountPerBlock := txBatchLimit - len(txBatch) - len(collapseBatch) - 1
	if txQueueLen < txCountPerBlock {
		txCountPerBlock = txQueueLen
	}

	var size int
	for _, tx := range txQueue {
		if len(txBatch) + len(collapseBatch) == txCountPerBlock {
			break
		}

		size = tx.Size()
		totalSize += size

		// tx is too big try for look up another one
		if totalSize > m.cfg.BlockSize {
			totalSize -= size
			continue
		}

		hash := tx.Hash().Hex()

		// check if empty validator slots exists and skip stake tx if not exist
		if tx.Type() == common.StakeTxType {
			var amount uint64
			for _, out := range tx.Outputs() {
				if out.Node().Hex() == common.BlackHoleAddress {
					amount += out.Amount()
				}
			}

			err = m.att.StakePool().ReserveSlots(amount)
			if err != nil {
				totalSize -= size // return size of current tx

				// Delete stake tx from pool
				if err := m.att.TxPool().DeleteTransaction(tx); err != nil {
					return nil, errors.Wrap(err, "Error creating block")
				}

				log.Warnf("Skip stake tx %s: %s", hash, err)
				continue
			}

			log.Debugf("Add StakeTx %s to the block %d.", hash, bn)
		}

		txBatch = append(txBatch, tx.GetTx())
		log.Debugf("Add StandardTx %s to the block %d", hash, bn)

		collapseTx, err := m.createCollapseTx(tx, m.bc.GetBlockCount())
		if err != nil {
			log.Errorf("Can't collapse tx %s outputs", hash)
			return nil, errors.Wrap(err, "Error collapsing tx outputs")
		}

		// no need to create collapse tx for given tx addresses
		if collapseTx != nil {
			err := m.att.TxPool().InsertCollapseTx(collapseTx)
			if err != nil {
				log.Error(err)
			} else {
				collapseBatch = append(collapseBatch, collapseTx.GetTx())
				totalSize += collapseTx.Size()

				log.Debugf("Add CollapseTx %s to the block %d", collapseTx.Hash().Hex(), bn)
			}
		}

		// we fill block successfully
		if totalSize == m.cfg.BlockSize {
			break
		}
	}

	// add collapsed transaction to the end of the batch
	txBatch = append(txBatch, collapseBatch...)

	// generate fee tx for block
	if len(txBatch) > 0 {
		txFee, err := m.createFeeTx(txBatch)
		if err != nil && !errors.Is(err, ErrZeroFeeAmount) {
			return nil, err
		} else if err != nil {
			log.Debug(err)
		}else {
			txBatch = append(txBatch, txFee)
			log.Debugf("Add FeeTx %s to the block", common.Encode(txFee.Hash))
		}
	}

	// clear collapse list
	m.skipAddr = map[string]struct{}{}

	// get block instance
	block := types.NewBlock(m.bc.GetBlockCount(), m.bc.ParentHash(), txBatch, m.cfg.Proposer)

	end := time.Since(start)
	log.Warnf("Generate block with transactions count: %d. TxPool transactions count: %d. Size: %d kB. Time: %s", len(txBatch), txQueueLen, totalSize/1024, common.StatFmt(end))
	updateForgeMetrics(end)

	return block, nil
}

// FinalizeBlock validate given block and save it to the blockchain
func (m *Miner) FinalizeBlock(block *prototype.Block) error {
	finalizeStart := time.Now()

	// validate block
	failedTx, err := m.att.Validator().ValidateBlock(block)
	if err != nil {
		if failedTx != nil {
			m.att.TxPool().Finalize([]*types.Transaction{failedTx})
		}
		return errors.Wrap(err, "ValidateBlockError")
	}

	// save block
	err = m.bc.SaveBlock(block)
	if err != nil {
		return err
	}

	// update SQL
	err = m.bc.ProcessBlock(block)
	if err != nil {
		return errors.Wrap(err, "Error process block")
	}

	// clear pool
	m.att.TxPool().Finalize(utypes.PbTxBatchToTyped(block.Transactions))

	err = m.att.StakePool().UpdateStakeSlots(block)
	if err != nil {
		return errors.Wrap(err, "StakePool error")
	}

	// Reset reserved validator slots
	m.att.StakePool().FlushReservedSlots()

	err = m.bc.CheckBalance()
	if err != nil {
		return errors.Wrap(err, "Balances inconsistency")
	}

	finalizeBlockTime.Observe(float64(time.Since(finalizeStart).Milliseconds()))

	return nil
}

func (m *Miner) createFeeTx(txarr []*prototype.Transaction) (*prototype.Transaction, error) {
	var feeAmount uint64

	for _, tx := range txarr {
		feeAmount += tx.GetRealFee()
	}

	if feeAmount == 0 {
		return nil, ErrZeroFeeAmount
	}

	opts := types.TxOptions{
		Outputs: []*prototype.TxOutput{
			types.NewOutput(common.HexToAddress(common.BlackHoleAddress).Bytes(), feeAmount, nil),
		},
		Type: common.FeeTxType,
		Fee:  0,
		Num:  m.bc.GetBlockCount(),
	}

	ntx, err := types.NewPbTransaction(opts, nil)
	if err != nil {
		return nil, err
	}

	return ntx, nil
}

func (m *Miner) createRewardTx(blockNum uint64) (*prototype.Transaction, error) {
	slots := m.att.StakePool().GetStakeSlots()
	outs := m.createRewardOutputs(slots)

	if len(outs) == 0 {
		return nil, consensus.ErrNoStakers
	}

	opts := types.TxOptions{
		Outputs: outs,
		Type:    common.RewardTxType,
		Fee:     0,
		Num:     blockNum,
	}

	ntx, err := types.NewPbTransaction(opts, nil)
	if err != nil {
		return nil, err
	}

	return ntx, nil
}

func (m *Miner) createCollapseTx(tx *types.Transaction, blockNum uint64) (*types.Transaction, error) {
	const CollapseOutputsNum = 100 // minimal count of UTxO to collapse address outputs
	const InputsPerTxLimit = 2000

	opts := types.TxOptions{
		Inputs: make([]*prototype.TxInput, 0, len(tx.Inputs())),
		Outputs: make([]*prototype.TxOutput, 0, len(tx.Outputs()) - 1),
		Fee: 0,
		Num: blockNum,
		Type: common.CollapseTxType,
	}

	from := ""
	if tx.Type() != common.RewardTxType {
		from = tx.From().Hex()
		m.skipAddr[from] = struct{}{}
	}

	inputsLimitReached := false
	for _, out := range tx.Outputs() {
		addr := out.Address().Hex()

		// skip already collapsed addresses and sender address
		if _, exists := m.skipAddr[addr]; exists {
			continue
		}

		utxo, err := m.bc.FindAllUTxO(addr)
		if err != nil {
			return nil, err
		}

		utxoCount := len(utxo)
		m.skipAddr[addr] = struct{}{}

		// check address need collapsing
		if utxoCount < CollapseOutputsNum {
			continue
		}

		// process address outputs
		userInputs := make([]*prototype.TxInput, 0, len(utxo))
		var balance uint64
		for _, uo := range utxo {
			input := uo.ToPbInput()

			balance += uo.Amount
			userInputs = append(userInputs, input)

			if len(opts.Inputs) + len(userInputs) == InputsPerTxLimit {
				inputsLimitReached = true
				break
			}
		}

		if len(userInputs) > 0 {
			opts.Inputs = append(opts.Inputs, userInputs...)
			collapsedOutput := types.NewOutput(out.Address(), balance, nil)
			opts.Outputs = append(opts.Outputs, collapsedOutput)
		}

		// break generation when inputs limit is reached
		if inputsLimitReached {
			break
		}
	}

	var collapseTx *prototype.Transaction
	var err error

	if len(opts.Inputs) > 0 {
		collapseTx, err = types.NewPbTransaction(opts, nil)
		if err != nil {
			return nil, err
		}

		log.Debugf("Collapse tx for %s with hash %s", tx.Hash().Hex(), common.Encode(collapseTx.Hash))
	}

	return types.NewTransaction(collapseTx), nil
}

func (m *Miner) createRewardOutputs(slots []string) []*prototype.TxOutput {
	size := len(slots)

	data := make([]*prototype.TxOutput, 0, size)
	if size == 0 {
		return data
	}

	// divide reward among all validator slots
	reward := m.getRewardAmount(size)

	rewardMap := map[string]uint64{}
	for _, addrHex := range slots {
		if _, exists := rewardMap[addrHex]; exists {
			rewardMap[addrHex] += reward
		} else {
			rewardMap[addrHex] = reward
		}
	}

	for addr, amount := range rewardMap {
		addr := common.HexToAddress(addr)
		data = append(data, types.NewOutput(addr.Bytes(), amount, nil))
	}

	return data
}

func (m *Miner) getRewardAmount(size int) uint64 {
	if size == 0 {
		return 0
	}

	return params.RaidoConfig().RewardBase / uint64(size)
}