package cdp_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/simulation"

	abci "github.com/tendermint/tendermint/abci/types"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"

	"github.com/kava-labs/kava/app"
	auctiontypes "github.com/kava-labs/kava/x/auction/types"
	"github.com/kava-labs/kava/x/cdp"
	"github.com/kava-labs/kava/x/cdp/keeper"
	"github.com/kava-labs/kava/x/cdp/types"
)

type ModuleTestSuite struct {
	suite.Suite

	keeper       keeper.Keeper
	addrs        []sdk.AccAddress
	app          app.TestApp
	cdps         types.CDPs
	ctx          sdk.Context
	liquidations liquidationTracker
}

type liquidationTracker struct {
	xrp  []uint64
	btc  []uint64
	debt int64
}

func (suite *ModuleTestSuite) SetupTest() {
	tApp := app.NewTestApp()
	ctx := tApp.NewContext(true, tmproto.Header{Height: 1, Time: tmtime.Now()})
	tracker := liquidationTracker{}

	coins := cs(c("btc", 100000000), c("xrp", 10000000000))
	_, addrs := app.GeneratePrivKeyAddressPairs(100)
	authGS := app.NewFundedGenStateWithSameCoins(tApp.AppCodec(), coins, addrs)
	tApp.InitializeFromGenesisStates(
		authGS,
		NewPricefeedGenStateMulti(tApp.AppCodec()),
		NewCDPGenStateMulti(tApp.AppCodec()),
	)
	suite.ctx = ctx
	suite.app = tApp
	suite.keeper = tApp.GetCDPKeeper()
	suite.cdps = types.CDPs{}
	suite.addrs = addrs
	suite.liquidations = tracker
}

func (suite *ModuleTestSuite) createCdps() {
	tApp := app.NewTestApp()
	ctx := tApp.NewContext(true, tmproto.Header{Height: 1, Time: tmtime.Now()})
	cdps := make(types.CDPs, 100)
	tracker := liquidationTracker{}

	coins := cs(c("btc", 100000000), c("xrp", 10000000000))
	_, addrs := app.GeneratePrivKeyAddressPairs(100)

	authGS := app.NewFundedGenStateWithSameCoins(tApp.AppCodec(), coins, addrs)
	tApp.InitializeFromGenesisStates(
		authGS,
		NewPricefeedGenStateMulti(tApp.AppCodec()),
		NewCDPGenStateMulti(tApp.AppCodec()),
	)

	suite.ctx = ctx
	suite.app = tApp
	suite.keeper = tApp.GetCDPKeeper()

	// create 100 cdps
	for j := 0; j < 100; j++ {
		// 50 of the cdps will be collateralized with xrp
		collateral := "xrp"
		amount := 10000000000
		debt := simulation.RandIntBetween(rand.New(rand.NewSource(int64(j))), 750000000, 1249000000)
		// the other half (50) will be collateralized with btc
		if j%2 == 0 {
			collateral = "btc"
			amount = 100000000
			debt = simulation.RandIntBetween(rand.New(rand.NewSource(int64(j))), 2700000000, 5332000000)
			if debt >= 4000000000 {
				tracker.btc = append(tracker.btc, uint64(j+1))
				tracker.debt += int64(debt)
			}
		} else {
			if debt >= 1000000000 {
				tracker.xrp = append(tracker.xrp, uint64(j+1))
				tracker.debt += int64(debt)
			}
		}
		suite.Nil(suite.keeper.AddCdp(suite.ctx, addrs[j], c(collateral, int64(amount)), c("usdx", int64(debt)), collateral+"-a"))
		c, f := suite.keeper.GetCDP(suite.ctx, collateral+"-a", uint64(j+1))
		suite.True(f)
		cdps[j] = c
	}

	suite.cdps = cdps
	suite.addrs = addrs
	suite.liquidations = tracker
}

func (suite *ModuleTestSuite) setPrice(price sdk.Dec, market string) {
	pfKeeper := suite.app.GetPriceFeedKeeper()

	_, err := pfKeeper.SetPrice(suite.ctx, sdk.AccAddress{}, market, price, suite.ctx.BlockTime().Add(time.Hour*3))
	suite.NoError(err)

	err = pfKeeper.SetCurrentPrices(suite.ctx, market)
	suite.NoError(err)
	pp, err := pfKeeper.GetCurrentPrice(suite.ctx, market)
	suite.NoError(err)
	suite.Equal(price, pp.Price)
}

