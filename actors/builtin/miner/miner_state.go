package miner

import (
	"fmt"
	"io"
	"reflect"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	cid "github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-core/peer"
	errors "github.com/pkg/errors"
	cbg "github.com/whyrusleeping/cbor-gen"
	xerrors "golang.org/x/xerrors"

	abi "github.com/filecoin-project/specs-actors/actors/abi"
	big "github.com/filecoin-project/specs-actors/actors/abi/big"
	power "github.com/filecoin-project/specs-actors/actors/builtin/power"
	. "github.com/filecoin-project/specs-actors/actors/util"
	adt "github.com/filecoin-project/specs-actors/actors/util/adt"
)

// Balance of Miner Actor should be greater than or equal to
// the sum of PreCommitDeposits and LockedFunds.
// Excess balance as computed by st.GetAvailableBalance will be
// withdrawable or usable for pre-commit deposit or pledge lock-up.
type State struct {
	// Information not related to sectors.
	// TODO: this should be a cid of the miner Info struct so it's not re-written when other fields change.
	Info MinerInfo

	PreCommitDeposits abi.TokenAmount // Total funds locked as PreCommitDeposits
	LockedFunds       abi.TokenAmount // Total unvested funds locked as pledge collateral
	VestingFunds      cid.Cid         // Array, AMT[ChainEpoch]TokenAmount

	// Sectors that have been pre-committed but not yet proven.
	PreCommittedSectors cid.Cid // Map, HAMT[SectorNumber]SectorPreCommitOnChainInfo

	// Information for all proven and not-yet-expired sectors.
	Sectors cid.Cid // Array, AMT[SectorNumber]SectorOnChainInfo (sparse)

	// Sector numbers prove-committed since period start, to be added to Deadlines at next proving period boundary.
	NewSectors *abi.BitField

	// Sector numbers indexed by expiry epoch (which are on proving period boundaries).
	// Invariant: Keys(Sectors) == union(SectorExpirations.Values())
	SectorExpirations cid.Cid // Array, AMT[ChainEpoch]Bitfield

	// The sector numbers due for PoSt at each deadline in the current proving period, frozen at period start.
	// New sectors are added and expired ones removed at proving period boundary.
	// Faults are not subtracted from this in state, but on the fly.
	Deadlines cid.Cid

	// All currently known faulty sectors, mutated eagerly.
	// These sectors are exempt from inclusion in PoSt.
	Faults *abi.BitField

	// Faulty sector numbers indexed by the start epoch of the proving period in which detected.
	// Used to track fault durations for eventual sector termination.
	// At most 14 entries, b/c sectors faulty longer expire.
	// Invariant: Faults == union(FaultEpochs.Values())
	FaultEpochs cid.Cid // AMT[ChainEpoch]Bitfield

	// Faulty sectors that will recover when next included in a valid PoSt.
	// Invariant: Recoveries ⊆ Faults.
	Recoveries *abi.BitField

	// Records successful PoSt submission in the current proving period by partition number.
	// The presence of a partition number indicates on-time PoSt received.
	PostSubmissions *abi.BitField
}

type MinerInfo struct {
	// Account that owns this miner.
	// - Income and returned collateral are paid to this address.
	// - This address is also allowed to change the worker address for the miner.
	Owner addr.Address // Must be an ID-address.

	// Worker account for this miner.
	// The associated pubkey-type address is used to sign blocks and messages on behalf of this miner.
	Worker addr.Address // Must be an ID-address.

	PendingWorkerKey *WorkerKeyChange

	// Libp2p identity that should be used when connecting to this miner.
	PeerId peer.ID

	// Amount of space in each sector committed to the network by this miner.
	SectorSize abi.SectorSize

	// The offset of this miner's proving period from zero.
	// An un-changing number in range [0, proving period).
	// A miner's current proving period start is the highest multiple of this boundary <= the current epoch.
	ProvingPeriodBoundary abi.ChainEpoch
}

type PeerID peer.ID

type WorkerKeyChange struct {
	NewWorker   addr.Address // Must be an ID address
	EffectiveAt abi.ChainEpoch
}

