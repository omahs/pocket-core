package keeper

import (
	"encoding/hex"
	"encoding/json"
	pc "github.com/pokt-network/pocket-core/x/pocketcore/types"
	"github.com/pokt-network/posmint/crypto/keys"
	sdk "github.com/pokt-network/posmint/types"
	"github.com/pokt-network/posmint/x/auth"
	"github.com/pokt-network/posmint/x/auth/util"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/rpc/client"
	"math"
	"strconv"
)

func BeginBlocker(ctx sdk.Context, _ abci.RequestBeginBlock, k Keeper) {
	// delete the proofs held within the world state for too long
	k.DeleteExpiredClaims(ctx)
}

// validate the zero knowledge range proof using the proof message and the claim message
func (k Keeper) ValidateProof(ctx sdk.Context, claimMsg pc.MsgClaim, proofMsg pc.MsgProof) error {
	// generate the needed pseudorandom claimMsg index
	reqProof := k.GetPseudorandomIndex(ctx, claimMsg.TotalRelays, claimMsg.SessionHeader)
	// if the required claimMsg index does not match the proofMsg leafNode index
	if reqProof != int64(proofMsg.MerkleProofs[0].Index) {
		return pc.NewInvalidProofsError(pc.ModuleName)
	}
	// validate level count on claimMsg by total relays
	if len(proofMsg.MerkleProofs[0].HashSums) != int(math.Ceil(math.Log2(float64(claimMsg.TotalRelays)))) {
		return pc.NewInvalidProofsError(pc.ModuleName)
	}
	// do a merkle claimMsg using the merkle claimMsg, the previously submitted root, and the leafNode to ensure validity of the proofMsg
	if !proofMsg.MerkleProofs.Validate(claimMsg.MerkleRoot, proofMsg.Leaf, proofMsg.Cousin, claimMsg.TotalRelays) {
		return pc.NewInvalidMerkleVerifyError(pc.ModuleName)
	}
	// check the validity of the token
	if err := proofMsg.Leaf.Token.Validate(); err != nil {
		return err
	}
	// verify the client signature
	if err := pc.SignatureVerification(proofMsg.Leaf.Token.ClientPublicKey, proofMsg.Leaf.HashString(), proofMsg.Leaf.Signature); err != nil {
		return err
	}
	return nil
}

// generates the required pseudorandom index for the zero knowledge proof
func (k Keeper) GetPseudorandomIndex(ctx sdk.Context, totalRelays int64, header pc.SessionHeader) int64 {
	// get the context for the proof (the proof context is X sessions after the session began)
	proofContext := ctx.WithBlockHeight(header.SessionBlockHeight + k.ProofWaitingPeriod(ctx)*k.SessionFrequency(ctx)) // next session block hash
	// get the pseudorandomGenerator json bytes
	proofBlockHeader := proofContext.BlockHeader()
	r, err := json.Marshal(pseudorandomGenerator{
		blockHash: hex.EncodeToString(proofBlockHeader.GetLastBlockId().Hash), // block hash
		header:    header.HashString(),                                        // header hashstring
	})
	if err != nil {
		panic(err)
	}
	// hash the bytes and take the first 15 characters of the string
	proofsHash := hex.EncodeToString(pc.Hash(r))[:15]
	var maxValue int64
	// for each hex character of the hash
	for i := 15; i > 0; i-- {
		// parse the integer from this point of the hex string onward
		maxValue, err = strconv.ParseInt(string(proofsHash[:i]), 16, 64)
		if err != nil {
			panic(err)

		}
		// if the total relays is greater than the resulting integer, this is the pseudorandom chosen proof
		if totalRelays >= maxValue {
			firstCharacter, err := strconv.ParseInt(string(proofsHash[0]), 16, 64)
			if err != nil {
				panic(err)
			}
			selection := firstCharacter%int64(i) + 1
			// parse the integer from this point of the hex string onward
			index, err := strconv.ParseInt(proofsHash[:selection], 16, 64)
			if err != nil {
				panic(err)
			}
			return index
		}
	}
	return 0
}

// struct used for creating the psuedorandom index
type pseudorandomGenerator struct {
	blockHash string
	header    string
}

