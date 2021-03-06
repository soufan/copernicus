// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package syncmanager

import (
	"container/list"
	"github.com/copernet/copernicus/model/pow"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/copernet/copernicus/errcode"
	"github.com/copernet/copernicus/log"
	"github.com/copernet/copernicus/logic/lchain"
	"github.com/copernet/copernicus/logic/lmempool"
	"github.com/copernet/copernicus/model"
	"github.com/copernet/copernicus/model/block"
	"github.com/copernet/copernicus/model/blockindex"
	"github.com/copernet/copernicus/model/chain"
	"github.com/copernet/copernicus/model/mempool"
	"github.com/copernet/copernicus/model/outpoint"
	"github.com/copernet/copernicus/model/tx"
	"github.com/copernet/copernicus/model/utxo"
	"github.com/copernet/copernicus/net/wire"
	"github.com/copernet/copernicus/peer"
	"github.com/copernet/copernicus/util"
)

const (
	// maxRejectedTxns is the maximum number of rejected transactions
	// hashes to store in memory.
	maxRejectedTxns = 1000

	// maxRequestedBlocks is the maximum number of requested block
	// hashes to store in memory.
	maxRequestedBlocks = wire.MaxInvPerMsg

	// maxRequestedTxns is the maximum number of requested transactions
	// hashes to store in memory.
	maxRequestedTxns = wire.MaxInvPerMsg

	blockRequestTimeoutTime = 20 * time.Minute

	//MAX_BLOCKS_IN_TRANSIT_PER_PEER is Number of blocks that can be requested at any given time from a single peer
	MAX_BLOCKS_IN_TRANSIT_PER_PEER = 16

	// BLOCK_DOWNLOAD_WINDOW see below
	/**
	 * Size of the "block download window": how far ahead of our current height do
	 * we fetch ? Larger windows tolerate larger download speed differences between
	 * peer, but increase the potential degree of disordering of blocks on disk
	 * (which make reindexing and in the future perhaps pruning harder). We'll
	 * probably want to make this a per-peer adaptive value at some point.
	 */
	BLOCK_DOWNLOAD_WINDOW = 1024

	// MAX_UNCONNECTING_HEADERS Maximum number of unconnecting headers announcements before DoS score
	MAX_UNCONNECTING_HEADERS = 10

	// fetchInterval is the interval to fetchHeaderBlocks for all peer
	fetchInterval = 1 * time.Second

	// BLOCK_STALLING_TIMEOUT in microsecond during which a peer must stall block
	// download progress before being disconnected
	BLOCK_STALLING_TIMEOUT = 2 * 1000000
)

// zeroHash is the zero value hash (all zeros).  It is defined as a convenience.
var zeroHash util.Hash

// newPeerMsg signifies a newly connected peer to the block handler.
type newPeerMsg struct {
	peer *peer.Peer
}

// blockMsg packages a bitcoin block message and the peer it came from together
// so the block handler has access to that information.
type blockMsg struct {
	block *block.Block
	buf   []byte
	peer  *peer.Peer
	reply chan<- struct{}
}

type minedBlockMsg struct {
	block *block.Block
	reply chan<- error
}

// poolMsg package a bitcoin mempool message and peer it come from together
type poolMsg struct {
	pool  *wire.MsgMemPool
	peer  *peer.Peer
	reply chan<- struct{}
}

// getdataMsg package a bitcoin getdata message And peer it come from together
type getdataMsg struct {
	getdata *wire.MsgGetData
	peer    *peer.Peer
	reply   chan<- struct{}
}

// getBlocksMsg package a bitcoin getblocks message And peer it come from together
type getBlocksMsg struct {
	getblocks *wire.MsgGetBlocks
	peer      *peer.Peer
	reply     chan<- struct{}
}

// invMsg packages a bitcoin inv message and the peer it came from together
// so the block handler has access to that information.
type invMsg struct {
	inv  *wire.MsgInv
	peer *peer.Peer
}

// headersMsg packages a bitcoin headers message and the peer it came from
// together so the block handler has access to that information.
type headersMsg struct {
	headers *wire.MsgHeaders
	peer    *peer.Peer
}

type pingMsg struct {
	ping  *wire.MsgPing
	peer  *peer.Peer
	reply chan<- struct{}
}

// donePeerMsg signifies a newly disconnected peer to the block handler.
type donePeerMsg struct {
	peer *peer.Peer
}

// txMsg packages a bitcoin tx message and the peer it came from together
// so the block handler has access to that information.
type txMsg struct {
	tx    *tx.Tx
	peer  *peer.Peer
	reply chan<- struct{}
}

// getSyncPeerMsg is a message type to be sent across the message channel for
// retrieving the current sync peer.
type getSyncPeerMsg struct {
	reply chan int32
}

// processBlockResponse is a response sent to the reply channel of a
// processBlockMsg.
type processBlockResponse struct {
	isOrphan bool
	err      error
}

// processBlockMsg is a message type to be sent across the message channel
// for requested a block is processed.  Note this call differs from blockMsg
// above in that blockMsg is intended for blocks that came from peers and have
// extra handling whereas this message essentially is just a concurrent safe
// way to call ProcessBlock on the internal block chain instance.
type processBlockMsg struct {
	block *block.Block
	flags chain.BehaviorFlags
	reply chan processBlockResponse
}

// isCurrentMsg is a message type to be sent across the message channel for
// requesting whether or not the sync manager believes it is synced with the
// currently connected peers.
type isCurrentMsg struct {
	reply chan bool
}

// pauseMsg is a message type to be sent across the message channel for
// pausing the sync manager.  This effectively provides the caller with
// exclusive access over the manager until a receive is performed on the
// unpause channel.
type pauseMsg struct {
	unpause <-chan struct{}
}

// headerNode is used as a node in a list of headers that are linked together
type headerNode struct {
	height int32
	hash   *util.Hash
}

// peerSyncState stores additional information that the SyncManager tracks
// about a peer.
type peerSyncState struct {
	syncCandidate       bool
	requestQueue        []*wire.InvVect
	requestedTxns       map[util.Hash]struct{}
	requestedBlocks     map[util.Hash]struct{}
	unconnectingHeaders int
}

// SyncManager is used to communicate block related messages with peers. The
// SyncManager is started as by executing Start() in a goroutine. Once started,
// it selects peers to sync from and starts the initial block download. Once the
// chain is in sync, the SyncManager handles incoming block and header
// notifications and relays announcements of new blocks to peers.
type SyncManager struct {
	peerNotifier        PeerNotifier
	started             int32
	shutdown            int32
	chainParams         *model.BitcoinParams
	progressLogger      *blockProgressLogger
	processBusinessChan chan interface{}
	wg                  sync.WaitGroup
	quit                chan struct{}

	// These fields should only be accessed from the messagesHandler
	rejectedTxns    map[util.Hash]struct{}
	requestedTxns   map[util.Hash]struct{}
	requestedBlocks map[util.Hash]*peer.Peer
	syncPeer        *peer.Peer
	peerStates      map[*peer.Peer]*peerSyncState

	// callback for transaction And block process
	ProcessTransactionCallBack func(*tx.Tx, map[util.Hash]struct{}, int64) ([]*tx.Tx, []util.Hash, []util.Hash, error)
	ProcessBlockCallBack       func(*block.Block, bool) (bool, error)
	ProcessBlockHeadCallBack   func([]*block.BlockHeader, *blockindex.BlockIndex) error
	AddBanScoreCallBack        func(string, uint32, uint32, string)

	// An optional fee estimator.
	//feeEstimator *mempool.FeeEstimator
}

