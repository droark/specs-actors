package market

import (
	addr "github.com/filecoin-project/go-address"

	cbg "github.com/whyrusleeping/cbor-gen"

	abi "github.com/filecoin-project/specs-actors/actors/abi"
	big "github.com/filecoin-project/specs-actors/actors/abi/big"
	builtin "github.com/filecoin-project/specs-actors/actors/builtin"
	verifreg "github.com/filecoin-project/specs-actors/actors/builtin/verifreg"
	vmr "github.com/filecoin-project/specs-actors/actors/runtime"
	exitcode "github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	. "github.com/filecoin-project/specs-actors/actors/util"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
)

type Actor struct{}

type Runtime = vmr.Runtime

func (a Actor) Exports() []interface{} {
	return []interface{}{
		builtin.MethodConstructor: a.Constructor,
		2:                         a.AddBalance,
		3:                         a.WithdrawBalance,
		4:                         a.HandleExpiredDeals,
		5:                         a.PublishStorageDeals,
		6:                         a.VerifyDealsOnSectorProveCommit,
		7:                         a.OnMinerSectorsTerminate,
		8:                         a.ComputeDataCommitment,
		9:                         a.HandleInitTimeoutDeals,
	}
}

var _ abi.Invokee = Actor{}

////////////////////////////////////////////////////////////////////////////////
// Actor methods
////////////////////////////////////////////////////////////////////////////////

func (a Actor) Constructor(rt Runtime, _ *adt.EmptyValue) *adt.EmptyValue {
	rt.ValidateImmediateCallerIs(builtin.SystemActorAddr)

	emptyArray, err := adt.MakeEmptyArray(adt.AsStore(rt)).Root()
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalState, "failed to create storage market state: %v", err)
	}

	emptyMap, err := adt.MakeEmptyMap(adt.AsStore(rt)).Root()
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalState, "failed to create storage market state: %v", err)
	}

	emptyMSet, err := MakeEmptySetMultimap(adt.AsStore(rt)).Root()
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalState, "failed to create storage market state: %v", err)
	}

	st := ConstructState(emptyArray, emptyMap, emptyMSet)
	rt.State().Create(st)
	return nil
}

type WithdrawBalanceParams struct {
	ProviderOrClientAddress addr.Address
	Amount                  abi.TokenAmount
}

// Attempt to withdraw the specified amount from the balance held in escrow.
// If less than the specified amount is available, yields the entire available balance.
func (a Actor) WithdrawBalance(rt Runtime, params *WithdrawBalanceParams) *adt.EmptyValue {
	if params.Amount.LessThan(big.Zero()) {
		rt.Abortf(exitcode.ErrIllegalArgument, "negative amount %v", params.Amount)
	}

	nominal, recipient := escrowAddress(rt, params.ProviderOrClientAddress)

	amountSlashedTotal := abi.NewTokenAmount(0)
	var amountExtracted abi.TokenAmount
	var st State
	rt.State().Transaction(&st, func() interface{} {
		// Before any operations that check the balance tables for funds, execute all deferred
		// deal state updates.
		amountSlashedTotal = big.Add(amountSlashedTotal, st.updatePendingDealStatesForParty(rt, nominal))

		minBalance := st.GetLockedBalance(rt, nominal)

		et, err := adt.AsBalanceTable(adt.AsStore(rt), st.EscrowTable)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "load escrow table: %v", err)
		}
		ex, err := et.SubtractWithMinimum(nominal, params.Amount, minBalance)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "subtract form escrow table: %v", err)
		}

		etc, err := et.Root()
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to flush escrow table: %w", err)
		}
		st.EscrowTable = etc
		amountExtracted = ex
		return nil
	})

	if amountSlashedTotal.GreaterThan(big.Zero()) {
		_, code := rt.Send(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, amountSlashedTotal)
		builtin.RequireSuccess(rt, code, "failed to burn slashed funds")
	}

	_, code := rt.Send(recipient, builtin.MethodSend, nil, amountExtracted)
	builtin.RequireSuccess(rt, code, "failed to send funds")
	return nil
}