type SectorPreCommitInfo struct {
	RegisteredProof abi.RegisteredProof
	SectorNumber    abi.SectorNumber
	SealedCID       cid.Cid // CommR
	SealRandEpoch   abi.ChainEpoch
	DealIDs         []abi.DealID
	Expiration      abi.ChainEpoch // Sector Expiration
}

type SectorPreCommitOnChainInfo struct {
	Info             SectorPreCommitInfo
	PreCommitDeposit abi.TokenAmount
	PreCommitEpoch   abi.ChainEpoch
}

type SectorOnChainInfo struct {
	Info               SectorPreCommitInfo
	ActivationEpoch    abi.ChainEpoch // Epoch at which SectorProveCommit is accepted
	DealWeight         abi.DealWeight // Integral of active deals over sector lifetime
	VerifiedDealWeight abi.DealWeight // Integral of active verified deals over sector lifetime
}

func ConstructState(emptyArrayCid, emptyMapCid, emptyDeadlinesCid cid.Cid, ownerAddr, workerAddr addr.Address,
	peerId peer.ID, sectorSize abi.SectorSize, periodBoundary abi.ChainEpoch) *State {
	return &State{
		Info: MinerInfo{
			Owner:                 ownerAddr,
			Worker:                workerAddr,
			PendingWorkerKey:      nil,
			PeerId:                peerId,
			SectorSize:            sectorSize,
			ProvingPeriodBoundary: periodBoundary,
		},

		PreCommitDeposits: abi.NewTokenAmount(0),
		LockedFunds:       abi.NewTokenAmount(0),
		VestingFunds:      emptyArrayCid,

		PreCommittedSectors: emptyMapCid,
		Sectors:             emptyArrayCid,
		NewSectors:          abi.NewBitField(),
		SectorExpirations:   emptyArrayCid,
		Deadlines:           emptyDeadlinesCid,
		Faults:              abi.NewBitField(),
		FaultEpochs:         emptyArrayCid,
		Recoveries:          abi.NewBitField(),
		PostSubmissions:     abi.NewBitField(),
	}
}

func (st *State) GetWorker() addr.Address {
	return st.Info.Worker
}

func (st *State) GetSectorSize() abi.SectorSize {
	return st.Info.SectorSize
}

// Computes the current proving period and deadline, and whether that period is whole.
func (st *State) DeadlineInfo(currEpoch abi.ChainEpoch) (*DeadlineInfo, bool) {
	return ComputeProvingPeriodDeadline(st.Info.ProvingPeriodBoundary, currEpoch)
}

func (st *State) GetSectorCount(store adt.Store) (uint64, error) {
	arr, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return 0, err
	}

	return arr.Length(), nil
}

func (st *State) GetMaxAllowedFaults(store adt.Store) (uint64, error) {
	sectorCount, err := st.GetSectorCount(store)
	if err != nil {
		return 0, err
	}
	return 2 * sectorCount, nil
}

func (st *State) PutPrecommittedSector(store adt.Store, info *SectorPreCommitOnChainInfo) error {
	precommitted, err := adt.AsMap(store, st.PreCommittedSectors)
	if err != nil {
		return err
	}

	err = precommitted.Put(SectorKey(info.Info.SectorNumber), info)
	if err != nil {
		return errors.Wrapf(err, "failed to store precommitment for %v", info)
	}
	st.PreCommittedSectors, err = precommitted.Root()
	return err
}

func (st *State) GetPrecommittedSector(store adt.Store, sectorNo abi.SectorNumber) (*SectorPreCommitOnChainInfo, bool, error) {
	precommitted, err := adt.AsMap(store, st.PreCommittedSectors)
	if err != nil {
		return nil, false, err
	}

	var info SectorPreCommitOnChainInfo
	found, err := precommitted.Get(SectorKey(sectorNo), &info)
	if err != nil {
		return nil, false, errors.Wrapf(err, "failed to load precommitment for %v", sectorNo)
	}
	return &info, found, nil
}

