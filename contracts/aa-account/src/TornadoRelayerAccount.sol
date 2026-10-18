// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Ownable, Ownable2Step} from "@openzeppelin/contracts/access/Ownable2Step.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {IERC20, SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {Address} from "@openzeppelin/contracts/utils/Address.sol";
import {EnumerableSet} from "@openzeppelin/contracts/utils/structs/EnumerableSet.sol";

/// ERC-4337 的 PackedUserOperation 结构（v0.7 / v0.8 / v0.9 相同）。
/// 字段顺序和类型必须与 EntryPoint 完全一致，否则 validateUserOp 的选择器和 userOpHash 都会不同。
struct PackedUserOperation {
    address sender; // 发起方，即本账户合约地址
    uint256 nonce; // 高 192 位为 key（nonce 通道），低 64 位为序号
    bytes initCode; // 账户部署代码；本账户手动部署，不使用（v0.9 对已部署账户直接忽略）
    bytes callData; // 执行阶段 EntryPoint 调用本账户的数据，只允许 relayWithdraw
    bytes32 accountGasLimits; // 高 128 位 verificationGasLimit | 低 128 位 callGasLimit
    uint256 preVerificationGas; // 补偿 bundler 的固定开销（calldata 等），全额收取
    bytes32 gasFees; // 高 128 位 maxPriorityFeePerGas | 低 128 位 maxFeePerGas
    bytes paymasterAndData; // Paymaster 信息；本项目不用 Paymaster（合约不检查，由签名覆盖）
    bytes signature; // 本合约自定义格式：validAfter(6) | validUntil(6) | r | s | v
}

/// 本合约只用到 EntryPoint 的这几个函数。
interface IEntryPointMinimal {
    function depositTo(address account) external payable;
    function withdrawTo(address payable withdrawAddress, uint256 withdrawAmount) external;
    function balanceOf(address account) external view returns (uint256);
}

