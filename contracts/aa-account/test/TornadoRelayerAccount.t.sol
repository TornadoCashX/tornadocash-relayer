// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import "forge-std/Test.sol";
import {EntryPoint} from "@account-abstraction/core/EntryPoint.sol";
import {IEntryPoint} from "@account-abstraction/interfaces/IEntryPoint.sol";
import {PackedUserOperation as EPUserOp} from "@account-abstraction/interfaces/PackedUserOperation.sol";
import {TornadoRelayerAccount, IEntryPointMinimal, PackedUserOperation} from "../src/TornadoRelayerAccount.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";

contract TestToken is ERC20 {
    constructor() ERC20("T", "T") {}

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}

/// Mimics ETHTornado.withdraw: no msg.sender check, nonReentrant, marks the
/// nullifier before paying recipient and _relayer by call (forwarding all gas).
contract MockTornadoPool {
    uint256 public constant DENOMINATION = 1 ether;
    mapping(bytes32 => bool) public nullifierHashes;
    bool private locked;

    modifier nonReentrant() {
        require(!locked, "ReentrancyGuard: reentrant call");
        locked = true;
        _;
        locked = false;
    }

    constructor() payable {}

    function withdraw(
        bytes calldata proof,
        bytes32,
        bytes32 nullifierHash,
        address payable recipient,
        address payable relayer,
        uint256 fee,
        uint256 refund
    ) external payable nonReentrant {
        require(fee <= DENOMINATION, "Fee exceeds transfer value");
        require(!nullifierHashes[nullifierHash], "The note has been already spent");
        require(proof.length == 256, "Invalid withdraw proof");
        require(msg.value == refund, "Incorrect refund amount received by the contract");
        nullifierHashes[nullifierHash] = true;
        (bool ok,) = recipient.call{value: DENOMINATION - fee}("");
        require(ok, "payment to _recipient did not succeed");
        if (fee > 0) {
            (ok,) = relayer.call{value: fee}("");
            require(ok, "payment to _relayer did not succeed");
        }
    }

    function isSpent(bytes32 n) external view returns (bool) {
        return nullifierHashes[n];
    }
}

/// Malicious recipient: on receiving funds it tries every re-entry path and
/// records which ones succeeded, without reverting itself.
contract ReentrantRecipient {
    TornadoRelayerAccount public account;
    EntryPoint public ep;
    MockTornadoPool public pool;
    bytes public replayData;
    bool public reenteredAccount;
    bool public reenteredEntryPoint;
    bool public reenteredPool;
    bool public changedConfig;
    bool public pulledDeposit;
    bool public hit;

    function arm(TornadoRelayerAccount a, EntryPoint e, MockTornadoPool p, bytes calldata d) external {
        account = a;
        ep = e;
        pool = p;
        replayData = d;
    }

    receive() external payable {
        hit = true;
        bool ok;
        (ok,) = address(account).call(abi.encodeCall(TornadoRelayerAccount.relayWithdraw, (address(pool), replayData)));
        reenteredAccount = ok;
        EPUserOp[] memory none = new EPUserOp[](0);
        (ok,) = address(ep).call(abi.encodeCall(EntryPoint.handleOps, (none, payable(address(this)))));
        reenteredEntryPoint = ok;
        (ok,) = address(pool).call(replayData);
        reenteredPool = ok;
        (ok,) = address(account).call(abi.encodeCall(TornadoRelayerAccount.setRewardAccount, (address(this))));
        changedConfig = ok;
        uint256 before = address(this).balance;
        (ok,) = address(account)
            .call(abi.encodeCall(TornadoRelayerAccount.withdrawEntryPoint, (payable(address(this)), 1)));
        (ok,) = address(ep).call(abi.encodeCall(ep.withdrawTo, (payable(address(this)), 1)));
        pulledDeposit = address(this).balance > before;
    }
}

/// Griefing recipient: burns all forwarded gas so the withdraw reverts.
contract GasBurnerRecipient {
    receive() external payable {
        while (true) {}
    }
}