// findNextHeaderCheckpoint returns the next checkpoint after the passed height.
// It returns nil when there is not one either because the height is already
// later than the final checkpoint or some other reason such as disabled
// checkpoints.
func (sm *SyncManager) findNextHeaderCheckpoint(height int32) *model.Checkpoint {
	//todo !!! need to be modified to be flexible for checkpoint with chainpram.
	checkpoints := model.ActiveNetParams.Checkpoints
	log.Trace("come into findNextHeaderCheckpoint, numbers : %d ...", len(checkpoints))
	if len(checkpoints) == 0 {
		return nil
	}

	// There is no next checkpoint if the height is already after the final
	// checkpoint.
	finalCheckpoint := checkpoints[len(checkpoints)-1]
	log.Trace("finalCheckpoint.Height : %d, current height : %d ", finalCheckpoint.Height, height)
	if height >= finalCheckpoint.Height {
		return nil
	}

	// Find the next checkpoint.
	nextCheckpoint := finalCheckpoint
	for i := len(checkpoints) - 2; i >= 0; i-- {
		if height >= checkpoints[i].Height {
			break
		}
		nextCheckpoint = checkpoints[i]
	}
	log.Trace("return checkpoint heigth : %d", nextCheckpoint.Height)
	return nextCheckpoint
}

// startSync will choose the best peer among the available candidate peers to
// download/sync the blockchain from.  When syncing is already running, it
// simply returns.  It also examines the candidates for any which are no longer
// candidates and removes them as needed.
func (sm *SyncManager) startSync() {
	// Return now if we're already syncing.
	if sm.syncPeer != nil {
		return
	}

	best := chain.GetInstance().Tip()
	var bestPeer *peer.Peer
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While technically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if peer.LastBlock() < best.Height {
			continue
		}

		// TODO(davec): Use a better algorithm to choose the best peer.
		// For now, just pick the first available candidate.
		bestPeer = peer
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		activeChain := chain.GetInstance()
		pindexBestHeader := activeChain.GetIndexBestHeader()
		if pindexBestHeader == nil {
			pindexBestHeader = activeChain.Tip()
			activeChain.SetIndexBestHeader(pindexBestHeader)
		}
		pindexStart := pindexBestHeader
		/**
		 * If possible, start at the block preceding the currently best
		 * known header. This ensures that we always get a non-empty list of
		 * headers back as long as the peer is up-to-date. With a non-empty
		 * response, we can initialise the peer's known best block. This
		 * wouldn't be possible if we requested starting at pindexBestHeader
		 * and got back an empty response.
		 */
		if pindexStart.Prev != nil {
			pindexStart = pindexStart.Prev
		}
		locator := activeChain.GetLocator(pindexStart)
		log.Info("Syncing to block height %d from peer %v",
			bestPeer.LastBlock(), bestPeer.Addr())

		bestPeer.PushGetHeadersMsg(*locator, &zeroHash)

		sm.syncPeer = bestPeer
		if sm.current() {
			log.Debug("request mempool in startSync")
			bestPeer.RequestMemPool()
		}
	} else {
		log.Warn("No sync peer candidates available")
	}
}

// isSyncCandidate returns whether or not the peer is a candidate to consider
// syncing from.
func (sm *SyncManager) isSyncCandidate(peer *peer.Peer) bool {
	// Typically a peer is not a candidate for sync if it's not a full node,
	// however regression test is special in that the regression tool is
	// not a full node and still needs to be considered a sync candidate.
	if sm.chainParams == &model.RegressionNetParams {
		// The peer is not a candidate if it's not coming from localhost
		// or the hostname can't be determined for some reason.
		host, _, err := net.SplitHostPort(peer.Addr())
		if err != nil {
			return false
		}

		if host != "127.0.0.1" && host != "localhost" {
			return false
		}
	} else {
		// The peer is not a candidate for sync if it's not a full
		// node. Additionally, if the segwit soft-fork package has
		// activated, then the peer must also be upgraded.
		nodeServices := peer.Services()
		if nodeServices&wire.SFNodeNetwork != wire.SFNodeNetwork {
			return false
		}
	}

	// Candidate if all checks passed.
	return true
}

// handleNewPeerMsg deals with new peers that have signalled they may
// be considered as a sync peer (they have already successfully negotiated).  It
// also starts syncing if needed.  It is invoked from the syncHandler goroutine.
func (sm *SyncManager) handleNewPeerMsg(peer *peer.Peer) {
	// Ignore if in the process of shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	log.Info("New valid peer %s (%s), start height : %d", peer.Addr(), peer.UserAgent(), peer.StartingHeight())

	// Initialize the peer state
	isSyncCandidate := sm.isSyncCandidate(peer)
	sm.peerStates[peer] = &peerSyncState{
		syncCandidate:   isSyncCandidate,
		requestedTxns:   make(map[util.Hash]struct{}),
		requestedBlocks: make(map[util.Hash]struct{}),
	}

	if !lchain.IsInitialBlockDownload() && peer.VerAckReceived() {
		gChain := chain.GetInstance()
		pindexBestHeader := gChain.GetIndexBestHeader()
		if pindexBestHeader == nil {
			pindexBestHeader = gChain.Tip()
			gChain.SetIndexBestHeader(pindexBestHeader)
		}
		pindexStart := pindexBestHeader
		if pindexStart.Prev != nil {
			pindexStart = pindexStart.Prev
		}
		locator := gChain.GetLocator(pindexStart)
		peer.PushGetHeadersMsg(*locator, &zeroHash)
	}

	// Start syncing by choosing the best candidate if needed.
	if isSyncCandidate && sm.syncPeer == nil {
		sm.startSync()
		return
	}

	if isSyncCandidate && sm.current() && peer.LastBlock() > sm.syncPeer.LastBlock() {
		sm.syncPeer = nil
		sm.startSync()
		return
	}

	sm.fetchHeaderBlocks(peer)
}