func (st *State) DeletePrecommittedSector(store adt.Store, sectorNo abi.SectorNumber) error {
	precommitted, err := adt.AsMap(store, st.PreCommittedSectors)
	if err != nil {
		return err
	}

	err = precommitted.Delete(SectorKey(sectorNo))
	if err != nil {
		return errors.Wrapf(err, "failed to delete precommitment for %v", sectorNo)
	}
	st.PreCommittedSectors, err = precommitted.Root()
	return err
}

func (st *State) HasSectorNo(store adt.Store, sectorNo abi.SectorNumber) (bool, error) {
	sectors, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return false, err
	}

	var info SectorOnChainInfo
	found, err := sectors.Get(uint64(sectorNo), &info)
	if err != nil {
		return false, xerrors.Errorf("failed to get sector %v: %w", sectorNo, err)
	}
	return found, nil
}

func (st *State) PutSector(store adt.Store, sector *SectorOnChainInfo) error {
	sectors, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return err
	}

	if err := sectors.Set(uint64(sector.Info.SectorNumber), sector); err != nil {
		return errors.Wrapf(err, "failed to put sector %v", sector)
	}
	st.Sectors, err = sectors.Root()
	return err
}

func (st *State) GetSector(store adt.Store, sectorNo abi.SectorNumber) (*SectorOnChainInfo, bool, error) {
	sectors, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return nil, false, err
	}

	var info SectorOnChainInfo
	found, err := sectors.Get(uint64(sectorNo), &info)
	if err != nil {
		return nil, false, errors.Wrapf(err, "failed to get sector %v", sectorNo)
	}
	return &info, found, nil
}

func (st *State) DeleteSectors(store adt.Store, sectorNos *abi.BitField) error {
	sectors, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return err
	}
	err = sectorNos.ForEach(func(sectorNo uint64) error {
		if err = sectors.Delete(sectorNo); err != nil {
			return errors.Wrapf(err, "failed to delete sector %v", sectorNos)
		}
		return nil
	})
	if err != nil {
		return err
	}

	st.Sectors, err = sectors.Root()
	return err
}

func (st *State) ForEachSector(store adt.Store, f func(*SectorOnChainInfo)) error {
	sectors, err := adt.AsArray(store, st.Sectors)
	if err != nil {
		return err
	}
	var sector SectorOnChainInfo
	return sectors.ForEach(&sector, func(idx int64) error {
		f(&sector)
		return nil
	})
}

// Adds some sector numbers to the new sectors bitfield.
func (st *State) AddNewSectors(sectorNos ...abi.SectorNumber) (err error) {
	ns := abi.NewBitField()
	for _, sector := range sectorNos {
		ns.Set(uint64(sector))
	}
	st.NewSectors, err = bitfield.MergeBitFields(st.NewSectors, ns)
	if err != nil {
		return err
	}

	count, err := st.NewSectors.Count()
	if err != nil {
		return err
	}
	if count > NewSectorsPerPeriodMax {
		return fmt.Errorf("too many new sectors %d, max %d", count, NewSectorsPerPeriodMax)
	}
	return nil
}

// Removes some sector numbers from the new sectors bitfield, if present.
func (st *State) RemoveNewSectors(sectorNos *abi.BitField) (err error) {
	st.NewSectors, err = bitfield.SubtractBitField(st.NewSectors, sectorNos)
	return err
}

// Clears the new sectors bitfield.
func (st *State) ClearNewSectors() error {
	st.NewSectors = abi.NewBitField()
	return nil
}

// Gets the sector numbers expiring at some epoch.
func (st *State) GetSectorExpirations(store adt.Store, expiry abi.ChainEpoch) (*abi.BitField, error) {
	arr, err := adt.AsArray(store, st.SectorExpirations)
	if err != nil {
		return nil, err
	}

	bf := abi.NewBitField()
	_, err = arr.Get(uint64(expiry), bf)
	if err != nil {
		return nil, err
	}

	return bf, nil
}

// Iterates sector expiration groups in order.
// Note that the sectors bitfield provided to the callback is not safe to store.
func (st *State) ForEachSectorExpiration(store adt.Store, f func(expiry abi.ChainEpoch, sectors *abi.BitField) error) error {
	arr, err := adt.AsArray(store, st.SectorExpirations)
	if err != nil {
		return err
	}

	bf := abi.NewBitField()
	return arr.ForEach(bf, func(i int64) error {
		return f(abi.ChainEpoch(i), bf)
	})
}

