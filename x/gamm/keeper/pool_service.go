package keeper

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"github.com/c-osmosis/osmosis/x/gamm/types"
)

func (k Keeper) CreatePool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolParams types.PoolParams,
	records []types.Record,
) (uint64, error) {
	if len(records) < 2 {
		return 0, types.ErrTooLittleRecords
	}
	// TODO: Add the limit of binding token to the pool params?
	if len(records) > 8 {
		return 0, sdkerrors.Wrapf(
			types.ErrTooManyRecords,
			"pool has too many records (%d)", len(records),
		)
	}

	if poolParams.Lock {
		panic("don't create the locked pool")
	}

	poolId := k.GetNextPoolNumber(ctx)
	poolAcc, err := k.NewPool(ctx, poolId, poolParams)
	if err != nil {
		return 0, err
	}

	err = poolAcc.AddRecords(records)
	if err != nil {
		return 0, err
	}

	err = k.SetPool(ctx, poolAcc)
	if err != nil {
		return 0, err
	}

	// Transfer the records tokens to the pool account from the user account.
	var coins sdk.Coins
	for _, record := range records {
		coins = append(coins, record.Token)
	}
	if coins == nil {
		panic("oh my god")
	}

	err = k.bankKeeper.SendCoins(ctx, sender, poolAcc.GetAddress(), coins)
	if err != nil {
		return 0, err
	}

	// Mint the initial 100.000000 share token to the sender
	initialShareSupply := sdk.NewIntWithDecimal(100, 6)
	err = k.MintPoolShareToAccount(ctx, poolAcc, sender, initialShareSupply)
	if err != nil {
		return 0, err
	}

	// Finally, add the share token's meta data to the bank keeper.
	poolShareBaseDenom := types.GetPoolShareDenom(poolAcc.GetId())
	poolShareDisplayDenom := fmt.Sprintf("GAMM-%d", poolAcc.GetId())
	k.bankKeeper.SetDenomMetaData(ctx, banktypes.Metadata{
		Description: fmt.Sprintf("The share token of the gamm pool %d", poolAcc.GetId()),
		DenomUnits: []*banktypes.DenomUnit{
			{
				Denom:    poolShareBaseDenom,
				Exponent: 0,
				Aliases: []string{
					"micropoolshare",
				},
			},
			{
				Denom:    poolShareDisplayDenom,
				Exponent: 6,
				Aliases:  nil,
			},
		},
		Base:    poolShareBaseDenom,
		Display: poolShareDisplayDenom,
	})

	return poolAcc.GetId(), nil
}

func (k Keeper) JoinPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	shareOutAmount sdk.Int,
	tokenInMaxs sdk.Coins,
) (err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return err
	}

	if poolAcc.GetPoolParams().Lock {
		return types.ErrPoolLocked
	}

	totalShareAmount := poolAcc.GetTotalShare().Amount
	shareRatio := shareOutAmount.ToDec().QuoInt(totalShareAmount)
	if shareRatio.LTE(sdk.ZeroDec()) {
		return sdkerrors.Wrapf(types.ErrInvalidMathApprox, "share ratio is zero or negative")
	}

	// Assume that the tokenInMaxAmounts is validated.
	tokenInMaxMap := make(map[string]sdk.Int)
	for _, max := range tokenInMaxs {
		tokenInMaxMap[max.Denom] = max.Amount
	}

	records := poolAcc.GetAllRecords()
	newRecords := make([]types.Record, 0, len(records))
	// Transfer the records tokens to the pool account from the user account.
	var coins sdk.Coins
	for _, record := range records {
		tokenInAmount := shareRatio.MulInt(record.Token.Amount).TruncateInt()
		if tokenInAmount.LTE(sdk.ZeroInt()) {
			return sdkerrors.Wrapf(types.ErrInvalidMathApprox, "token amount is zero or negative")
		}

		if tokenInMaxAmount, ok := tokenInMaxMap[record.Token.Denom]; ok && tokenInAmount.GT(tokenInMaxAmount) {
			return sdkerrors.Wrapf(types.ErrLimitMaxAmount, "%s token is larger than max amount", record.Token.Denom)
		}

		newRecord := types.Record{
			Weight: record.Weight,
			Token:  sdk.NewCoin(record.Token.Denom, record.Token.Amount.Add(tokenInAmount)),
		}
		newRecords = append(newRecords, newRecord)
		coins = append(coins, sdk.NewCoin(record.Token.Denom, tokenInAmount))
	}

	err = poolAcc.SetRecords(newRecords)
	if err != nil {
		return err
	}

	err = k.SetPool(ctx, poolAcc)
	if err != nil {
		return err
	}

	err = k.bankKeeper.SendCoins(ctx, sender, poolAcc.GetAddress(), coins)
	if err != nil {
		return err
	}

	err = k.MintPoolShareToAccount(ctx, poolAcc, sender, shareOutAmount)
	if err != nil {
		return err
	}

	return nil
}