func (sm *SyncManager) clearSyncPeerState(peer *peer.Peer) {
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warn("Received done peer message for unknown peer %s", peer)
		return
	}

	// Remove the peer from the list of candidate peers.
	delete(sm.peerStates, peer)

	log.Info("Lost peer %s", peer.Addr())

	// Remove requested transactions from the global map so that they will
	// be fetched from elsewhere next time we get an inv.
	for txHash := range state.requestedTxns {
		delete(sm.requestedTxns, txHash)
	}

	// Remove requested blocks from the global map so that they will be
	// fetched from elsewhere next time we get an inv.
	// TODO: we could possibly here check which peers have these blocks
	// and request them now to speed things up a little.
	for blockHash := range state.requestedBlocks {
		delete(sm.requestedBlocks, blockHash)
	}
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It
// removes the peer as a candidate for syncing and in the case where it was
// the current sync peer, attempts to select a new best peer to sync from.  It
// is invoked from the syncHandler goroutine.
func (sm *SyncManager) handleDonePeerMsg(peer *peer.Peer) {
	sm.clearSyncPeerState(peer)

	// Attempt to find a new peer to sync from if the quitting peer is the
	// sync peer.  Also, reset the headers-first state if in headers-first
	// mode so
	if sm.syncPeer == peer {
		sm.syncPeer = nil
		sm.startSync()
	}
}

func (sm *SyncManager) alreadyHave(txHash *util.Hash) bool {
	// Ignore transactions that we have already rejected.  Do not
	// send a reject message here because if the transaction was already
	// rejected, the transaction was unsolicited.
	if _, exists := sm.rejectedTxns[*txHash]; exists {
		log.Debug("Ignoring unsolicited previously rejected transaction %v", txHash)
		return true
	}

	have, err := sm.haveInventory(wire.NewInvVect(wire.InvTypeTx, txHash))
	return err == nil && have
}

// handleTxMsg handles transaction messages from all peers.
func (sm *SyncManager) handleTxMsg(tmsg *txMsg) {
	peer := tmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warn("Received tx message from unknown peer %s", peer.Addr())
		return
	}

	txHash := tmsg.tx.GetHash()

	if sm.alreadyHave(&txHash) {
		//TODO: relay tx for whitelistrelay node
		log.Trace("Ignore already processed tx from %s", peer.Addr())
		return
	}

	// Process the transaction to include validation, insertion in the memory pool, orphan handling, etc.
	acceptTxs, missTxs, rejectTxs, err := sm.ProcessTransactionCallBack(tmsg.tx, sm.rejectedTxns, int64(peer.ID()))

	sm.updateTxRequestState(state, txHash, rejectTxs)

	fetchMissingTx(missTxs, peer)

	if err != nil {
		if rejectCode, reason, ok := errcode.IsRejectCode(err); ok {
			peer.PushRejectMsg(wire.CmdTx, rejectCode, reason, &txHash, false)
			log.Debug("Reject tx %s from %s: %v", txHash, peer.Addr(), err)
			return
		}

		log.Warn("Failed to process transaction %s: %v", txHash, err)
		return
	}

	txentrys := make([]*mempool.TxEntry, 0, len(acceptTxs))
	for _, tx := range acceptTxs {
		if entry := lmempool.FindTxInMempool(tx.GetHash()); entry != nil {
			txentrys = append(txentrys, entry)
		} else {
			panic("the transaction must be in mempool")
		}
	}

	sm.peerNotifier.AnnounceNewTransactions(txentrys)
}

func (sm *SyncManager) updateTxRequestState(state *peerSyncState, txHash util.Hash, rejectTxs []util.Hash) {
	// Remove transaction from request maps. Either the mempool/chain already knows about it
	// and as such we shouldn't have any more instances of trying to fetch it, or we failed to
	// insert and thus we'll retry next time we get an inv.
	delete(state.requestedTxns, txHash)
	delete(sm.requestedTxns, txHash)

	// Do not request these transactions again until a new block has been processed.
	for _, rejectTx := range rejectTxs {
		sm.rejectedTxns[rejectTx] = struct{}{}
	}
	sm.limitMap(sm.rejectedTxns, maxRejectedTxns)
}

func fetchMissingTx(missTxs []util.Hash, peer *peer.Peer) {
	invMsg := wire.NewMsgInvSizeHint(uint(len(missTxs)))
	for _, hash := range missTxs {
		iv := wire.NewInvVect(wire.InvTypeTx, &hash)
		invMsg.AddInvVect(iv)
	}
	if len(missTxs) > 0 {
		peer.QueueMessage(invMsg, nil)
	}
}

// current returns true if we believe we are synced with our peers, false if we
// still have blocks to check
func (sm *SyncManager) current() bool {
	if !chain.GetInstance().IsCurrent() {
		return false
	}

	// if blockChain thinks we are current and we have no syncPeer it
	// is probably right.
	if sm.syncPeer == nil {
		return true
	}

	// No matter what chain thinks, if we are below the block we are syncing
	// to we are not current.
	if chain.GetInstance().Tip().Height < sm.syncPeer.LastBlock() {
		return false
	}
	return true
}

// handleBlockMsg handles block messages from all peers.
func (sm *SyncManager) handleBlockMsg(bmsg *blockMsg) {
	peer := bmsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warn("Received block message from unknown peer %s", peer.Addr())
		return
	}

	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := bmsg.block.GetHash()
	if _, exists = state.requestedBlocks[blockHash]; !exists {
		// The regression test intentionally sends some blocks twice
		// to test duplicate block insertion fails.  Don't disconnect
		// the peer or ignore the block when we're in regression test
		// mode in this case so the chain code is actually fed the
		// duplicate blocks.
		if sm.chainParams != &model.RegressionNetParams {
			log.Warn("Got unrequested block %v from %s -- "+
				"disconnecting", blockHash, peer.Addr())
			peer.Disconnect()
			return
		}
	}

	// Process all blocks from whitelisted peers, even if not requested,
	// unless we're still syncing with the network. Such an unrequested
	// block may still be processed, subject to the conditions in AcceptBlock().
	fromWhitelist := peer.IsWhitelisted() && !lchain.IsInitialBlockDownload()
	_, requested := sm.requestedBlocks[blockHash]

	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	delete(state.requestedBlocks, blockHash)
	delete(sm.requestedBlocks, blockHash)
	peer.SetStallingSince(0)

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.
	_, err := sm.ProcessBlockCallBack(bmsg.block, requested || fromWhitelist)
	if err != nil {
		// When the error is a rule error, it means the block was simply
		// rejected as opposed to something actually going wrong, so log
		// it as such.  Otherwise, something really did go wrong, so log
		// it as an actual error.
		if rejectCode, reason, ok := errcode.IsRejectCode(err); ok {
			peer.PushRejectMsg(wire.CmdBlock, rejectCode, reason, &blockHash, false)
			log.Debug("ProcessBlockCallBack reject err:%v, hash: %s", err, blockHash)
		} else {
			log.Error("ProcessBlockCallBack err:%v, hash: %s", err, blockHash)
		}

		if len(state.requestedBlocks) == 0 {
			sm.fetchHeaderBlocks(peer)
		}
		return
	}

	// Meta-data about the new block this peer is reporting. We use this
	// below to update this peer's lastest block height and the heights of
	// other peers based on their last announced block hash. This allows us
	// to dynamically update the block heights of peers, avoiding stale
	// heights when looking for a new sync peer. Upon acceptance of a block
	// or recognition of an orphan, we also use this information to update
	// the block heights over other peers who's invs may have been ignored
	// if we are actively syncing while the chain is not yet current or
	// who may have lost the lock announcment race.
	var heightUpdate int32
	var blkHashUpdate *util.Hash

	// When the block is not an orphan, log information about it and
	// update the chain state.
	sm.progressLogger.LogBlockHeight(bmsg.block)

	// Update this peer's latest block height, for future
	// potential sync node candidacy.
	best := chain.GetInstance().Tip()
	heightUpdate = best.Height
	blkHashUpdate = best.GetBlockHash()

	// Clear the rejected transactions.
	sm.rejectedTxns = make(map[util.Hash]struct{})

	// Update the block height for this peer. But only send a message to
	// the server for updating peer heights if this is an orphan or our
	// chain is "current". This avoids sending a spammy amount of messages
	// if we're syncing the chain from scratch.
	if blkHashUpdate != nil && heightUpdate != 0 {
		peer.UpdateLastBlockHeight(heightUpdate)
		if sm.current() && peer == sm.syncPeer {
			go sm.peerNotifier.UpdatePeerHeights(blkHashUpdate, heightUpdate, peer)
			log.Debug("request mempool in handleBlockMsg")
			peer.RequestMemPool()
		}
	}

	sm.fetchHeaderBlocks(peer)
}