// Adds some sector numbers to the set expiring at an epoch.
// The sector numbers are given as uint64s to avoid pointless conversions.
func (st *State) AddSectorExpirations(store adt.Store, expiry abi.ChainEpoch, sectors ...uint64) error {
	arr, err := adt.AsArray(store, st.SectorExpirations)
	if err != nil {
		return err
	}

	bf := abi.NewBitField()
	_, err = arr.Get(uint64(expiry), bf)
	if err != nil {
		return err
	}

	bf, err = bitfield.MergeBitFields(bf, bitfield.NewFromSet(sectors))
	if err != nil {
		return err
	}
	count, err := bf.Count()
	if err != nil {
		return err
	}
	if count > SectorsMax {
		return fmt.Errorf("too many sectors at expiration %d, %d, max %d", expiry, count, SectorsMax)
	}

	if err = arr.Set(uint64(expiry), bf); err != nil {
		return err
	}

	st.SectorExpirations, err = arr.Root()
	return err
}

// Removes some sector numbers from the set expiring at an epoch.
func (st *State) RemoveSectorExpirations(store adt.Store, expiry abi.ChainEpoch, sectors ...uint64) error {
	arr, err := adt.AsArray(store, st.SectorExpirations)
	if err != nil {
		return err
	}

	bf := abi.NewBitField()
	_, err = arr.Get(uint64(expiry), bf)
	if err != nil {
		return err
	}

	bf, err = bitfield.SubtractBitField(bf, bitfield.NewFromSet(sectors))
	if err != nil {
		return err
	}

	if err = arr.Set(uint64(expiry), bf); err != nil {
		return err
	}

	st.SectorExpirations, err = arr.Root()
	return err
}

// Removes all sector numbers from the set expiring some epochs.
func (st *State) ClearSectorExpirations(store adt.Store, expirations ...abi.ChainEpoch) error {
	arr, err := adt.AsArray(store, st.SectorExpirations)
	if err != nil {
		return err
	}

	for _, exp := range expirations {
		err = arr.Delete(uint64(exp))
		if err != nil {
			return err
		}
	}

	st.SectorExpirations, err = arr.Root()
	return err
}

// Adds sectors numbers to faults and fault epochs.
func (st *State) AddFaults(store adt.Store, sectorNos *abi.BitField, faultEpoch abi.ChainEpoch) (err error) {
	empty, err := sectorNos.IsEmpty()
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	{
		st.Faults, err = bitfield.MergeBitFields(st.Faults, sectorNos)
		if err != nil {
			return err
		}

		count, err := st.Faults.Count()
		if err != nil {
			return err
		}
		if count > SectorsMax {
			return fmt.Errorf("too many faults %d, max %d", count, SectorsMax)
		}
	}

	{
		epochFaultArr, err := adt.AsArray(store, st.FaultEpochs)
		if err != nil {
			return err
		}

		bf := abi.NewBitField()
		_, err = epochFaultArr.Get(uint64(faultEpoch), bf)
		if err != nil {
			return err
		}

		bf, err = bitfield.MergeBitFields(bf, sectorNos)
		if err != nil {
			return err
		}

		if err = epochFaultArr.Set(uint64(faultEpoch), bf); err != nil {
			return err
		}

		st.FaultEpochs, err = epochFaultArr.Root()
		if err != nil {
			return err
		}
	}

	return nil
}