func (k Keeper) JoinSwapExternAmountIn(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	tokenIn sdk.Coin,
	shareOutMinAmount sdk.Int,
) (shareOutAmount sdk.Int, err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return sdk.Int{}, err
	}

	if poolAcc.GetPoolParams().Lock {
		return sdk.Int{}, types.ErrPoolLocked
	}

	record, err := poolAcc.GetRecord(tokenIn.Denom)
	if err != nil {
		return sdk.Int{}, err
	}

	shareOutAmount = calcPoolOutGivenSingleIn(
		record.Token.Amount.ToDec(),
		record.Weight.ToDec(),
		poolAcc.GetTotalShare().Amount.ToDec(),
		poolAcc.GetTotalWeight().ToDec(),
		tokenIn.Amount.ToDec(),
		poolAcc.GetPoolParams().SwapFee,
	).TruncateInt()

	if shareOutAmount.LTE(sdk.ZeroInt()) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrInvalidMathApprox, "share amount is zero or negative")
	}

	if shareOutAmount.LT(shareOutMinAmount) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrLimitMinAmount, "%s token is lesser than min amount", record.Token.Denom)
	}

	record.Token = record.Token.Add(tokenIn)
	err = poolAcc.SetRecords([]types.Record{record})
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.SetPool(ctx, poolAcc)
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.bankKeeper.SendCoins(ctx, sender, poolAcc.GetAddress(), sdk.Coins{tokenIn})
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.MintPoolShareToAccount(ctx, poolAcc, sender, shareOutAmount)
	if err != nil {
		return sdk.Int{}, err
	}

	return shareOutAmount, nil
}

func (k Keeper) JoinSwapShareAmountOut(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	tokenInDenom string,
	shareOutAmount sdk.Int,
	tokenInMaxAmount sdk.Int,
) (tokenInAmount sdk.Int, err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return sdk.Int{}, err
	}

	if poolAcc.GetPoolParams().Lock {
		return sdk.Int{}, types.ErrPoolLocked
	}

	record, err := poolAcc.GetRecord(tokenInDenom)
	if err != nil {
		return sdk.Int{}, err
	}

	tokenInAmount = calcSingleInGivenPoolOut(
		record.Token.Amount.ToDec(),
		record.Weight.ToDec(),
		poolAcc.GetTotalShare().Amount.ToDec(),
		poolAcc.GetTotalWeight().ToDec(),
		shareOutAmount.ToDec(),
		poolAcc.GetPoolParams().SwapFee,
	).TruncateInt()

	if tokenInAmount.LTE(sdk.ZeroInt()) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrInvalidMathApprox, "token amount is zero or negative")
	}

	if tokenInAmount.LT(tokenInMaxAmount) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrLimitMaxAmount, "%s token is larger than max amount", record.Token.Denom)
	}

	record.Token.Amount = record.Token.Amount.Add(tokenInAmount)
	err = poolAcc.SetRecords([]types.Record{record})
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.SetPool(ctx, poolAcc)
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.bankKeeper.SendCoins(ctx, sender, poolAcc.GetAddress(), sdk.Coins{sdk.NewCoin(tokenInDenom, tokenInAmount)})
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.MintPoolShareToAccount(ctx, poolAcc, sender, shareOutAmount)
	if err != nil {
		return sdk.Int{}, err
	}

	return shareOutAmount, nil
}