func (sm *SyncManager) handleMinedBlockMsg(mbmsg *minedBlockMsg) {
	var err error
	defer func() {
		if mbmsg.reply != nil {
			mbmsg.reply <- err
		}
	}()
	hash := mbmsg.block.GetHash()
	_, err = sm.ProcessBlockCallBack(mbmsg.block, true)
	if err != nil {
		log.Error("process mined block(%v) err(%v)", &hash, err)
		return
	}
	log.Debug("process mined block(%v) done via submitblock", &hash)
}

func lastAccouncedBlock(peer *peer.Peer) *blockindex.BlockIndex {
	pindexBestKnownHash := peer.LastAnnouncedBlock()
	if pindexBestKnownHash == nil {
		log.Info("peer(%d) best known block nil, forgive temporary", peer.ID())
		return nil
	}

	pindexBestKnownBlock := chain.GetInstance().FindBlockIndex(*pindexBestKnownHash)
	if pindexBestKnownBlock == nil {
		log.Debug("blkIndex find fail of peer(%d). blkhash:%s", peer.ID(), *pindexBestKnownHash)
		return nil
	}

	return pindexBestKnownBlock
}

func (sm *SyncManager) syncPoints(peer *peer.Peer) (pindexWalk, pindexBestKnownBlock *blockindex.BlockIndex) {
	gChain := chain.GetInstance()
	pindexBestKnownBlock = lastAccouncedBlock(peer)
	if pindexBestKnownBlock == nil {
		// update best block of this peer
		pindexStart := gChain.GetIndexBestHeader()
		if pindexStart.Prev != nil {
			pindexStart = pindexStart.Prev
		}
		locator := gChain.GetLocator(pindexStart)
		peer.PushGetHeadersMsg(*locator, &zeroHash)
		return nil, nil
	}

	if lchain.IsInitialBlockDownload() {
		if gChain.Tip().Height > pindexBestKnownBlock.Height ||
			gChain.Tip().ChainWork.Cmp(&pindexBestKnownBlock.ChainWork) == 1 {
			return nil, nil
		}
		return gChain.Tip(), pindexBestKnownBlock
	}

	return gChain.FindFork(pindexBestKnownBlock), pindexBestKnownBlock
}

// fetchHeaderBlocks creates and sends a request to the peer for the next
// list of blocks to be downloaded based on the current known headers.
// Download blocks via several peers parallel
func (sm *SyncManager) fetchHeaderBlocks(peer *peer.Peer) {
	reqNum := len(sm.requestedBlocks)
	if 0 != reqNum {
		log.Debug("now %d requestedBlocks", reqNum)
	}
	if !sm.isSyncCandidate(peer) {
		log.Info("peer(%d)%s not a sync candidate, forgive fetch", peer.ID(), peer.Addr())
		return
	}

	if !peer.VerAckReceived() {
		log.Info("peer(%d)%s VerAck not recved, do not use it", peer.ID(), peer.Addr())
		return
	}
	gChain := chain.GetInstance()
	peerState, exists := sm.peerStates[peer]
	if !exists {
		log.Error("fetchHeaderBlocks called with peer state nil")
		return
	}

	if len(peerState.requestedBlocks) == MAX_BLOCKS_IN_TRANSIT_PER_PEER {
		log.Debug("peer(%d) has full requestedBlocks, don't GetData any more", peer.ID())
		return
	}

	minWorkSum := pow.MiniChainWork()
	pindexBestHeader := gChain.GetIndexBestHeader()
	if pindexBestHeader.ChainWork.Cmp(&minWorkSum) == -1 {
		log.Info("pindexBestHeader.ChainWork less than minChainWork, wait header download", peer.ID())
		return
	}

	pindexWalk, pindexBestKnownBlock := sm.syncPoints(peer)
	if pindexWalk == nil || pindexBestKnownBlock == nil {
		log.Debug("fetchHeaderBlocks can not find block hashes to fetch from peer(%d) ", peer.ID())
		return
	}

	if pindexBestKnownBlock.ChainWork.Cmp(&minWorkSum) == -1 {
		log.Info("peer(%d) ChainWork less than minChainWork, do not use this peer", peer.ID())
		return
	}

	tipWork := gChain.Tip().ChainWork
	if pindexBestKnownBlock.ChainWork.Cmp(&tipWork) == -1 {
		log.Info("peer(%d) ChainWork less than tipWork, has nothing interesting", peer.ID())
		return
	}

	vToFetch := list.New()
	// Never fetch further than the best block we know the peer has, or more
	// than BLOCK_DOWNLOAD_WINDOW + 1 beyond the last linked block we have in
	// common with this peer. The +1 is so we can detect stalling, namely if we
	// would be able to download that next block if the window were 1 larger.
	nWindowEnd := pindexWalk.Height + BLOCK_DOWNLOAD_WINDOW
	nMaxHeight := util.MinI32(pindexBestKnownBlock.Height, nWindowEnd+1)

	gdmsg := wire.NewMsgGetData()

	waitingfor := peer
	waitingfor = nil

out:
	for pindexWalk.Height < nMaxHeight {
		// Read up to 128 (or more, if more blocks than that are needed)
		// successors of pindexWalk (towards pindexBestKnownBlock) into
		// vToFetch. We fetch 128, because CBlockIndex::GetAncestor may be as
		// expensive as iterating over ~100 CBlockIndex* entries anyway.
		nToFetch := util.MinI32(nMaxHeight-pindexWalk.Height, 128)
		pindexWalk = pindexBestKnownBlock.GetAncestor(pindexWalk.Height + nToFetch)
		vToFetch.PushFront(pindexWalk)
		pindex := pindexWalk
		for i := nToFetch - 1; i > 0; i-- {
			vToFetch.PushFront(pindex.Prev)
			pindex = pindex.Prev
		}
		// Iterate over those blocks in vToFetch (in forward direction), adding
		// the ones that are not yet downloaded and not in flight to msg.
		for e := vToFetch.Front(); e != nil; e = e.Next() {
			pindex := e.Value.(*blockindex.BlockIndex)
			if !pindex.IsValid(blockindex.BlockValidTree) {
				// We consider the chain that this peer is on invalid.
				break out
			}
			if pindex.HasData() {
				continue
			}
			if waitpeer, exists := sm.requestedBlocks[*pindex.GetBlockHash()]; exists {
				// now in flight
				if waitingfor == nil {
					waitingfor = waitpeer
				}
				continue
			}
			if pindex.Height > nWindowEnd {
				if len(gdmsg.InvList) == 0 && waitingfor != nil && waitingfor != peer {
					log.Info("window stalled by peer(%d) %s",
						waitingfor.ID(), waitingfor.Addr())
					if len(peerState.requestedBlocks) == 0 {
						stallsince := waitingfor.GetStallingSince()
						if stallsince == 0 {
							nNow := time.Now().UnixNano() / 1000
							waitingfor.SetStallingSince(nNow)
							log.Info("Stall started peer(%d)", waitingfor.ID())
						}
					}
				}
				break out
			}
			iv := wire.NewInvVect(wire.InvTypeBlock, pindex.GetBlockHash())
			sm.requestedBlocks[*pindex.GetBlockHash()] = peer
			peerState.requestedBlocks[*pindex.GetBlockHash()] = struct{}{}
			gdmsg.AddInvVect(iv)
			if len(peerState.requestedBlocks) == MAX_BLOCKS_IN_TRANSIT_PER_PEER {
				break out
			}
		}
	}

	if len(gdmsg.InvList) > 0 {
		log.Trace("ready to send getdata request, inv Number : %d", len(gdmsg.InvList))
		peer.QueueMessage(gdmsg, nil)
	}
}