// Removes sector numbers from faults and fault epochs, if present.
func (st *State) RemoveFaults(store adt.Store, sectorNos *abi.BitField) (err error) {
	empty, err := sectorNos.IsEmpty()
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	st.Faults, err = bitfield.SubtractBitField(st.Faults, sectorNos)
	if err != nil {
		return err
	}

	arr, err := adt.AsArray(store, st.FaultEpochs)
	if err != nil {
		return err
	}

	changed := map[uint64]*abi.BitField{}

	bf1 := &abi.BitField{}
	err = arr.ForEach(bf1, func(i int64) error {
		c1, err := bf1.Count()
		if err != nil {
			return err
		}

		bf2, err := bitfield.SubtractBitField(bf1, sectorNos)
		if err != nil {
			return err
		}

		c2, err := bf2.Count()
		if err != nil {
			return err
		}

		if c1 != c2 {
			changed[uint64(i)] = bf2
		}

		return nil
	})
	if err != nil {
		return err
	}

	for i, field := range changed {
		if err = arr.Set(i, field); err != nil {
			return err
		}
	}

	st.FaultEpochs, err = arr.Root()
	return err
}

// Iterates faults by declaration epoch, in order.
func (st *State) ForEachFaultEpoch(store adt.Store, cb func(epoch abi.ChainEpoch, faults *abi.BitField) error) error {
	arr, err := adt.AsArray(store, st.FaultEpochs)
	if err != nil {
		return err
	}

	bf := abi.NewBitField()
	return arr.ForEach(bf, func(i int64) error {
		return cb(abi.ChainEpoch(i), bf)
	})
}

func (st *State) ClearFaultEpochs(store adt.Store, epochs ...abi.ChainEpoch) error {
	arr, err := adt.AsArray(store, st.FaultEpochs)
	if err != nil {
		return err
	}

	for _, exp := range epochs {
		err = arr.Delete(uint64(exp))
		if err != nil {
			return nil
		}
	}

	st.FaultEpochs, err = arr.Root()
	return nil
}

// Adds sectors to recoveries.
func (st *State) AddRecoveries(sectorNos *abi.BitField) (err error) {
	empty, err := sectorNos.IsEmpty()
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	st.Recoveries, err = bitfield.MergeBitFields(st.Recoveries, sectorNos)
	if err != nil {
		return err
	}

	count, err := st.Recoveries.Count()
	if err != nil {
		return err
	}
	if count > SectorsMax {
		return fmt.Errorf("too many recoveries %d, max %d", count, SectorsMax)
	}
	return nil
}

// Removes sectors from recoveries, if present.
func (st *State) RemoveRecoveries(sectorNos *abi.BitField) (err error) {
	empty, err := sectorNos.IsEmpty()
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	st.Recoveries, err = bitfield.SubtractBitField(st.Recoveries, sectorNos)
	return err
}

// Loads sector info for a sequence of sectors.
func (st *State) LoadSectorInfos(store adt.Store, sectors *abi.BitField) ([]*SectorOnChainInfo, error) {
	var sectorInfos []*SectorOnChainInfo
	err := sectors.ForEach(func(i uint64) error {
		sectorOnChain, found, err := st.GetSector(store, abi.SectorNumber(i))
		if err != nil {
			return fmt.Errorf("failed to load sector %d: %v", i, err)
		} else if !found {
			return fmt.Errorf("can't find sector %d", i)
		}
		sectorInfos = append(sectorInfos, sectorOnChain)
		return nil
	})
	return sectorInfos, err
}

// Loads sector info for a sequence of sectors, substituting info for a stand-in sector for any that are faulty.
func (st *State) LoadSectorInfosWithFaultMask(store adt.Store, sectors *abi.BitField, faults *abi.BitField, faultStandIn abi.SectorNumber) ([]*SectorOnChainInfo, error) {
	sectorOnChain, found, err := st.GetSector(store, faultStandIn)
	if err != nil {
		return nil, fmt.Errorf("failed to load stand-in sector %d: %v", faultStandIn, err)
	} else if !found {
		return nil, fmt.Errorf("can't find stand-in sector %d", faultStandIn)
	}
	standInInfo := sectorOnChain

	// Expand faults into a map for quick lookups.
	// The faults bitfield should already be a subset of the sectors bitfield.
	faultMax, err := sectors.Count()
	if err != nil {
		return nil, err
	}
	faultSet, err := faults.AllMap(faultMax)
	if err != nil {
		return nil, fmt.Errorf("failed to expand faults: %w", err)
	}

	// Load the sector infos, masking out fault sectors with a good one.
	var sectorInfos []*SectorOnChainInfo
	err = sectors.ForEach(func(i uint64) error {
		sector := standInInfo
		faulty := faultSet[i]
		if !faulty {
			sectorOnChain, found, err = st.GetSector(store, abi.SectorNumber(i))
			if err != nil {
				return fmt.Errorf("failed to load sector %d: %v", i, err)
			} else if !found {
				return fmt.Errorf("can't find sector %d", i)
			}
			sector = sectorOnChain
		}
		sectorInfos = append(sectorInfos, sector)
		return nil
	})
	return sectorInfos, err
}