func (suite *ModuleTestSuite) TestBeginBlock() {
	// test setup, creating
	// 50 xrp cdps each with
	// collateral: 10000000000
	// debt: between 750000000 - 1249000000
	// if debt above 10000000000,
	//     cdp added to tracker / liquidation list
	//     debt total added to trackers debt total
	// 50 btc cdps each with
	// collateral: 10000000000
	// debt: between 2700000000 - 5332000000
	// if debt above 4000000000,
	//     cdp added to tracker / liquidation list
	//     debt total added to trackers debt total

	// naively we expect roughly half of the cdps to be above the debt tracking floor, roughly 25 of them collaterallized with xrp, the other 25 with btcb

	// usdx is the principal for all cdps
	suite.createCdps()
	ak := suite.app.GetAccountKeeper()
	bk := suite.app.GetBankKeeper()

	// test case 1 setup
	acc := ak.GetModuleAccount(suite.ctx, types.ModuleName)
	// track how much xrp collateral exists in the cdp module
	originalXrpCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "xrp").Amount
	// set the trading price for xrp:usd pools
	suite.setPrice(d("0.2"), "xrp:usd")

	// test case 1 execution
	cdp.BeginBlocker(suite.ctx, abci.RequestBeginBlock{Header: suite.ctx.BlockHeader()}, suite.keeper)

	// test case 1 assert
	acc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	// get the current amount of xrp held by the cdp module
	finalXrpCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "xrp").Amount
	seizedXrpCollateral := originalXrpCollateral.Sub(finalXrpCollateral)
	// calculate the number of cdps that were liquidated based on the total
	// seized collateral divided by the size of each cdp when it was created
	xrpLiquidations := int(seizedXrpCollateral.Quo(i(10000000000)).Int64())
	// should be 10 because...?
	suite.Equal(10, xrpLiquidations)

	// btc collateral test case setup
	acc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	originalBtcCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "btc").Amount
	// set the trading price for btc:usd pools
	suite.setPrice(d("6000"), "btc:usd")

	// btc collateral test case execution
	cdp.BeginBlocker(suite.ctx, abci.RequestBeginBlock{Header: suite.ctx.BlockHeader()}, suite.keeper)

	// btc collateral test case assertion 1
	acc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	finalBtcCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "btc").Amount
	seizedBtcCollateral := originalBtcCollateral.Sub(finalBtcCollateral)
	// calculate the number of btc cdps that were liquidated based on the
	// total seized collateral divided by the fixed size of each cdp
	// when it was created during test setup
	btcLiquidations := int(seizedBtcCollateral.Quo(i(100000000)).Int64())
	suite.Equal(10, btcLiquidations)

	// btc collateral test case assertion 2
	// test that the auction module has a balance equal to the amount of collateral seized
	acc = ak.GetModuleAccount(suite.ctx, auctiontypes.ModuleName)
	// should be this exact value because...?
	suite.Equal(int64(71955653865), bk.GetBalance(suite.ctx, acc.GetAddress(), "debt").Amount.Int64())
}

func (suite *ModuleTestSuite) TestSeizeSingleCdpWithFees() {
	// test setup
	// starting with zero cdps, add a single cdp of
	// xrp backed 1:1 with usdx
	err := suite.keeper.AddCdp(suite.ctx, suite.addrs[0], c("xrp", 10000000000), c("usdx", 1000000000), "xrp-a")
	suite.NoError(err)
	// verify the total value of all assets in cdps composed of xrp-a/usdx pair equals the amount of the single cdp we just added above
	suite.Equal(i(1000000000), suite.keeper.GetTotalPrincipal(suite.ctx, "xrp-a", "usdx"))
	ak := suite.app.GetAccountKeeper()
	bk := suite.app.GetBankKeeper()

	cdpMacc := ak.GetModuleAccount(suite.ctx, types.ModuleName)
	suite.Equal(i(1000000000), bk.GetBalance(suite.ctx, cdpMacc.GetAddress(), "debt").Amount)
	for i := 0; i < 100; i++ {
		suite.ctx = suite.ctx.WithBlockTime(suite.ctx.BlockTime().Add(time.Second * 6))
		cdp.BeginBlocker(suite.ctx, abci.RequestBeginBlock{Header: suite.ctx.BlockHeader()}, suite.keeper)
	}

	cdpMacc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	suite.Equal(i(1000000891), (bk.GetBalance(suite.ctx, cdpMacc.GetAddress(), "debt").Amount))
	cdp, _ := suite.keeper.GetCDP(suite.ctx, "xrp-a", 1)

	err = suite.keeper.SeizeCollateral(suite.ctx, cdp)
	suite.NoError(err)
	_, found := suite.keeper.GetCDP(suite.ctx, "xrp-a", 1)
	suite.False(found)
}

