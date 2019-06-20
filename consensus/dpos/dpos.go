// Copyright 2018 The Fractal Team Authors
// This file is part of the fractal project.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package dpos

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/fractalplatform/fractal/accountmanager"
	"github.com/fractalplatform/fractal/common"
	"github.com/fractalplatform/fractal/consensus"
	"github.com/fractalplatform/fractal/crypto"
	"github.com/fractalplatform/fractal/snapshot"
	"github.com/fractalplatform/fractal/state"
	"github.com/fractalplatform/fractal/types"
	lru "github.com/hashicorp/golang-lru"
)

var (
	errMissingSignature       = errors.New("extra-data 65 byte suffix signature missing")
	errInvalidMintBlockTime   = errors.New("invalid time to mint the block")
	errInvalidBlockCandidate  = errors.New("invalid block candidate")
	errInvalidTimestamp       = errors.New("invalid timestamp")
	ErrIllegalCandidateName   = errors.New("illegal candidate name")
	ErrIllegalCandidatePubKey = errors.New("illegal candidate pubkey")
	ErrTooMuchRreversible     = errors.New("too much rreversible blocks")
	ErrSystemTakeOver         = errors.New("system account take over")
	errUnknownBlock           = errors.New("unknown block")
	extraSeal                 = 65
	timeOfGenesisBlock        int64
)

type stateDB struct {
	name    string
	assetid uint64
	state   *state.StateDB
}

func (s *stateDB) GetSnapshot(key string, timestamp uint64) ([]byte, error) {
	snapshotManager := snapshot.NewSnapshotManager(s.state)
	return snapshotManager.GetSnapshotMsg(s.name, key, timestamp)
}

func (s *stateDB) Get(key string) ([]byte, error) {
	return s.state.Get(s.name, key)
}
func (s *stateDB) Put(key string, value []byte) error {
	s.state.Put(s.name, key, value)
	return nil
}
func (s *stateDB) Delete(key string) error {
	s.state.Delete(s.name, key)
	return nil
}
func (s *stateDB) Undelegate(to string, amount *big.Int) (*types.Action, error) {
	action := types.NewAction(types.Transfer, common.StrToName(s.name), common.StrToName(to), 0, s.assetid, 0, amount, nil, nil)
	accountDB, err := accountmanager.NewAccountManager(s.state)
	if err != nil {
		return action, err
	}
	return action, accountDB.TransferAsset(common.StrToName(s.name), common.StrToName(to), s.assetid, amount)
}
func (s *stateDB) IncAsset2Acct(from string, to string, amount *big.Int) (*types.Action, error) {
	action := types.NewAction(types.IncreaseAsset, common.StrToName(s.name), common.StrToName(to), 0, s.assetid, 0, amount, nil, nil)
	accountDB, err := accountmanager.NewAccountManager(s.state)
	if err != nil {
		return action, err
	}
	return action, accountDB.IncAsset2Acct(common.StrToName(from), common.StrToName(to), s.assetid, amount)
}
func (s *stateDB) IsValidSign(name string, pubkey []byte) bool {
	accountDB, err := accountmanager.NewAccountManager(s.state)
	if err != nil {
		return false
	}
	return accountDB.IsValidSign(common.StrToName(name), common.BytesToPubKey(pubkey)) == nil
}
func (s *stateDB) GetBalanceByTime(name string, timestamp uint64) (*big.Int, error) {
	accountDB, err := accountmanager.NewAccountManager(s.state)
	if err != nil {
		return big.NewInt(0), err
	}
	balance, err := accountDB.GetBalanceByTime(common.StrToName(name), s.assetid, 1, timestamp)
	if err == accountmanager.ErrAccountNotExist {
		return big.NewInt(0), nil
	}
	return balance, err
}

// Genesis dpos genesis store
func Genesis(cfg *Config, state *state.StateDB, timestamp uint64, number uint64) error {
	sys := NewSystem(state, cfg)

	epoch := cfg.epoch(timestamp)
	if err := sys.SetLastestEpoch(epoch); err != nil {
		return err
	}
	if err := sys.SetState(&GlobalState{
		Epoch:                  epoch,
		PreEpoch:               epoch,
		ActivatedTotalQuantity: big.NewInt(0),
		TotalQuantity:          big.NewInt(0),
		OffCandidateNumber:     []uint64{},
		OffCandidateSchedule:   []uint64{},
		Number:                 number,
	}); err != nil {
		return err
	}
	if err := sys.SetCandidate(&CandidateInfo{
		Epoch:         epoch,
		Name:          cfg.SystemName,
		URL:           cfg.SystemURL,
		Quantity:      big.NewInt(0),
		TotalQuantity: big.NewInt(0),
		Number:        number,
	}); err != nil {
		return err
	}
	return nil
}

// SignFn signature function
type SignFn func([]byte, *state.StateDB) ([]byte, error)

// Dpos dpos engine
type Dpos struct {
	rw sync.RWMutex

	signFn SignFn

	config *Config

	// cache
	bftIrreversibles *lru.Cache
}

// New creates a DPOS consensus engine
func New(config *Config, chain consensus.IChainReader) *Dpos {
	dpos := &Dpos{
		config: config,
	}
	dpos.bftIrreversibles, _ = lru.New(int(config.CandidateScheduleSize))
	return dpos
}