contract TornadoRelayerAccountTest is Test {
    EntryPoint ep;
    TornadoRelayerAccount account;
    MockTornadoPool pool;
    MockTornadoPool otherPool;

    /// 测试默认使用的 nonce 通道（生产环境 Go 端复用 key = 0..3）。
    uint192 constant LANE = 1;
    MockTornadoPool pool3;
    MockTornadoPool pool4;

    uint256 ownerKey = 0xA11CE; // signer #1
    address owner;
    uint256 signer2Key = 0xB0B; // signer #2
    address signer2;
    address payable rewardAccount = payable(makeAddr("rewardAccount"));
    address payable funder = payable(makeAddr("funder"));
    address admin = makeAddr("admin");
    address payable bundler = payable(makeAddr("bundler"));
    address payable recipient = payable(makeAddr("recipient"));

    bytes32 constant NULLIFIER = keccak256("nullifier-1");
    uint256 constant FEE = 0.01 ether;

    function setUp() public {
        vm.warp(1_000_000);
        owner = vm.addr(ownerKey);
        signer2 = vm.addr(signer2Key);
        ep = new EntryPoint();
        pool = new MockTornadoPool{value: 10 ether}();
        otherPool = new MockTornadoPool{value: 10 ether}();
        pool3 = new MockTornadoPool{value: 10 ether}();
        pool4 = new MockTornadoPool{value: 10 ether}();
        address[] memory signers = new address[](2);
        signers[0] = owner;
        signers[1] = signer2;
        account = new TornadoRelayerAccount(
            IEntryPointMinimal(address(ep)), admin, rewardAccount, signers, fourPools(), caps()
        );
        vm.deal(address(account), 1 ether); // 本合约余额（不会自动进入押金）
        ep.depositTo{value: 1 ether}(address(account)); // gas 只从押金扣
    }

    // ------------------------------------------------------------------ helpers

    function withdrawData(address relayer, uint256 refund, bytes32 nullifier) internal view returns (bytes memory) {
        return abi.encodeWithSelector(
            MockTornadoPool.withdraw.selector,
            new bytes(256),
            bytes32(uint256(1)),
            nullifier,
            recipient,
            relayer,
            FEE,
            refund
        );
    }

    function buildOp(address target, bytes memory data, uint192 key) internal view returns (EPUserOp memory op) {
        op.sender = address(account);
        op.nonce = ep.getNonce(address(account), key);
        op.callData = abi.encodeCall(TornadoRelayerAccount.relayWithdraw, (target, data));
        op.accountGasLimits = bytes32((uint256(200_000) << 128) | uint256(300_000));
        op.preVerificationGas = 50_000;
        op.gasFees = bytes32((uint256(1 gwei) << 128) | uint256(2 gwei));
    }

    uint96 constant CAP_FEE = 100 gwei;
    uint32 constant CAP_PVG = 100_000;
    uint32 constant CAP_VGL = 300_000;
    uint32 constant CAP_CGL = 500_000;

    /// 固定池：pool / pool3 / pool4（第 4 个位置重复填 pool）；otherPool 不是固定池。

    function caps() internal pure returns (TornadoRelayerAccount.Config memory) {
        return TornadoRelayerAccount.Config(CAP_FEE, CAP_PVG, CAP_VGL, CAP_CGL);
    }

    function fourPools() internal view returns (address[4] memory) {
        return [address(pool), address(pool3), address(pool4), address(pool)];
    }

    /// 真实的 validAfter：当前时间前 1 秒（v0.9 要求 block.timestamp > validAfter）。
    function ago() internal view returns (uint48) {
        return uint48(block.timestamp - 1);
    }

    function sign(EPUserOp memory op, uint256 key, uint48 validAfter, uint48 validUntil) internal view {
        bytes32 h = ep.getUserOpHash(op);
        bytes32 digest = account.signingDigest(h, validAfter, validUntil);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(key, digest);
        op.signature = abi.encodePacked(bytes6(validAfter), bytes6(validUntil), r, s, v);
    }

    function handle(EPUserOp memory op) internal {
        EPUserOp[] memory ops = new EPUserOp[](1);
        ops[0] = op;
        vm.prank(bundler, bundler);
        ep.handleOps(ops, bundler);
    }

    function validOp() internal view returns (EPUserOp memory op) {
        op = buildOp(address(pool), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
    }

    // ------------------------------------------------------------------ happy path

    function test_withdrawThroughEntryPoint_paysFeeAndConsumesLane() public {
        uint192 key = LANE;
        uint256 balBefore = address(account).balance;
        uint256 depBefore = ep.balanceOf(address(account));
        handle(validOp());

        assertTrue(pool.isSpent(NULLIFIER), "nullifier spent");
        assertEq(recipient.balance, 1 ether - FEE, "recipient paid");
        assertEq(ep.getNonce(address(account), key), (uint256(key) << 64) | 1, "lane consumed");
        // 手续费到账；gas 只从押金扣；本合约余额不变
        assertEq(rewardAccount.balance, FEE, "fee paid to rewardAccount");
        assertLt(ep.balanceOf(address(account)), depBefore, "gas paid from deposit");
        assertEq(address(account).balance, balBefore, "account balance untouched");
        assertGt(bundler.balance, 0, "bundler compensated");
    }

    function test_replayOfSameLaneIsRejected() public {
        EPUserOp memory op = validOp();
        handle(op);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA25 invalid account nonce"));
        handle(op);
    }

    function test_paysFromEntryPointDepositWhenAccountEmpty() public {
        vm.deal(address(account), 0);
        handle(validOp());
        assertTrue(pool.isSpent(NULLIFIER));
        assertEq(address(account).balance, 0, "gas paid from deposit only");
        assertEq(rewardAccount.balance, FEE, "fee paid to rewardAccount");
    }

    // ------------------------------------------------------------------ 押金：不自动补交，手工存入

    function drainDeposit() internal {
        uint256 dep = ep.balanceOf(address(account));
        vm.prank(admin);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.EntryPointWithdrawn(admin, dep);
        account.withdrawEntryPoint(payable(admin), dep);
        assertEq(ep.balanceOf(address(account)), 0);
    }

    /// 押金不足时自动从本合约余额补交差额；结算后的退款留在押金里。
    function test_autoTopUpFromAccountBalance() public {
        drainDeposit();
        uint256 bal = address(account).balance;
        EPUserOp memory op = validOp();
        uint256 prefund = (200_000 + 300_000 + 50_000) * 2 gwei; // (VGL + CGL + PVG) × maxFeePerGas
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
        assertEq(address(account).balance, bal - prefund, "missingAccountFunds paid from balance");
        assertGt(ep.balanceOf(address(account)), 0, "unused prefund refunded to deposit");
        assertLt(ep.balanceOf(address(account)), prefund, "actual gas charged from deposit");
    }

    /// 押金只够一部分时，只补交差额。
    function test_autoTopUpPaysOnlyShortfall() public {
        drainDeposit();
        uint256 prefund = (200_000 + 300_000 + 50_000) * 2 gwei;
        vm.prank(admin);
        account.depositEntryPoint(prefund / 4);
        uint256 bal = address(account).balance;
        handle(validOp());
        assertEq(address(account).balance, bal - (prefund - prefund / 4), "only shortfall paid");
    }

    /// 押金和本合约余额都不足时，EntryPoint 以 AA21 拒绝。
    function test_rejectsWhenDepositAndBalanceInsufficient() public {
        drainDeposit();
        vm.deal(address(account), 1);
        EPUserOp memory op = validOp();
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA21 didn't pay prefund"));
        handle(op);
        assertFalse(pool.isSpent(NULLIFIER));
    }

    /// owner 把本合约余额存入押金后即可提现。
    function test_ownerAddDepositThenWithdraw() public {
        drainDeposit();
        uint256 bal = address(account).balance;
        vm.prank(admin);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.EntryPointDeposited(0.2 ether);
        account.depositEntryPoint(0.2 ether);
        assertEq(ep.balanceOf(address(account)), 0.2 ether);
        assertEq(address(account).balance, bal - 0.2 ether);
        handle(validOp());
        assertTrue(pool.isSpent(NULLIFIER));
        assertEq(address(account).balance, bal - 0.2 ether, "deposit sufficient: no auto top-up");
    }

    function test_depositEntryPointOnlyOwnerAndBalanceBound() public {
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.depositEntryPoint(1);
        uint256 bal = address(account).balance;
        vm.prank(admin);
        vm.expectRevert(); // 余额不足
        account.depositEntryPoint(bal + 1);
    }

    /// 任何第三方都可以直接通过 EntryPoint.depositTo 为本合约存押金。
    function test_thirdPartyCanDepositForAccount() public {
        drainDeposit();
        address stranger = makeAddr("stranger");
        vm.deal(stranger, 1 ether);
        vm.prank(stranger);
        ep.depositTo{value: 0.1 ether}(address(account));
        assertEq(ep.balanceOf(address(account)), 0.1 ether);
        handle(validOp());
        assertTrue(pool.isSpent(NULLIFIER));
    }

    /// 直接向 EntryPoint 转账记入的是发送者自己的押金，不是本合约。
    function test_plainTransferToEntryPointCreditsSender() public {
        drainDeposit();
        address stranger = makeAddr("stranger");
        vm.deal(stranger, 1 ether);
        vm.prank(stranger);
        (bool ok,) = address(ep).call{value: 0.1 ether}("");
        assertTrue(ok);
        assertEq(ep.balanceOf(stranger), 0.1 ether);
        assertEq(ep.balanceOf(address(account)), 0);
    }

    // ------------------------------------------------------------------ policy rejections (validation phase, no gas burnt)

    function test_rejectsWrongRelayer() public {
        EPUserOp memory op = buildOp(address(pool), withdrawData(makeAddr("attacker"), 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
        assertFalse(pool.isSpent(NULLIFIER));
    }

    function test_rejectsPoolNotWhitelisted() public {
        EPUserOp memory op = buildOp(address(otherPool), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
    }

    /// nonce 通道不由合约强制：任意 key 都可用；EntryPoint 只保证 (key, 序号) 不重复（AA25）。
    function test_nonceKeyNotEnforcedButSequenceIs() public {
        EPUserOp memory op = buildOp(address(pool), withdrawData(rewardAccount, 0, NULLIFIER), 0);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA25 invalid account nonce"));
        handle(op);
    }

    /// 签名长度不单独检查：不足 77 字节时切片越界 revert（AA23）；多余字节被忽略。
    function test_signatureLengthHandledBySlicing() public {
        EPUserOp memory op = validOp();
        bytes memory full = op.signature;
        bytes memory shortSig = new bytes(76);
        for (uint256 i = 0; i < 76; i++) {
            shortSig[i] = full[i];
        }
        op.signature = shortSig;
        vm.expectRevert();
        handle(op);
        op.signature = bytes.concat(full, hex"00");
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    function test_rejectsArbitraryCallData() public {
        EPUserOp memory op = validOp();
        op.callData = abi.encodeCall(TornadoRelayerAccount.withdrawNative, (payable(address(0xBEEF)), 1 ether));
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
    }

    function test_rejectsWrongSigner() public {
        EPUserOp memory op = validOp();
        sign(op, 0xBAD, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    function test_rejectsExpired() public {
        EPUserOp memory op = validOp();
        sign(op, ownerKey, ago(), uint48(block.timestamp + 10 minutes));
        vm.warp(block.timestamp + 11 minutes);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA22 expired or not due"));
        handle(op);
    }

    function test_rejectsHighS() public {
        EPUserOp memory op = validOp();
        bytes32 h = ep.getUserOpHash(op);
        uint48 until = uint48(block.timestamp + 1 hours);
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(ownerKey, account.signingDigest(h, ago(), until));
        uint256 n = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141;
        bytes32 highS = bytes32(n - uint256(s));
        uint8 flippedV = v == 27 ? 28 : 27;
        op.signature = abi.encodePacked(bytes6(ago()), bytes6(until), r, highS, flippedV);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    // ------------------------------------------------------------------ access control

    function test_directCallsRejected() public {
        bytes memory data = withdrawData(rewardAccount, 0, NULLIFIER);
        vm.expectRevert(TornadoRelayerAccount.NotEntryPoint.selector);
        account.relayWithdraw(address(pool), data);
        vm.prank(owner);
        vm.expectRevert(TornadoRelayerAccount.NotEntryPoint.selector);
        account.relayWithdraw(address(pool), data);
    }

    function test_signerCannotMoveFunds() public {
        vm.startPrank(owner); // 热签名私钥地址
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.withdrawNative(payable(owner), 1);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.withdrawEntryPoint(payable(owner), 1);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.withdrawToken(IERC20(address(1)), owner, 1);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.setSigner(makeAddr("evil"), true);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.setGasCaps(1, 1, 1, 1);
        vm.stopPrank();
    }

    function test_ownerWithdrawsToAnyAddress() public {
        vm.deal(funder, 1 ether);
        vm.prank(funder);
        ep.depositTo{value: 0.3 ether}(address(account));
        uint256 depBefore = ep.balanceOf(address(account));
        address payable dest1 = payable(makeAddr("dest1"));
        address payable dest2 = payable(makeAddr("dest2"));

        vm.startPrank(admin);
        account.withdrawNative(dest1, 0.4 ether);
        account.withdrawEntryPoint(dest2, 0.3 ether);
        account.setSigner(makeAddr("newSigner"), true);
        vm.stopPrank();

        assertEq(dest1.balance, 0.4 ether);
        assertEq(dest2.balance, 0.3 ether);
        assertEq(ep.balanceOf(address(account)), depBefore - 0.3 ether);
        assertTrue(account.isSigner(makeAddr("newSigner")));
        assertEq(account.signers().length, 3);
    }

    function test_revokedSignerRejected() public {
        vm.prank(admin);
        account.setSigner(owner, false);
        EPUserOp memory op = validOp();
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    /// Measures the 4337 overhead versus a direct EOA call to the pool.
    function test_gasOverhead() public {
        bytes memory data = withdrawData(rewardAccount, 0, keccak256("direct"));
        uint256 g0 = gasleft();
        (bool ok,) = address(pool).call(data);
        uint256 direct = g0 - gasleft();
        assertTrue(ok);

        EPUserOp memory op = validOp();
        vm.recordLogs();
        handle(op);
        Vm.Log[] memory logs = vm.getRecordedLogs();
        uint256 actualGasUsed;
        bytes32 topic = keccak256("UserOperationEvent(bytes32,address,address,uint256,bool,uint256,uint256)");
        for (uint256 i = 0; i < logs.length; i++) {
            if (logs[i].topics[0] == topic) {
                (,,, actualGasUsed) = abi.decode(logs[i].data, (uint256, bool, uint256, uint256));
            }
        }
        emit log_named_uint("direct pool call gas (excl. 21000 intrinsic)", direct);
        emit log_named_uint("UserOp actualGasUsed (incl. preVerificationGas)", actualGasUsed);
    }

    // ------------------------------------------------------------------ signers / rewardAccount

    function test_secondSignerAccepted() public {
        EPUserOp memory op = buildOp(address(pool), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(op, signer2Key, ago(), uint48(block.timestamp + 1 hours));
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
        assertEq(rewardAccount.balance, FEE);
    }

    function test_rejectsRelayerEqualToAccountItself() public {
        EPUserOp memory op = buildOp(address(pool), withdrawData(address(account), 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
        assertFalse(pool.isSpent(NULLIFIER));
    }

    function test_malformedSignatureNeverMatchesZeroAddress() public {
        EPUserOp memory op = validOp();
        uint48 until = uint48(block.timestamp + 1 hours);
        op.signature = abi.encodePacked(bytes6(ago()), bytes6(until), bytes32(0), bytes32(0), uint8(27));
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    /// 允许空签名者列表部署（之后由 owner 添加）；零地址签名者始终被拒绝。
    function test_constructorAndSignerGuards() public {
        address[4] memory pools = fourPools();
        address[] memory none = new address[](0);
        TornadoRelayerAccount a =
            new TornadoRelayerAccount(IEntryPointMinimal(address(ep)), admin, rewardAccount, none, pools, caps());
        assertEq(a.signers().length, 0);

        address[] memory zero = new address[](1);
        vm.expectRevert(TornadoRelayerAccount.ZeroAddress.selector);
        new TornadoRelayerAccount(IEntryPointMinimal(address(ep)), admin, rewardAccount, zero, pools, caps());

        vm.prank(admin);
        vm.expectRevert(TornadoRelayerAccount.ZeroAddress.selector);
        account.setSigner(address(0), true);
    }

    // ------------------------------------------------------------------ owner / rewardAccount

    function test_twoStepOwnershipTransfer() public {
        address newAdmin = makeAddr("newAdmin");
        vm.prank(admin);
        account.transferOwnership(newAdmin);
        assertEq(account.owner(), admin, "owner unchanged until accepted");
        assertEq(account.pendingOwner(), newAdmin);

        vm.prank(makeAddr("stranger"));
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, makeAddr("stranger")));
        account.acceptOwnership();

        vm.prank(newAdmin);
        account.acceptOwnership();
        assertEq(account.owner(), newAdmin);
        assertEq(account.pendingOwner(), address(0));

        vm.prank(admin);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, admin));
        account.setSigner(admin, true);
    }

    function test_ownershipProposalCanBeCancelled() public {
        address newAdmin = makeAddr("newAdmin");
        vm.startPrank(admin);
        account.transferOwnership(newAdmin);
        account.transferOwnership(address(0));
        vm.stopPrank();
        vm.prank(newAdmin);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, newAdmin));
        account.acceptOwnership();
        assertEq(account.owner(), admin);
    }

    function test_onlyOwnerCanChangeRewardAccountOrTransfer() public {
        vm.startPrank(owner); // hot signer key
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.setRewardAccount(makeAddr("x"));
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.transferOwnership(owner);
        vm.stopPrank();
        vm.prank(funder);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, funder));
        account.setRewardAccount(makeAddr("x"));
    }

    function test_changedRewardAccount() public {
        address payable newReward = payable(makeAddr("newReward"));
        EPUserOp memory oldOp = validOp(); // proof names the old rewardAccount
        vm.prank(admin);
        account.setRewardAccount(newReward);
        assertEq(account.rewardAccount(), newReward);

        vm.expectRevert();
        handle(oldOp);
        assertFalse(pool.isSpent(NULLIFIER));

        EPUserOp memory op = buildOp(address(pool), withdrawData(newReward, 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        handle(op);
        assertEq(newReward.balance, FEE);
        assertEq(rewardAccount.balance, 0);
    }

    // ------------------------------------------------------------------ re-entrancy / malicious recipient

    function opFor(address payable to, bytes32 nullifier)
        internal
        view
        returns (EPUserOp memory op, bytes memory data)
    {
        data = abi.encodeWithSelector(
            MockTornadoPool.withdraw.selector,
            new bytes(256),
            bytes32(uint256(1)),
            nullifier,
            to,
            rewardAccount,
            FEE,
            uint256(0)
        );
        op = buildOp(address(pool), data, LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
    }

    function test_maliciousRecipientCannotReenter() public {
        vm.deal(address(account), 0);
        vm.deal(funder, 1 ether);
        vm.prank(funder);
        ep.depositTo{value: 0.5 ether}(address(account));

        ReentrantRecipient evil = new ReentrantRecipient();
        bytes32 n = keccak256("evil");
        (EPUserOp memory op, bytes memory data) = opFor(payable(address(evil)), n);
        evil.arm(account, ep, pool, data);
        uint256 depositBefore = ep.balanceOf(address(account));

        handle(op);

        assertTrue(evil.hit(), "recipient callback ran");
        assertTrue(pool.isSpent(n), "withdraw itself succeeded");
        assertFalse(evil.reenteredAccount(), "relayWithdraw is onlyEntryPoint");
        assertFalse(evil.reenteredEntryPoint(), "EntryPoint handleOps is nonReentrant");
        assertFalse(evil.reenteredPool(), "pool is nonReentrant / nullifier already spent");
        assertFalse(evil.changedConfig(), "admin is onlyOwner");
        assertFalse(evil.pulledDeposit(), "deposit cannot be pulled");
        assertEq(account.rewardAccount(), rewardAccount);
        assertEq(address(evil).balance, 1 ether - FEE, "recipient got exactly its amount");
        assertEq(rewardAccount.balance, FEE, "fee paid once");
        assertLt(ep.balanceOf(address(account)), depositBefore, "only gas was charged");
    }

    /// Residual risk: a recipient that reverts or burns gas at execution time
    /// makes the withdraw revert while the account still pays the gas.
    function test_gasBurningRecipientCostsGasButMovesNoFunds() public {
        GasBurnerRecipient burner = new GasBurnerRecipient();
        bytes32 n = keccak256("burn");
        (EPUserOp memory op,) = opFor(payable(address(burner)), n);
        uint192 key = LANE;
        uint256 accountFunds = address(account).balance + ep.balanceOf(address(account));

        vm.recordLogs();
        handle(op);
        Vm.Log[] memory logs = vm.getRecordedLogs();
        bool success = true;
        uint256 gasCost;
        bytes32 topic = keccak256("UserOperationEvent(bytes32,address,address,uint256,bool,uint256,uint256)");
        for (uint256 i = 0; i < logs.length; i++) {
            if (logs[i].topics[0] == topic) {
                (, success, gasCost,) = abi.decode(logs[i].data, (uint256, bool, uint256, uint256));
            }
        }
        assertFalse(success, "inner withdraw reverted");
        assertFalse(pool.isSpent(n), "nullifier not spent");
        assertEq(rewardAccount.balance, 0, "no fee received");
        assertEq(ep.getNonce(address(account), key), (uint256(key) << 64) | 1, "lane consumed");
        assertEq(
            accountFunds - (address(account).balance + ep.balanceOf(address(account))), gasCost, "account paid gas only"
        );
        emit log_named_uint("gas cost paid for the reverted op (wei)", gasCost);
    }

    // ------------------------------------------------------------------ H-1: gas caps

    function signedWithGas(uint256 pvg, uint256 vgl, uint256 cgl, uint256 priority, uint256 maxFee, bytes32 n)
        internal
        view
        returns (EPUserOp memory op)
    {
        op = buildOp(address(pool), withdrawData(rewardAccount, 0, n), LANE);
        op.preVerificationGas = pvg;
        op.accountGasLimits = bytes32((vgl << 128) | cgl);
        op.gasFees = bytes32((priority << 128) | maxFee);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
    }

    function expectPolicyRevert(EPUserOp memory op, bytes memory inner) internal {
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOpWithRevert.selector, 0, "AA23 reverted", inner));
        handle(op);
    }

    function test_atCapsAccepted() public {
        handle(signedWithGas(CAP_PVG, CAP_VGL, CAP_CGL, 1 gwei, CAP_FEE, NULLIFIER));
        assertTrue(pool.isSpent(NULLIFIER));
    }

    function test_rejectsMaxFeeAboveCap() public {
        expectPolicyRevert(
            signedWithGas(50_000, 200_000, 300_000, 1 gwei, uint256(CAP_FEE) + 1, NULLIFIER),
            abi.encodeWithSelector(TornadoRelayerAccount.MaxFeePerGasTooHigh.selector, uint256(CAP_FEE) + 1, CAP_FEE)
        );
    }

    function test_rejectsPreVerificationGasAboveCap() public {
        expectPolicyRevert(
            signedWithGas(uint256(CAP_PVG) + 1, 200_000, 300_000, 1 gwei, 2 gwei, NULLIFIER),
            abi.encodeWithSelector(
                TornadoRelayerAccount.PreVerificationGasTooHigh.selector, uint256(CAP_PVG) + 1, CAP_PVG
            )
        );
    }

    function test_rejectsVerificationGasAboveCap() public {
        expectPolicyRevert(
            signedWithGas(50_000, uint256(CAP_VGL) + 1, 300_000, 1 gwei, 2 gwei, NULLIFIER),
            abi.encodeWithSelector(TornadoRelayerAccount.VerificationGasTooHigh.selector, uint256(CAP_VGL) + 1, CAP_VGL)
        );
    }

    function test_rejectsCallGasAboveCap() public {
        expectPolicyRevert(
            signedWithGas(50_000, 200_000, uint256(CAP_CGL) + 1, 1 gwei, 2 gwei, NULLIFIER),
            abi.encodeWithSelector(TornadoRelayerAccount.CallGasTooHigh.selector, uint256(CAP_CGL) + 1, CAP_CGL)
        );
    }

    /// Leaked signer + own bundler as beneficiary, base fee 0 so the whole
    /// effective price is priority fee that the bundler pockets.
    function test_leakedSignerFakeBundlerExtractionIsBounded() public {
        address payable attacker = payable(makeAddr("attackerBundler"));
        vm.fee(0);
        vm.deal(address(account), 0);
        vm.deal(funder, 10 ether);
        vm.prank(funder);
        ep.depositTo{value: 5 ether}(address(account));

        // 1) Over-cap variants are refused before any charge.
        EPUserOp[] memory ops = new EPUserOp[](1);
        ops[0] = signedWithGas(10_000_000, CAP_VGL, CAP_CGL, 10_000 gwei, 10_000 gwei, keccak256("x1"));
        vm.prank(attacker, attacker);
        vm.expectRevert();
        ep.handleOps(ops, attacker);
        ops[0] = signedWithGas(CAP_PVG, CAP_VGL, CAP_CGL, 10_000 gwei, 10_000 gwei, keccak256("x2"));
        vm.prank(attacker, attacker);
        vm.expectRevert();
        ep.handleOps(ops, attacker);
        assertEq(attacker.balance, 0);

        // 2) Max allowed op with a junk proof (fresh lane, pool reverts): bounded loss.
        uint256 before = ep.balanceOf(address(account));
        bytes32 n = keccak256("junk");
        bytes memory junk = abi.encodeWithSelector(
            MockTornadoPool.withdraw.selector, new bytes(1), bytes32(0), n, recipient, rewardAccount, FEE, uint256(0)
        );
        EPUserOp memory op = buildOp(address(pool), junk, LANE);
        op.preVerificationGas = CAP_PVG;
        op.accountGasLimits = bytes32((uint256(CAP_VGL) << 128) | CAP_CGL);
        op.gasFees = bytes32((uint256(CAP_FEE) << 128) | CAP_FEE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        ops[0] = op;
        vm.prank(attacker, attacker);
        ep.handleOps(ops, attacker);

        uint256 lost = before - ep.balanceOf(address(account));
        uint256 bound = (uint256(CAP_PVG) + CAP_VGL + CAP_CGL) * CAP_FEE;
        assertEq(attacker.balance, lost, "everything lost went to the fake bundler");
        assertGt(lost, 0);
        assertLe(lost, bound, "per-op extraction bounded by caps");
        emit log_named_uint("max extraction per op (wei), bound", bound);
        emit log_named_uint("actual extraction (wei)", lost);
    }

    function test_setGasCapsOnlyOwnerNonZero() public {
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.setGasCaps(1, 1, 1, 1);

        vm.startPrank(admin);
        vm.expectRevert(TornadoRelayerAccount.ZeroLimit.selector);
        account.setGasCaps(0, 1, 1, 1);
        account.setGasCaps(5 gwei, 60_000, 250_000, 400_000);
        vm.stopPrank();
        (uint96 fee, uint32 pvg, uint32 vgl, uint32 cgl) = account.config();
        assertEq(fee, 5 gwei);
        assertEq(pvg, 60_000);
        assertEq(vgl, 250_000);
        assertEq(cgl, 400_000);
    }

    // ------------------------------------------------------------------ 已删除的检查：交给 EntryPoint / Go 端

    /// 不检查 initCode：v0.9 对已部署账户直接忽略 initCode（发 IgnoredInitCode），提现照常完成。
    function test_initCodeIgnoredByEntryPoint() public {
        EPUserOp memory op = validOp();
        op.initCode = abi.encodePacked(address(0xdead), hex"01");
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    /// 不检查时间窗合法性：空 / 倒置的时间窗由 EntryPoint 以 AA22 拒绝（账户不付 gas）。
    function test_invertedTimeWindowRejectedByEntryPoint() public {
        uint48 t0 = uint48(block.timestamp + 100);
        EPUserOp memory op = validOp();
        sign(op, ownerKey, t0, t0);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA22 expired or not due"));
        handle(op);
        op = validOp();
        sign(op, ownerKey, ago(), ago() - 1);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA22 expired or not due"));
        handle(op);
        assertFalse(pool.isSpent(NULLIFIER));
    }

    /// 不检查时间窗：validUntil = 0（永不过期）或很长的窗口都会被放行——Go 端必须自己保证。
    function test_timeWindowNotCheckedByAccount() public {
        EPUserOp memory op = validOp();
        sign(op, ownerKey, ago(), 0);
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    /// 撤销全部签名者即可替代暂停：所有 UserOp 以 AA24 拒绝，加回后恢复。
    function test_revokeAllSignersActsAsPause() public {
        EPUserOp memory op = validOp();
        vm.startPrank(admin);
        account.setSigner(owner, false);
        account.setSigner(signer2, false);
        vm.stopPrank();
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
        vm.prank(admin);
        account.setSigner(owner, true);
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    function toAccountOp(EPUserOp memory op) internal pure returns (PackedUserOperation memory a) {
        a = PackedUserOperation(
            op.sender,
            op.nonce,
            op.initCode,
            op.callData,
            op.accountGasLimits,
            op.preVerificationGas,
            op.gasFees,
            op.paymasterAndData,
            op.signature
        );
    }

    // ------------------------------------------------------------------ L-1 / L-2

    function test_signingDigestIsRawHash() public view {
        bytes32 h = keccak256("op");
        assertEq(account.signingDigest(h, 1, 2), keccak256(abi.encode(h, uint48(1), uint48(2))));
    }

    /// 加了 EIP-191 / EIP-712 前缀的签名都不被接受，只认原始摘要。
    function test_rejectsPrefixedSignatures() public {
        EPUserOp memory op = validOp();
        uint48 until = uint48(block.timestamp + 1 hours);
        bytes32 raw = keccak256(abi.encode(ep.getUserOpHash(op), ago(), until));
        bytes32 prefixed = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", raw));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(ownerKey, prefixed);
        op.signature = abi.encodePacked(bytes6(ago()), bytes6(until), r, s, v);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    /// 签名同时绑定时间窗：改动 validUntil 后原签名失效。
    function test_tamperedTimeWindowRejected() public {
        EPUserOp memory op = validOp();
        uint48 until = uint48(block.timestamp + 1 hours);
        sign(op, ownerKey, ago(), until);
        bytes memory sig = op.signature;
        sig[11] = bytes1(uint8(sig[11]) + 1); // validUntil 最低字节 +1
        op.signature = sig;
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    function test_withdrawTokenRequiresContract() public {
        vm.prank(admin);
        vm.expectRevert(abi.encodeWithSelector(SafeERC20.SafeERC20FailedOperation.selector, makeAddr("eoa")));
        account.withdrawToken(IERC20(makeAddr("eoa")), admin, 1);
    }

    // ------------------------------------------------------------------ calldata parsing

    function test_rejectsOutOfBoundsBytesOffset() public {
        EPUserOp memory op = validOp();
        op.callData = abi.encodePacked(
            TornadoRelayerAccount.relayWithdraw.selector, uint256(uint160(address(pool))), uint256(0x1000)
        );
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
    }

    function test_rejectsDirtyPoolAddress() public {
        EPUserOp memory op = validOp();
        bytes memory cd = op.callData;
        cd[4] = 0x01; // high byte of the pool word
        op.callData = cd;
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
    }

    function test_rejectsTruncatedWithdrawData() public {
        bytes memory shortData = abi.encodePacked(bytes4(0x21a0adb6), new bytes(100));
        EPUserOp memory op = buildOp(address(pool), shortData, 0);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(op);
    }

    // ------------------------------------------------------------------ gas measurement (run with --isolate)

    function laneOp(uint192 key, bytes32 nullifier) internal view returns (EPUserOp memory op) {
        op = buildOp(address(pool), withdrawData(rewardAccount, 0, nullifier), key);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
    }

    function handleAndLogUsed(EPUserOp memory op, string memory label) internal {
        vm.recordLogs();
        handle(op);
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 i = 0; i < logs.length; i++) {
            if (logs[i].topics[0] == IEntryPoint.UserOperationEvent.selector) {
                (,,, uint256 used) = abi.decode(logs[i].data, (uint256, bool, uint256, uint256));
                emit log_named_uint(label, used);
            }
        }
    }

    /// 生产场景：复用通道。先用一次 LANE（序号 0 → 1；同时 bundler 收款地址从 0 余额变为非 0，约多 25k），
    /// 再测同一通道下一笔（序号 1 → 2，非 0 → 非 0 写入）。
    /// --gas-report 中 handleOps 的 Min = 复用通道的稳态基准（实测 128,586）；
    /// 对照：收款地址已有余额但通道首次使用约 145.7k（多出的 ~17k 即新建序号槽）。
    function test_gas_standardWithdraw() public {
        vm.deal(rewardAccount, 1); // existing account, like production
        handleAndLogUsed(laneOp(LANE, keccak256("warm-up")), "actualGasUsed (lane first use)");
        handleAndLogUsed(validOp(), "actualGasUsed (reused lane)");
        assertEq(uint64(ep.getNonce(address(account), LANE)), 2);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    /// 4 条通道轮流复用，每条通道序号逐步递增；同一 (key, 序号) 不能执行两次，也不能跳号。
    function test_fourLanesRotate() public {
        for (uint256 round = 0; round < 2; round++) {
            for (uint192 key = 0; key < 4; key++) {
                EPUserOp memory op = laneOp(key, keccak256(abi.encode(round, key)));
                assertEq(uint64(op.nonce), round, "sequence from EntryPoint");
                handle(op);
                vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA25 invalid account nonce"));
                handle(op); // 重放
            }
        }
        for (uint192 key = 0; key < 4; key++) {
            assertEq(uint64(ep.getNonce(address(account), key)), 2);
        }
        // 跳号：通道 0 当前序号为 2，直接用 3 被拒绝
        EPUserOp memory skip = buildOp(address(pool), withdrawData(rewardAccount, 0, keccak256("skip")), 0);
        skip.nonce += 1;
        sign(skip, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA25 invalid account nonce"));
        handle(skip);
    }

    /// 池执行失败时序号照样被消耗（Go 端必须重新读取 getNonce）；验证失败时序号不被消耗。
    function test_laneSequenceOnFailures() public {
        handle(validOp());
        // 执行失败：同一 nullifier 再提一次，池 revert，但序号 1 → 2
        handle(laneOp(LANE, NULLIFIER));
        assertEq(uint64(ep.getNonce(address(account), LANE)), 2, "consumed on execution failure");
        // 验证失败：错误签名者，整笔回滚，序号不变
        EPUserOp memory bad = buildOp(address(pool), withdrawData(rewardAccount, 0, keccak256("x")), LANE);
        sign(bad, uint256(keccak256("not a signer")), ago(), uint48(block.timestamp + 1 hours));
        vm.expectRevert();
        handle(bad);
        assertEq(uint64(ep.getNonce(address(account), LANE)), 2, "not consumed on validation failure");
    }

    // ------------------------------------------------------------------ 时间窗：validAfter 必须小于 validUntil

    function test_futureValidAfterRejectedUntilReached() public {
        uint48 after_ = uint48(block.timestamp + 60);
        EPUserOp memory op = validOp();
        sign(op, ownerKey, after_, after_ + 3600);
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA22 expired or not due"));
        handle(op);
        vm.warp(after_); // v0.9: validAfter 为开区间，等于 validAfter 时仍未生效
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA22 expired or not due"));
        handle(op);
        vm.warp(after_ + 1);
        handle(op);
        assertTrue(pool.isSpent(NULLIFIER));
    }

    // ------------------------------------------------------------------ OpenZeppelin 组件

    function test_withdrawTokenToAnyAddress() public {
        TestToken token = new TestToken();
        token.mint(address(account), 100);
        address dest = makeAddr("tokenDest");
        vm.prank(admin);
        account.withdrawToken(IERC20(address(token)), dest, 60);
        assertEq(token.balanceOf(dest), 60);
        assertEq(token.balanceOf(address(account)), 40);
    }

    function test_withdrawToZeroAddressRejected() public {
        vm.startPrank(admin);
        vm.expectRevert(TornadoRelayerAccount.ZeroAddress.selector);
        account.withdrawNative(payable(address(0)), 1);
        vm.expectRevert(TornadoRelayerAccount.ZeroAddress.selector);
        account.withdrawEntryPoint(payable(address(0)), 1);
        vm.expectRevert(TornadoRelayerAccount.ZeroAddress.selector);
        account.withdrawToken(IERC20(address(1)), address(0), 1);
        vm.stopPrank();
    }

    function test_renounceOwnershipDisabled() public {
        vm.prank(admin);
        vm.expectRevert(TornadoRelayerAccount.RenounceDisabled.selector);
        account.renounceOwnership();
        assertEq(account.owner(), admin);
        vm.prank(owner);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, owner));
        account.renounceOwnership();
    }

    function test_zeroOwnerRejectedByOz() public {
        address[] memory one = new address[](1);
        one[0] = owner;
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableInvalidOwner.selector, address(0)));
        new TornadoRelayerAccount(IEntryPointMinimal(address(ep)), address(0), rewardAccount, one, fourPools(), caps());
    }

    // ------------------------------------------------------------------ EntryPoint v0.9 特性

    /// v0.9 只允许 EOA 直接调用 handleOps：合约（包括恶意 recipient）无法调用。
    function test_handleOpsOnlyFromEoa() public {
        EPUserOp[] memory ops = new EPUserOp[](1);
        ops[0] = validOp();
        vm.prank(address(pool)); // 有代码的地址
        vm.expectRevert(EntryPoint.Reentrancy.selector);
        ep.handleOps(ops, bundler);
    }

    /// userOpHash 是 EIP-712 哈希（v0.8 起），域为 ERC4337 / 1 / chainId / EntryPoint。
    function test_userOpHashUsesEntryPointDomain() public view {
        (, string memory name, string memory version, uint256 chainId, address verifying,,) = ep.eip712Domain();
        assertEq(name, "ERC4337");
        assertEq(version, "1");
        assertEq(chainId, block.chainid);
        assertEq(verifying, address(ep));
    }

    // ------------------------------------------------------------------ 第二轮审查修改

    /// v 只接受 27/28：同一签名改写为 0/1 后被拒绝（签名编码唯一）。
    function test_rejectsV01() public {
        EPUserOp memory op = validOp();
        bytes memory sig = op.signature;
        sig[76] = bytes1(uint8(sig[76]) - 27);
        op.signature = sig;
        vm.expectRevert(abi.encodeWithSelector(IEntryPoint.FailedOp.selector, 0, "AA24 signature error"));
        handle(op);
    }

    /// 签名者可链上枚举；重复设置不改变集合；固定池可查询。
    function test_enumerableSignersAndPools() public {
        address[] memory s = account.signers();
        assertEq(s.length, 2);
        assertEq(s[0], owner);
        assertEq(s[1], signer2);
        vm.startPrank(admin);
        account.setSigner(owner, true); // 重复添加
        account.setSigner(signer2, false);
        account.setSigner(signer2, false); // 重复撤销
        vm.stopPrank();
        assertEq(account.signers().length, 1);
        assertFalse(account.isSigner(signer2));
        address[4] memory p = account.pools();
        assertEq(p[0], address(pool));
        assertEq(p[1], address(pool3));
        assertEq(p[2], address(pool4));
        assertTrue(account.isPoolAllowed(address(pool4)));
        assertFalse(account.isPoolAllowed(address(otherPool)));
    }

    /// 池 revert 时，原因原样记录在 UserOperationRevertReason 中（OZ Address.functionCall）。
    function test_poolRevertReasonBubbled() public {
        EPUserOp memory op = validOp();
        handle(op); // 第一次成功，nullifier 已花费
        bytes32 h;
        EPUserOp memory again = buildOp(address(pool), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(again, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        h = ep.getUserOpHash(again);
        vm.recordLogs();
        handle(again);
        Vm.Log[] memory logs = vm.getRecordedLogs();
        bool found;
        for (uint256 i = 0; i < logs.length; i++) {
            if (logs[i].topics[0] == IEntryPoint.UserOperationRevertReason.selector && logs[i].topics[1] == h) {
                (, bytes memory reason) = abi.decode(logs[i].data, (uint256, bytes));
                assertGt(reason.length, 0, "revert reason bubbled");
                found = true;
            }
        }
        assertTrue(found);
    }

    /// 部署时 4 个池都必须有代码（零地址同样被拒绝）。
    function test_setPoolRequiresCode() public {
        address eoa = makeAddr("eoaPool");
        address[] memory one = new address[](1);
        one[0] = owner;
        address[4] memory badPools = fourPools();
        badPools[2] = eoa;
        vm.expectRevert(abi.encodeWithSelector(TornadoRelayerAccount.PoolNotContract.selector, eoa));
        new TornadoRelayerAccount(IEntryPointMinimal(address(ep)), admin, rewardAccount, one, badPools, caps());
        badPools[2] = address(0);
        vm.expectRevert(abi.encodeWithSelector(TornadoRelayerAccount.PoolNotContract.selector, address(0)));
        new TornadoRelayerAccount(IEntryPointMinimal(address(ep)), admin, rewardAccount, one, badPools, caps());
    }

    // ------------------------------------------------------------------ 固定池

    /// 不是 4 个固定池之一：验证阶段 PoolNotAllowed。
    function test_rejectsNonFixedPool() public {
        MockTornadoPool fifth = new MockTornadoPool{value: 10 ether}();
        EPUserOp memory op = buildOp(address(fifth), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
        expectPolicyRevert(op, abi.encodeWithSelector(TornadoRelayerAccount.PoolNotAllowed.selector, address(fifth)));
    }

    function opFor(MockTornadoPool p) internal view returns (EPUserOp memory op) {
        op = buildOp(address(p), withdrawData(rewardAccount, 0, NULLIFIER), LANE);
        sign(op, ownerKey, ago(), uint48(block.timestamp + 1 hours));
    }

    /// 每个固定池（包括最后一个比较位置）都能正常提现。
    function test_allFixedPoolsWork() public {
        handle(opFor(pool4));
        assertTrue(pool4.isSpent(NULLIFIER));
        handle(opFor(pool3));
        assertTrue(pool3.isSpent(NULLIFIER));
    }

    // ------------------------------------------------------------------ 资金方向事件

    function test_fundFlowEvents() public {
        TestToken token = new TestToken();
        token.mint(address(account), 5);
        address payable dest = payable(makeAddr("flowDest"));
        vm.startPrank(admin);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.EntryPointDeposited(0.1 ether);
        account.depositEntryPoint(0.1 ether);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.EntryPointWithdrawn(dest, 0.05 ether);
        account.withdrawEntryPoint(dest, 0.05 ether);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.NativeWithdrawn(dest, 0.01 ether);
        account.withdrawNative(dest, 0.01 ether);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.TokenWithdrawn(address(token), dest, 5);
        account.withdrawToken(IERC20(address(token)), dest, 5);
        vm.expectEmit(address(account));
        emit TornadoRelayerAccount.SignerChanged(signer2, false);
        account.setSigner(signer2, false);
        vm.stopPrank();
        assertEq(dest.balance, 0.06 ether);
    }
}
