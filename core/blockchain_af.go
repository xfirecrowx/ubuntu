package core

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// errReorgFinality represents an error caused by artificial finality mechanisms.
var errReorgFinality = errors.New("finality-enforced invalid new chain")

// ArtificialFinalityNoDisable overrides toggling of AF features, forcing it on.
// n  = 1 : ON
// n != 1 : OFF
func (bc *BlockChain) ArtificialFinalityNoDisable(n int32) {
	log.Warn("Deactivating ECBP1100 (MESS) disablers", "always on", true)
	bc.artificialFinalityNoDisable = new(int32)
	atomic.StoreInt32(bc.artificialFinalityNoDisable, n)
}

// EnableArtificialFinality enables and disable artificial finality features for the blockchain.
// Currently toggled features include:
// - ECBP1100-MESS: modified exponential subject scoring
//
// This level of activation works BELOW the chain configuration for any of the
// potential features. eg. If ECBP1100 is not activated at the chain config x block number,
// then calling bc.EnableArtificialFinality(true) will be a noop.
// The method is idempotent.
func (bc *BlockChain) EnableArtificialFinality(enable bool, logValues ...interface{}) {
	// Short circuit if AF state is enabled and nodisable=true.
	if bc.artificialFinalityNoDisable != nil && atomic.LoadInt32(bc.artificialFinalityNoDisable) == 1 &&
		bc.IsArtificialFinalityEnabled() && !enable {
		log.Warn("Preventing disable artificial finality", "enabled", true, "nodisable", true)
		return
	}

	// Store enable/disable value regardless of config activation.
	var statusLog string
	if enable {
		statusLog = "Enabled"
		atomic.StoreInt32(&bc.artificialFinalityEnabledStatus, 1)
	} else {
		statusLog = "Disabled"
		atomic.StoreInt32(&bc.artificialFinalityEnabledStatus, 0)
	}
	if !bc.chainConfig.IsEnabled(bc.chainConfig.GetECBP1100Transition, bc.CurrentHeader().Number) {
		// Don't log anything if the config hasn't enabled it yet.
		return
	}
	logFn := log.Warn // Deactivated and enabled
	if enable {
		logFn = log.Info // Activated and enabled
	}
	logFn(fmt.Sprintf("%s artificial finality features", statusLog), logValues...)
}

// IsArtificialFinalityEnabled returns the status of the blockchain's artificial
// finality feature setting.
// This status is agnostic of feature activation by chain configuration.
func (bc *BlockChain) IsArtificialFinalityEnabled() bool {
	return atomic.LoadInt32(&bc.artificialFinalityEnabledStatus) == 1
}

// getTDRatio is a helper function returning the total difficulty ratio of
// proposed over current chain segments.
func (bc *BlockChain) getTDRatio(commonAncestor, current, proposed *types.Header) float64 {
	// Get the total difficulty ratio of the proposed chain segment over the existing one.
	commonAncestorTD := bc.GetTd(commonAncestor.Hash(), commonAncestor.Number.Uint64())

	proposedParentTD := bc.GetTd(proposed.ParentHash, proposed.Number.Uint64()-1)
	proposedTD := new(big.Int).Add(proposed.Difficulty, proposedParentTD)

	localTD := bc.GetTd(current.Hash(), current.Number.Uint64())

	tdRatio, _ := new(big.Float).Quo(
		new(big.Float).SetInt(new(big.Int).Sub(proposedTD, commonAncestorTD)),
		new(big.Float).SetInt(new(big.Int).Sub(localTD, commonAncestorTD)),
	).Float64()
	return tdRatio
}

// adessOmega is a grace period in which premier-canonical tallies are not counted.
// The grace period spans from the fork block on.
var adessOmega uint64 = 4

// adessEpsilon is the per-block penalty: 1 / (1 + epsilon)
var adessEpsilonQuo = big.NewInt(1000) // ie. 1 / 1000