func (sm *SyncManager) fetchHeadersToConnect(peer *peer.Peer, state *peerSyncState) {
	gChain := chain.GetInstance()

	//peer from functional test may send version message with -1 height,
	//and failed to be selected as syncPeer
	if lchain.IsInitialBlockDownload() && sm.syncPeer == nil {
		sm.syncPeer = peer
	}

	if lchain.IsInitialBlockDownload() && sm.syncPeer != peer {
		log.Debug("IBD: unrequested headers from nonSyncPeer: %v, maybe new header announce", peer.Addr())
		//ignore headers from nonSyncPeer, but we can try to get blocks from the peer
		sm.fetchHeaderBlocks(peer)
		return
	}

	state.unconnectingHeaders++
	pindexBestHeader := gChain.GetIndexBestHeader()
	peer.PushGetHeadersMsg(*gChain.GetLocator(pindexBestHeader), &zeroHash)

	log.Debug("recv headers cannot connect, send getheaders (%d) to peer %v. IBD:%t, unconnectingHeaders:%d",
		pindexBestHeader.Height, peer.Addr(), lchain.IsInitialBlockDownload(),
		state.unconnectingHeaders)

	if state.unconnectingHeaders%MAX_UNCONNECTING_HEADERS == 0 {
		sm.misbehaving(peer.Addr(), 20, "too-many-unconnected-headers")
	}
}

// handleHeadersMsg handles block header messages from all peers.  Headers are
// requested when performing a headers-first sync.
func (sm *SyncManager) handleHeadersMsg(hmsg *headersMsg) {
	gChain := chain.GetInstance()
	peer := hmsg.peer
	headers := hmsg.headers.Headers
	log.Info("Received %d block headers from peer %s", len(headers), peer.Addr())

	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warn("recv headers from unknown peer %s", peer.Addr(), len(headers))
		return
	}
	if len(headers) == 0 {
		sm.fetchHeaderBlocks(peer)
		log.Info("recv 0 headers from peer %s", peer.Addr())
		return
	}

	if canNotConnect := gChain.FindBlockIndex(headers[0].HashPrevBlock) == nil; canNotConnect {
		sm.fetchHeadersToConnect(peer, state)
		return
	}

	if isContinuousHeaders := hmsg.headers.IsContinuousHeaders(); !isContinuousHeaders {
		log.Warn("recv non-continuous headers from %v ", peer.Addr())
		peer.Disconnect()
		return
	}

	peerTip := sm.updatePeerState(headers, peer, gChain)

	var pindexLast blockindex.BlockIndex
	if err := sm.ProcessBlockHeadCallBack(headers, &pindexLast); err != nil {
		beginHash := headers[0].GetHash()
		log.Warn("processblockheader error, begin: %s, end: %s, err: %s.", beginHash, peerTip, err.Error())
		return
	}

	if state.unconnectingHeaders > 0 {
		log.Info("peer=%d: resetting unconnectingHeaders (%d -> 0)", peer.ID(), state.unconnectingHeaders)
		state.unconnectingHeaders = 0
	}

	hasMore := len(headers) == wire.MaxBlockHeadersPerMsg
	if hasMore && peer == sm.syncPeer {
		blkIndex := gChain.FindBlockIndex(peerTip)
		peer.PushGetHeadersMsg(*gChain.GetLocator(blkIndex), &zeroHash)
		log.Info("send more getheaders (%d) to peer %s", blkIndex.Height, peer.Addr())
	}

	// If this set of headers is valid and ends in a block with at least
	// as much work as our tip, download as much as possible.
	if !pindexLast.IsValid(blockindex.BlockValidTree) {
		log.Info("no need to fetch, pindexLast not ValidTree")
		return
	}

	if gChain.Tip().ChainWork.Cmp(&pindexLast.ChainWork) == 1 {
		log.Info("no need to fetch, TipChainWork>pindexLast")
		return
	}

	canDirectFetch := gChain.CanDirectFetch()
	if canDirectFetch {
		vToFetch, isLargeReorg := sm.blocksToFetch(pindexLast)
		if isLargeReorg {
			log.Info("Large reorg, won't direct fetch to %s (%d)", *pindexLast.GetBlockHash(), pindexLast.Height)
			sm.fetchHeaderBlocks(peer)
			return
		}

		sm.fetchBlocks(vToFetch, state, peer)
		return
	}

	if len(sm.peerStates) <= 2 {
		sm.fetchHeaderBlocks(peer)
		return
	}
}

func (sm *SyncManager) updatePeerState(headers []*block.BlockHeader, peer *peer.Peer, gChain *chain.Chain) util.Hash {
	for _, header := range headers {
		peer.AddKnownInventory(&wire.InvVect{Type: wire.InvTypeBlock, Hash: header.GetHash()})
	}

	peerTip := headers[len(headers)-1].GetHash()
	peer.UpdateLastAnnouncedBlock(&peerTip)
	if sm.current() {
		if blkIndex := gChain.FindHashInActive(peerTip); blkIndex != nil {
			peer.UpdateLastBlockHeight(blkIndex.Height)
		}
	}
	return peerTip
}

