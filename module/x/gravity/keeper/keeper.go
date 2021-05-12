package keeper

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	sdk "github.com/cosmos/cosmos-sdk/types"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/cosmos/gravity-bridge/module/x/gravity/types"
)

// Keeper maintains the link to storage and exposes getter/setter methods for the various parts of the state machine
type Keeper struct {
	StakingKeeper types.StakingKeeper

	storeKey   sdk.StoreKey // Unexposed key to access store from sdk.Context
	paramSpace paramtypes.Subspace

	cdc            codec.BinaryMarshaler // The wire codec for binary encoding/decoding.
	bankKeeper     types.BankKeeper
	SlashingKeeper types.SlashingKeeper

	EthereumEventVoteRecordHandler interface {
		Handle(sdk.Context, types.EthereumEventVoteRecord, types.EthereumEvent) error
	}
}

// NewKeeper returns a new instance of the gravity keeper
func NewKeeper(cdc codec.BinaryMarshaler, storeKey sdk.StoreKey, paramSpace paramtypes.Subspace, stakingKeeper types.StakingKeeper, bankKeeper types.BankKeeper, slashingKeeper types.SlashingKeeper) Keeper {
	// set KeyTable if it has not already been set
	if !paramSpace.HasKeyTable() {
		paramSpace = paramSpace.WithKeyTable(types.ParamKeyTable())
	}

	k := Keeper{
		cdc:            cdc,
		paramSpace:     paramSpace,
		storeKey:       storeKey,
		StakingKeeper:  stakingKeeper,
		bankKeeper:     bankKeeper,
		SlashingKeeper: slashingKeeper,
	}
	k.EthereumEventVoteRecordHandler = EthereumEventVoteRecordHandler{
		keeper:     k,
		bankKeeper: bankKeeper,
	}

	return k
}

/////////////////////////////
//     VALSET REQUESTS     //
/////////////////////////////

// SetValsetRequest returns a new instance of the Gravity BridgeValidatorSet
// i.e. {"nonce": 1, "memebers": [{"eth_addr": "foo", "power": 11223}]}
func (k Keeper) SetValsetRequest(ctx sdk.Context) *types.Valset {
	valset := k.GetCurrentValset(ctx)
	k.StoreValset(ctx, valset)

	// Store the checkpoint as a legit past valset
	checkpoint := valset.GetCheckpoint(k.GetGravityID(ctx))
	k.SetPastEthSignatureCheckpoint(ctx, checkpoint)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeMultisigUpdateRequest,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
			sdk.NewAttribute(types.AttributeKeyContract, k.GetBridgeContractAddress(ctx)),
			sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.GetBridgeChainID(ctx)))),
			sdk.NewAttribute(types.AttributeKeyMultisigID, fmt.Sprint(valset.Nonce)),
			sdk.NewAttribute(types.AttributeKeyNonce, fmt.Sprint(valset.Nonce)),
		),
	)

	return valset
}

// StoreValset is for storing a valiator set at a given height
func (k Keeper) StoreValset(ctx sdk.Context, valset *types.Valset) {
	store := ctx.KVStore(k.storeKey)
	valset.Height = uint64(ctx.BlockHeight())
	store.Set(types.GetValsetKey(valset.Nonce), k.cdc.MustMarshalBinaryBare(valset))
	k.SetLatestValsetNonce(ctx, valset.Nonce)
}

//  SetLatestValsetNonce sets the latest valset nonce
func (k Keeper) SetLatestValsetNonce(ctx sdk.Context, nonce uint64) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.LatestValsetNonce, types.UInt64Bytes(nonce))
}

// StoreValsetUnsafe is for storing a valiator set at a given height
func (k Keeper) StoreValsetUnsafe(ctx sdk.Context, valset *types.Valset) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetValsetKey(valset.Nonce), k.cdc.MustMarshalBinaryBare(valset))
	k.SetLatestValsetNonce(ctx, valset.Nonce)
}

// HasValsetRequest returns true if a valset defined by a nonce exists
func (k Keeper) HasValsetRequest(ctx sdk.Context, nonce uint64) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetValsetKey(nonce))
}

// DeleteValset deletes the valset at a given nonce from state
func (k Keeper) DeleteValset(ctx sdk.Context, nonce uint64) {
	ctx.KVStore(k.storeKey).Delete(types.GetValsetKey(nonce))
}