// adessPenaltyAssignment implements the logic to decide whether or not the proposed chain segment
// should be assigned a penalty/handicap on their consensus score (total difficulty) value.
// The penalty will be assigned IFF the proposed segment has FEWER premier-canonical blocks
// than the incumbent segment.
// If the incumbent segment has fewer premier-canonical blocks, the penalty will not be assigned
// and the proposed segment's eligibility for canonical status is invariant from GHOST.
func (bc *BlockChain) adessPenaltyAssignment(commonAncestor, current, proposed *types.Header) bool {

	// FIRST, we must guarantee there exist >= omega blocks in the proposed chain
	// if NOT, the condition is unsatisfied, and we return early, doing nothing (noop).
	alphaHeight := commonAncestor.Number.Uint64() + adessOmega
	if proposed.Number.Uint64() < alphaHeight {
		return false // No penalty; ADESS inactive.
	}

	currentPCCount := bc.adessCountPremierCanonical(current, commonAncestor)
	proposedPCCount := bc.adessCountPremierCanonical(proposed, commonAncestor)

	// The penalty is applied if the current segment has more premier-canonical blocks
	// than the proposed segment.
	return currentPCCount > proposedPCCount
}

// adessCountPremierCanonical counts how many blocks in the chain segment are premier-canonical blocks.
// It skips the grace period range defined by adessOmega.
// This is a 'helper' function; it is only used by the adessPenaltyAssignment function.
func (bc *BlockChain) adessCountPremierCanonical(head, common *types.Header) (total int) {
	for h := head; h != nil && h.Hash() != common.Hash(); h = bc.GetHeaderByHash(h.ParentHash) {
		if h.Number.Uint64() < common.Number.Uint64()+adessOmega {
			// This header is within the alpha-defined 'grace period'.
			continue
		}

		if rawdb.ReadPremierCanonicalHash(bc.db, premiereCanonicalNumber(h)) == h.Hash() {
			// This block WAS first seen.
			// The ADESS penalty assignment condition is NOT satisfied.
			total++
		}
	}
	return
}

// adessPenaltyProposed returns the discount (expressed in Total Difficulty) that will be
// applied to the proposed chain if and when the penalty is assigned.
func (bc *BlockChain) adessPenaltyProposed(commonAncestor, current, proposed *types.Header) *big.Int {
	// Traverse the proposed segment backwards and sum up the total discount.
	totalDiscount := new(big.Int)
	for h := proposed; h != nil && h.Hash() != commonAncestor.Hash(); h = bc.GetHeaderByHash(h.ParentHash) {
		blockTD := bc.GetTd(h.Hash(), h.Number.Uint64())
		blockDiscount := new(big.Int).Div(blockTD, adessEpsilonQuo)
		totalDiscount.Add(totalDiscount, blockDiscount)
	}
	return totalDiscount
}

// adess implements the proposal documented in 'A Proof-of-Work Protocol to Deter Double-Spend Attacks'
// The function returns 'nil' (no error) if the proposed reorganization is allowed,
// otherwise it returns an error contextualizing the insufficiency of the proposed segment.
// Variables (parameters) for this logic are defined immediately above.
func (bc *BlockChain) adess(commonAncestor, current, proposed *types.Header) error {

	// If the penalty is not assigned, return early.
	// ADESS is inactive.
	if !bc.adessPenaltyAssignment(commonAncestor, current, proposed) {
		return nil
	}

	// Get the total difficulties of the proposed chain segment and the existing one.

	// Operational boilerplate:
	commonAncestorTD := bc.GetTd(commonAncestor.Hash(), commonAncestor.Number.Uint64())
	proposedParentTD := bc.GetTd(proposed.ParentHash, proposed.Number.Uint64()-1)
	proposedTD := new(big.Int).Add(proposed.Difficulty, proposedParentTD)
	localTD := bc.GetTd(current.Hash(), current.Number.Uint64())

	// Local and proposed segment TDs (post fork block):
	proposedSubchainTD := new(big.Int).Sub(proposedTD, commonAncestorTD)
	localSubchainTD := new(big.Int).Sub(localTD, commonAncestorTD)

	// proposedPenalty is the raw penalty value derived from the proposed segment.
	// This value will be deducted from the proposed segment's TD.
	proposedPenalty := bc.adessPenaltyProposed(commonAncestor, current, proposed)

	// Deduct the penalty directly from the proposed segment's TD value.
	proposedSubchainTD.Sub(proposedSubchainTD, proposedPenalty)

	// If the local score is greater than the handicapped proposed score,
	// return an error indicating that ADESS wants to reject the proposed segment.
	if localSubchainTD.Cmp(proposedSubchainTD) > 0 {
		return fmt.Errorf(`ADESS rejects proposed segment`)
	}

	// Otherwise the proposed segment has met or exceeded the penalty demand
	// and should become canonical; ADESS permits this chain.
	return nil
}