// Adds partition numbers to the set of PoSt submissions
func (st *State) AddPoStSubmissions(partitionNos *abi.BitField) (err error) {
	st.PostSubmissions, err = bitfield.MergeBitFields(st.PostSubmissions, partitionNos)
	return err
}

// Removes all PoSt submissions
func (st *State) ClearPoStSubmissions() error {
	st.PostSubmissions = abi.NewBitField()
	return nil
}

func (st *State) LoadDeadlines(store adt.Store) (*Deadlines, error) {
	var deadlines Deadlines
	if err := store.Get(store.Context(), st.Deadlines, &deadlines); err != nil {
		return nil, fmt.Errorf("failed to load deadlines (%s): %w", st.Deadlines, err)
	}

	return &deadlines, nil
}

func (st *State) SaveDeadlines(store adt.Store, deadlines *Deadlines) error {
	c, err := store.Put(store.Context(), deadlines)
	if err != nil {
		return err
	}
	st.Deadlines = c
	return nil
}

//
// PoSt Deadlines and partitions
//

type Deadlines struct {
	// A bitfield of sector numbers due at each deadline.
	// The sectors for each deadline are logically grouped into sequential partitions for proving.
	Due [WPoStPeriodDeadlines]*abi.BitField
}

func ConstructDeadlines() *Deadlines {
	d := &Deadlines{Due: [WPoStPeriodDeadlines]*abi.BitField{}}
	for i := range d.Due {
		d.Due[i] = abi.NewBitField()
	}
	return d
}

// Adds sector numbers to a deadline.
// The sector numbers are given as uint64 to avoid pointless conversions for bitfield use.
func (d *Deadlines) AddToDeadline(deadline uint64, newSectors ...uint64) (err error) {
	ns := bitfield.NewFromSet(newSectors)
	d.Due[deadline], err = bitfield.MergeBitFields(d.Due[deadline], ns)
	return err
}

// Removes sector numbers from all deadlines.
func (d *Deadlines) RemoveFromAllDeadlines(sectorNos *abi.BitField) (err error) {
	for i := range d.Due {
		d.Due[i], err = bitfield.SubtractBitField(d.Due[i], sectorNos)
		if err != nil {
			return err
		}
	}
	return nil
}

//
// Funds and vesting
//

func (st *State) AddPreCommitDeposit(amount abi.TokenAmount) {
	newTotal := big.Add(st.PreCommitDeposits, amount)
	AssertMsg(newTotal.GreaterThanEqual(big.Zero()), "negative pre-commit deposit %s after adding %s to prior %s",
		newTotal, amount, st.PreCommitDeposits)
	st.PreCommitDeposits = newTotal
}