// GetLatestValsetNonce returns the latest valset nonce
func (k Keeper) GetLatestValsetNonce(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bytes := store.Get(types.LatestValsetNonce)

	if len(bytes) == 0 {
		return 0
	}
	return types.UInt64FromBytes(bytes)
}

// GetValset returns a valset by nonce
func (k Keeper) GetValset(ctx sdk.Context, nonce uint64) *types.Valset {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.GetValsetKey(nonce))
	if bz == nil {
		return nil
	}
	var valset types.Valset
	k.cdc.MustUnmarshalBinaryBare(bz, &valset)
	return &valset
}

// IterateValsets retruns all valsetRequests
func (k Keeper) IterateValsets(ctx sdk.Context, cb func(key []byte, val *types.Valset) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.ValsetRequestKey)
	iter := prefixStore.ReverseIterator(nil, nil)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var valset types.Valset
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &valset)
		// cb returns true to stop early
		if cb(iter.Key(), &valset) {
			break
		}
	}
}

// GetValsets returns all the validator sets in state
func (k Keeper) GetValsets(ctx sdk.Context) (out []*types.Valset) {
	k.IterateValsets(ctx, func(_ []byte, val *types.Valset) bool {
		out = append(out, val)
		return false
	})
	sort.Sort(types.Valsets(out))
	return
}

// GetLatestValset returns the latest validator set in state
func (k Keeper) GetLatestValset(ctx sdk.Context) (out *types.Valset) {
	latestValsetNonce := k.GetLatestValsetNonce(ctx)
	out = k.GetValset(ctx, latestValsetNonce)
	return
}

// setLastSlashedValsetNonce sets the latest slashed valset nonce
func (k Keeper) SetLastSlashedValsetNonce(ctx sdk.Context, nonce uint64) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.LastSlashedValsetNonce, types.UInt64Bytes(nonce))
}

// GetLastSlashedValsetNonce returns the latest slashed valset nonce
func (k Keeper) GetLastSlashedValsetNonce(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bytes := store.Get(types.LastSlashedValsetNonce)

	if len(bytes) == 0 {
		return 0
	}
	return types.UInt64FromBytes(bytes)
}

// SetLastUnBondingBlockHeight sets the last unbonding block height
func (k Keeper) SetLastUnBondingBlockHeight(ctx sdk.Context, unbondingBlockHeight uint64) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.LastUnBondingBlockHeight, types.UInt64Bytes(unbondingBlockHeight))
}

// GetLastUnBondingBlockHeight returns the last unbonding block height
func (k Keeper) GetLastUnBondingBlockHeight(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bytes := store.Get(types.LastUnBondingBlockHeight)

	if len(bytes) == 0 {
		return 0
	}
	return types.UInt64FromBytes(bytes)
}

// GetUnSlashedValsets returns all the unslashed validator sets in state
func (k Keeper) GetUnSlashedValsets(ctx sdk.Context, maxHeight uint64) (out []*types.Valset) {
	lastSlashedValsetNonce := k.GetLastSlashedValsetNonce(ctx)
	k.IterateValsetBySlashedValsetNonce(ctx, lastSlashedValsetNonce, maxHeight, func(_ []byte, valset *types.Valset) bool {
		if valset.Nonce > lastSlashedValsetNonce {
			out = append(out, valset)
		}
		return false
	})
	return
}

// IterateValsetBySlashedValsetNonce iterates through all valset by last slashed valset nonce in ASC order
func (k Keeper) IterateValsetBySlashedValsetNonce(ctx sdk.Context, lastSlashedValsetNonce uint64, maxHeight uint64, cb func([]byte, *types.Valset) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.ValsetRequestKey)
	iter := prefixStore.Iterator(types.UInt64Bytes(lastSlashedValsetNonce), types.UInt64Bytes(maxHeight))
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		var valset types.Valset
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &valset)
		// cb returns true to stop early
		if cb(iter.Key(), &valset) {
			break
		}
	}
}

/////////////////////////////
//     VALSET CONFIRMS     //
/////////////////////////////

