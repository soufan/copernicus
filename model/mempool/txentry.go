package mempool

import (
	"unsafe"

	"github.com/copernet/copernicus/model/chain"
	"github.com/copernet/copernicus/model/tx"
	"github.com/copernet/copernicus/util"
	"github.com/copernet/copernicus/util/algorithm/mapcontainer"
)

type TxEntry struct {
	Tx     *tx.Tx
	TxSize int
	// txFee tis transaction fee
	TxFee    int64
	TxHeight int32
	// sigOpCount sigop plus P2SH sigops count
	SigOpCount int
	// time Local time when entering the memPool
	time int64
	// usageSize and total memory usage;
	usageSize int
	// childTx the tx's all Descendants transaction
	ChildTx map[*TxEntry]struct{}
	// parentTx the tx's all Ancestors transaction
	ParentTx map[*TxEntry]struct{}
	// lp Track the height and time at which tx was final
	lp LockPoints
	// spendsCoinBase keep track of transactions that spend a coinBase
	spendsCoinbase bool

	//Statistics Information for every txentry with its ancestors And descend.
	StatisInformation
}

type StatisInformation struct {
	// sumTxCountWithDescendants is this tx and all Descendants transaction's number. init number is 1.
	SumTxCountWithDescendants int64
	// sumFeeWithDescendants is calculated by this tx and all Descendants transaction;
	SumTxFeeWithDescendants int64
	// sumSizeWithDescendants size calculated by this tx and all Descendants transaction;
	SumTxSizeWithDescendants int64

	SumTxCountWithAncestors      int64
	SumTxSizeWitAncestors        int64
	SumTxSigOpCountWithAncestors int64
	SumTxFeeWithAncestors        int64
}
// statisticAddEntry
func (t *TxEntry) statisticIncrease(ancestors map[*TxEntry]struct{}) {
	t.UpdateAncestorState(statisticSum(ancestors))
	statisticIncrementalUpdateDescendant(ancestors, 1, t.TxSize, t.TxFee)
}
// statisticDelEntry
func (t *TxEntry) statisticDecrease(ancestors map[*TxEntry]struct{}, descendants map[*TxEntry]struct{}) {
	for antor := range ancestors {
		antor.UpdateDescendantState(-1, -t.TxSize, -t.TxFee)
	}

	for desdand := range descendants {
		desdand.UpdateAncestorState(-1, -t.TxSize, -t.SigOpCount, -t.TxFee)
	}
}

func statisticIncrementalUpdateDescendant(entries map[*TxEntry]struct{}, deltaCount, deltaSize int, deltaFee int64) {
	for e := range entries {
		e.UpdateDescendantState(deltaCount, deltaSize, deltaFee)
	}
}

func statisticSum(entries map[*TxEntry]struct{}) (int, int, int, int64) {
	cnt := len(entries)
	var size, sigOpCnt int
	var fee int64
	for e := range entries {
		fee += e.TxFee
		sigOpCnt += e.SigOpCount
		size += e.TxSize
	}
	return cnt, size, sigOpCnt, fee
}

func (t *TxEntry) GetSigOpCountWithAncestors() int64 {
	return t.SumTxSigOpCountWithAncestors
}

func (t *TxEntry) GetUsageSize() int64 {
	return int64(t.usageSize)
}

func (t *TxEntry) SetLockPointFromTxEntry(lp LockPoints) {
	t.lp = lp
}

func (t *TxEntry) GetLockPointFromTxEntry() LockPoints {
	return t.lp
}

func (t *TxEntry) GetSpendsCoinbase() bool {
	return t.spendsCoinbase
}

func (t *TxEntry) GetTime() int64 {
	return t.time
}


func (t *TxEntry) addParent(parent *TxEntry) {
	t.ParentTx[parent] = struct{}{}
}

func (t *TxEntry) delParent(parent *TxEntry) {
	delete(t.ParentTx, parent)
}

func (t *TxEntry) addChild(child *TxEntry) {
	t.ChildTx[child] = struct{}{}
}

func (t *TxEntry) delChild(child *TxEntry) {
	delete(t.ChildTx, child)
}

func (t *TxEntry) associateRelationship(parents map[util.Hash]*TxEntry) {
	for _, parentTxEntry := range parents {
		// Update txEntry's parents
		t.addParent(parentTxEntry)

		// Update txEntry's parents' child
		parentTxEntry.addChild(t)
	}
}

func (t *TxEntry) unassociateRelationship() {
	for parent := range t.ParentTx {
		parent.delChild(t)
	}
	t.ParentTx = nil

	for child := range t.ChildTx {
		child.delParent(t)
	}
	t.ChildTx = nil
}