// SetConfig set dpos config
func (dpos *Dpos) SetConfig(config *Config) {
	dpos.rw.Lock()
	defer dpos.rw.Unlock()
	dpos.config = config
}

// Config return dpos config
func (dpos *Dpos) Config() *Config {
	dpos.rw.RLock()
	defer dpos.rw.RUnlock()
	return dpos.config
}

// SetSignFn set signature function
func (dpos *Dpos) SetSignFn(signFn SignFn) {
	dpos.rw.Lock()
	defer dpos.rw.Unlock()
	dpos.signFn = signFn
}

// Author implements consensus.Engine, returning the header's coinbase
func (dpos *Dpos) Author(header *types.Header) (common.Name, error) {
	return header.Coinbase, nil
}

// Prepare initializes the consensus fields of a block header according to the rules of a particular engine. The changes are executed inline.
func (dpos *Dpos) Prepare(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) error {
	if fid := header.CurForkID(); fid >= 1 {
		return dpos.prepare1(chain, header, txs, receipts, state)
	}
	return dpos.prepare0(chain, header, txs, receipts, state)
}

func (dpos *Dpos) prepare0(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) error {
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)
	sys := NewSystem(state, dpos.config)
	parent := chain.GetHeaderByHash(header.ParentHash)
	pepoch := dpos.config.epoch(parent.Time.Uint64())
	epoch := dpos.config.epoch(header.Time.Uint64())
	if header.Number.Uint64() != 1 {
		gstate, err := sys.GetState(LastEpoch)
		if err != nil {
			return err
		}
		if dpos.CalcProposedIrreversible(chain, parent, true) == 0 || header.Time.Uint64()-parent.Time.Uint64() > 2*dpos.config.mepochInterval() {
			if systemio := strings.Compare(header.Coinbase.String(), dpos.config.SystemName) == 0; systemio {
				gstate.TakeOver = true
				if err := sys.SetState(gstate); err != nil {
					return err
				}
			}
		}
		pstate, err := sys.GetState(gstate.PreEpoch)
		if err != nil {
			return err
		}

		//  pepoch missing
		if timestamp := parent.Time.Uint64() + dpos.config.blockInterval(); timestamp < header.Time.Uint64() {
			etimestamp := sys.config.epochTimeStamp(gstate.Epoch+1) + 2*sys.config.blockInterval()
			if header.Time.Uint64() < etimestamp {
				etimestamp = header.Time.Uint64()
			}
			poffset := dpos.config.getoffset(parent.Time.Uint64(), 0)
			for ; timestamp < etimestamp; timestamp += dpos.config.blockInterval() {
				coffset := dpos.config.getoffset(timestamp, 0)
				if coffset != poffset {
					if coffset >= uint64(len(pstate.ActivatedCandidateSchedule)) {
						continue
					}
					name := pstate.ActivatedCandidateSchedule[coffset]
					for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
						roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
						if roffset == coffset {
							name = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
							break
						}
					}
					pcandidate, err := sys.GetCandidate(gstate.Epoch, name)
					if err != nil {
						return err
					}
					c := dpos.config.shouldCounter(timestamp, etimestamp)
					pcandidate.Counter += c
					log.Debug("should counter missing++", "add", c, "candidate", pcandidate.Name, "should", pcandidate.Counter, "actual", pcandidate.ActualCounter, "number", header.Number)
					if err := sys.SetCandidate(pcandidate); err != nil {
						return err
					}
				}
				poffset = coffset
			}
		}

		candidate, err := sys.GetCandidate(gstate.Epoch, header.Coinbase.String())
		if err != nil {
			return err
		}
		if candidate != nil {
			candidate.ActualCounter++
			if gstate.TakeOver {
				candidate.Counter++
			} else if /*dpos.config.getoffset(parent.Time.Uint64())*/ dpos.config.getoffset(header.Time.Uint64()-dpos.config.blockInterval(), 0) != dpos.config.getoffset(header.Time.Uint64(), 0) ||
				strings.Compare(parent.Coinbase.String(), header.Coinbase.String()) != 0 {
				etimestamp := sys.config.epochTimeStamp(gstate.Epoch+1) + 2*sys.config.blockInterval()
				c := dpos.config.shouldCounter(header.Time.Uint64(), etimestamp)
				candidate.Counter += c
				log.Debug("should counter ++", "add", c, "candidate", candidate.Name, "should", candidate.Counter, "actual", candidate.ActualCounter, "number", header.Number)
			}
			if err := sys.SetCandidate(candidate); err != nil {
				return err
			}
		}
	}

	if pepoch != epoch {
		log.Debug("UpdateElectedCandidates", "prev", pepoch, "curr", epoch, "number", parent.Number.Uint64(), "time", parent.Time.Uint64())
		sys.UpdateElectedCandidates0(pepoch, epoch, parent.Number.Uint64(), header.Coinbase.String())
		if timestamp := parent.Time.Uint64() + dpos.config.blockInterval(); parent.Number.Uint64() > 0 && timestamp < header.Time.Uint64() {
			gstate, err := sys.GetState(LastEpoch)
			if err != nil {
				return err
			}
			pstate, err := sys.GetState(gstate.PreEpoch)
			if err != nil {
				return err
			}
			stimestamp := dpos.config.epochTimeStamp(gstate.Epoch) + sys.config.blockInterval()
			if stimestamp < parent.Time.Uint64() {
				stimestamp = parent.Time.Uint64()
			}

			poffset := dpos.config.getoffset(stimestamp, 0)
			for stimestamp += dpos.config.blockInterval(); stimestamp < header.Time.Uint64(); stimestamp += dpos.config.blockInterval() {
				coffset := dpos.config.getoffset(stimestamp, 0)
				if coffset != poffset {
					if coffset >= uint64(len(pstate.ActivatedCandidateSchedule)) {
						continue
					}
					name := pstate.ActivatedCandidateSchedule[coffset]
					for index, roffset := range pstate.OffCandidateSchedule {
						if roffset == coffset {
							name = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(index)]
							break
						}
					}
					pcandidate, err := sys.GetCandidate(gstate.Epoch, name)
					if err != nil {
						return err
					}
					c := dpos.config.shouldCounter(stimestamp, header.Time.Uint64())
					pcandidate.Counter += c
					log.Debug("should counter ++missing", "add", c, "candidate", pcandidate.Name, "should", pcandidate.Counter, "actual", pcandidate.ActualCounter, "number", header.Number)
					if err := sys.SetCandidate(pcandidate); err != nil {
						return err
					}
				}
				poffset = coffset
			}
		}
	}

	if header.Number.Uint64() == 1 {
		candidate, err := sys.GetCandidate(epoch, header.Coinbase.String())
		if err != nil {
			return err
		}
		if candidate != nil {
			candidate.ActualCounter++
			c := dpos.config.shouldCounter(header.Time.Uint64(), sys.config.epochTimeStamp(epoch+1)+2*sys.config.blockInterval())
			candidate.Counter += c
			log.Debug("should counter ++", "add", c, "candidate", candidate.Name, "should", candidate.Counter, "actual", candidate.ActualCounter, "number", header.Number)
			if err := sys.SetCandidate(candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func (dpos *Dpos) prepare1(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) error {
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)
	return nil
}

// Finalize assembles the final block.
func (dpos *Dpos) Finalize(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) (*types.Block, error) {
	if chain == nil {
		header.Root = state.IntermediateRoot()
		return types.NewBlock(header, txs, receipts), nil
	}
	if fid := header.CurForkID(); fid >= 1 {
		return dpos.finalize1(chain, header, txs, receipts, state)
	}
	return dpos.finalize0(chain, header, txs, receipts, state)
}

func (dpos *Dpos) finalize0(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) (*types.Block, error) {
	sys := NewSystem(state, dpos.config)
	counter := int64(0)
	extraReward := new(big.Int).Mul(dpos.config.extraBlockReward(), big.NewInt(counter))
	reward := new(big.Int).Add(dpos.config.blockReward(), extraReward)
	sys.IncAsset2Acct(dpos.config.SystemName, header.Coinbase.String(), reward)

	blk := types.NewBlock(header, txs, receipts)

	// first hard fork at a specific number
	// If the block number is greater than or equal to the hard forking number,
	// the fork function will take effect. This function is valid only in the test network.
	if err := chain.ForkUpdate(blk, state); err != nil {
		return nil, err
	}

	prevHeader := chain.GetHeaderByHash(blk.ParentHash())
	snapshotInterval := chain.Config().SnapshotInterval * uint64(time.Millisecond)
	prevTime := prevHeader.Time.Uint64()
	prevTimeFormat := prevTime / snapshotInterval * snapshotInterval

	currentTime := blk.Time().Uint64()
	currentTimeFormat := currentTime / snapshotInterval * snapshotInterval

	if prevTimeFormat != currentTimeFormat {
		snapshotManager := snapshot.NewSnapshotManager(state)
		err := snapshotManager.SetSnapshot(currentTimeFormat, snapshot.BlockInfo{Number: blk.NumberU64(), BlockHash: blk.ParentHash(), Timestamp: prevTimeFormat})
		if err != nil {
			return nil, err
		}
	}

	gstate, err := sys.GetState(LastEpoch)
	if err != nil {
		return nil, err
	}
	if timestamp := dpos.config.epochTimeStamp(gstate.Epoch) + dpos.config.blockInterval(); prevHeader.Time.Uint64() >= timestamp {
		pmepoch := (prevHeader.Time.Uint64() - timestamp) / dpos.config.mepochInterval()
		if mepoch := (header.Time.Uint64() - timestamp) / dpos.config.mepochInterval(); pmepoch != mepoch &&
			mepoch%dpos.config.minMEpoch() == 0 &&
			mepoch > 0 {
			t := time.Now()
			pstate, err := sys.GetState(gstate.PreEpoch)
			if err != nil {
				return nil, err
			}
			ttimestamp := header.Time.Uint64()
			oldCandidates := map[string]*CandidateInfo{}
			for theader := prevHeader; ; {
				name := theader.Coinbase.String()
				info, ok := oldCandidates[name]
				if !ok {
					info = &CandidateInfo{
						Name: name,
					}
					oldCandidates[name] = info
				}
				info.ActualCounter++

				pheader := chain.GetHeaderByHash(theader.ParentHash)
				coffset := dpos.config.getoffset(theader.Time.Uint64(), 0)
				poffset := dpos.config.getoffset(pheader.Time.Uint64(), 0)
				exit := pheader.Time.Uint64() < timestamp || (pheader.Time.Uint64()-timestamp)/dpos.config.mepochInterval() < mepoch-dpos.config.minMEpoch()
				if ftimestamp := pheader.Time.Uint64() + dpos.config.blockInterval(); ftimestamp < theader.Time.Uint64() && poffset != coffset {
					tpoffset := poffset
					toffset := dpos.config.getoffset(ftimestamp, 0)
					for {
						if toffset == coffset {
							break
						}
						if toffset != tpoffset {
							// missing
							if toffset >= uint64(len(pstate.ActivatedCandidateSchedule)) {
								continue
							}
							tname := pstate.ActivatedCandidateSchedule[toffset]
							for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
								roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
								if roffset == toffset {
									tname = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
									break
								}
							}
							info, ok := oldCandidates[tname]
							if !ok {
								info = &CandidateInfo{
									Name: tname,
								}
								oldCandidates[tname] = info
							}
							info.Counter += dpos.config.shouldCounter(ftimestamp, theader.Time.Uint64())
							tpoffset = toffset
						}
						ftimestamp += dpos.config.blockInterval()
						toffset = dpos.config.getoffset(ftimestamp, 0)
					}
					info.Counter += dpos.config.shouldCounter(ftimestamp, theader.Time.Uint64())
				}
				if systemio := strings.Compare(theader.Coinbase.String(), dpos.config.SystemName) == 0; systemio {
					info.Counter++
				} else if exit || poffset != coffset ||
					strings.Compare(pheader.Coinbase.String(), theader.Coinbase.String()) != 0 {
					info.Counter += dpos.config.shouldCounter(theader.Time.Uint64(), ttimestamp)
				}
				if exit {
					break
				}
				theader = pheader
			}

			for index, tname := range pstate.ActivatedCandidateSchedule {
				if uint64(index) >= dpos.config.CandidateScheduleSize {
					break
				}
				for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
					roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
					if roffset == uint64(index) {
						tname = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
						break
					}
				}

				tcandidate, ok := oldCandidates[tname]
				if !ok {
					continue
				}
				scnt := tcandidate.Counter
				acnt := tcandidate.ActualCounter
				if scnt < acnt {
					log.Warn("replace over", "candidate", tname, "scnt", scnt, "acnt", acnt)
					continue
				}
				log.Debug("replace check", "num", header.Number, "mepoch", mepoch, "candidate", tname, "scnt", scnt, "acnt", acnt)
				if scnt-acnt >= scnt/2 && uint64(len(pstate.OffCandidateSchedule))+dpos.config.CandidateScheduleSize < uint64(len(pstate.ActivatedCandidateSchedule)) {
					pstate.OffCandidateSchedule = append(pstate.OffCandidateSchedule, uint64(index))
					log.Info("replace index", "num", header.Number, "mepoch", mepoch, "candidate", tname, "scnt", scnt, "acnt", acnt, "rcandidate", pstate.ActivatedCandidateSchedule[uint64(len(pstate.OffCandidateSchedule)-1)+dpos.config.CandidateScheduleSize])
				}
			}

			if err := sys.SetState(pstate); err != nil {
				return nil, err
			}
			log.Info("replace check", "mepoch", mepoch, "elapsed", common.PrettyDuration(time.Now().Sub(t)))
		}
	}

	// update state root at the end
	blk.Head.Root = state.IntermediateRoot()
	if strings.Compare(header.Coinbase.String(), dpos.config.SystemName) == 0 {
		dpos.bftIrreversibles.Purge()
	}
	dpos.bftIrreversibles.Add(header.Coinbase, header.ProposedIrreversible)
	return blk, nil
}