// GetSignerSetTxSignature returns a valset confirmation by a nonce and validator address
func (k Keeper) GetSignerSetTxSignature(ctx sdk.Context, nonce uint64, validator sdk.AccAddress) *types.MsgSignerSetTxSignature {
	store := ctx.KVStore(k.storeKey)
	entity := store.Get(types.GetSignerSetTxSignatureKey(nonce, validator))
	if entity == nil {
		return nil
	}
	confirm := types.MsgSignerSetTxSignature{}
	k.cdc.MustUnmarshalBinaryBare(entity, &confirm)
	return &confirm
}

// SetSignerSetTxSignature sets a valset confirmation
func (k Keeper) SetSignerSetTxSignature(ctx sdk.Context, valsetConf types.MsgSignerSetTxSignature) []byte {
	store := ctx.KVStore(k.storeKey)
	addr, err := sdk.AccAddressFromBech32(valsetConf.Orchestrator)
	if err != nil {
		panic(err)
	}
	key := types.GetSignerSetTxSignatureKey(valsetConf.Nonce, addr)
	store.Set(key, k.cdc.MustMarshalBinaryBare(&valsetConf))
	return key
}

// GetSignerSetTxSignatures returns all validator set confirmations by nonce
func (k Keeper) GetSignerSetTxSignatures(ctx sdk.Context, nonce uint64) (confirms []*types.MsgSignerSetTxSignature) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.SignerSetTxSignatureKey)
	start, end := prefixRange(types.UInt64Bytes(nonce))
	iterator := prefixStore.Iterator(start, end)

	defer iterator.Close()

	for ; iterator.Valid(); iterator.Next() {
		confirm := types.MsgSignerSetTxSignature{}
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &confirm)
		confirms = append(confirms, &confirm)
	}

	return confirms
}

// IterateSignerSetTxSignatureByNonce iterates through all valset confirms by nonce in ASC order
// MARK finish-batches: this is where the key is iterated in the old (presumed working) code
// TODO: specify which nonce this is
func (k Keeper) IterateSignerSetTxSignatureByNonce(ctx sdk.Context, nonce uint64, cb func([]byte, types.MsgSignerSetTxSignature) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.SignerSetTxSignatureKey)
	iter := prefixStore.Iterator(prefixRange(types.UInt64Bytes(nonce)))
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		confirm := types.MsgSignerSetTxSignature{}
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &confirm)
		// cb returns true to stop early
		if cb(iter.Key(), confirm) {
			break
		}
	}
}

/////////////////////////////
//      BATCH CONFIRMS     //
/////////////////////////////

// GetBatchConfirm returns a batch confirmation given its nonce, the token contract, and a validator address
func (k Keeper) GetBatchConfirm(ctx sdk.Context, nonce uint64, tokenContract string, validator sdk.AccAddress) *types.MsgBatchTxSignature {
	store := ctx.KVStore(k.storeKey)
	entity := store.Get(types.GetBatchConfirmKey(tokenContract, nonce, validator))
	if entity == nil {
		return nil
	}
	confirm := types.MsgBatchTxSignature{}
	k.cdc.MustUnmarshalBinaryBare(entity, &confirm)
	return &confirm
}

// SetBatchConfirm sets a batch confirmation by a validator
func (k Keeper) SetBatchConfirm(ctx sdk.Context, batch *types.MsgBatchTxSignature) []byte {
	store := ctx.KVStore(k.storeKey)
	acc, err := sdk.AccAddressFromBech32(batch.Orchestrator)
	if err != nil {
		panic(err)
	}
	key := types.GetBatchConfirmKey(batch.TokenContract, batch.Nonce, acc)
	store.Set(key, k.cdc.MustMarshalBinaryBare(batch))
	return key
}

// IterateBatchConfirmByNonceAndTokenContract iterates through all batch confirmations
// MARK finish-batches: this is where the key is iterated in the old (presumed working) code
// TODO: specify which nonce this is
func (k Keeper) IterateBatchConfirmByNonceAndTokenContract(ctx sdk.Context, nonce uint64, tokenContract string, cb func([]byte, types.MsgBatchTxSignature) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.BatchConfirmKey)
	prefix := append([]byte(tokenContract), types.UInt64Bytes(nonce)...)
	iter := prefixStore.Iterator(prefixRange(prefix))
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		confirm := types.MsgBatchTxSignature{}
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &confirm)
		// cb returns true to stop early
		if cb(iter.Key(), confirm) {
			break
		}
	}
}