func (k Keeper) ExitPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	shareInAmount sdk.Int,
	tokenOutMins sdk.Coins,
) (err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return err
	}

	if poolAcc.GetPoolParams().Lock {
		return types.ErrPoolLocked
	}

	totalShareAmount := poolAcc.GetTotalShare().Amount
	exitFee := poolAcc.GetPoolParams().ExitFee.MulInt(shareInAmount).TruncateInt()
	shareInAmountAfterExitFee := shareInAmount.Sub(exitFee)
	shareRatio := shareInAmountAfterExitFee.ToDec().QuoInt(totalShareAmount)

	if shareRatio.LTE(sdk.ZeroDec()) {
		return sdkerrors.Wrapf(types.ErrInvalidMathApprox, "share ratio is zero or negative")
	}

	// Assume that the tokenInMaxAmounts is validated.
	tokenOutMinMap := make(map[string]sdk.Int)
	for _, min := range tokenOutMins {
		tokenOutMinMap[min.Denom] = min.Amount
	}

	records := poolAcc.GetAllRecords()
	newRecords := make([]types.Record, 0, len(records))
	// Transfer the records tokens to the user account from the pool account.
	var coins sdk.Coins
	for _, record := range records {
		tokenOutAmount := shareRatio.MulInt(record.Token.Amount).TruncateInt()
		if tokenOutAmount.LTE(sdk.ZeroInt()) {
			return sdkerrors.Wrapf(types.ErrInvalidMathApprox, "token amount is zero or negative")
		}

		if tokenOutMinAmount, ok := tokenOutMinMap[record.Token.Denom]; ok && tokenOutAmount.LT(tokenOutMinAmount) {
			return sdkerrors.Wrapf(types.ErrLimitMinAmount, "%s token is lesser than min amount", record.Token.Denom)
		}

		newRecord := types.Record{
			Weight: record.Weight,
			Token:  sdk.NewCoin(record.Token.Denom, record.Token.Amount.Sub(tokenOutAmount)),
		}
		newRecords = append(newRecords, newRecord)
		coins = append(coins, sdk.NewCoin(record.Token.Denom, tokenOutAmount))
	}

	err = poolAcc.SetRecords(newRecords)
	if err != nil {
		return err
	}

	err = k.SetPool(ctx, poolAcc)
	if err != nil {
		return err
	}

	err = k.bankKeeper.SendCoins(ctx, poolAcc.GetAddress(), sender, coins)
	if err != nil {
		return err
	}

	// TODO: `balancer` contract sends the exit fee to the `factory` contract.
	//       But, it is unclear that how the exit fees in the `factory` contract are handled.
	//       And, it seems to be not good way to send the exit fee to the pool account,
	//       because the pool account doesn't have the record about exit fee.
	//       So, temporarily, just burn the exit fee.
	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, exitFee)
	if err != nil {
		return err
	}

	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, shareInAmountAfterExitFee)
	if err != nil {
		return err
	}

	return nil
}