// missing [timestamp,etimestamp)
func (dpos *Dpos) missing(sys *System, timestamp, etimestamp uint64) error {
	if timestamp >= etimestamp {
		return nil
	}
	epcho := dpos.config.epoch(timestamp)
	gstate, err := sys.GetState(epcho)
	if err != nil {
		return err
	}
	if t := dpos.config.epoch(gstate.Epoch + 1); t < etimestamp {
		etimestamp = t
	}
	pstate, err := sys.GetState(gstate.PreEpoch)
	if err != nil {
		return err
	}
	candidates := map[uint64]*CandidateInfo{}
	for ; timestamp < etimestamp; timestamp += dpos.config.blockInterval() {
		coffset := dpos.config.getoffset(timestamp, 1)
		if coffset >= uint64(len(pstate.ActivatedCandidateSchedule)) {
			continue
		}
		candidate, ok := candidates[coffset]
		if !ok {
			name := pstate.ActivatedCandidateSchedule[coffset]
			for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
				roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
				if roffset == coffset {
					name = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
					break
				}
			}
			pcandidate, err := sys.GetCandidate(gstate.Epoch, name)
			if err != nil {
				return err
			}
			candidates[coffset] = pcandidate
			candidate = pcandidate
		}
		candidate.Counter++
	}
	for _, candidate := range candidates {
		if err := sys.SetCandidate(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (dpos *Dpos) finalize1(chain consensus.IChainReader, header *types.Header, txs []*types.Transaction, receipts []*types.Receipt, state *state.StateDB) (*types.Block, error) {
	parent := chain.GetHeaderByHash(header.ParentHash)
	sys := NewSystem(state, dpos.config)

	// reward
	extraCounter := int64(0)
	extraReward := new(big.Int).Mul(dpos.config.extraBlockReward(), big.NewInt(extraCounter))
	reward := new(big.Int).Add(dpos.config.blockReward(), extraReward)
	sys.IncAsset2Acct(dpos.config.SystemName, header.Coinbase.String(), reward)

	blk := types.NewBlock(header, txs, receipts)
	// first hard fork at a specific number
	// If the block number is greater than or equal to the hard forking number,
	// the fork function will take effect. This function is valid only in the test network.
	if err := chain.ForkUpdate(blk, state); err != nil {
		return nil, err
	}

	//snapshot
	snapshotInterval := chain.Config().SnapshotInterval * uint64(time.Millisecond)
	parentTimeFormat := parent.Time.Uint64() / snapshotInterval * snapshotInterval
	currentTimeFormat := header.Time.Uint64() / snapshotInterval * snapshotInterval
	if parentTimeFormat != currentTimeFormat {
		snapshotManager := snapshot.NewSnapshotManager(state)
		if err := snapshotManager.SetSnapshot(currentTimeFormat, snapshot.BlockInfo{Number: header.Number.Uint64(), BlockHash: blk.ParentHash(), Timestamp: parentTimeFormat}); err != nil {
			return nil, err
		}
	}

	// actual counter & should counter
	// 1. prev missing
	dpos.missing(sys, parent.Time.Uint64()+dpos.config.blockInterval(), header.Time.Uint64())
	// 2. epoch update
	pepoch := dpos.config.epoch(parent.Time.Uint64())
	epoch := dpos.config.epoch(header.Time.Uint64())
	sys.UpdateElectedCandidates1(pepoch, epoch, header.Number.Uint64(), header.Coinbase.String())

	gstate, err := sys.GetState(epoch)
	if err != nil {
		return nil, err
	}
	etimestamp := dpos.config.epochTimeStamp(epoch)
	// 3. next missing
	dpos.missing(sys, etimestamp, header.Time.Uint64())
	// 4. cur
	candidate, err := sys.GetCandidate(gstate.Epoch, header.Coinbase.String())
	if err != nil {
		return nil, err
	}
	if candidate != nil {
		candidate.ActualCounter++
		candidate.Counter++
		if err := sys.SetCandidate(candidate); err != nil {
			return nil, err
		}
	}

	// replace
	pmepoch := (parent.Time.Uint64() - etimestamp) / dpos.config.mepochInterval() / dpos.config.minMEpoch()
	mepoch := (header.Time.Uint64() - etimestamp) / dpos.config.mepochInterval() / dpos.config.minMEpoch()
	if pmepoch != mepoch {
		pstate, err := sys.GetState(gstate.PreEpoch)
		if err != nil {
			return nil, err
		}
		for index, tname := range pstate.ActivatedCandidateSchedule {
			if uint64(index) >= dpos.config.CandidateScheduleSize {
				break
			}
			for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
				roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
				if roffset == uint64(index) {
					tname = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
					break
				}
			}
			candidate, err := sys.GetCandidate(gstate.Epoch, tname)
			if err != nil {
				return nil, err
			}
			tcandidate := candidate.copy()
			if strings.Compare(tcandidate.Name, header.Coinbase.String()) == 0 {
				tcandidate.Counter--
				tcandidate.ActualCounter--
			}
			for timestamp := header.Time.Uint64() - dpos.config.blockInterval(); (timestamp-etimestamp)/dpos.config.mepochInterval()/dpos.config.minMEpoch() != pmepoch; timestamp -= dpos.config.blockInterval() {
				if dpos.config.getoffset(timestamp, 1) == uint64(index) {
					tcandidate.Counter--
				}
			}
			if mepoch != 0 {
				ptcandidate, err := sys.GetActivatedCandidate(uint64(index))
				if err != nil {
					return nil, err
				}
				if ptcandidate == nil {
					continue
				}
				if ptcandidate.Name != tcandidate.Name {
					panic("not reached")
				}
				scnt := tcandidate.Counter - ptcandidate.Counter
				acnt := tcandidate.ActualCounter - ptcandidate.Counter
				log.Debug("replace check", "epoch", epoch, "mepoch", mepoch, "candidate", tname, "scnt", scnt, "acnt", acnt)
				if scnt >= acnt+scnt/2 && uint64(len(pstate.OffCandidateSchedule))+dpos.config.CandidateScheduleSize < uint64(len(pstate.ActivatedCandidateSchedule)) {
					pstate.OffCandidateSchedule = append(pstate.OffCandidateSchedule, uint64(index))
					rname := pstate.ActivatedCandidateSchedule[uint64(len(pstate.OffCandidateSchedule)-1)+dpos.config.CandidateScheduleSize]
					log.Info("replace index", "epoch", epoch, "mepoch", mepoch, "candidate", tname, "scnt", scnt, "acnt", acnt, "rcandidate", rname)
					rcandidate, err := sys.GetCandidate(gstate.Epoch, rname)
					if err != nil {
						return nil, err
					}
					tcandidate = rcandidate
				}
			}
			if err := sys.SetActivatedCandidate(uint64(index), tcandidate); err != nil {
				return nil, err
			}
			log.Debug("replace start", "epoch", epoch, "mepoch", mepoch, "index", index, "candiate", tcandidate.Name, "counter", tcandidate.Counter, "actual", tcandidate.ActualCounter)
		}
		if err := sys.SetState(pstate); err != nil {
			return nil, err
		}
	}

	// bftIrreversibles
	if strings.Compare(header.Coinbase.String(), dpos.config.SystemName) == 0 {
		dpos.bftIrreversibles.Purge()
	}
	dpos.bftIrreversibles.Add(header.Coinbase, header.ProposedIrreversible)

	// update state root at the end
	blk.Head.Root = state.IntermediateRoot()
	return blk, nil
}

// Seal generates a new block for the given input block with the local miner's seal place on top.
func (dpos *Dpos) Seal(chain consensus.IChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	header := block.Header()
	number := header.Number.Uint64()
	if number == 0 {
		return nil, errUnknownBlock
	}

	parent := chain.GetHeader(header.ParentHash, number-1)
	state, err := chain.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}
	sighash, err := dpos.signFn(signHash(header, chain.Config().ChainID.Bytes()).Bytes(), state)
	if err != nil {
		return nil, err
	}

	copy(header.Extra[len(header.Extra)-extraSeal:], sighash)
	return block.WithSeal(header), nil
}

