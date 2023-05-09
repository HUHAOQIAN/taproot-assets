package itest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/taro/chanutils"
	unirpc "github.com/lightninglabs/taro/tarorpc/universerpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"
)

func testUniverseSync(t *harnessTest) {
	// First, we'll create out usual set of simple and also issuable
	// assets.
	rpcSimpleAssets := mintAssetsConfirmBatch(t, t.tarod, simpleAssets)
	rpcIssuableAssets := mintAssetsConfirmBatch(t, t.tarod, issuableAssets)

	// With those assets created, we'll now create a new node that we'll
	// use to exercise the Universe sync.
	bob := setupTarodHarness(
		t.t, t, t.lndHarness.Bob, nil,
	)
	defer func() {
		require.NoError(t.t, bob.stop(true))
	}()

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultWaitTimeout)
	defer cancel()

	// Before we start, we'll fetch the complete set of Universe roots from
	// our primary node.
	universeRoots, err := t.tarod.AssetRoots(
		ctxt, &unirpc.AssetRootRequest{},
	)
	require.NoError(t.t, err)

	// Now we have an initial benchmark, so we'll kick off the universe
	// sync with Bob syncing off the primary harness node that created the
	// assets.
	ctxt, cancel = context.WithTimeout(ctxb, defaultWaitTimeout)
	defer cancel()
	syncDiff, err := bob.SyncUniverse(ctxt, &unirpc.SyncRequest{
		UniverseHost: t.tarod.rpcHost(),
		SyncMode:     unirpc.UniverseSyncMode_SYNC_ISSUANCE_ONLY,
	})
	require.NoError(t.t, err)

	// Bob's universe diff should contain an entry for each of the assets
	// we created above.
	totalAssets := len(rpcSimpleAssets) + len(rpcIssuableAssets)
	require.Len(t.t, syncDiff.SyncedUniverses, totalAssets)

	// Each item in the diff should match the set of universe roots we got
	// from the source node above.
	for _, uniDiff := range syncDiff.SyncedUniverses {
		// The old root should be blank, as we're syncing this asset
		// for the first time.
		require.True(t.t, uniDiff.OldAssetRoot.MssmtRoot == nil)

		// A single new leaf should be present.
		require.Len(t.t, uniDiff.NewAssetLeaves, 1)

		// The new root should match the root we got from the primary
		// node above.
		newRoot := uniDiff.NewAssetRoot
		require.NotNil(t.t, newRoot)

		uniKey := func() string {
			switch {
			case newRoot.Id.GetAssetId() != nil:
				return hex.EncodeToString(
					newRoot.Id.GetAssetId(),
				)

			case newRoot.Id.GetGroupKey() != nil:
				groupKey, err := schnorr.ParsePubKey(
					newRoot.Id.GetGroupKey(),
				)
				require.NoError(t.t, err)

				h := sha256.Sum256(
					schnorr.SerializePubKey(groupKey),
				)

				return hex.EncodeToString(h[:])
			default:
				t.Fatalf("unknown universe asset id type")
				return ""
			}
		}()

		srcRoot, ok := universeRoots.UniverseRoots[uniKey]
		require.True(t.t, ok)
		assertUniverseRootEqual(t.t, srcRoot, newRoot)
	}

	// Now we'll fetch the Universe roots from Bob. These should match the
	// same roots that we got from the main universe node earlier.
	universeRootsBob, err := bob.AssetRoots(
		ctxt, &unirpc.AssetRootRequest{},
	)
	require.NoError(t.t, err)
	assertUniverseRootsEqual(t.t, universeRoots, universeRootsBob)

	// Finally, we'll ensure that the universe keys and leaves matches for
	// both parties.
	uniRoots := maps.Values(universeRoots.UniverseRoots)
	uniIDs := chanutils.Map(uniRoots,
		func(root *unirpc.UniverseRoot) *unirpc.ID {
			return root.Id
		},
	)
	assertUniverseKeysEqual(t.t, uniIDs, t.tarod, bob)
	assertUniverseLeavesEqual(t.t, uniIDs, t.tarod, bob)
}