func (st *State) AddLockedFunds(store adt.Store, currEpoch abi.ChainEpoch, vestingSum abi.TokenAmount, spec *VestSpec) error {
	AssertMsg(vestingSum.GreaterThanEqual(big.Zero()), "negative vesting sum %s", vestingSum)
	vestingFunds, err := adt.AsArray(store, st.VestingFunds)
	if err != nil {
		return err
	}

	// Nothing unlocks here, this is just the start of the clock.
	vestBegin := currEpoch + spec.InitialDelay
	vestPeriod := big.NewInt(int64(spec.VestPeriod))

	vestedSoFar := big.Zero()
	for e := vestBegin + spec.StepDuration; vestedSoFar.LessThan(vestingSum); e += spec.StepDuration {
		vestEpoch := quantizeUp(e, spec.Quantization)
		elapsed := vestEpoch - vestBegin

		targetVest := big.Zero()
		if elapsed < spec.VestPeriod {
			// Linear vesting, PARAM_FINISH
			targetVest = big.Div(big.Mul(vestingSum, big.NewInt(int64(elapsed))), vestPeriod)
		} else {
			targetVest = vestingSum
		}

		vestThisTime := big.Sub(targetVest, vestedSoFar)
		vestedSoFar = targetVest

		// Load existing entry, else set a new one
		key := EpochKey(vestEpoch)
		lockedFundEntry := big.Zero()
		_, err = vestingFunds.Get(key, &lockedFundEntry)
		if err != nil {
			return err
		}

		lockedFundEntry = big.Add(lockedFundEntry, vestThisTime)
		err = vestingFunds.Set(key, &lockedFundEntry)
		if err != nil {
			return err
		}
	}

	st.VestingFunds, err = vestingFunds.Root()
	if err != nil {
		return err
	}
	st.LockedFunds = big.Add(st.LockedFunds, vestingSum)

	return nil
}

// Unlocks an amount of funds that have *not yet vested*, if possible.
// The soonest-vesting entries are unlocked first.
// Returns the amount actually unlocked.
func (st *State) UnlockUnvestedFunds(store adt.Store, currEpoch abi.ChainEpoch, target abi.TokenAmount) (abi.TokenAmount, error) {
	vestingFunds, err := adt.AsArray(store, st.VestingFunds)
	if err != nil {
		return abi.TokenAmount{}, err
	}

	amountUnlocked := big.Zero()

	var lockedEntry abi.TokenAmount
	var toDelete []uint64
	var finished = fmt.Errorf("finished")

	// Iterate vestingFunds are in order of release.
	err = vestingFunds.ForEach(&lockedEntry, func(k int64) error {
		if amountUnlocked.LessThan(target) {
			if k >= int64(currEpoch) {
				unlockAmount := big.Min(big.Sub(target, amountUnlocked), lockedEntry)
				amountUnlocked = big.Add(amountUnlocked, unlockAmount)
				lockedEntry = big.Sub(lockedEntry, unlockAmount)

				if lockedEntry.IsZero() {
					toDelete = append(toDelete, uint64(k))
				} else {
					if err = vestingFunds.Set(uint64(k), &lockedEntry); err != nil {
						return err
					}
				}
			}
			return nil
		} else {
			return finished
		}
	})

	if err != nil && err != finished {
		return big.Zero(), err
	}

	err = deleteMany(vestingFunds, toDelete)
	if err != nil {
		return big.Zero(), errors.Wrapf(err, "failed to delete locked fund during slash: %v", err)
	}

	st.LockedFunds = big.Sub(st.LockedFunds, amountUnlocked)
	Assert(st.LockedFunds.GreaterThanEqual(big.Zero()))
	st.VestingFunds, err = vestingFunds.Root()
	if err != nil {
		return big.Zero(), err
	}

	return amountUnlocked, nil
}

// Unlocks all vesting funds that have vested before the provided epoch.
// Returns the amount unlocked.
func (st *State) UnlockVestedFunds(store adt.Store, currEpoch abi.ChainEpoch) (abi.TokenAmount, error) {
	vestingFunds, err := adt.AsArray(store, st.VestingFunds)
	if err != nil {
		return abi.TokenAmount{}, err
	}

	amountUnlocked := big.Zero()

	var lockedEntry abi.TokenAmount
	var toDelete []uint64
	var finished = fmt.Errorf("finished")

	// Iterate vestingFunds  in order of release.
	err = vestingFunds.ForEach(&lockedEntry, func(k int64) error {
		if k < int64(currEpoch) {
			amountUnlocked = big.Add(amountUnlocked, lockedEntry)
			toDelete = append(toDelete, uint64(k))
		} else {
			return finished // stop iterating
		}
		return nil
	})

	if err != nil && err != finished {
		return big.Zero(), err
	}

	err = deleteMany(vestingFunds, toDelete)
	if err != nil {
		return big.Zero(), errors.Wrapf(err, "failed to delete locked fund during vest: %v", err)
	}

	st.LockedFunds = big.Sub(st.LockedFunds, amountUnlocked)
	Assert(st.LockedFunds.GreaterThanEqual(big.Zero()))
	st.VestingFunds, err = vestingFunds.Root()
	if err != nil {
		return big.Zero(), err
	}

	return amountUnlocked, nil
}