// auto sends a claim of work based on relays completed
func (k Keeper) SendClaimTx(ctx sdk.Context, n client.Client, keybase keys.Keybase, claimTx func(keybase keys.Keybase, cliCtx util.CLIContext, txBuilder auth.TxBuilder, header pc.SessionHeader, totalRelays int64, root pc.HashSum) (*sdk.TxResponse, error)) {
	kp, err := keybase.GetCoinbase()
	if err != nil {
		panic(err)
	}
	// get all the invoices held in memory
	invoices := pc.GetAllInvoices()
	// for every invoice in Invoices
	for _, invoice := range (*invoices).M {
		// if the blockchain in the invoice is not supported then delete it because nodes don't get paid for unsupported blockchains
		if !k.IsPocketSupportedBlockchain(ctx.WithBlockHeight(invoice.SessionHeader.SessionBlockHeight), invoice.SessionHeader.Chain) && invoice.TotalRelays > 0 {
			invoices.DeleteInvoice(invoice.SessionHeader)
			continue
		}
		// check the current state to see if the unverified invoice has already been sent and processed (if so, then skip this invoice)
		if _, found := k.GetClaim(ctx, sdk.Address(kp.GetAddress()), invoice.SessionHeader); found {
			continue
		}
		// generate the auto txbuilder and clictx
		txBuilder, cliCtx := newTxBuilderAndCliCtx(ctx, n, keybase, k)
		// generate the merkle root for this invoice
		root := invoice.GenerateMerkleRoot()
		// send in the invoice header, the total relays completed, and the merkle root (ensures data integrity)
		if _, err := claimTx(keybase, cliCtx, txBuilder, invoice.SessionHeader, invoice.TotalRelays, root); err != nil {
			panic(err)
		}
	}
}

// auto sends a proof transaction for the claim
func (k Keeper) SendProofTx(ctx sdk.Context, n client.Client, keybase keys.Keybase, claimTx func(cliCtx util.CLIContext, txBuilder auth.TxBuilder, branches [2]pc.MerkleProof, leafNode, cousin pc.RelayProof) (*sdk.TxResponse, error)) {
	kp, err := keybase.GetCoinbase()
	if err != nil {
		panic(err)
	}
	// get the self address
	addr := sdk.Address(kp.GetAddress())
	// get all mature (waiting period has passed) proofs for your address
	proofs := k.GetMatureClaims(ctx, addr)
	// for every proof of the mature set
	for _, proof := range proofs {
		// if the proof is found to be verified in the world state, you can delete it from the cache and not send again
		if _, found := k.GetInvoice(ctx, addr, proof.SessionHeader); found {
			// remove from the local cache
			pc.GetAllInvoices().DeleteInvoice(proof.SessionHeader)
			// remove from the temporary world state
			k.DeleteClaim(ctx, addr, proof.SessionHeader)
			continue
		}
		// generate the auto txbuilder and clictx
		txBuilder, cliCtx := newTxBuilderAndCliCtx(ctx, n, keybase, k)
		// generate the proof of relay object using the found proof and local cache
		inv := pc.Invoice{
			SessionHeader: proof.SessionHeader,
			TotalRelays:   proof.TotalRelays,
			Proofs:        pc.GetAllInvoices().GetProofs(proof.SessionHeader),
		}
		// generate the needed pseudorandom proof using the information found in the first transaction
		reqProof := int(k.GetPseudorandomIndex(ctx, proof.TotalRelays, proof.SessionHeader))
		// get the merkle proof object for the pseudorandom proof index
		branch, cousinIndex := inv.GenerateMerkleProof(reqProof)
		// get the leaf for the required pseudorandom proof index
		leaf := pc.GetAllInvoices().GetProof(proof.SessionHeader, reqProof)
		cousin := pc.GetAllInvoices().GetProof(proof.SessionHeader, cousinIndex)
		// send the claim TX
		_, err := claimTx(cliCtx, txBuilder, branch, leaf, cousin)
		if err != nil {
			panic(err)
		}
	}
}

// stored invoices (already proved)

// set the verified invoice
func (k Keeper) SetInvoice(ctx sdk.Context, address sdk.Address, p pc.StoredInvoice) {
	store := ctx.KVStore(k.storeKey)
	bz := k.cdc.MustMarshalBinaryBare(p)
	store.Set(pc.KeyForInvoice(ctx, address, p.SessionHeader), bz)
}

// retrieve the verified invoice
func (k Keeper) GetInvoice(ctx sdk.Context, address sdk.Address, header pc.SessionHeader) (invoice pc.StoredInvoice, found bool) {
	store := ctx.KVStore(k.storeKey)
	res := store.Get(pc.KeyForInvoice(ctx, address, header))
	if res == nil {
		return pc.StoredInvoice{}, false
	}
	k.cdc.MustUnmarshalBinaryBare(res, &invoice)
	return invoice, true
}

func (k Keeper) SetInvoices(ctx sdk.Context, invoices []pc.StoredInvoice) {
	store := ctx.KVStore(k.storeKey)
	for _, invoice := range invoices {
		addrbz, err := hex.DecodeString(invoice.ServicerAddress)
		if err != nil {
			panic(err)
		}
		bz := k.cdc.MustMarshalBinaryBare(invoice)
		store.Set(pc.KeyForInvoice(ctx, addrbz, invoice.SessionHeader), bz)
	}
}

// get all verified invoices for this address
func (k Keeper) GetInvoices(ctx sdk.Context, address sdk.Address) (invoices []pc.StoredInvoice) {
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.KeyForInvoices(address))
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		var summary pc.StoredInvoice
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &summary)
		invoices = append(invoices, summary)
	}
	return
}

// get all invoices for this address
func (k Keeper) GetAllInvoices(ctx sdk.Context) (invoices []pc.StoredInvoice) {
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.InvoiceKey)
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		var summary pc.StoredInvoice
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &summary)
		invoices = append(invoices, summary)
	}
	return
}