func (sm *SyncManager) fetchBlocks(vToFetch *list.List, state *peerSyncState, peer *peer.Peer) {
	// Download as much as possible, from earliest to latest.
	gdmsg := wire.NewMsgGetData()
	for e := vToFetch.Front(); e != nil; e = e.Next() {
		if len(state.requestedBlocks) >= MAX_BLOCKS_IN_TRANSIT_PER_PEER {
			break
		}

		hash := *(e.Value.(*blockindex.BlockIndex).GetBlockHash())
		iv := wire.NewInvVect(wire.InvTypeBlock, &hash)
		gdmsg.AddInvVect(iv)

		sm.requestedBlocks[hash] = peer
		state.requestedBlocks[hash] = struct{}{}
		log.Debug("Requesting block %s from peer=%d", hash.String(), peer.ID())
	}

	if len(gdmsg.InvList) > 0 {
		log.Debug("Downloading blocks toward %s via headers direct fetch", gdmsg.InvList[0].Hash)
		peer.QueueMessage(gdmsg, nil)
	}
}

func (sm *SyncManager) blocksToFetch(pindexLast blockindex.BlockIndex) (*list.List, bool) {
	gChain := chain.GetInstance()
	vToFetch := list.New()
	pindexWalk := &pindexLast
	// Calculate all the blocks we'd need to switch to pindexLast, up to a limit.
	for pindexWalk != nil &&
		!gChain.Contains(pindexWalk) &&
		vToFetch.Len() <= MAX_BLOCKS_IN_TRANSIT_PER_PEER {

		if !pindexWalk.HasData() {
			if _, exists := sm.requestedBlocks[*pindexWalk.GetBlockHash()]; !exists {
				vToFetch.PushFront(pindexWalk)
			}
		}
		pindexWalk = pindexWalk.Prev
	}

	isLargeReorg := !gChain.Contains(pindexWalk)
	return vToFetch, isLargeReorg
}

// haveInventory returns whether or not the inventory represented by the passed
// inventory vector is known.  This includes checking all of the various places
// inventory can be when it is in different states such as blocks that are part
// of the main chain, on a side chain, in the orphan pool, and transactions that
// are in the memory pool (either the main pool or orphan pool).
func (sm *SyncManager) haveInventory(invVect *wire.InvVect) (bool, error) {
	activeChain := chain.GetInstance()
	switch invVect.Type {
	case wire.InvTypeBlock:
		// Ask chain if the block is known to it in any form (main
		// chain, side chain, or orphan).
		blkIndex := activeChain.FindBlockIndex(invVect.Hash)
		if blkIndex == nil {
			return false, nil
		}
		if blkIndex.HasData() {
			return true, nil
		}
		return false, nil

	case wire.InvTypeTx:
		// Ask the transaction memory pool if the transaction is known
		// to it in any form (main pool or orphan).
		if lmempool.FindTxInMempool(invVect.Hash) != nil {
			return true, nil
		}
		// Check if the transaction exists from the point of view of the
		// end of the main chain.
		pcoins := utxo.GetUtxoCacheInstance()
		out := outpoint.OutPoint{Hash: invVect.Hash, Index: 0}
		if pcoins.GetCoin(&out) != nil {
			return true, nil
		}
		out.Index = 1
		if pcoins.GetCoin(&out) != nil {
			return true, nil
		}
		if lmempool.FindOrphanTxInMemPool(invVect.Hash) != nil {
			return true, nil
		}
		return false, nil
	}

	// The requested inventory is is an unsupported type, so just claim
	// it is known to avoid requesting it.
	return true, nil
}

// handleInvMsg handles inv messages from all peers.
// We examine the inventory advertised by the remote peer and act accordingly.
func (sm *SyncManager) handleInvMsg(imsg *invMsg) {
	peer := imsg.peer
	state, exists := sm.peerStates[peer]
	if !exists {
		log.Warn("Received inv message from unknown peer %s", peer.Addr())
		return
	}

	log.Trace("Received INV msg, And current IBD:%v", lchain.IsInitialBlockDownload())

	// Attempt to find the final block in the inventory list.  There may
	// not be one.
	lastBlock := -1
	invVects := imsg.inv.InvList
	for i := len(invVects) - 1; i >= 0; i-- {
		if invVects[i].Type == wire.InvTypeBlock {
			lastBlock = i
			break
		}
	}

	if lastBlock != -1 {
		peer.CheckRevertToInv(&invVects[lastBlock].Hash, true)
	}

	// If this inv contains a block announcement, and this isn't coming from
	// our current sync peer or we're current, then update the last
	// announced block for this peer. We'll use this information later to
	// update the heights of peers based on blocks we've accepted that they
	// previously announced.
	if lastBlock != -1 && (peer != sm.syncPeer || sm.current()) {
		peer.UpdateLastAnnouncedBlock(&invVects[lastBlock].Hash)
	}

	activeChain := chain.GetInstance()
	// If our chain is current and a peer announces a block we already
	// know of, then update their current block height.
	if lastBlock != -1 && sm.current() {
		blkIndex := activeChain.FindHashInActive(invVects[lastBlock].Hash)
		if blkIndex != nil {
			peer.UpdateLastBlockHeight(blkIndex.Height)
		}
	}

	var invBlkCnt int
	// Request the advertised inventory if we don't already have it.  Also,
	// request parent blocks of orphans if we receive one we already have.
	// Finally, attempt to detect potential stalls due to long side chains
	// we already have and request more blocks to prevent them.
	for _, iv := range invVects {
		// Ignore unsupported inventory types.
		switch iv.Type {
		case wire.InvTypeBlock:
			invBlkCnt++
		case wire.InvTypeTx:
		default:
			continue
		}

		// Add the inventory to the cache of known inventory
		// for the peer.
		peer.AddKnownInventory(iv)

		// Request the inventory if we don't already have it.
		haveInv, err := sm.haveInventory(iv)
		if err != nil {
			log.Warn("Unexpected failure when checking for "+
				"existing inventory during inv message "+
				"processing: %v", err)
			continue
		}
		if !haveInv {
			if iv.Type == wire.InvTypeTx {
				// Skip the transaction if it has already been
				// rejected.
				if _, exists := sm.rejectedTxns[iv.Hash]; exists {
					continue
				}
			}

			// Add it to the request queue.
			state.requestQueue = append(state.requestQueue, iv)
		}
	}

	log.Debug(
		"invBlkCnt=%d len(invVects)=%d peer=%p(%s) sm.syncPeer=%p",
		invBlkCnt, len(invVects), peer, peer.Addr(), sm.syncPeer)

	// Request as much as possible at once.  Anything that won't fit into
	// the request will be requested on the next inv message.
	numRequested := 0
	gdmsg := wire.NewMsgGetData()
	requestQueue := state.requestQueue
	for len(requestQueue) != 0 {
		iv := requestQueue[0]
		requestQueue[0] = nil
		requestQueue = requestQueue[1:]

		switch iv.Type {
		case wire.InvTypeBlock:
			// Request the block if there is not already a pending
			// request.
			if _, exists := sm.requestedBlocks[iv.Hash]; !exists {
				pindexBestHeader := activeChain.GetIndexBestHeader()
				locator := activeChain.GetLocator(pindexBestHeader)
				log.Info("Syncing to block height %d from peer %v",
					peer.LastBlock(), peer.Addr())

				peer.PushGetHeadersMsg(*locator, &iv.Hash)
			}

		case wire.InvTypeTx:
			// Request the transaction if there is not already a
			// pending request.
			if _, exists := sm.requestedTxns[iv.Hash]; !exists {
				sm.requestedTxns[iv.Hash] = struct{}{}
				sm.limitMap(sm.requestedTxns, maxRequestedTxns)
				state.requestedTxns[iv.Hash] = struct{}{}
				gdmsg.AddInvVect(iv)
				numRequested++
			}
		}

		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}

	state.requestQueue = requestQueue
	if len(gdmsg.InvList) > 0 {
		peer.QueueMessage(gdmsg, nil)
	}
}

