package keeper

import (
	"errors"
	"fmt"
	"strconv"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	legacyerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/lavanet/lava/utils"
	epochstoragetypes "github.com/lavanet/lava/x/epochstorage/types"
	rewardstypes "github.com/lavanet/lava/x/rewards/types"
	"github.com/lavanet/lava/x/subscription/types"
)

const LIMIT_TOKEN_PER_CU = 100

// GetTrackedCu gets the tracked CU counter (with QoS influence) and the trackedCu entry's block
func (k Keeper) GetTrackedCu(ctx sdk.Context, sub string, provider string, chainID string, subBlock uint64) (cu uint64, found bool, key string) {
	cuTrackerKey := types.CuTrackerKey(sub, provider, chainID)
	var trackedCu types.TrackedCu
	entryBlock, _, _, found := k.cuTrackerFS.FindEntryDetailed(ctx, cuTrackerKey, subBlock, &trackedCu)
	if !found || entryBlock != subBlock {
		// entry not found/deleted -> this is the first, so not an error. return CU=0
		return 0, false, cuTrackerKey
	}
	return trackedCu.Cu, found, cuTrackerKey
}

// AddTrackedCu adds CU to the CU counters in relevant trackedCu entry
func (k Keeper) AddTrackedCu(ctx sdk.Context, sub string, provider string, chainID string, cuToAdd uint64, block uint64) error {
	cu, found, key := k.GetTrackedCu(ctx, sub, provider, chainID, block)

	// Note that the trackedCu entry usually has one version since we used
	// the subscription's block which is constant during a specific month
	// (updating an entry using append in the same block acts as ModifyEntry).
	// At most, there can be two trackedCu entries. Two entries occur
	// in the time period after a month has passed but before the payment
	// timer ended (in this time, a provider can still request payment for the previous month)
	if found {
		k.cuTrackerFS.ModifyEntry(ctx, key, block, &types.TrackedCu{Cu: cu + cuToAdd})
	} else {
		err := k.cuTrackerFS.AppendEntry(ctx, key, block, &types.TrackedCu{Cu: cuToAdd})
		if err != nil {
			return utils.LavaFormatError("cannot create new tracked CU entry", err,
				utils.Attribute{Key: "tracked_cu_key", Value: key},
				utils.Attribute{Key: "sub_block", Value: strconv.FormatUint(block, 10)},
				utils.Attribute{Key: "current_cu", Value: strconv.FormatUint(cu, 10)},
				utils.Attribute{Key: "cu_to_be_added", Value: strconv.FormatUint(cuToAdd, 10)})
		}
	}

	utils.LavaFormatDebug("adding tracked cu",
		utils.LogAttr("sub", sub),
		utils.LogAttr("provider", provider),
		utils.LogAttr("chain_id", chainID),
		utils.LogAttr("added_cu", cuToAdd),
		utils.LogAttr("block", block))

	return nil
}

// GetAllSubTrackedCuIndices gets all the trackedCu entries that are related to a specific subscription
func (k Keeper) GetAllSubTrackedCuIndices(ctx sdk.Context, sub string) []string {
	return k.cuTrackerFS.GetAllEntryIndicesWithPrefix(ctx, sub)
}

// removeCuTracker removes a trackedCu entry
func (k Keeper) resetCuTracker(ctx sdk.Context, sub string, info trackedCuInfo, subBlock uint64) error {
	key := types.CuTrackerKey(sub, info.provider, info.chainID)
	var trackedCu types.TrackedCu
	_, _, isLatest, _ := k.cuTrackerFS.FindEntryDetailed(ctx, key, subBlock, &trackedCu)
	if isLatest {
		return k.cuTrackerFS.DelEntry(ctx, key, uint64(ctx.BlockHeight()))
	}
	return nil
}

type trackedCuInfo struct {
	provider  string
	chainID   string
	trackedCu uint64
	block     uint64
}

func (k Keeper) GetSubTrackedCuInfo(ctx sdk.Context, sub string, block uint64) (trackedCuList []trackedCuInfo, totalCuTracked uint64) {
	keys := k.GetAllSubTrackedCuIndices(ctx, sub)

	for _, key := range keys {
		_, provider, chainID := types.DecodeCuTrackerKey(key)
		cu, found, _ := k.GetTrackedCu(ctx, sub, provider, chainID, block)
		if !found {
			utils.LavaFormatWarning("cannot remove cu tracker", legacyerrors.ErrKeyNotFound,
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "block", Value: strconv.FormatUint(block, 10)},
			)
			continue
		}
		trackedCuList = append(trackedCuList, trackedCuInfo{
			provider:  provider,
			trackedCu: cu,
			chainID:   chainID,
			block:     block,
		})
		totalCuTracked += cu
	}

	return trackedCuList, totalCuTracked
}