// claims ----
func (k Keeper) SetClaim(ctx sdk.Context, msg pc.MsgClaim) {
	store := ctx.KVStore(k.storeKey)
	bz := k.cdc.MustMarshalBinaryBare(msg)
	store.Set(pc.KeyForClaim(ctx, msg.FromAddress, msg.SessionHeader), bz)
}
func (k Keeper) GetClaim(ctx sdk.Context, address sdk.Address, header pc.SessionHeader) (msg pc.MsgClaim, found bool) {
	store := ctx.KVStore(k.storeKey)
	res := store.Get(pc.KeyForClaim(ctx, address, header))
	if res == nil {
		return pc.MsgClaim{}, false
	}
	k.cdc.MustUnmarshalBinaryBare(res, &msg)
	return msg, true
}

func (k Keeper) SetClaims(ctx sdk.Context, claims []pc.MsgClaim) {
	store := ctx.KVStore(k.storeKey)
	for _, msg := range claims {
		bz := k.cdc.MustMarshalBinaryBare(msg)
		store.Set(pc.KeyForClaim(ctx, msg.FromAddress, msg.SessionHeader), bz)
	}
}

func (k Keeper) GetClaims(ctx sdk.Context, address sdk.Address) (proofs []pc.MsgClaim) {
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.KeyForClaims(address))
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		var summary pc.MsgClaim
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &summary)
		proofs = append(proofs, summary)
	}
	return
}

func (k Keeper) GetAllClaims(ctx sdk.Context) (proofs []pc.MsgClaim) {
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.ClaimKey)
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		var summary pc.MsgClaim
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &summary)
		proofs = append(proofs, summary)
	}
	return
}

func (k Keeper) DeleteClaim(ctx sdk.Context, address sdk.Address, header pc.SessionHeader) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(pc.KeyForClaim(ctx, address, header))
}

// get the mature unverified proofs for this address
func (k Keeper) GetMatureClaims(ctx sdk.Context, address sdk.Address) (matureProofs []pc.MsgClaim) {
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.KeyForClaims(address))
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		var msg pc.MsgClaim
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &msg)
		if k.ClaimIsMature(ctx, msg.SessionBlockHeight) {
			matureProofs = append(matureProofs, msg)
		}
	}
	return
}

// delete expired
func (k Keeper) DeleteExpiredClaims(ctx sdk.Context) {
	var msg = pc.MsgClaim{}
	store := ctx.KVStore(k.storeKey)
	iterator := sdk.KVStorePrefixIterator(store, pc.ClaimKey)
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		k.cdc.MustUnmarshalBinaryBare(iterator.Value(), &msg)
		sessionContext := ctx.WithBlockHeight(msg.SessionBlockHeight)
		// if more sessions has passed than the expiration of unverified pseudorandomGenerator, delete from set
		if (ctx.BlockHeight()-msg.SessionBlockHeight)/k.SessionFrequency(sessionContext) >= k.ClaimExpiration(sessionContext) { // todo confirm these contexts should be now and not when submitted
			store.Delete(iterator.Key())
		}
	}
}

// is the proof mature? able to be claimed because the `waiting period` has passed since the sessionBlock
func (k Keeper) ClaimIsMature(ctx sdk.Context, sessionBlockHeight int64) bool {
	waitingPeriodInBlocks := k.ProofWaitingPeriod(ctx) * k.SessionFrequency(ctx)
	if ctx.BlockHeight() >= waitingPeriodInBlocks+sessionBlockHeight {
		return true
	}
	return false
}

// todo this auto tx needs to be fixed
func newTxBuilderAndCliCtx(ctx sdk.Context, n client.Client, keybase keys.Keybase, k Keeper) (txBuilder auth.TxBuilder, cliCtx util.CLIContext) {
	kp, err := keybase.GetCoinbase()
	if err != nil {
		panic(err)
	}
	genDoc, err := n.Genesis()
	if err != nil {
		panic(err)
	}
	pubKey := kp.PublicKey
	fromAddr := sdk.Address(pubKey.Bytes())
	cliCtx = util.NewCLIContext(n, fromAddr, k.coinbasePassphrase).WithCodec(k.cdc)
	cliCtx.BroadcastMode = util.BroadcastSync
	accGetter := auth.NewAccountRetriever(cliCtx)
	err = accGetter.EnsureExists(fromAddr)
	if err != nil {
		panic(err)
	}
	account, err := accGetter.GetAccount(fromAddr)
	if err != nil {
		panic(err)
	}
	txBuilder = auth.NewTxBuilder(
		auth.DefaultTxEncoder(k.cdc),
		account.GetAccountNumber(),
		account.GetSequence(),
		genDoc.Genesis.ChainID,
		"",
		sdk.NewCoins(sdk.NewCoin(k.posKeeper.StakeDenom(ctx), sdk.NewInt(10))),
	).WithKeybase(keybase)
	return
}