// VerifySeal checks whether the crypto seal on a header is valid according to the consensus rules of the given engine.
func (dpos *Dpos) VerifySeal(chain consensus.IChainReader, header *types.Header) error {
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if dpos.config.nextslot(parent.Time.Uint64()) > header.Time.Uint64() {
		return errInvalidTimestamp
	}
	proudcer := header.Coinbase.String()
	state, err := chain.StateAt(parent.Root)
	if err != nil {
		return err
	}

	pubkey, err := ecrecover(header, chain.Config().ChainID.Bytes())
	if err != nil {
		return err
	}

	if err := dpos.IsValidateCandidate(chain, parent, header.Time.Uint64(), proudcer, [][]byte{pubkey}, state, true, header.CurForkID()); err != nil {
		return err
	}

	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm.
// It returns the difficulty that a new block should have when created at time given the parent block's time and difficulty.
func (dpos *Dpos) CalcDifficulty(chain consensus.IChainReader, time uint64, parent *types.Header) *big.Int {
	// return the current number as difficulty
	if timeOfGenesisBlock == 0 {
		if genesisBlock := chain.GetHeaderByNumber(0); genesisBlock != nil {
			timeOfGenesisBlock = genesisBlock.Time.Int64()
		}
	}
	return big.NewInt((int64(time)-timeOfGenesisBlock)/int64(dpos.config.blockInterval()) + 1)
}

//IsValidateCandidate current candidate
func (dpos *Dpos) IsValidateCandidate(chain consensus.IChainReader, parent *types.Header, timestamp uint64, candidate string, pubkeys [][]byte, state *state.StateDB, force bool, fid uint64) error {
	if timestamp%dpos.BlockInterval() != 0 {
		return errInvalidMintBlockTime
	}

	db := &stateDB{
		name:  dpos.config.AccountName,
		state: state,
	}
	has := false
	for _, pubkey := range pubkeys {
		if db.IsValidSign(candidate, pubkey) {
			has = true
		}
	}
	if !has {
		return ErrIllegalCandidatePubKey
	}

	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(LastEpoch)
	if err != nil {
		return err
	}

	systemio := strings.Compare(candidate, dpos.config.SystemName) == 0
	if gstate.TakeOver {
		if force && systemio {
			return nil
		}
		return ErrSystemTakeOver
	} else if parent.Number.Uint64() > 0 && (dpos.CalcProposedIrreversible(chain, parent, true) == 0 || timestamp-parent.Time.Uint64() > 2*dpos.config.mepochInterval()) {
		if force && systemio {
			// first take over
			return nil
		}
		if parent.Number.Uint64() >= dpos.config.CandidateScheduleSize*dpos.config.BlockFrequency {
			return ErrTooMuchRreversible
		}
	}

	pstate, err := sys.GetState(gstate.PreEpoch)
	if err != nil {
		return err
	}
	tname := ""
	offset := dpos.config.getoffset(timestamp, fid)
	if offset < uint64(len(pstate.ActivatedCandidateSchedule)) {
		tname = pstate.ActivatedCandidateSchedule[offset]
		for rindex := len(pstate.OffCandidateSchedule); rindex > 0; rindex-- {
			roffset := pstate.OffCandidateSchedule[uint64(rindex-1)]
			if roffset == uint64(offset) {
				tname = pstate.ActivatedCandidateSchedule[dpos.config.CandidateScheduleSize+uint64(rindex-1)]
				break
			}
		}

		if fid > 1 {
			epoch := dpos.config.epoch(timestamp)
			etimestamp := dpos.config.epochTimeStamp(epoch)
			pmepoch := (parent.Time.Uint64() - etimestamp) / dpos.config.mepochInterval() / dpos.config.minMEpoch()
			mepoch := (timestamp - etimestamp) / dpos.config.mepochInterval() / dpos.config.minMEpoch()
			if pmepoch != mepoch && mepoch > 0 {
				candidate, err := sys.GetCandidate(gstate.Epoch, tname)
				if err != nil {
					return err
				}
				for timestamp := timestamp - dpos.config.blockInterval(); (timestamp-etimestamp)/dpos.config.mepochInterval()/dpos.config.minMEpoch() != pmepoch; timestamp -= dpos.config.blockInterval() {
					if dpos.config.getoffset(timestamp, 1) == offset {
						candidate.Counter--
					}
				}
				tcandidate, err := sys.GetActivatedCandidate(offset)
				if err != nil {
					return err
				}
				if tcandidate != nil {
					if strings.Compare(tcandidate.Name, tname) != 0 {
						panic("not reached")
					}
					scnt := tcandidate.Counter - candidate.Counter
					acnt := tcandidate.ActualCounter - candidate.Counter
					if scnt >= acnt+scnt/2 && uint64(len(pstate.OffCandidateSchedule))+dpos.config.CandidateScheduleSize < uint64(len(pstate.ActivatedCandidateSchedule)) {
						rname := pstate.ActivatedCandidateSchedule[uint64(len(pstate.OffCandidateSchedule))+dpos.config.CandidateScheduleSize]
						log.Info("replace index IsValidateCandidate", "epoch", epoch, "mepoch", mepoch, "candidate", tname, "scnt", scnt, "acnt", acnt, "rcandidate", rname)
						tname = rname
					}
				}
			}
		}
	}

	if strings.Compare(tname, candidate) != 0 {
		return fmt.Errorf("%v %v, except %v index %v (%v) ", errInvalidBlockCandidate, candidate, pstate.ActivatedCandidateSchedule, offset, pstate.Epoch)
	}
	return nil
}

// BlockInterval block interval
func (dpos *Dpos) BlockInterval() uint64 {
	return dpos.config.blockInterval()
}

// Slot slot
func (dpos *Dpos) Slot(timestamp uint64) uint64 {
	return dpos.config.slot(timestamp)
}

// GetDelegatedByTime get delegate of candidate
func (dpos *Dpos) GetDelegatedByTime(state *state.StateDB, candidate string, timestamp uint64) (*big.Int, error) {
	sys := NewSystem(state, dpos.config)
	candidateInfo, err := sys.GetCandidateInfoByTime(sys.config.epoch(timestamp), candidate, timestamp)
	if err != nil || candidateInfo == nil {
		return big.NewInt(0), err
	}
	return new(big.Int).Mul(candidateInfo.Quantity, sys.config.unitStake()), nil
}

// GetLatestEpoch get latest epoch
func (dpos *Dpos) GetLatestEpoch(state *state.StateDB) (epoch uint64, err error) {
	sys := NewSystem(state, dpos.config)
	return sys.GetLastestEpoch()
}

// GetPrevEpoch get pre epoch
func (dpos *Dpos) GetPrevEpoch(state *state.StateDB, epoch uint64) (uint64, error) {
	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(epoch)
	if err != nil {
		return 0, err
	}
	if gstate == nil {
		return 0, fmt.Errorf("not found")
	}
	return gstate.PreEpoch, nil
}

// GetNextEpoch get next epoch
func (dpos *Dpos) GetNextEpoch(state *state.StateDB, epoch uint64) (uint64, error) {
	sys := NewSystem(state, dpos.config)
	latest, err := sys.GetLastestEpoch()
	if err != nil {
		return 0, err
	}
	for {
		epoch++
		if epoch > latest {
			return 0, nil
		}
		gstate, err := sys.GetState(epoch)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return 0, err
		}
		if gstate != nil {
			return gstate.Epoch, nil
		}
	}
}