func (k Keeper) ExitSwapShareAmountIn(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	tokenOutDenom string,
	shareInAmount sdk.Int,
	tokenOutMinAmount sdk.Int,
) (tokenOutAmount sdk.Int, err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return sdk.Int{}, err
	}

	if poolAcc.GetPoolParams().Lock {
		return sdk.Int{}, types.ErrPoolLocked
	}

	record, err := poolAcc.GetRecord(tokenOutDenom)
	if err != nil {
		return sdk.Int{}, err
	}

	tokenOutAmount = calcSingleOutGivenPoolIn(
		record.Token.Amount.ToDec(),
		record.Weight.ToDec(),
		poolAcc.GetTotalShare().Amount.ToDec(),
		poolAcc.GetTotalWeight().ToDec(),
		shareInAmount.ToDec(),
		poolAcc.GetPoolParams().SwapFee,
	).TruncateInt()

	if tokenOutAmount.LTE(sdk.ZeroInt()) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrInvalidMathApprox, "token amount is zero or negative")
	}

	if tokenOutAmount.LT(tokenOutMinAmount) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrLimitMinAmount, "%s token is lesser than min amount", record.Token.Denom)
	}

	record.Token.Amount = record.Token.Amount.Sub(tokenOutAmount)

	exitFee := poolAcc.GetPoolParams().ExitFee.MulInt(shareInAmount).TruncateInt()
	shareInAmountAfterExitFee := shareInAmount.Sub(exitFee)

	err = k.bankKeeper.SendCoins(ctx, poolAcc.GetAddress(), sender, sdk.Coins{
		sdk.NewCoin(tokenOutDenom, tokenOutAmount),
	})
	if err != nil {
		return sdk.Int{}, err
	}

	// TODO: `balancer` contract sends the exit fee to the `factory` contract.
	//       But, it is unclear that how the exit fees in the `factory` contract are handled.
	//       And, it seems to be not good way to send the exit fee to the pool account,
	//       because the pool account doesn't have the record about exit fee.
	//       So, temporarily, just burn the exit fee.
	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, exitFee)
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, shareInAmountAfterExitFee)
	if err != nil {
		return sdk.Int{}, err
	}

	return tokenOutAmount, nil
}

func (k Keeper) ExitSwapExternAmountOut(
	ctx sdk.Context,
	sender sdk.AccAddress,
	poolId uint64,
	tokenOut sdk.Coin,
	shareInMaxAmount sdk.Int,
) (shareInAmount sdk.Int, err error) {
	poolAcc, err := k.GetPool(ctx, poolId)
	if err != nil {
		return sdk.Int{}, err
	}

	if poolAcc.GetPoolParams().Lock {
		return sdk.Int{}, types.ErrPoolLocked
	}

	record, err := poolAcc.GetRecord(tokenOut.Denom)
	if err != nil {
		return sdk.Int{}, err
	}

	shareInAmount = calcPoolInGivenSingleOut(
		record.Token.Amount.ToDec(),
		record.Weight.ToDec(),
		poolAcc.GetTotalShare().Amount.ToDec(),
		poolAcc.GetTotalWeight().ToDec(),
		tokenOut.Amount.ToDec(),
		poolAcc.GetPoolParams().SwapFee,
	).TruncateInt()

	if shareInAmount.LTE(sdk.ZeroInt()) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrInvalidMathApprox, "token amount is zero or negative")
	}

	if shareInAmount.GT(shareInMaxAmount) {
		return sdk.Int{}, sdkerrors.Wrapf(types.ErrLimitMaxAmount, "%s token is larger than max amount", record.Token.Denom)
	}

	record.Token.Amount = record.Token.Amount.Sub(tokenOut.Amount)

	exitFee := poolAcc.GetPoolParams().ExitFee.MulInt(shareInAmount).TruncateInt()
	shareInAmountAfterExitFee := shareInAmount.Sub(exitFee)

	err = k.bankKeeper.SendCoins(ctx, poolAcc.GetAddress(), sender, sdk.Coins{
		tokenOut,
	})
	if err != nil {
		return sdk.Int{}, err
	}

	// TODO: `balancer` contract sends the exit fee to the `factory` contract.
	//       But, it is unclear that how the exit fees in the `factory` contract are handled.
	//       And, it seems to be not good way to send the exit fee to the pool account,
	//       because the pool account doesn't have the record about exit fee.
	//       So, temporarily, just burn the exit fee.
	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, exitFee)
	if err != nil {
		return sdk.Int{}, err
	}

	err = k.BurnPoolShareFromAccount(ctx, poolAcc, sender, shareInAmountAfterExitFee)
	if err != nil {
		return sdk.Int{}, err
	}

	return shareInAmount, nil
}