func (t *TxEntry) UpdateChild(child *TxEntry, add bool) {
	if add {
		t.ChildTx[child] = struct{}{}
		return
	}
	delete(t.ChildTx, child)
}

func (t *TxEntry) UpdateChildOfParents(add bool) {
	// update the parent's child transaction set;
	for piter := range t.ParentTx {
		if add {
			piter.UpdateChild(t, true)
		} else {
			piter.UpdateChild(t, false)
		}
	}
}

func (t *TxEntry) UpdateDescendantState(updateCount, updateSize int, updateFee int64) {
	t.SumTxCountWithDescendants += int64(updateCount)
	t.SumTxSizeWithDescendants += int64(updateSize)
	t.SumTxFeeWithDescendants += updateFee
}

func (t *TxEntry) UpdateAncestorState(updateCount, updateSize, updateSigOps int, updateFee int64) {
	t.SumTxSizeWitAncestors += int64(updateSize)
	t.SumTxCountWithAncestors += int64(updateCount)
	t.SumTxSigOpCountWithAncestors += int64(updateSigOps)
	t.SumTxFeeWithAncestors += updateFee
}

func (t *TxEntry) Less(than mapcontainer.Lesser) bool {
	th := than.(*TxEntry)
	if t.time == th.time {
		thash := t.Tx.GetHash()
		thhash := th.Tx.GetHash()
		return thash.Cmp(&thhash) > 0
	}
	return t.time < th.time
}

func NewTxentry(tx *tx.Tx, txFee int64, acceptTime int64, height int32, lp LockPoints, sigOpsCount int,
	spendCoinbase bool) *TxEntry {
	t := new(TxEntry)
	t.Tx = tx
	t.time = acceptTime
	t.TxSize = int(tx.SerializeSize())
	t.TxFee = txFee
	t.usageSize = t.TxSize + int(unsafe.Sizeof(*t))
	t.spendsCoinbase = spendCoinbase
	t.lp = lp
	t.TxHeight = height
	t.SigOpCount = sigOpsCount

	t.SumTxSizeWithDescendants = int64(t.TxSize)
	t.SumTxFeeWithDescendants = txFee
	t.SumTxCountWithDescendants = 1

	t.SumTxFeeWithAncestors = txFee
	t.SumTxSizeWitAncestors = int64(t.TxSize)
	t.SumTxCountWithAncestors = 1
	t.SumTxSigOpCountWithAncestors = int64(sigOpsCount)

	t.ParentTx = make(map[*TxEntry]struct{})
	t.ChildTx = make(map[*TxEntry]struct{})

	return t
}

func (t *TxEntry) GetFeeRate() *util.FeeRate {
	return util.NewFeeRateWithSize(t.TxFee, int64(t.TxSize))
}

func (t *TxEntry) GetInfo() *TxMempoolInfo {
	return &TxMempoolInfo{
		Tx:      t.Tx,
		Time:    t.time,
		FeeRate: *t.GetFeeRate(),
	}
}

func (t *TxEntry) CheckLockPointValidity(chain *chain.Chain) bool {
	if t.lp.MaxInputBlock != nil {
		if !chain.Contains(t.lp.MaxInputBlock) {
			return false
		}
	}
	return true
}

type EntryFeeSort TxEntry

func (e EntryFeeSort) Less(than mapcontainer.Lesser) bool {
	t := than.(EntryFeeSort)
	if e.SumTxFeeWithAncestors == t.SumTxFeeWithAncestors {
		thash := e.Tx.GetHash()
		thhash := t.Tx.GetHash()
		return thash.Cmp(&thhash) < 0
	}
	return e.SumTxFeeWithAncestors > than.(EntryFeeSort).SumTxFeeWithAncestors
}

type EntryAncestorFeeRateSort TxEntry

func (r *EntryAncestorFeeRateSort) Less(than mapcontainer.Lesser) bool {
	t := than.(*EntryAncestorFeeRateSort)

	b1 := util.NewFeeRateWithSize((r).SumTxFeeWithAncestors, r.SumTxSizeWitAncestors).SataoshisPerK
	b2 := util.NewFeeRateWithSize(t.SumTxFeeWithAncestors, t.SumTxSizeWitAncestors).SataoshisPerK
	if b1 == b2 {
		rhash := r.Tx.GetHash()
		thhash := t.Tx.GetHash()
		return rhash.Cmp(&thhash) < 0
	}
	return b1 > b2
}