// GetActivedCandidateSize get actived candidate size
func (dpos *Dpos) GetActivedCandidateSize(state *state.StateDB, epoch uint64) (uint64, error) {
	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(epoch)
	if err != nil {
		return 0, err
	}
	pstate, err := sys.GetState(gstate.PreEpoch)
	if err != nil {
		return 0, err
	}
	return uint64(len(pstate.ActivatedCandidateSchedule)), nil
}

// GetActivedCandidate get actived candidate info
func (dpos *Dpos) GetActivedCandidate(state *state.StateDB, epoch uint64, index uint64) (string, *big.Int, *big.Int, uint64, uint64, uint64, error) {
	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(epoch)
	if err != nil {
		return "", big.NewInt(0), big.NewInt(0), 0, 0, 0, err
	}
	pstate, err := sys.GetState(gstate.PreEpoch)
	if err != nil {
		return "", big.NewInt(0), big.NewInt(0), 0, 0, 0, err
	}
	if index >= uint64(len(pstate.ActivatedCandidateSchedule)) {
		return "", big.NewInt(0), big.NewInt(0), 0, 0, 0, fmt.Errorf("out of index")
	}
	candidate := pstate.ActivatedCandidateSchedule[index]

	prevCandidateInfo, err := sys.GetCandidate(gstate.PreEpoch, candidate)
	if err != nil {
		return "", big.NewInt(0), big.NewInt(0), 0, 0, 0, err
	}

	candidateInfo, err := sys.GetCandidate(gstate.Epoch, candidate)
	if err != nil {
		return "", big.NewInt(0), big.NewInt(0), 0, 0, 0, err
	}

	if prevCandidateInfo == nil {
		prevCandidateInfo = &CandidateInfo{}
	}
	if candidateInfo == nil {
		candidateInfo = &CandidateInfo{}
	}

	counter := candidateInfo.Counter
	actualCounter := candidateInfo.ActualCounter
	if prevCandidateInfo != nil {
		counter -= prevCandidateInfo.Counter
		actualCounter -= prevCandidateInfo.ActualCounter
	}
	if counter < actualCounter {
		counter = actualCounter
	}
	rindex := uint64(0)
	if s := uint64(len(pstate.OffCandidateSchedule)); index >= dpos.config.CandidateScheduleSize && index-dpos.config.CandidateScheduleSize < s {
		rindex = pstate.OffCandidateSchedule[index-dpos.config.CandidateScheduleSize] + 1
	}

	return candidate, new(big.Int).Mul(prevCandidateInfo.Quantity, sys.config.unitStake()), new(big.Int).Mul(prevCandidateInfo.TotalQuantity, sys.config.unitStake()), counter, actualCounter, rindex, err
}