// GetBatchConfirmByNonceAndTokenContract returns the batch confirms
func (k Keeper) GetBatchConfirmByNonceAndTokenContract(ctx sdk.Context, nonce uint64, tokenContract string) (out []types.MsgBatchTxSignature) {
	k.IterateBatchConfirmByNonceAndTokenContract(ctx, nonce, tokenContract, func(_ []byte, msg types.MsgBatchTxSignature) bool {
		out = append(out, msg)
		return false
	})
	return
}

/////////////////////////////
//    ADDRESS DELEGATION   //
/////////////////////////////

// SetOrchestratorValidator sets the Orchestrator key for a given validator
func (k Keeper) SetOrchestratorValidator(ctx sdk.Context, val sdk.ValAddress, orch sdk.AccAddress) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetOrchestratorAddressKey(orch), val.Bytes())
}

// GetOrchestratorValidator returns the validator key associated with an orchestrator key
func (k Keeper) GetOrchestratorValidator(ctx sdk.Context, orch sdk.AccAddress) sdk.ValAddress {
	store := ctx.KVStore(k.storeKey)
	return sdk.ValAddress(store.Get(types.GetOrchestratorAddressKey(orch)))
}

/////////////////////////////
//       ETH ADDRESS       //
/////////////////////////////

// SetEthAddress sets the ethereum address for a given validator
func (k Keeper) SetEthAddressForValidator(ctx sdk.Context, validator sdk.ValAddress, ethAddr string) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetEthAddressByValidatorKey(validator), []byte(ethAddr))
	store.Set(types.GetValidatorByEthAddressKey(ethAddr), []byte(validator))
}

// GetEthAddressByValidator returns the eth address for a given gravity validator
func (k Keeper) GetEthAddressByValidator(ctx sdk.Context, validator sdk.ValAddress) string {
	store := ctx.KVStore(k.storeKey)
	return string(store.Get(types.GetEthAddressByValidatorKey(validator)))
}

// GetValidatorByEthAddress returns the validator for a given eth address
func (k Keeper) GetValidatorByEthAddress(ctx sdk.Context, ethAddr string) (validator stakingtypes.Validator, found bool) {
	store := ctx.KVStore(k.storeKey)
	valAddr := store.Get(types.GetValidatorByEthAddressKey(ethAddr))
	if valAddr == nil {
		return stakingtypes.Validator{}, false
	}
	return k.StakingKeeper.GetValidator(ctx, valAddr)
}

// GetCurrentValset gets powers from the store and normalizes them
// into an integer percentage with a resolution of uint32 Max meaning
// a given validators 'gravity power' is computed as
// Cosmos power / total cosmos power = x / uint32 Max
// where x is the voting power on the gravity contract. This allows us
// to only use integer division which produces a known rounding error
// from truncation equal to the ratio of the validators
// Cosmos power / total cosmos power ratio, leaving us at uint32 Max - 1
// total voting power. This is an acceptable rounding error since floating
// point may cause consensus problems if different floating point unit
// implementations are involved.
func (k Keeper) GetCurrentValset(ctx sdk.Context) *types.Valset {
	validators := k.StakingKeeper.GetBondedValidatorsByPower(ctx)
	bridgeValidators := make([]*types.BridgeValidator, len(validators))
	var totalPower uint64
	// TODO someone with in depth info on Cosmos staking should determine
	// if this is doing what I think it's doing
	for i, validator := range validators {
		val := validator.GetOperator()

		p := uint64(k.StakingKeeper.GetLastValidatorPower(ctx, val))
		totalPower += p

		bridgeValidators[i] = &types.BridgeValidator{Power: p}
		if ethAddr := k.GetEthAddressByValidator(ctx, val); ethAddr != "" {
			bridgeValidators[i].EthereumAddress = ethAddr
		}
	}
	// normalize power values
	for i := range bridgeValidators {
		bridgeValidators[i].Power = sdk.NewUint(bridgeValidators[i].Power).MulUint64(math.MaxUint32).QuoUint64(totalPower).Uint64()
	}

	// TODO: make the nonce an incrementing one (i.e. fetch last nonce from state, increment, set here)
	return types.NewValset(uint64(ctx.BlockHeight()), uint64(ctx.BlockHeight()), bridgeValidators)
}

/////////////////////////////
//       LOGICCALLS        //
/////////////////////////////