// limitMap is a helper function for maps that require a maximum limit by
// evicting a random transaction if adding a new value would cause it to
// overflow the maximum allowed.
func (sm *SyncManager) limitMap(m map[util.Hash]struct{}, limit int) {
	if len(m)+1 > limit {
		// Remove a random entry from the map.  For most compilers, Go's
		// range statement iterates starting at a random item although
		// that is not 100% guaranteed by the spec.  The iteration order
		// is not important here because an adversary would have to be
		// able to pull off preimage attacks on the hashing function in
		// order to target eviction of specific entries anyways.
		for txHash := range m {
			delete(m, txHash)
			return
		}
	}
}

func (sm *SyncManager) scanToFetchHeaderBlocks() {
	for peer, state := range sm.peerStates {
		if !state.syncCandidate {
			continue
		}

		// detect whether we're stalling the concurrent download window
		now := time.Now().UnixNano() / 1000
		stallsince := peer.GetStallingSince()
		if stallsince != 0 && stallsince < now-BLOCK_STALLING_TIMEOUT {
			// Stalling only triggers when the block download window cannot move.
			// During normal steady state, the download window should be much larger
			// than the to-be-downloaded set of blocks, so disconnection should only
			// happen during initial block download.
			log.Info("Peer(%d)%s is stalling block download, disconnecting",
				peer.ID(), peer.Addr())
			peer.Disconnect()
			continue
		}

		// try fetch
		if len(state.requestedBlocks) < MAX_BLOCKS_IN_TRANSIT_PER_PEER {
			sm.fetchHeaderBlocks(peer)
		}
	}
}

// messagesHandler is the main handler for the sync manager.  It must be run as a
// goroutine.  It processes block and inv messages in a separate goroutine
// from the peer handlers so the block (MsgBlock) messages are handled by a
// single thread without needing to lock memory data structures.  This is
// important because the sync manager controls which blocks are needed and how
// the fetching should proceed.
func (sm *SyncManager) messagesHandler() {
	fetchTicker := time.NewTicker(fetchInterval)
	defer fetchTicker.Stop()
out:
	for {
		select {
		//for all peer to try fetchHeaderBlocks
		case <-fetchTicker.C:
			sm.scanToFetchHeaderBlocks()

		//business msg
		case m := <-sm.processBusinessChan:
			switch msg := m.(type) {
			case *newPeerMsg:
				sm.handleNewPeerMsg(msg.peer)

			case *txMsg:
				sm.handleTxMsg(msg)
				msg.reply <- struct{}{}

			case *blockMsg:
				sm.handleBlockMsg(msg)
				msg.reply <- struct{}{}

			case *invMsg:
				sm.handleInvMsg(msg)

			case *headersMsg:
				sm.handleHeadersMsg(msg)

			case *poolMsg:
				if msg.peer.Cfg.Listeners.OnMemPool != nil {
					msg.peer.Cfg.Listeners.OnMemPool(msg.peer, msg.pool)
				}
				msg.reply <- struct{}{}
			case getBlocksMsg:
				if msg.peer.Cfg.Listeners.OnGetBlocks != nil {
					msg.peer.Cfg.Listeners.OnGetBlocks(msg.peer, msg.getblocks)
				}
				msg.reply <- struct{}{}

			case *donePeerMsg:
				sm.handleDonePeerMsg(msg.peer)

			case getSyncPeerMsg:
				var peerID int32
				if sm.syncPeer != nil {
					peerID = sm.syncPeer.ID()
				}
				msg.reply <- peerID

			case isCurrentMsg:
				msg.reply <- sm.current()

			case pauseMsg:
				// Wait until the sender unpauses the manager.
				<-msg.unpause

			case *minedBlockMsg:
				sm.handleMinedBlockMsg(msg)
			default:
				log.Warn("Invalid message type in block "+
					"handler: %T, %#v", msg, msg)
			}

		case <-sm.quit:
			break out
		}
	}

	sm.wg.Done()
	log.Trace("Block handler done")
}

// handleBlockchainNotification handles notifications from blockchain.  It does
// things such as request orphan block parents and relay accepted blocks to
// connected peers.
func (sm *SyncManager) handleBlockchainNotification(notification *chain.Notification) {
	switch notification.Type {

	case chain.NTChainTipUpdated:
		event, ok := notification.Data.(*chain.TipUpdatedEvent)
		if !ok {
			panic("TipUpdatedEvent: malformed event payload")
		}

		sm.peerNotifier.RelayUpdatedTipBlocks(event)

	// A block has been accepted into the block chain.  Relay it to other peers.
	case chain.NTNewPoWValidBlock:
		block, ok := notification.Data.(*block.Block)
		if !ok {
			log.Warn("Chain accepted notification is not a block.")
			break
		}

		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeBlock, &block.Header.Hash)
		sm.peerNotifier.RelayInventory(iv, &block.Header)

	// A block has been connected to the main block chain.
	case chain.NTBlockConnected:
		block, ok := notification.Data.(*block.Block)
		if !ok {
			log.Warn("Chain connected notification is not a block.")
			break
		}

		// Remove all of the transactions (except the coinbase) in the
		// connected block from the transaction pool.  Secondly, remove any
		// transactions which are now double spends as a result of these
		// new transactions.  Finally, remove any transaction that is
		// no longer an orphan. Transactions which depend on a confirmed
		// transaction are NOT removed recursively because they are still
		// valid.
		lmempool.RemoveTxSelf(block.Txs[1:])
		for _, tx := range block.Txs[1:] {
			// TODO: add it back when rcp command @SendRawTransaction is ready for broadcasting tx
			// sm.peerNotifier.TransactionConfirmed(tx)

			lmempool.TryAcceptOrphansTxs(tx, chain.GetInstance().Height(), true)
		}

		// Register block with the fee estimator, if it exists.
		//if sm.feeEstimator != nil {
		//	err := sm.feeEstimator.RegisterBlock(block)
		//
		//	// If an error is somehow generated then the fee estimator
		//	// has entered an invalid state. Since it doesn't know how
		//	// to recover, create a new one.
		//	if err != nil {
		//		sm.feeEstimator = mempool.NewFeeEstimator(
		//			mempool.DefaultEstimateFeeMaxRollback,
		//			mempool.DefaultEstimateFeeMinRegisteredBlocks)
		//	}
		//}

		// A block has been disconnected from the main block chain.
	case chain.NTBlockDisconnected:
		_, ok := notification.Data.(*block.Block)
		if !ok {
			log.Warn("Chain disconnected notification is not a block.")
			break
		}

		// Rollback previous block recorded by the fee estimator.
		//if sm.feeEstimator != nil {
		//	sm.feeEstimator.Rollback(&block.Header.Hash)
		//}
	}
}