// GetCandidateStake candidate delegate stake
func (dpos *Dpos) GetCandidateStake(state *state.StateDB, epoch uint64, candidate string) (*big.Int, error) {
	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(epoch)
	if err != nil {
		return big.NewInt(0), err
	}
	candidateInfo, err := sys.GetCandidate(gstate.PreEpoch, candidate)
	if err != nil {
		return big.NewInt(0), err
	}
	return new(big.Int).Mul(candidateInfo.Quantity, sys.config.unitStake()), nil
}

// GetVoterStake voter stake
func (dpos *Dpos) GetVoterStake(state *state.StateDB, epoch uint64, voter string, candidate string) (*big.Int, error) {
	sys := NewSystem(state, dpos.config)
	gstate, err := sys.GetState(epoch)
	if err != nil {
		return big.NewInt(0), err
	}
	voterInfo, err := sys.GetVoter(gstate.PreEpoch, voter, candidate)
	if err != nil {
		return big.NewInt(0), err
	}
	if voterInfo == nil {
		return big.NewInt(0), nil
	}
	return new(big.Int).Mul(voterInfo.Quantity, sys.config.unitStake()), nil
}

// Engine an engine
func (dpos *Dpos) Engine() consensus.IEngine {
	return dpos
}

// CalcBFTIrreversible calc irreversible
func (dpos *Dpos) CalcBFTIrreversible() uint64 {
	irreversibles := UInt64Slice{}
	keys := dpos.bftIrreversibles.Keys()
	for _, key := range keys {
		if irreversible, ok := dpos.bftIrreversibles.Get(key); ok {
			irreversibles = append(irreversibles, irreversible.(uint64))
		}
	}

	if len(irreversibles) == 0 {
		return 0
	}

	sort.Sort(irreversibles)

	/// 2/3 must be greater, so if I go 1/3 into the list sorted from low to high, then 2/3 are greater
	return irreversibles[(len(irreversibles)-1)/3]
}