// ecbp1100 implements the "MESS" artificial finality mechanism
// "Modified Exponential Subjective Scoring" used to prefer known chain segments
// over later-to-come counterparts, especially proposed segments stretching far into the past.
func (bc *BlockChain) ecbp1100(commonAncestor, current, proposed *types.Header) error {

	// Short-circuit to no-op in case proposed segment has greater premier-canonical TD than current.
	//
	// Intention: We want to exempt proposed segments which have greater saturation of first-to-canonical blocks
	// than the incumbent segment, but keep MESS activated when this is not the case.
	//
	// The logic behind this is that malicious/suspicious segments will exhibit characteristically lower
	// first-to-canonical saturation because of their necessary initial isolation (secrecy).
	// This logic provides a way to identify segments having greater or lesser "competitive publicity" rate (which is argued to
	// be relatively indicative of "honesty").
	// Relatively competitively-public (identified as "honest") segments are not subject to the MESS acceptance algorithm.
	//
	// PCS: Premier Canonical Score
	currentPCS := bc.getTdPremierCanonical(commonAncestor, current, current.Time)
	proposedPCS := bc.getTdPremierCanonical(commonAncestor, proposed, current.Time)

	if proposedPCS.Cmp(currentPCS) > 0 {
		// MESS is not applied (and reorg continues normally), since the proposed chain is more saturated with
		// first-to-canonical blocks.
		//
		// Code: Short circuit, returning no error and allowing the reorg to proceed without MESS intervention.
		return nil
	}

	// Get the total difficulties of the proposed chain segment and the existing one.
	commonAncestorTD := bc.GetTd(commonAncestor.Hash(), commonAncestor.Number.Uint64())
	proposedParentTD := bc.GetTd(proposed.ParentHash, proposed.Number.Uint64()-1)
	proposedTD := new(big.Int).Add(proposed.Difficulty, proposedParentTD)
	localTD := bc.GetTd(current.Hash(), current.Number.Uint64())

	// if proposed_subchain_td * CURVE_FUNCTION_DENOMINATOR < get_curve_function_numerator(proposed.Time - commonAncestor.Time) * local_subchain_td.
	proposedSubchainTD := new(big.Int).Sub(proposedTD, commonAncestorTD)
	localSubchainTD := new(big.Int).Sub(localTD, commonAncestorTD)

	xBig := big.NewInt(int64(current.Time - commonAncestor.Time))
	eq := ecbp1100PolynomialV(xBig)
	want := eq.Mul(eq, localSubchainTD)

	got := new(big.Int).Mul(proposedSubchainTD, ecbp1100PolynomialVCurveFunctionDenominator)

	if got.Cmp(want) < 0 {
		prettyRatio, _ := new(big.Float).Quo(
			new(big.Float).SetInt(got),
			new(big.Float).SetInt(want),
		).Float64()
		return fmt.Errorf(`%w: ECBP1100-MESS 🔒 status=rejected age=%v current.span=%v proposed.span=%v tdr/gravity=%0.6f common.bno=%d common.hash=%s current.bno=%d current.hash=%s proposed.bno=%d proposed.hash=%s`,
			errReorgFinality,
			common.PrettyAge(time.Unix(int64(commonAncestor.Time), 0)),
			common.PrettyDuration(time.Duration(current.Time-commonAncestor.Time)*time.Second),
			common.PrettyDuration(time.Duration(int32(xBig.Uint64()))*time.Second),
			prettyRatio,
			commonAncestor.Number.Uint64(), commonAncestor.Hash().Hex(),
			current.Number.Uint64(), current.Hash().Hex(),
			proposed.Number.Uint64(), proposed.Hash().Hex(),
		)
	}
	return nil
}