// Deposits the received value into the balance held in escrow.
func (a Actor) AddBalance(rt Runtime, providerOrClientAddress *addr.Address) *adt.EmptyValue {
	nominal, _ := escrowAddress(rt, *providerOrClientAddress)

	var st State
	rt.State().Transaction(&st, func() interface{} {
		msgValue := rt.Message().ValueReceived()

		err := st.AddEscrowBalance(adt.AsStore(rt), nominal, msgValue)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "adding to escrow table: %v", err)
		}

		// ensure there is an entry in the locked table
		err = st.AddLockedBalance(adt.AsStore(rt), nominal, big.Zero())
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalArgument, "adding to locked table: %v", err)
		}

		return nil
	})
	return nil
}

type PublishStorageDealsParams struct {
	Deals []ClientDealProposal
}

type PublishStorageDealsReturn struct {
	IDs []abi.DealID
}

// Publish a new set of storage deals (not yet included in a sector).
func (a Actor) PublishStorageDeals(rt Runtime, params *PublishStorageDealsParams) *PublishStorageDealsReturn {
	amountSlashedTotal := abi.NewTokenAmount(0)

	// Deal message must have a From field identical to the provider of all the deals.
	// This allows us to retain and verify only the client's signature in each deal proposal itself.
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	if len(params.Deals) == 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "empty deals parameter")
	}

	// All deals should have the same provider so get worker once
	providerRaw := params.Deals[0].Proposal.Provider
	provider, ok := rt.ResolveAddress(providerRaw)
	if !ok {
		rt.Abortf(exitcode.ErrNotFound, "failed to resolve provider address %v", providerRaw)
	}

	_, worker := builtin.RequestMinerControlAddrs(rt, provider)
	if worker != rt.Message().Caller() {
		rt.Abortf(exitcode.ErrForbidden, "caller is not provider %v", provider)
	}

	for _, deal := range params.Deals {
		// Check VerifiedClient allowed cap and deduct PieceSize from cap.
		// Either the DealSize is within the available DataCap of the VerifiedClient
		// or this message will fail. We do not allow a deal that is partially verified.
		if deal.Proposal.VerifiedDeal {
			_, code := rt.Send(
				builtin.VerifiedRegistryActorAddr,
				builtin.MethodsVerifiedRegistry.UseBytes,
				&verifreg.UseBytesParams{
					Address:  deal.Proposal.Client,
					DealSize: big.NewIntUnsigned(uint64(deal.Proposal.PieceSize)),
				},
				abi.NewTokenAmount(0),
			)
			builtin.RequireSuccess(rt, code, "failed to add verified deal for client: %v", deal.Proposal.Client)
		}
	}

	var newDealIds []abi.DealID
	var st State
	rt.State().Transaction(&st, func() interface{} {
		proposals, err := AsDealProposalArray(adt.AsStore(rt), st.Proposals)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to load proposals array: %s", err)
		}

		dbp, err := AsSetMultimap(adt.AsStore(rt), st.DealIDsByParty)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to load deal ids set: %s", err)
		}

		// All storage proposals will be added in an atomic transaction; this operation will be unrolled if any of them fails.
		for _, deal := range params.Deals {
			validateDeal(rt, deal)

			if deal.Proposal.Provider != provider && deal.Proposal.Provider != providerRaw {
				rt.Abortf(exitcode.ErrIllegalArgument, "cannot publish deals from different providers at the same time")
			}

			client, ok := rt.ResolveAddress(deal.Proposal.Client)
			if !ok {
				rt.Abortf(exitcode.ErrNotFound, "failed to resolve client address %v", deal.Proposal.Client)
			}
			// Normalise provider and client addresses in the proposal stored on chain (after signature verification).
			deal.Proposal.Provider = provider
			deal.Proposal.Client = client

			// Before any operations that check the balance tables for funds, execute all deferred
			// deal state updates.
			//
			// Note: as an optimization, implementations may cache efficient data structures indicating
			// which of the following set of updates are redundant and can be skipped.
			amountSlashedTotal = big.Add(amountSlashedTotal, st.updatePendingDealStatesForParty(rt, client))
			amountSlashedTotal = big.Add(amountSlashedTotal, st.updatePendingDealStatesForParty(rt, provider))

			st.lockBalanceOrAbort(rt, client, deal.Proposal.ClientBalanceRequirement())
			st.lockBalanceOrAbort(rt, provider, deal.Proposal.ProviderBalanceRequirement())

			id := st.generateStorageDealID()

			err := proposals.Set(id, &deal.Proposal)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "set deal: %v", err)
			}

			if err = dbp.Put(client, id); err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "set client deal id: %v", err)
			}
			if err = dbp.Put(provider, id); err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "set provider deal id: %v", err)
			}

			newDealIds = append(newDealIds, id)
		}
		propc, err := proposals.Root()
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to flush proposal set: %w", err)
		}
		st.Proposals = propc

		dipc, err := dbp.Root()
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to flush deal ids map: %w", err)
		}

		st.DealIDsByParty = dipc
		return nil
	})

	_, code := rt.Send(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, amountSlashedTotal)
	builtin.RequireSuccess(rt, code, "failed to burn funds")
	return &PublishStorageDealsReturn{newDealIds}
}