// remove only before the sub is deleted
func (k Keeper) RewardAndResetCuTracker(ctx sdk.Context, cuTrackerTimerKeyBytes []byte, cuTrackerTimerData []byte) {
	sub := string(cuTrackerTimerKeyBytes)
	var timerData types.CuTrackerTimerData
	err := k.cdc.Unmarshal(cuTrackerTimerData, &timerData)
	if err != nil {
		utils.LavaFormatError(types.ErrCuTrackerPayoutFailed.Error(), fmt.Errorf("invalid data from cu tracker timer"),
			utils.Attribute{Key: "timer_data", Value: timerData.String()},
			utils.Attribute{Key: "consumer", Value: sub},
		)
		return
	}
	trackedCuList, totalCuTracked := k.GetSubTrackedCuInfo(ctx, sub, timerData.Block)

	if len(trackedCuList) == 0 || totalCuTracked == 0 {
		// no tracked CU for this sub, nothing to do
		return
	}

	// Note: We take the subscription from the FixationStore, based on the given block.
	// So, even if the plan changed during the month, we still take the original plan, based on the given block.
	block := trackedCuList[0].block

	totalTokenAmount := timerData.Credit.Amount
	if totalTokenAmount.Quo(sdk.NewIntFromUint64(totalCuTracked)).GT(sdk.NewIntFromUint64(LIMIT_TOKEN_PER_CU)) {
		totalTokenAmount = sdk.NewIntFromUint64(LIMIT_TOKEN_PER_CU * totalCuTracked)
	}

	// get the adjustment factor, and delete the entries
	adjustments := k.GetConsumerAdjustments(ctx, sub)
	adjustmentFactorForProvider := k.GetAdjustmentFactorProvider(ctx, adjustments)
	k.RemoveConsumerAdjustments(ctx, sub)

	totalTokenRewarded := sdk.ZeroInt()
	for _, trackedCuInfo := range trackedCuList {
		trackedCu := trackedCuInfo.trackedCu
		provider := trackedCuInfo.provider
		chainID := trackedCuInfo.chainID

		providerAddr, err := sdk.AccAddressFromBech32(provider)
		if err != nil {
			utils.LavaFormatError("invalid provider address", err,
				utils.Attribute{Key: "provider", Value: provider},
			)
			continue
		}

		err = k.resetCuTracker(ctx, sub, trackedCuInfo, block)
		if err != nil {
			utils.LavaFormatError("removing/reseting tracked CU entry failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "tracked_cu", Value: trackedCu},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "block", Value: ctx.BlockHeight()},
			)
			continue
		}

		// provider monthly reward = (tracked_CU / total_CU_used_in_sub_this_month) * totalTokenAmount
		providerAdjustment, ok := adjustmentFactorForProvider[provider]
		if !ok {
			maxRewardBoost := k.rewardsKeeper.MaxRewardBoost(ctx)
			if maxRewardBoost == 0 {
				utils.LavaFormatWarning("maxRewardBoost is zero", fmt.Errorf("critical: Attempt to divide by zero"),
					utils.LogAttr("maxRewardBoost", maxRewardBoost),
				)
				return
			}
			providerAdjustment = sdk.OneDec().QuoInt64(int64(maxRewardBoost))
		}

		// calculate the provider reward (smaller than totalMonthlyReward
		// because it's shared with delegators)
		totalMonthlyReward := k.CalcTotalMonthlyReward(ctx, totalTokenAmount, trackedCu, totalCuTracked)
		creditToSub := sdk.NewCoin(k.stakingKeeper.BondDenom(ctx), totalMonthlyReward)
		totalTokenRewarded = totalTokenRewarded.Add(totalMonthlyReward)

		// aggregate the reward for the provider
		k.rewardsKeeper.AggregateRewards(ctx, provider, chainID, providerAdjustment, totalMonthlyReward)

		// Transfer some of the total monthly reward to validators contribution and community pool
		totalMonthlyReward, err = k.rewardsKeeper.ContributeToValidatorsAndCommunityPool(ctx, totalMonthlyReward, types.ModuleName)
		if err != nil {
			utils.LavaFormatError("could not contribute to validators and community pool", err,
				utils.Attribute{Key: "total_monthly_reward", Value: totalMonthlyReward.String() + k.stakingKeeper.BondDenom(ctx)})
		}

		// Note: if the reward function doesn't reward the provider
		// because he was unstaked, we only print an error and not returning
		providerReward, _, err := k.dualstakingKeeper.RewardProvidersAndDelegators(ctx, providerAddr, chainID, totalMonthlyReward, types.ModuleName, false, false, false)
		if errors.Is(err, epochstoragetypes.ErrProviderNotStaked) || errors.Is(err, epochstoragetypes.ErrStakeStorageNotFound) {
			utils.LavaFormatWarning("sending provider reward with delegations failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "block", Value: strconv.FormatInt(ctx.BlockHeight(), 10)},
			)
		} else if err != nil {
			utils.LavaFormatError("sending provider reward with delegations failed", err,
				utils.Attribute{Key: "provider", Value: provider},
				utils.Attribute{Key: "tracked_cu", Value: trackedCu},
				utils.Attribute{Key: "chain_id", Value: chainID},
				utils.Attribute{Key: "sub", Value: sub},
				utils.Attribute{Key: "sub_total_used_cu", Value: totalCuTracked},
				utils.Attribute{Key: "block", Value: ctx.BlockHeight()},
			)
		} else {
			utils.LogLavaEvent(ctx, k.Logger(ctx), types.MonthlyCuTrackerProviderRewardEventName, map[string]string{
				"provider":       provider,
				"sub":            sub,
				"tracked_cu":     strconv.FormatUint(trackedCu, 10),
				"credit_used":    creditToSub.String(),
				"reward":         providerReward.String(),
				"block":          strconv.FormatInt(ctx.BlockHeight(), 10),
				"adjustment_raw": providerAdjustment.String(),
			}, "Provider got monthly reward successfully")
		}
	}

	rewardsRemainder := totalTokenAmount.Sub(totalTokenRewarded)

	var latestSub types.Subscription
	latestEntryBlock, _, _, found := k.subsFS.FindEntryDetailed(ctx, sub, uint64(ctx.BlockHeight()), &latestSub)
	if found {
		if latestSub.Credit.Amount.LT(totalTokenRewarded) {
			latestSub.Credit.Amount = sdk.ZeroInt()
			utils.LavaFormatWarning("providers rewarded more than the subscription credit", nil,
				utils.LogAttr("credit", latestSub.Credit.String()),
				utils.LogAttr("rewarded", totalTokenRewarded),
				utils.LogAttr("subscription", sub),
			)
		} else {
			latestSub.Credit.Amount = latestSub.Credit.Amount.Sub(totalTokenRewarded)
		}

		k.subsFS.ModifyEntry(ctx, latestSub.Consumer, latestEntryBlock, &latestSub)
	} else if rewardsRemainder.IsPositive() {
		{
			// sub expired (no need to update credit), send rewards remainder to the validators
			pool := rewardstypes.ValidatorsRewardsDistributionPoolName
			err = k.bankKeeper.SendCoinsFromModuleToModule(ctx, types.ModuleName, string(pool), sdk.NewCoins(sdk.NewCoin(k.stakingKeeper.BondDenom(ctx), rewardsRemainder)))
			if err != nil {
				utils.LavaFormatError("failed sending remainder of rewards to the community pool", err,
					utils.Attribute{Key: "rewards_remainder", Value: rewardsRemainder.String()},
				)
			}
		}
	}
	utils.LogLavaEvent(ctx, k.Logger(ctx), types.RemainingCreditEventName, map[string]string{
		"sub":              sub,
		"credit_remaining": latestSub.Credit.String(),
		"block":            strconv.FormatInt(ctx.BlockHeight(), 10),
	}, "CU tracker reward and reset executed")
}

func (k Keeper) CalcTotalMonthlyReward(ctx sdk.Context, totalAmount math.Int, trackedCu uint64, totalCuUsedBySub uint64) math.Int {
	if totalCuUsedBySub == 0 {
		return math.ZeroInt()
	}
	totalMonthlyReward := totalAmount.MulRaw(int64(trackedCu)).QuoRaw(int64(totalCuUsedBySub))
	return totalMonthlyReward
}