// getTdPremierCanonical gets the sum of difficulties for all blocks in a segment which are marked
// as "premier-canonical," that is, as being the first-seen for a given block number.
// We assume that all headers between common ancestor (inclusive) and the head (non inclusive, since we have the header value already)
// will exist in DB, and thus have an available TD measurement.
func (bc *BlockChain) getTdPremierCanonical(commonAncestor, head *types.Header, segmentLatestTime uint64) (score *big.Int) {

	score = big.NewInt(0)

	// Iterate backwards through the whole segment, starting at the top, and going until the common ancestor.
	for focus := head; focus != nil /* safety, should never happen */ && focus.Hash() != commonAncestor.Hash(); focus = bc.GetHeaderByHash(focus.ParentHash) {

		// There are two conditions for the addition of the block's value to the score:

		// 1. If the block's timestamp lies within the respective span of the current segment.
		if focus.Time > segmentLatestTime {
			continue
		}

		// 2. If the header is marked as premier-canonical, its unit score is included in the sum.
		if rawdb.ReadPremierCanonicalHash(bc.db, premiereCanonicalNumber(focus)) == focus.Hash() {

			// Emphasize the priority associated with the leading (oldest) sections versus the later section
			// of the segment.

			// Older blocks get higher difficulty compensation.
			// current.Time - block.Time
			// Old blocks get bigger numbers, eg. 1 hour = 3600
			// Young blocks get small numbers, eg. 2 minutes = 120

			// old:    		 3600
			// medium: 		 1000
			// medium-young: 500
			// young:        120
			// v. young:     3

			score.Add(score, new(big.Int).SetUint64(segmentLatestTime-focus.Time))
		}
	}

	// Net weighted difficulty for a chain segment.
	return score
}

// premiereCanonicalNumber gets the value (number) on which premier-canonical entries in the database
// are keyed.
// FIXME: (Sort of, FIXME... more like WIP).
// This function wraps a header value and returns a number.
// It's essentially used to define the key (a number) under which the premier-canonical entry is
// stored in the database.
// For the time being, this is the HEADER NUMBER.
// However, we have also experimented with using HEADER TIMESTAMP and DIFFICULTY.
// HEADER NUMBER is the simplest to reason about and easiest to implement, so, pending any discoveries
// w/r/t the algorithm in theory, we'll probably stick with this.
//
// When this decision is finalized, the function can be removed,
// and callers of rawdb.[Get|Write|Delete]PremierCanonicalHash can just use the proper header value directly.
func premiereCanonicalNumber(header *types.Header) uint64 {
	return header.Number.Uint64()
}