type VerifyDealsOnSectorProveCommitParams struct {
	DealIDs      []abi.DealID
	SectorSize   abi.SectorSize
	SectorExpiry abi.ChainEpoch
}

type VerifyDealsOnSectorProveCommitReturn struct {
	DealWeight         abi.DealWeight
	VerifiedDealWeight abi.DealWeight
}

// Verify that a given set of storage deals is valid for a sector currently being ProveCommitted,
// update the market's internal state accordingly, and return DealWeight of the set of storage deals given.
// Note: in the case of a capacity-commitment sector (one with zero deals), this function should succeed vacuously.
// The weight is defined as the sum, over all deals in the set, of the product of its size
// with its duration. This quantity may be an input into the functions specifying block reward,
// sector power, collateral, and/or other parameters.
func (a Actor) VerifyDealsOnSectorProveCommit(rt Runtime, params *VerifyDealsOnSectorProveCommitParams) *VerifyDealsOnSectorProveCommitReturn {
	rt.ValidateImmediateCallerType(builtin.StorageMinerActorCodeID)
	minerAddr := rt.Message().Caller()
	totalDealSpaceTime := big.Zero()
	totalVerifiedDealSpaceTime := big.Zero()

	var st State
	rt.State().Transaction(&st, func() interface{} {
		// if there are no dealIDs, it is a CommittedCapacity sector
		// and the totalDealSpaceTime should be zero
		states, err := AsDealStateArray(adt.AsStore(rt), st.States)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "load states %v", err)
		}

		proposals, err := AsDealProposalArray(adt.AsStore(rt), st.Proposals)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "load proposals %v", err)
		}

		for _, dealID := range params.DealIDs {
			deal, err := states.Get(dealID)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "get deal %v", err)
			}
			proposal, err := proposals.Get(dealID)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "get deal %v", err)
			}

			validateDealCanActivate(rt, minerAddr, params.SectorExpiry, deal, proposal)

			deal.SectorStartEpoch = rt.CurrEpoch()
			err = states.Set(dealID, deal)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "set deal %v", err)
			}

			// Compute deal weight
			dealDuration := big.NewInt(int64(proposal.Duration()))
			dealSize := big.NewIntUnsigned(uint64(proposal.PieceSize))
			dealSpaceTime := big.Mul(dealDuration, dealSize)

			if proposal.VerifiedDeal {
				totalVerifiedDealSpaceTime = big.Add(totalVerifiedDealSpaceTime, dealSpaceTime)
			} else {
				totalDealSpaceTime = big.Add(totalDealSpaceTime, dealSpaceTime)
			}

		}

		st.States, err = states.Root()
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to flush deal states: %s", err)
		}
		return nil
	})
	return &VerifyDealsOnSectorProveCommitReturn{
		DealWeight:         totalDealSpaceTime,
		VerifiedDealWeight: totalVerifiedDealSpaceTime,
	}
}

type ComputeDataCommitmentParams struct {
	DealIDs    []abi.DealID
	SectorType abi.RegisteredProof
}