// NewPeer informs the sync manager of a newly active peer.
//
func (sm *SyncManager) NewPeer(peer *peer.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}
	sm.processBusinessChan <- &newPeerMsg{peer: peer}
}

// QueueTx adds the passed transaction message and peer to the block handling
// queue. Responds to the done channel argument after the tx message is
// processed.
func (sm *SyncManager) QueueTx(tx *tx.Tx, peer *peer.Peer, done chan<- struct{}) {
	// Don't accept more transactions if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.processBusinessChan <- &txMsg{tx: tx, peer: peer, reply: done}
}

func (sm *SyncManager) QueueMinedBlock(block *block.Block, done chan error) {
	sm.processBusinessChan <- &minedBlockMsg{block: block, reply: done}
}

// QueueBlock adds the passed block message and peer to the block handling
// queue. Responds to the done channel argument after the block message is
// processed.
func (sm *SyncManager) QueueBlock(block *block.Block, buf []byte, peer *peer.Peer, done chan<- struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.processBusinessChan <- &blockMsg{block: block, buf: buf, peer: peer, reply: done}
}

func (sm *SyncManager) QueueMessgePool(pool *wire.MsgMemPool, peer *peer.Peer, done chan<- struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.processBusinessChan <- &poolMsg{pool, peer, done}
}

func (sm *SyncManager) QueueGetBlocks(getblocks *wire.MsgGetBlocks, peer *peer.Peer, done chan<- struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.processBusinessChan <- getBlocksMsg{getblocks, peer, done}
}

func (sm *SyncManager) QueuePing(ping *wire.MsgPing, peer *peer.Peer, done chan<- struct{}) {
	// Don't accept more blocks if we're shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		done <- struct{}{}
		return
	}

	sm.processBusinessChan <- pingMsg{ping, peer, done}
}

// QueueInv adds the passed inv message and peer to the block handling queue.
func (sm *SyncManager) QueueInv(inv *wire.MsgInv, peer *peer.Peer) {
	// No channel handling here because peers do not need to block on inv
	// messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.processBusinessChan <- &invMsg{inv: inv, peer: peer}
}

// QueueHeaders adds the passed headers message and peer to the block handling
// queue.
func (sm *SyncManager) QueueHeaders(headers *wire.MsgHeaders, peer *peer.Peer) {
	// No channel handling here because peers do not need to block on
	// headers messages.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.processBusinessChan <- &headersMsg{headers: headers, peer: peer}
}

// DonePeer informs the blockmanager that a peer has disconnected.
func (sm *SyncManager) DonePeer(peer *peer.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&sm.shutdown) != 0 {
		return
	}

	sm.processBusinessChan <- &donePeerMsg{peer: peer}
}

// Start begins the core block handler which processes block and inv messages.
func (sm *SyncManager) Start() {
	// Already started?
	if atomic.AddInt32(&sm.started, 1) != 1 {
		return
	}

	log.Trace("Starting sync manager")
	sm.wg.Add(1)
	go sm.messagesHandler()
}

// Stop gracefully shuts down the sync manager by stopping all asynchronous
// handlers and waiting for them to finish.
func (sm *SyncManager) Stop() error {
	if atomic.AddInt32(&sm.shutdown, 1) != 1 {
		log.Warn("Sync manager is already in the process of " +
			"shutting down")
		return nil
	}

	log.Info("Sync manager shutting down")
	close(sm.quit)
	sm.wg.Wait()
	return nil
}

// SyncPeerID returns the ID of the current sync peer, or 0 if there is none.
func (sm *SyncManager) SyncPeerID() int32 {
	reply := make(chan int32)
	sm.processBusinessChan <- getSyncPeerMsg{reply: reply}
	return <-reply
}

// IsCurrent returns whether or not the sync manager believes it is synced with
// the connected peers.
func (sm *SyncManager) IsCurrent() bool {
	reply := make(chan bool)
	sm.processBusinessChan <- isCurrentMsg{reply: reply}
	return <-reply
}

// Pause pauses the sync manager until the returned channel is closed.
//
// Note that while paused, all peer and block processing is halted.  The
// message sender should avoid pausing the sync manager for long durations.
func (sm *SyncManager) Pause() chan<- struct{} {
	c := make(chan struct{})
	sm.processBusinessChan <- pauseMsg{c}
	return c
}

func (sm *SyncManager) misbehaving(peerAddr string, banScore uint32, reason string) {
	sm.AddBanScoreCallBack(peerAddr, banScore, 0, reason)
}

// New constructs a new SyncManager. Use Start to begin processing asynchronous
// block, tx, and inv updates.
func New(config *Config) (*SyncManager, error) {
	sm := SyncManager{
		peerNotifier:        config.PeerNotifier,
		chainParams:         config.ChainParams,
		rejectedTxns:        make(map[util.Hash]struct{}),
		requestedTxns:       make(map[util.Hash]struct{}),
		requestedBlocks:     make(map[util.Hash]*peer.Peer),
		peerStates:          make(map[*peer.Peer]*peerSyncState),
		progressLogger:      newBlockProgressLogger("Processed", log.GetLogger()),
		processBusinessChan: make(chan interface{}, config.MaxPeers*3),
		quit:                make(chan struct{}),
	}
	//chain.InitGlobalChain(nil)
	best := chain.GetInstance().Tip()
	if best == nil {
		panic("best is nil")
	}

	chain.GetInstance().Subscribe(sm.handleBlockchainNotification)

	return &sm, nil
}

// PeerNotifier exposes methods to notify peers of status changes to
// transactions, blocks, etc. Currently server (in the main package) implements
// this interface.
type PeerNotifier interface {
	AnnounceNewTransactions(newTxs []*mempool.TxEntry)

	UpdatePeerHeights(latestBlkHash *util.Hash, latestHeight int32, updateSource *peer.Peer)

	RelayInventory(invVect *wire.InvVect, data interface{})

	RelayUpdatedTipBlocks(event *chain.TipUpdatedEvent)

	TransactionConfirmed(tx *tx.Tx)
}

// Config is a configuration struct used to initialize a new SyncManager.
type Config struct {
	PeerNotifier PeerNotifier
	ChainParams  *model.BitcoinParams

	MaxPeers int
}
