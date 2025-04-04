// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/access/Ownable.sol";
import "@openzeppelin/contracts/security/ReentrancyGuard.sol";
import "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import "@openzeppelin/contracts/token/ERC20/IERC20.sol";

interface IUniswapV2Router {
    function WETH() external pure returns (address);

    function swapExactETHForTokensSupportingFeeOnTransferTokens(
        uint256 amountOutMin,
        address[] calldata path,
        address to,
        uint256 deadline
    ) external payable;

    function swapExactTokensForETHSupportingFeeOnTransferTokens(
        uint256 amountIn,
        uint256 amountOutMin,
        address[] calldata path,
        address to,
        uint256 deadline
    ) external;
}

contract PureArbitrage is Ownable, ReentrancyGuard {
    using SafeERC20 for IERC20;

    address public immutable router;
    mapping(address => bool) private _approvedTokens;

    constructor(address _router) Ownable(msg.sender) {
        require(_router != address(0), "Invalid router");
        router = _router;
    }

    // 前导交易：ETH -> Token (路径: WETH -> Token)
    function frontRun(address token, uint256 amountOutMin,)
        external
        payable
        onlyOwner
        nonReentrant
    {
        require(msg.value > 0, "ETH required");
        address[] memory path = new address[](2);
        path[0] = IUniswapV2Router(router).WETH();
        path[1] = token;

        IUniswapV2Router(router)
            .swapExactETHForTokensSupportingFeeOnTransferTokens{
            value: msg.value
        }(amountOutMin, path, address(this), block.timestamp);
    }

    // 后导交易：Token -> ETH (路径: Token -> WETH)
    function backRun(address token, uint256 amountOutMin)
        external
        onlyOwner
        nonReentrant
    {
        IERC20 tokenContract = IERC20(token);
        uint256 balance = tokenContract.balanceOf(address(this));
        require(balance > 0, "No balance");

        // 动态授权检查
        if (!_approvedTokens[token]) {
            tokenContract.forceApprove(router, 0);
            tokenContract.forceApprove(router, type(uint256).max);
            _approvedTokens[token] = true;
        }

        address[] memory path = new address[](2);
        path[0] = token;
        path[1] = IUniswapV2Router(router).WETH();

        // 执行交易
        IUniswapV2Router(router)
            .swapExactTokensForETHSupportingFeeOnTransferTokens(
                balance,
                amountOutMin,
                path,
                owner(),
                block.timestamp
            );
    }

    // 紧急提现ETH
    function emergencyWithdrawETH() external onlyOwner nonReentrant {
        payable(owner()).transfer(address(this).balance);
    }

    // 紧急提现代币
    function emergencyWithdrawToken(address token)
        external
        onlyOwner
        nonReentrant
    {
        uint256 balance = IERC20(token).balanceOf(address(this));
        IERC20(token).safeTransfer(owner(), balance);
    }

    receive() external payable {}
}