/// @title TornadoRelayerAccount
/// @notice Tornado Cash 中继专用的 ERC-4337 智能账户。
///
/// 角色：
///   - signer（热私钥，可多个）：只能签"提现" UserOp，无法直接转移资金。
///   - owner（冷钱包/多签）：管理签名者、gas 上限、rewardAccount，
///     并可把账户资金（余额、押金、代币）提取到任意地址；
///     owner 通过 OpenZeppelin Ownable2Step "提名 + 确认"两步转移，禁止放弃所有权。
///   - rewardAccount（owner 可修改）：提现参数 _relayer 必须等于它，手续费直接付给它。
///
/// 引用的 OpenZeppelin 组件：Ownable2Step（owner 两步转移）、ECDSA（签名恢复、low-s 检查）、
/// EnumerableSet（签名者集合，可链上枚举）、SafeERC20（兼容 USDT 的代币转账）、
/// Address（原生币转账）。
/// 没有暂停开关：紧急情况下由 owner 撤销全部签名者（效果相同，所有 UserOp 以 AA24 拒绝）。
/// 4 个 gas 上限打包在同一个槽里，验证阶段一次 SLOAD 读完。
///
/// 池地址：部署时写死 4 个（immutable，存在字节码里，验证阶段比较不需要 SLOAD）。
/// 部署后不能新增、替换或停用池；加池、换池需重新部署（紧急停止靠撤销签名者）。
///
/// 一笔 UserOp 被放行的条件（全部满足）：
///   1. gas 参数不超过 owner 设定的上限（initCode 不检查：v0.9 对已部署账户直接忽略）；
///   2. callData = relayWithdraw(pool, withdraw数据)，pool 是 4 个固定池之一（pool 字无脏高位），
///      _relayer == rewardAccount；
///   （_refund、_recipient 等由池合约和 zk proof 自己约束，本合约不重复检查；
///    检查只在验证阶段做一次，执行阶段不重复——两者在同一笔交易里，calldata 完全相同。）
///   3. 签名来自已授权的 signer（v 只能是 27/28）；时间窗 validAfter / validUntil 随签名一起签，
///      由 EntryPoint 强制（AA22）。本合约不检查时间窗的合法性和长度，Go 端必须保证：
///      validUntil != 0（0 = 永不过期）、validAfter < validUntil、使用时间戳（不得设置 2^47 区块号标志）、
///      窗口足够短。
///
/// 签名私钥泄露时的损失（重要）：
///   gas 上限只限制"单笔"最多被扣多少；攻击者可用任意 nonce key 构造无限多条通道，
///   并自建 bundler 批量提交、把 beneficiary 设为自己，最终可耗尽"EntryPoint 押金 + 账户余额"。
///   总损失只能靠：押金只存少量并定期补充、监控消耗、发现后 owner 撤销签名者。
///
/// nonce 通道（合约不检查，完全由 Go 端管理）：
///   nonce = key(192 位) << 64 | 序号(64 位)，EntryPoint 保证每个 (key, 序号) 只执行一次（AA25）且按序执行。
///   Go 端固定复用 4 条通道 key = 0..3（ERC-7562：未质押账户在内存池最多 4 笔 UserOp）：
///     - 每条通道同时最多 1 笔在途；序号一律以 EntryPoint.getNonce(本账户, key) 为准；
///     - 替换（提价）用相同 key + 序号重签；过期未上链的序号原样复用，不得跳号；
///     - 一个 nullifier 同时只签在一个 (key, 序号) 上；
///   复用通道时 EntryPoint 的序号槽是"非 0 → 非 0"写入（约 5k gas），每条通道只有首次使用付 22.1k。
///
/// 目标 EntryPoint：v0.9（BSC 地址 0x433709009B8330FDa32311DF1C2AFA402eD8D009），兼容 v0.7/v0.8。
///
/// gas 最终只从本账户在 EntryPoint 的押金中扣除。押金来源：
///   - 自动：押金不足时，validateUserOp 从本合约余额补交 EntryPoint 要求的差额（missingAccountFunds）；
///     本合约余额也不足时转账失败，EntryPoint 以 AA21 拒绝该 UserOp；
///   - 手工：owner 调用 depositEntryPoint，把本合约余额存入押金；
///   - 第三方：任何人直接调用 EntryPoint.depositTo{value: x}(本合约地址)。
/// 本账户没有手续费收入。
contract TornadoRelayerAccount is Ownable2Step {
    using SafeERC20 for IERC20;
    using EnumerableSet for EnumerableSet.AddressSet;

    // =====================================================================
    // 类型
    // =====================================================================

    /// 验证阶段每次都要读的配置，打包在同一个存储槽（共 192 位），一次 SLOAD 读完。
    /// 字段顺序决定位布局，必须与下面的 *_SHIFT 常量一致。
    struct Config {
        uint96 maxFeePerGas; // 位 [0,96)：userOp.maxFeePerGas 上限（wei）
        uint32 maxPreVerificationGas; // 位 [96,128)：userOp.preVerificationGas 上限
        uint32 maxVerificationGas; // 位 [128,160)：userOp.verificationGasLimit 上限
        uint32 maxCallGas; // 位 [160,192)：userOp.callGasLimit 上限
    }

    // =====================================================================
    // 常量
    // =====================================================================

    /// Tornado 池 withdraw(bytes,bytes32,bytes32,address,address,uint256,uint256) 的选择器。
    bytes4 internal constant POOL_WITHDRAW_SELECTOR = 0x21a0adb6;

    /// withdraw 调用数据的最小长度：4 字节选择器 + 7 个 32 字节的头部字。
    uint256 internal constant WITHDRAW_HEAD_LEN = 4 + 7 * 32;

    /// ERC-4337 约定：签名错误时返回 1（而不是 revert），便于 bundler 预估 gas。
    uint256 internal constant SIG_VALIDATION_FAILED = 1;

    /// Config 槽中各字段的位偏移。
    uint256 internal constant PVG_SHIFT = 96;
    uint256 internal constant VGL_SHIFT = 128;
    uint256 internal constant CGL_SHIFT = 160;

    // =====================================================================
    // 不可修改的配置（部署时写入字节码）
    // =====================================================================

    /// EntryPoint 地址（v0.9）；只有它能调用 validateUserOp 和 relayWithdraw。
    IEntryPointMinimal public immutable entryPoint;

    /// 4 个固定的 Tornado 池（部署后不可修改）。
    address internal immutable POOL_0;
    address internal immutable POOL_1;
    address internal immutable POOL_2;
    address internal immutable POOL_3;

    // =====================================================================
    // 存储（全部属于本账户自己，符合 ERC-7562 验证阶段的存储访问规则）
    // =====================================================================

    // owner / pendingOwner 由 OZ Ownable2Step 存储。

    /// gas 上限（验证阶段热数据，一个槽）。
    Config public config;

    /// 提现参数 _relayer 必须等于此地址，手续费付给它（验证阶段热数据）。
    address public rewardAccount;

    /// 授权签名者集合（OZ EnumerableSet：contains 只读一次存储，并可链上枚举）。
    EnumerableSet.AddressSet private _signers;

    // =====================================================================
    // 事件
    // =====================================================================

    // OwnershipTransferStarted / OwnershipTransferred 事件由 OZ Ownable2Step 定义。
    event RewardAccountChanged(address indexed previousRewardAccount, address indexed newRewardAccount);
    event SignerChanged(address indexed signer, bool allowed); // true = 已添加，false = 已撤销
    event GasCapsChanged(
        uint96 maxFeePerGas, uint32 maxPreVerificationGas, uint32 maxVerificationGas, uint32 maxCallGas
    );
    event NativeWithdrawn(address indexed to, uint256 amount); // 取出：本合约原生币余额 → to
    event TokenWithdrawn(address indexed token, address indexed to, uint256 amount); // 取出：本合约代币余额 → to
    event EntryPointDeposited(uint256 amount); // 存入：本合约余额 → EntryPoint 押金
    event EntryPointWithdrawn(address indexed to, uint256 amount); // 取出：EntryPoint 押金 → to

    // =====================================================================
    // 错误
    // =====================================================================

    error NotEntryPoint(); // 调用者不是 EntryPoint
    // 权限错误使用 OZ 的 OwnableUnauthorizedAccount(account) / OwnableInvalidOwner(owner)。
    error ZeroAddress(); // 提款目标地址或签名者为零地址
    error ZeroLimit(); // gas 上限不能为 0
    error BadCallData(); // callData / withdraw 数据格式不合法
    error PoolNotAllowed(address pool); // 不是 4 个固定池之一
    error RelayerMismatch(address relayer); // _relayer 不等于 rewardAccount
    error MaxFeePerGasTooHigh(uint256 value, uint256 cap);
    error PreVerificationGasTooHigh(uint256 value, uint256 cap);
    error VerificationGasTooHigh(uint256 value, uint256 cap);
    error CallGasTooHigh(uint256 value, uint256 cap);
    error RenounceDisabled(); // 禁止放弃所有权（否则资金和配置将永久无人管理）
    error PoolNotContract(address pool); // 部署时传入的池地址没有代码

    // =====================================================================
    // 修饰器
    // =====================================================================

    /// 只允许 EntryPoint 调用（验证和执行阶段的入口）。
    modifier onlyEntryPoint() {
        if (msg.sender != address(entryPoint)) revert NotEntryPoint();
        _;
    }

    // onlyOwner 修饰器由 OZ Ownable 提供。

    // =====================================================================
    // 构造函数
    // =====================================================================

    /// @param entryPoint_    EntryPoint v0.9 地址
    /// @param owner_         初始管理员（为 0 时 OZ Ownable 会 revert）
    /// @param rewardAccount_ 初始手续费接收地址
    /// @param signers_       初始签名者列表（可为空，部署后由 owner 添加）
    /// @param pools_         4 个固定池地址（必须都有代码；用不到的位置可填重复地址）
    /// @param initConfig     初始 gas 上限
    constructor(
        IEntryPointMinimal entryPoint_,
        address owner_,
        address rewardAccount_,
        address[] memory signers_,
        address[4] memory pools_,
        Config memory initConfig
    ) Ownable(owner_) {
        entryPoint = entryPoint_;
        // 没有代码的地址被调用时会"成功"但什么都不做，白白消耗 gas（零地址同样被拒绝）。
        for (uint256 i = 0; i < 4; i++) {
            if (pools_[i].code.length == 0) revert PoolNotContract(pools_[i]);
        }
        POOL_0 = pools_[0];
        POOL_1 = pools_[1];
        POOL_2 = pools_[2];
        POOL_3 = pools_[3];
        rewardAccount = rewardAccount_;
        emit RewardAccountChanged(address(0), rewardAccount_);
        _setGasCaps(
            initConfig.maxFeePerGas,
            initConfig.maxPreVerificationGas,
            initConfig.maxVerificationGas,
            initConfig.maxCallGas
        );
        for (uint256 i = 0; i < signers_.length; i++) {
            _setSigner(signers_[i], true);
        }
    }

    /// 接收原生币：作为押金的备用资金（押金不足时自动补交，或由 owner 调用 depositEntryPoint 存入）；
    /// 也用于接收 withdrawEntryPoint(本合约地址, x) 提回的押金。必须保持为空。
    receive() external payable {}

    // =====================================================================
    // ERC-4337 验证阶段
    // =====================================================================

    /// EntryPoint 在执行 UserOp 前调用，判断这笔 UserOp 是否可以放行。
    /// 不符合策略时直接 revert（bundler 拒收；若已上链则整笔 handleOps 回滚，由 bundler 承担 gas）；
    /// 签名错误时返回 SIG_VALIDATION_FAILED。
    /// 只读取本账户存储和 calldata，不使用禁用 opcode；时间窗通过返回值交给 EntryPoint 检查。
    /// @param userOp              待验证的 UserOp
    /// @param userOpHash          EntryPoint 计算的哈希（已包含 chainId、EntryPoint、账户地址和 nonce）
    /// @param missingAccountFunds 押金不足时 EntryPoint 要求补交的金额
    ///                            （= 本笔最大可能 gas 费 - 当前押金；押金足够时为 0）
    /// @return validationData     签名失败标志 | validUntil << 160 | validAfter << 208
    function validateUserOp(PackedUserOperation calldata userOp, bytes32 userOpHash, uint256 missingAccountFunds)
        external
        onlyEntryPoint
        returns (uint256 validationData)
    {
        uint256 packed = _loadConfig(); // 一次 SLOAD 读出 4 个 gas 上限
        _validateEnvelope(userOp, packed); // gas 上限
        _validatePolicy(userOp.callData); // 调用内容（nonce 由 EntryPoint 检查，通道由 Go 端管理）
        validationData = _validateSignature(userOp.signature, userOpHash); // 签名（时间窗交给 EntryPoint）

        // 押金不足时，从本合约余额向 EntryPoint（msg.sender）补交它要求的差额，记入本合约押金；
        // 结算后未用完的部分退回押金（不退回本合约余额）。
        // 不检查转账结果：本合约余额不足时转账失败，EntryPoint 自己会以 AA21 拒绝。
        if (missingAccountFunds != 0) {
            assembly ("memory-safe") {
                pop(call(gas(), caller(), missingAccountFunds, 0, 0, 0, 0))
            }
        }
    }

    /// 检查 UserOp 的"外壳"：各项 gas 参数不超过上限。
    /// 不检查 initCode（v0.9 对已部署账户只发 IgnoredInitCode 事件后忽略）和 paymasterAndData
    /// （包含在 userOpHash 里，bundler 无法私自添加；若使用 Paymaster，gas 由对方支付）。
    /// 作用：限制"单笔" UserOp 最多被扣 (PVG + VGL + CGL 上限) × maxFeePerGas 上限，
    /// 并压缩签名私钥泄露时攻击者每笔的利润。
    /// 注意：它不限制总损失——攻击者可批量提交任意多笔，总损失上限是押金 + 账户余额。
    function _validateEnvelope(PackedUserOperation calldata userOp, uint256 packed) internal pure {
        // maxFeePerGas 位于 gasFees 的低 128 位。
        uint256 cap = packed & type(uint96).max;
        uint256 maxFee = uint128(uint256(userOp.gasFees));
        if (maxFee > cap) revert MaxFeePerGasTooHigh(maxFee, cap);
        // maxPriorityFeePerGas 不单独设上限：实际单价 = min(maxFee, baseFee + priority) <= maxFee。

        cap = packed >> PVG_SHIFT & type(uint32).max;
        if (userOp.preVerificationGas > cap) revert PreVerificationGasTooHigh(userOp.preVerificationGas, cap);

        uint256 limits = uint256(userOp.accountGasLimits);
        uint256 gasValue = limits >> 128; // 高 128 位：verificationGasLimit
        cap = packed >> VGL_SHIFT & type(uint32).max;
        if (gasValue > cap) revert VerificationGasTooHigh(gasValue, cap);

        gasValue = uint128(limits); // 低 128 位：callGasLimit
        cap = packed >> CGL_SHIFT & type(uint32).max;
        if (gasValue > cap) revert CallGasTooHigh(gasValue, cap);
    }

    /// 检查 callData 必须是 relayWithdraw(pool, data)，且 data 是合法的 Tornado withdraw。
    /// 不检查 nonce 通道：EntryPoint 只保证每个 (key, 序号) 执行一次（AA25），通道分配见合约头部说明。
    /// 直接从 calldata 解析（不把约 500 字节的 proof 复制到内存），
    /// 偏移规则与 Solidity 对 relayWithdraw 参数的 ABI 解码完全一致，所有偏移都做了越界检查。
    ///
    /// callData 布局：
    ///   [0,4)    relayWithdraw 选择器
    ///   [4,36)   pool 地址（高 96 位必须为 0）
    ///   [36,68)  data 的相对偏移 rel（相对于第 4 字节）
    ///   [4+rel, 4+rel+32)     data 长度
    ///   [4+rel+32, ...)       data 内容（Tornado withdraw 调用数据）
    function _validatePolicy(bytes calldata callData) internal view {
        uint256 base;
        uint256 size;
        uint256 selector;
        uint256 poolWord;
        uint256 rel;
        assembly ("memory-safe") {
            base := callData.offset
            size := callData.length
            selector := shr(224, calldataload(base))
            poolWord := calldataload(add(base, 4))
            rel := calldataload(add(base, 36))
        }
        if (size < 68 || selector != uint32(this.relayWithdraw.selector) || poolWord >> 160 != 0) revert BadCallData();
        if (rel > size) revert BadCallData(); // 先限制 rel，保证下面的加法不会溢出
        uint256 dataOffset;
        uint256 dataLength;
        unchecked {
            uint256 lenPos = 4 + rel; // 长度字段位置，<= size + 4
            if (lenPos + 32 > size) revert BadCallData(); // 长度字段必须完整
            assembly ("memory-safe") {
                dataLength := calldataload(add(base, lenPos))
            }
            if (dataLength > size - lenPos - 32) revert BadCallData(); // data 不能超出 callData
            dataOffset = base + lenPos + 32;
        }
        _checkWithdraw(address(uint160(poolWord)), dataOffset, dataLength);
    }

    /// 校验签名和时间窗，返回 validationData。
    /// 签名格式：validAfter(6) | validUntil(6) | r(32) | s(32) | v(1)，v 只能是 27 或 28。
    /// 被签名的是 signingDigest = keccak256(abi.encode(userOpHash, validAfter, validUntil))，不加任何前缀。
    /// userOpHash 已包含 chainId、EntryPoint、账户地址和 nonce，负责防重放；
    /// 时间窗不在 userOpHash 里，必须一起签，否则可被任意篡改。
    /// 前提：签名私钥专用于本合约，不得用于任何其他签名场景，也不得导入钱包。
    /// 时间窗不在本合约检查（合法性、长度均由 Go 端保证），原样放进 validationData 交给 EntryPoint。
    /// 不单独检查签名长度：不足 77 字节时下面的切片越界自动 revert；超出部分被忽略（不影响结果）。
    function _validateSignature(bytes calldata sig, bytes32 userOpHash) internal view returns (uint256) {
        uint48 validAfter = uint48(bytes6(sig[0:6]));
        uint48 validUntil = uint48(bytes6(sig[6:12]));
        bytes32 digest = signingDigest(userOpHash, validAfter, validUntil);
        // OZ ECDSA.tryRecover：拒绝 high-s；v 不是 27/28（包括 0/1）或签名非法时返回 address(0)（不 revert），
        // 而 address(0) 永远不可能是签名者，因此按签名失败处理。只接受 27/28，保证签名编码唯一。
        (address signer,,) = ECDSA.tryRecover(digest, uint8(sig[76]), bytes32(sig[12:44]), bytes32(sig[44:76]));
        bool sigFailed = !_signers.contains(signer);
        // 时间窗交给 EntryPoint 检查（v0.9）：当前时间 > validUntil 或 <= validAfter 时拒绝（AA22），
        // 即生效时间是 validAfter 之后的下一秒。
        return (sigFailed ? SIG_VALIDATION_FAILED : 0) | (uint256(validUntil) << 160) | (uint256(validAfter) << 208);
    }

    // =====================================================================
    // 执行阶段
    // =====================================================================

    /// UserOp 唯一允许执行的操作：调用固定池的 withdraw。
    /// 只能由 EntryPoint 在验证通过后调用。不再重复验证阶段的检查：
    /// 验证与执行在同一笔 handleOps 交易里，执行的 calldata 正是验证过的那份。
    /// 不单独发事件：EntryPoint 的 UserOperationEvent 和池合约的 Withdrawal 事件已包含全部信息。
    /// @param pool 池地址
    /// @param data 完整的 Tornado withdraw 调用数据（以选择器 0x21a0adb6 开头）；
    ///             验证阶段已在 _checkWithdraw 中检查选择器、长度、池和 _relayer，这里原样转发给 pool。
    function relayWithdraw(address pool, bytes calldata data) external onlyEntryPoint {
        // 手写汇编调用池合约（比 OZ Address.functionCall 每笔省约 760 gas）：
        //   - 把 data 从 calldata 直接复制到空闲内存，不经过 Solidity 的 bytes memory 编码；
        //   - 成功时不复制返回数据（池的 withdraw 没有返回值）；
        //   - 失败时原样抛出池合约的 revert 原因（EntryPoint 记录为 success=false）。
        // OZ 版本附带的"pool 必须有代码"检查已移到构造函数（部署时检查 4 个固定池）。
        // 只写入空闲内存指针之后的区域且不再使用，符合 memory-safe 要求。
        assembly ("memory-safe") {
            let ptr := mload(0x40)
            calldatacopy(ptr, data.offset, data.length)
            if iszero(call(gas(), pool, 0, ptr, data.length, 0, 0)) {
                returndatacopy(ptr, 0, returndatasize())
                revert(ptr, returndatasize())
            }
        }
        // 外部调用之后不修改任何状态，不存在重入风险。
    }

    // =====================================================================
    // owner 两步转移（OZ Ownable2Step）
    // =====================================================================
    // transferOwnership(newOwner)：现任 owner 提名；传 address(0) 撤回提名。
    //   新 owner 确认之前，现任 owner 仍拥有全部权限。
    // acceptOwnership()：被提名地址自己确认后生效。
    // pendingOwner()：查询待确认的新 owner。

    /// 禁止放弃所有权：否则资金无法提取、签名者无法撤销。
    function renounceOwnership() public view override onlyOwner {
        revert RenounceDisabled();
    }

    // =====================================================================
    // owner 管理功能
    // =====================================================================

    /// 修改手续费接收地址。
    /// 注意：写着旧地址的在途 UserOp 从此会验证失败（RelayerMismatch），只能等其过期。
    /// 应先停止接收新请求、等在途 UserOp 处理完，再修改。
    function setRewardAccount(address newRewardAccount) external onlyOwner {
        emit RewardAccountChanged(rewardAccount, newRewardAccount);
        rewardAccount = newRewardAccount;
    }

    /// 增加或撤销签名者。被撤销签名者签过的在途 UserOp 会验证失败（AA24）。
    function setSigner(address signer, bool allowed) external onlyOwner {
        _setSigner(signer, allowed);
    }

    /// 修改每笔 UserOp 的 gas 上限（其他字段不变）。
    function setGasCaps(uint96 maxFeePerGas, uint32 maxPreVerificationGas, uint32 maxVerificationGas, uint32 maxCallGas)
        external
        onlyOwner
    {
        _setGasCaps(maxFeePerGas, maxPreVerificationGas, maxVerificationGas, maxCallGas);
    }

    /// 取出：本合约的原生币余额 → to（与 EntryPoint 押金无关）。
    function withdrawNative(address payable to, uint256 amount) external onlyOwner {
        if (to == address(0)) revert ZeroAddress();
        Address.sendValue(to, amount); // 余额不足或对方拒收时 revert
        emit NativeWithdrawn(to, amount);
    }

    /// 存入：本合约的原生币余额 → EntryPoint 押金（提前存入可省去每笔自动补交的转账开销）。
    /// 也可以由任何人直接调用 EntryPoint.depositTo{value: x}(本合约地址)，效果相同。
    function depositEntryPoint(uint256 amount) external onlyOwner {
        entryPoint.depositTo{value: amount}(address(this)); // 余额不足时 revert
        emit EntryPointDeposited(amount);
    }

    /// 取出：EntryPoint 押金 → to。
    function withdrawEntryPoint(address payable to, uint256 amount) external onlyOwner {
        if (to == address(0)) revert ZeroAddress();
        entryPoint.withdrawTo(to, amount);
        emit EntryPointWithdrawn(to, amount);
    }

    /// 取出：本合约持有的 ERC20 代币 → to。
    /// OZ SafeERC20：兼容不返回 bool 的代币（如 USDT），对没有代码的地址会 revert。
    function withdrawToken(IERC20 token, address to, uint256 amount) external onlyOwner {
        if (to == address(0)) revert ZeroAddress();
        token.safeTransfer(to, amount);
        emit TokenWithdrawn(address(token), to, amount);
    }

    // =====================================================================
    // 只读函数
    // =====================================================================

    /// 是否为授权签名者。
    function isSigner(address account) external view returns (bool) {
        return _signers.contains(account);
    }

    /// 全部授权签名者（用于监控；数量很少，直接返回数组）。
    function signers() external view returns (address[] memory) {
        return _signers.values();
    }

    /// 池是否是 4 个固定池之一。
    function isPoolAllowed(address pool) public view returns (bool) {
        return pool == POOL_0 || pool == POOL_1 || pool == POOL_2 || pool == POOL_3;
    }

    /// 4 个固定池地址。
    function pools() external view returns (address[4] memory) {
        return [POOL_0, POOL_1, POOL_2, POOL_3];
    }

    /// 签名者需要签的摘要：keccak256(abi.encode(userOpHash, validAfter, validUntil))。
    /// Go 端：sig := crypto.Sign(digest, key); sig[64] += 27；
    /// 签名格式 validAfter(6) | validUntil(6) | r | s | v(27/28)。
    function signingDigest(bytes32 userOpHash, uint48 validAfter, uint48 validUntil) public pure returns (bytes32) {
        return keccak256(abi.encode(userOpHash, validAfter, validUntil));
    }

    // =====================================================================
    // 内部函数
    // =====================================================================

    /// 检查位于 calldata [offset, offset+length) 的 Tornado withdraw 调用数据。
    /// 调用方已保证该区间不越界。
    ///
    /// withdraw 数据布局（每个字 32 字节，位置相对于 offset）：
    ///   [0,4)     选择器 0x21a0adb6
    ///   [4,36)    字0：_proof 的偏移
    ///   [36,68)   字1：_root
    ///   [68,100)  字2：_nullifierHash
    ///   [100,132) 字3：_recipient（不检查：由 proof 约束）
    ///   [132,164) 字4：_relayer   → 低 160 位必须等于 rewardAccount
    ///   [164,196) 字5：_fee
    ///   [196,228) 字6：_refund    （不检查：池合约自己要求与 msg.value 匹配，本合约发送 0）
    function _checkWithdraw(address pool, uint256 offset, uint256 length) internal view {
        // 与 4 个 immutable 地址比较（不需要 SLOAD）。
        if (!isPoolAllowed(pool)) revert PoolNotAllowed(pool);
        if (length < WITHDRAW_HEAD_LEN) revert BadCallData();
        uint256 selector;
        uint256 relayerWord;
        assembly ("memory-safe") {
            selector := shr(224, calldataload(offset))
            relayerWord := calldataload(add(offset, 132))
        }
        if (selector != uint32(POOL_WITHDRAW_SELECTOR)) revert BadCallData();
        // 只比较低 160 位：Tornado 池（solc 0.7 / ABI v1）对地址参数同样只取低 160 位，
        // 因此高位是否为 0 不影响手续费的实际去向，无需单独检查。
        address relayer = address(uint160(relayerWord));
        if (relayer != rewardAccount) revert RelayerMismatch(relayer);
    }

    /// 直接读取打包的 Config 槽（位布局见 *_SHIFT 常量），比按字段读取更省 gas。
    function _loadConfig() internal view returns (uint256 packed) {
        assembly ("memory-safe") {
            packed := sload(config.slot)
        }
    }

    /// 写入 gas 上限（一次性写整个槽）；任何一项都不能为 0。
    function _setGasCaps(
        uint96 maxFeePerGas,
        uint32 maxPreVerificationGas,
        uint32 maxVerificationGas,
        uint32 maxCallGas
    ) internal {
        if (maxFeePerGas == 0 || maxPreVerificationGas == 0 || maxVerificationGas == 0 || maxCallGas == 0) {
            revert ZeroLimit();
        }
        config = Config({
            maxFeePerGas: maxFeePerGas,
            maxPreVerificationGas: maxPreVerificationGas,
            maxVerificationGas: maxVerificationGas,
            maxCallGas: maxCallGas
        });
        emit GasCapsChanged(maxFeePerGas, maxPreVerificationGas, maxVerificationGas, maxCallGas);
    }

    /// 增加/撤销签名者；状态未变化时不发事件（幂等）。
    /// 零地址禁止成为签名者：签名非法时 tryRecover 返回 address(0)，若它在集合中，任何垃圾签名都会通过。
    function _setSigner(address signer, bool allowed) internal {
        if (signer == address(0)) revert ZeroAddress();
        bool changed = allowed ? _signers.add(signer) : _signers.remove(signer);
        if (changed) emit SignerChanged(signer, allowed);
    }
}