func testUniverseREST(t *harnessTest) {
	// Mint a few assets that we then want to inspect in the universe.
	rpcSimpleAssets := mintAssetsConfirmBatch(t, t.tarod, simpleAssets)
	rpcIssuableAssets := mintAssetsConfirmBatch(t, t.tarod, issuableAssets)

	urlPrefix := fmt.Sprintf("https://%s/v1/taro/universe",
		t.tarod.clientCfg.RpcConf.RawRESTListeners[0])

	// First of all, get all roots and make sure our assets are contained
	// in the returned list.
	roots, err := getJSON[unirpc.AssetRootResponse](
		fmt.Sprintf("%s/roots", urlPrefix),
	)
	require.NoError(t.t, err)

	// Simple assets are keyed by their asset ID.
	for _, simpleAsset := range rpcSimpleAssets {
		assetID := hex.EncodeToString(simpleAsset.AssetGenesis.AssetId)
		require.Contains(t.t, roots.UniverseRoots, assetID)

		// Query the specific root to make sure we get the same result.
		assetRoot, err := getJSON[unirpc.QueryRootResponse](
			fmt.Sprintf("%s/roots/asset-id/%s", urlPrefix, assetID),
		)
		require.NoError(t.t, err)
		require.Equal(
			t.t, roots.UniverseRoots[assetID], assetRoot.AssetRoot,
		)
	}

	// Re-issuable assets are keyed by their group keys.
	for _, issuableAsset := range rpcIssuableAssets {
		// The group key is the full 33-byte public key, but the
		// response instead will use the schnorr serialized public key.
		// universe commits to the hash of the Schnorr serialized
		// public key.
		groupKey := issuableAsset.AssetGroup.TweakedGroupKey
		groupKeyHash := sha256.Sum256(groupKey[1:])
		groupKeyID := hex.EncodeToString(groupKeyHash[:])
		require.Contains(t.t, roots.UniverseRoots, groupKeyID)

		// Query the specific root to make sure we get the same result.
		// Rather than use the hash above, the API exposes the
		// serialized schorr key instead as the URI param.
		queryGroupKey := hex.EncodeToString(groupKey[1:])
		queryURI := fmt.Sprintf(
			"%s/roots/group-key/%s", urlPrefix, queryGroupKey,
		)
		assetRoot, err := getJSON[unirpc.QueryRootResponse](queryURI)
		require.NoError(t.t, err)

		require.Equal(
			t.t, roots.UniverseRoots[groupKeyID],
			assetRoot.AssetRoot,
		)
	}
}

// getJSON retrieves the body of a given URL, ignoring any TLS certificate the
// server might present.
func getJSON[T any](url string) (*T, error) {
	jsonResp := new(T)

	resp, err := client.Get(url)
	if err != nil {
		return jsonResp, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return jsonResp, err
	}

	if err = jsonMarshaler.Unmarshal(body, jsonResp); err != nil {
		return jsonResp, fmt.Errorf("failed to unmarshal %s: %v", body,
			err)
	}

	return jsonResp, nil
}

func testUniverseFederation(t *harnessTest) {
	// We'll kick off the test by making a new node, without hooking it up to
	// any existing Universe server.
	bob := setupTarodHarness(
		t.t, t, t.lndHarness.Bob, nil,
	)
	defer func() {
		require.NoError(t.t, bob.stop(true))
	}()

	ctx := context.Background()

	// Now that Bob is active, we'll make a set of assets with the main node.
	_ = mintAssetsConfirmBatch(t, t.tarod, simpleAssets[:1])

	// We'll now add the main node, as a member of Bob's Universe
	// federation. We expect that their state is synchronized shortly after
	// the call returns.
	_, err := bob.AddFederationServer(
		ctx, &unirpc.AddFederationServerRequest{
			Servers: []*unirpc.UniverseFederationServer{
				{
					Host: t.tarod.rpcHost(),
				},
			},
		},
	)
	require.NoError(t.t, err)

	// If we fetch the set of federation nodes, then the main node should
	// be shown as beign a part of that set.
	fedNodes, err := bob.ListFederationServers(
		ctx, &unirpc.ListFederationServersRequest{},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(fedNodes.Servers))
	require.Equal(t.t, t.tarod.rpcHost(), fedNodes.Servers[0].Host)

	// At this point, both nodes should have the same Universe roots.
	assertUniverseStateEqual(t.t, bob, t.tarod)

	// We'll now make a new asset with Bob, and ensure that the state is
	// properly pushed to the main node which is a part of the federation.
	newAsset := mintAssetsConfirmBatch(t, bob, simpleAssets[1:])

	// Bob should have a new asset in its local Universe tree.
	assetID := newAsset[0].AssetGenesis.AssetId
	waitErr := wait.NoError(func() error {
		_, err := bob.QueryAssetRoots(ctx, &unirpc.AssetRootQuery{
			Id: &unirpc.ID{
				Id: &unirpc.ID_AssetId{
					AssetId: assetID,
				},
			},
		})
		return err
	}, defaultTimeout)
	require.NoError(t.t, waitErr)

	// At this point, both nodes should have the same Universe roots as Bob
	// should have optimistically pushed the update to its federation
	// members.
	assertUniverseStateEqual(t.t, bob, t.tarod)

	// Next, we'll try to delete the main node from the federation.
	_, err = bob.DeleteFederationServer(
		ctx, &unirpc.DeleteFederationServerRequest{
			Servers: []*unirpc.UniverseFederationServer{
				{
					Host: t.tarod.rpcHost(),
				},
			},
		},
	)
	require.NoError(t.t, err)

	// If we fetch the set of federation nodes, then the main node should
	// no longer be present.
	fedNodes, err = bob.ListFederationServers(
		ctx, &unirpc.ListFederationServersRequest{},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 0, len(fedNodes.Servers))
}