func (suite *ModuleTestSuite) TestCDPBeginBlockerRunsOnlyOnConfiguredInterval() {
	// test setup, creating
	// 50 xrp cdps each with
	// collateral: 10000000000
	// debt: between 750000000 - 1249000000
	// if debt above 10000000000,
	//     cdp added to tracker / liquidation list
	//     debt total added to trackers debt total
	// 50 btc cdps each with
	// collateral: 10000000000
	// debt: between 2700000000 - 5332000000
	// if debt above 4000000000,
	//     cdp added to tracker / liquidation list
	//     debt total added to trackers debt total

	// naively we expect roughly half of the cdps to be above the debt tracking floor, roughly 25 of them collaterallized with xrp, the other 25 with btcb

	// usdx is the principal for all cdps
	suite.createCdps()
	ak := suite.app.GetAccountKeeper()
	bk := suite.app.GetBankKeeper()

	// set the cdp begin blocker to run every other block
	params := suite.keeper.GetParams(suite.ctx)
	params.LiquidationBlockInterval = 2
	suite.keeper.SetParams(suite.ctx, params)

	// test case 1 setup
	acc := ak.GetModuleAccount(suite.ctx, types.ModuleName)
	// track how much xrp collateral exists in the cdp module
	originalXrpCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "xrp").Amount
	// set the trading price for xrp:usd pools
	suite.setPrice(d("0.2"), "xrp:usd")

	// test case 1 execution
	cdp.BeginBlocker(suite.ctx, abci.RequestBeginBlock{Header: suite.ctx.BlockHeader()}, suite.keeper)

	// test case 1 assert
	acc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	// get the current amount of xrp held by the cdp module
	finalXrpCollateral := bk.GetBalance(suite.ctx, acc.GetAddress(), "xrp").Amount
	seizedXrpCollateral := originalXrpCollateral.Sub(finalXrpCollateral)
	// calculate the number of cdps that were liquidated based on the total
	// seized collateral divided by the size of each cdp when it was created
	xrpLiquidations := int(seizedXrpCollateral.Quo(i(10000000000)).Int64())
	// should be 0 because the cdp begin blocker is configured to
	// skip execution every odd numbered block
	suite.Equal(0, xrpLiquidations, "expected cdp begin blocker not to run liqudations")

	// test case 2 setup
	// simulate running the second block of the chain
	suite.ctx = suite.ctx.WithBlockHeight(2)

	// test case 2 execution
	cdp.BeginBlocker(suite.ctx, abci.RequestBeginBlock{Header: suite.ctx.BlockHeader()}, suite.keeper)

	// test case 2 assert
	acc = ak.GetModuleAccount(suite.ctx, types.ModuleName)
	// get the current amount of xrp held by the cdp module
	finalXrpCollateral = bk.GetBalance(suite.ctx, acc.GetAddress(), "xrp").Amount
	seizedXrpCollateral = originalXrpCollateral.Sub(finalXrpCollateral)
	// calculate the number of cdps that were liquidated based on the total
	// seized collateral divided by the size of each cdp when it was created
	xrpLiquidations = int(seizedXrpCollateral.Quo(i(10000000000)).Int64())
	suite.Greater(xrpLiquidations, 0, "expected cdp begin blocker to run liquidations")
}

func TestModuleTestSuite(t *testing.T) {
	suite.Run(t, new(ModuleTestSuite))
}