// CalcProposedIrreversible calc irreversible
func (dpos *Dpos) CalcProposedIrreversible(chain consensus.IChainReader, parent *types.Header, strict bool) uint64 {
	curHeader := chain.CurrentHeader()
	if parent != nil {
		curHeader = chain.GetHeaderByHash(parent.Hash())
	}
	candidateMap := make(map[string]uint64)
	timestamp := curHeader.Time.Uint64()
	for curHeader.Number.Uint64() > 0 {
		if strings.Compare(curHeader.Coinbase.String(), dpos.config.SystemName) == 0 {
			return curHeader.Number.Uint64()
		}
		if strict && timestamp-curHeader.Time.Uint64() >= 2*dpos.config.mepochInterval() {
			break
		}
		candidateMap[curHeader.Coinbase.String()]++
		if uint64(len(candidateMap)) >= dpos.config.consensusSize() {
			return curHeader.Number.Uint64()
		}
		curHeader = chain.GetHeaderByHash(curHeader.ParentHash)
	}
	return 0
}

func ecrecover(header *types.Header, extra []byte) ([]byte, error) {
	// If the signature's already cached, return that
	if len(header.Extra) < extraSeal {
		return nil, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]
	pubkey, err := crypto.Ecrecover(signHash(header, extra).Bytes(), signature)
	if err != nil {
		return nil, err
	}
	return pubkey, nil
}

func signHash(header *types.Header, extra []byte) (hash common.Hash) {
	theader := types.CopyHeader(header)
	theader.Extra = theader.Extra[:len(theader.Extra)-extraSeal]
	return theader.Hash()
}

// UInt64Slice attaches the methods of sort.Interface to []uint64, sorting in increasing order.
type UInt64Slice []uint64

func (s UInt64Slice) Len() int           { return len(s) }
func (s UInt64Slice) Less(i, j int) bool { return s[i] < s[j] }
func (s UInt64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