/*
ecbp1100PolynomialV is a cubic function that looks a lot like Option 3's sin function,
but adds the benefit that the calculation can be done with integers (instead of yucky floating points).
> https://github.com/ethereumclassic/ECIPs/issues/374#issuecomment-694156719

CURVE_FUNCTION_DENOMINATOR = 128

def get_curve_function_numerator(time_delta: int) -> int:
    xcap = 25132 # = floor(8000*pi)
    ampl = 15
    height = CURVE_FUNCTION_DENOMINATOR * (ampl * 2)
    if x > xcap:
        x = xcap
    # The sine approximator `y = 3*x**2 - 2*x**3` rescaled to the desired height and width
    return CURVE_FUNCTION_DENOMINATOR + (3 * x**2 - 2 * x**3 // xcap) * height // xcap ** 2


The if tdRatio < antiGravity check would then be

if proposed_subchain_td * CURVE_FUNCTION_DENOMINATOR < get_curve_function_numerator(current.Time - commonAncestor.Time) * local_subchain_td.
*/
func ecbp1100PolynomialV(x *big.Int) *big.Int {

	// Make a copy; do not mutate argument value.

	// if x > xcap:
	//    x = xcap
	xA := new(big.Int).Set(x)
	if xA.Cmp(ecbp1100PolynomialVXCap) > 0 {
		xA.Set(ecbp1100PolynomialVXCap)
	}

	xB := new(big.Int).Set(x)
	if xB.Cmp(ecbp1100PolynomialVXCap) > 0 {
		xB.Set(ecbp1100PolynomialVXCap)
	}

	out := big.NewInt(0)

	// 3 * x**2
	xA.Exp(xA, big2, nil)
	xA.Mul(xA, big3)

	// 2 * x**3 // xcap
	xB.Exp(xB, big3, nil)
	xB.Mul(xB, big2)
	xB.Div(xB, ecbp1100PolynomialVXCap)

	// (3 * x**2 - 2 * x**3 // xcap)
	out.Sub(xA, xB)

	// // (3 * x**2 - 2 * x**3 // xcap) * height
	out.Mul(out, ecbp1100PolynomialVHeight)

	// xcap ** 2
	xcap2 := new(big.Int).Exp(ecbp1100PolynomialVXCap, big2, nil)

	// (3 * x**2 - 2 * x**3 // xcap) * height // xcap ** 2
	out.Div(out, xcap2)

	// CURVE_FUNCTION_DENOMINATOR + (3 * x**2 - 2 * x**3 // xcap) * height // xcap ** 2
	out.Add(out, ecbp1100PolynomialVCurveFunctionDenominator)
	return out
}

var big2 = big.NewInt(2)
var big3 = big.NewInt(3)

// ecbp1100PolynomialVCurveFunctionDenominator
// CURVE_FUNCTION_DENOMINATOR = 128
var ecbp1100PolynomialVCurveFunctionDenominator = big.NewInt(128)

// ecbp1100PolynomialVXCap
// xcap = 25132 # = floor(8000*pi)
var ecbp1100PolynomialVXCap = big.NewInt(25132)

// ecbp1100PolynomialVAmpl
// ampl = 15
var ecbp1100PolynomialVAmpl = big.NewInt(15)

// ecbp1100PolynomialVHeight
// height = CURVE_FUNCTION_DENOMINATOR * (ampl * 2)
var ecbp1100PolynomialVHeight = new(big.Int).Mul(new(big.Int).Mul(ecbp1100PolynomialVCurveFunctionDenominator, ecbp1100PolynomialVAmpl), big2)

/*
ecbp1100AGSinusoidalA is a sinusoidal function.

OPTION 3: Yet slower takeoff, yet steeper eventual ascent. Has a differentiable ceiling transition.
h(x)=15 sin((x+12000 π)/(8000))+15+1

*/
func ecbp1100AGSinusoidalA(x float64) (antiGravity float64) {
	ampl := float64(15)   // amplitude
	pDiv := float64(8000) // period divisor
	phaseShift := math.Pi * (pDiv * 1.5)
	peakX := math.Pi * pDiv // x value of first sin peak where x > 0
	if x > peakX {
		// Cause the x value to limit to the x value of the first peak of the sin wave (ceiling).
		x = peakX
	}
	return (ampl * math.Sin((x+phaseShift)/pDiv)) + ampl + 1
}

/*
ecbp1100AGExpB is an exponential function with x as a base (and rationalized exponent).

OPTION 2: Slightly slower takeoff, steeper eventual ascent
g(x)=x^(x*0.00002)
*/
func ecbp1100AGExpB(x float64) (antiGravity float64) {
	return math.Pow(x, x*0.00002)
}

/*
ecbp1100AGExpA is an exponential function with x as exponent.

This was (one of?) Vitalik's "original" specs:
> 1.0001 ** (number of seconds between when S1 was received and when S2 was received)
- https://bitcointalk.org/index.php?topic=865169.msg16349234#msg16349234
> gravity(B') = gravity(B) * 0.99 ^ n
- https://blog.ethereum.org/2014/11/25/proof-stake-learned-love-weak-subjectivity/

OPTION 1 (Original ESS)
f(x)=1.0001^(x)
*/
func ecbp1100AGExpA(x float64) (antiGravity float64) {
	return math.Pow(1.0001, x)
}