func (a Actor) ComputeDataCommitment(rt Runtime, params *ComputeDataCommitmentParams) *cbg.CborCid {
	rt.ValidateImmediateCallerType(builtin.StorageMinerActorCodeID)

	pieces := make([]abi.PieceInfo, 0)
	var st State
	rt.State().Transaction(&st, func() interface{} {
		for _, dealID := range params.DealIDs {
			deal := st.mustGetDeal(rt, dealID)
			pieces = append(pieces, abi.PieceInfo{
				PieceCID: deal.PieceCID,
				Size:     deal.PieceSize,
			})
		}
		return nil
	})

	commd, err := rt.Syscalls().ComputeUnsealedSectorCID(params.SectorType, pieces)
	if err != nil {
		rt.Abortf(exitcode.SysErrorIllegalArgument, "failed to compute unsealed sector CID: %s", err)
	}

	return (*cbg.CborCid)(&commd)
}

type OnMinerSectorsTerminateParams struct {
	DealIDs []abi.DealID
}

// Terminate a set of deals in response to their containing sector being terminated.
// Slash provider collateral, refund client collateral, and refund partial unpaid escrow
// amount to client.
func (a Actor) OnMinerSectorsTerminate(rt Runtime, params *OnMinerSectorsTerminateParams) *adt.EmptyValue {
	rt.ValidateImmediateCallerType(builtin.StorageMinerActorCodeID)
	minerAddr := rt.Message().Caller()

	var st State
	rt.State().Transaction(&st, func() interface{} {
		proposals, err := AsDealProposalArray(adt.AsStore(rt), st.Proposals)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "load proposals: %v", err)
		}

		states, err := AsDealStateArray(adt.AsStore(rt), st.States)
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "load states: %v", err)
		}

		for _, dealID := range params.DealIDs {
			deal, err := proposals.Get(dealID)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "get deal: %v", err)
			}
			Assert(deal.Provider == minerAddr)

			state, err := states.Get(dealID)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "get deal: %v", err)
			}

			// Note: we do not perform the balance transfers here, but rather simply record the flag
			// to indicate that processDealSlashed should be called when the deferred state computation
			// is performed. // TODO: Do that here

			state.SlashEpoch = rt.CurrEpoch()
			err = states.Set(dealID, state)
			if err != nil {
				rt.Abortf(exitcode.ErrIllegalState, "set deal: %v", err)
			}
		}

		st.States, err = states.Root()
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to flush states: %s", err)
		}
		return nil
	})
	return nil
}

type HandleExpiredDealsParams struct {
	Deals []abi.DealID // TODO: RLE
}

func (a Actor) HandleExpiredDeals(rt Runtime, params *HandleExpiredDealsParams) *adt.EmptyValue {
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	var slashed abi.TokenAmount
	var st State
	rt.State().Transaction(&st, func() interface{} {
		slashed = st.updatePendingDealStates(rt, params.Deals, rt.CurrEpoch())
		return nil
	})

	// TODO: award some small portion of slashed to caller as incentive

	_, code := rt.Send(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, slashed)
	builtin.RequireSuccess(rt, code, "failed to burn funds")
	return nil
}

type HandleInitTimeoutDealsParams struct {
	Deals []abi.DealID // TODO: RLE
}

func (a Actor) HandleInitTimeoutDeals(rt Runtime, params *HandleInitTimeoutDealsParams) *adt.EmptyValue {
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	var st State
	var verifiedDeals []*DealProposal
	slashedAmount := rt.State().Transaction(&st, func() interface{} {
		slashed := abi.NewTokenAmount(0)
		for _, dealID := range params.Deals {
			deal := st.mustGetDeal(rt, dealID)
			state := st.mustGetDealState(rt, dealID)

			// Deal has not been activated.
			if state.SectorStartEpoch == epochUndefined {
				// Now is after StartEpoch when the Deal should have been activated, hence clean up.
				if rt.CurrEpoch() > deal.StartEpoch {
					// Store VerifiedDeal to restore bytes for VerifiedClient.
					if deal.VerifiedDeal {
						verifiedDeals = append(verifiedDeals, deal)
					}
					newlySlashed := st.processDealInitTimedOut(rt, dealID)
					big.Add(slashed, newlySlashed)
				} else {
					// All deals must have timed out.
					rt.Abortf(exitcode.ErrIllegalArgument, "not all deals have timed out: %d", dealID)
				}
			}
		}

		return &slashed
	}).(abi.TokenAmount)

	// TODO: award some small portion of slashed to caller as incentive

	// Restore verified dataset allowance for verified clients.
	for _, deal := range verifiedDeals {
		_, code := rt.Send(
			builtin.VerifiedRegistryActorAddr,
			builtin.MethodsVerifiedRegistry.RestoreBytes,
			&verifreg.RestoreBytesParams{
				Address:  deal.Client,
				DealSize: big.NewIntUnsigned(uint64(deal.PieceSize)),
			},
			abi.NewTokenAmount(0),
		)
		builtin.RequireSuccess(rt, code, "failed to restore bytes for verified client: %v", deal.Client)
	}

	_, code := rt.Send(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, slashedAmount)
	builtin.RequireSuccess(rt, code, "failed to burn funds")
	return nil
}