// GetContractCallTx gets an outgoing logic call
func (k Keeper) GetContractCallTx(ctx sdk.Context, invalidationID []byte, invalidationNonce uint64) *types.ContractCallTx {
	store := ctx.KVStore(k.storeKey)
	call := types.ContractCallTx{}
	k.cdc.MustUnmarshalBinaryBare(store.Get(types.GetContractCallTxKey(invalidationID, invalidationNonce)), &call)
	return &call
}

// SetOutogingLogicCall sets an outgoing logic call
func (k Keeper) SetContractCallTx(ctx sdk.Context, call *types.ContractCallTx) {
	store := ctx.KVStore(k.storeKey)

	// Store checkpoint to prove that this logic call actually happened
	checkpoint := call.GetCheckpoint(k.GetGravityID(ctx))
	k.SetPastEthSignatureCheckpoint(ctx, checkpoint)

	store.Set(types.GetContractCallTxKey(call.InvalidationId, call.InvalidationNonce),
		k.cdc.MustMarshalBinaryBare(call))
}

// DeleteContractCallTx deletes outgoing logic calls
func (k Keeper) DeleteContractCallTx(ctx sdk.Context, invalidationID []byte, invalidationNonce uint64) {
	ctx.KVStore(k.storeKey).Delete(types.GetContractCallTxKey(invalidationID, invalidationNonce))
}

// IterateContractCallTxs iterates over outgoing logic calls
func (k Keeper) IterateContractCallTxs(ctx sdk.Context, cb func([]byte, *types.ContractCallTx) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyContractCallTx)
	iter := prefixStore.Iterator(nil, nil)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var call types.ContractCallTx
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &call)
		// cb returns true to stop early
		if cb(iter.Key(), &call) {
			break
		}
	}
}

// GetContractCallTxs returns the outgoing tx batches
func (k Keeper) GetContractCallTxs(ctx sdk.Context) (out []*types.ContractCallTx) {
	k.IterateContractCallTxs(ctx, func(_ []byte, call *types.ContractCallTx) bool {
		out = append(out, call)
		return false
	})
	return
}

// CancelContractCallTxs releases all TX in the batch and deletes the batch
func (k Keeper) CancelContractCallTx(ctx sdk.Context, invalidationId []byte, invalidationNonce uint64) error {
	call := k.GetContractCallTx(ctx, invalidationId, invalidationNonce)
	if call == nil {
		return types.ErrUnknown
	}
	// Delete batch since it is finished
	k.DeleteContractCallTx(ctx, call.InvalidationId, call.InvalidationNonce)

	// a consuming application will have to watch for this event and act on it
	batchEvent := sdk.NewEvent(
		types.EventTypeContractCallTxCanceled,
		sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
		sdk.NewAttribute(types.AttributeKeyInvalidationID, fmt.Sprint(call.InvalidationId)),
		sdk.NewAttribute(types.AttributeKeyInvalidationNonce, fmt.Sprint(call.InvalidationNonce)),
	)
	ctx.EventManager().EmitEvent(batchEvent)
	return nil
}

/////////////////////////////
//       LOGICCONFIRMS     //
/////////////////////////////

// SetLogicCallConfirm sets a logic confirm in the store
func (k Keeper) SetLogicCallConfirm(ctx sdk.Context, msg *types.MsgContractCallTxSignature) {
	bytes, err := hex.DecodeString(msg.InvalidationId)
	if err != nil {
		panic(err)
	}

	acc, err := sdk.AccAddressFromBech32(msg.Orchestrator)
	if err != nil {
		panic(err)
	}

	ctx.KVStore(k.storeKey).
		Set(types.GetLogicConfirmKey(bytes, msg.InvalidationNonce, acc), k.cdc.MustMarshalBinaryBare(msg))
}

// GetLogicCallConfirm gets a logic confirm from the store
func (k Keeper) GetLogicCallConfirm(ctx sdk.Context, invalidationId []byte, invalidationNonce uint64, val sdk.AccAddress) *types.MsgContractCallTxSignature {
	store := ctx.KVStore(k.storeKey)
	data := store.Get(types.GetLogicConfirmKey(invalidationId, invalidationNonce, val))
	if data == nil {
		return nil
	}
	out := types.MsgContractCallTxSignature{}
	k.cdc.MustUnmarshalBinaryBare(data, &out)
	return &out
}

