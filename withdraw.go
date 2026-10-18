package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var (
	addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	bytes32Re = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	proofRe   = regexp.MustCompile(`^0x[0-9a-fA-F]{512}$`)
)

const (
	proxyWithdrawMethodID = "0xb438689f"
	poolWithdrawMethodID  = "0x21a0adb6"
)

// canonicalWithdrawRequest parses the wire arguments according to methodId and
// rewrites every accepted request into one internal direct-pool representation:
//
//	proof=<proof>, contract=<instance>,
//	args=[root, nullifier, recipient, relayer, fee, refund]
//
// Full ABI argument positions are deliberately method-specific:
//
//	0x21a0adb6 Pool:  [proof, root, nullifier, recipient, relayer, fee, refund]
//	                                               ^ args[4]          ^ args[6]
//	0xb438689f Proxy: [instance, proof, root, nullifier, recipient, relayer, fee, refund]
//	                                                         ^ args[5]          ^ args[7]
//
// The Proxy target is never used as the transaction target. Its args[0]
// _tornado value becomes the trusted Pool target and the server later encodes a
// fresh 0x21a0adb6 Pool call. For backwards compatibility, the API also accepts
// the historical split-proof forms (top-level proof plus 6 Pool args or 7 Proxy
// args); those are normalized to the same representation.
func canonicalWithdrawRequest(body WithdrawRequest) (WithdrawRequest, error) {
	switch strings.ToLower(strings.TrimSpace(body.MethodID)) {
	case "":
		// Legacy direct-Pool JSON shape: proof is top-level and args starts at
		// _root. Any ABI-complete shape must carry methodId so the argument
		// indexes cannot be confused with the Proxy layout.
		if len(body.Args) != withdrawArgCount {
			return WithdrawRequest{}, fmt.Errorf(".methodId is required for ABI argument arrays; use %s for Pool or %s for Proxy", poolWithdrawMethodID, proxyWithdrawMethodID)
		}
		return body, nil
	case poolWithdrawMethodID:
		return canonicalPoolRequest(body)
	case proxyWithdrawMethodID:
		return canonicalProxyRequest(body)
	default:
		return WithdrawRequest{}, fmt.Errorf("unsupported .methodId %q; expected %s (Pool) or %s (Proxy)", body.MethodID, poolWithdrawMethodID, proxyWithdrawMethodID)
	}
}

// liftABIProof moves the ABI _proof argument at proofIndex to the top-level
// proof (it must agree with a top-level proof if one was sent) and keeps the
// args from firstCanonicalArg on.
func liftABIProof(body WithdrawRequest, proofIndex, firstCanonicalArg int) (WithdrawRequest, error) {
	proof := body.Args[proofIndex]
	if body.Proof != "" && !strings.EqualFold(body.Proof, proof) {
		return WithdrawRequest{}, errors.New("top-level .proof does not match ABI _proof argument")
	}
	body.Proof = proof
	body.Args = append([]string(nil), body.Args[firstCanonicalArg:]...)
	body.MethodID = ""
	return body, nil
}

// canonicalPoolRequest normalizes a 0x21a0adb6 Pool request.
func canonicalPoolRequest(body WithdrawRequest) (WithdrawRequest, error) {
	switch len(body.Args) {
	case withdrawArgCount: // split proof: [root, nullifier, recipient, relayer, fee, refund]
		body.MethodID = ""
		return body, nil
	case withdrawArgCount + 1: // full ABI: relayer=args[4], fee=args[5], refund=args[6]
		return liftABIProof(body, 0, 1)
	default:
		return WithdrawRequest{}, errors.New("pool withdraw args must contain 7 full ABI items, or 6 items when proof is top-level")
	}
}