func (st *State) GetAvailableBalance(actorBalance abi.TokenAmount) abi.TokenAmount {
	availableBal := big.Sub(big.Sub(actorBalance, st.LockedFunds), st.PreCommitDeposits)
	Assert(availableBal.GreaterThanEqual(big.Zero()))
	return availableBal
}

func (st *State) AssertBalanceInvariants(balance abi.TokenAmount) {
	Assert(st.PreCommitDeposits.GreaterThanEqual(big.Zero()))
	Assert(st.LockedFunds.GreaterThanEqual(big.Zero()))
	Assert(balance.GreaterThanEqual(big.Add(st.PreCommitDeposits, st.LockedFunds)))
}

//
// Sectors
//

func (s *SectorOnChainInfo) AsSectorInfo() abi.SectorInfo {
	return abi.SectorInfo{
		RegisteredProof: s.Info.RegisteredProof,
		SectorNumber:    s.Info.SectorNumber,
		SealedCID:       s.Info.SealedCID,
	}
}

func AsStorageWeightDesc(sectorSize abi.SectorSize, sectorInfo *SectorOnChainInfo) *power.SectorStorageWeightDesc {
	return &power.SectorStorageWeightDesc{
		SectorSize: sectorSize,
		DealWeight: sectorInfo.DealWeight,
		Duration:   sectorInfo.Info.Expiration - sectorInfo.ActivationEpoch,
	}
}

//
// Misc helpers
//

func deleteMany(arr *adt.Array, keys []uint64) error {
	// If AMT exposed a batch delete we could save some writes here.
	for _, i := range keys {
		err := arr.Delete(i)
		if err != nil {
			return err
		}
	}
	return nil
}

// Rounds e to the nearest exact multiple of the quantization unit, rounding up.
// Precondition: unit >= 0 else behaviour is undefined
func quantizeUp(e abi.ChainEpoch, unit abi.ChainEpoch) abi.ChainEpoch {
	remainder := e % unit
	if remainder == 0 {
		return e
	}
	return e - remainder + unit
}

// Rounds e to the nearest exact multiple of the quantization unit, rounding down.
// Precondition: unit >= 0 else behaviour is undefined
func quantizeDown(e abi.ChainEpoch, unit abi.ChainEpoch) abi.ChainEpoch {
	remainder := e % unit
	if remainder == 0 {
		return e
	}
	return e - remainder
}

func SectorKey(e abi.SectorNumber) adt.Keyer {
	return adt.UIntKey(uint64(e))
}

func EpochKey(e abi.ChainEpoch) uint64 {
	return uint64(e)
}

func init() {
	// Check that ChainEpoch is indeed an unsigned integer to confirm that SectorKey is making the right interpretation.
	var e abi.SectorNumber
	if reflect.TypeOf(e).Kind() != reflect.Uint64 {
		panic("incorrect sector number encoding")
	}
}

func (p *PeerID) MarshalCBOR(w io.Writer) error {
	if err := cbg.CborWriteHeader(w, cbg.MajByteString, uint64(len([]byte(*p)))); err != nil {
		return err
	}
	if _, err := w.Write([]byte(*p)); err != nil {
		return err
	}
	return nil
}

func (p *PeerID) UnmarshalCBOR(r io.Reader) error {
	t, l, err := cbg.CborReadHeader(r)
	if err != nil {
		return err
	}

	if t != cbg.MajByteString {
		return fmt.Errorf("expected MajByteString when reading peer ID, got %d instead", t)
	}

	if l > cbg.MaxLength {
		return fmt.Errorf("peer ID in input was too long")
	}

	buf := make([]byte, l)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return fmt.Errorf("failed to read full peer ID: %w", err)
	}

	*p = PeerID(buf)
	return nil
}