// DeleteLogicCallConfirm deletes a logic confirm from the store
func (k Keeper) DeleteLogicCallConfirm(
	ctx sdk.Context,
	invalidationID []byte,
	invalidationNonce uint64,
	val sdk.AccAddress) {
	ctx.KVStore(k.storeKey).Delete(types.GetLogicConfirmKey(invalidationID, invalidationNonce, val))
}

// IterateLogicConfirmByInvalidationIDAndNonce iterates over all logic confirms stored by nonce
func (k Keeper) IterateLogicConfirmByInvalidationIDAndNonce(
	ctx sdk.Context,
	invalidationID []byte,
	invalidationNonce uint64,
	cb func([]byte, *types.MsgContractCallTxSignature) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.KeyOutgoingLogicConfirm)
	iter := prefixStore.Iterator(prefixRange(append(invalidationID, types.UInt64Bytes(invalidationNonce)...)))
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		confirm := types.MsgContractCallTxSignature{}
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &confirm)
		// cb returns true to stop early
		if cb(iter.Key(), &confirm) {
			break
		}
	}
}

// GetLogicConfirmsByInvalidationIdAndNonce returns the logic call confirms
func (k Keeper) GetLogicConfirmByInvalidationIDAndNonce(ctx sdk.Context, invalidationId []byte, invalidationNonce uint64) (out []types.MsgContractCallTxSignature) {
	k.IterateLogicConfirmByInvalidationIDAndNonce(ctx, invalidationId, invalidationNonce, func(_ []byte, msg *types.MsgContractCallTxSignature) bool {
		out = append(out, *msg)
		return false
	})
	return
}

/////////////////////////////
//       PARAMETERS        //
/////////////////////////////

// GetParams returns the parameters from the store
func (k Keeper) GetParams(ctx sdk.Context) (params types.Params) {
	k.paramSpace.GetParamSet(ctx, &params)
	return
}

// SetParams sets the parameters in the store
func (k Keeper) SetParams(ctx sdk.Context, ps types.Params) {
	k.paramSpace.SetParamSet(ctx, &ps)
}

// GetBridgeContractAddress returns the bridge contract address on ETH
func (k Keeper) GetBridgeContractAddress(ctx sdk.Context) string {
	var a string
	k.paramSpace.Get(ctx, types.ParamsStoreKeyBridgeContractAddress, &a)
	return a
}

// GetBridgeChainID returns the chain id of the ETH chain we are running against
func (k Keeper) GetBridgeChainID(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamsStoreKeyBridgeContractChainID, &a)
	return a
}

// GetGravityID returns the GravityID the GravityID is essentially a salt value
// for bridge signatures, provided each chain running Gravity has a unique ID
// it won't be possible to play back signatures from one bridge onto another
// even if they share a validator set.
//
// The lifecycle of the GravityID is that it is set in the Genesis file
// read from the live chain for the contract deployment, once a Gravity contract
// is deployed the GravityID CAN NOT BE CHANGED. Meaning that it can't just be the
// same as the chain id since the chain id may be changed many times with each
// successive chain in charge of the same bridge
func (k Keeper) GetGravityID(ctx sdk.Context) string {
	var a string
	k.paramSpace.Get(ctx, types.ParamsStoreKeyGravityID, &a)
	return a
}

// Set GravityID sets the GravityID the GravityID is essentially a salt value
// for bridge signatures, provided each chain running Gravity has a unique ID
// it won't be possible to play back signatures from one bridge onto another
// even if they share a validator set.
//
// The lifecycle of the GravityID is that it is set in the Genesis file
// read from the live chain for the contract deployment, once a Gravity contract
// is deployed the GravityID CAN NOT BE CHANGED. Meaning that it can't just be the
// same as the chain id since the chain id may be changed many times with each
// successive chain in charge of the same bridge
func (k Keeper) SetGravityID(ctx sdk.Context, v string) {
	k.paramSpace.Set(ctx, types.ParamsStoreKeyGravityID, v)
}

// logger returns a module-specific logger.
func (k Keeper) logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

func (k Keeper) UnpackEthereumEventVoteRecordClaim(att *types.EthereumEventVoteRecord) (types.EthereumEvent, error) {
	var msg types.EthereumEvent
	err := k.cdc.UnpackAny(att.Claim, &msg)
	if err != nil {
		return nil, err
	} else {
		return msg, nil
	}
}