// canonicalProxyRequest normalizes a 0xb438689f Proxy request; its _tornado
// argument becomes the Pool target.
func canonicalProxyRequest(body WithdrawRequest) (WithdrawRequest, error) {
	if !addressRe.MatchString(body.Contract) {
		return WithdrawRequest{}, errors.New(".contract proxy target must be a valid Ethereum address")
	}
	if len(body.Args) != withdrawArgCount+1 && len(body.Args) != withdrawArgCount+2 {
		return WithdrawRequest{}, errors.New("proxy withdraw args must contain 8 full ABI items, or 7 items when proof is top-level")
	}
	if !addressRe.MatchString(body.Args[0]) {
		return WithdrawRequest{}, errors.New("proxy args[0] _tornado must be an Ethereum instance address")
	}

	instance := common.HexToAddress(body.Args[0]).Hex()
	if len(body.Args) == withdrawArgCount+1 {
		// Split proof: [instance, root, nullifier, recipient, relayer, fee, refund].
		body.Contract = instance
		body.Args = append([]string(nil), body.Args[1:]...)
		body.MethodID = ""
		return body, nil
	}

	// Full ABI: [instance, proof, root, nullifier, recipient, relayer,
	// fee, refund]. In particular _relayer is args[5] and _refund is
	// args[7] before normalization.
	canonical, err := liftABIProof(body, 1, 2)
	if err != nil {
		return WithdrawRequest{}, err
	}
	canonical.Contract = instance
	return canonical, nil
}

// validWithdraw is a canonical request that passed validateWithdraw, bound to
// its configured pool. Every later step uses it instead of re-validating.
type validWithdraw struct {
	req  WithdrawRequest // canonical: withdrawArgCount args, zero refund
	pool common.Address
	meta InstanceMeta
}

// validateWithdraw canonicalizes a request (any accepted wire shape, including
// a persisted one) and checks it against the wire formats and the current
// admission policy. It is the only place a request is validated.
func (r *Relayer) validateWithdraw(body WithdrawRequest) (validWithdraw, error) {
	body, err := canonicalWithdrawRequest(body)
	if err != nil {
		return validWithdraw{}, err
	}
	if !addressRe.MatchString(body.Args[argRelayer]) {
		return validWithdraw{}, errors.New("_relayer must be an Ethereum address")
	}
	if common.HexToAddress(body.Args[argRelayer]) != r.cfg.RewardAccount {
		return validWithdraw{}, errors.New("_relayer must match the configured rewardAccount")
	}
	pool, meta, err := r.configuredPool(body.Contract)
	if err != nil {
		return validWithdraw{}, err
	}
	if err := validateWithdrawArgs(body); err != nil {
		return validWithdraw{}, err
	}
	return validWithdraw{req: body, pool: pool, meta: meta}, nil
}

// validateWithdrawArgs checks the proof and argument formats of a canonical
// request.
func validateWithdrawArgs(body WithdrawRequest) error {
	if !proofRe.MatchString(body.Proof) {
		return errors.New(".proof should match pattern \"^0x[a-fA-F0-9]{512}$\"")
	}
	if !bytes32Re.MatchString(body.Args[argRoot]) {
		return errors.New(".args[0] root must be bytes32")
	}
	if !bytes32Re.MatchString(body.Args[argNullifierHash]) {
		return errors.New(".args[1] nullifierHash must be bytes32")
	}
	if !addressRe.MatchString(body.Args[argRecipient]) {
		return errors.New(".args[2] recipient must be an Ethereum address")
	}
	if common.HexToAddress(body.Args[argRecipient]) == (common.Address{}) {
		return errors.New(".args[2] recipient must not be the zero address")
	}
	if !bytes32Re.MatchString(body.Args[argFee]) {
		return errors.New("_fee must be bytes32")
	}
	if !bytes32Re.MatchString(body.Args[argRefund]) {
		return errors.New("_refund must be bytes32")
	}
	if common.HexToHash(body.Args[argRefund]) != (common.Hash{}) {
		return errors.New("_refund must be zero; non-zero refund transactions are not supported")
	}
	return nil
}

func (r *Relayer) nullifierKey(req WithdrawRequest) string {
	return strings.Join([]string{
		r.cfg.ChainID.String(),
		strings.ToLower(common.HexToAddress(req.Contract).Hex()),
		strings.ToLower(common.HexToHash(req.Args[argNullifierHash]).Hex()),
	}, ":")
}