////////////////////////////////////////////////////////////////////////////////
// Checks
////////////////////////////////////////////////////////////////////////////////

func validateDealCanActivate(rt Runtime, minerAddr addr.Address, sectorExpiration abi.ChainEpoch, deal *DealState, proposal *DealProposal) {
	if proposal.Provider != minerAddr {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal has incorrect miner as its provider.")
	}

	if deal.SectorStartEpoch != epochUndefined {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal has already appeared in proven sector.")
	}

	if rt.CurrEpoch() > proposal.StartEpoch {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal start epoch has already elapsed.")
	}

	if proposal.EndEpoch > sectorExpiration {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal would outlive its containing sector.")
	}
}

func validateDeal(rt Runtime, deal ClientDealProposal) {
	if err := dealProposalIsInternallyValid(rt, deal); err != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, "Invalid deal proposal: %s", err)
	}

	proposal := deal.Proposal

	if rt.CurrEpoch() > proposal.StartEpoch {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal start epoch has already elapsed.")
	}

	minDuration, maxDuration := dealDurationBounds(proposal.PieceSize)
	if proposal.Duration() < minDuration || proposal.Duration() > maxDuration {
		rt.Abortf(exitcode.ErrIllegalArgument, "Deal duration out of bounds.")
	}

	minPrice, maxPrice := dealPricePerEpochBounds(proposal.PieceSize, proposal.Duration())
	if proposal.StoragePricePerEpoch.LessThan(minPrice) || proposal.StoragePricePerEpoch.GreaterThan(maxPrice) {
		rt.Abortf(exitcode.ErrIllegalArgument, "Storage price out of bounds.")
	}

	minProviderCollateral, maxProviderCollateral := dealProviderCollateralBounds(proposal.PieceSize, proposal.Duration())
	if proposal.ProviderCollateral.LessThan(minProviderCollateral) || proposal.ProviderCollateral.GreaterThan(maxProviderCollateral) {
		rt.Abortf(exitcode.ErrIllegalArgument, "Provider collateral out of bounds.")
	}

	minClientCollateral, maxClientCollateral := dealClientCollateralBounds(proposal.PieceSize, proposal.Duration())
	if proposal.ClientCollateral.LessThan(minClientCollateral) || proposal.ClientCollateral.GreaterThan(maxClientCollateral) {
		rt.Abortf(exitcode.ErrIllegalArgument, "Client collateral out of bounds.")
	}
}

// Resolves a provider or client address to the canonical form against which a balance should be held, and
// the designated recipient address of withdrawals (which is the same, for simple account parties).
func escrowAddress(rt Runtime, addr addr.Address) (nominal addr.Address, recipient addr.Address) {
	// Resolve the provided address to the canonical form against which the balance is held.
	nominal, ok := rt.ResolveAddress(addr)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "failed to resolve address %v", addr)
	}

	codeID, ok := rt.GetActorCodeCID(nominal)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "no code for address %v", nominal)
	}

	if codeID.Equals(builtin.StorageMinerActorCodeID) {
		// Storage miner actor entry; implied funds recipient is the associated owner address.
		ownerAddr, workerAddr := builtin.RequestMinerControlAddrs(rt, nominal)
		rt.ValidateImmediateCallerIs(ownerAddr, workerAddr)
		return nominal, ownerAddr
	}

	// Ordinary account-style actor entry; funds recipient is just the entry address itself.
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	return nominal, nominal
}