// GetDelegateKeys iterates both the EthAddress and Orchestrator address indexes to produce
// a vector of MsgDelegateKeys entires containing all the delgate keys for state
// export / import. This may seem at first glance to be excessively complicated, why not combine
// the EthAddress and Orchestrator address indexes and simply iterate one thing? The answer is that
// even though we set the Eth and Orchestrator address in the same place we use them differently we
// always go from Orchestrator address to Validator address and from validator address to Ethereum address
// we want to keep looking up the validator address for various reasons, so a direct Orchestrator to Ethereum
// address mapping will mean having to keep two of the same data around just to provide lookups.
//
// For the time being this will serve
func (k Keeper) GetDelegateKeys(ctx sdk.Context) []*types.MsgDelegateKeys {
	store := ctx.KVStore(k.storeKey)
	prefix := []byte(types.EthAddressByValidatorKey)
	iter := store.Iterator(prefixRange(prefix))
	defer iter.Close()

	ethAddresses := make(map[string]string)

	for ; iter.Valid(); iter.Next() {
		// the 'key' contains both the prefix and the value, so we need
		// to cut off the starting bytes, if you don't do this a valid
		// cosmos key will be made out of EthAddressByValidatorKey + the startin bytes
		// of the actual key
		key := iter.Key()[len(types.EthAddressByValidatorKey):]
		value := iter.Value()
		ethAddress := string(value)
		valAddress := sdk.ValAddress(key)
		ethAddresses[valAddress.String()] = ethAddress
	}

	store = ctx.KVStore(k.storeKey)
	prefix = []byte(types.KeyOrchestratorAddress)
	iter = store.Iterator(prefixRange(prefix))
	defer iter.Close()

	orchAddresses := make(map[string]string)

	for ; iter.Valid(); iter.Next() {
		key := iter.Key()[len(types.KeyOrchestratorAddress):]
		value := iter.Value()
		orchAddress := sdk.AccAddress(key).String()
		valAddress := sdk.ValAddress(value)
		orchAddresses[valAddress.String()] = orchAddress
	}

	var result []*types.MsgDelegateKeys

	for valAddr, ethAddr := range ethAddresses {
		orch, ok := orchAddresses[valAddr]
		if !ok {
			// this should never happen unless the store
			// is somehow inconsistent
			panic("Can't find address")
		}
		result = append(result, &types.MsgDelegateKeys{
			Orchestrator: orch,
			Validator:    valAddr,
			EthAddress:   ethAddr,
		})

	}

	// we iterated over a map, so now we have to sort to ensure the
	// output here is deterministic, eth address chosen for no particular
	// reason
	sort.Slice(result[:], func(i, j int) bool {
		return result[i].EthAddress < result[j].EthAddress
	})

	return result
}

// GetUnbondingvalidators returns UnbondingValidators.
// Adding here in gravity keeper as cdc is available inside endblocker.
func (k Keeper) GetUnbondingvalidators(unbondingVals []byte) stakingtypes.ValAddresses {
	unbondingValidators := stakingtypes.ValAddresses{}
	k.cdc.MustUnmarshalBinaryBare(unbondingVals, &unbondingValidators)
	return unbondingValidators
}

// prefixRange turns a prefix into a (start, end) range. The start is the given prefix value and
// the end is calculated by adding 1 bit to the start value. Nil is not allowed as prefix.
// 		Example: []byte{1, 3, 4} becomes []byte{1, 3, 5}
// 				 []byte{15, 42, 255, 255} becomes []byte{15, 43, 0, 0}
//
// In case of an overflow the end is set to nil.
//		Example: []byte{255, 255, 255, 255} becomes nil
// MARK finish-batches: this is where some crazy shit happens
func prefixRange(prefix []byte) ([]byte, []byte) {
	if prefix == nil {
		panic("nil key not allowed")
	}
	// special case: no prefix is whole range
	if len(prefix) == 0 {
		return nil, nil
	}

	// copy the prefix and update last byte
	end := make([]byte, len(prefix))
	copy(end, prefix)
	l := len(end) - 1
	end[l]++

	// wait, what if that overflowed?....
	for end[l] == 0 && l > 0 {
		l--
		end[l]++
	}

	// okay, funny guy, you gave us FFF, no end to this range...
	if l == 0 && end[0] == 0 {
		end = nil
	}
	return prefix, end
}
