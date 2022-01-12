//
// Copyright 2021, Offchain Labs, Inc. All rights reserved.
//

package precompiles

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/params"
	"github.com/offchainlabs/arbstate/arbos/retryables"
	"github.com/offchainlabs/arbstate/util"
)

type ArbRetryableTx struct {
	Address                 addr
	TicketCreated           func(ctx, mech, [32]byte) error
	LifetimeExtended        func(ctx, mech, [32]byte, huge) error
	RedeemScheduled         func(ctx, mech, [32]byte, [32]byte, uint64, uint64, addr) error
	Redeemed                func(ctx, mech, [32]byte) error
	Canceled                func(ctx, mech, [32]byte) error
	TicketCreatedGasCost    func([32]byte) (uint64, error)
	LifetimeExtendedGasCost func([32]byte, huge) (uint64, error)
	RedeemScheduledGasCost  func([32]byte, [32]byte, uint64, uint64, addr) (uint64, error)
	RedeemedGasCost         func([32]byte) (uint64, error)
	CanceledGasCost         func([32]byte) (uint64, error)
}

var NotFoundError = errors.New("ticketId not found")

func (con ArbRetryableTx) Cancel(c ctx, evm mech, ticketId [32]byte) error {
	retryableState := c.state.RetryableState()
	retryable, err := retryableState.OpenRetryable(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return err
	}
	if retryable == nil {
		return NotFoundError
	}
	beneficiary, err := retryable.Beneficiary()
	if err != nil {
		return err
	}
	if c.caller != beneficiary {
		return errors.New("only the beneficiary may cancel a retryable")
	}

	// no refunds are given for deleting retryables because they use rented space
	_, err = retryableState.DeleteRetryable(ticketId)
	if err != nil {
		return err
	}
	return con.Canceled(c, evm, ticketId)
}

func (con ArbRetryableTx) GetBeneficiary(c ctx, evm mech, ticketId [32]byte) (addr, error) {
	retryableState := c.state.RetryableState()
	retryable, err := retryableState.OpenRetryable(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return addr{}, err
	}
	if retryable == nil {
		return addr{}, NotFoundError
	}
	return retryable.Beneficiary()
}

func (con ArbRetryableTx) GetLifetime(c ctx, evm mech) (huge, error) {
	// there's no need to burn gas for something this cheap
	return big.NewInt(retryables.RetryableLifetimeSeconds), nil
}

func (con ArbRetryableTx) GetTimeout(c ctx, evm mech, ticketId [32]byte) (huge, error) {
	retryableState := c.state.RetryableState()
	retryable, err := retryableState.OpenRetryable(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return nil, err
	}
	if retryable == nil {
		return nil, NotFoundError
	}
	timeout, err := retryable.Timeout()
	if err != nil {
		return nil, err
	}
	return big.NewInt(int64(timeout)), nil
}

func (con ArbRetryableTx) Keepalive(c ctx, evm mech, ticketId [32]byte) (huge, error) {

	// charge for the expiry update
	retryableState := c.state.RetryableState()
	nbytes, err := retryableState.RetryableSizeBytes(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return nil, err
	}
	if nbytes == 0 {
		return nil, NotFoundError
	}
	updateCost := util.WordsForBytes(nbytes) * params.SstoreSetGas / 100
	if err := c.Burn(updateCost); err != nil {
		return big.NewInt(0), err
	}

	currentTime := evm.Context.Time.Uint64()
	window := currentTime + retryables.RetryableLifetimeSeconds
	err = retryableState.Keepalive(ticketId, currentTime, window, retryables.RetryableLifetimeSeconds)
	if err != nil {
		return big.NewInt(0), err
	}

	retryable, err := retryableState.OpenRetryable(ticketId, currentTime)
	if err != nil {
		return nil, err
	}
	newTimeout, err := retryable.Timeout()
	if err != nil {
		return nil, err
	}
	err = con.LifetimeExtended(c, evm, ticketId, big.NewInt(int64(newTimeout)))
	if err != nil {
		return nil, err
	}
	return big.NewInt(int64(newTimeout)), nil
}

func (con ArbRetryableTx) Redeem(c ctx, evm mech, ticketId [32]byte) ([32]byte, error) {

	retryableState := c.state.RetryableState()
	byteCount, err := retryableState.RetryableSizeBytes(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return hash{}, err
	}
	writeBytes := util.WordsForBytes(byteCount)
	if err := c.Burn(params.SloadGas * writeBytes); err != nil {
		return hash{}, err
	}

	retryable, err := retryableState.OpenRetryable(ticketId, evm.Context.Time.Uint64())
	if err != nil {
		return hash{}, err
	}
	if retryable == nil {
		return hash{}, NotFoundError
	}
	sequenceNum, err := retryable.IncrementNumTries()
	if err != nil {
		return hash{}, err
	}
	redeemTxId := retryables.TxIdForRedeemAttempt(ticketId, sequenceNum)
	err = con.RedeemScheduled(c, evm, ticketId, redeemTxId, sequenceNum, c.gasLeft, c.caller)
	if err != nil {
		return hash{}, err
	}

	// To prepare for the enqueued retry event, we burn gas here, adding it back to the pool right before retrying.
	// The gas payer for this tx will get a credit for the wei they paid for this gas when retrying.

	// We want to donate as much gas as we can to the retry, but to do this safely the user must not run out
	// of gas later. Since the only charge that happens after this method returns is for encoding the return
	// result, we'll donate all but that known cost to the retry.

	// ensure the user will be able to pay for the return result
	gasCostToReturnResult := 32 * params.CopyGas
	if c.gasLeft < gasCostToReturnResult {
		return hash{}, c.Burn(gasCostToReturnResult)
	}

	if err := c.Burn(c.gasLeft - gasCostToReturnResult); err != nil {
		return hash{}, err
	}
	return redeemTxId, nil
}
